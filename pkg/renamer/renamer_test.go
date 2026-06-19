package renamer

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"file-dedup/internal/testutil"
	"file-dedup/pkg/collector"
	"file-dedup/pkg/metadata"
	"file-dedup/pkg/safepath"
	"file-dedup/pkg/trash"
)

// createTestFile creates a file with specific modification time.
func createTestFile(t *testing.T, dir, name string, modTime time.Time) {
	t.Helper()

	createTestFileWithContent(t, dir, name, "test content", modTime)
}

func createTestFileWithContent(t *testing.T, dir, name, content string, modTime time.Time) {
	t.Helper()
	testutil.CreateFileWithModTime(t, filepath.Join(dir, name), content, modTime)
}

func collectFiles(t *testing.T, root string) []collector.FileInfo {
	t.Helper()

	c := collector.New(collector.Options{})
	files, err := c.Collect(root)
	require.NoError(t, err)

	return files
}

func TestRenamer_RenameFiles_DryRun(t *testing.T) {
	tmpDir := t.TempDir()

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	createTestFile(t, tmpDir, "My Document.pdf", modTime)

	files := collectFiles(t, tmpDir)
	require.Len(t, files, 1)

	r, err := New(tmpDir, true) // dry run
	require.NoError(t, err)
	result := r.RenameFiles(files)

	assert.Equal(t, 1, result.TotalFiles)
	assert.Equal(t, 1, result.RenamedCount)
	assert.Equal(t, 0, result.SkippedCount)
	assert.Equal(t, 0, result.ErrorCount)

	// Verify file was NOT actually renamed (dry run)
	_, err = os.Stat(filepath.Join(tmpDir, "My Document.pdf"))
	require.NoError(t, err, "original file should still exist in dry run")

	// Verify operation details
	require.Len(t, result.Operations, 1)
	op := result.Operations[0]
	assert.Equal(t, "My Document.pdf", op.OriginalName)
	assert.Equal(t, "2018-06-15_my_document.pdf", op.NewName)
	assert.False(t, op.Skipped)
	assert.NoError(t, op.Error)
}

func TestNew_InvalidRootReturnsError(t *testing.T) {
	_, err := New(filepath.Join(t.TempDir(), "missing"), true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to create path validator")
}

func TestRenamer_RenameFiles_InvalidReadPathCountsError(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	path := filepath.Join(outside, "escape.txt")
	testutil.CreateFile(t, path, "escape")

	validator, err := safepath.New(root)
	require.NoError(t, err)
	r, err := NewWithValidator(validator, true, nil)
	require.NoError(t, err)

	var progressCalls []int
	result := r.RenameFilesWithProgress([]collector.FileInfo{{
		Dir:  outside,
		Name: "escape.txt",
		Path: path,
	}}, func(processed, total int) {
		progressCalls = append(progressCalls, processed, total)
	})

	assert.Equal(t, 1, result.TotalFiles)
	assert.Equal(t, 1, result.ErrorCount)
	assert.Equal(t, []int{1, 1}, progressCalls)
	require.Len(t, result.Operations, 1)
	assert.ErrorContains(t, result.Operations[0].Error, "source path escapes root")
}

func TestRenamer_SameContentReportsHashErrors(t *testing.T) {
	root := t.TempDir()
	filePath := filepath.Join(root, "file.txt")
	testutil.CreateFile(t, filePath, "content")
	dirPath := filepath.Join(root, "dir")
	require.NoError(t, os.Mkdir(dirPath, 0o755))

	r, err := New(root, true)
	require.NoError(t, err)

	same, err := r.sameContent(dirPath, filePath)
	assert.False(t, same)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to hash")

	same, err = r.sameContent(filePath, dirPath)
	assert.False(t, same)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to hash")
}

func TestRenamer_RenameFiles_ActualRename(t *testing.T) {
	tmpDir := t.TempDir()

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	createTestFile(t, tmpDir, "My Document.pdf", modTime)

	files := collectFiles(t, tmpDir)
	require.Len(t, files, 1)

	r, err := New(tmpDir, false) // actual rename
	require.NoError(t, err)
	result := r.RenameFiles(files)

	assert.Equal(t, 1, result.TotalFiles)
	assert.Equal(t, 1, result.RenamedCount)
	assert.Equal(t, 0, result.ErrorCount)

	// Verify file WAS renamed
	_, err = os.Stat(filepath.Join(tmpDir, "My Document.pdf"))
	assert.True(t, os.IsNotExist(err), "original file should not exist after rename")

	_, err = os.Stat(filepath.Join(tmpDir, "2018-06-15_my_document.pdf"))
	assert.NoError(t, err, "renamed file should exist")
}

func TestRenamer_RenameFiles_AlreadyNamedWithDatePrefix(t *testing.T) {
	tmpDir := t.TempDir()

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	// Create a file that already has a date prefix - it will get another one
	// because GenerateTimestampedName always adds the date prefix
	createTestFile(t, tmpDir, "2018-06-15_document.pdf", modTime)

	files := collectFiles(t, tmpDir)
	require.Len(t, files, 1)

	r, err := New(tmpDir, false)
	require.NoError(t, err)
	result := r.RenameFiles(files)

	assert.Equal(t, 1, result.TotalFiles)
	assert.Equal(t, 0, result.RenamedCount)
	assert.Equal(t, 1, result.SkippedCount)

	require.Len(t, result.Operations, 1)
	assert.Equal(t, "2018-06-15_document.pdf", result.Operations[0].NewName)
	assert.True(t, result.Operations[0].Skipped)
	assert.Equal(t, "name unchanged", result.Operations[0].SkipReason)
}

func TestRenamer_RenameFiles_TBDPrefixSkipped(t *testing.T) {
	tmpDir := t.TempDir()

	modTime := time.Date(2019, 6, 15, 12, 0, 0, 0, time.UTC)
	createTestFile(t, tmpDir, "2019-TBD-TBD_document.pdf", modTime)

	files := collectFiles(t, tmpDir)
	require.Len(t, files, 1)

	r, err := New(tmpDir, false)
	require.NoError(t, err)
	result := r.RenameFiles(files)

	assert.Equal(t, 1, result.TotalFiles)
	assert.Equal(t, 0, result.RenamedCount)
	assert.Equal(t, 1, result.SkippedCount)
	assert.Equal(t, 0, result.ErrorCount)

	require.Len(t, result.Operations, 1)
	op := result.Operations[0]
	assert.True(t, op.Skipped)
	assert.Equal(t, "already has TBD prefix", op.SkipReason)

	_, err = os.Stat(filepath.Join(tmpDir, "2019-TBD-TBD_document.pdf"))
	require.NoError(t, err, "file with TBD prefix should remain")
}

func TestRenamer_RenameFiles_DoubleDatePrefixCollapsed(t *testing.T) {
	tmpDir := t.TempDir()

	modTime := time.Date(2025, 1, 1, 8, 0, 0, 0, time.UTC)
	createTestFile(t, tmpDir, "2025-01-01_2025-01-01_report.pdf", modTime)

	files := collectFiles(t, tmpDir)
	require.Len(t, files, 1)

	r, err := New(tmpDir, true)
	require.NoError(t, err)
	result := r.RenameFiles(files)

	assert.Equal(t, 1, result.TotalFiles)
	assert.Equal(t, 1, result.RenamedCount)
	assert.Equal(t, 0, result.SkippedCount)

	require.Len(t, result.Operations, 1)
	op := result.Operations[0]
	assert.Equal(t, "2025-01-01_report.pdf", op.NewName)
	assert.False(t, op.Skipped)
	assert.NoError(t, op.Error)
}

func TestRenamer_RenameFiles_HandleConflicts(t *testing.T) {
	tmpDir := t.TempDir()

	// Create two files with same name but different content
	// They will have the same sanitized name
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	createTestFileWithContent(t, tmpDir, "Document.pdf", "content-a", modTime)
	createTestFileWithContent(t, tmpDir, "document.pdf", "content-bb", modTime) // different case, size

	files := collectFiles(t, tmpDir)
	require.Len(t, files, 2)

	r, err := New(tmpDir, true) // dry run first
	require.NoError(t, err)
	result := r.RenameFiles(files)

	assert.Equal(t, 2, result.TotalFiles)

	// One should be normal, one should have suffix
	names := make(map[string]bool)
	for _, op := range result.Operations {
		names[op.NewName] = true
	}

	assert.True(t, names["2018-06-15_document.pdf"], "should have base name")
	assert.True(t, names["2018-06-15_document_1.pdf"], "should have suffixed name")
}

func TestRenamer_RenameFiles_SameSizeDifferentContent_BatchKeepsBoth(t *testing.T) {
	tmpDir := t.TempDir()

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	createTestFileWithContent(t, tmpDir, "Photo.jpg", "alpha-123", modTime)
	createTestFileWithContent(t, tmpDir, "photo.jpg", "omega-12", modTime)

	files := collectFiles(t, tmpDir)
	require.Len(t, files, 2)

	r, err := New(tmpDir, false)
	require.NoError(t, err)
	result := r.RenameFiles(files)

	assert.Equal(t, 2, result.TotalFiles)
	assert.Equal(t, 2, result.RenamedCount)
	assert.Equal(t, 0, result.SkippedCount)
	assert.Equal(t, 0, result.DeletedCount)
	assert.Equal(t, 0, result.ErrorCount)

	basePath := filepath.Join(tmpDir, "2018-06-15_photo.jpg")
	suffixPath := filepath.Join(tmpDir, "2018-06-15_photo_1.jpg")

	_, err = os.Stat(basePath)
	require.NoError(t, err)
	_, err = os.Stat(suffixPath)
	require.NoError(t, err)

	baseContent, err := os.ReadFile(basePath)
	require.NoError(t, err)
	suffixContent, err := os.ReadFile(suffixPath)
	require.NoError(t, err)

	assert.NotEqual(t, string(baseContent), string(suffixContent))
}

func TestRenamer_RenameFiles_RemovesDuplicateInBatch(t *testing.T) {
	tmpDir := t.TempDir()

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	createTestFileWithContent(t, tmpDir, "Report.pdf", "same-content", modTime)
	createTestFileWithContent(t, tmpDir, "report.pdf", "same-content", modTime)

	files := collectFiles(t, tmpDir)
	require.Len(t, files, 2)

	r, err := New(tmpDir, false)
	require.NoError(t, err)
	result := r.RenameFiles(files)

	assert.Equal(t, 2, result.TotalFiles)
	assert.Equal(t, 1, result.RenamedCount)
	assert.Equal(t, 0, result.SkippedCount)
	assert.Equal(t, 1, result.DeletedCount)
	assert.Equal(t, 0, result.ErrorCount)
	require.Len(t, result.Operations, 2)

	var renamedOp, duplicateOp RenameOperation
	for _, op := range result.Operations {
		if op.Deleted {
			duplicateOp = op
			continue
		}
		renamedOp = op
	}
	assert.Equal(t, filepath.Join(tmpDir, "2018-06-15_report.pdf"), renamedOp.NewPath)
	assert.False(t, renamedOp.Skipped)
	assert.False(t, renamedOp.Deleted)
	assert.True(t, duplicateOp.Skipped)
	assert.Equal(t, "duplicate file already exists", duplicateOp.SkipReason)
	assert.True(t, duplicateOp.Deleted)

	entries, err := os.ReadDir(tmpDir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "2018-06-15_report.pdf", entries[0].Name())
}

func TestRenamer_RenameFiles_RemovesDuplicateTarget(t *testing.T) {
	tmpDir := t.TempDir()

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	createTestFile(t, tmpDir, "My Doc.pdf", modTime)
	createTestFile(t, tmpDir, "2018-06-15_my_doc.pdf", modTime)

	originalPath := filepath.Join(tmpDir, "My Doc.pdf")
	info, err := os.Stat(originalPath)
	require.NoError(t, err)

	files := []collector.FileInfo{
		{
			Path:    originalPath,
			Dir:     tmpDir,
			Name:    "My Doc.pdf",
			Size:    info.Size(),
			ModTime: info.ModTime(),
		},
	}

	r, err := New(tmpDir, false)
	require.NoError(t, err)
	result := r.RenameFiles(files)

	assert.Equal(t, 1, result.TotalFiles)
	assert.Equal(t, 0, result.RenamedCount)
	assert.Equal(t, 0, result.SkippedCount)
	assert.Equal(t, 1, result.DeletedCount)
	assert.Equal(t, 0, result.ErrorCount)

	require.Len(t, result.Operations, 1)
	op := result.Operations[0]
	assert.True(t, op.Skipped)
	assert.Equal(t, "duplicate file already exists", op.SkipReason)
	assert.True(t, op.Deleted)

	_, err = os.Stat(originalPath)
	assert.True(t, os.IsNotExist(err), "duplicate source should be removed")

	_, err = os.Stat(filepath.Join(tmpDir, "2018-06-15_my_doc.pdf"))
	require.NoError(t, err, "existing target should remain")
}

func TestRenamer_RenameFiles_SameSizeDifferentContent_TargetCollisionKeepsSource(t *testing.T) {
	tmpDir := t.TempDir()

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	createTestFileWithContent(t, tmpDir, "My Doc.pdf", "ABCD", modTime)
	createTestFileWithContent(t, tmpDir, "2018-06-15_my_doc.pdf", "WXYZ", modTime)

	originalPath := filepath.Join(tmpDir, "My Doc.pdf")
	info, err := os.Stat(originalPath)
	require.NoError(t, err)

	files := []collector.FileInfo{
		{
			Path:    originalPath,
			Dir:     tmpDir,
			Name:    "My Doc.pdf",
			Size:    info.Size(),
			ModTime: info.ModTime(),
		},
	}

	r, err := New(tmpDir, false)
	require.NoError(t, err)
	result := r.RenameFiles(files)

	assert.Equal(t, 1, result.TotalFiles)
	assert.Equal(t, 0, result.RenamedCount)
	assert.Equal(t, 1, result.SkippedCount)
	assert.Equal(t, 0, result.DeletedCount)
	assert.Equal(t, 0, result.ErrorCount)

	require.Len(t, result.Operations, 1)
	op := result.Operations[0]
	assert.True(t, op.Skipped)
	assert.Equal(t, "target file already exists", op.SkipReason)
	assert.False(t, op.Deleted)

	_, err = os.Stat(originalPath)
	require.NoError(t, err, "source should remain when target has different bytes")

	_, err = os.Stat(filepath.Join(tmpDir, "2018-06-15_my_doc.pdf"))
	require.NoError(t, err, "existing target should remain")
}

func TestRenamer_RenameFiles_DryRun_NoFilesystemMutations(t *testing.T) {
	tmpDir := t.TempDir()

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	createTestFileWithContent(t, tmpDir, "Report.pdf", "same-content", modTime)
	createTestFileWithContent(t, tmpDir, "report.pdf", "same-content", modTime)

	files := collectFiles(t, tmpDir)
	require.Len(t, files, 2)

	r, err := New(tmpDir, true)
	require.NoError(t, err)
	result := r.RenameFiles(files)

	assert.Equal(t, 2, result.TotalFiles)
	assert.Equal(t, 1, result.RenamedCount)
	assert.Equal(t, 1, result.SkippedCount)
	assert.Equal(t, 0, result.DeletedCount)
	assert.Equal(t, 0, result.ErrorCount)
	require.Len(t, result.Operations, 2)
	for _, op := range result.Operations {
		assert.False(t, op.Deleted)
		assert.Empty(t, op.TrashedTo)
	}

	_, err = os.Stat(filepath.Join(tmpDir, "Report.pdf"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(tmpDir, "report.pdf"))
	require.NoError(t, err)

	_, err = os.Stat(filepath.Join(tmpDir, "2018-06-15_report.pdf"))
	assert.True(t, os.IsNotExist(err), "dry-run must not create renamed files")
}

func TestRenamer_RenameFiles_MultipleFiles(t *testing.T) {
	tmpDir := t.TempDir()

	modTime1 := time.Date(2018, 1, 15, 12, 0, 0, 0, time.UTC)
	modTime2 := time.Date(2018, 6, 20, 12, 0, 0, 0, time.UTC)
	modTime3 := time.Date(2018, 12, 25, 12, 0, 0, 0, time.UTC)

	createTestFile(t, tmpDir, "Report (Final).docx", modTime1)
	createTestFile(t, tmpDir, "Työpöytä.txt", modTime2)
	createTestFile(t, tmpDir, "KeePass.kdbx", modTime3)

	files := collectFiles(t, tmpDir)
	require.Len(t, files, 3)

	r, err := New(tmpDir, false) // actual rename
	require.NoError(t, err)
	result := r.RenameFiles(files)

	assert.Equal(t, 3, result.TotalFiles)
	assert.Equal(t, 3, result.RenamedCount)
	assert.Equal(t, 0, result.ErrorCount)

	// Verify all files exist with new names
	expectedFiles := []string{
		"2018-01-15_report_final.docx",
		"2018-06-20_tyopoyta.txt",
		"2018-12-25_keepass.kdbx",
	}

	for _, name := range expectedFiles {
		_, err := os.Stat(filepath.Join(tmpDir, name))
		assert.NoError(t, err, "expected file %s to exist", name)
	}
}

func TestRenamer_RenameFiles_SubdirectoriesInPlace(t *testing.T) {
	tmpDir := t.TempDir()

	// Create subdirectory with file
	subDir := filepath.Join(tmpDir, "subdir")
	err := os.MkdirAll(subDir, 0o755)
	require.NoError(t, err)

	modTime := time.Date(2018, 3, 10, 12, 0, 0, 0, time.UTC)
	createTestFile(t, subDir, "Nested File.pdf", modTime)

	files := collectFiles(t, tmpDir)
	require.Len(t, files, 1)

	r, err := New(tmpDir, false)
	require.NoError(t, err)
	result := r.RenameFiles(files)

	assert.Equal(t, 1, result.RenamedCount)

	// Verify file was renamed IN PLACE (still in subdir)
	_, err = os.Stat(filepath.Join(subDir, "2018-03-10_nested_file.pdf"))
	require.NoError(t, err, "file should be renamed in place within subdir")

	// Verify it's not in root
	_, err = os.Stat(filepath.Join(tmpDir, "2018-03-10_nested_file.pdf"))
	assert.True(t, os.IsNotExist(err), "file should not be in root dir")
}

func TestRenamer_DryRun(t *testing.T) {
	tmpDir := t.TempDir()

	r, err := New(tmpDir, true)
	require.NoError(t, err)
	assert.True(t, r.DryRun())

	r, err = New(tmpDir, false)
	require.NoError(t, err)
	assert.False(t, r.DryRun())
}

func TestRenamer_RenameFiles_EmptyList(t *testing.T) {
	tmpDir := t.TempDir()

	r, err := New(tmpDir, false)
	require.NoError(t, err)
	result := r.RenameFiles([]collector.FileInfo{})

	assert.Equal(t, 0, result.TotalFiles)
	assert.Equal(t, 0, result.RenamedCount)
	assert.Equal(t, 0, result.SkippedCount)
	assert.Equal(t, 0, result.ErrorCount)
	assert.Empty(t, result.Operations)
}

func TestRenamer_RenameFiles_InvalidReadPathsEmitCompleteProgress(t *testing.T) {
	tmpDir := t.TempDir()
	outsideDir := t.TempDir()
	outsidePath := filepath.Join(outsideDir, "escape.txt")
	testutil.CreateFile(t, outsidePath, "escape")

	r, err := New(tmpDir, false)
	require.NoError(t, err)

	var progressCalls [][2]int
	result := r.RenameFilesWithProgress([]collector.FileInfo{
		{
			Path: outsidePath,
			Dir:  outsideDir,
			Name: "escape.txt",
		},
	}, func(processed, total int) {
		progressCalls = append(progressCalls, [2]int{processed, total})
	})

	assert.Equal(t, 1, result.TotalFiles)
	assert.Equal(t, 0, result.RenamedCount)
	assert.Equal(t, 0, result.SkippedCount)
	assert.Equal(t, 1, result.ErrorCount)
	require.Len(t, result.Operations, 1)
	assert.Contains(t, result.Operations[0].Error.Error(), "source path escapes root")
	assert.Equal(t, [][2]int{{1, 1}}, progressCalls)
}

func TestRenamer_Root(t *testing.T) {
	tmpDir := t.TempDir()

	r, err := New(tmpDir, false)
	require.NoError(t, err)
	assert.Equal(t, tmpDir, r.Root())
}

func TestNewWithValidatorRequiresValidator(t *testing.T) {
	r, err := NewWithValidator(nil, false, nil)

	assert.Nil(t, r)
	assert.EqualError(t, err, "validator is required")
}

func TestRenamerResolveNameConflict_FirstUseRecordsHashAndPath(t *testing.T) {
	tmpDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	createTestFileWithContent(t, tmpDir, "Report.pdf", "content", modTime)

	files := collectFiles(t, tmpDir)
	require.Len(t, files, 1)

	r, err := New(tmpDir, true)
	require.NoError(t, err)
	usageMap := make(map[string]nameUsage)
	op := RenameOperation{}

	newName, handled := r.resolveNameConflict(
		&op,
		files[0],
		"2018-06-15_report.pdf",
		"2018-06-15_report",
		".pdf",
		usageMap,
	)

	assert.Equal(t, "2018-06-15_report.pdf", newName)
	assert.False(t, handled)
	assert.NoError(t, op.Error)
	require.Contains(t, usageMap, "2018-06-15_report.pdf")
	usage := usageMap["2018-06-15_report.pdf"]
	assert.Equal(t, 1, usage.count)
	assert.Equal(t, files[0].Size, usage.size)
	assert.NotEmpty(t, usage.hash)
	assert.Equal(t, files[0].Path, usage.path)
}

func TestRenamerResolveNameConflict_HashErrorStillTracksName(t *testing.T) {
	tmpDir := t.TempDir()
	missingPath := filepath.Join(tmpDir, "missing.pdf")
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)

	r, err := New(tmpDir, true)
	require.NoError(t, err)
	usageMap := make(map[string]nameUsage)
	op := RenameOperation{}

	newName, handled := r.resolveNameConflict(
		&op,
		collector.FileInfo{
			Path:    missingPath,
			Dir:     tmpDir,
			Name:    "missing.pdf",
			Size:    12,
			ModTime: modTime,
		},
		"2018-06-15_missing.pdf",
		"2018-06-15_missing",
		".pdf",
		usageMap,
	)

	assert.Equal(t, "2018-06-15_missing.pdf", newName)
	assert.False(t, handled)
	require.Contains(t, usageMap, "2018-06-15_missing.pdf")
	usage := usageMap["2018-06-15_missing.pdf"]
	assert.Equal(t, int64(12), usage.size)
	assert.Empty(t, usage.hash)
	assert.Equal(t, missingPath, usage.path)
}

func TestRenamerProcessFile_BranchesAndConflictMetadata(t *testing.T) {
	tmpDir := t.TempDir()

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	createTestFileWithContent(t, tmpDir, "Report.pdf", "same", modTime)
	createTestFileWithContent(t, tmpDir, "report.pdf", "same", modTime)
	createTestFileWithContent(t, tmpDir, "REPORT.pdf", "different", modTime)

	files := collectFiles(t, tmpDir)
	require.Len(t, files, 3)

	byName := make(map[string]collector.FileInfo)
	for _, file := range files {
		byName[file.Name] = file
	}

	r, err := New(tmpDir, true)
	require.NoError(t, err)
	dirNames := make(map[string]map[string]nameUsage)

	firstOp := r.processFile(byName["Report.pdf"], dirNames)
	assert.NoError(t, firstOp.Error)
	assert.False(t, firstOp.Skipped)
	assert.False(t, firstOp.Deleted)
	assert.Equal(t, "2018-06-15_report.pdf", firstOp.NewName)
	assert.Equal(t, filepath.Join(tmpDir, "2018-06-15_report.pdf"), firstOp.NewPath)

	duplicateOp := r.processFile(byName["report.pdf"], dirNames)
	assert.NoError(t, duplicateOp.Error)
	assert.True(t, duplicateOp.Skipped)
	assert.False(t, duplicateOp.Deleted)
	assert.Equal(t, "duplicate file already exists", duplicateOp.SkipReason)
	assert.Empty(t, duplicateOp.NewName)
	assert.Empty(t, duplicateOp.NewPath)

	conflictOp := r.processFile(byName["REPORT.pdf"], dirNames)
	assert.NoError(t, conflictOp.Error)
	assert.False(t, conflictOp.Skipped)
	assert.Equal(t, "2018-06-15_report_1.pdf", conflictOp.NewName)
	assert.Equal(t, filepath.Join(tmpDir, "2018-06-15_report_1.pdf"), conflictOp.NewPath)
}

func TestRenamerExistingTargetBranches(t *testing.T) {
	tmpDir := t.TempDir()

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	createTestFileWithContent(t, tmpDir, "My Doc.pdf", "source", modTime)
	createTestFileWithContent(t, tmpDir, "2018-06-15_my_doc.pdf", "longer-target", modTime)

	sourcePath := filepath.Join(tmpDir, "My Doc.pdf")
	targetPath := filepath.Join(tmpDir, "2018-06-15_my_doc.pdf")
	sourceInfo, err := os.Stat(sourcePath)
	require.NoError(t, err)
	targetInfo, err := os.Stat(targetPath)
	require.NoError(t, err)

	r, err := New(tmpDir, false)
	require.NoError(t, err)

	op := RenameOperation{
		OriginalPath: sourcePath,
		OriginalName: "My Doc.pdf",
		NewPath:      targetPath,
		NewName:      "2018-06-15_my_doc.pdf",
	}
	r.handleExistingTarget(&op, collector.FileInfo{
		Path:    sourcePath,
		Dir:     tmpDir,
		Name:    "My Doc.pdf",
		Size:    sourceInfo.Size(),
		ModTime: sourceInfo.ModTime(),
	}, targetInfo)

	assert.NoError(t, op.Error)
	assert.True(t, op.Skipped)
	assert.False(t, op.Deleted)
	assert.Equal(t, "target file already exists", op.SkipReason)
	assert.FileExists(t, sourcePath)
	assert.FileExists(t, targetPath)
}

func TestRenamerExistingTargetDryRunDuplicateDoesNotDelete(t *testing.T) {
	tmpDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	createTestFileWithContent(t, tmpDir, "My Doc.pdf", "same", modTime)
	createTestFileWithContent(t, tmpDir, "2018-06-15_my_doc.pdf", "same", modTime)

	sourcePath := filepath.Join(tmpDir, "My Doc.pdf")
	targetPath := filepath.Join(tmpDir, "2018-06-15_my_doc.pdf")
	sourceInfo, err := os.Stat(sourcePath)
	require.NoError(t, err)
	targetInfo, err := os.Stat(targetPath)
	require.NoError(t, err)

	r, err := New(tmpDir, true)
	require.NoError(t, err)
	op := RenameOperation{OriginalPath: sourcePath, NewPath: targetPath}
	r.handleExistingTarget(&op, collector.FileInfo{
		Path:    sourcePath,
		Dir:     tmpDir,
		Name:    "My Doc.pdf",
		Size:    sourceInfo.Size(),
		ModTime: sourceInfo.ModTime(),
	}, targetInfo)

	assert.NoError(t, op.Error)
	assert.True(t, op.Skipped)
	assert.False(t, op.Deleted)
	assert.Equal(t, "duplicate file already exists", op.SkipReason)
	assert.FileExists(t, sourcePath)
	assert.FileExists(t, targetPath)
}

func TestRenamerSameContentBranches(t *testing.T) {
	tmpDir := t.TempDir()
	outsideDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	createTestFileWithContent(t, tmpDir, "a.txt", "same", modTime)
	createTestFileWithContent(t, tmpDir, "b.txt", "same", modTime)
	createTestFileWithContent(t, tmpDir, "c.txt", "different", modTime)
	createTestFileWithContent(t, outsideDir, "outside.txt", "same", modTime)

	r, err := New(tmpDir, true)
	require.NoError(t, err)

	aPath := filepath.Join(tmpDir, "a.txt")
	bPath := filepath.Join(tmpDir, "b.txt")
	cPath := filepath.Join(tmpDir, "c.txt")
	outsidePath := filepath.Join(outsideDir, "outside.txt")

	same, err := r.sameContent(aPath, bPath)
	require.NoError(t, err)
	assert.True(t, same)

	same, err = r.sameContent(aPath, cPath)
	require.NoError(t, err)
	assert.False(t, same)

	_, err = r.sameContent(outsidePath, bPath)
	require.ErrorIs(t, err, safepath.ErrPathEscape)

	_, err = r.sameContent(aPath, outsidePath)
	require.ErrorIs(t, err, safepath.ErrPathEscape)
}

func TestRenamerMarkAsDuplicateErrorBranches(t *testing.T) {
	tmpDir := t.TempDir()
	outsideDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	keptPath := filepath.Join(tmpDir, "kept.txt")
	dupePath := filepath.Join(tmpDir, "dupe.txt")
	outsidePath := filepath.Join(outsideDir, "outside.txt")
	createTestFileWithContent(t, tmpDir, "kept.txt", "same", modTime)
	createTestFileWithContent(t, tmpDir, "dupe.txt", "same", modTime)
	createTestFileWithContent(t, outsideDir, "outside.txt", "same", modTime)

	r, err := New(tmpDir, false)
	require.NoError(t, err)
	hash, err := r.hasher.ComputeHash(dupePath)
	require.NoError(t, err)

	missingKeptOp := RenameOperation{OriginalPath: dupePath}
	r.markAsDuplicate(&missingKeptOp, collector.FileInfo{Path: dupePath}, hash, filepath.Join(tmpDir, "missing.txt"))
	assert.True(t, missingKeptOp.Skipped)
	assert.False(t, missingKeptOp.Deleted)
	require.Error(t, missingKeptOp.Error)
	assert.Contains(t, missingKeptOp.Error.Error(), "kept file missing")

	changedOp := RenameOperation{OriginalPath: dupePath}
	require.NoError(t, os.WriteFile(dupePath, []byte("changed"), 0o600))
	r.markAsDuplicate(&changedOp, collector.FileInfo{Path: dupePath}, hash, keptPath)
	assert.True(t, changedOp.Skipped)
	assert.False(t, changedOp.Deleted)
	require.ErrorIs(t, changedOp.Error, ErrContentChanged)

	outsideHash, err := r.hasher.ComputeHash(outsidePath)
	require.NoError(t, err)
	unsafeDeleteOp := RenameOperation{OriginalPath: outsidePath}
	r.markAsDuplicate(&unsafeDeleteOp, collector.FileInfo{Path: outsidePath}, outsideHash, keptPath)
	assert.True(t, unsafeDeleteOp.Skipped)
	assert.False(t, unsafeDeleteOp.Deleted)
	require.Error(t, unsafeDeleteOp.Error)
	assert.Contains(t, unsafeDeleteOp.Error.Error(), "failed to delete")
	assert.FileExists(t, outsidePath)
}

func TestRenamerHandleExistingTargetMissingTargetBranch(t *testing.T) {
	tmpDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	createTestFileWithContent(t, tmpDir, "source.pdf", "same", modTime)
	createTestFileWithContent(t, tmpDir, "target.pdf", "same", modTime)

	sourcePath := filepath.Join(tmpDir, "source.pdf")
	targetPath := filepath.Join(tmpDir, "target.pdf")
	sourceInfo, err := os.Stat(sourcePath)
	require.NoError(t, err)
	targetInfo, err := os.Stat(targetPath)
	require.NoError(t, err)

	r, err := New(tmpDir, false)
	require.NoError(t, err)
	require.NoError(t, os.Remove(targetPath))

	op := RenameOperation{OriginalPath: sourcePath, NewPath: targetPath}
	r.handleExistingTarget(&op, collector.FileInfo{
		Path:    sourcePath,
		Dir:     tmpDir,
		Name:    "source.pdf",
		Size:    sourceInfo.Size(),
		ModTime: sourceInfo.ModTime(),
	}, targetInfo)

	assert.False(t, op.Skipped)
	assert.False(t, op.Deleted)
	require.Error(t, op.Error)
	assert.Contains(t, op.Error.Error(), "path escapes root")
	assert.FileExists(t, sourcePath)
}

func TestRenamerResolveNameConflictIncrementsUsage(t *testing.T) {
	tmpDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	createTestFileWithContent(t, tmpDir, "A.txt", "one", modTime)
	createTestFileWithContent(t, tmpDir, "B.txt", "two", modTime)
	createTestFileWithContent(t, tmpDir, "C.txt", "three", modTime)

	r, err := New(tmpDir, true)
	require.NoError(t, err)
	usageMap := map[string]nameUsage{
		"2018-06-15_file.txt": {count: 1, size: 99, hash: "different", path: filepath.Join(tmpDir, "kept.txt")},
	}

	secondName, handled := r.resolveNameConflict(&RenameOperation{}, collector.FileInfo{
		Path: filepath.Join(tmpDir, "B.txt"), Dir: tmpDir, Name: "B.txt", Size: 3, ModTime: modTime,
	}, "2018-06-15_file.txt", "2018-06-15_file", ".txt", usageMap)
	thirdName, handledAgain := r.resolveNameConflict(&RenameOperation{}, collector.FileInfo{
		Path: filepath.Join(tmpDir, "C.txt"), Dir: tmpDir, Name: "C.txt", Size: 5, ModTime: modTime,
	}, "2018-06-15_file.txt", "2018-06-15_file", ".txt", usageMap)

	assert.False(t, handled)
	assert.False(t, handledAgain)
	assert.Equal(t, "2018-06-15_file_1.txt", secondName)
	assert.Equal(t, "2018-06-15_file_2.txt", thirdName)
	assert.Equal(t, 3, usageMap["2018-06-15_file.txt"].count)
}

func TestRenamerRenameFilesEmitsProgressForEachFile(t *testing.T) {
	tmpDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	createTestFile(t, tmpDir, "A.txt", modTime)
	createTestFile(t, tmpDir, "B.txt", modTime)

	r, err := New(tmpDir, true)
	require.NoError(t, err)
	var calls [][2]int
	result := r.RenameFilesWithProgress(collectFiles(t, tmpDir), func(processed, total int) {
		calls = append(calls, [2]int{processed, total})
	})

	assert.Equal(t, 2, result.TotalFiles)
	assert.Equal(t, [][2]int{{1, 2}, {2, 2}}, calls)
}

func TestRenamerMarkAsDuplicateHandlesHashAndDeleteErrors(t *testing.T) {
	t.Run("hash error clears deleted flag", func(t *testing.T) {
		tmpDir := t.TempDir()
		keptPath := filepath.Join(tmpDir, "kept.txt")
		createTestFileWithContent(t, tmpDir, "kept.txt", "same", time.Now().UTC())

		r, err := New(tmpDir, false)
		require.NoError(t, err)
		op := RenameOperation{}
		r.markAsDuplicate(&op, collector.FileInfo{Path: tmpDir, Dir: tmpDir, Name: filepath.Base(tmpDir)}, "hash", keptPath)

		require.Error(t, op.Error)
		assert.Contains(t, op.Error.Error(), "cannot re-verify file")
		assert.False(t, op.Deleted)
	})

	t.Run("delete error clears deleted flag", func(t *testing.T) {
		tmpDir := t.TempDir()
		outsideDir := t.TempDir()
		keptPath := filepath.Join(tmpDir, "kept.txt")
		outsidePath := filepath.Join(outsideDir, "dup.txt")
		testutil.CreateFile(t, keptPath, "same")
		testutil.CreateFile(t, outsidePath, "same")

		r, err := New(tmpDir, false)
		require.NoError(t, err)
		hash, err := r.hasher.ComputeHash(outsidePath)
		require.NoError(t, err)
		op := RenameOperation{}
		r.markAsDuplicate(&op, collector.FileInfo{Path: outsidePath, Dir: outsideDir, Name: "dup.txt"}, hash, keptPath)

		require.Error(t, op.Error)
		assert.Contains(t, op.Error.Error(), "failed to delete")
		assert.False(t, op.Deleted)
		assert.FileExists(t, outsidePath)
	})
}

func TestRenamerTrashOrRemoveHardDelete(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "delete-me.txt")
	createTestFileWithContent(t, tmpDir, "delete-me.txt", "content", time.Now().UTC())

	r, err := New(tmpDir, false)
	require.NoError(t, err)

	trashedTo, err := r.trashOrRemove(path)
	require.NoError(t, err)
	assert.Empty(t, trashedTo)
	assert.NoFileExists(t, path)
}

func TestRenamerTrashOrRemoveRejectsEscapingPath(t *testing.T) {
	tmpDir := t.TempDir()
	outsideDir := t.TempDir()
	outsidePath := filepath.Join(outsideDir, "delete-me.txt")
	testutil.CreateFile(t, outsidePath, "content")

	r, err := New(tmpDir, false)
	require.NoError(t, err)

	trashedTo, err := r.trashOrRemove(outsidePath)

	assert.Empty(t, trashedTo)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to delete")
	assert.FileExists(t, outsidePath)
}

func TestNew_InvalidRoot(t *testing.T) {
	_, err := New("/nonexistent/path/12345", false)
	assert.Error(t, err)
}

// Test that duplicate files are trashed (not permanently deleted) when a trasher is provided.
func TestRenamer_RenameFiles_TrashesFilesWhenTrasherProvided(t *testing.T) {
	tmpDir := t.TempDir()

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	// Two files with identical content that sanitize to the same name = duplicate in rename.
	createTestFileWithContent(t, tmpDir, "Report.pdf", "same-content", modTime)
	createTestFileWithContent(t, tmpDir, "report.pdf", "same-content", modTime)

	v, err := safepath.New(tmpDir)
	require.NoError(t, err)

	metaDir, err := metadata.Init(tmpDir, v)
	require.NoError(t, err)

	runID := metaDir.RunID("rename")
	trasher, err := trash.New(metaDir, runID, v)
	require.NoError(t, err)

	r, err := NewWithValidator(v, false, trasher)
	require.NoError(t, err)

	files := collectFiles(t, tmpDir)
	require.Len(t, files, 2)

	result := r.RenameFiles(files)

	assert.Equal(t, 2, result.TotalFiles)
	assert.Equal(t, 1, result.RenamedCount)
	assert.Equal(t, 1, result.DeletedCount)
	assert.Equal(t, 0, result.ErrorCount)

	// Renamed file should exist.
	assert.FileExists(t, filepath.Join(tmpDir, "2018-06-15_report.pdf"))

	// Find the deleted operation.
	var deletedOp *RenameOperation
	for i := range result.Operations {
		if result.Operations[i].Deleted {
			deletedOp = &result.Operations[i]
			break
		}
	}
	require.NotNil(t, deletedOp, "should have a deleted operation")
	assert.NotEmpty(t, deletedOp.TrashedTo, "TrashedTo should be populated")
	assert.Contains(t, deletedOp.TrashedTo, ".file-dedup/trash/")

	// The trashed file should exist at the trash destination.
	assert.FileExists(t, deletedOp.TrashedTo)
}

// Test that a file modified between initial hash and deletion is NOT deleted.
func TestRenamer_RenameFiles_RefusesDeleteWhenContentChanged(t *testing.T) {
	tmpDir := t.TempDir()

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)

	// Create two files with identical content (will produce same sanitized name).
	createTestFileWithContent(t, tmpDir, "Report.pdf", "same-content", modTime)
	createTestFileWithContent(t, tmpDir, "report.pdf", "same-content", modTime)

	files := collectFiles(t, tmpDir)
	require.Len(t, files, 2)

	// Mutate the second file after collection but before rename processing.
	// Since the renamer hashes lazily per-file, we need to intercept between
	// the first hash (in resolveNameConflict for the first file) and the
	// re-hash (in markAsDuplicate for the second file).
	// We can't intercept mid-processing easily, so instead test the
	// markAsDuplicate method directly with a mismatched hash.

	v, err := safepath.New(tmpDir)
	require.NoError(t, err)

	r, err := NewWithValidator(v, false, nil)
	require.NoError(t, err)

	secondFile := files[1]
	op := RenameOperation{
		OriginalPath: secondFile.Path,
		OriginalName: secondFile.Name,
	}

	// The kept file exists but the expectedHash won't match the actual file content.
	r.markAsDuplicate(&op, secondFile, "bogus_hash_that_wont_match", files[0].Path)

	require.Error(t, op.Error, "should error when re-hash doesn't match expected hash")
	require.ErrorIs(t, op.Error, ErrContentChanged)
	assert.False(t, op.Deleted, "file should NOT be marked as deleted")

	// File must still exist on disk.
	assert.FileExists(t, secondFile.Path, "file must be preserved when content changed")
}

// Test that a missing kept file prevents duplicate deletion in the batch path.
func TestRenamer_RenameFiles_RefusesDeleteWhenKeptFileMissing(t *testing.T) {
	tmpDir := t.TempDir()

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	createTestFileWithContent(t, tmpDir, "report.pdf", "same-content", modTime)

	v, err := safepath.New(tmpDir)
	require.NoError(t, err)

	r, err := NewWithValidator(v, false, nil)
	require.NoError(t, err)

	dupPath := filepath.Join(tmpDir, "report.pdf")
	info, err := os.Stat(dupPath)
	require.NoError(t, err)

	dupFile := collector.FileInfo{
		Path:    dupPath,
		Dir:     tmpDir,
		Name:    "report.pdf",
		Size:    info.Size(),
		ModTime: info.ModTime(),
	}

	// Compute the real hash so re-hash would match if the Lstat check didn't fail first.
	realHash, err := r.hasher.ComputeHash(dupPath)
	require.NoError(t, err)

	nonexistentKept := filepath.Join(tmpDir, "vanished.pdf")

	op := RenameOperation{
		OriginalPath: dupFile.Path,
		OriginalName: dupFile.Name,
	}
	r.markAsDuplicate(&op, dupFile, realHash, nonexistentKept)

	require.Error(t, op.Error, "should error when kept file is missing")
	assert.Contains(t, op.Error.Error(), "kept file missing",
		"error should mention kept file missing")
	assert.False(t, op.Deleted, "file should NOT be marked as deleted")
	assert.FileExists(t, dupPath, "duplicate must be preserved when kept file is gone")
}

// Test that handleExistingTarget refuses to delete source when target disappears.
func TestRenamer_RenameFiles_RefusesDeleteWhenTargetDisappears(t *testing.T) {
	tmpDir := t.TempDir()

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)

	// Create source and target with same content.
	createTestFileWithContent(t, tmpDir, "My Doc.pdf", "test content", modTime)
	createTestFileWithContent(t, tmpDir, "2018-06-15_my_doc.pdf", "test content", modTime)

	sourcePath := filepath.Join(tmpDir, "My Doc.pdf")
	targetPath := filepath.Join(tmpDir, "2018-06-15_my_doc.pdf")
	info, err := os.Stat(sourcePath)
	require.NoError(t, err)

	// Remove the target before processing to simulate it disappearing.
	require.NoError(t, os.Remove(targetPath))

	v, err := safepath.New(tmpDir)
	require.NoError(t, err)

	r, err := NewWithValidator(v, false, nil)
	require.NoError(t, err)

	// Manually construct the file info for just the source file, which will
	// try to rename to the now-missing target path. Since the target no longer
	// exists on disk, the lstat in processFile won't find it and the file
	// will simply be renamed (no handleExistingTarget path).
	// Instead, test the Lstat guard directly via handleExistingTarget.
	targetInfo, err := os.Stat(sourcePath) // use source info as stand-in for size match
	require.NoError(t, err)

	op := RenameOperation{
		OriginalPath: sourcePath,
		OriginalName: "My Doc.pdf",
		NewPath:      targetPath,
		NewName:      "2018-06-15_my_doc.pdf",
	}

	sourceFile := collector.FileInfo{
		Path:    sourcePath,
		Dir:     tmpDir,
		Name:    "My Doc.pdf",
		Size:    info.Size(),
		ModTime: info.ModTime(),
	}

	r.handleExistingTarget(&op, sourceFile, targetInfo)

	// Source must be preserved regardless of which check caught the missing target.
	require.Error(t, op.Error, "should error when target is gone")
	assert.False(t, op.Deleted, "source should NOT be deleted")
	assert.FileExists(t, sourcePath, "source must be preserved when target is gone")
}
