package organizer

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"file-dedup/internal/testutil"
	"file-dedup/pkg/collector"
	"file-dedup/pkg/safepath"
)

func createTestFile(t *testing.T, path, content string, modTime time.Time) {
	t.Helper()
	testutil.CreateFileWithModTime(t, path, content, modTime)
}

func collectFiles(t *testing.T, root string) []collector.FileInfo {
	t.Helper()

	c := collector.New(collector.Options{})
	files, err := c.Collect(root)
	require.NoError(t, err)

	return files
}

func TestOrganizer_BasicSingleExtension(t *testing.T) {
	tmpDir := t.TempDir()
	modTime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	createTestFile(t, filepath.Join(tmpDir, "doc.pdf"), "content", modTime)

	files := collectFiles(t, tmpDir)
	require.Len(t, files, 1)

	o, err := New(tmpDir, false)
	require.NoError(t, err)
	result := o.OrganizeFiles(files)

	assert.Equal(t, 1, result.TotalFiles)
	assert.Equal(t, 1, result.MovedCount)
	assert.Equal(t, 0, result.SkippedCount)
	assert.Equal(t, 0, result.ErrorCount)

	assert.FileExists(t, filepath.Join(tmpDir, "pdf", "doc.pdf"))
	assert.NoFileExists(t, filepath.Join(tmpDir, "doc.pdf"))
}

func TestOrganizer_ConstructorsRejectInvalidInputs(t *testing.T) {
	t.Parallel()

	_, err := New(filepath.Join(t.TempDir(), "missing"), false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to create path validator")
	assert.ErrorIs(t, err, safepath.ErrInvalidRoot)

	_, err = NewWithValidator(nil, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "validator is required")
}

func TestOrganizer_InvalidSourceCountsErrorAndProgress(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	createTestFile(t, outside, "outside", time.Now())

	o, err := New(root, true)
	require.NoError(t, err)
	var progressCalls []int
	result := o.OrganizeFilesWithProgress([]collector.FileInfo{{
		Path: outside,
		Dir:  filepath.Dir(outside),
		Name: filepath.Base(outside),
		Size: int64(len("outside")),
	}}, func(processed, total int) {
		progressCalls = append(progressCalls, processed+total)
	})

	assert.Equal(t, 1, result.TotalFiles)
	assert.Equal(t, 1, result.ErrorCount)
	require.Len(t, result.Operations, 1)
	assert.ErrorIs(t, result.Operations[0].Error, safepath.ErrPathEscape)
	assert.Equal(t, []int{2}, progressCalls)
}

func TestOrganizer_MultipleExtensions(t *testing.T) {
	tmpDir := t.TempDir()
	modTime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	createTestFile(t, filepath.Join(tmpDir, "doc.pdf"), "pdf-content", modTime)
	createTestFile(t, filepath.Join(tmpDir, "photo.jpg"), "jpg-content", modTime)
	createTestFile(t, filepath.Join(tmpDir, "notes.txt"), "txt-content", modTime)

	files := collectFiles(t, tmpDir)
	require.Len(t, files, 3)

	o, err := New(tmpDir, false)
	require.NoError(t, err)
	result := o.OrganizeFiles(files)

	assert.Equal(t, 3, result.TotalFiles)
	assert.Equal(t, 3, result.MovedCount)
	assert.Equal(t, 0, result.ErrorCount)

	assert.FileExists(t, filepath.Join(tmpDir, "pdf", "doc.pdf"))
	assert.FileExists(t, filepath.Join(tmpDir, "jpg", "photo.jpg"))
	assert.FileExists(t, filepath.Join(tmpDir, "txt", "notes.txt"))
}

func TestOrganizer_NoExtension_GoesToOther(t *testing.T) {
	tmpDir := t.TempDir()
	modTime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	createTestFile(t, filepath.Join(tmpDir, "Makefile"), "content", modTime)

	files := collectFiles(t, tmpDir)
	require.Len(t, files, 1)

	o, err := New(tmpDir, false)
	require.NoError(t, err)
	result := o.OrganizeFiles(files)

	assert.Equal(t, 1, result.MovedCount)
	assert.FileExists(t, filepath.Join(tmpDir, "other", "Makefile"))
	assert.Equal(t, "other", result.Operations[0].Extension)
}

func TestOrganizer_Dotfiles_GoToOther(t *testing.T) {
	tmpDir := t.TempDir()
	modTime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	createTestFile(t, filepath.Join(tmpDir, ".gitignore"), "content", modTime)

	files := collectFiles(t, tmpDir)
	require.Len(t, files, 1)

	o, err := New(tmpDir, false)
	require.NoError(t, err)
	result := o.OrganizeFiles(files)

	assert.Equal(t, 1, result.MovedCount)
	assert.FileExists(t, filepath.Join(tmpDir, "other", ".gitignore"))
	assert.Equal(t, "other", result.Operations[0].Extension)
}

func TestOrganizer_CaseInsensitive(t *testing.T) {
	tmpDir := t.TempDir()
	modTime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	createTestFile(t, filepath.Join(tmpDir, "photo.JPG"), "content1", modTime)
	createTestFile(t, filepath.Join(tmpDir, "image.Jpg"), "content2", modTime)

	files := collectFiles(t, tmpDir)
	require.Len(t, files, 2)

	o, err := New(tmpDir, false)
	require.NoError(t, err)
	result := o.OrganizeFiles(files)

	assert.Equal(t, 2, result.MovedCount)
	// Both should go to "jpg" directory.
	assert.DirExists(t, filepath.Join(tmpDir, "jpg"))
	assert.FileExists(t, filepath.Join(tmpDir, "jpg", "image.Jpg"))
	assert.FileExists(t, filepath.Join(tmpDir, "jpg", "photo.JPG"))
}

func TestOrganizer_NameConflicts(t *testing.T) {
	tmpDir := t.TempDir()
	modTime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	// Create files with the same name in different subdirectories.
	createTestFile(t, filepath.Join(tmpDir, "dir1", "file.txt"), "content1", modTime)
	createTestFile(t, filepath.Join(tmpDir, "dir2", "file.txt"), "content2", modTime)
	createTestFile(t, filepath.Join(tmpDir, "dir3", "file.txt"), "content3", modTime)

	files := collectFiles(t, tmpDir)
	require.Len(t, files, 3)

	o, err := New(tmpDir, false)
	require.NoError(t, err)
	result := o.OrganizeFiles(files)

	assert.Equal(t, 3, result.TotalFiles)
	assert.Equal(t, 3, result.MovedCount)
	assert.Equal(t, 0, result.ErrorCount)

	// Should have file.txt, file_1.txt, file_2.txt in txt/
	assert.FileExists(t, filepath.Join(tmpDir, "txt", "file.txt"))
	assert.FileExists(t, filepath.Join(tmpDir, "txt", "file_1.txt"))
	assert.FileExists(t, filepath.Join(tmpDir, "txt", "file_2.txt"))
}

func TestOrganizer_AlreadyOrganized(t *testing.T) {
	tmpDir := t.TempDir()
	modTime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	// File already in correct extension directory.
	createTestFile(t, filepath.Join(tmpDir, "pdf", "doc.pdf"), "content", modTime)

	files := collectFiles(t, tmpDir)
	require.Len(t, files, 1)

	o, err := New(tmpDir, false)
	require.NoError(t, err)
	result := o.OrganizeFiles(files)

	assert.Equal(t, 1, result.TotalFiles)
	assert.Equal(t, 0, result.MovedCount)
	assert.Equal(t, 1, result.SkippedCount)
	assert.Equal(t, filepath.Join(tmpDir, "pdf", "doc.pdf"), result.Operations[0].NewPath)
	assert.Equal(t, "already organized", result.Operations[0].SkipReason)

	// File should remain in place.
	assert.FileExists(t, filepath.Join(tmpDir, "pdf", "doc.pdf"))
}

func TestOrganizer_DryRun_NoFilesystemChanges(t *testing.T) {
	tmpDir := t.TempDir()
	modTime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	createTestFile(t, filepath.Join(tmpDir, "doc.pdf"), "content", modTime)
	createTestFile(t, filepath.Join(tmpDir, "photo.jpg"), "image", modTime)

	files := collectFiles(t, tmpDir)
	require.Len(t, files, 2)

	o, err := New(tmpDir, true) // dry run
	require.NoError(t, err)
	result := o.OrganizeFiles(files)

	assert.Equal(t, 2, result.TotalFiles)
	assert.Equal(t, 2, result.MovedCount)
	assert.Equal(t, 0, result.ErrorCount)
	assert.Equal(t, 2, result.CreatedDirsCount)
	assert.Equal(t, filepath.Join(tmpDir, "pdf", "doc.pdf"), result.Operations[0].NewPath)
	assert.Equal(t, filepath.Join(tmpDir, "jpg", "photo.jpg"), result.Operations[1].NewPath)

	// Files should NOT have moved (dry run).
	assert.FileExists(t, filepath.Join(tmpDir, "doc.pdf"))
	assert.FileExists(t, filepath.Join(tmpDir, "photo.jpg"))

	// Target directories should NOT exist.
	assert.NoDirExists(t, filepath.Join(tmpDir, "pdf"))
	assert.NoDirExists(t, filepath.Join(tmpDir, "jpg"))
}

func TestOrganizer_EmptyFileList(t *testing.T) {
	tmpDir := t.TempDir()

	o, err := New(tmpDir, false)
	require.NoError(t, err)
	result := o.OrganizeFiles([]collector.FileInfo{})

	assert.Equal(t, 0, result.TotalFiles)
	assert.Equal(t, 0, result.MovedCount)
	assert.Empty(t, result.Operations)
}

func TestOrganizer_NestedSubdirectories(t *testing.T) {
	tmpDir := t.TempDir()
	modTime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	createTestFile(t, filepath.Join(tmpDir, "a", "b", "c", "deep.pdf"), "pdf", modTime)
	createTestFile(t, filepath.Join(tmpDir, "x", "y", "deep.txt"), "txt", modTime)

	files := collectFiles(t, tmpDir)
	require.Len(t, files, 2)

	o, err := New(tmpDir, false)
	require.NoError(t, err)
	result := o.OrganizeFiles(files)

	assert.Equal(t, 2, result.TotalFiles)
	assert.Equal(t, 2, result.MovedCount)

	// All files should be in root-level extension directories.
	assert.FileExists(t, filepath.Join(tmpDir, "pdf", "deep.pdf"))
	assert.FileExists(t, filepath.Join(tmpDir, "txt", "deep.txt"))
}

func TestOrganizer_MultiDotExtension(t *testing.T) {
	tmpDir := t.TempDir()
	modTime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	createTestFile(t, filepath.Join(tmpDir, "archive.tar.gz"), "tarball", modTime)

	files := collectFiles(t, tmpDir)
	require.Len(t, files, 1)

	o, err := New(tmpDir, false)
	require.NoError(t, err)
	result := o.OrganizeFiles(files)

	assert.Equal(t, 1, result.MovedCount)
	// filepath.Ext returns ".gz" for "archive.tar.gz"
	assert.FileExists(t, filepath.Join(tmpDir, "gz", "archive.tar.gz"))
	assert.Equal(t, "gz", result.Operations[0].Extension)
}

func TestOrganizer_CreatedDirsCount(t *testing.T) {
	tmpDir := t.TempDir()
	modTime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	createTestFile(t, filepath.Join(tmpDir, "a.pdf"), "pdf", modTime)
	createTestFile(t, filepath.Join(tmpDir, "b.pdf"), "pdf2", modTime)
	createTestFile(t, filepath.Join(tmpDir, "c.txt"), "txt", modTime)

	files := collectFiles(t, tmpDir)

	o, err := New(tmpDir, false)
	require.NoError(t, err)
	result := o.OrganizeFiles(files)

	assert.Equal(t, 3, result.MovedCount)
	assert.Equal(t, 2, result.CreatedDirsCount) // pdf/ and txt/
}

func TestOrganizer_UnsafeSymlinkFailsBeforeMutations(t *testing.T) {
	tmpDir := t.TempDir()

	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "outside.txt")
	require.NoError(t, os.WriteFile(outsideFile, []byte("outside"), 0o600))

	safeFile := filepath.Join(tmpDir, "safe.txt")
	createTestFile(t, safeFile, "safe-content", time.Now().UTC())

	linkPath := filepath.Join(tmpDir, "escape_link.txt")
	if err := os.Symlink(outsideFile, linkPath); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	files := collectFiles(t, tmpDir)

	o, err := New(tmpDir, false)
	require.NoError(t, err)

	var progressCalls []int
	result := o.OrganizeFilesWithProgress(files, func(processed, _ int) {
		progressCalls = append(progressCalls, processed)
	})

	assert.Equal(t, 0, result.MovedCount)
	assert.Equal(t, 1, result.ErrorCount)
	assert.Equal(t, []int{len(files)}, progressCalls)
	require.Len(t, result.Operations, 1)
	require.ErrorIs(t, result.Operations[0].Error, safepath.ErrSymlinkEscape)

	// Safe file should not have been moved.
	assert.FileExists(t, safeFile)
	assert.NoDirExists(t, filepath.Join(tmpDir, "txt"))
}

func TestOrganizer_InvalidSourcePathFailsFast(t *testing.T) {
	tmpDir := t.TempDir()
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "outside.txt")
	require.NoError(t, os.WriteFile(outsideFile, []byte("outside"), 0o600))

	safeFile := filepath.Join(tmpDir, "safe.txt")
	createTestFile(t, safeFile, "safe", time.Now().UTC())

	o, err := New(tmpDir, false)
	require.NoError(t, err)

	files := []collector.FileInfo{
		{Path: outsideFile, Name: "outside.txt", Dir: outsideDir},
		{Path: safeFile, Name: "safe.txt", Dir: tmpDir},
	}
	var gotProgress []int
	result := o.OrganizeFilesWithProgress(files, func(processed, _ int) {
		gotProgress = append(gotProgress, processed)
	})

	assert.Equal(t, 0, result.MovedCount)
	assert.Equal(t, 1, result.ErrorCount)
	assert.Equal(t, []int{2}, gotProgress)
	require.Len(t, result.Operations, 1)
	assert.Equal(t, outsideFile, result.Operations[0].OriginalPath)
	assert.ErrorContains(t, result.Operations[0].Error, "source path escapes root")
	assert.FileExists(t, safeFile)
	assert.NoDirExists(t, filepath.Join(tmpDir, "txt"))
}

func TestNew_InvalidRoot(t *testing.T) {
	_, err := New("/nonexistent/path/12345", false)
	assert.Error(t, err)
}

func TestOrganizer_NewWithValidator_NilValidator(t *testing.T) {
	_, err := NewWithValidator(nil, false)
	assert.Error(t, err)
}

func TestOrganizer_DryRun_Accessor(t *testing.T) {
	tmpDir := t.TempDir()

	o, err := New(tmpDir, true)
	require.NoError(t, err)
	assert.True(t, o.DryRun())

	o, err = New(tmpDir, false)
	require.NoError(t, err)
	assert.False(t, o.DryRun())
}

func TestOrganizer_Root_Accessor(t *testing.T) {
	tmpDir := t.TempDir()

	o, err := New(tmpDir, false)
	require.NoError(t, err)
	assert.Equal(t, tmpDir, o.Root())
}

func TestExtensionCategory(t *testing.T) {
	tests := []struct {
		filename string
		expected string
	}{
		{"file.pdf", "pdf"},
		{"file.PDF", "pdf"},
		{"file.Jpg", "jpg"},
		{"file.tar.gz", "gz"},
		{"Makefile", "other"},
		{".gitignore", "other"},
		{"noext", "other"},
		{"file.TXT", "txt"},
	}

	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			assert.Equal(t, tt.expected, extensionCategory(tt.filename))
		})
	}
}

func TestOrganizer_ProgressCallback(t *testing.T) {
	tmpDir := t.TempDir()
	modTime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	createTestFile(t, filepath.Join(tmpDir, "a.pdf"), "pdf", modTime)
	createTestFile(t, filepath.Join(tmpDir, "b.txt"), "txt", modTime)

	files := collectFiles(t, tmpDir)

	o, err := New(tmpDir, true)
	require.NoError(t, err)

	var progressCalls []int
	result := o.OrganizeFilesWithProgress(files, func(processed, _ int) {
		progressCalls = append(progressCalls, processed)
	})

	assert.Equal(t, 2, result.TotalFiles)
	assert.Equal(t, []int{1, 2}, progressCalls)
}
