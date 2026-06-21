package unzipper

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"file-dedup/pkg/collector"
	"file-dedup/pkg/metadata"
	"file-dedup/pkg/safepath"
	"file-dedup/pkg/trash"

	"github.com/haapjari/flate/pkg/flate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnzip(t *testing.T) {
	t.Run("extracts files from valid zip archive", func(t *testing.T) {
		root := t.TempDir()

		srcDir := filepath.Join(root, "src")
		require.NoError(t, os.MkdirAll(srcDir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(srcDir, "hello.txt"), []byte("hello world"), 0644))
		require.NoError(t, os.WriteFile(filepath.Join(srcDir, "data.bin"), []byte("binary data"), 0644))

		archivePath := filepath.Join(root, "test.zip")
		createZipArchive(t, srcDir, archivePath)

		require.NoError(t, os.RemoveAll(srcDir))

		file := collector.FileInfo{
			Dir:  root,
			Name: "test.zip",
			Path: archivePath,
		}

		_, err := unzip(file)
		require.NoError(t, err)

		content, err := os.ReadFile(filepath.Join(root, "hello.txt"))
		require.NoError(t, err)
		assert.Equal(t, "hello world", string(content))

		content, err = os.ReadFile(filepath.Join(root, "data.bin"))
		require.NoError(t, err)
		assert.Equal(t, "binary data", string(content))
	})

	t.Run("extracts nested directory structure", func(t *testing.T) {
		root := t.TempDir()

		srcDir := filepath.Join(root, "src")
		nestedDir := filepath.Join(srcDir, "a", "b", "c")
		require.NoError(t, os.MkdirAll(nestedDir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(nestedDir, "deep.txt"), []byte("deep content"), 0644))
		require.NoError(t, os.WriteFile(filepath.Join(srcDir, "a", "shallow.txt"), []byte("shallow content"), 0644))

		archivePath := filepath.Join(root, "nested.zip")
		createZipArchive(t, srcDir, archivePath)
		require.NoError(t, os.RemoveAll(srcDir))

		file := collector.FileInfo{
			Dir:  root,
			Name: "nested.zip",
			Path: archivePath,
		}

		_, err := unzip(file)
		require.NoError(t, err)

		content, err := os.ReadFile(filepath.Join(root, "a", "b", "c", "deep.txt"))
		require.NoError(t, err)
		assert.Equal(t, "deep content", string(content))

		content, err = os.ReadFile(filepath.Join(root, "a", "shallow.txt"))
		require.NoError(t, err)
		assert.Equal(t, "shallow content", string(content))
	})

	t.Run("rejects path traversal entries", func(t *testing.T) {
		root := t.TempDir()

		archivePath := filepath.Join(root, "evil.zip")
		f, err := os.Create(archivePath)
		require.NoError(t, err)
		zw := zip.NewWriter(f)
		w, err := zw.Create("../escape.txt")
		require.NoError(t, err)
		_, err = w.Write([]byte("escaped"))
		require.NoError(t, err)
		require.NoError(t, zw.Close())
		require.NoError(t, f.Close())

		file := collector.FileInfo{
			Dir:  root,
			Name: "evil.zip",
			Path: archivePath,
		}

		_, err = unzip(file)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "illegal entry path")
		assert.Contains(t, err.Error(), "contains path traversal")
	})

	t.Run("allows filename containing double dots", func(t *testing.T) {
		root := t.TempDir()

		archivePath := filepath.Join(root, "double_dots.zip")
		f, err := os.Create(archivePath)
		require.NoError(t, err)

		zw := zip.NewWriter(f)
		w, err := zw.Create("Tiedostot/Suunnitelma/Isompi kuin -ohjelma..txt")
		require.NoError(t, err)
		_, err = w.Write([]byte("ok"))
		require.NoError(t, err)
		require.NoError(t, zw.Close())
		require.NoError(t, f.Close())

		file := collector.FileInfo{
			Dir:  root,
			Name: "double_dots.zip",
			Path: archivePath,
		}

		_, err = unzip(file)
		require.NoError(t, err)

		extractedPath := filepath.Join(root, "Tiedostot", "Suunnitelma", "Isompi kuin -ohjelma..txt")
		content, err := os.ReadFile(extractedPath)
		require.NoError(t, err)
		assert.Equal(t, "ok", string(content))
	})

	t.Run("allows non utf8 filename bytes", func(t *testing.T) {
		root := t.TempDir()
		nonUTF8Name := "Ensimm" + string([]byte{0x84}) + "inen kirjoitus.docx"

		archivePath := filepath.Join(root, "non_utf8.zip")
		f, err := os.Create(archivePath)
		require.NoError(t, err)

		entryName := "Tiedostot/Blog/" + nonUTF8Name
		zw := zip.NewWriter(f)
		w, err := zw.Create(entryName)
		require.NoError(t, err)
		_, err = w.Write([]byte("doc content"))
		require.NoError(t, err)
		require.NoError(t, zw.Close())
		require.NoError(t, f.Close())

		file := collector.FileInfo{
			Dir:  root,
			Name: "non_utf8.zip",
			Path: archivePath,
		}

		_, err = unzip(file)
		require.NoError(t, err)

		extractedPath := filepath.Join(root, "Tiedostot", "Blog", nonUTF8Name)
		content, err := os.ReadFile(extractedPath)
		require.NoError(t, err)
		assert.Equal(t, "doc content", string(content))
	})

	t.Run("returns error for non-existent archive", func(t *testing.T) {
		root := t.TempDir()

		file := collector.FileInfo{
			Dir:  root,
			Name: "missing.zip",
			Path: filepath.Join(root, "missing.zip"),
		}

		_, err := unzip(file)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to open archive")
	})

	t.Run("returns error for corrupt archive", func(t *testing.T) {
		root := t.TempDir()

		corruptPath := filepath.Join(root, "corrupt.zip")
		require.NoError(t, os.WriteFile(corruptPath, []byte("this is not a zip file"), 0644))

		file := collector.FileInfo{
			Dir:  root,
			Name: "corrupt.zip",
			Path: corruptPath,
		}

		_, err := unzip(file)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to open archive")
	})

	t.Run("empty zip archive extracts successfully", func(t *testing.T) {
		root := t.TempDir()

		archivePath := filepath.Join(root, "empty.zip")
		f, err := os.Create(archivePath)
		require.NoError(t, err)
		zw := zip.NewWriter(f)
		require.NoError(t, zw.Close())
		require.NoError(t, f.Close())

		file := collector.FileInfo{
			Dir:  root,
			Name: "empty.zip",
			Path: archivePath,
		}

		_, err = unzip(file)
		require.NoError(t, err)
	})

	t.Run("overwrites existing files", func(t *testing.T) {
		root := t.TempDir()

		srcDir := filepath.Join(root, "src")
		require.NoError(t, os.MkdirAll(srcDir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(srcDir, "file.txt"), []byte("new content"), 0644))

		archivePath := filepath.Join(root, "overwrite.zip")
		createZipArchive(t, srcDir, archivePath)
		require.NoError(t, os.RemoveAll(srcDir))

		require.NoError(t, os.WriteFile(filepath.Join(root, "file.txt"), []byte("old content"), 0644))

		file := collector.FileInfo{
			Dir:  root,
			Name: "overwrite.zip",
			Path: archivePath,
		}

		_, err := unzip(file)
		require.NoError(t, err)

		content, err := os.ReadFile(filepath.Join(root, "file.txt"))
		require.NoError(t, err)
		assert.Equal(t, "new content", string(content))
	})

	t.Run("does not delete archive after extraction", func(t *testing.T) {
		root := t.TempDir()

		srcDir := filepath.Join(root, "src")
		require.NoError(t, os.MkdirAll(srcDir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(srcDir, "keep.txt"), []byte("keep"), 0644))

		archivePath := filepath.Join(root, "archive.zip")
		createZipArchive(t, srcDir, archivePath)
		require.NoError(t, os.RemoveAll(srcDir))

		file := collector.FileInfo{
			Dir:  root,
			Name: "archive.zip",
			Path: archivePath,
		}

		_, err := unzip(file)
		require.NoError(t, err)

		_, err = os.Stat(archivePath)
		assert.NoError(t, err, "archive should still exist after extraction")
	})
}

func TestGetRootDirectory(t *testing.T) {
	t.Run("empty slice returns empty string", func(t *testing.T) {
		result := getRootDirectory([]collector.FileInfo{})
		assert.Empty(t, result)
	})

	t.Run("nil slice returns empty string", func(t *testing.T) {
		result := getRootDirectory(nil)
		assert.Empty(t, result)
	})

	t.Run("single file returns its directory", func(t *testing.T) {
		files := []collector.FileInfo{
			{Dir: "/home/user/documents"},
		}
		result := getRootDirectory(files)
		assert.Equal(t, "/home/user/documents", result)
	})

	t.Run("files in same directory", func(t *testing.T) {
		files := []collector.FileInfo{
			{Dir: "/home/user/documents"},
			{Dir: "/home/user/documents"},
			{Dir: "/home/user/documents"},
		}
		result := getRootDirectory(files)
		assert.Equal(t, "/home/user/documents", result)
	})

	t.Run("files in nested subdirectories", func(t *testing.T) {
		files := []collector.FileInfo{
			{Dir: "/home/user/documents/a"},
			{Dir: "/home/user/documents/b"},
			{Dir: "/home/user/documents/a/deep"},
		}
		result := getRootDirectory(files)
		assert.Equal(t, "/home/user/documents", result)
	})

	t.Run("files with deeply nested common ancestor", func(t *testing.T) {
		files := []collector.FileInfo{
			{Dir: "/a/b/c/d/e"},
			{Dir: "/a/b/c/x/y"},
		}
		result := getRootDirectory(files)
		assert.Equal(t, "/a/b/c", result)
	})

	t.Run("files sharing only root as common ancestor", func(t *testing.T) {
		files := []collector.FileInfo{
			{Dir: "/foo/bar"},
			{Dir: "/baz/qux"},
		}
		result := getRootDirectory(files)
		assert.Equal(t, "/", result)
	})

	t.Run("parent and child directory", func(t *testing.T) {
		files := []collector.FileInfo{
			{Dir: "/home/user"},
			{Dir: "/home/user/sub/deep"},
		}
		result := getRootDirectory(files)
		assert.Equal(t, "/home/user", result)
	})
}

func TestFilterNewArchivesSkipsProcessedArchivePaths(t *testing.T) {
	root := t.TempDir()
	archiveA := filepath.Join(root, "a.zip")
	archiveB := filepath.Join(root, "nested", "b.zip")
	plain := filepath.Join(root, "plain.txt")
	createZipFile(t, archiveA)
	require.NoError(t, os.MkdirAll(filepath.Dir(archiveB), 0o755))
	createZipFile(t, archiveB)
	require.NoError(t, os.WriteFile(plain, []byte("plain"), 0o644))

	files := []collector.FileInfo{
		{Path: archiveA, Dir: root, Name: "a.zip"},
		{Path: archiveB, Dir: filepath.Dir(archiveB), Name: "b.zip"},
		{Path: plain, Dir: root, Name: "plain.txt"},
	}

	archives := filterNewArchives(files, map[string]bool{archiveA: true})

	require.Len(t, archives, 1)
	assert.Equal(t, archiveB, filepath.Join(archives[0].Dir, archives[0].Name))
}

func TestExtractBatchAccountingDryRun(t *testing.T) {
	root := t.TempDir()
	srcDir := filepath.Join(root, "src")
	require.NoError(t, os.MkdirAll(filepath.Join(srcDir, "dir"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "dir", "file.txt"), []byte("data"), 0o644))
	archivePath := filepath.Join(root, "batch.zip")
	createZipArchiveWithDirectory(t, srcDir, archivePath)
	require.NoError(t, os.RemoveAll(srcDir))

	uz, err := New(root, true)
	require.NoError(t, err)
	processed := make(map[string]bool)
	result := Result{}
	var progressCalls []string
	err = uz.extractBatch([]collector.FileInfo{{Path: archivePath, Dir: root, Name: "batch.zip"}}, processed, func(stage string, processed, total int) {
		progressCalls = append(progressCalls, fmt.Sprintf("%s:%d/%d", stage, processed, total))
	}, &result)

	require.NoError(t, err)
	assert.True(t, processed[archivePath])
	assert.Equal(t, 1, result.ArchivesProcessed)
	assert.Equal(t, 1, result.ExtractedArchives)
	assert.Equal(t, 0, result.DeletedArchives)
	assert.Equal(t, 1, result.ExtractedFiles)
	assert.Equal(t, 1, result.ExtractedDirs)
	assert.Equal(t, 0, result.SkippedCount)
	assert.Equal(t, []string{"extracting:0/1", "extracting:1/1"}, progressCalls)
	require.Len(t, result.Operations, 1)
	assert.True(t, result.Operations[0].ExtractionComplete)
	assert.False(t, result.Operations[0].DeletedArchive)
	assert.FileExists(t, archivePath)
}

func TestRemoveArchiveHardDeleteAndMissingError(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "remove.zip")
	createZipFile(t, archivePath)

	uz, err := New(root, false)
	require.NoError(t, err)

	trashedTo, err := uz.removeArchive(archivePath)
	require.NoError(t, err)
	assert.Empty(t, trashedTo)
	assert.NoFileExists(t, archivePath)

	_, err = uz.removeArchive(archivePath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to remove archive")
}

func TestNewWithValidatorRequiresValidator(t *testing.T) {
	uz, err := NewWithValidator(nil, false, nil)

	assert.Nil(t, uz)
	assert.EqualError(t, err, "validator is required")
}

func TestNewInvalidRootReturnsError(t *testing.T) {
	uz, err := New(filepath.Join(t.TempDir(), "missing"), false)

	assert.Nil(t, uz)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to create path validator")
}

func TestBackupExistingFileBranches(t *testing.T) {
	root := t.TempDir()
	validator, err := safepath.New(root)
	require.NoError(t, err)
	metaDir, err := metadata.Init(root, validator)
	require.NoError(t, err)
	trasher, err := trash.New(metaDir, metaDir.RunID("unzip"), validator)
	require.NoError(t, err)

	targetPath := filepath.Join(root, "target.txt")
	replaced, replacedFile, err := backupExistingFile(targetPath, nil)
	require.NoError(t, err)
	assert.False(t, replaced)
	assert.Zero(t, replacedFile)

	replaced, replacedFile, err = backupExistingFile(targetPath, trasher)
	require.NoError(t, err)
	assert.False(t, replaced)
	assert.Zero(t, replacedFile)

	require.NoError(t, os.Mkdir(targetPath, 0o755))
	replaced, _, err = backupExistingFile(targetPath, trasher)
	require.Error(t, err)
	assert.False(t, replaced)
	assert.Contains(t, err.Error(), "target exists as directory")
	require.NoError(t, os.Remove(targetPath))

	require.NoError(t, os.WriteFile(targetPath, []byte("old"), 0o644))
	replaced, replacedFile, err = backupExistingFile(targetPath, trasher)
	require.NoError(t, err)
	assert.True(t, replaced)
	assert.Equal(t, targetPath, replacedFile.OriginalPath)
	assert.NotEmpty(t, replacedFile.TrashedTo)
	assert.NotEmpty(t, replacedFile.Hash)
	assert.NoFileExists(t, targetPath)
	assert.FileExists(t, replacedFile.TrashedTo)
}

func TestProcessArchiveSetsRemovalFields(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "archive.zip")
	createZipFile(t, archivePath)

	uz, err := New(root, false)
	require.NoError(t, err)
	op, err := uz.processArchive(collector.FileInfo{Dir: root, Name: "archive.zip"}, archivePath)
	require.NoError(t, err)
	assert.Equal(t, archivePath, op.ArchivePath)
	assert.True(t, op.ExtractionComplete)
	assert.Empty(t, op.TrashedTo)
	assert.NoError(t, op.Error)
	assert.NoFileExists(t, archivePath)

	missingOp, err := uz.processArchive(collector.FileInfo{Dir: root, Name: "missing.zip"}, filepath.Join(root, "missing.zip"))
	require.Error(t, err)
	require.Error(t, missingOp.Error)
	assert.Contains(t, missingOp.Error.Error(), "failed to open archive")
}

func TestExtractArchivesWithProgressRecursively(t *testing.T) {
	newProgressTracker := func() (func(string, int, int), *[]struct {
		stage     string
		processed int
		total     int
	}) {
		var calls []struct {
			stage     string
			processed int
			total     int
		}
		fn := func(stage string, processed, total int) {
			calls = append(calls, struct {
				stage     string
				processed int
				total     int
			}{stage, processed, total})
		}
		return fn, &calls
	}

	setup := func(t *testing.T, root string, dryRun bool) (*Unzipper, []collector.FileInfo) {
		t.Helper()
		uz, err := New(root, dryRun)
		require.NoError(t, err)
		files, err := getAllFilesRecursively(root)
		require.NoError(t, err)
		return uz, files
	}

	t.Run("empty file list returns zero result", func(t *testing.T) {
		uz, err := New(t.TempDir(), false)
		require.NoError(t, err)

		result, err := uz.ExtractArchivesWithProgressRecursively([]collector.FileInfo{}, nil)
		require.NoError(t, err)

		assert.Equal(t, 0, result.TotalFiles)
		assert.Equal(t, 0, result.ArchivesFound)
		assert.Equal(t, 0, result.ArchivesProcessed)
		assert.Equal(t, 0, result.ExtractedArchives)
		assert.Equal(t, 0, result.ExtractedFiles)
		assert.Equal(t, 0, result.ExtractedDirs)
		assert.Equal(t, 0, result.ErrorCount)
		assert.Empty(t, result.Operations)
	})

	t.Run("no archives among plain files", func(t *testing.T) {
		root := t.TempDir()
		createTestFiles(t, root, 0)
		subDir := filepath.Join(root, "subdir_0")
		require.NoError(t, os.MkdirAll(subDir, 0755))
		createTestFiles(t, subDir, 1)

		uz, files := setup(t, root, false)
		progress, calls := newProgressTracker()

		result, err := uz.ExtractArchivesWithProgressRecursively(files, progress)
		require.NoError(t, err)

		assert.Equal(t, 0, result.ArchivesFound)
		assert.Equal(t, 0, result.ArchivesProcessed)
		assert.Equal(t, 0, result.ErrorCount)
		assert.Equal(t, 0, result.ExtractedFiles)
		_ = calls
	})

	t.Run("extracts single archive", func(t *testing.T) {
		root := t.TempDir()

		srcDir := filepath.Join(root, "archived_content")
		require.NoError(t, os.MkdirAll(srcDir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(srcDir, "a.txt"), []byte("aaa"), 0644))
		require.NoError(t, os.WriteFile(filepath.Join(srcDir, "b.txt"), []byte("bbb"), 0644))

		archivePath := filepath.Join(root, "test.zip")
		createZipArchive(t, srcDir, archivePath)
		require.NoError(t, os.RemoveAll(srcDir))

		uz, files := setup(t, root, false)
		progress, _ := newProgressTracker()

		result, err := uz.ExtractArchivesWithProgressRecursively(files, progress)
		require.NoError(t, err)

		assert.GreaterOrEqual(t, result.ArchivesFound, 1)
		assert.Equal(t, 0, result.ErrorCount)
		assert.GreaterOrEqual(t, result.ExtractedFiles, 2, "expected at least the 2 files from the archive")

		// verify extracted content exists on disk
		content, err := os.ReadFile(filepath.Join(root, "a.txt"))
		require.NoError(t, err)
		assert.Equal(t, "aaa", string(content))

		content, err = os.ReadFile(filepath.Join(root, "b.txt"))
		require.NoError(t, err)
		assert.Equal(t, "bbb", string(content))
	})

	t.Run("extracts nested archives recursively", func(t *testing.T) {
		root := t.TempDir()

		// create inner archive content
		innerDir := filepath.Join(root, "inner_src")
		require.NoError(t, os.MkdirAll(innerDir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(innerDir, "deep.txt"), []byte("deep content"), 0644))

		innerZipPath := filepath.Join(root, "inner.zip")
		createZipArchive(t, innerDir, innerZipPath)
		require.NoError(t, os.RemoveAll(innerDir))

		// create outer archive containing the inner zip
		outerDir := filepath.Join(root, "outer_src")
		require.NoError(t, os.MkdirAll(outerDir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(outerDir, "outer.txt"), []byte("outer content"), 0644))

		// copy inner zip into outer source dir
		innerData, err := os.ReadFile(innerZipPath)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(outerDir, "inner.zip"), innerData, 0644))

		outerZipPath := filepath.Join(root, "outer.zip")
		createZipArchive(t, outerDir, outerZipPath)
		require.NoError(t, os.RemoveAll(outerDir))
		require.NoError(t, os.Remove(innerZipPath))

		uz, files := setup(t, root, false)

		result, err := uz.ExtractArchivesWithProgressRecursively(files, nil)
		require.NoError(t, err)

		assert.GreaterOrEqual(t, result.ArchivesFound, 1, "expected at least the outer archive")
		assert.Equal(t, 0, result.ErrorCount)
		// the nested archive should have been discovered and extracted too
		assert.GreaterOrEqual(t, result.ExtractedArchives, 1)
	})

	t.Run("leaves zip-readable non-zip entries untouched", func(t *testing.T) {
		root := t.TempDir()

		gpSourcePath := filepath.Join(root, "source.gp3")
		createZipFile(t, gpSourcePath)
		gpData, err := os.ReadFile(gpSourcePath)
		require.NoError(t, err)

		outerDir := filepath.Join(root, "outer_src")
		require.NoError(t, os.MkdirAll(outerDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(outerDir, "song.gp3"), gpData, 0o644))

		outerZipPath := filepath.Join(root, "tabs.zip")
		createZipArchive(t, outerDir, outerZipPath)
		require.NoError(t, os.RemoveAll(outerDir))
		require.NoError(t, os.Remove(gpSourcePath))

		uz, files := setup(t, root, false)

		result, err := uz.ExtractArchivesWithProgressRecursively(files, nil)
		require.NoError(t, err)

		assert.Equal(t, 1, result.ArchivesFound)
		assert.Equal(t, 1, result.ArchivesProcessed)
		assert.Equal(t, 1, result.ExtractedArchives)
		assert.Equal(t, 0, result.ErrorCount)
		_, statErr := os.Stat(filepath.Join(root, "song.gp3"))
		require.NoError(t, statErr, "Guitar Pro file must remain as a normal file")
		_, statErr = os.Stat(filepath.Join(root, "hello.txt"))
		assert.True(t, os.IsNotExist(statErr), "non-.zip entry must not be recursively extracted")
	})

	t.Run("progress callback receives calls", func(t *testing.T) {
		root := t.TempDir()

		srcDir := filepath.Join(root, "content")
		require.NoError(t, os.MkdirAll(srcDir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(srcDir, "file.txt"), []byte("data"), 0644))

		archivePath := filepath.Join(root, "progress_test.zip")
		createZipArchive(t, srcDir, archivePath)
		require.NoError(t, os.RemoveAll(srcDir))

		uz, files := setup(t, root, false)
		progress, calls := newProgressTracker()

		result, err := uz.ExtractArchivesWithProgressRecursively(files, progress)
		require.NoError(t, err)

		require.Equal(t, 2, len(*calls), "single archive batch should emit start and final progress")
		assert.Equal(t, progressStageExtracting, (*calls)[0].stage)
		assert.Equal(t, 0, (*calls)[0].processed)
		assert.Equal(t, 1, (*calls)[0].total)
		assert.Equal(t, progressStageExtracting, (*calls)[1].stage)
		assert.Equal(t, result.ArchivesProcessed, (*calls)[1].processed)
		assert.Equal(t, result.ArchivesProcessed, (*calls)[1].total)
	})

	t.Run("dry run inspects archive without filesystem mutations", func(t *testing.T) {
		root := t.TempDir()
		srcDir := filepath.Join(root, "src")
		require.NoError(t, os.MkdirAll(filepath.Join(srcDir, "dir"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(srcDir, "dir", "file.txt"), []byte("data"), 0o644))

		archivePath := filepath.Join(root, "dry.zip")
		createZipArchiveWithDirectory(t, srcDir, archivePath)
		require.NoError(t, os.RemoveAll(srcDir))

		uz, files := setup(t, root, true)
		result, err := uz.ExtractArchivesWithProgressRecursively(files, nil)
		require.NoError(t, err)

		require.Len(t, result.Operations, 1)
		op := result.Operations[0]
		assert.True(t, op.ExtractionComplete)
		assert.Equal(t, archivePath, op.ArchivePath)
		assert.Equal(t, 1, op.ExtractedFiles)
		assert.Equal(t, 1, op.ExtractedDirs)
		assert.False(t, op.DeletedArchive)
		assert.Equal(t, 1, result.ExtractedArchives)
		assert.Equal(t, 0, result.DeletedArchives)
		assert.Equal(t, 1, result.ExtractedFiles)
		assert.Equal(t, 1, result.ExtractedDirs)

		_, statErr := os.Stat(archivePath)
		require.NoError(t, statErr, "dry-run must keep source archive")
		_, statErr = os.Stat(filepath.Join(root, "dir", "file.txt"))
		assert.True(t, os.IsNotExist(statErr), "dry-run must not extract files")
	})

	t.Run("nil progress callback does not panic", func(t *testing.T) {
		root := t.TempDir()

		srcDir := filepath.Join(root, "src")
		require.NoError(t, os.MkdirAll(srcDir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(srcDir, "x.txt"), []byte("x"), 0644))

		archivePath := filepath.Join(root, "nil_progress.zip")
		createZipArchive(t, srcDir, archivePath)
		require.NoError(t, os.RemoveAll(srcDir))

		uz, files := setup(t, root, false)

		assert.NotPanics(t, func() {
			_, err := uz.ExtractArchivesWithProgressRecursively(files, nil)
			require.NoError(t, err)
		})
	})

	t.Run("corrupt archive is filtered out by isArchive", func(t *testing.T) {
		root := t.TempDir()

		corruptPath := filepath.Join(root, "corrupt.zip")
		require.NoError(t, os.WriteFile(corruptPath, []byte("this is not a zip file at all"), 0644))

		// also add a valid non-archive file
		require.NoError(t, os.WriteFile(filepath.Join(root, "normal.txt"), []byte("hello"), 0644))

		uz, files := setup(t, root, false)

		result, err := uz.ExtractArchivesWithProgressRecursively(files, nil)
		require.NoError(t, err)
		assert.Equal(t, 0, result.ArchivesFound, "corrupt zip should not pass isArchive filter")
	})

	t.Run("archive with deflate64 compression method is extracted", func(t *testing.T) {
		root := t.TempDir()
		archivePath := filepath.Join(root, "deflate64.zip")
		createDeflate64Archive(t, archivePath, "method9.txt", []byte("payload"))

		uz, files := setup(t, root, false)
		result, err := uz.ExtractArchivesWithProgressRecursively(files, nil)
		require.NoError(t, err)

		require.Len(t, result.Operations, 1)
		op := result.Operations[0]
		assert.False(t, op.Skipped)
		assert.True(t, op.ExtractionComplete)
		assert.Equal(t, 0, result.SkippedCount)
		assert.Equal(t, 1, result.ExtractedArchives)
		assert.Equal(t, 1, result.DeletedArchives)
		assert.Equal(t, 1, result.ExtractedFiles)

		content, readErr := os.ReadFile(filepath.Join(root, "method9.txt"))
		require.NoError(t, readErr)
		assert.Equal(t, "payload", string(content))

		_, statErr := os.Stat(archivePath)
		assert.True(t, os.IsNotExist(statErr), "deflate64 archive should be removed after extraction")
	})

	t.Run("archive with unsupported compression method is skipped", func(t *testing.T) {
		root := t.TempDir()

		srcDir := filepath.Join(root, "unsupported_method_src")
		require.NoError(t, os.MkdirAll(srcDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(srcDir, "unsupported.txt"), []byte("payload"), 0o644))

		archivePath := filepath.Join(root, "unsupported_method.zip")
		createZipArchive(t, srcDir, archivePath)
		require.NoError(t, os.RemoveAll(srcDir))
		setAllZipEntryMethods(t, archivePath, 99)

		uz, files := setup(t, root, false)
		result, err := uz.ExtractArchivesWithProgressRecursively(files, nil)
		require.NoError(t, err)

		require.Len(t, result.Operations, 1)
		op := result.Operations[0]

		assert.True(t, op.Skipped)
		assert.Contains(t, op.SkipReason, "unsupported compression method")
		assert.Contains(t, op.SkipReason, "unknown")
		assert.Equal(t, 1, result.SkippedCount)
		assert.Equal(t, 0, result.ExtractedArchives)
		assert.Equal(t, 0, result.DeletedArchives)

		_, statErr := os.Stat(archivePath)
		require.NoError(t, statErr, "unsupported archive must remain on disk")
		_, readErr := os.Stat(filepath.Join(root, "unsupported.txt"))
		require.Error(t, readErr, "entry should not be extracted when archive is skipped")
	})

	t.Run("multiple archives at same level", func(t *testing.T) {
		root := t.TempDir()

		for i := range 3 {
			srcDir := filepath.Join(root, fmt.Sprintf("src_%d", i))
			require.NoError(t, os.MkdirAll(srcDir, 0755))
			require.NoError(t, os.WriteFile(
				filepath.Join(srcDir, fmt.Sprintf("file_%d.txt", i)),
				[]byte(fmt.Sprintf("content_%d", i)),
				0644,
			))
			createZipArchive(t, srcDir, filepath.Join(root, fmt.Sprintf("archive_%d.zip", i)))
			require.NoError(t, os.RemoveAll(srcDir))
		}

		uz, files := setup(t, root, false)

		result, err := uz.ExtractArchivesWithProgressRecursively(files, nil)
		require.NoError(t, err)

		assert.GreaterOrEqual(t, result.ArchivesFound, 3, "expected 3 archives")
		assert.Equal(t, 0, result.ErrorCount)

		// verify all extracted files exist
		for i := range 3 {
			content, err := os.ReadFile(filepath.Join(root, fmt.Sprintf("file_%d.txt", i)))
			require.NoError(t, err)
			assert.Equal(t, fmt.Sprintf("content_%d", i), string(content))
		}
	})

	t.Run("archive in subdirectory", func(t *testing.T) {
		root := t.TempDir()
		subDir := filepath.Join(root, "nested", "dir")
		require.NoError(t, os.MkdirAll(subDir, 0755))

		srcDir := filepath.Join(subDir, "src")
		require.NoError(t, os.MkdirAll(srcDir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(srcDir, "nested_file.txt"), []byte("nested"), 0644))

		createZipArchive(t, srcDir, filepath.Join(subDir, "sub_archive.zip"))
		require.NoError(t, os.RemoveAll(srcDir))

		uz, files := setup(t, root, false)

		result, err := uz.ExtractArchivesWithProgressRecursively(files, nil)
		require.NoError(t, err)

		assert.GreaterOrEqual(t, result.ArchivesFound, 1)
		assert.Equal(t, 0, result.ErrorCount)

		content, err := os.ReadFile(filepath.Join(subDir, "nested_file.txt"))
		require.NoError(t, err)
		assert.Equal(t, "nested", string(content))
	})

	t.Run("uses full test structure with multiple levels", func(t *testing.T) {
		root := createTestFileAndFolderStructure(t, 3)

		uz, files := setup(t, root, false)
		progress, calls := newProgressTracker()

		result, err := uz.ExtractArchivesWithProgressRecursively(files, progress)
		require.NoError(t, err)

		assert.GreaterOrEqual(t, result.ArchivesFound, 1, "expected at least 1 archive from test structure")
		assert.Equal(t, 0, result.ErrorCount, "expected no errors")
		assert.NotEmpty(t, *calls, "expected progress to be called")
	})

	t.Run("result operations list matches archives processed", func(t *testing.T) {
		root := t.TempDir()

		srcDir := filepath.Join(root, "ops_src")
		require.NoError(t, os.MkdirAll(srcDir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(srcDir, "op.txt"), []byte("op"), 0644))

		createZipArchive(t, srcDir, filepath.Join(root, "ops.zip"))
		require.NoError(t, os.RemoveAll(srcDir))

		uz, files := setup(t, root, false)

		result, err := uz.ExtractArchivesWithProgressRecursively(files, nil)
		require.NoError(t, err)

		assert.Equal(t, len(result.Operations), result.ArchivesProcessed,
			"operations count should match archives processed")
	})

	t.Run("empty archive extracts without error", func(t *testing.T) {
		root := t.TempDir()

		archivePath := filepath.Join(root, "empty.zip")
		f, err := os.Create(archivePath)
		require.NoError(t, err)
		zw := zip.NewWriter(f)
		require.NoError(t, zw.Close())
		require.NoError(t, f.Close())

		uz, files := setup(t, root, false)

		result, err := uz.ExtractArchivesWithProgressRecursively(files, nil)
		require.NoError(t, err)

		assert.Equal(t, 0, result.ErrorCount)
	})
}

func TestExtractArchivesWithProgressRecursively_Deflate64(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "deflate64.zip")
	createDeflate64Archive(t, archivePath, "method9.txt", []byte("payload"))

	uz, err := New(root, false)
	require.NoError(t, err)
	files, err := getAllFilesRecursively(root)
	require.NoError(t, err)

	result, err := uz.ExtractArchivesWithProgressRecursively(files, nil)
	require.NoError(t, err)

	require.Len(t, result.Operations, 1)
	op := result.Operations[0]
	assert.False(t, op.Skipped)
	assert.True(t, op.ExtractionComplete)
	assert.Equal(t, 0, result.SkippedCount)
	assert.Equal(t, 1, result.ExtractedArchives)
	assert.Equal(t, 1, result.DeletedArchives)
	assert.Equal(t, 1, result.ExtractedFiles)

	content, readErr := os.ReadFile(filepath.Join(root, "method9.txt"))
	require.NoError(t, readErr)
	assert.Equal(t, "payload", string(content))

	_, statErr := os.Stat(archivePath)
	assert.True(t, os.IsNotExist(statErr), "deflate64 archive should be removed after extraction")
}

func TestExtractArchivesWithProgressRecursively_Deflate64TraversalBlocked(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, "target")
	require.NoError(t, os.MkdirAll(root, 0o755))

	outsideSentinel := filepath.Join(workspace, "outside.txt")
	require.NoError(t, os.WriteFile(outsideSentinel, []byte("original"), 0o644))
	archivePath := filepath.Join(root, "evil.zip")
	createDeflate64Archive(t, archivePath, "../outside.txt", []byte("attack"))

	uz, err := New(root, false)
	require.NoError(t, err)
	files, err := getAllFilesRecursively(root)
	require.NoError(t, err)

	result, err := uz.ExtractArchivesWithProgressRecursively(files, nil)
	require.Error(t, err)
	require.ErrorContains(t, err, "illegal entry path")
	require.ErrorContains(t, err, "contains path traversal")
	assert.Equal(t, 1, result.ErrorCount)

	assertFileContent(t, outsideSentinel, "original")
	_, statErr := os.Stat(archivePath)
	require.NoError(t, statErr, "unsafe archive must remain on disk")
}

func TestExtractArchivesWithProgressRecursively_RejectsSymlinkEntry(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "symlink.zip")
	createSymlinkArchive(t, archivePath, "link.txt", "target.txt")

	uz, err := New(root, false)
	require.NoError(t, err)
	files, err := getAllFilesRecursively(root)
	require.NoError(t, err)

	result, err := uz.ExtractArchivesWithProgressRecursively(files, nil)
	require.Error(t, err)
	require.ErrorContains(t, err, "symlink archive entries are not supported")
	assert.Equal(t, 1, result.ErrorCount)
	require.Len(t, result.Operations, 1)
	assert.Error(t, result.Operations[0].Error)
	assert.False(t, result.Operations[0].ExtractionComplete)

	_, statErr := os.Stat(archivePath)
	require.NoError(t, statErr, "rejected archive must remain on disk")
	_, statErr = os.Lstat(filepath.Join(root, "link.txt"))
	assert.True(t, os.IsNotExist(statErr), "symlink entry must not be created")
}

func TestExtractArchivesWithProgressRecursively_DryRunRejectsSymlinkEntry(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "symlink.zip")
	createSymlinkArchive(t, archivePath, "link.txt", "target.txt")

	uz, err := New(root, true)
	require.NoError(t, err)
	files, err := getAllFilesRecursively(root)
	require.NoError(t, err)

	result, err := uz.ExtractArchivesWithProgressRecursively(files, nil)
	require.Error(t, err)
	require.ErrorContains(t, err, "symlink archive entries are not supported")
	assert.Equal(t, 1, result.ErrorCount)
	require.Len(t, result.Operations, 1)
	op := result.Operations[0]
	assert.Error(t, op.Error)
	assert.False(t, op.ExtractionComplete)
	assert.False(t, op.DeletedArchive)
	assert.False(t, op.Skipped)

	assert.FileExists(t, archivePath)
	_, statErr := os.Lstat(filepath.Join(root, "link.txt"))
	assert.True(t, os.IsNotExist(statErr), "dry-run must not create symlink entry")
}

func TestExtractArchivesWithProgressRecursively_ReplacesExistingFileThroughTrash(t *testing.T) {
	root := t.TempDir()
	existingPath := filepath.Join(root, "same.txt")
	require.NoError(t, os.WriteFile(existingPath, []byte("old"), 0o644))

	srcDir := filepath.Join(root, "src")
	require.NoError(t, os.MkdirAll(srcDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "same.txt"), []byte("new"), 0o644))
	archivePath := filepath.Join(root, "replace.zip")
	createZipArchive(t, srcDir, archivePath)
	require.NoError(t, os.RemoveAll(srcDir))

	validator, err := safepath.New(root)
	require.NoError(t, err)
	metaDir, err := metadata.Init(root, validator)
	require.NoError(t, err)
	trasher, err := trash.New(metaDir, "replace-test", validator)
	require.NoError(t, err)
	uz, err := NewWithValidator(validator, false, trasher)
	require.NoError(t, err)
	files, err := getAllFilesRecursively(root)
	require.NoError(t, err)

	result, err := uz.ExtractArchivesWithProgressRecursively(files, nil)
	require.NoError(t, err)

	require.Len(t, result.Operations, 1)
	op := result.Operations[0]
	require.Len(t, op.ReplacedFiles, 1)
	replaced := op.ReplacedFiles[0]
	assert.Equal(t, existingPath, replaced.OriginalPath)
	assert.NotEmpty(t, replaced.Hash)
	assert.FileExists(t, replaced.TrashedTo)
	assertFileContent(t, replaced.TrashedTo, "old")
	assertFileContent(t, existingPath, "new")
	assert.NotEmpty(t, op.TrashedTo, "source archive should be moved to trash")
	assert.FileExists(t, op.TrashedTo)
}

func TestExtractFileWithLimit_MaxSize(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method uint16
	}{
		{name: "store", method: zip.Store},
		{name: "deflate", method: zip.Deflate},
		{name: "deflate64", method: deflate64Method},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			archivePath := filepath.Join(root, tc.name+".zip")
			createSingleMethodArchive(t, archivePath, "large.txt", tc.method, []byte("12345"))

			r, err := openArchiveReader(archivePath)
			require.NoError(t, err)
			defer func() {
				require.NoError(t, r.Close())
			}()
			require.Len(t, r.files, 1)

			targetPath := filepath.Join(root, "large.txt")
			err = extractFileWithLimit(r.files[0], targetPath, 4)
			require.Error(t, err)
			require.ErrorContains(t, err, "decompressed file exceeds maximum size")

			_, statErr := os.Stat(archivePath)
			require.NoError(t, statErr, "over-limit archive must remain on disk")
		})
	}
}

func TestCopyWithDecompressedSizeLimit(t *testing.T) {
	t.Run("exact max size without extra bytes succeeds", func(t *testing.T) {
		var dst bytes.Buffer

		err := copyWithDecompressedSizeLimit(&dst, bytes.NewBufferString("1234"), 4)

		require.NoError(t, err)
		assert.Equal(t, "1234", dst.String())
	})

	t.Run("short content returns before extra-byte check", func(t *testing.T) {
		var dst bytes.Buffer

		err := copyWithDecompressedSizeLimit(&dst, bytes.NewBufferString("123"), 4)

		require.NoError(t, err)
		assert.Equal(t, "123", dst.String())
	})

	t.Run("read error after limit is reported", func(t *testing.T) {
		var dst bytes.Buffer
		src := &errorAfterLimitReader{data: []byte("1234")}

		err := copyWithDecompressedSizeLimit(&dst, src, 4)

		require.Error(t, err)
		require.ErrorContains(t, err, "failed to verify decompressed size")
	})
}

func TestExtractArchivesWithProgressRecursively_MixedSupportedMethodsDeflate64(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "mixed_supported.zip")
	createMixedSupportedMethodsArchive(t, archivePath)

	uz, err := New(root, false)
	require.NoError(t, err)
	files, err := getAllFilesRecursively(root)
	require.NoError(t, err)

	result, err := uz.ExtractArchivesWithProgressRecursively(files, nil)
	require.NoError(t, err)

	require.Len(t, result.Operations, 1)
	op := result.Operations[0]
	assert.False(t, op.Skipped)
	assert.True(t, op.ExtractionComplete)
	assert.True(t, op.DeletedArchive)
	assert.NoError(t, op.Error)
	assert.Equal(t, 0, result.SkippedCount)
	assert.Equal(t, 1, result.ExtractedArchives)
	assert.Equal(t, 1, result.DeletedArchives)
	assert.Equal(t, 3, result.ExtractedFiles)

	assertFileContent(t, filepath.Join(root, "stored.txt"), "stored payload")
	assertFileContent(t, filepath.Join(root, "deflated.txt"), "deflated payload")
	assertFileContent(t, filepath.Join(root, "deflate64.txt"), "deflate64 payload")

	_, statErr := os.Stat(archivePath)
	assert.True(t, os.IsNotExist(statErr), "mixed supported archive should be removed after extraction")
}

func TestExtractArchivesWithProgressRecursively_MixedUnsupportedCompressionMethod(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "mixed_unsupported.zip")
	createMixedUnsupportedMethodsArchive(t, archivePath)

	uz, err := New(root, false)
	require.NoError(t, err)
	files, err := getAllFilesRecursively(root)
	require.NoError(t, err)

	result, err := uz.ExtractArchivesWithProgressRecursively(files, nil)
	require.NoError(t, err)

	require.Len(t, result.Operations, 1)
	op := result.Operations[0]
	assert.True(t, op.Skipped)
	assert.Contains(t, op.SkipReason, "unsupported compression method")
	assert.Contains(t, op.SkipReason, "unknown")
	assert.NoError(t, op.Error)
	assert.False(t, op.ExtractionComplete)
	assert.False(t, op.DeletedArchive)
	assert.Equal(t, 1, result.SkippedCount)
	assert.Equal(t, 0, result.ExtractedArchives)
	assert.Equal(t, 0, result.DeletedArchives)
	assert.Equal(t, 0, result.ExtractedFiles)

	_, statErr := os.Stat(archivePath)
	require.NoError(t, statErr, "archive with unsupported entry must remain on disk")
	_, readErr := os.Stat(filepath.Join(root, "stored.txt"))
	require.Error(t, readErr, "supported entry should not extract when archive is skipped")
}

func TestExtractArchivesWithProgressRecursively_UnsupportedCompressionMethod(t *testing.T) {
	root := t.TempDir()
	srcDir := filepath.Join(root, "unsupported_method_src")
	require.NoError(t, os.MkdirAll(srcDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "unsupported.txt"), []byte("payload"), 0o644))

	archivePath := filepath.Join(root, "unsupported_method.zip")
	createZipArchive(t, srcDir, archivePath)
	require.NoError(t, os.RemoveAll(srcDir))
	setAllZipEntryMethods(t, archivePath, 99)

	uz, err := New(root, false)
	require.NoError(t, err)
	files, err := getAllFilesRecursively(root)
	require.NoError(t, err)

	result, err := uz.ExtractArchivesWithProgressRecursively(files, nil)
	require.NoError(t, err)

	require.Len(t, result.Operations, 1)
	op := result.Operations[0]
	assert.True(t, op.Skipped)
	assert.Contains(t, op.SkipReason, "unsupported compression method")
	assert.Contains(t, op.SkipReason, "unknown")
	assert.NoError(t, op.Error)
	assert.False(t, op.ExtractionComplete)
	assert.False(t, op.DeletedArchive)
	assert.Equal(t, 1, result.SkippedCount)
	assert.Equal(t, 0, result.ExtractedArchives)
	assert.Equal(t, 0, result.DeletedArchives)

	_, statErr := os.Stat(archivePath)
	require.NoError(t, statErr, "unsupported archive must remain on disk")
	_, readErr := os.Stat(filepath.Join(root, "unsupported.txt"))
	require.Error(t, readErr, "entry should not be extracted when archive is skipped")
}

func TestIsArchive(t *testing.T) {
	root := t.TempDir()

	zipPath := filepath.Join(root, "valid.zip")
	createZipFile(t, zipPath)

	txtPath := filepath.Join(root, "plain.txt")
	require.NoError(t, os.WriteFile(txtPath, []byte("not a zip"), 0644))

	fakeZipPath := filepath.Join(root, "fake.zip")
	require.NoError(t, os.WriteFile(fakeZipPath, []byte("this is not a zip"), 0644))
	zipReadableGPPath := filepath.Join(root, "valid.gp3")
	createZipFile(t, zipReadableGPPath)

	emptyPath := filepath.Join(root, "empty.bin")
	require.NoError(t, os.WriteFile(emptyPath, nil, 0644))

	tests := []struct {
		name string
		path string
		want bool
	}{
		{name: "valid zip archive", path: zipPath, want: true},
		{name: "plain text file", path: txtPath, want: false},
		{name: "fake zip extension", path: fakeZipPath, want: false},
		{name: "zip-readable Guitar Pro file", path: zipReadableGPPath, want: false},
		{name: "empty file", path: emptyPath, want: false},
		{name: "non-existent file", path: filepath.Join(root, "missing.zip"), want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isArchive(tc.path)
			assert.Equal(t, tc.want, got, "isArchive(%s) returned unexpected result", tc.path)
		})
	}
}

func TestValidateArchiveEntryPath(t *testing.T) {
	tests := []struct {
		name      string
		entry     string
		wantError error
	}{
		{
			name:  "valid name with double dots",
			entry: "Tiedostot/Suunnitelma/Isompi kuin -ohjelma..txt",
		},
		{
			name:  "valid non utf8 bytes",
			entry: "Tiedostot/Blog/Ensimm" + string([]byte{0x84}) + "inen kirjoitus.docx",
		},
		{
			name:      "unix path traversal",
			entry:     "../escape.txt",
			wantError: errArchiveEntryPathTraversal,
		},
		{
			name:      "normalized parent traversal",
			entry:     "a/../b.txt",
			wantError: errArchiveEntryPathTraversal,
		},
		{
			name:      "windows path traversal",
			entry:     `..\\escape.txt`,
			wantError: errArchiveEntryPathTraversal,
		},
		{
			name:      "windows inner path traversal",
			entry:     `dir\\..\\escape.txt`,
			wantError: errArchiveEntryPathTraversal,
		},
		{
			name:      "absolute unix path",
			entry:     "/escape.txt",
			wantError: errArchiveEntryPathTraversal,
		},
		{
			name:      "windows drive absolute path",
			entry:     "C:/escape.txt",
			wantError: errArchiveEntryPathTraversal,
		},
		{
			name:  "windows separators without traversal",
			entry: `Tiedostot\\Suunnitelma\\safe.txt`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateArchiveEntryPath(tc.entry)
			if tc.wantError != nil {
				require.Error(t, err)
				assert.ErrorIs(t, err, tc.wantError)
				return
			}

			require.NoError(t, err)
		})
	}
}

func TestValidateArchiveEntryPath_InvalidMalformedNames(t *testing.T) {
	tests := []struct {
		name  string
		entry string
	}{
		{name: "empty name", entry: ""},
		{name: "nul byte", entry: "safe\x00name.txt"},
		{name: "current directory segment", entry: "a/./b.txt"},
		{name: "empty middle segment", entry: "a//b.txt"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateArchiveEntryPath(tc.entry)
			require.Error(t, err)
			assert.ErrorContains(t, err, "contains invalid entry path")
		})
	}
}

func TestValidateArchiveEntryPathExactInvalidSentinels(t *testing.T) {
	for _, entry := range []string{"", "safe\x00name.txt", "a/./b.txt", "a//b.txt"} {
		err := validateArchiveEntryPath(entry)
		require.Error(t, err)
		assert.ErrorIs(t, err, errArchiveEntryInvalidPath)
	}
}

func TestValidateArchiveEntryPathTraversalSentinels(t *testing.T) {
	for _, entry := range []string{
		".",
		"..",
		"../escape.txt",
		"a/../../escape.txt",
		"/escape.txt",
		"C:/escape.txt",
	} {
		err := validateArchiveEntryPath(entry)
		require.Error(t, err, entry)
	}
}

func TestResolveArchiveEntryPathWithValidatorRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	validator, err := safepath.New(root)
	require.NoError(t, err)

	_, err = resolveArchiveEntryPath(filepath.Dir(root), filepath.Base(root)+"-outside/file.txt", validator)
	require.Error(t, err)
	assert.ErrorIs(t, err, errArchiveEntryPathTraversal)
}

func TestCompressionMethodHelpers(t *testing.T) {
	assert.True(t, isCompressionMethodSupported(zip.Store))
	assert.True(t, isCompressionMethodSupported(zip.Deflate))
	assert.True(t, isCompressionMethodSupported(deflate64Method))
	assert.False(t, isCompressionMethodSupported(99))

	assert.Equal(t, "store", compressionMethodName(zip.Store))
	assert.Equal(t, "deflate", compressionMethodName(zip.Deflate))
	assert.Equal(t, "deflate64", compressionMethodName(deflate64Method))
	assert.Equal(t, "unknown", compressionMethodName(99))

	err := validateCompressionMethods([]*zip.File{
		{FileHeader: zip.FileHeader{Name: "dir/"}},
		{FileHeader: zip.FileHeader{Name: "bad.bin", Method: 99}},
	})
	require.ErrorIs(t, err, zip.ErrAlgorithm)
	assert.Contains(t, err.Error(), "bad.bin")
	assert.Contains(t, err.Error(), "unknown")
}

func TestInspectArchiveWithValidatorCountsAndErrors(t *testing.T) {
	t.Run("counts directories and files without extracting", func(t *testing.T) {
		root := t.TempDir()
		archivePath := filepath.Join(root, "inspect.zip")
		f, err := os.Create(archivePath)
		require.NoError(t, err)
		zw := zip.NewWriter(f)
		_, err = zw.Create("dir/")
		require.NoError(t, err)
		w, err := zw.Create("dir/file.txt")
		require.NoError(t, err)
		_, err = w.Write([]byte("payload"))
		require.NoError(t, err)
		require.NoError(t, zw.Close())
		require.NoError(t, f.Close())

		validator, err := safepath.New(root)
		require.NoError(t, err)
		op, err := inspectArchiveWithValidator(collector.FileInfo{Dir: root, Name: "inspect.zip"}, validator)

		require.NoError(t, err)
		assert.True(t, op.ExtractionComplete)
		assert.Equal(t, 1, op.ExtractedDirs)
		assert.Equal(t, 1, op.ExtractedFiles)
		assert.NoDirExists(t, filepath.Join(root, "dir"))
	})

	t.Run("records unsupported method error", func(t *testing.T) {
		root := t.TempDir()
		archivePath := filepath.Join(root, "unsupported.zip")
		createSingleMethodArchive(t, archivePath, "bad.txt", zip.Store, []byte("payload"))
		setAllZipEntryMethods(t, archivePath, 99)

		validator, err := safepath.New(root)
		require.NoError(t, err)
		op, err := inspectArchiveWithValidator(collector.FileInfo{Dir: root, Name: "unsupported.zip"}, validator)

		require.ErrorIs(t, err, zip.ErrAlgorithm)
		require.Error(t, op.Error)
		assert.Equal(t, err, op.Error)
		assert.False(t, op.ExtractionComplete)
	})

	t.Run("records invalid path error", func(t *testing.T) {
		root := t.TempDir()
		archivePath := filepath.Join(root, "badpath.zip")
		f, err := os.Create(archivePath)
		require.NoError(t, err)
		zw := zip.NewWriter(f)
		_, err = zw.Create("../bad.txt")
		require.NoError(t, err)
		require.NoError(t, zw.Close())
		require.NoError(t, f.Close())

		validator, err := safepath.New(root)
		require.NoError(t, err)
		op, err := inspectArchiveWithValidator(collector.FileInfo{Dir: root, Name: "badpath.zip"}, validator)

		require.Error(t, err)
		require.Error(t, op.Error)
		assert.Equal(t, err, op.Error)
		assert.Contains(t, err.Error(), "illegal entry path")
	})
}

func TestFilterOnlyArchives(t *testing.T) {
	root := t.TempDir()

	zipPath := filepath.Join(root, "archive.zip")
	createZipFile(t, zipPath)

	zipPath2 := filepath.Join(root, "another.zip")
	createZipFile(t, zipPath2)

	txtPath := filepath.Join(root, "readme.txt")
	require.NoError(t, os.WriteFile(txtPath, []byte("hello"), 0644))

	imgPath := filepath.Join(root, "photo.png")
	require.NoError(t, os.WriteFile(imgPath, []byte("not really a png"), 0644))

	fakeZipPath := filepath.Join(root, "fake.zip")
	require.NoError(t, os.WriteFile(fakeZipPath, []byte("not a zip"), 0644))

	mkInfo := func(path string) collector.FileInfo {
		info, err := os.Stat(path)
		require.NoError(t, err)
		return collector.FileInfo{
			Path:    path,
			Dir:     filepath.Dir(path),
			Name:    filepath.Base(path),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		}
	}

	t.Run("mixed archives and non-archives", func(t *testing.T) {
		input := []collector.FileInfo{
			mkInfo(zipPath),
			mkInfo(txtPath),
			mkInfo(zipPath2),
			mkInfo(imgPath),
		}

		filtered := filterOnlyArchives(input)
		assert.Len(t, filtered, 2, "expected exactly 2 archives")
		assert.Equal(t, "archive.zip", filtered[0].Name)
		assert.Equal(t, "another.zip", filtered[1].Name)
	})

	t.Run("no archives in input", func(t *testing.T) {
		input := []collector.FileInfo{
			mkInfo(txtPath),
			mkInfo(imgPath),
			mkInfo(fakeZipPath),
		}

		filtered := filterOnlyArchives(input)
		assert.Empty(t, filtered, "expected empty filtered slice")
	})

	t.Run("all archives", func(t *testing.T) {
		input := []collector.FileInfo{
			mkInfo(zipPath),
			mkInfo(zipPath2),
		}

		filtered := filterOnlyArchives(input)
		assert.Len(t, filtered, 2, "expected all entries to be archives")
	})

	t.Run("empty input", func(t *testing.T) {
		filtered := filterOnlyArchives([]collector.FileInfo{})
		assert.Empty(t, filtered, "expected empty filtered slice")
	})

	t.Run("nil input", func(t *testing.T) {
		filtered := filterOnlyArchives(nil)
		assert.Empty(t, filtered, "expected empty filtered slice")
	})

	t.Run("non-existent file in input", func(t *testing.T) {
		input := []collector.FileInfo{
			{
				Path: filepath.Join(root, "missing.zip"),
				Dir:  root,
				Name: "missing.zip",
				Size: 0,
			},
			mkInfo(zipPath),
		}

		filtered := filterOnlyArchives(input)
		assert.Len(t, filtered, 1, "expected only the valid archive")
		assert.Equal(t, "archive.zip", filtered[0].Name)
	})
}

func TestGetAllFilesRecursively(t *testing.T) {
	t.Run("traverse 1 level deep", func(t *testing.T) {
		root := createTestFileAndFolderStructure(t, 1)

		files, err := getAllFilesRecursively(root)
		require.NoError(t, err)
		assert.NotEmpty(t, files, "expected files to be returned")

		for _, f := range files {
			assert.True(t, filepath.IsAbs(f.Path), "expected absolute path, got %s", f.Path)
			rel, err := filepath.Rel(root, f.Path)
			require.NoError(t, err)
			assert.False(t, filepath.IsAbs(rel), "file %s escapes root", f.Path)
			assert.NotEmpty(t, f.Name, "expected non-empty filename")
			assert.Positive(t, f.Size, "expected positive file size for %s", f.Path)
		}

		assert.GreaterOrEqual(t, len(files), 31, "expected at least 30 test files + 1 archive")
		assert.LessOrEqual(t, len(files), 51, "expected at most 50 test files + 1 archive")
	})

	t.Run("traverse 5 level deep", func(t *testing.T) {
		root := createTestFileAndFolderStructure(t, 5)

		files, err := getAllFilesRecursively(root)
		require.NoError(t, err)
		assert.NotEmpty(t, files, "expected files to be returned")

		for _, f := range files {
			assert.True(t, filepath.IsAbs(f.Path), "expected absolute path, got %s", f.Path)
			rel, err := filepath.Rel(root, f.Path)
			require.NoError(t, err)
			assert.False(t, filepath.IsAbs(rel), "file %s escapes root", f.Path)
			assert.NotEmpty(t, f.Name, "expected non-empty filename")
			assert.Positive(t, f.Size, "expected positive file size for %s", f.Path)
		}

		assert.GreaterOrEqual(t, len(files), 5*31, "expected at least 5*(30 files + 1 archive)")
		assert.LessOrEqual(t, len(files), 5*51, "expected at most 5*(50 files + 1 archive)")
	})

	t.Run("traverse 10 level deep", func(t *testing.T) {
		root := createTestFileAndFolderStructure(t, 10)

		files, err := getAllFilesRecursively(root)
		require.NoError(t, err)
		assert.NotEmpty(t, files, "expected files to be returned")

		for _, f := range files {
			assert.True(t, filepath.IsAbs(f.Path), "expected absolute path, got %s", f.Path)
			rel, err := filepath.Rel(root, f.Path)
			require.NoError(t, err)
			assert.False(t, filepath.IsAbs(rel), "file %s escapes root", f.Path)
			assert.NotEmpty(t, f.Name, "expected non-empty filename")
			assert.Positive(t, f.Size, "expected positive file size for %s", f.Path)
		}

		assert.GreaterOrEqual(t, len(files), 10*30+10, "expected at least 10*30 test files + 10 archives")
		assert.LessOrEqual(t, len(files), 10*50+10, "expected at most 10*50 test files + 10 archives")
	})
}

// createTestFiles generates a specified number of test files (30-50) in the given directory.
func createTestFiles(t *testing.T, rootPath string, level int) {
	t.Helper()

	numFiles := 30 + rand.IntN(21) //nolint:gosec // test fixture; cryptographic randomness not needed
	for i := range numFiles {
		fileName := fmt.Sprintf("file_%d.txt", i)
		filePath := filepath.Join(rootPath, fileName)
		content := fmt.Sprintf("content of file %d at level %d", i, level)
		require.NoError(t, os.WriteFile(filePath, []byte(content), 0644))
	}
}

// createTestFileAndFolderStructure builds a nested directory tree of the given depth
// inside a temporary directory. Each level contains 30-50 random test files and a
// subdirectory for the next level (subdir_0/subdir_1/…). After all directories and
// files are created, zip archives are generated bottom-up so that each archive at
// level i includes the contents of its subdirectory (and thus the child archive at
// level i+1). Returns the root temp directory path, or "" if level < 0.
func createTestFileAndFolderStructure(t *testing.T, level int) string {
	t.Helper()

	if level < 0 {
		return ""
	}

	path := t.TempDir()

	currentDir := path
	dirs := make([]string, level)

	// create all the directories and files
	for i := range level {
		subDir := filepath.Join(currentDir, fmt.Sprintf("subdir_%d", i))
		require.NoError(t, os.MkdirAll(subDir, 0755))
		createTestFiles(t, subDir, i+1)
		dirs[i] = subDir
		currentDir = subDir
	}

	// create archive bottom-up so each zip includes a child zip
	for i := level - 1; i >= 0; i-- {
		parent := path
		if i > 0 {
			parent = dirs[i-1]
		}
		zipPath := filepath.Join(parent, fmt.Sprintf("archive_level_%d.zip", i))
		createZipArchive(t, dirs[i], zipPath)
	}

	return path
}

// createZipArchive creates a ZIP archive at zipPath containing all files found
// recursively under sourceDir. File paths inside the archive are stored as
// slash-separated paths relative to sourceDir. Directories themselves are not
// stored as explicit entries. The test is failed immediately if the zip file
// cannot be created, and an assertion error is reported if the directory walk
// encounters any issue.
func createZipArchive(t *testing.T, sourceDir, zipPath string) {
	t.Helper()

	zipFile, err := os.Create(zipPath)
	if err != nil {
		t.Fatalf("create zip file: %v", err)
	}
	defer zipFile.Close()

	zw := zip.NewWriter(zipFile)
	defer zw.Close()

	err = filepath.WalkDir(sourceDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		w, err := zw.Create(filepath.ToSlash(path[len(sourceDir)+1:]))
		if err != nil {
			return err
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()

		_, err = io.Copy(w, f)
		return err
	})

	assert.NoError(t, err, "unable to create zip archive")
}

func createZipArchiveWithDirectory(t *testing.T, sourceDir, zipPath string) {
	t.Helper()

	zipFile, err := os.Create(zipPath)
	require.NoError(t, err)
	defer zipFile.Close()

	zw := zip.NewWriter(zipFile)
	_, err = zw.Create("dir/")
	require.NoError(t, err)

	w, err := zw.Create("dir/file.txt")
	require.NoError(t, err)
	content, err := os.ReadFile(filepath.Join(sourceDir, "dir", "file.txt"))
	require.NoError(t, err)
	_, err = w.Write(content)
	require.NoError(t, err)

	require.NoError(t, zw.Close())
}

func createSymlinkArchive(t *testing.T, archivePath, linkName, target string) {
	t.Helper()

	archiveFile, err := os.Create(archivePath)
	require.NoError(t, err)
	defer archiveFile.Close()

	zw := zip.NewWriter(archiveFile)
	fh := &zip.FileHeader{Name: filepath.ToSlash(linkName)}
	fh.SetMode(os.ModeSymlink | 0o777)
	w, err := zw.CreateHeader(fh)
	require.NoError(t, err)
	_, err = w.Write([]byte(target))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
}

// createZipFile creates a minimal valid zip archive at the given path.
func createZipFile(t *testing.T, path string) {
	t.Helper()

	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()

	zw := zip.NewWriter(f)
	w, err := zw.Create("hello.txt")
	require.NoError(t, err)

	_, err = w.Write([]byte("hello"))
	require.NoError(t, err)

	require.NoError(t, zw.Close())
}

func setAllZipEntryMethods(t *testing.T, archivePath string, method uint16) {
	t.Helper()

	data, err := os.ReadFile(archivePath)
	require.NoError(t, err)

	localSig := []byte("PK\x03\x04")
	centralSig := []byte("PK\x01\x02")

	for offset := 0; ; {
		idx := bytes.Index(data[offset:], localSig)
		if idx < 0 {
			break
		}
		abs := offset + idx
		require.GreaterOrEqual(t, len(data), abs+10)
		binary.LittleEndian.PutUint16(data[abs+8:abs+10], method)
		offset = abs + len(localSig)
	}

	for offset := 0; ; {
		idx := bytes.Index(data[offset:], centralSig)
		if idx < 0 {
			break
		}
		abs := offset + idx
		require.GreaterOrEqual(t, len(data), abs+12)
		binary.LittleEndian.PutUint16(data[abs+10:abs+12], method)
		offset = abs + len(centralSig)
	}

	require.NoError(t, os.WriteFile(archivePath, data, 0o644))
}

func createDeflate64Archive(t *testing.T, archivePath, entryName string, payload []byte) {
	t.Helper()

	archiveFile, err := os.Create(archivePath)
	require.NoError(t, err)
	defer archiveFile.Close()

	zw := zip.NewWriter(archiveFile)

	compressed := deflate64StoredBlock(t, payload)
	fh := &zip.FileHeader{
		Name:               filepath.ToSlash(entryName),
		Method:             deflate64Method,
		CRC32:              crc32.ChecksumIEEE(payload),
		UncompressedSize64: uint64(len(payload)),
		CompressedSize64:   uint64(len(compressed)),
	}

	w, err := zw.CreateRaw(fh)
	require.NoError(t, err)

	_, err = w.Write(compressed)
	require.NoError(t, err)

	require.NoError(t, zw.Close())
}

func createMixedSupportedMethodsArchive(t *testing.T, archivePath string) {
	t.Helper()

	archiveFile, err := os.Create(archivePath)
	require.NoError(t, err)
	defer archiveFile.Close()

	zw := zip.NewWriter(archiveFile)
	writeZipEntry(t, zw, "stored.txt", zip.Store, []byte("stored payload"))
	writeZipEntry(t, zw, "deflated.txt", zip.Deflate, []byte("deflated payload"))
	writeDeflate64ZipEntry(t, zw, "deflate64.txt", []byte("deflate64 payload"))
	require.NoError(t, zw.Close())
}

func createMixedUnsupportedMethodsArchive(t *testing.T, archivePath string) {
	t.Helper()

	archiveFile, err := os.Create(archivePath)
	require.NoError(t, err)
	defer archiveFile.Close()

	zw := zip.NewWriter(archiveFile)
	writeZipEntry(t, zw, "stored.txt", zip.Store, []byte("stored payload"))
	writeRawZipEntry(t, zw, "unsupported.txt", 99, []byte("unsupported payload"), []byte("unsupported payload"))
	require.NoError(t, zw.Close())
}

func createSingleMethodArchive(t *testing.T, archivePath, entryName string, method uint16, payload []byte) {
	t.Helper()

	archiveFile, err := os.Create(archivePath)
	require.NoError(t, err)
	defer archiveFile.Close()

	zw := zip.NewWriter(archiveFile)
	switch method {
	case zip.Store, zip.Deflate:
		writeZipEntry(t, zw, entryName, method, payload)
	case deflate64Method:
		writeDeflate64ZipEntry(t, zw, entryName, payload)
	default:
		t.Fatalf("unsupported fixture method: %d", method)
	}
	require.NoError(t, zw.Close())
}

func writeZipEntry(t *testing.T, zw *zip.Writer, name string, method uint16, payload []byte) {
	t.Helper()

	fh := &zip.FileHeader{Name: filepath.ToSlash(name), Method: method}
	w, err := zw.CreateHeader(fh)
	require.NoError(t, err)
	_, err = w.Write(payload)
	require.NoError(t, err)
}

func writeDeflate64ZipEntry(t *testing.T, zw *zip.Writer, name string, payload []byte) {
	t.Helper()

	compressed := deflate64StoredBlock(t, payload)
	writeRawZipEntry(t, zw, name, deflate64Method, payload, compressed)
}

func writeRawZipEntry(t *testing.T, zw *zip.Writer, name string, method uint16, payload, compressed []byte) {
	t.Helper()

	fh := &zip.FileHeader{
		Name:               filepath.ToSlash(name),
		Method:             method,
		CRC32:              crc32.ChecksumIEEE(payload),
		UncompressedSize64: uint64(len(payload)),
		CompressedSize64:   uint64(len(compressed)),
	}
	w, err := zw.CreateRaw(fh)
	require.NoError(t, err)
	_, err = w.Write(compressed)
	require.NoError(t, err)
}

func assertFileContent(t *testing.T, path, expected string) {
	t.Helper()

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, expected, string(content))
}

type errorAfterLimitReader struct {
	data []byte
	off  int
}

func (r *errorAfterLimitReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, errors.New("forced read error")
	}

	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}

func deflate64StoredBlock(t *testing.T, payload []byte) []byte {
	t.Helper()
	require.LessOrEqual(t, len(payload), 0xffff)

	length := len(payload)
	block := make([]byte, 5+len(payload))
	block[0] = 0x01
	block[1] = byte(length & 0xff)
	block[2] = byte((length >> 8) & 0xff)
	nlen := ^length & 0xffff
	block[3] = byte(nlen & 0xff)
	block[4] = byte((nlen >> 8) & 0xff)
	copy(block[5:], payload)

	r := flate.NewReader64(bytes.NewReader(block))
	decompressed, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, r.Close())
	require.Equal(t, payload, decompressed)

	return block
}
