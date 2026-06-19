package deduplicator

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"file-dedup/internal/testutil"
	"file-dedup/pkg/collector"
	"file-dedup/pkg/hasher"
	"file-dedup/pkg/metadata"
	"file-dedup/pkg/safepath"
	"file-dedup/pkg/trash"
)

func setupTestDir(t *testing.T) string {
	t.Helper()
	return testutil.TempDir(t)
}

func createTestFile(t *testing.T, path, content string, modTime time.Time) {
	t.Helper()
	testutil.CreateFileWithModTime(t, path, content, modTime)
}

func createTestFileBytes(t *testing.T, path string, content []byte, modTime time.Time) {
	t.Helper()
	testutil.CreateFileBytesWithModTime(t, path, content, modTime)
}

func collectDeduplicatorFiles(t *testing.T, root string) []collector.FileInfo {
	t.Helper()

	c := collector.New(collector.Options{})
	files, err := c.Collect(root)
	require.NoError(t, err)

	return files
}

// Test FindDuplicates with no duplicates (all unique content).
func TestDeduplicator_FindDuplicates_NoDuplicates(t *testing.T) {
	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	createTestFile(t, filepath.Join(tmpDir, "file1.txt"), "unique content 1", modTime)
	createTestFile(t, filepath.Join(tmpDir, "file2.txt"), "unique content 2", modTime)
	createTestFile(t, filepath.Join(tmpDir, "file3.txt"), "unique content 3", modTime)

	c := collector.New(collector.Options{})
	files, err := c.Collect(tmpDir)
	require.NoError(t, err)

	d, err := New(tmpDir, true)
	require.NoError(t, err)
	result := d.FindDuplicates(files)

	assert.Equal(t, 3, result.TotalFiles)
	assert.Equal(t, 0, result.DuplicatesFound)
	assert.Equal(t, 0, result.DeletedCount)
	assert.Empty(t, result.Operations)
}

func TestDeduplicator_NewWithValidatorRejectsNil(t *testing.T) {
	t.Parallel()

	_, err := NewWithValidator(nil, false, 0, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "validator is required")
}

func TestDeduplicator_FindDuplicates_InvalidSourceCountsError(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	createTestFile(t, outside, "outside", time.Now())

	d, err := New(root, true)
	require.NoError(t, err)
	result := d.FindDuplicates([]collector.FileInfo{{
		Path: outside,
		Dir:  filepath.Dir(outside),
		Name: filepath.Base(outside),
		Size: int64(len("outside")),
	}})

	assert.Equal(t, 1, result.TotalFiles)
	assert.Equal(t, 1, result.ErrorCount)
	require.Len(t, result.Operations, 1)
	assert.Equal(t, outside, result.Operations[0].Path)
	assert.Equal(t, int64(len("outside")), result.Operations[0].Size)
	assert.ErrorIs(t, result.Operations[0].Error, safepath.ErrPathEscape)
}

// Test FindDuplicates with identical files (same content).
func TestDeduplicator_FindDuplicates_IdenticalContent(t *testing.T) {
	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	content := "identical content"

	createTestFile(t, filepath.Join(tmpDir, "original.txt"), content, modTime)
	createTestFile(t, filepath.Join(tmpDir, "duplicate.txt"), content, modTime)

	c := collector.New(collector.Options{})
	files, err := c.Collect(tmpDir)
	require.NoError(t, err)
	require.Len(t, files, 2)

	// Dry run first.
	d, err := New(tmpDir, true)
	require.NoError(t, err)
	result := d.FindDuplicates(files)

	assert.Equal(t, 2, result.TotalFiles)
	assert.Equal(t, 1, result.DuplicatesFound)
	assert.Equal(t, 1, result.DeletedCount)
	assert.Equal(t, 0, result.SkippedCount)
	assert.Equal(t, 0, result.ErrorCount)
	assert.Equal(t, int64(len(content)), result.BytesRecovered)
	require.Len(t, result.Operations, 1)

	op := result.Operations[0]
	assert.Equal(t, filepath.Join(tmpDir, "original.txt"), op.Path)
	assert.Equal(t, filepath.Join(tmpDir, "duplicate.txt"), op.OriginalOf)
	assert.Equal(t, int64(len(content)), op.Size)
	assert.NotEmpty(t, op.Hash)
	assert.False(t, op.Skipped)
	assert.Empty(t, op.SkipReason)
	assert.Empty(t, op.TrashedTo)
	assert.NoError(t, op.Error)

	// Both files should still exist (dry run).
	_, err = os.Stat(filepath.Join(tmpDir, "original.txt"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(tmpDir, "duplicate.txt"))
	require.NoError(t, err)
}

func TestDeduplicator_FindDuplicatesWithProgress_SmallFiles(t *testing.T) {
	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	content := "same content"
	createTestFile(t, filepath.Join(tmpDir, "a.txt"), content, modTime)
	createTestFile(t, filepath.Join(tmpDir, "b.txt"), content, modTime)
	createTestFile(t, filepath.Join(tmpDir, "unique.txt"), "unique", modTime)

	files := collectDeduplicatorFiles(t, tmpDir)

	d, err := New(tmpDir, true)
	require.NoError(t, err)

	type event struct {
		stage     string
		processed int
		total     int
	}
	var events []event
	result := d.FindDuplicatesWithProgress(files, func(stage string, processed, total int) {
		events = append(events, event{stage: stage, processed: processed, total: total})
	})

	require.Len(t, result.Operations, 1)
	assert.Equal(t, []event{
		{stage: progressStageHashing, processed: 1, total: 2},
		{stage: progressStageHashing, processed: 2, total: 2},
		{stage: progressStageDeleting, processed: 1, total: 1},
	}, events)
}

func TestDeduplicator_FindDuplicates_ThresholdSizedFiles(t *testing.T) {
	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	content := make([]byte, hasher.SmallFileThreshold)
	for i := range content {
		content[i] = byte(i % 251)
	}
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	createTestFileBytes(t, filepath.Join(tmpDir, "a.bin"), content, modTime)
	createTestFileBytes(t, filepath.Join(tmpDir, "b.bin"), content, modTime)

	files := collectDeduplicatorFiles(t, tmpDir)

	d, err := New(tmpDir, true)
	require.NoError(t, err)
	result := d.FindDuplicates(files)

	assert.Equal(t, 2, result.TotalFiles)
	assert.Equal(t, 1, result.DuplicatesFound)
	require.Len(t, result.Operations, 1)
	assert.Equal(t, int64(hasher.SmallFileThreshold), result.Operations[0].Size)
}

// Test FindDuplicates actually deletes files.
func TestDeduplicator_FindDuplicates_ActualDelete(t *testing.T) {
	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	content := "same content"

	createTestFile(t, filepath.Join(tmpDir, "aaa.txt"), content, modTime)
	createTestFile(t, filepath.Join(tmpDir, "bbb.txt"), content, modTime)

	c := collector.New(collector.Options{})
	files, err := c.Collect(tmpDir)
	require.NoError(t, err)

	// Real run (not dry run).
	d, err := New(tmpDir, false)
	require.NoError(t, err)
	result := d.FindDuplicates(files)

	assert.Equal(t, 1, result.DeletedCount)
	assert.Equal(t, int64(len(content)), result.BytesRecovered)

	// One file should exist, one should be gone.
	entries, err := os.ReadDir(tmpDir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

// Test with multiple duplicates (3+ identical files).
func TestDeduplicator_FindDuplicates_MultipleDuplicates(t *testing.T) {
	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	content := "same content everywhere"

	createTestFile(t, filepath.Join(tmpDir, "file1.txt"), content, modTime)
	createTestFile(t, filepath.Join(tmpDir, "file2.txt"), content, modTime)
	createTestFile(t, filepath.Join(tmpDir, "file3.txt"), content, modTime)
	createTestFile(t, filepath.Join(tmpDir, "file4.txt"), content, modTime)

	c := collector.New(collector.Options{})
	files, err := c.Collect(tmpDir)
	require.NoError(t, err)
	require.Len(t, files, 4)

	d, err := New(tmpDir, false)
	require.NoError(t, err)
	result := d.FindDuplicates(files)

	assert.Equal(t, 4, result.TotalFiles)
	assert.Equal(t, 3, result.DuplicatesFound)
	assert.Equal(t, 3, result.DeletedCount)

	// Only one file should remain.
	entries, err := os.ReadDir(tmpDir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

// Test that files with same size but different content are NOT duplicates.
func TestDeduplicator_FindDuplicates_SameSizeDifferentContent(t *testing.T) {
	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)

	// Same size (10 bytes) but different content.
	createTestFile(t, filepath.Join(tmpDir, "file1.txt"), "aaaaaaaaaa", modTime)
	createTestFile(t, filepath.Join(tmpDir, "file2.txt"), "bbbbbbbbbb", modTime)

	c := collector.New(collector.Options{})
	files, err := c.Collect(tmpDir)
	require.NoError(t, err)

	d, err := New(tmpDir, true)
	require.NoError(t, err)
	result := d.FindDuplicates(files)

	assert.Equal(t, 0, result.DuplicatesFound)
	assert.Empty(t, result.Operations)
}

// Test that files with different sizes are NOT considered for hashing.
func TestDeduplicator_FindDuplicates_DifferentSizes(t *testing.T) {
	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)

	createTestFile(t, filepath.Join(tmpDir, "small.txt"), "small", modTime)
	createTestFile(t, filepath.Join(tmpDir, "medium.txt"), "medium content", modTime)
	createTestFile(t, filepath.Join(tmpDir, "large.txt"), "this is a larger file", modTime)

	c := collector.New(collector.Options{})
	files, err := c.Collect(tmpDir)
	require.NoError(t, err)

	d, err := New(tmpDir, true)
	require.NoError(t, err)
	result := d.FindDuplicates(files)

	assert.Equal(t, 3, result.TotalFiles)
	assert.Equal(t, 0, result.DuplicatesFound)
}

// Test with empty file list.
func TestDeduplicator_FindDuplicates_Empty(t *testing.T) {
	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	d, err := New(tmpDir, true)
	require.NoError(t, err)
	result := d.FindDuplicates([]collector.FileInfo{})

	assert.Equal(t, 0, result.TotalFiles)
	assert.Equal(t, 0, result.DuplicatesFound)
	assert.Empty(t, result.Operations)
}

// Test real-world scenario: duplicate files with different names.
func TestDeduplicator_FindDuplicates_RealWorldScenario(t *testing.T) {
	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	modTime := time.Date(2017, 8, 3, 12, 0, 0, 0, time.UTC)
	videoContent := "fake video content for testing"

	// Simulate the flattening scenario: original + renamed duplicate.
	createTestFile(t, filepath.Join(tmpDir, "2017-08-03_4_kills_eco_round.flv"), videoContent, modTime)
	createTestFile(t, filepath.Join(tmpDir, "2017-08-03_4_kills_eco_round_1.flv"), videoContent, modTime)

	c := collector.New(collector.Options{})
	files, err := c.Collect(tmpDir)
	require.NoError(t, err)

	d, err := New(tmpDir, false)
	require.NoError(t, err)
	result := d.FindDuplicates(files)

	assert.Equal(t, 1, result.DuplicatesFound)
	assert.Equal(t, 1, result.DeletedCount)

	// Only one file should remain.
	entries, err := os.ReadDir(tmpDir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

// Test with mixed: some duplicates, some unique.
func TestDeduplicator_FindDuplicates_MixedFiles(t *testing.T) {
	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)

	// Group 1: duplicates (same content).
	createTestFile(t, filepath.Join(tmpDir, "video1.mp4"), "video content", modTime)
	createTestFile(t, filepath.Join(tmpDir, "video1_copy.mp4"), "video content", modTime)

	// Group 2: unique file.
	createTestFile(t, filepath.Join(tmpDir, "photo.jpg"), "photo data", modTime)

	// Group 3: another set of duplicates.
	createTestFile(t, filepath.Join(tmpDir, "doc.pdf"), "document text", modTime)
	createTestFile(t, filepath.Join(tmpDir, "doc_backup.pdf"), "document text", modTime)
	createTestFile(t, filepath.Join(tmpDir, "doc_copy.pdf"), "document text", modTime)

	c := collector.New(collector.Options{})
	files, err := c.Collect(tmpDir)
	require.NoError(t, err)
	require.Len(t, files, 6)

	d, err := New(tmpDir, false)
	require.NoError(t, err)
	result := d.FindDuplicates(files)

	assert.Equal(t, 6, result.TotalFiles)
	assert.Equal(t, 3, result.DuplicatesFound) // 1 from video, 2 from doc
	assert.Equal(t, 3, result.DeletedCount)

	// Check remaining files.
	entries, err := os.ReadDir(tmpDir)
	require.NoError(t, err)
	require.Len(t, entries, 3)
}

// Test in subdirectories (recursive).
func TestDeduplicator_FindDuplicates_Recursive(t *testing.T) {
	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	content := "same content"

	// Files in subdirectory.
	createTestFile(t, filepath.Join(tmpDir, "subdir", "file.txt"), content, modTime)
	createTestFile(t, filepath.Join(tmpDir, "subdir", "nested", "file_copy.txt"), content, modTime)

	// Files in root.
	createTestFile(t, filepath.Join(tmpDir, "root.txt"), content, modTime)

	c := collector.New(collector.Options{})
	files, err := c.Collect(tmpDir)
	require.NoError(t, err)
	require.Len(t, files, 3)

	d, err := New(tmpDir, false)
	require.NoError(t, err)
	result := d.FindDuplicates(files)

	// All 3 files have same content, so 2 are duplicates.
	assert.Equal(t, 2, result.DuplicatesFound)
	assert.Equal(t, 2, result.DeletedCount)
}

// Test DryRun getter.
func TestDeduplicator_DryRun(t *testing.T) {
	tmpDir := t.TempDir()

	d, err := New(tmpDir, true)
	require.NoError(t, err)
	assert.True(t, d.DryRun())

	d, err = NewWithWorkers(tmpDir, false, 2)
	require.NoError(t, err)
	assert.False(t, d.DryRun())
}

// Test Root getter.
func TestDeduplicator_Root(t *testing.T) {
	tmpDir := t.TempDir()

	d, err := NewWithWorkers(tmpDir, false, 2)
	require.NoError(t, err)
	assert.Equal(t, tmpDir, d.Root())
}

// Test New with invalid root.
func TestNew_InvalidRoot(t *testing.T) {
	_, err := NewWithWorkers("/nonexistent/path/12345", false, 2)
	assert.Error(t, err)
}

// Test BytesRecovered calculation.
func TestDeduplicator_FindDuplicates_BytesRecovered(t *testing.T) {
	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	content := "1234567890" // 10 bytes

	createTestFile(t, filepath.Join(tmpDir, "a.txt"), content, modTime)
	createTestFile(t, filepath.Join(tmpDir, "b.txt"), content, modTime)
	createTestFile(t, filepath.Join(tmpDir, "c.txt"), content, modTime)

	c := collector.New(collector.Options{})
	files, err := c.Collect(tmpDir)
	require.NoError(t, err)

	d, err := New(tmpDir, false)
	require.NoError(t, err)
	result := d.FindDuplicates(files)

	assert.Equal(t, 2, result.DeletedCount)
	assert.Equal(t, int64(20), result.BytesRecovered) // 10 bytes * 2 files
}

func TestResultCalculateCounts_AllOperationOutcomes(t *testing.T) {
	result := Result{Operations: []DeleteOperation{
		{Path: "deleted-a", OriginalOf: "kept-a", Size: 10},
		{Path: "deleted-b", OriginalOf: "kept-b", Size: 20},
		{Path: "skipped", OriginalOf: "kept-c", Size: 30, Skipped: true, SkipReason: "kept"},
		{Path: "errored", Size: 40, Error: os.ErrPermission},
	}}

	result.calculateCounts()

	assert.Equal(t, 3, result.DuplicatesFound)
	assert.Equal(t, 2, result.DeletedCount)
	assert.Equal(t, 1, result.SkippedCount)
	assert.Equal(t, 1, result.ErrorCount)
	assert.Equal(t, int64(30), result.BytesRecovered)
}

func TestGroupBySizeAndBuildDuplicateGroups(t *testing.T) {
	files := []collector.FileInfo{
		{Path: "z.txt", Size: 10},
		{Path: "a.txt", Size: 10},
		{Path: "unique.txt", Size: 5},
	}

	sizeGroups := groupBySize(files)
	require.Len(t, sizeGroups[10], 2)
	require.Len(t, sizeGroups[5], 1)

	duplicateGroups := buildDuplicateGroups(map[string][]collector.FileInfo{
		"hash":       sizeGroups[10],
		"uniqueHash": sizeGroups[5],
	})

	require.Len(t, duplicateGroups, 1)
	assert.Equal(t, "hash", duplicateGroups[0].Hash)
	assert.Equal(t, int64(10), duplicateGroups[0].Size)
	assert.Equal(t, "a.txt", duplicateGroups[0].Keep.Path)
	require.Len(t, duplicateGroups[0].Dupes, 1)
	assert.Equal(t, "z.txt", duplicateGroups[0].Dupes[0].Path)
}

func TestDeduplicatorDeleteFile_DryRunAndErrorBranches(t *testing.T) {
	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	keptPath := filepath.Join(tmpDir, "kept.txt")
	dupePath := filepath.Join(tmpDir, "dupe.txt")
	createTestFile(t, keptPath, "same", modTime)
	createTestFile(t, dupePath, "same", modTime)

	dryRunDeduper, err := New(tmpDir, true)
	require.NoError(t, err)
	hash, err := dryRunDeduper.hasher.ComputeHash(dupePath)
	require.NoError(t, err)

	dryRunOp := dryRunDeduper.deleteFile(collector.FileInfo{Path: dupePath, Size: 4}, keptPath, hash)
	assert.Equal(t, dupePath, dryRunOp.Path)
	assert.Equal(t, keptPath, dryRunOp.OriginalOf)
	assert.Equal(t, int64(4), dryRunOp.Size)
	assert.Equal(t, hash, dryRunOp.Hash)
	assert.Empty(t, dryRunOp.TrashedTo)
	assert.NoError(t, dryRunOp.Error)
	assert.FileExists(t, dupePath)

	liveDeduper, err := New(tmpDir, false)
	require.NoError(t, err)
	missingKeptOp := liveDeduper.deleteFile(collector.FileInfo{Path: dupePath, Size: 4}, filepath.Join(tmpDir, "missing.txt"), hash)
	require.Error(t, missingKeptOp.Error)
	assert.Contains(t, missingKeptOp.Error.Error(), "kept file missing")
	assert.FileExists(t, dupePath)

	require.NoError(t, os.WriteFile(dupePath, []byte("changed"), 0o600))
	changedOp := liveDeduper.deleteFile(collector.FileInfo{Path: dupePath, Size: 4}, keptPath, hash)
	require.ErrorIs(t, changedOp.Error, ErrContentChanged)
	assert.FileExists(t, dupePath)
}

func TestDeduplicatorGroupFilesByHashFiltersErrorsAndUnknownResults(t *testing.T) {
	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	knownA := filepath.Join(tmpDir, "a.txt")
	knownB := filepath.Join(tmpDir, "b.txt")
	createTestFile(t, knownA, "same", modTime)
	createTestFile(t, knownB, "same", modTime)

	d, err := New(tmpDir, true)
	require.NoError(t, err)

	files := []collector.FileInfo{
		{Path: knownA, Size: 4},
		{Path: knownB, Size: 4},
	}

	type event struct {
		stage     string
		processed int
		total     int
	}
	var events []event
	groups := d.groupFilesByHash(files, func(toHash []hasher.FileToHash) <-chan hasher.HashResult {
		ch := make(chan hasher.HashResult, len(toHash)+2)
		ch <- hasher.HashResult{Path: knownA, Hash: "hash"}
		ch <- hasher.HashResult{Path: filepath.Join(tmpDir, "unknown.txt"), Hash: "hash"}
		ch <- hasher.HashResult{Path: knownB, Error: os.ErrPermission}
		close(ch)
		return ch
	}, progressStageHashing, func(stage string, processed, total int) {
		events = append(events, event{stage: stage, processed: processed, total: total})
	})

	require.Len(t, groups, 1)
	require.Len(t, groups["hash"], 1)
	assert.Equal(t, knownA, groups["hash"][0].Path)
	assert.Equal(t, []event{
		{stage: progressStageHashing, processed: 1, total: 2},
		{stage: progressStageHashing, processed: 2, total: 2},
		{stage: progressStageHashing, processed: 2, total: 2},
	}, events)
}

func TestDeduplicatorFindDuplicatesInSizeGroupRequiresPair(t *testing.T) {
	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	d, err := New(tmpDir, true)
	require.NoError(t, err)

	groups := d.findDuplicatesInSizeGroup([]collector.FileInfo{{Path: filepath.Join(tmpDir, "single.txt"), Size: 1}}, nil)
	assert.Nil(t, groups)
}

// Test deterministic ordering of operations.
func TestDeduplicator_FindDuplicates_DeterministicOrdering(t *testing.T) {
	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	content := "content"

	// Create many files.
	createTestFile(t, filepath.Join(tmpDir, "z.txt"), content, modTime)
	createTestFile(t, filepath.Join(tmpDir, "a.txt"), content, modTime)
	createTestFile(t, filepath.Join(tmpDir, "m.txt"), content, modTime)

	c := collector.New(collector.Options{})
	files, err := c.Collect(tmpDir)
	require.NoError(t, err)

	d, err := New(tmpDir, true)
	require.NoError(t, err)

	// Run multiple times to ensure deterministic ordering.
	var lastPaths []string
	for range 5 {
		result := d.FindDuplicates(files)
		paths := make([]string, len(result.Operations))
		for j, op := range result.Operations {
			paths[j] = op.Path
		}

		if lastPaths != nil {
			assert.Equal(t, lastPaths, paths, "operations should be in same order")
		}
		lastPaths = paths
	}
}

// Test ComputeFileHash function.
func TestComputeFileHash(t *testing.T) {
	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	content := "test content for hashing"

	path1 := filepath.Join(tmpDir, "file1.txt")
	path2 := filepath.Join(tmpDir, "file2.txt")

	createTestFile(t, path1, content, modTime)
	createTestFile(t, path2, content, modTime)

	hash1, err := ComputeFileHash(path1)
	require.NoError(t, err)

	hash2, err := ComputeFileHash(path2)
	require.NoError(t, err)

	assert.Equal(t, hash1, hash2)
	assert.Len(t, hash1, 64) // SHA256 hex string is 64 chars
}

// Test ComputeFileHash with different content.
func TestComputeFileHash_DifferentContent(t *testing.T) {
	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)

	path1 := filepath.Join(tmpDir, "file1.txt")
	path2 := filepath.Join(tmpDir, "file2.txt")

	createTestFile(t, path1, "content A", modTime)
	createTestFile(t, path2, "content B", modTime)

	hash1, err := ComputeFileHash(path1)
	require.NoError(t, err)

	hash2, err := ComputeFileHash(path2)
	require.NoError(t, err)

	assert.NotEqual(t, hash1, hash2)
}

// Test with large files (triggers partial hash optimization).
func TestDeduplicator_FindDuplicates_LargeFiles(t *testing.T) {
	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)

	// Create large files (> 8KB to trigger partial hash).
	largeContent := make([]byte, 20000) // 20KB
	_, err := rand.Read(largeContent)
	require.NoError(t, err)

	createTestFileBytes(t, filepath.Join(tmpDir, "large1.bin"), largeContent, modTime)
	createTestFileBytes(t, filepath.Join(tmpDir, "large2.bin"), largeContent, modTime)

	c := collector.New(collector.Options{})
	files, err := c.Collect(tmpDir)
	require.NoError(t, err)
	require.Len(t, files, 2)

	d, err := New(tmpDir, false)
	require.NoError(t, err)
	result := d.FindDuplicates(files)

	assert.Equal(t, 1, result.DuplicatesFound)
	assert.Equal(t, 1, result.DeletedCount)
}

// Test large files with same size but different content.
func TestDeduplicator_FindDuplicates_LargeFilesDifferentContent(t *testing.T) {
	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)

	// Create two large files with same size but different content.
	size := 20000
	content1 := make([]byte, size)
	content2 := make([]byte, size)

	_, err := rand.Read(content1)
	require.NoError(t, err)
	_, err = rand.Read(content2)
	require.NoError(t, err)

	createTestFileBytes(t, filepath.Join(tmpDir, "large1.bin"), content1, modTime)
	createTestFileBytes(t, filepath.Join(tmpDir, "large2.bin"), content2, modTime)

	c := collector.New(collector.Options{})
	files, err := c.Collect(tmpDir)
	require.NoError(t, err)

	d, err := New(tmpDir, true)
	require.NoError(t, err)
	result := d.FindDuplicates(files)

	// Different content = not duplicates.
	assert.Equal(t, 0, result.DuplicatesFound)
}

// Test large files where only middle differs (partial hash won't catch it).
func TestDeduplicator_FindDuplicates_LargeFilesMiddleDiffers(t *testing.T) {
	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)

	// Create two files: same first 4KB and last 4KB, but different middle.
	size := 20000
	content1 := make([]byte, size)
	for i := range content1 {
		content1[i] = byte(i % 256)
	}

	content2 := make([]byte, size)
	copy(content2, content1)
	// Change middle portion.
	for i := 5000; i < 15000; i++ {
		content2[i] = byte((i + 128) % 256)
	}

	createTestFileBytes(t, filepath.Join(tmpDir, "file1.bin"), content1, modTime)
	createTestFileBytes(t, filepath.Join(tmpDir, "file2.bin"), content2, modTime)

	c := collector.New(collector.Options{})
	files, err := c.Collect(tmpDir)
	require.NoError(t, err)

	d, err := New(tmpDir, true)
	require.NoError(t, err)
	result := d.FindDuplicates(files)

	// Full hash should catch the difference.
	assert.Equal(t, 0, result.DuplicatesFound)
}

// Test empty files (edge case).
func TestDeduplicator_FindDuplicates_EmptyFiles(t *testing.T) {
	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)

	createTestFile(t, filepath.Join(tmpDir, "empty1.txt"), "", modTime)
	createTestFile(t, filepath.Join(tmpDir, "empty2.txt"), "", modTime)

	c := collector.New(collector.Options{})
	files, err := c.Collect(tmpDir)
	require.NoError(t, err)

	d, err := New(tmpDir, false)
	require.NoError(t, err)
	result := d.FindDuplicates(files)

	// Empty files are duplicates of each other.
	assert.Equal(t, 1, result.DuplicatesFound)
}

// Test hash is included in operations.
func TestDeduplicator_FindDuplicates_HashInOperations(t *testing.T) {
	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	content := "test content"

	createTestFile(t, filepath.Join(tmpDir, "a.txt"), content, modTime)
	createTestFile(t, filepath.Join(tmpDir, "b.txt"), content, modTime)

	c := collector.New(collector.Options{})
	files, err := c.Collect(tmpDir)
	require.NoError(t, err)

	d, err := New(tmpDir, true)
	require.NoError(t, err)
	result := d.FindDuplicates(files)

	require.Len(t, result.Operations, 1)
	assert.NotEmpty(t, result.Operations[0].Hash)
	assert.Len(t, result.Operations[0].Hash, 64) // SHA256 hex
}

// Test that operations include reference to original file.
func TestDeduplicator_FindDuplicates_OriginalOfReference(t *testing.T) {
	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	content := "test content"

	createTestFile(t, filepath.Join(tmpDir, "original.txt"), content, modTime)
	createTestFile(t, filepath.Join(tmpDir, "duplicate.txt"), content, modTime)

	c := collector.New(collector.Options{})
	files, err := c.Collect(tmpDir)
	require.NoError(t, err)

	d, err := New(tmpDir, true)
	require.NoError(t, err)
	result := d.FindDuplicates(files)

	require.Len(t, result.Operations, 1)
	assert.NotEmpty(t, result.Operations[0].OriginalOf)
	// The OriginalOf should be one of the files.
	assert.True(t,
		result.Operations[0].OriginalOf == filepath.Join(tmpDir, "original.txt") ||
			result.Operations[0].OriginalOf == filepath.Join(tmpDir, "duplicate.txt"))
}

// Test multiple duplicate groups.
func TestDeduplicator_FindDuplicates_MultipleDuplicateGroups(t *testing.T) {
	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)

	// Group A: 3 identical files.
	createTestFile(t, filepath.Join(tmpDir, "a1.txt"), "group A", modTime)
	createTestFile(t, filepath.Join(tmpDir, "a2.txt"), "group A", modTime)
	createTestFile(t, filepath.Join(tmpDir, "a3.txt"), "group A", modTime)

	// Group B: 2 identical files.
	createTestFile(t, filepath.Join(tmpDir, "b1.txt"), "group B", modTime)
	createTestFile(t, filepath.Join(tmpDir, "b2.txt"), "group B", modTime)

	// Unique file.
	createTestFile(t, filepath.Join(tmpDir, "unique.txt"), "unique", modTime)

	c := collector.New(collector.Options{})
	files, err := c.Collect(tmpDir)
	require.NoError(t, err)

	d, err := New(tmpDir, false)
	require.NoError(t, err)
	result := d.FindDuplicates(files)

	assert.Equal(t, 6, result.TotalFiles)
	assert.Equal(t, 3, result.DuplicatesFound) // 2 from group A, 1 from group B
	assert.Equal(t, 3, result.DeletedCount)

	// 3 files should remain.
	entries, err := os.ReadDir(tmpDir)
	require.NoError(t, err)
	assert.Len(t, entries, 3)
}

// Test partial hash calculation.
func TestComputePartialHash(t *testing.T) {
	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)

	// Create a large file.
	size := 20000
	content := make([]byte, size)
	for i := range content {
		content[i] = byte(i % 256)
	}

	path := filepath.Join(tmpDir, "large.bin")
	createTestFileBytes(t, path, content, modTime)

	h := hasher.New()
	hash, err := h.ComputePartialHash(path, int64(size))
	require.NoError(t, err)
	assert.NotEmpty(t, hash)
	assert.Len(t, hash, 64)
}

// Test that files are kept deterministically (alphabetically first).
func TestDeduplicator_FindDuplicates_KeepsDeterministically(t *testing.T) {
	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	content := "content"

	createTestFile(t, filepath.Join(tmpDir, "zzz.txt"), content, modTime)
	createTestFile(t, filepath.Join(tmpDir, "aaa.txt"), content, modTime)
	createTestFile(t, filepath.Join(tmpDir, "mmm.txt"), content, modTime)

	c := collector.New(collector.Options{})
	files, err := c.Collect(tmpDir)
	require.NoError(t, err)

	d, err := New(tmpDir, false)
	require.NoError(t, err)
	result := d.FindDuplicates(files)

	assert.Equal(t, 2, result.DeletedCount)

	// aaa.txt should be kept (alphabetically first).
	entries, err := os.ReadDir(tmpDir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "aaa.txt", entries[0].Name())
}

// Test that deleting a duplicate is refused when the kept (original) file has disappeared.
func TestDeduplicator_DeleteFile_RefusesWhenKeptFileMissing(t *testing.T) {
	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)

	// Create the "duplicate" file that would be deleted.
	dupPath := filepath.Join(tmpDir, "duplicate.txt")
	createTestFile(t, dupPath, "same content", modTime)

	// The "original" path that no longer exists on disk.
	originalPath := filepath.Join(tmpDir, "original.txt")

	v, err := safepath.New(tmpDir)
	require.NoError(t, err)

	d, err := NewWithValidator(v, false, 1, nil)
	require.NoError(t, err)

	dupFile := collector.FileInfo{
		Path:    dupPath,
		Dir:     tmpDir,
		Name:    "duplicate.txt",
		Size:    int64(len("same content")),
		ModTime: modTime,
	}

	op := d.deleteFile(dupFile, originalPath, "somehash")

	require.Error(t, op.Error, "should error when kept file is missing")
	assert.Contains(t, op.Error.Error(), "kept file missing", "error should mention kept file missing")

	// Duplicate must still exist on disk.
	assert.FileExists(t, dupPath, "duplicate must be preserved when kept file is gone")
}

func TestDeduplicator_FindDuplicates_UnsafeSymlinkFailsBeforeDeletes(t *testing.T) {
	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	createTestFile(t, filepath.Join(tmpDir, "a.txt"), "same", modTime)
	createTestFile(t, filepath.Join(tmpDir, "b.txt"), "same", modTime)

	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "outside.txt")
	require.NoError(t, os.WriteFile(outsideFile, []byte("outside"), 0o600))

	linkPath := filepath.Join(tmpDir, "escape_link.txt")
	if err := os.Symlink(outsideFile, linkPath); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	c := collector.New(collector.Options{})
	files, err := c.Collect(tmpDir)
	require.NoError(t, err)

	d, err := New(tmpDir, false)
	require.NoError(t, err)
	result := d.FindDuplicates(files)

	assert.Equal(t, 1, result.ErrorCount)
	assert.Equal(t, 0, result.DuplicatesFound)
	assert.Equal(t, 0, result.DeletedCount)
	require.Len(t, result.Operations, 1)
	require.ErrorIs(t, result.Operations[0].Error, safepath.ErrSymlinkEscape)

	assert.FileExists(t, filepath.Join(tmpDir, "a.txt"))
	assert.FileExists(t, filepath.Join(tmpDir, "b.txt"))
}

// Test that duplicates are trashed (not permanently deleted) when a trasher is provided.
func TestDeduplicator_FindDuplicates_TrashesFilesWhenTrasherProvided(t *testing.T) {
	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	content := "identical content for trash test"

	createTestFile(t, filepath.Join(tmpDir, "keep.txt"), content, modTime)
	createTestFile(t, filepath.Join(tmpDir, "zzz_duplicate.txt"), content, modTime)

	v, err := safepath.New(tmpDir)
	require.NoError(t, err)

	metaDir, err := metadata.Init(tmpDir, v)
	require.NoError(t, err)

	runID := metaDir.RunID("duplicate")
	trasher, err := trash.New(metaDir, runID, v)
	require.NoError(t, err)

	d, err := NewWithValidator(v, false, 1, trasher)
	require.NoError(t, err)

	c := collector.New(collector.Options{SkipDirs: []string{".file-dedup"}})
	files, err := c.Collect(tmpDir)
	require.NoError(t, err)
	require.Len(t, files, 2)

	result := d.FindDuplicates(files)

	assert.Equal(t, 2, result.TotalFiles)
	assert.Equal(t, 1, result.DuplicatesFound)
	assert.Equal(t, 1, result.DeletedCount)
	assert.Equal(t, 0, result.ErrorCount)

	// The kept file (alphabetically first) should remain.
	assert.FileExists(t, filepath.Join(tmpDir, "keep.txt"))

	// The duplicate should be gone from original location.
	assert.NoFileExists(t, filepath.Join(tmpDir, "zzz_duplicate.txt"))

	// The operation should have a populated TrashedTo field.
	require.Len(t, result.Operations, 1)
	assert.NotEmpty(t, result.Operations[0].TrashedTo, "TrashedTo should be populated")
	assert.Contains(t, result.Operations[0].TrashedTo, ".file-dedup/trash/")

	// The trashed file should exist at the trash destination.
	assert.FileExists(t, result.Operations[0].TrashedTo)
}

// Test that deleting a duplicate is refused when the file content changed after hashing.
func TestDeduplicator_DeleteFile_RefusesWhenContentChanged(t *testing.T) {
	tmpDir := setupTestDir(t)
	defer os.RemoveAll(tmpDir)

	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	content := "same content"

	// Create the "original" (kept) file and the "duplicate".
	originalPath := filepath.Join(tmpDir, "original.txt")
	dupPath := filepath.Join(tmpDir, "duplicate.txt")
	createTestFile(t, originalPath, content, modTime)
	createTestFile(t, dupPath, content, modTime)

	// Compute the hash of the duplicate while it still matches.
	h := hasher.New()
	originalHash, err := h.ComputeHash(dupPath)
	require.NoError(t, err)

	// Now modify the duplicate on disk to simulate content changing after hashing.
	require.NoError(t, os.WriteFile(dupPath, []byte("modified content"), 0o600))

	v, err := safepath.New(tmpDir)
	require.NoError(t, err)

	d, err := NewWithValidator(v, false, 1, nil)
	require.NoError(t, err)

	dupFile := collector.FileInfo{
		Path:    dupPath,
		Dir:     tmpDir,
		Name:    "duplicate.txt",
		Size:    int64(len(content)),
		ModTime: modTime,
	}

	op := d.deleteFile(dupFile, originalPath, originalHash)

	require.Error(t, op.Error, "should error when re-hash doesn't match")
	require.ErrorIs(t, op.Error, ErrContentChanged)

	// Duplicate must still exist on disk.
	assert.FileExists(t, dupPath, "duplicate must be preserved when content changed")
}
