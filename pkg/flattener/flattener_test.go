package flattener

import (
	"fmt"
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

func TestFlattener_FlattenFiles_Basic(t *testing.T) {
	tmpDir := t.TempDir()

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	createTestFile(t, filepath.Join(tmpDir, "subdir", "file.txt"), "content", modTime)

	files := collectFiles(t, tmpDir)
	require.Len(t, files, 1)

	f, err := New(tmpDir, false)
	require.NoError(t, err)
	result := f.FlattenFiles(files)

	assert.Equal(t, 1, result.TotalFiles)
	assert.Equal(t, 1, result.MovedCount)
	assert.Equal(t, 0, result.DuplicatesCount)
	assert.Equal(t, 0, result.SkippedCount)
	assert.Equal(t, 0, result.ErrorCount)
	require.Len(t, result.Operations, 1)
	assert.Equal(t, filepath.Join(tmpDir, "subdir", "file.txt"), result.Operations[0].OriginalPath)
	assert.Equal(t, filepath.Join(tmpDir, "file.txt"), result.Operations[0].NewPath)
	assert.NotEmpty(t, result.Operations[0].Hash)
	assert.False(t, result.Operations[0].Duplicate)
	assert.False(t, result.Operations[0].Skipped)
	assert.Empty(t, result.Operations[0].SkipReason)
	assert.Empty(t, result.Operations[0].TrashedTo)
	assert.NoError(t, result.Operations[0].Error)

	// Verify file is now in root.
	_, err = os.Stat(filepath.Join(tmpDir, "file.txt"))
	require.NoError(t, err)

	// Verify original location is empty.
	_, err = os.Stat(filepath.Join(tmpDir, "subdir", "file.txt"))
	assert.True(t, os.IsNotExist(err))
}

func TestFlattener_ConstructorsRejectInvalidInputs(t *testing.T) {
	t.Parallel()

	_, err := New(filepath.Join(t.TempDir(), "missing"), false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to create path validator")
	assert.ErrorIs(t, err, safepath.ErrInvalidRoot)

	_, err = NewWithValidator(nil, false, 0, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "validator is required")
}

func TestFlattener_FlattenFiles_InvalidSourceCountsError(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	createTestFile(t, outside, "outside", time.Now())

	f, err := New(root, true)
	require.NoError(t, err)
	result := f.FlattenFiles([]collector.FileInfo{{
		Path: outside,
		Dir:  filepath.Dir(outside),
		Name: filepath.Base(outside),
		Size: int64(len("outside")),
	}})

	assert.Equal(t, 1, result.TotalFiles)
	assert.Equal(t, 1, result.ErrorCount)
	require.Len(t, result.Operations, 1)
	assert.Equal(t, outside, result.Operations[0].OriginalPath)
	assert.ErrorIs(t, result.Operations[0].Error, safepath.ErrPathEscape)
}

func TestFlattener_FlattenFilesWithProgress(t *testing.T) {
	tmpDir := t.TempDir()

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	createTestFile(t, filepath.Join(tmpDir, "dir1", "file.txt"), "content", modTime)
	createTestFile(t, filepath.Join(tmpDir, "dir2", "file.txt"), "content", modTime)

	files := collectFiles(t, tmpDir)
	require.Len(t, files, 2)

	f, err := NewWithWorkers(tmpDir, true, 1)
	require.NoError(t, err)

	type event struct {
		stage     string
		processed int
		total     int
	}
	var events []event
	result := f.FlattenFilesWithProgress(files, func(stage string, processed, total int) {
		events = append(events, event{stage: stage, processed: processed, total: total})
	})

	assert.Equal(t, 1, result.MovedCount)
	assert.Equal(t, 1, result.DuplicatesCount)
	assert.Equal(t, 0, result.ErrorCount)
	assert.Equal(t, []event{
		{stage: progressStageHashing, processed: 1, total: 2},
		{stage: progressStageHashing, processed: 2, total: 2},
		{stage: progressStageMoving, processed: 1, total: 2},
		{stage: progressStageMoving, processed: 2, total: 2},
	}, events)
}

func TestFlattener_FlattenFiles_DryRun(t *testing.T) {
	tmpDir := t.TempDir()

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	createTestFile(t, filepath.Join(tmpDir, "subdir", "file.txt"), "content", modTime)

	files := collectFiles(t, tmpDir)

	f, err := New(tmpDir, true) // dry run
	require.NoError(t, err)
	result := f.FlattenFiles(files)

	assert.Equal(t, 1, result.MovedCount)

	// File should NOT have moved (dry run).
	_, err = os.Stat(filepath.Join(tmpDir, "subdir", "file.txt"))
	require.NoError(t, err, "file should still be in original location")

	_, err = os.Stat(filepath.Join(tmpDir, "file.txt"))
	assert.True(t, os.IsNotExist(err), "file should not be in root")
}

func TestFlattener_FlattenFiles_DryRun_NoFilesystemMutations(t *testing.T) {
	tmpDir := t.TempDir()

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	createTestFile(t, filepath.Join(tmpDir, "dir1", "file.txt"), "content", modTime)
	createTestFile(t, filepath.Join(tmpDir, "dir2", "file.txt"), "content", modTime)

	files := collectFiles(t, tmpDir)
	require.Len(t, files, 2)

	f, err := New(tmpDir, true)
	require.NoError(t, err)
	result := f.FlattenFiles(files)

	assert.Equal(t, 2, result.TotalFiles)
	assert.Equal(t, 1, result.MovedCount)
	assert.Equal(t, 1, result.DuplicatesCount)
	assert.Equal(t, 0, result.DeletedDirsCount)
	assert.Equal(t, 0, result.ErrorCount)

	_, err = os.Stat(filepath.Join(tmpDir, "dir1", "file.txt"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(tmpDir, "dir2", "file.txt"))
	require.NoError(t, err)

	_, err = os.Stat(filepath.Join(tmpDir, "file.txt"))
	assert.True(t, os.IsNotExist(err), "dry-run must not move files to root")

	_, err = os.Stat(filepath.Join(tmpDir, "dir1"))
	require.NoError(t, err, "dry-run must not remove directories")
	_, err = os.Stat(filepath.Join(tmpDir, "dir2"))
	require.NoError(t, err, "dry-run must not remove directories")
}

func TestFlattener_FlattenFiles_Duplicates(t *testing.T) {
	tmpDir := t.TempDir()

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	// Same name, same size, same mtime = duplicate.
	createTestFile(t, filepath.Join(tmpDir, "dir1", "file.txt"), "content", modTime)
	createTestFile(t, filepath.Join(tmpDir, "dir2", "file.txt"), "content", modTime)

	files := collectFiles(t, tmpDir)
	require.Len(t, files, 2)

	f, err := New(tmpDir, false)
	require.NoError(t, err)
	result := f.FlattenFiles(files)

	assert.Equal(t, 2, result.TotalFiles)
	assert.Equal(t, 1, result.MovedCount)
	assert.Equal(t, 1, result.DuplicatesCount)

	// Only one file should exist in root.
	_, err = os.Stat(filepath.Join(tmpDir, "file.txt"))
	require.NoError(t, err)
}

func TestFlattener_FlattenFiles_SameMetadataDifferentContent_NotDuplicate(t *testing.T) {
	tmpDir := t.TempDir()

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	createTestFile(t, filepath.Join(tmpDir, "dir1", "file.txt"), "alpha-1", modTime)
	createTestFile(t, filepath.Join(tmpDir, "dir2", "file.txt"), "omega-2", modTime)

	files := collectFiles(t, tmpDir)
	require.Len(t, files, 2)

	f, err := New(tmpDir, false)
	require.NoError(t, err)
	result := f.FlattenFiles(files)

	assert.Equal(t, 2, result.TotalFiles)
	assert.Equal(t, 2, result.MovedCount)
	assert.Equal(t, 0, result.DuplicatesCount)
	assert.Equal(t, 0, result.ErrorCount)

	basePath := filepath.Join(tmpDir, "file.txt")
	suffixPath := filepath.Join(tmpDir, "file_1.txt")

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

func TestFlattener_FlattenFiles_NameConflict(t *testing.T) {
	tmpDir := t.TempDir()

	modTime1 := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	modTime2 := time.Date(2018, 7, 20, 12, 0, 0, 0, time.UTC)
	// Same name, different mtime = NOT duplicate, needs suffix.
	createTestFile(t, filepath.Join(tmpDir, "dir1", "file.txt"), "content1", modTime1)
	createTestFile(t, filepath.Join(tmpDir, "dir2", "file.txt"), "content2", modTime2)

	files := collectFiles(t, tmpDir)
	require.Len(t, files, 2)

	f, err := New(tmpDir, false)
	require.NoError(t, err)
	result := f.FlattenFiles(files)

	assert.Equal(t, 2, result.TotalFiles)
	assert.Equal(t, 2, result.MovedCount)
	assert.Equal(t, 0, result.DuplicatesCount)

	// Both files should exist with different names.
	_, err = os.Stat(filepath.Join(tmpDir, "file.txt"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(tmpDir, "file_1.txt"))
	require.NoError(t, err)
}

func TestFlattener_FlattenFiles_AlreadyInRoot(t *testing.T) {
	tmpDir := t.TempDir()

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	createTestFile(t, filepath.Join(tmpDir, "rootfile.txt"), "content", modTime)

	files := collectFiles(t, tmpDir)
	require.Len(t, files, 1)

	f, err := New(tmpDir, false)
	require.NoError(t, err)
	result := f.FlattenFiles(files)

	assert.Equal(t, 1, result.TotalFiles)
	assert.Equal(t, 0, result.MovedCount)
	assert.Equal(t, 1, result.SkippedCount)
	assert.Equal(t, "already in root", result.Operations[0].SkipReason)
}

func TestFlattener_FlattenFiles_RemovesEmptyDirs(t *testing.T) {
	tmpDir := t.TempDir()

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	createTestFile(t, filepath.Join(tmpDir, "a", "b", "c", "file.txt"), "content", modTime)

	files := collectFiles(t, tmpDir)

	f, err := New(tmpDir, false)
	require.NoError(t, err)
	result := f.FlattenFiles(files)

	assert.Equal(t, 1, result.MovedCount)
	assert.Equal(t, 3, result.DeletedDirsCount) // a, b, c

	// Verify directories are gone.
	_, err = os.Stat(filepath.Join(tmpDir, "a"))
	assert.True(t, os.IsNotExist(err))
}

func TestFlattener_FlattenFiles_Empty(t *testing.T) {
	tmpDir := t.TempDir()

	f, err := New(tmpDir, false)
	require.NoError(t, err)
	result := f.FlattenFiles([]collector.FileInfo{})

	assert.Equal(t, 0, result.TotalFiles)
	assert.Equal(t, 0, result.MovedCount)
	assert.Empty(t, result.Operations)
}

func TestFlattener_DryRun(t *testing.T) {
	tmpDir := t.TempDir()

	f, err := New(tmpDir, true)
	require.NoError(t, err)
	assert.True(t, f.DryRun())

	f, err = NewWithWorkers(tmpDir, false, 4)
	require.NoError(t, err)
	assert.False(t, f.DryRun())
}

func TestFlattener_FlattenFiles_DeepNesting(t *testing.T) {
	tmpDir := t.TempDir()

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	// Create files at various depths.
	createTestFile(t, filepath.Join(tmpDir, "l1", "file1.txt"), "1", modTime)
	createTestFile(t, filepath.Join(tmpDir, "l1", "l2", "file2.txt"), "2", modTime)
	createTestFile(t, filepath.Join(tmpDir, "l1", "l2", "l3", "file3.txt"), "3", modTime)
	createTestFile(t, filepath.Join(tmpDir, "l1", "l2", "l3", "l4", "file4.txt"), "4", modTime)
	createTestFile(t, filepath.Join(tmpDir, "l1", "l2", "l3", "l4", "l5", "file5.txt"), "5", modTime)

	files := collectFiles(t, tmpDir)
	require.Len(t, files, 5)

	f, err := New(tmpDir, false)
	require.NoError(t, err)
	result := f.FlattenFiles(files)

	assert.Equal(t, 5, result.TotalFiles)
	assert.Equal(t, 5, result.MovedCount)
	assert.Equal(t, 0, result.DuplicatesCount)

	// All files should be in root.
	for i := 1; i <= 5; i++ {
		name := filepath.Join(tmpDir, fmt.Sprintf("file%d.txt", i))
		_, statErr := os.Stat(name)
		require.NoError(t, statErr, "file%d.txt should exist in root", i)
	}

	// All subdirs should be removed.
	_, err = os.Stat(filepath.Join(tmpDir, "l1"))
	assert.True(t, os.IsNotExist(err))
}

func TestFlattener_Root(t *testing.T) {
	tmpDir := t.TempDir()

	f, err := NewWithWorkers(tmpDir, false, 2)
	require.NoError(t, err)
	assert.Equal(t, tmpDir, f.Root())
}

func TestFlattenerProcessFile_ErrorSkipConflictAndDuplicateBranches(t *testing.T) {
	tmpDir := t.TempDir()

	rootPath := filepath.Join(tmpDir, "root.txt")
	nestedPath := filepath.Join(tmpDir, "nested", "file.txt")
	secondNestedPath := filepath.Join(tmpDir, "other", "file.txt")
	duplicatePath := filepath.Join(tmpDir, "dupe", "root.txt")
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	createTestFile(t, rootPath, "root", modTime)
	createTestFile(t, nestedPath, "alpha", modTime)
	createTestFile(t, secondNestedPath, "bravo", modTime)
	createTestFile(t, duplicatePath, "root", modTime)

	f, err := New(tmpDir, true)
	require.NoError(t, err)

	rootHash, err := f.hasher.ComputeHash(rootPath)
	require.NoError(t, err)
	nestedHash, err := f.hasher.ComputeHash(nestedPath)
	require.NoError(t, err)
	secondNestedHash, err := f.hasher.ComputeHash(secondNestedPath)
	require.NoError(t, err)

	seenHash := make(map[string]string)
	nameCount := make(map[string]int)

	missingHashOp := f.processFile(&collector.FileInfo{
		Path: nestedPath,
		Dir:  filepath.Dir(nestedPath),
		Name: "file.txt",
	}, "", seenHash, nameCount)
	require.Error(t, missingHashOp.Error)
	assert.Contains(t, missingHashOp.Error.Error(), "could not compute hash")
	assert.False(t, missingHashOp.Duplicate)
	assert.False(t, missingHashOp.Skipped)

	rootOp := f.processFile(&collector.FileInfo{
		Path: rootPath,
		Dir:  tmpDir,
		Name: "root.txt",
	}, rootHash, seenHash, nameCount)
	assert.NoError(t, rootOp.Error)
	assert.True(t, rootOp.Skipped)
	assert.Equal(t, "already in root", rootOp.SkipReason)
	assert.Equal(t, rootPath, rootOp.NewPath)
	assert.Equal(t, rootHash, rootOp.Hash)
	assert.Equal(t, rootPath, seenHash[rootHash])

	duplicateOp := f.processFile(&collector.FileInfo{
		Path: duplicatePath,
		Dir:  filepath.Dir(duplicatePath),
		Name: "root.txt",
	}, rootHash, seenHash, nameCount)
	assert.NoError(t, duplicateOp.Error)
	assert.True(t, duplicateOp.Duplicate)
	assert.Equal(t, rootPath, duplicateOp.NewPath)

	moveOp := f.processFile(&collector.FileInfo{
		Path: nestedPath,
		Dir:  filepath.Dir(nestedPath),
		Name: "file.txt",
	}, nestedHash, seenHash, nameCount)
	assert.NoError(t, moveOp.Error)
	assert.False(t, moveOp.Skipped)
	assert.False(t, moveOp.Duplicate)
	assert.Equal(t, filepath.Join(tmpDir, "file.txt"), moveOp.NewPath)

	conflictOp := f.processFile(&collector.FileInfo{
		Path: secondNestedPath,
		Dir:  filepath.Dir(secondNestedPath),
		Name: "file.txt",
	}, secondNestedHash, seenHash, nameCount)
	assert.NoError(t, conflictOp.Error)
	assert.Equal(t, filepath.Join(tmpDir, "file_1.txt"), conflictOp.NewPath)
}

func TestFlattenerComputeHashesReportsOnlyReadableSafeFiles(t *testing.T) {
	tmpDir := t.TempDir()
	outsideDir := t.TempDir()

	safePath := filepath.Join(tmpDir, "safe.txt")
	outsidePath := filepath.Join(outsideDir, "outside.txt")
	createTestFile(t, safePath, "safe", time.Now().UTC())
	createTestFile(t, outsidePath, "outside", time.Now().UTC())

	f, err := New(tmpDir, false)
	require.NoError(t, err)

	var progressCalls [][2]int
	hashes, invalidReadErrors := f.computeHashes([]collector.FileInfo{
		{Path: safePath, Size: 4},
		{Path: outsidePath, Size: 7},
	}, func(processed, total int) {
		progressCalls = append(progressCalls, [2]int{processed, total})
	})

	require.Len(t, hashes, 1)
	assert.NotEmpty(t, hashes[safePath])
	require.Len(t, invalidReadErrors, 1)
	require.ErrorIs(t, invalidReadErrors[outsidePath], safepath.ErrPathEscape)
	assert.Equal(t, [][2]int{{1, 1}}, progressCalls)
}

func TestFlattenerHandleDuplicateDryRunAndDeleteErrorBranches(t *testing.T) {
	tmpDir := t.TempDir()
	outsideDir := t.TempDir()

	keptPath := filepath.Join(tmpDir, "kept.txt")
	dupePath := filepath.Join(tmpDir, "dupe.txt")
	outsidePath := filepath.Join(outsideDir, "outside.txt")
	createTestFile(t, keptPath, "same", time.Now().UTC())
	createTestFile(t, dupePath, "same", time.Now().UTC())
	createTestFile(t, outsidePath, "same", time.Now().UTC())

	dryRunFlattener, err := New(tmpDir, true)
	require.NoError(t, err)
	dryRunOp := MoveOperation{OriginalPath: dupePath, Hash: "hash"}
	dryRunOp = dryRunFlattener.handleDuplicate(&dryRunOp, dupePath, keptPath)
	assert.True(t, dryRunOp.Duplicate)
	assert.Equal(t, keptPath, dryRunOp.NewPath)
	assert.NoError(t, dryRunOp.Error)
	assert.FileExists(t, dupePath)

	liveFlattener, err := New(tmpDir, false)
	require.NoError(t, err)
	missingPath := filepath.Join(tmpDir, "missing.txt")
	missingOp := MoveOperation{OriginalPath: missingPath, Hash: "hash"}
	missingOp = liveFlattener.handleDuplicate(&missingOp, missingPath, keptPath)
	assert.True(t, missingOp.Duplicate)
	require.Error(t, missingOp.Error)
	assert.Contains(t, missingOp.Error.Error(), "re-hash before delete")

	outsideHash, err := liveFlattener.hasher.ComputeHash(outsidePath)
	require.NoError(t, err)
	unsafeDeleteOp := MoveOperation{OriginalPath: outsidePath, Hash: outsideHash}
	unsafeDeleteOp = liveFlattener.handleDuplicate(&unsafeDeleteOp, outsidePath, keptPath)
	assert.True(t, unsafeDeleteOp.Duplicate)
	require.Error(t, unsafeDeleteOp.Error)
	assert.Contains(t, unsafeDeleteOp.Error.Error(), "failed to delete duplicate")
	assert.FileExists(t, outsidePath)
}

func TestFlattenerRemoveEmptyDirsSkipsNonEmptyAndUnreadableDirs(t *testing.T) {
	tmpDir := t.TempDir()

	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, "empty", "child"), 0o755))
	createTestFile(t, filepath.Join(tmpDir, "non-empty", "file.txt"), "content", time.Now().UTC())

	f, err := New(tmpDir, false)
	require.NoError(t, err)

	assert.Equal(t, 2, f.removeEmptyDirs())
	assert.NoDirExists(t, filepath.Join(tmpDir, "empty"))
	assert.DirExists(t, filepath.Join(tmpDir, "non-empty"))
	assert.FileExists(t, filepath.Join(tmpDir, "non-empty", "file.txt"))
}

func TestNew_InvalidRoot(t *testing.T) {
	_, err := NewWithWorkers("/nonexistent/path/12345", false, 2)
	assert.Error(t, err)
}

func TestFlattener_FlattenFiles_DuplicatePreservedWhenKeptFileDisappears(t *testing.T) {
	tmpDir := t.TempDir()

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	// Create two identical files in subdirectories.
	keptFile := filepath.Join(tmpDir, "dir1", "file.txt")
	dupeFile := filepath.Join(tmpDir, "dir2", "file.txt")
	createTestFile(t, keptFile, "content", modTime)
	createTestFile(t, dupeFile, "content", modTime)

	files := collectFiles(t, tmpDir)
	require.Len(t, files, 2)

	f, err := New(tmpDir, false)
	require.NoError(t, err)

	// Pre-compute hashes (same as FlattenFiles does internally).
	fileHashes, _ := f.computeHashes(files, nil)
	seenHash := make(map[string]string)
	nameCount := make(map[string]int)

	// Process the first file (will be moved to root).
	op1 := f.processFile(&files[0], fileHashes[files[0].Path], seenHash, nameCount)
	require.NoError(t, op1.Error)
	require.False(t, op1.Duplicate)

	// Now delete the kept file to simulate it disappearing.
	require.NoError(t, os.Remove(op1.NewPath))

	// Process the second file — it has the same hash so it's a duplicate.
	// The kept file no longer exists, so deletion should be refused.
	op2 := f.processFile(&files[1], fileHashes[files[1].Path], seenHash, nameCount)
	require.Error(t, op2.Error, "should refuse to delete duplicate when kept file is missing")
	assert.True(t, op2.Duplicate)
	assert.Contains(t, op2.Error.Error(), "kept file missing")

	// The duplicate should still exist on disk.
	assert.FileExists(t, dupeFile)
}

// Test that duplicate files are trashed (not permanently deleted) when a trasher is provided.
func TestFlattener_FlattenFiles_TrashesFilesWhenTrasherProvided(t *testing.T) {
	tmpDir := t.TempDir()

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	// Same name, same content, same mtime = duplicate during flatten.
	createTestFile(t, filepath.Join(tmpDir, "dir1", "file.txt"), "content", modTime)
	createTestFile(t, filepath.Join(tmpDir, "dir2", "file.txt"), "content", modTime)

	v, err := safepath.New(tmpDir)
	require.NoError(t, err)

	metaDir, err := metadata.Init(tmpDir, v)
	require.NoError(t, err)

	runID := metaDir.RunID("flatten")
	trasher, err := trash.New(metaDir, runID, v)
	require.NoError(t, err)

	f, err := NewWithValidator(v, false, 1, trasher)
	require.NoError(t, err)

	c := collector.New(collector.Options{SkipDirs: []string{".file-dedup"}})
	files, err := c.Collect(tmpDir)
	require.NoError(t, err)
	require.Len(t, files, 2)

	result := f.FlattenFiles(files)

	assert.Equal(t, 2, result.TotalFiles)
	assert.Equal(t, 1, result.MovedCount)
	assert.Equal(t, 1, result.DuplicatesCount)
	assert.Equal(t, 0, result.ErrorCount)

	// One file should exist in root.
	assert.FileExists(t, filepath.Join(tmpDir, "file.txt"))

	// Find the duplicate operation.
	var dupOp *MoveOperation
	for i := range result.Operations {
		if result.Operations[i].Duplicate {
			dupOp = &result.Operations[i]
			break
		}
	}
	require.NotNil(t, dupOp, "should have a duplicate operation")
	assert.NotEmpty(t, dupOp.TrashedTo, "TrashedTo should be populated")
	assert.Contains(t, dupOp.TrashedTo, ".file-dedup/trash/")

	// The trashed file should exist at the trash destination.
	assert.FileExists(t, dupOp.TrashedTo)
}

func TestFlattener_FlattenFiles_UnsafeSymlinkFailsBeforeMutations(t *testing.T) {
	tmpDir := t.TempDir()

	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "outside.txt")
	require.NoError(t, os.WriteFile(outsideFile, []byte("outside"), 0o600))

	safeFile := filepath.Join(tmpDir, "nested", "safe.txt")
	createTestFile(t, safeFile, "safe-content", time.Now().UTC())

	linkPath := filepath.Join(tmpDir, "nested", "escape_link.txt")
	if err := os.Symlink(outsideFile, linkPath); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	files := collectFiles(t, tmpDir)

	f, err := New(tmpDir, false)
	require.NoError(t, err)
	result := f.FlattenFiles(files)

	assert.Equal(t, 0, result.MovedCount)
	assert.Equal(t, 1, result.ErrorCount)
	require.Len(t, result.Operations, 1)
	require.ErrorIs(t, result.Operations[0].Error, safepath.ErrSymlinkEscape)

	assert.FileExists(t, safeFile)
	assert.NoFileExists(t, filepath.Join(tmpDir, "safe.txt"))
}

// Test that a duplicate is preserved when its content changed after hashing.
func TestFlattener_FlattenFiles_DuplicatePreservedWhenContentChanged(t *testing.T) {
	tmpDir := t.TempDir()

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	// Create two identical files in subdirectories.
	keptFile := filepath.Join(tmpDir, "dir1", "file.txt")
	dupeFile := filepath.Join(tmpDir, "dir2", "file.txt")
	createTestFile(t, keptFile, "content", modTime)
	createTestFile(t, dupeFile, "content", modTime)

	files := collectFiles(t, tmpDir)
	require.Len(t, files, 2)

	f, err := New(tmpDir, false)
	require.NoError(t, err)

	// Pre-compute hashes (same as FlattenFiles does internally).
	fileHashes, _ := f.computeHashes(files, nil)
	seenHash := make(map[string]string)
	nameCount := make(map[string]int)

	// Process the first file (will be moved to root).
	op1 := f.processFile(&files[0], fileHashes[files[0].Path], seenHash, nameCount)
	require.NoError(t, op1.Error)
	require.False(t, op1.Duplicate)

	// Now modify the second file on disk to simulate content changing after hashing.
	require.NoError(t, os.WriteFile(dupeFile, []byte("modified content"), 0o600))

	// Process the second file — it has the same hash in the map so it's a duplicate,
	// but the re-hash should detect the change and refuse to delete.
	op2 := f.processFile(&files[1], fileHashes[files[1].Path], seenHash, nameCount)
	require.Error(t, op2.Error, "should refuse to delete duplicate when content changed")
	assert.True(t, op2.Duplicate)
	require.ErrorIs(t, op2.Error, ErrContentChanged)

	// The duplicate should still exist on disk.
	assert.FileExists(t, dupeFile)
}
