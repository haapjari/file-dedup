// Package unzipper extracts zip archives safely within a root directory.
package unzipper

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"file-dedup/pkg/collector"
	"file-dedup/pkg/hasher"
	"file-dedup/pkg/safepath"
	"file-dedup/pkg/trash"
)

const (
	// progressStageExtracting is the stage name reported
	// to progress callbacks during archive extraction.
	progressStageExtracting = "extracting"

	// deflate64Method is the ZIP compression method ID for Deflate64.
	deflate64Method uint16 = 9

	// maxDecompressedSize is the maximum allowed size for
	// a single extracted file (100 GiB). This guards
	// against zip-bomb attacks where a small archive
	// expands to enormous size, while still allowing
	// extraction of very large legitimate files.
	maxDecompressedSize = 100 << 30
)

var (
	// errArchiveEntryPathTraversal is returned when an
	// archive entry name contains path traversal components
	// (e.g., "../") that would escape the extraction
	// directory.
	errArchiveEntryPathTraversal = errors.New("contains path traversal")

	// errArchiveEntryInvalidPath is returned when an archive
	// entry name is malformed: empty, contains NUL bytes,
	// or has degenerate path segments like "." or "".
	errArchiveEntryInvalidPath = errors.New("contains invalid entry path")

	// errArchiveEntrySymlink is returned when an archive entry describes a
	// symlink. Symlink extraction is never allowed because link targets can
	// escape the validated root after extraction.
	errArchiveEntrySymlink = errors.New("symlink archive entries are not supported")

	// windowsVolumePrefixPattern matches Windows drive-volume
	// prefixes (e.g., "C:") at the start of a path string.
	windowsVolumePrefixPattern = regexp.MustCompile(`^[A-Za-z]:`)
)

// ExtractOperation represents a single archive
// extraction operation.
type ExtractOperation struct {
	// ArchivePath is the absolute path to the archive
	// file being extracted.
	ArchivePath string

	// ExtractedFiles is the count of regular files
	// successfully extracted from this archive.
	ExtractedFiles int

	// ExtractedDirs is the count of directories created
	// during extraction.
	ExtractedDirs int

	// SkippedEntries is the count of archive entries that
	// were skipped.
	SkippedEntries int

	// EntryErrors contains error messages for individual
	// entries that failed during extraction.
	EntryErrors []string

	// NestedArchives is the count of archive files
	// discovered within this archive during extraction.
	NestedArchives int

	// ExtractionComplete indicates whether the archive
	// was fully extracted without fatal errors.
	ExtractionComplete bool

	// DeletedArchive indicates whether the source archive
	// was deleted after successful extraction.
	DeletedArchive bool

	// TrashedTo is the trash location path if the archive
	// was soft-deleted (moved to trash).
	TrashedTo string

	// Skipped indicates whether this archive was skipped
	// entirely without attempting extraction.
	Skipped bool

	// SkipReason contains the explanation when Skipped is
	// true (e.g., "not a zip file", "path escape").
	SkipReason string

	// Error contains any fatal error that prevented
	// extraction or archive deletion.
	Error error

	// ReplacedFiles contains files that existed before extraction and were moved
	// to trash to prevent overwrite data loss.
	ReplacedFiles []ReplacedFile
}

// ReplacedFile describes one pre-existing file that was moved to trash before
// an archive entry overwrote its original path.
type ReplacedFile struct {
	OriginalPath string
	TrashedTo    string
	Hash         string
}

// Result contains the aggregated statistics and outcomes from an unzip operation.
// Use this to understand the overall impact of extraction: how many archives were
// found and processed, how many files were extracted, and whether any errors occurred.
// The Operations slice provides detailed per-archive breakdown.
type Result struct {
	// Operations contains detailed results for each individual archive extraction.
	Operations []ExtractOperation

	// TotalFiles is the total number of files scanned in the target directory.
	TotalFiles int

	// ArchivesFound is the number of archive files discovered during collection.
	ArchivesFound int

	// ArchivesProcessed is the number of archives that were attempted for extraction.
	ArchivesProcessed int

	// ExtractedArchives is the count of archives successfully extracted.
	ExtractedArchives int

	// DeletedArchives is the number of archives removed after successful extraction.
	DeletedArchives int

	// ExtractedFiles is the total number of files extracted across all archives.
	ExtractedFiles int

	// ExtractedDirs is the total number of directories created during extraction.
	ExtractedDirs int

	// SkippedCount is the number of archives skipped (e.g., already extracted).
	SkippedCount int

	// ErrorCount is the number of archives that failed to extract.
	ErrorCount int
}

// Unzipper extracts archives recursively while enforcing path containment.
//
// An Unzipper orchestrates the extraction of archive files within a validated root directory.
// It ensures all extracted paths remain within the target directory boundary through safepath
// validation, preventing path traversal attacks and symlink escapes.
//
// Key features:
//   - Recursive extraction: archives within archives are discovered and extracted
//   - Path safety: all extraction paths validated via safepath.Validator
//   - Soft-delete support: optional trash integration for reversible archive removal
//   - Dry-run mode: preview extraction without modifying the filesystem
//   - Progress tracking: reports extraction progress through callback functions
//
// Usage:
//
//	// Create with automatic validator
//	uz, err := unzipper.New("/path/to/target", false)
//
//	// Or with existing validator and trash support
//	uz, err := unzipper.NewWithValidator(validator, false, trasher)
//
//	// Extract archives with progress tracking
//	result := uz.ExtractArchivesWithProgress(files, func(stage string, processed, total int) {
//	    fmt.Printf("Stage: %s, Progress: %d/%d\n", stage, processed, total)
//	})
//
// Safety guarantees:
//   - All extraction paths validated before creation
//   - Symlinks that escape root directory are rejected
//   - Archive entries with path traversal attempts (../) are blocked
//   - Overwrite protection available through configuration
//   - Optional soft-delete moves archives to trash instead of permanent deletion
//
// The Unzipper is safe for concurrent use within different root directories,
// but should not be shared across goroutines for the same extraction operation.
type Unzipper struct {
	// dryRun when true prevents all filesystem modifications.
	// Extraction logic executes but no files are created, moved, or deleted.
	// Use this to preview what would happen before committing changes.
	dryRun bool

	// validator enforces path containment for all extraction operations.
	// Every extracted file path is validated to ensure it stays within the
	// root directory. This prevents malicious archives from writing outside
	// the target directory through techniques like path traversal or symlinks.
	validator *safepath.Validator

	// trasher enables soft-delete (reversible removal) of archives after extraction.
	// When set, successfully extracted archives are moved to .file-dedup/trash/<run-id>/
	// instead of being permanently deleted. This allows undo operations.
	// If nil, archives are permanently deleted when deletion is requested.
	trasher *trash.Trasher
}

// New creates an Unzipper rooted at rootDir.
func New(rootDir string, dryRun bool) (*Unzipper, error) {
	validator, err := safepath.New(rootDir)
	if err != nil {
		return nil, fmt.Errorf("failed to create path validator: %w", err)
	}

	return NewWithValidator(validator, dryRun, nil)
}

// NewWithValidator creates an Unzipper with an existing validator.
// An optional trasher enables soft-delete (move to trash) instead of permanent removal.
func NewWithValidator(
	validator *safepath.Validator,
	dryRun bool,
	trasher *trash.Trasher,
) (*Unzipper, error) {
	if validator == nil {
		return nil, errors.New("validator is required")
	}

	return &Unzipper{
		dryRun:    dryRun,
		validator: validator,
		trasher:   trasher,
	}, nil
}

// ExtractArchivesWithProgressRecursively extracts all archive files from the
// provided file list, then re-scans the directory to discover and extract any
// nested archives that were contained within the originals. This process repeats
// until no more archives remain.
//
// The progress callback is invoked during extraction to report the current stage,
// number of archives processed, and total archive count. Pass nil to disable
// progress reporting.
//
// It determines the root directory from the common ancestor of all provided files
// and returns an empty [Result] if the file list is empty. On each iteration,
// only archive files are selected for extraction; after extraction, the directory
// is re-collected to find any newly revealed archives.
//
// Returns the aggregated [Result] and any error encountered during extraction or
// file collection.
func (u *Unzipper) ExtractArchivesWithProgressRecursively(
	files []collector.FileInfo,
	progress func(stage string, processed, total int),
) (Result, error) {
	res := Result{TotalFiles: len(files)}
	rootDir := getRootDirectory(files)

	if rootDir == "" {
		return res, nil
	}

	processed := make(map[string]bool)

	for {
		archives := filterNewArchives(files, processed)
		if len(archives) == 0 {
			break
		}

		res.ArchivesFound += len(archives)

		if err := u.extractBatch(archives, processed, progress, &res); err != nil {
			return res, err
		}

		var err error
		files, err = getAllFilesRecursively(rootDir)
		if err != nil {
			return res, err
		}

		res.TotalFiles = len(files)
	}

	return res, nil
}

// filterNewArchives returns archives from files that have not yet been processed.
func filterNewArchives(files []collector.FileInfo, processed map[string]bool) []collector.FileInfo {
	archives := filterOnlyArchives(files)

	unprocessed := make([]collector.FileInfo, 0, len(archives))
	for _, a := range archives {
		key := filepath.Join(a.Dir, a.Name)
		if !processed[key] {
			unprocessed = append(unprocessed, a)
		}
	}

	return unprocessed
}

// extractBatch processes a batch of archives, updating the result and processed map.
func (u *Unzipper) extractBatch(
	archives []collector.FileInfo,
	processed map[string]bool,
	progress func(stage string, processed, total int),
	res *Result,
) error {
	for i, archive := range archives {
		if progress != nil {
			progress(progressStageExtracting, i, len(archives))
		}

		archivePath := filepath.Join(archive.Dir, archive.Name)
		processed[archivePath] = true

		op, err := u.processArchive(archive, archivePath)
		res.ArchivesProcessed++

		if err != nil {
			res.ErrorCount++
			res.Operations = append(res.Operations, op)
			return err
		}

		if op.Skipped {
			res.SkippedCount++
			res.Operations = append(res.Operations, op)
			continue
		}

		res.ExtractedFiles += op.ExtractedFiles
		res.ExtractedDirs += op.ExtractedDirs
		if op.SkippedEntries > 0 {
			res.ErrorCount++
			res.Operations = append(res.Operations, op)
			continue
		}

		res.ExtractedArchives++

		if !u.dryRun {
			op.DeletedArchive = true
			res.DeletedArchives++
		}
		res.Operations = append(res.Operations, op)
	}

	if progress != nil {
		progress(progressStageExtracting, len(archives), len(archives))
	}

	return nil
}

// processArchive extracts or inspects a single archive, then removes it if not in dry-run mode.
// processArchive handles a single archive file by either inspecting it (dry-run)
// or extracting its contents to the archive's parent directory. In non-dry-run mode,
// the source archive is removed after successful extraction — moved to trash if a
// trasher is configured, or permanently deleted otherwise.
//
// The returned [ExtractOperation] contains extraction statistics (files, dirs,
// nested archives) and, when applicable, the trash destination path. If extraction
// or archive removal fails, the partial operation result is returned alongside the
// error, with op.Error set to the cause.
func (u *Unzipper) processArchive(archive collector.FileInfo, archivePath string) (ExtractOperation, error) {
	var op ExtractOperation
	var err error

	if u.dryRun {
		op, err = inspectArchiveWithValidator(archive, u.validator)
	} else {
		op, err = unzipWithValidator(archive, u.validator, u.trasher)
	}

	if err != nil {
		if errors.Is(err, zip.ErrAlgorithm) {
			op.Skipped = true
			op.SkipReason = err.Error()
			op.Error = nil
			return op, nil
		}

		return op, err
	}

	if !u.dryRun && op.ExtractionComplete {
		trashedTo, rmErr := u.removeArchive(archivePath)
		if rmErr != nil {
			op.Error = rmErr
			return op, rmErr
		}
		op.TrashedTo = trashedTo
	}

	return op, nil
}

// removeArchive deletes or trashes the archive at the given path.
// Returns the trash destination path (non-empty only when using a trasher).
func (u *Unzipper) removeArchive(archivePath string) (string, error) {
	if u.trasher != nil {
		dest, err := u.trasher.TrashWithDest(archivePath)
		if err != nil {
			return "", fmt.Errorf("failed to trash archive %s: %w", archivePath, err)
		}
		return dest, nil
	}

	if err := os.Remove(archivePath); err != nil {
		return "", fmt.Errorf("failed to remove archive %s: %w", archivePath, err)
	}

	return "", nil
}

// getRootDirectory computes the lowest common ancestor directory for a slice of
// [collector.FileInfo]. It iteratively walks up the directory tree until it finds
// a path that is a parent of (or equal to) every file's directory. Returns an
// empty string if the slice is empty.
func getRootDirectory(f []collector.FileInfo) string {
	if len(f) == 0 {
		return ""
	}

	root := f[0].Dir
	for _, fi := range f[1:] {
		for !isSubPath(root, fi.Dir) {
			parent := filepath.Dir(root)
			if parent == root {
				return root
			}
			root = parent
		}
	}

	return root
}

// isSubPath reports whether child is equal to or nested under parent.
func isSubPath(parent, child string) bool {
	return child == parent || strings.HasPrefix(child, parent+string(filepath.Separator))
}

// filterOnlyArchives filters a slice of FileInfo, returning only entries whose
// filenames are recognized as archive formats.
func filterOnlyArchives(blob []collector.FileInfo) []collector.FileInfo {
	filteredBlob := make([]collector.FileInfo, 0)
	for _, f := range blob {
		if ok := isArchive(filepath.Join(f.Dir, f.Name)); ok {
			filteredBlob = append(filteredBlob, f)
		}
	}

	return filteredBlob
}

// normalizeArchiveEntryPath converts an archive entry name to a canonical
// forward-slash path by applying [filepath.ToSlash] and replacing any
// remaining literal backslash sequences with forward slashes.
func normalizeArchiveEntryPath(entryName string) string {
	return strings.ReplaceAll(filepath.ToSlash(entryName), `\\`, "/")
}

// hasWindowsVolumePrefix reports whether pathName starts with a Windows
// drive-volume prefix (e.g., "C:").
func hasWindowsVolumePrefix(pathName string) bool {
	return windowsVolumePrefixPattern.MatchString(pathName)
}

// validateArchiveEntryPath checks that entryName is a safe, relative file path
// suitable for extraction. It rejects absolute paths, traversal segments,
// Windows drive-volume prefixes, empty path elements, and NUL bytes.
// Returns [errArchiveEntryPathTraversal] for traversal-like entries and
// [errArchiveEntryInvalidPath] for malformed names.
func validateArchiveEntryPath(entryName string) error {
	// Normalize to forward-slash canonical form so all checks work uniformly
	// regardless of the originating OS (Windows backslashes, etc.).
	normalized := normalizeArchiveEntryPath(entryName)
	if normalized == "" {
		return errArchiveEntryInvalidPath
	}

	// Reject absolute paths and Windows drive prefixes (e.g. "C:") — extracted
	// files must always resolve relative to the extraction directory.
	if strings.HasPrefix(normalized, "/") || hasWindowsVolumePrefix(normalized) {
		return errArchiveEntryPathTraversal
	}

	// NUL bytes can truncate paths at the OS level, potentially bypassing
	// later validation checks. Reject them outright.
	if strings.ContainsRune(normalized, '\x00') {
		return errArchiveEntryInvalidPath
	}

	// Strip trailing slashes so directory entries (e.g. "foo/bar/") don't
	// produce empty final segments during the per-component check below.
	trimmed := strings.TrimRight(normalized, "/")
	if trimmed == "" {
		return errArchiveEntryInvalidPath
	}

	// Walk each path component individually to catch traversal ("..") and
	// degenerate segments ("", ".") that could escape or confuse extraction.
	for part := range strings.SplitSeq(trimmed, "/") {
		switch part {
		case "..":
			return errArchiveEntryPathTraversal
		case "", ".":
			return errArchiveEntryInvalidPath
		}
	}

	// Use path.Clean as a second line of defense: it collapses redundant
	// separators and resolves "." / ".." sequences that might have slipped
	// through the component-level check above.
	cleanPath := path.Clean(trimmed)

	// After cleaning, the path must still be a proper relative path — not ".",
	// not absolute, and not a Windows volume root.
	if cleanPath == "." || strings.HasPrefix(cleanPath, "/") || hasWindowsVolumePrefix(cleanPath) {
		return errArchiveEntryInvalidPath
	}

	// Final traversal guard: even after cleaning, ensure the result doesn't
	// escape upward via ".." at the start of the resolved path.
	if cleanPath == ".." || strings.HasPrefix(cleanPath, "../") {
		return errArchiveEntryPathTraversal
	}

	return nil
}

// resolveArchiveEntryPath validates and resolves an archive entry name to a safe
// absolute filesystem path under baseDir. It first checks the entry name for path
// traversal attempts, then normalizes platform-specific separators and joins the
// result with baseDir. When a [safepath.Validator] is provided, the resolved path
// is additionally validated to ensure it remains within the allowed root directory.
// Returns the resolved absolute path or an error if the entry name is invalid or
// escapes the target directory.
func resolveArchiveEntryPath(
	baseDir string,
	entryName string,
	validator *safepath.Validator,
) (string, error) {
	if err := validateArchiveEntryPath(entryName); err != nil {
		return "", err
	}

	normalized := normalizeArchiveEntryPath(entryName)
	targetPath := filepath.Join(baseDir, filepath.FromSlash(normalized))

	if validator != nil {
		if err := validator.ValidatePathForWrite(targetPath); err != nil {
			return "", fmt.Errorf("%w: %w", errArchiveEntryPathTraversal, err)
		}
	}

	return targetPath, nil
}

// unzip extracts all entries from the zip archive identified by file into the
// same directory that contains the archive. It creates directories as needed and
// writes regular files with their archived content. Archive entries containing
// path traversal components (e.g., "../") are rejected to prevent zip-slip
// attacks. Returns an ExtractOperation describing what was extracted and any
// error encountered.
func unzip(file collector.FileInfo) (ExtractOperation, error) {
	return unzipWithValidator(file, nil, nil)
}

// unzipWithValidator extracts all entries from the zip archive identified by file
// into the archive's parent directory, optionally enforcing path containment via
// the provided [safepath.Validator]. When validator is non-nil, every resolved
// extraction path is checked to ensure it remains within the allowed root
// directory; a nil validator skips this additional check (basic path-traversal
// validation is still performed by [resolveArchiveEntryPath]).
//
// For each archive entry, directories are created with their archived permission
// bits (ORed with 0o755), and regular files are written via [extractFile]. Any
// extracted file that is itself a recognized archive format increments the
// NestedArchives counter in the returned [ExtractOperation].
//
// Extraction stops on the first error encountered and returns both the partial
// operation result and the error.
func unzipWithValidator(
	file collector.FileInfo,
	validator *safepath.Validator,
	trasher *trash.Trasher,
) (ExtractOperation, error) {
	archivePath := filepath.Join(file.Dir, file.Name)
	op := ExtractOperation{ArchivePath: archivePath}

	r, err := openArchiveReader(archivePath)
	if err != nil {
		op.Error = fmt.Errorf("failed to open archive %s: %w", archivePath, err)
		return op, op.Error
	}
	defer func() {
		_ = r.Close()
	}()

	if methodErr := validateCompressionMethods(r.files); methodErr != nil {
		op.Error = methodErr
		return op, op.Error
	}

	for _, entry := range r.files {
		entryErr := extractArchiveEntry(file, entry, validator, trasher, &op)
		if entryErr != nil {
			op.Error = entryErr
			return op, op.Error
		}
	}

	if op.SkippedEntries > 0 {
		return op, nil
	}

	op.ExtractionComplete = true
	return op, nil
}

// extractArchiveEntry extracts a single zip entry into the directory containing
// the source archive. For directory entries, it creates the target directory with
// the archived permission bits (ORed with 0o755). For regular files, it ensures
// the parent directory exists, backs up any pre-existing file at the target path
// to trash (when a [trash.Trasher] is configured), and writes the entry content
// via [extractFile]. Extracted files that are themselves recognized archive
// formats increment op.NestedArchives for later recursive processing.
//
// All resolved paths are validated through the provided [safepath.Validator] to
// prevent path traversal and symlink escape attacks. Entry-open failures are
// recorded as skipped entries so one malformed file does not abort the full run;
// safety and filesystem mutation failures remain fatal.
func extractArchiveEntry(
	file collector.FileInfo,
	entry *zip.File,
	validator *safepath.Validator,
	trasher *trash.Trasher,
	op *ExtractOperation,
) error {
	// Resolve the archive entry name to a safe absolute path under the archive's
	// parent directory, validating against path traversal and symlink escapes.
	targetPath, pathErr := resolveArchiveEntryPath(file.Dir, entry.Name, validator)
	if pathErr != nil {
		return fmt.Errorf("illegal entry path %q: %w", entry.Name, pathErr)
	}
	if entry.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("illegal entry %q: %w", entry.Name, errArchiveEntrySymlink)
	}

	// If the entry is a directory, create it with the archived permissions
	// (ensuring at least rwxr-xr-x via OR with 0o755) and return early.
	if entry.FileInfo().IsDir() {
		if mkErr := os.MkdirAll(targetPath, entry.Mode().Perm()|0o755); mkErr != nil {
			return fmt.Errorf("failed to create directory %s: %w", targetPath, mkErr)
		}
		op.ExtractedDirs++
		return nil
	}

	rc, openErr := entry.Open()
	if openErr != nil {
		recordSkippedEntry(op, entry, fmt.Errorf("failed to open entry: %w", openErr))
		return nil
	}
	defer func() {
		_ = rc.Close()
	}()

	// For regular files, ensure the parent directory exists before writing.
	parentDir := filepath.Dir(targetPath)
	if mkErr := os.MkdirAll(parentDir, 0o755); mkErr != nil {
		return fmt.Errorf("failed to create parent directory %s: %w", parentDir, mkErr)
	}

	// If a file already exists at the target path and a trasher is configured,
	// move the existing file to trash so it can be recovered via undo.
	replaced, replacedFile, replaceErr := backupExistingFile(targetPath, trasher)
	if replaceErr != nil {
		return fmt.Errorf("failed to backup existing target %s: %w", targetPath, replaceErr)
	}
	// Track any replaced file in the operation result for journaling/undo support.
	if replaced {
		op.ReplacedFiles = append(op.ReplacedFiles, replacedFile)
	}

	// Decompress and write the archive entry contents to the target path.
	if writeErr := extractOpenFile(entry, rc, targetPath, maxDecompressedSize); writeErr != nil {
		return fmt.Errorf("failed to extract %s: %w", entry.Name, writeErr)
	}
	op.ExtractedFiles++

	// Check if the newly extracted file is itself an archive, so the caller
	// can schedule it for recursive extraction in a subsequent pass.
	if isArchive(targetPath) {
		op.NestedArchives++
	}

	return nil
}

func recordSkippedEntry(op *ExtractOperation, entry *zip.File, err error) {
	op.SkippedEntries++
	op.EntryErrors = append(op.EntryErrors, fmt.Sprintf(
		"%s (method=%d %s compressed=%d uncompressed=%d): %v",
		entry.Name,
		entry.Method,
		compressionMethodName(entry.Method),
		entry.CompressedSize64,
		entry.UncompressedSize64,
		err,
	))
}

// backupExistingFile moves an existing extraction target to trash before
// overwrite so the original bytes are recoverable.
func backupExistingFile(targetPath string, trasher *trash.Trasher) (bool, ReplacedFile, error) {
	if trasher == nil {
		return false, ReplacedFile{}, nil
	}

	info, err := os.Lstat(targetPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, ReplacedFile{}, nil
	}
	if err != nil {
		return false, ReplacedFile{}, err
	}
	if info.IsDir() {
		return false, ReplacedFile{}, errors.New("target exists as directory")
	}

	h := hasher.New()
	originalHash, err := h.ComputeHash(targetPath)
	if err != nil {
		return false, ReplacedFile{}, fmt.Errorf("hash existing target: %w", err)
	}

	trashedTo, err := trasher.TrashWithDest(targetPath)
	if err != nil {
		return false, ReplacedFile{}, err
	}

	return true, ReplacedFile{
		OriginalPath: targetPath,
		TrashedTo:    trashedTo,
		Hash:         originalHash,
	}, nil
}

// inspectArchiveWithValidator performs a dry-run inspection of the zip archive
// identified by file, validating all entry paths against the provided
// [safepath.Validator] without extracting any content to disk. It counts the
// number of regular files and directories contained in the archive, returning
// an [ExtractOperation] with ExtractionComplete set to true on success.
//
// Inspection stops on the first invalid entry path or archive open failure,
// returning both the partial operation result and the error. This is used in
// dry-run mode to preview what an extraction would produce.
func inspectArchiveWithValidator(file collector.FileInfo, validator *safepath.Validator) (ExtractOperation, error) {
	archivePath := filepath.Join(file.Dir, file.Name)
	op := ExtractOperation{ArchivePath: archivePath}

	r, err := openArchiveReader(archivePath)
	if err != nil {
		op.Error = fmt.Errorf("failed to open archive %s: %w", archivePath, err)
		return op, op.Error
	}
	defer func() {
		_ = r.Close()
	}()

	if methodErr := validateCompressionMethods(r.files); methodErr != nil {
		op.Error = methodErr
		return op, op.Error
	}

	for _, entry := range r.files {
		if _, pathErr := resolveArchiveEntryPath(file.Dir, entry.Name, validator); pathErr != nil {
			op.Error = fmt.Errorf("illegal entry path %q: %w", entry.Name, pathErr)
			return op, op.Error
		}
		if entry.Mode()&os.ModeSymlink != 0 {
			op.Error = fmt.Errorf("illegal entry %q: %w", entry.Name, errArchiveEntrySymlink)
			return op, op.Error
		}

		if entry.FileInfo().IsDir() {
			op.ExtractedDirs++
			continue
		}

		op.ExtractedFiles++
	}

	op.ExtractionComplete = true
	return op, nil
}

// extractFile writes a single zip file entry to targetPath. It opens the
// compressed entry for reading, creates the destination file, and copies the
// content. The destination file receives the permission bits stored in the
// archive entry. Extraction is limited to [maxDecompressedSize] bytes to
// prevent decompression bombs.
func extractFile(entry *zip.File, targetPath string) error {
	return extractFileWithLimit(entry, targetPath, maxDecompressedSize)
}

func extractFileWithLimit(entry *zip.File, targetPath string, maxSize int64) error {
	rc, err := entry.Open()
	if err != nil {
		return fmt.Errorf("failed to open entry: %w", err)
	}
	defer func() {
		_ = rc.Close()
	}()

	return extractOpenFile(entry, rc, targetPath, maxSize)
}

func extractOpenFile(entry *zip.File, rc io.Reader, targetPath string, maxSize int64) error {
	outFile, err := os.OpenFile(targetPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, entry.Mode().Perm())
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}

	if err = copyWithDecompressedSizeLimit(outFile, rc, maxSize); err != nil {
		_ = outFile.Close()
		return err
	}

	return outFile.Close()
}

func copyWithDecompressedSizeLimit(dst io.Writer, src io.Reader, maxSize int64) error {
	written, err := io.Copy(dst, io.LimitReader(src, maxSize))
	if err != nil {
		return fmt.Errorf("failed to write file content: %w", err)
	}
	if written < maxSize {
		return nil
	}

	var extra [1]byte
	n, err := src.Read(extra[:])
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("failed to verify decompressed size: %w", err)
	}
	if n > 0 {
		return fmt.Errorf("decompressed file exceeds maximum size of %d bytes", maxSize)
	}

	return nil
}

// validateCompressionMethods checks that all non-directory entries in the archive
// use a supported compression method. It returns an error
// wrapping [zip.ErrAlgorithm] for the first entry that uses an unsupported method,
// or nil if all entries are compatible.
func validateCompressionMethods(entries []*zip.File) error {
	for _, entry := range entries {
		if entry.FileInfo().IsDir() {
			continue
		}

		if isCompressionMethodSupported(entry.Method) {
			continue
		}

		return fmt.Errorf(
			"entry %q uses unsupported compression method %d (%s): %w",
			entry.Name,
			entry.Method,
			compressionMethodName(entry.Method),
			zip.ErrAlgorithm,
		)
	}

	return nil
}

// isCompressionMethodSupported reports whether method is a zip compression
// algorithm that this package can extract.
func isCompressionMethodSupported(method uint16) bool {
	return method == zip.Store || method == zip.Deflate || method == deflate64Method
}

// compressionMethodName returns a human-readable name for a zip compression
// method code. Known methods include "store" (0), "deflate" (8), and
// "deflate64" (9). Unrecognized method codes return "unknown".
func compressionMethodName(method uint16) string {
	switch method {
	case zip.Store:
		return "store"
	case zip.Deflate:
		return "deflate"
	case deflate64Method:
		return "deflate64"
	default:
		return "unknown"
	}
}

// isArchive reports whether filePath is a .zip file that can be opened as a ZIP
// archive. Non-.zip files may contain ZIP-like bytes for application-specific
// formats and must not be treated as extraction candidates.
func isArchive(filePath string) bool {
	if !strings.EqualFold(filepath.Ext(filePath), ".zip") {
		return false
	}

	r, err := openArchiveReader(filePath)
	if err != nil {
		slog.Debug("skipped a file", "path", filePath, "error", err)
		return false
	}
	_ = r.Close()

	return true
}

// getAllFilesRecursively collects all files under rootDir, skipping the .file-dedup
// metadata directory. It returns a slice of FileInfo for every regular file found.
func getAllFilesRecursively(rootDir string) ([]collector.FileInfo, error) {
	c := collector.New(collector.Options{
		SkipDirs: []string{".file-dedup"},
	})

	files, err := c.Collect(rootDir)
	if err != nil {
		return nil, fmt.Errorf("failed to collect files: %w", err)
	}

	return files, nil
}
