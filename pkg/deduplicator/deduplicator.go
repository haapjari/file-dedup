// Package deduplicator identifies and removes duplicate files using content hashing.
// It uses a hybrid approach for performance:
// 1. Group files by size (instant filter - different sizes can't be duplicates)
// 2. For same-size files, compute partial hash (first + last 4KB) for quick comparison
// 3. For files with matching partial hash, compute full SHA256 to confirm
// This approach is both fast and reliable - no false positives possible.
package deduplicator

import (
	"errors"
	"fmt"
	"os"
	"sort"

	"file-dedup/pkg/collector"
	"file-dedup/pkg/hasher"
	"file-dedup/pkg/progress"
	"file-dedup/pkg/safepath"
	"file-dedup/pkg/trash"
)

// ErrContentChanged indicates that a file's content has changed since its hash
// was computed. This sentinel allows callers to detect and handle stale-hash
// situations with errors.Is.
var ErrContentChanged = errors.New("file content changed since hash was computed")

// DeleteOperation represents a single delete operation.
type DeleteOperation struct {
	Path       string // Path of file to delete
	OriginalOf string // Path of the original file this is a duplicate of
	Size       int64
	Hash       string // SHA256 hash of the file
	TrashedTo  string // Trash destination (empty when trasher is nil)
	Skipped    bool
	SkipReason string
	Error      error
}

// Result contains the results of a deduplication operation.
type Result struct {
	Operations      []DeleteOperation
	TotalFiles      int
	DuplicatesFound int
	DeletedCount    int
	SkippedCount    int
	ErrorCount      int
	BytesRecovered  int64
}

// Deduplicator identifies and removes duplicate files using content hashing.
type Deduplicator struct {
	dryRun    bool
	validator *safepath.Validator
	hasher    *hasher.Hasher
	trasher   *trash.Trasher
}

const (
	progressStageHashing  = "hashing"
	progressStageDeleting = "deleting"
)

// New creates a new Deduplicator with path containment validation.
func New(rootDir string, dryRun bool) (*Deduplicator, error) {
	return NewWithWorkers(rootDir, dryRun, 0)
}

// NewWithWorkers creates a new Deduplicator with custom worker count.
func NewWithWorkers(rootDir string, dryRun bool, workers int) (*Deduplicator, error) {
	v, err := safepath.New(rootDir)
	if err != nil {
		return nil, fmt.Errorf("failed to create path validator: %w", err)
	}

	return NewWithValidator(v, dryRun, workers, nil)
}

// NewWithValidator creates a new Deduplicator with an existing validator.
// An optional trasher enables soft-delete (move to trash) instead of permanent removal.
func NewWithValidator(validator *safepath.Validator, dryRun bool, workers int, trasher *trash.Trasher) (*Deduplicator, error) {
	if validator == nil {
		return nil, errors.New("validator is required")
	}

	return &Deduplicator{
		dryRun:    dryRun,
		validator: validator,
		hasher:    hasher.New(hasher.WithWorkers(workers)),
		trasher:   trasher,
	}, nil
}

// DuplicateGroup represents a group of files that are duplicates of each other.
type DuplicateGroup struct {
	Hash  string             // SHA256 hash shared by all files
	Size  int64              // Size shared by all files
	Keep  collector.FileInfo // File to keep (first found)
	Dupes []collector.FileInfo
}

// FindDuplicates analyzes files and identifies duplicates using content hashing.
// Returns a Result containing all duplicate files that should be deleted.
func (d *Deduplicator) FindDuplicates(files []collector.FileInfo) Result {
	return d.FindDuplicatesWithProgress(files, nil)
}

// FindDuplicatesWithProgress analyzes files and reports stage progress.
func (d *Deduplicator) FindDuplicatesWithProgress(files []collector.FileInfo, onProgress func(stage string, processed, total int)) Result {
	result := Result{
		TotalFiles: len(files),
		Operations: make([]DeleteOperation, 0),
	}

	if len(files) == 0 {
		return result
	}

	safeFiles, invalidReadOps := safepath.ValidateReadPaths(d.validator, files, func(file collector.FileInfo, err error) DeleteOperation {
		return DeleteOperation{
			Path:  file.Path,
			Size:  file.Size,
			Error: fmt.Errorf("path escapes root: %w", err),
		}
	})
	result.Operations = append(result.Operations, invalidReadOps...)
	if len(invalidReadOps) > 0 {
		sort.Slice(result.Operations, func(i, j int) bool {
			return result.Operations[i].Path < result.Operations[j].Path
		})
		result.calculateCounts()
		return result
	}

	// Step 1: Group by size (files with unique sizes cannot be duplicates).
	sizeGroups := groupBySize(safeFiles)

	// Step 2: For each size group with multiple files, find duplicates by hash.
	for _, group := range sizeGroups {
		if len(group) < 2 {
			continue
		}

		duplicateGroups := d.findDuplicatesInSizeGroup(group, onProgress)
		deleteTotal := 0
		for i := range duplicateGroups {
			deleteTotal += len(duplicateGroups[i].Dupes)
		}

		deleteProcessed := 0
		for i := range duplicateGroups {
			for j := range duplicateGroups[i].Dupes {
				op := d.deleteFile(duplicateGroups[i].Dupes[j], duplicateGroups[i].Keep.Path, duplicateGroups[i].Hash)
				result.Operations = append(result.Operations, op)
				deleteProcessed++
				progress.EmitStage(onProgress, progressStageDeleting, deleteProcessed, deleteTotal)
			}
		}
	}

	// Sort operations by path for deterministic output.
	sort.Slice(result.Operations, func(i, j int) bool {
		return result.Operations[i].Path < result.Operations[j].Path
	})

	result.calculateCounts()

	return result
}

func (r *Result) calculateCounts() {
	for _, op := range r.Operations {
		if op.OriginalOf != "" {
			r.DuplicatesFound++
		}

		switch {
		case op.Error != nil:
			r.ErrorCount++
		case op.Skipped:
			r.SkippedCount++
		default:
			r.DeletedCount++
			r.BytesRecovered += op.Size
		}
	}
}

// groupBySize groups files by their size.
func groupBySize(files []collector.FileInfo) map[int64][]collector.FileInfo {
	groups := make(map[int64][]collector.FileInfo)
	for _, f := range files {
		groups[f.Size] = append(groups[f.Size], f)
	}
	return groups
}

// findDuplicatesInSizeGroup finds duplicates among files of the same size.
func (d *Deduplicator) findDuplicatesInSizeGroup(files []collector.FileInfo, onProgress func(stage string, processed, total int)) []DuplicateGroup {
	if len(files) < 2 {
		return nil
	}

	size := files[0].Size

	// For small files, go straight to full hash.
	if size <= hasher.SmallFileThreshold {
		return d.findDuplicatesByFullHash(files, onProgress)
	}

	// For larger files, use partial hash first.
	return d.findDuplicatesByPartialThenFullHash(files, onProgress)
}

// findDuplicatesByFullHash groups files by their full SHA256 hash.
func (d *Deduplicator) findDuplicatesByFullHash(files []collector.FileInfo, onProgress func(stage string, processed, total int)) []DuplicateGroup {
	return buildDuplicateGroups(d.groupFilesByHash(files, d.hasher.HashFilesWithSizes, progressStageHashing, onProgress))
}

// findDuplicatesByPartialThenFullHash uses partial hash for initial grouping,
// then confirms with full hash.
func (d *Deduplicator) findDuplicatesByPartialThenFullHash(files []collector.FileInfo, onProgress func(stage string, processed, total int)) []DuplicateGroup {
	partialGroups := d.groupFilesByHash(files, d.hasher.HashPartialFilesWithSizes, progressStageHashing, onProgress)

	// Second pass: for groups with multiple files, confirm with full hash.
	var result []DuplicateGroup
	for _, group := range partialGroups {
		if len(group) < 2 {
			continue
		}
		// These files have matching partial hash - compute full hash to confirm.
		confirmed := d.findDuplicatesByFullHash(group, onProgress)
		result = append(result, confirmed...)
	}

	return result
}

func (d *Deduplicator) groupFilesByHash(files []collector.FileInfo, hashFn func([]hasher.FileToHash) <-chan hasher.HashResult, stage string, onProgress func(stage string, processed, total int)) map[string][]collector.FileInfo {
	hashGroups := make(map[string][]collector.FileInfo)
	toHash := make([]hasher.FileToHash, 0, len(files))
	fileByPath := make(map[string]collector.FileInfo, len(files))

	for _, file := range files {
		fileByPath[file.Path] = file
		toHash = append(toHash, hasher.FileToHash{
			Path: file.Path,
			Size: file.Size,
		})
	}

	processed := 0
	total := len(files)
	for result := range hashFn(toHash) {
		processed++
		progress.EmitStage(onProgress, stage, processed, total)

		if result.Error != nil {
			continue
		}
		file, ok := fileByPath[result.Path]
		if !ok {
			continue
		}
		hashGroups[result.Hash] = append(hashGroups[result.Hash], file)
	}

	return hashGroups
}

// buildDuplicateGroups converts hash groups to DuplicateGroup slice.
func buildDuplicateGroups(hashGroups map[string][]collector.FileInfo) []DuplicateGroup {
	groups := make([]DuplicateGroup, 0, len(hashGroups))

	for hash, files := range hashGroups {
		if len(files) < 2 {
			continue
		}

		// Sort files by path for deterministic "keep" selection.
		sort.Slice(files, func(i, j int) bool {
			return files[i].Path < files[j].Path
		})

		groups = append(groups, DuplicateGroup{
			Hash:  hash,
			Size:  files[0].Size,
			Keep:  files[0],
			Dupes: files[1:],
		})
	}

	return groups
}

// deleteFile creates a delete operation and optionally performs the deletion.
func (d *Deduplicator) deleteFile(file collector.FileInfo, originalPath, hash string) DeleteOperation {
	op := DeleteOperation{
		Path:       file.Path,
		OriginalOf: originalPath,
		Size:       file.Size,
		Hash:       hash,
	}

	// Validate path is within root.
	if err := d.validator.ValidatePathForRead(file.Path); err != nil {
		op.Error = fmt.Errorf("path escapes root: %w", err)
		return op
	}

	// Perform deletion if not dry run.
	if !d.dryRun {
		// Verify the kept file still exists before deleting the duplicate.
		if _, err := os.Lstat(originalPath); err != nil {
			op.Error = fmt.Errorf("kept file missing, refusing to delete duplicate: %w", err)
			return op
		}

		// Re-hash the file to confirm it hasn't changed since initial hash.
		currentHash, err := d.hasher.ComputeHash(file.Path)
		if err != nil {
			op.Error = fmt.Errorf("re-hash before delete: %w", err)
			return op
		}
		if currentHash != hash {
			op.Error = fmt.Errorf("content changed since hashing: %w", ErrContentChanged)
			return op
		}

		op.TrashedTo, op.Error = d.trashOrRemove(file.Path)
	}

	return op
}

// trashOrRemove soft-deletes a file when a trasher is configured, otherwise
// permanently removes it. Returns the trash destination (empty on hard delete).
func (d *Deduplicator) trashOrRemove(path string) (string, error) {
	if d.trasher != nil {
		return d.trasher.TrashWithDest(path)
	}

	if err := d.validator.SafeRemove(path); err != nil {
		return "", fmt.Errorf("failed to delete: %w", err)
	}

	return "", nil
}

// DryRun returns whether the deduplicator is in dry-run mode.
func (d *Deduplicator) DryRun() bool {
	return d.dryRun
}

// Root returns the root directory being validated against.
func (d *Deduplicator) Root() string {
	return d.validator.Root()
}

// ComputeFileHash computes and returns the SHA256 hash of a file.
// Exported for use by callers who need to verify file hashes.
func ComputeFileHash(path string) (string, error) {
	h := hasher.New()
	return h.ComputeHash(path)
}
