package trash

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"file-dedup/internal/testutil"
	"file-dedup/pkg/metadata"
	"file-dedup/pkg/safepath"
)

func setup(t *testing.T) (string, *metadata.Dir, *safepath.Validator) {
	t.Helper()

	root := t.TempDir()
	v, err := safepath.New(root)
	require.NoError(t, err)

	metaDir, err := metadata.Init(root, v)
	require.NoError(t, err)

	return root, metaDir, v
}

func TestNew_CreatesTrashDirectory(t *testing.T) {
	t.Parallel()

	root, metaDir, v := setup(t)

	trasher, err := New(metaDir, "flatten-20260208T143022", v)
	require.NoError(t, err)

	expectedDir := filepath.Join(root, ".file-dedup", "trash", "flatten-20260208T143022")
	assert.Equal(t, expectedDir, trasher.trashRoot)

	info, err := os.Stat(expectedDir)
	require.NoError(t, err, "trash directory should exist")
	assert.True(t, info.IsDir(), "trash directory should be a directory")
}

func TestTrash_MovesFileToTrash(t *testing.T) {
	t.Parallel()

	root, metaDir, v := setup(t)

	filePath := filepath.Join(root, "docs", "report.pdf")
	testutil.CreateFile(t, filePath, "report content")

	trasher, err := New(metaDir, "run1", v)
	require.NoError(t, err)

	err = trasher.Trash(filePath)
	require.NoError(t, err)

	// Original should be gone.
	_, err = os.Stat(filePath)
	assert.True(t, os.IsNotExist(err), "original file should not exist")

	// Should be in trash preserving relative structure.
	trashedPath := filepath.Join(root, ".file-dedup", "trash", "run1", "docs", "report.pdf")
	content, err := os.ReadFile(trashedPath)
	require.NoError(t, err)
	assert.Equal(t, "report content", string(content))
}

func TestTrashPath_PreviewsDestination(t *testing.T) {
	t.Parallel()

	root, metaDir, v := setup(t)

	filePath := filepath.Join(root, "sub", "file.txt")
	testutil.CreateFile(t, filePath, "content")

	trasher, err := New(metaDir, "run1", v)
	require.NoError(t, err)

	dest, err := trasher.TrashPath(filePath)
	require.NoError(t, err)

	expected := filepath.Join(root, ".file-dedup", "trash", "run1", "sub", "file.txt")
	assert.Equal(t, expected, dest)
}

func TestTrashWithDest_MovesFileAndReturnsDestination(t *testing.T) {
	t.Parallel()

	root, metaDir, v := setup(t)
	filePath := filepath.Join(root, "sub", "file.txt")
	testutil.CreateFile(t, filePath, "content")

	trasher, err := New(metaDir, "with-dest", v)
	require.NoError(t, err)

	dest, err := trasher.TrashWithDest(filePath)
	require.NoError(t, err)

	expected := filepath.Join(root, ".file-dedup", "trash", "with-dest", "sub", "file.txt")
	assert.Equal(t, expected, dest)
	assert.NoFileExists(t, filePath)
	assert.FileExists(t, expected)
}

func TestTrash_RejectsOutsideSource(t *testing.T) {
	t.Parallel()

	_, metaDir, v := setup(t)
	outsideDir := t.TempDir()
	outsidePath := filepath.Join(outsideDir, "outside.txt")
	testutil.CreateFile(t, outsidePath, "outside")

	trasher, err := New(metaDir, "outside-source", v)
	require.NoError(t, err)

	err = trasher.Trash(outsidePath)
	require.Error(t, err)
	assert.ErrorIs(t, err, safepath.ErrPathEscape)
	assert.FileExists(t, outsidePath)
}

func TestTrashPath_OutsideSourceStillComputesEscapingDestination(t *testing.T) {
	t.Parallel()

	root, metaDir, v := setup(t)
	outsideDir := t.TempDir()
	outsidePath := filepath.Join(outsideDir, "outside.txt")
	testutil.CreateFile(t, outsidePath, "outside")

	trasher, err := New(metaDir, "outside-path", v)
	require.NoError(t, err)

	dest, err := trasher.TrashPath(outsidePath)
	require.NoError(t, err)
	assert.Contains(t, dest, filepath.Join(root, ".file-dedup", "trash"))
	assert.Contains(t, dest, "outside.txt")
}

func TestRestore_MovesFileBack(t *testing.T) {
	t.Parallel()

	root, metaDir, v := setup(t)

	filePath := filepath.Join(root, "data", "notes.txt")
	testutil.CreateFile(t, filePath, "my notes")

	trasher, err := New(metaDir, "run1", v)
	require.NoError(t, err)

	err = trasher.Trash(filePath)
	require.NoError(t, err)

	trashedPath := filepath.Join(root, ".file-dedup", "trash", "run1", "data", "notes.txt")
	err = trasher.Restore(trashedPath)
	require.NoError(t, err)

	// Original should be back.
	content, err := os.ReadFile(filePath)
	require.NoError(t, err)
	assert.Equal(t, "my notes", string(content))

	// Trashed copy should be gone.
	_, err = os.Stat(trashedPath)
	assert.True(t, os.IsNotExist(err), "trashed file should be removed after restore")
}

func TestRestore_RejectsPathOutsideRunTrash(t *testing.T) {
	t.Parallel()

	root, metaDir, v := setup(t)
	filePath := filepath.Join(root, "file.txt")
	testutil.CreateFile(t, filePath, "content")

	trasher, err := New(metaDir, "restore-run", v)
	require.NoError(t, err)
	require.NoError(t, trasher.Trash(filePath))

	otherTrashPath := filepath.Join(root, ".file-dedup", "trash", "other-run", "file.txt")
	testutil.CreateFile(t, otherTrashPath, "other")

	err = trasher.Restore(otherTrashPath)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotInTrash)
	assert.FileExists(t, otherTrashPath)
}

func TestRestore_RejectsTrashRootItself(t *testing.T) {
	t.Parallel()

	_, metaDir, v := setup(t)
	trasher, err := New(metaDir, "restore-root", v)
	require.NoError(t, err)

	err = trasher.Restore(trasher.trashRoot)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotInTrash)
}

func TestRestore_RejectsParentOfTrashRoot(t *testing.T) {
	t.Parallel()

	root, metaDir, v := setup(t)
	trasher, err := New(metaDir, "restore-parent", v)
	require.NoError(t, err)

	err = trasher.Restore(filepath.Dir(trasher.trashRoot))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotInTrash)
	assert.DirExists(t, filepath.Join(root, ".file-dedup", "trash"))
}

func TestRestore_RejectsEscapingTrashRelativePath(t *testing.T) {
	t.Parallel()

	root, metaDir, v := setup(t)
	trasher, err := New(metaDir, "restore-escape", v)
	require.NoError(t, err)

	escapePath := filepath.Join(trasher.trashRoot, "..", "other", "file.txt")
	testutil.CreateFile(t, escapePath, "other")

	err = trasher.Restore(escapePath)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotInTrash)
	assert.FileExists(t, filepath.Join(root, ".file-dedup", "trash", "other", "file.txt"))
}

func TestTrashAndRestore_RoundTrip(t *testing.T) {
	t.Parallel()

	root, metaDir, v := setup(t)

	files := map[string]string{
		filepath.Join(root, "a.txt"):         "alpha",
		filepath.Join(root, "sub", "b.txt"):  "beta",
		filepath.Join(root, "deep", "c.txt"): "gamma",
	}

	for path, content := range files {
		testutil.CreateFile(t, path, content)
	}

	trasher, err := New(metaDir, "roundtrip", v)
	require.NoError(t, err)

	// Trash all files.
	for path := range files {
		trashErr := trasher.Trash(path)
		require.NoError(t, trashErr)
	}

	// All originals should be gone.
	for path := range files {
		_, statErr := os.Stat(path)
		assert.True(t, os.IsNotExist(statErr), "original should not exist: %s", path)
	}

	// Restore all.
	err = trasher.RestoreAll()
	require.NoError(t, err)

	// All originals should be back with correct content.
	for path, expectedContent := range files {
		content, err := os.ReadFile(path)
		require.NoError(t, err, "restored file should exist: %s", path)
		assert.Equal(t, expectedContent, string(content), "restored content mismatch: %s", path)
	}
}

func TestPurge_PermanentlyDeletesTrash(t *testing.T) {
	t.Parallel()

	root, metaDir, v := setup(t)

	filePath := filepath.Join(root, "file.txt")
	testutil.CreateFile(t, filePath, "content")

	trasher, err := New(metaDir, "purge-run", v)
	require.NoError(t, err)

	err = trasher.Trash(filePath)
	require.NoError(t, err)

	trashDir := filepath.Join(root, ".file-dedup", "trash", "purge-run")
	_, err = os.Stat(trashDir)
	require.NoError(t, err, "trash directory should exist before purge")

	err = trasher.Purge()
	require.NoError(t, err)

	_, err = os.Stat(trashDir)
	assert.True(t, os.IsNotExist(err), "trash directory should not exist after purge")
}

func TestTrash_PreservesDirectoryStructure(t *testing.T) {
	t.Parallel()

	root, metaDir, v := setup(t)

	// Create a nested file.
	deepPath := filepath.Join(root, "a", "b", "c", "deep.txt")
	testutil.CreateFile(t, deepPath, "deep content")

	trasher, err := New(metaDir, "struct-run", v)
	require.NoError(t, err)

	err = trasher.Trash(deepPath)
	require.NoError(t, err)

	// Verify the structure is preserved in trash.
	trashedPath := filepath.Join(root, ".file-dedup", "trash", "struct-run", "a", "b", "c", "deep.txt")
	content, err := os.ReadFile(trashedPath)
	require.NoError(t, err)
	assert.Equal(t, "deep content", string(content))
}

func TestTrash_RootLevelFile(t *testing.T) {
	t.Parallel()

	root, metaDir, v := setup(t)

	filePath := filepath.Join(root, "rootfile.txt")
	testutil.CreateFile(t, filePath, "root content")

	trasher, err := New(metaDir, "root-run", v)
	require.NoError(t, err)

	err = trasher.Trash(filePath)
	require.NoError(t, err)

	trashedPath := filepath.Join(root, ".file-dedup", "trash", "root-run", "rootfile.txt")
	content, err := os.ReadFile(trashedPath)
	require.NoError(t, err)
	assert.Equal(t, "root content", string(content))
}

func TestRestore_RefusesOverwriteExistingFile(t *testing.T) {
	t.Parallel()

	root, metaDir, v := setup(t)

	filePath := filepath.Join(root, "report.txt")
	testutil.CreateFile(t, filePath, "original content")

	trasher, err := New(metaDir, "overwrite-run", v)
	require.NoError(t, err)

	// Trash the file.
	err = trasher.Trash(filePath)
	require.NoError(t, err)

	// Create a new file at the same path while the original is in trash.
	testutil.CreateFile(t, filePath, "new content that must not be lost")

	// Attempt to restore — should fail because target already exists.
	trashedPath := filepath.Join(root, ".file-dedup", "trash", "overwrite-run", "report.txt")
	err = trasher.Restore(trashedPath)
	require.Error(t, err, "restore should refuse to overwrite an existing file")
	require.ErrorIs(t, err, safepath.ErrTargetExists, "error should be ErrTargetExists")

	// Verify the new file was NOT overwritten.
	content, readErr := os.ReadFile(filePath)
	require.NoError(t, readErr)
	assert.Equal(t, "new content that must not be lost", string(content),
		"existing file content must be preserved")

	// Verify the trashed file still exists in trash.
	_, statErr := os.Stat(trashedPath)
	assert.NoError(t, statErr, "trashed file should still exist after failed restore")
}

func TestRestoreAll_SkipsExistingFiles(t *testing.T) {
	t.Parallel()

	root, metaDir, v := setup(t)

	fileA := filepath.Join(root, "a.txt")
	fileB := filepath.Join(root, "b.txt")
	testutil.CreateFile(t, fileA, "alpha")
	testutil.CreateFile(t, fileB, "beta")

	trasher, err := New(metaDir, "restore-all-run", v)
	require.NoError(t, err)

	// Trash both files.
	require.NoError(t, trasher.Trash(fileA))
	require.NoError(t, trasher.Trash(fileB))

	// Create a conflicting file at the path of fileA.
	testutil.CreateFile(t, fileA, "new alpha that must survive")

	// RestoreAll should fail because one file has a conflict.
	err = trasher.RestoreAll()
	require.Error(t, err, "RestoreAll should fail when restore destination exists")

	// The conflicting file must not be overwritten.
	content, readErr := os.ReadFile(fileA)
	require.NoError(t, readErr)
	assert.Equal(t, "new alpha that must survive", string(content))
}
