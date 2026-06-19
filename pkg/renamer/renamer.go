// Package renamer provides file renaming utilities.
// Phase 1: Renames files in place with timestamp prefix and sanitized names.
package renamer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"file-dedup/pkg/collector"
	"file-dedup/pkg/hasher"
	"file-dedup/pkg/progress"
	"file-dedup/pkg/safepath"
	"file-dedup/pkg/sanitizer"
	"file-dedup/pkg/trash"
)

// RenameOperation represents a single rename operation.
type RenameOperation struct {
	OriginalPath string
	NewPath      string
	OriginalName string
	NewName      string
	Skipped      bool
	SkipReason   string
	Deleted      bool
	TrashedTo    string // Trash destination (empty when trasher is nil)
	Error        error
}

// Result contains the results of a rename operation.
type Result struct {
	Operations   []RenameOperation
	TotalFiles   int
	RenamedCount int
	SkippedCount int
	DeletedCount int
	ErrorCount   int
}

// Renamer handles file renaming operations.
type Renamer struct {
	dryRun    bool
	validator *safepath.Validator
	hasher    *hasher.Hasher
	trasher   *trash.Trasher
}

// ErrContentChanged indicates that a file's content has changed since its hash
// was computed. This sentinel allows callers to detect and handle stale-hash
// situations with errors.Is.
var ErrContentChanged = errors.New("file content changed since hash was computed")

var tbdPrefixPattern = regexp.MustCompile(`^\d{4}-TBD-TBD_`)

type nameUsage struct {
	count int
	size  int64
	hash  string
	path  string // path of the kept file
}

// New creates a new Renamer with path containment validation.
func New(rootDir string, dryRun bool) (*Renamer, error) {
	v, err := safepath.New(rootDir)
	if err != nil {
		return nil, fmt.Errorf("failed to create path validator: %w", err)
	}

	return NewWithValidator(v, dryRun, nil)
}

// NewWithValidator creates a new Renamer with an existing validator.
// An optional trasher enables soft-delete (move to trash) instead of permanent removal.
func NewWithValidator(validator *safepath.Validator, dryRun bool, trasher *trash.Trasher) (*Renamer, error) {
	if validator == nil {
		return nil, errors.New("validator is required")
	}

	return &Renamer{
		dryRun:    dryRun,
		validator: validator,
		hasher:    hasher.New(),
		trasher:   trasher,
	}, nil
}

// RenameFiles renames all files in the given list according to the naming conventions.
// Files are renamed in place (same directory).
func (r *Renamer) RenameFiles(files []collector.FileInfo) Result {
	return r.RenameFilesWithProgress(files, nil)
}

// RenameFilesWithProgress renames files and reports per-file progress.
func (r *Renamer) RenameFilesWithProgress(files []collector.FileInfo, onProgress func(processed, total int)) Result {
	result := Result{
		TotalFiles: len(files),
		Operations: make([]RenameOperation, 0, len(files)),
	}
	totalFiles := len(files)

	_, invalidReadOps := safepath.ValidateReadPaths(r.validator, files, func(file collector.FileInfo, err error) RenameOperation {
		return RenameOperation{
			OriginalPath: file.Path,
			OriginalName: file.Name,
			Error:        fmt.Errorf("source path escapes root: %w", err),
		}
	})
	if len(invalidReadOps) > 0 {
		result.Operations = append(result.Operations, invalidReadOps...)
		result.ErrorCount = len(invalidReadOps)
		progress.Emit(onProgress, totalFiles, totalFiles)
		return result
	}

	// Track new names within each directory to handle conflicts and duplicates
	dirNames := make(map[string]map[string]nameUsage) // dir -> name -> usage

	for i, f := range files {
		op := r.processFile(f, dirNames)
		result.Operations = append(result.Operations, op)

		if op.Error != nil {
			result.ErrorCount++
		} else if op.Deleted {
			result.DeletedCount++
		} else if op.Skipped {
			result.SkippedCount++
		} else {
			result.RenamedCount++
		}

		progress.Emit(onProgress, i+1, totalFiles)
	}

	return result
}

// processFile handles renaming a single file.
func (r *Renamer) processFile(f collector.FileInfo, dirNames map[string]map[string]nameUsage) RenameOperation {
	op := RenameOperation{
		OriginalPath: f.Path,
		OriginalName: f.Name,
	}

	if tbdPrefixPattern.MatchString(f.Name) {
		op.Skipped = true
		op.SkipReason = "already has TBD prefix"
		return op
	}

	// Validate source path is within root.
	if err := r.validator.ValidatePathForRead(f.Path); err != nil {
		op.Error = fmt.Errorf("source path escapes root: %w", err)
		return op
	}

	// Generate new name
	newName := sanitizer.GenerateTimestampedName(f.Name, f.ModTime)

	// Initialize directory tracking if needed
	if dirNames[f.Dir] == nil {
		dirNames[f.Dir] = make(map[string]nameUsage)
	}

	// Handle naming conflicts within the same directory
	baseName := newName
	ext := filepath.Ext(newName)
	nameWithoutExt := newName[:len(newName)-len(ext)]

	newName, handled := r.resolveNameConflict(&op, f, baseName, nameWithoutExt, ext, dirNames[f.Dir])
	if handled {
		return op
	}

	op.NewName = newName
	op.NewPath = filepath.Join(f.Dir, newName)

	// Validate destination path is within root.
	if err := r.validator.ValidatePath(op.NewPath); err != nil {
		op.Error = fmt.Errorf("destination path escapes root: %w", err)
		return op
	}

	// Skip if name hasn't changed
	if f.Name == newName {
		op.Skipped = true
		op.SkipReason = "name unchanged"
		return op
	}

	// Skip if target already exists (safety check)
	_, lstatErr := os.Lstat(op.NewPath)
	if lstatErr == nil {
		if validateErr := r.validator.ValidatePathForRead(op.NewPath); validateErr != nil {
			op.Error = fmt.Errorf("destination path escapes root: %w", validateErr)
			return op
		}

		info, statErr := os.Stat(op.NewPath)
		if statErr != nil {
			op.Error = fmt.Errorf("failed to stat existing target: %w", statErr)
			return op
		}

		r.handleExistingTarget(&op, f, info)
		return op
	} else if !os.IsNotExist(lstatErr) {
		op.Error = fmt.Errorf("failed to inspect target path: %w", lstatErr)
		return op
	}

	// Perform rename if not dry run, using safe rename.
	if !r.dryRun {
		if err := r.validator.SafeRename(f.Path, op.NewPath); err != nil {
			op.Error = err
			return op
		}
	}

	// Update the tracked path so subsequent duplicate checks reference the actual location.
	if usage, ok := dirNames[f.Dir][baseName]; ok {
		usage.path = op.NewPath
		dirNames[f.Dir][baseName] = usage
	}

	return op
}

// DryRun returns whether the renamer is in dry-run mode.
func (r *Renamer) DryRun() bool {
	return r.dryRun
}

// Root returns the root directory being validated against.
func (r *Renamer) Root() string {
	return r.validator.Root()
}

func (r *Renamer) sameContent(pathA, pathB string) (bool, error) {
	if err := r.validator.ValidatePathForRead(pathA); err != nil {
		return false, fmt.Errorf("path escapes root: %w", err)
	}
	if err := r.validator.ValidatePathForRead(pathB); err != nil {
		return false, fmt.Errorf("path escapes root: %w", err)
	}

	hashA, err := r.hasher.ComputeHash(pathA)
	if err != nil {
		return false, fmt.Errorf("failed to hash %s: %w", pathA, err)
	}

	hashB, err := r.hasher.ComputeHash(pathB)
	if err != nil {
		return false, fmt.Errorf("failed to hash %s: %w", pathB, err)
	}

	return hashA == hashB, nil
}

func (r *Renamer) resolveNameConflict(op *RenameOperation, f collector.FileInfo, baseName, nameWithoutExt, ext string, usageMap map[string]nameUsage) (string, bool) {
	usage, ok := usageMap[baseName]
	if !ok {
		hash, err := r.hasher.ComputeHash(f.Path)
		if err != nil {
			hash = ""
		}

		usageMap[baseName] = nameUsage{
			count: 1,
			size:  f.Size,
			hash:  hash,
			path:  f.Path,
		}
		return baseName, false
	}

	if usage.size == f.Size {
		currentHash, err := r.hasher.ComputeHash(f.Path)
		if err == nil && usage.hash != "" && currentHash == usage.hash {
			r.markAsDuplicate(op, f, usage.hash, usage.path)
			return "", true
		}
	}

	newName := fmt.Sprintf("%s_%d%s", nameWithoutExt, usage.count, ext)
	usage.count++
	usageMap[baseName] = usage
	return newName, false
}

func (r *Renamer) markAsDuplicate(op *RenameOperation, f collector.FileInfo, expectedHash, keptPath string) {
	op.Skipped = true
	op.SkipReason = "duplicate file already exists"
	op.Deleted = !r.dryRun

	if r.dryRun {
		return
	}

	// Verify the kept file still exists before deleting the duplicate.
	if _, err := os.Lstat(keptPath); err != nil {
		op.Error = fmt.Errorf("kept file missing, refusing to delete duplicate: %w", err)
		op.Deleted = false
		return
	}

	// Re-hash the file to confirm it hasn't changed since initial hash.
	currentHash, err := r.hasher.ComputeHash(f.Path)
	if err != nil {
		op.Error = fmt.Errorf("cannot re-verify file before deletion: %w", err)
		op.Deleted = false
		return
	}
	if currentHash != expectedHash {
		op.Error = fmt.Errorf("%w, refusing to delete", ErrContentChanged)
		op.Deleted = false
		return
	}

	trashedTo, trashErr := r.trashOrRemove(f.Path)
	if trashErr != nil {
		op.Error = trashErr
		op.Deleted = false
		return
	}
	op.TrashedTo = trashedTo
}

func (r *Renamer) handleExistingTarget(op *RenameOperation, f collector.FileInfo, info os.FileInfo) {
	if info.Size() != f.Size {
		op.Skipped = true
		op.SkipReason = "target file already exists"
		return
	}

	isDuplicate, err := r.sameContent(op.NewPath, f.Path)
	if err != nil {
		op.Error = err
		return
	}

	if !isDuplicate {
		op.Skipped = true
		op.SkipReason = "target file already exists"
		return
	}

	op.Skipped = true
	op.SkipReason = "duplicate file already exists"

	if r.dryRun {
		return
	}

	// Verify target still exists before deleting source.
	if _, err := os.Lstat(op.NewPath); err != nil {
		op.Error = fmt.Errorf("target file missing, refusing to delete source: %w", err)
		return
	}

	op.Deleted = true

	trashedTo, trashErr := r.trashOrRemove(f.Path)
	if trashErr != nil {
		op.Error = trashErr
		op.Deleted = false
		return
	}
	op.TrashedTo = trashedTo
}

// trashOrRemove soft-deletes a file when a trasher is configured, otherwise
// permanently removes it. Returns the trash destination (empty on hard delete).
func (r *Renamer) trashOrRemove(path string) (string, error) {
	if r.trasher != nil {
		return r.trasher.TrashWithDest(path)
	}

	if err := r.validator.SafeRemove(path); err != nil {
		return "", fmt.Errorf("failed to delete: %w", err)
	}

	return "", nil
}
