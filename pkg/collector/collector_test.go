package collector

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"file-dedup/internal/testutil"
)

// setupTestDir creates a temporary directory structure for testing.
func setupTestDir(t *testing.T) string {
	t.Helper()

	tmpDir := t.TempDir()

	// Create test structure:
	// tmpDir/
	//   file1.txt
	//   file2.pdf
	//   subdir1/
	//     file3.txt
	//     subdir2/
	//       file4.txt
	//   skip_this/
	//     file5.txt

	files := []string{
		"file1.txt",
		"file2.pdf",
		"subdir1/file3.txt",
		"subdir1/subdir2/file4.txt",
		"skip_this/file5.txt",
	}

	for _, f := range files {
		fullPath := filepath.Join(tmpDir, f)
		testutil.CreateFile(t, fullPath, "test content for "+f)
	}

	return tmpDir
}

func collectFiles(t *testing.T, c *Collector, root string) []FileInfo {
	t.Helper()

	files, err := c.Collect(root)
	require.NoError(t, err)

	return files
}

func TestCollector_Collect(t *testing.T) {
	tmpDir := setupTestDir(t)

	c := New(Options{})

	files := collectFiles(t, c, tmpDir)
	assert.Len(t, files, 5)

	// Verify all files have required metadata
	for _, f := range files {
		assert.NotEmpty(t, f.Path, "file has empty Path")
		assert.NotEmpty(t, f.Name, "file has empty Name")
		assert.NotEmpty(t, f.Dir, "file has empty Dir")
		assert.NotZero(t, f.Size, "file has zero Size")
		assert.False(t, f.ModTime.IsZero(), "file has zero ModTime")
	}
}

func TestCollector_Collect_SkipFiles(t *testing.T) {
	tmpDir := setupTestDir(t)

	c := New(Options{
		SkipFiles: []string{"file1.txt", "file3.txt"},
	})

	files := collectFiles(t, c, tmpDir)
	assert.Len(t, files, 3, "expected 3 files (skipped 2)")

	// Verify skipped files are not in result
	for _, f := range files {
		assert.NotEqual(t, "file1.txt", f.Name, "file1.txt should have been skipped")
		assert.NotEqual(t, "file3.txt", f.Name, "file3.txt should have been skipped")
	}
}

func TestCollector_Collect_SkipDirs(t *testing.T) {
	tmpDir := setupTestDir(t)

	c := New(Options{
		SkipDirs: []string{"skip_this"},
	})

	files := collectFiles(t, c, tmpDir)
	assert.Len(t, files, 4, "expected 4 files (skipped skip_this dir)")

	// Verify files from skipped dir are not in result
	for _, f := range files {
		assert.NotEqual(t, "file5.txt", f.Name, "file5.txt from skip_this dir should have been skipped")
	}
}

func TestCollector_Collect_SkipFileDedupDir(t *testing.T) {
	tmpDir := t.TempDir()

	// Create files inside .file-dedup directory (metadata dir)
	testutil.CreateFile(t, filepath.Join(tmpDir, ".file-dedup", "lock"), "lock")
	testutil.CreateFile(t, filepath.Join(tmpDir, ".file-dedup", "trash", "run1", "file.txt"), "trashed")
	testutil.CreateFile(t, filepath.Join(tmpDir, "normal.txt"), "normal")

	c := New(Options{
		SkipDirs: []string{".file-dedup"},
	})

	files := collectFiles(t, c, tmpDir)
	assert.Len(t, files, 1, "expected only 1 file (everything in .file-dedup skipped)")
	assert.Equal(t, "normal.txt", files[0].Name, "only normal.txt should be collected")
}

func TestCollector_CollectFromDir(t *testing.T) {
	tmpDir := setupTestDir(t)

	c := New(Options{})

	files, err := c.CollectFromDir(tmpDir)
	require.NoError(t, err)

	// Should only get files directly in tmpDir, not subdirs
	assert.Len(t, files, 2, "expected 2 files in root dir")

	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.Name
	}
	assert.ElementsMatch(t, []string{"file1.txt", "file2.pdf"}, names)
}

func TestCollector_CollectFromDir_SkipFiles(t *testing.T) {
	tmpDir := t.TempDir()
	testutil.CreateFile(t, filepath.Join(tmpDir, "keep.txt"), "keep")
	testutil.CreateFile(t, filepath.Join(tmpDir, "skip.txt"), "skip")

	c := New(Options{SkipFiles: []string{"skip.txt"}})
	files, err := c.CollectFromDir(tmpDir)
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal(t, "keep.txt", files[0].Name)
}

func TestCollector_CollectFromDir_NonExistentDir(t *testing.T) {
	c := New(Options{})

	files, err := c.CollectFromDir(filepath.Join(t.TempDir(), "missing"))
	require.Error(t, err)
	assert.Nil(t, files)
}

func TestCollector_Collect_EmptyDir(t *testing.T) {
	tmpDir := t.TempDir()

	c := New(Options{})

	files := collectFiles(t, c, tmpDir)
	assert.Empty(t, files, "expected 0 files in empty dir")
}

func TestCollector_Collect_NonExistentDir(t *testing.T) {
	c := New(Options{})

	_, err := c.Collect("/nonexistent/path/that/does/not/exist")
	assert.Error(t, err, "expected error for nonexistent directory")
}

func TestFileInfo_ModTime(t *testing.T) {
	tmpDir := t.TempDir()

	testFile := filepath.Join(tmpDir, "test.txt")
	err := os.WriteFile(testFile, []byte("test"), 0644)
	require.NoError(t, err)

	// Set a specific modification time
	expectedTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	err = os.Chtimes(testFile, expectedTime, expectedTime)
	require.NoError(t, err)

	c := New(Options{})
	files := collectFiles(t, c, tmpDir)
	require.Len(t, files, 1)

	// Compare times (allow for some filesystem precision differences)
	assert.True(t, files[0].ModTime.Equal(expectedTime), "ModTime = %v, want %v", files[0].ModTime, expectedTime)
}
