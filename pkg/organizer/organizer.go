// Package organizer groups files into subdirectories by file extension.
package organizer

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"file-dedup/pkg/collector"
	"file-dedup/pkg/progress"
	"file-dedup/pkg/safepath"
	"file-dedup/pkg/sanitizer"
)

// MoveOperation represents a single organize operation.
type MoveOperation struct {
	OriginalPath string
	NewPath      string
	Extension    string // target extension folder name
	Skipped      bool
	SkipReason   string
	Error        error
}

// Result contains the results of an organize operation.
type Result struct {
	Operations       []MoveOperation
	TotalFiles       int
	MovedCount       int
	SkippedCount     int
	ErrorCount       int
	CreatedDirsCount int
}

// Organizer groups files into extension-based subdirectories.
type Organizer struct {
	dryRun    bool
	rootDir   string
	validator *safepath.Validator
}

// New creates a new Organizer with path containment validation.
func New(rootDir string, dryRun bool) (*Organizer, error) {
	v, err := safepath.New(rootDir)
	if err != nil {
		return nil, fmt.Errorf("failed to create path validator: %w", err)
	}

	return NewWithValidator(v, dryRun)
}

// NewWithValidator creates a new Organizer with an existing validator.
func NewWithValidator(validator *safepath.Validator, dryRun bool) (*Organizer, error) {
	if validator == nil {
		return nil, errors.New("validator is required")
	}

	return &Organizer{
		rootDir:   validator.Root(),
		dryRun:    dryRun,
		validator: validator,
	}, nil
}

// OrganizeFiles groups all files into extension subdirectories.
func (o *Organizer) OrganizeFiles(files []collector.FileInfo) Result {
	return o.OrganizeFilesWithProgress(files, nil)
}

// OrganizeFilesWithProgress groups files and reports per-file progress.
func (o *Organizer) OrganizeFilesWithProgress(files []collector.FileInfo, onProgress func(processed, total int)) Result {
	result := Result{
		TotalFiles: len(files),
		Operations: make([]MoveOperation, 0, len(files)),
	}

	if len(files) == 0 {
		return result
	}

	// Validate all source paths for read safety (fail-fast on symlink escape).
	_, invalidOps := safepath.ValidateReadPaths(o.validator, files, func(file collector.FileInfo, err error) MoveOperation {
		return MoveOperation{
			OriginalPath: file.Path,
			Error:        fmt.Errorf("source path escapes root: %w", err),
		}
	})
	if len(invalidOps) > 0 {
		result.Operations = append(result.Operations, invalidOps...)
		result.ErrorCount = len(invalidOps)
		progress.Emit(onProgress, len(files), len(files))
		return result
	}

	// Track name conflicts per target directory.
	nameCount := make(map[string]map[string]int) // extDir -> name -> count
	createdDirs := make(map[string]bool)

	totalFiles := len(files)
	for i := range files {
		op := o.processFile(&files[i], nameCount, createdDirs)
		result.Operations = append(result.Operations, op)

		if op.Error != nil {
			result.ErrorCount++
		} else if op.Skipped {
			result.SkippedCount++
		} else {
			result.MovedCount++
		}

		progress.Emit(onProgress, i+1, totalFiles)
	}

	result.CreatedDirsCount = len(createdDirs)

	return result
}

func (o *Organizer) processFile(file *collector.FileInfo, nameCount map[string]map[string]int, createdDirs map[string]bool) MoveOperation {
	op := MoveOperation{
		OriginalPath: file.Path,
	}

	// Validate source path is within root.
	if err := o.validator.ValidatePathForRead(file.Path); err != nil {
		op.Error = fmt.Errorf("source path escapes root: %w", err)
		return op
	}

	ext := extensionCategory(file.Name)
	op.Extension = ext
	targetDir := filepath.Join(o.rootDir, ext)

	// Skip if file is already in the correct extension subdirectory.
	if file.Dir == targetDir {
		op.Skipped = true
		op.SkipReason = "already organized"
		op.NewPath = file.Path
		return op
	}

	// Initialize name tracking for this target directory.
	if nameCount[targetDir] == nil {
		nameCount[targetDir] = make(map[string]int)
	}

	// Handle name conflicts with _1, _2 suffixes.
	count := nameCount[targetDir][file.Name]
	targetName := sanitizer.ResolveNameConflict(file.Name, count)
	nameCount[targetDir][file.Name] = count + 1

	op.NewPath = filepath.Join(targetDir, targetName)

	// Validate destination path is within root.
	if err := o.validator.ValidatePath(op.NewPath); err != nil {
		op.Error = fmt.Errorf("destination path escapes root: %w", err)
		return op
	}

	if o.dryRun {
		createdDirs[targetDir] = true
		return op
	}

	if err := o.ensureDir(targetDir, createdDirs); err != nil {
		op.Error = err
		return op
	}

	if err := o.validator.SafeRename(file.Path, op.NewPath); err != nil {
		op.Error = fmt.Errorf("failed to move: %w", err)
		return op
	}

	return op
}

func (o *Organizer) ensureDir(targetDir string, createdDirs map[string]bool) error {
	if createdDirs[targetDir] {
		return nil
	}

	if err := o.validator.SafeMkdirAll(targetDir); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	createdDirs[targetDir] = true
	return nil
}

// extensionCategory returns the lowercase extension (without dot) for a filename,
// or "other" if the file has no extension.
// Dotfiles like ".gitignore" (where the name starts with dot and has no other extension)
// are treated as having no extension and go to "other".
func extensionCategory(filename string) string {
	ext := filepath.Ext(filename)
	if ext == "" {
		return "other"
	}

	// filepath.Ext(".gitignore") returns ".gitignore" — treat dotfiles as no extension.
	base := filename[:len(filename)-len(ext)]
	if base == "" {
		return "other"
	}

	// Strip the leading dot and lowercase.
	return strings.ToLower(ext[1:])
}

// DryRun returns whether the organizer is in dry-run mode.
func (o *Organizer) DryRun() bool {
	return o.dryRun
}

// Root returns the root directory being validated against.
func (o *Organizer) Root() string {
	return o.validator.Root()
}
