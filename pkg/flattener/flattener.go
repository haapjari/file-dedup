// Package flattener moves all files to root directory and removes duplicates.
package flattener

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"file-dedup/pkg/collector"
	"file-dedup/pkg/hasher"
	"file-dedup/pkg/progress"
	"file-dedup/pkg/safepath"
	"file-dedup/pkg/sanitizer"
	"file-dedup/pkg/trash"
)

// ErrContentChanged indicates that a file's content has changed since its hash
// was computed. This sentinel allows callers to detect and handle stale-hash
// situations with errors.Is.
var ErrContentChanged = errors.New("file content changed since hash was computed")

// MoveOperation represents a single move operation.
type MoveOperation struct {
	OriginalPath string
	NewPath      string
	Hash         string // SHA256 content hash
	Duplicate    bool   // true if this file was deleted as duplicate
	TrashedTo    string // Trash destination (empty when trasher is nil)
	Skipped      bool   // true if skipped (e.g., already in root)
	SkipReason   string
	Error        error
}

// Result contains the results of a flatten operation.
type Result struct {
	Operations       []MoveOperation
	TotalFiles       int
	MovedCount       int
	DuplicatesCount  int
	SkippedCount     int
	ErrorCount       int
	DeletedDirsCount int
}

// Flattener moves files to root and removes duplicates.
type Flattener struct {
	dryRun    bool
	rootDir   string
	validator *safepath.Validator
	hasher    *hasher.Hasher
	trasher   *trash.Trasher
}

const (
	progressStageHashing = "hashing"
	progressStageMoving  = "moving"
)

// New creates a new Flattener with path containment validation.
func New(rootDir string, dryRun bool) (*Flattener, error) {
	return NewWithWorkers(rootDir, dryRun, 0)
}

// NewWithWorkers creates a new Flattener with custom worker count.
func NewWithWorkers(rootDir string, dryRun bool, workers int) (*Flattener, error) {
	v, err := safepath.New(rootDir)
	if err != nil {
		return nil, fmt.Errorf("failed to create path validator: %w", err)
	}

	return NewWithValidator(v, dryRun, workers, nil)
}

// NewWithValidator creates a new Flattener with an existing validator.
// An optional trasher enables soft-delete (move to trash) instead of permanent removal.
func NewWithValidator(validator *safepath.Validator, dryRun bool, workers int, trasher *trash.Trasher) (*Flattener, error) {
	if validator == nil {
		return nil, errors.New("validator is required")
	}

	return &Flattener{
		rootDir:   validator.Root(),
		dryRun:    dryRun,
		validator: validator,
		hasher:    hasher.New(hasher.WithWorkers(workers)),
		trasher:   trasher,
	}, nil
}

// FlattenFiles moves all files to root directory, removing duplicates.
// Duplicates are identified by SHA256 content hash - this is safe and reliable.
func (f *Flattener) FlattenFiles(files []collector.FileInfo) Result {
	return f.FlattenFilesWithProgress(files, nil)
}

// FlattenFilesWithProgress moves files and reports stage progress.
func (f *Flattener) FlattenFilesWithProgress(files []collector.FileInfo, onProgress func(stage string, processed, total int)) Result {
	result := Result{
		TotalFiles: len(files),
		Operations: make([]MoveOperation, 0, len(files)),
	}

	if len(files) == 0 {
		return result
	}

	// Step 1: Pre-compute hashes for all files using parallel hashing.
	fileHashes, invalidReadErrors := f.computeHashes(files, func(processed, total int) {
		progress.EmitStage(onProgress, progressStageHashing, processed, total)
	})
	if len(invalidReadErrors) > 0 {
		for i := range files {
			pathErr, hasErr := invalidReadErrors[files[i].Path]
			if !hasErr {
				continue
			}

			op := MoveOperation{
				OriginalPath: files[i].Path,
				Error:        pathErr,
			}
			result.Operations = append(result.Operations, op)
			result.ErrorCount++
		}

		return result
	}

	// Step 2: Track seen content hashes to detect duplicates.
	seenHash := make(map[string]string) // hash -> path of first occurrence

	// Track name conflicts (same name but different content).
	nameCount := make(map[string]int)

	totalFiles := len(files)
	for i := range files {
		hash := fileHashes[files[i].Path]
		op := f.processFile(&files[i], hash, seenHash, nameCount)
		result.Operations = append(result.Operations, op)

		if op.Error != nil {
			result.ErrorCount++
		} else if op.Duplicate {
			result.DuplicatesCount++
		} else if op.Skipped {
			result.SkippedCount++
		} else {
			result.MovedCount++
		}

		progress.EmitStage(onProgress, progressStageMoving, i+1, totalFiles)
	}

	// Remove empty directories if not dry run.
	if !f.dryRun {
		result.DeletedDirsCount = f.removeEmptyDirs()
	}

	return result
}

// computeHashes pre-computes SHA256 hashes for all files using parallel hashing.
func (f *Flattener) computeHashes(files []collector.FileInfo, onProgress func(processed, total int)) (hashes map[string]string, invalidReadErrors map[string]error) {
	hashes = make(map[string]string, len(files))
	invalidReadErrors = make(map[string]error)

	// Prepare files for parallel hashing.
	toHash := make([]hasher.FileToHash, 0, len(files))
	for _, file := range files {
		if err := f.validator.ValidatePathForRead(file.Path); err != nil {
			invalidReadErrors[file.Path] = fmt.Errorf("source path escapes root: %w", err)
			continue
		}

		toHash = append(toHash, hasher.FileToHash{
			Path: file.Path,
			Size: file.Size,
		})
	}

	// Hash files in parallel.
	processed := 0
	total := len(toHash)
	for result := range f.hasher.HashFilesWithSizes(toHash) {
		processed++
		progress.Emit(onProgress, processed, total)

		if result.Error == nil {
			hashes[result.Path] = result.Hash
		}
	}

	return hashes, invalidReadErrors
}

func (f *Flattener) processFile(file *collector.FileInfo, hash string, seenHash map[string]string, nameCount map[string]int) MoveOperation {
	op := MoveOperation{
		OriginalPath: file.Path,
		Hash:         hash,
	}

	// Validate source path is within root.
	if err := f.validator.ValidatePathForRead(file.Path); err != nil {
		op.Error = fmt.Errorf("source path escapes root: %w", err)
		return op
	}

	// If we couldn't compute hash, skip this file with error.
	if hash == "" {
		op.Error = errors.New("could not compute hash for file")
		return op
	}

	// Check if already in root directory.
	if file.Dir == f.rootDir {
		op.Skipped = true
		op.SkipReason = "already in root"
		op.NewPath = file.Path
		// Still track the hash so duplicates in subdirs are detected.
		if _, exists := seenHash[hash]; !exists {
			seenHash[hash] = file.Path
		}
		return op
	}

	// Check if this is a duplicate by content hash.
	if existingPath, exists := seenHash[hash]; exists {
		return f.handleDuplicate(&op, file.Path, existingPath)
	}

	// Determine target path, handling name conflicts.
	count := nameCount[file.Name]
	targetName := sanitizer.ResolveNameConflict(file.Name, count)
	nameCount[file.Name] = count + 1

	op.NewPath = filepath.Join(f.rootDir, targetName)

	// Validate destination path is within root.
	if err := f.validator.ValidatePath(op.NewPath); err != nil {
		op.Error = fmt.Errorf("destination path escapes root: %w", err)
		return op
	}

	// Move the file using safe rename. Only track the hash after a successful
	// rename so that seenHash never points to a non-existent file.
	if f.dryRun {
		seenHash[hash] = op.NewPath
	} else {
		if err := f.validator.SafeRename(file.Path, op.NewPath); err != nil {
			op.Error = fmt.Errorf("failed to move: %w", err)
			return op
		}
		seenHash[hash] = op.NewPath
	}

	return op
}

// handleDuplicate records a duplicate and optionally deletes it after verifying
// the kept copy still exists.
func (f *Flattener) handleDuplicate(op *MoveOperation, dupPath, existingPath string) MoveOperation {
	op.Duplicate = true
	op.NewPath = existingPath // reference to the kept file

	if !f.dryRun {
		// Verify the kept file still exists before deleting the duplicate.
		if _, err := os.Lstat(existingPath); err != nil {
			op.Error = fmt.Errorf("kept file missing, refusing to delete duplicate: %w", err)
			return *op
		}

		// Re-hash the file to confirm it hasn't changed since initial hash.
		currentHash, err := f.hasher.ComputeHash(dupPath)
		if err != nil {
			op.Error = fmt.Errorf("re-hash before delete: %w", err)
			return *op
		}
		if currentHash != op.Hash {
			op.Error = fmt.Errorf("content changed since hashing: %w", ErrContentChanged)
			return *op
		}

		op.TrashedTo, op.Error = f.trashOrRemove(dupPath)
	}
	return *op
}

// trashOrRemove soft-deletes a file when a trasher is configured, otherwise
// permanently removes it. Returns the trash destination (empty on hard delete).
func (f *Flattener) trashOrRemove(path string) (string, error) {
	if f.trasher != nil {
		return f.trasher.TrashWithDest(path)
	}

	if err := f.validator.SafeRemove(path); err != nil {
		return "", fmt.Errorf("failed to delete duplicate: %w", err)
	}

	return "", nil
}

// removeEmptyDirs removes all empty directories under rootDir.
func (f *Flattener) removeEmptyDirs() int {
	count := 0

	// Walk bottom-up by collecting all dirs first.
	var dirs []string
	err := filepath.Walk(f.rootDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr == nil && info.IsDir() && path != f.rootDir {
			dirs = append(dirs, path)
		}
		return nil
	})
	if err != nil {
		return 0
	}

	// Remove in reverse order (deepest first).
	for i := len(dirs) - 1; i >= 0; i-- {
		dir := dirs[i]
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		if len(entries) == 0 {
			if err := f.validator.SafeRemoveDir(dir); err == nil {
				count++
			}
		}
	}

	return count
}

// DryRun returns whether the flattener is in dry-run mode.
func (f *Flattener) DryRun() bool {
	return f.dryRun
}

// Root returns the root directory being validated against.
func (f *Flattener) Root() string {
	return f.validator.Root()
}
