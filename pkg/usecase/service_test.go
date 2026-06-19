package usecase

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/haapjari/flate/pkg/flate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"file-dedup/internal/testutil"
	"file-dedup/pkg/collector"
	"file-dedup/pkg/deduplicator"
	"file-dedup/pkg/filelock"
	"file-dedup/pkg/flattener"
	"file-dedup/pkg/journal"
	"file-dedup/pkg/manifest"
	"file-dedup/pkg/metadata"
	"file-dedup/pkg/organizer"
	"file-dedup/pkg/renamer"
	"file-dedup/pkg/safepath"
	"file-dedup/pkg/trash"
	"file-dedup/pkg/unzipper"
)

type zipFixtureEntry struct {
	name    string
	content []byte
}

const deflate64Method uint16 = 9

// filterConfirmed returns only Success=true entries from a journal,
// filtering out the intent entries from write-ahead journaling.
func filterConfirmed(entries []journal.Entry) []journal.Entry {
	var confirmed []journal.Entry
	for i := range entries {
		if entries[i].Success {
			confirmed = append(confirmed, entries[i])
		}
	}
	return confirmed
}

func writeZipArchive(t *testing.T, archivePath string, entries []zipFixtureEntry) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(archivePath), 0o755))

	file, err := os.Create(archivePath)
	require.NoError(t, err)

	writer := zip.NewWriter(file)
	for _, entry := range entries {
		entryWriter, err := writer.Create(entry.name)
		require.NoError(t, err)

		_, err = entryWriter.Write(entry.content)
		require.NoError(t, err)
	}

	require.NoError(t, writer.Close())
	require.NoError(t, file.Close())
}

func writeDeflate64ZipArchive(t *testing.T, archivePath, entryName string, payload []byte) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(archivePath), 0o755))

	file, err := os.Create(archivePath)
	require.NoError(t, err)

	writer := zip.NewWriter(file)
	compressed := deflate64StoredBlock(t, payload)
	header := &zip.FileHeader{
		Name:               filepath.ToSlash(entryName),
		Method:             deflate64Method,
		CRC32:              crc32.ChecksumIEEE(payload),
		UncompressedSize64: uint64(len(payload)),
		CompressedSize64:   uint64(len(compressed)),
	}

	entryWriter, err := writer.CreateRaw(header)
	require.NoError(t, err)

	_, err = entryWriter.Write(compressed)
	require.NoError(t, err)

	require.NoError(t, writer.Close())
	require.NoError(t, file.Close())
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

	reader := flate.NewReader64(bytes.NewReader(block))
	decompressed, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	require.Equal(t, payload, decompressed)

	return block
}

func zipBytes(t *testing.T, entries []zipFixtureEntry) []byte {
	t.Helper()

	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, entry := range entries {
		entryWriter, err := writer.Create(entry.name)
		require.NoError(t, err)

		_, err = entryWriter.Write(entry.content)
		require.NoError(t, err)
	}

	require.NoError(t, writer.Close())

	return buffer.Bytes()
}

func TestNew_DefensivelyCopiesOptions(t *testing.T) {
	skipFiles := []string{"one.tmp", "two.tmp"}
	skipDirs := []string{"cache", "logs"}

	s := New(Options{SkipFiles: skipFiles, SkipDirs: skipDirs, NoSnapshot: true})
	skipFiles[0] = "changed"
	skipDirs[0] = "changed"

	assert.Equal(t, []string{"one.tmp", "two.tmp"}, s.skipFiles)
	assert.Equal(t, []string{"cache", "logs"}, s.skipDirs)
	assert.True(t, s.noSnapshot)
}

func TestExecutionMeta(t *testing.T) {
	collectDuration := 3 * time.Second
	want := WorkflowMeta{
		RootDir:         "/tmp/root",
		FileCount:       7,
		CollectDuration: collectDuration,
		SnapshotPath:    "/tmp/root/.file-dedup/manifests/run.json",
		JournalPath:     "/tmp/root/.file-dedup/journal/run.jsonl",
	}

	tests := []struct {
		name string
		meta WorkflowMeta
	}{
		{
			name: "rename",
			meta: RenameExecution{
				RootDir:         want.RootDir,
				FileCount:       want.FileCount,
				CollectDuration: want.CollectDuration,
				SnapshotPath:    want.SnapshotPath,
				JournalPath:     want.JournalPath,
			}.Meta(),
		},
		{
			name: "flatten",
			meta: FlattenExecution{
				RootDir:         want.RootDir,
				FileCount:       want.FileCount,
				CollectDuration: want.CollectDuration,
				SnapshotPath:    want.SnapshotPath,
				JournalPath:     want.JournalPath,
			}.Meta(),
		},
		{
			name: "duplicate",
			meta: DuplicateExecution{
				RootDir:         want.RootDir,
				FileCount:       want.FileCount,
				CollectDuration: want.CollectDuration,
				SnapshotPath:    want.SnapshotPath,
				JournalPath:     want.JournalPath,
			}.Meta(),
		},
		{
			name: "unzip",
			meta: UnzipExecution{
				RootDir:         want.RootDir,
				FileCount:       want.FileCount,
				CollectDuration: want.CollectDuration,
				SnapshotPath:    want.SnapshotPath,
				JournalPath:     want.JournalPath,
			}.Meta(),
		},
		{
			name: "organize",
			meta: OrganizeExecution{
				RootDir:         want.RootDir,
				FileCount:       want.FileCount,
				CollectDuration: want.CollectDuration,
				SnapshotPath:    want.SnapshotPath,
				JournalPath:     want.JournalPath,
			}.Meta(),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, want, tc.meta)
		})
	}
}

func TestServiceSkipLists(t *testing.T) {
	s := New(Options{SkipFiles: []string{"a.tmp"}, SkipDirs: []string{"cache"}})

	files := s.skipFileList()
	dirs := s.skipDirList()
	files[0] = "changed"
	dirs[0] = "changed"

	assert.Equal(t, []string{"a.tmp"}, s.skipFiles)
	assert.Equal(t, []string{"cache"}, s.skipDirs)
	assert.Equal(t, []string{"cache", metadata.DirName}, s.skipDirList())
}

func TestSimpleExecutor_ForwardsProgress(t *testing.T) {
	root := t.TempDir()
	validator, err := safepath.New(root)
	require.NoError(t, err)
	files := []collector.FileInfo{{Name: "a.txt", Dir: root, Path: filepath.Join(root, "a.txt")}}
	var calls []string

	executor := simpleExecutor(
		false,
		func(stage string, processed, total int) {
			calls = append(calls, fmt.Sprintf("%s:%d/%d", stage, processed, total))
		},
		func(v *safepath.Validator, dryRun bool) (string, error) {
			assert.Equal(t, validator, v)
			assert.False(t, dryRun)
			return "worker", nil
		},
		"create worker",
		"testing",
		func(worker string, gotFiles []collector.FileInfo, cb func(processed, total int)) int {
			assert.Equal(t, "worker", worker)
			assert.Equal(t, files, gotFiles)
			cb(1, len(gotFiles))
			return len(gotFiles)
		},
	)

	result, err := executor(root, validator, files)

	require.NoError(t, err)
	assert.Equal(t, 1, result)
	assert.Equal(t, []string{"testing:1/1"}, calls)
}

func TestTrashedWorkerExecutor_ForwardsProgressAndCreatesTrash(t *testing.T) {
	root := t.TempDir()
	validator, err := safepath.New(root)
	require.NoError(t, err)
	files := []collector.FileInfo{{Name: "a.txt", Dir: root, Path: filepath.Join(root, "a.txt")}}
	var calls []string

	executor := trashedWorkerExecutor(
		false,
		2,
		func(stage string, processed, total int) {
			calls = append(calls, fmt.Sprintf("%s:%d/%d", stage, processed, total))
		},
		"testcmd",
		func(v *safepath.Validator, dryRun bool, workers int, trasher *trash.Trasher) (string, error) {
			assert.Equal(t, validator, v)
			assert.False(t, dryRun)
			assert.Equal(t, 2, workers)
			require.NotNil(t, trasher)
			return "worker", nil
		},
		"create worker",
		func(worker string, gotFiles []collector.FileInfo, cb func(string, int, int)) int {
			assert.Equal(t, "worker", worker)
			assert.Equal(t, files, gotFiles)
			cb("stage", 1, len(gotFiles))
			return len(gotFiles)
		},
	)

	result, err := executor(root, validator, files)

	require.NoError(t, err)
	assert.Equal(t, 1, result)
	assert.Equal(t, []string{"stage:1/1"}, calls)
	assert.DirExists(t, filepath.Join(root, metadata.DirName, "trash"))
}

func TestTrashRunHelpers(t *testing.T) {
	root := t.TempDir()
	validator, err := safepath.New(root)
	require.NoError(t, err)
	metaDir, err := metadata.Init(root, validator)
	require.NoError(t, err)

	runA := filepath.Join(metaDir.Root(), "trash", "run-a")
	runB := filepath.Join(metaDir.Root(), "trash", "run-b")
	require.NoError(t, os.MkdirAll(filepath.Join(runA, "nested"), 0o755))
	require.NoError(t, os.MkdirAll(runB, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(runA, "one.txt"), []byte("123"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(runA, "nested", "two.txt"), []byte("45"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(runB, "three.txt"), []byte("6789"), 0o644))

	fileCount, totalSize := walkTrashDir(runA)
	assert.Equal(t, 2, fileCount)
	assert.Equal(t, int64(5), totalSize)

	runs, err := listTrashRuns(metaDir)
	require.NoError(t, err)
	require.Len(t, runs, 2)
	assert.Equal(t, "run-a", runs[0].RunID)
	assert.Equal(t, runA, runs[0].Path)
	assert.Equal(t, 2, runs[0].FileCount)
	assert.Equal(t, int64(5), runs[0].TotalSize)
	assert.Equal(t, "run-b", runs[1].RunID)

	assert.Equal(t, []TrashRunInfo{runs[1]}, filterTrashRuns(runs, PurgeRequest{RunID: "run-b"}))
	assert.Empty(t, filterTrashRuns(runs, PurgeRequest{RunID: "missing"}))
	assert.Equal(t, runs, filterTrashRuns(runs, PurgeRequest{All: true}))
	assert.Empty(t, filterTrashRuns(runs, PurgeRequest{}))
	assert.Empty(t, filterTrashRuns([]TrashRunInfo{{RunID: "equal", Age: time.Hour}}, PurgeRequest{OlderThan: time.Hour}))

	dryRunOp := purgeRun(runs[0], true)
	assert.True(t, dryRunOp.Purged)
	assert.Equal(t, "run-a", dryRunOp.RunID)
	assert.Equal(t, 2, dryRunOp.FileCount)
	assert.Equal(t, int64(5), dryRunOp.TotalSize)
	assert.DirExists(t, runA)

	op := purgeRun(runs[1], false)
	require.NoError(t, op.Error)
	assert.True(t, op.Purged)
	assert.NoDirExists(t, runB)

	blockedPath := filepath.Join(root, "blocked")
	require.NoError(t, os.WriteFile(blockedPath, []byte("file"), 0o644))
	errorOp := purgeRun(TrashRunInfo{RunID: "bad", Path: filepath.Join(blockedPath, "child")}, false)
	require.Error(t, errorOp.Error)
	assert.Contains(t, errorOp.Error.Error(), "remove trash directory")
	assert.False(t, errorOp.Purged)

	missingCount, missingSize := walkTrashDir(filepath.Join(root, "missing"))
	assert.Equal(t, 0, missingCount)
	assert.Equal(t, int64(0), missingSize)
}

func TestService_FileWorkflowsRejectInvalidTarget(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	s := New(Options{})

	tests := []struct {
		name string
		run  func() error
	}{
		{
			name: "rename",
			run: func() error {
				_, err := s.RunRename(RenameRequest{TargetDir: missing, DryRun: true})
				return err
			},
		},
		{
			name: "flatten",
			run: func() error {
				_, err := s.RunFlatten(FlattenRequest{TargetDir: missing, DryRun: true})
				return err
			},
		},
		{
			name: "duplicate",
			run: func() error {
				_, err := s.RunDuplicate(DuplicateRequest{TargetDir: missing, DryRun: true})
				return err
			},
		},
		{
			name: "unzip",
			run: func() error {
				_, err := s.RunUnzip(UnzipRequest{TargetDir: missing, DryRun: true})
				return err
			},
		},
		{
			name: "organize",
			run: func() error {
				_, err := s.RunOrganize(OrganizeRequest{TargetDir: missing, DryRun: true})
				return err
			},
		},
		{
			name: "manifest",
			run: func() error {
				_, err := s.RunManifest(ManifestRequest{TargetDir: missing})
				return err
			},
		},
		{
			name: "undo",
			run: func() error {
				_, err := s.RunUndo(UndoRequest{TargetDir: missing, DryRun: true})
				return err
			},
		},
		{
			name: "purge",
			run: func() error {
				_, err := s.RunPurge(PurgeRequest{TargetDir: missing, DryRun: true})
				return err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "cannot access directory")
		})
	}
}

func TestService_WorkflowsRejectFileTarget(t *testing.T) {
	root := t.TempDir()
	filePath := filepath.Join(root, "file.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("data"), 0o644))

	_, err := New(Options{}).RunRename(RenameRequest{TargetDir: filePath, DryRun: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not a directory")
}

func TestUndoEntryBranchesAndMetadata(t *testing.T) {
	root := t.TempDir()
	validator, err := safepath.New(root)
	require.NoError(t, err)
	target := workflowTarget{rootDir: root, validator: validator}

	failed := undoEntry(target, journal.Entry{Type: "trash", Source: "a.txt", Dest: "trash/a.txt", Success: false}, false)
	assert.Equal(t, "trash", failed.EntryType)
	assert.Equal(t, "a.txt", failed.Source)
	assert.Equal(t, "trash/a.txt", failed.Dest)
	assert.Equal(t, undoActionSkip, failed.Action)
	assert.Equal(t, "original operation was not successful", failed.SkipReason)

	extract := undoEntry(target, journal.Entry{Type: "extract", Source: "archive.zip", Success: true}, false)
	assert.Equal(t, "extract", extract.EntryType)
	assert.Equal(t, "archive.zip", extract.Source)
	assert.Equal(t, undoActionSkip, extract.Action)
	assert.Equal(t, "extract operations cannot be automatically undone", extract.SkipReason)

	unknown := undoEntry(target, journal.Entry{Type: "mystery", Source: "src", Dest: "dst", Success: true}, false)
	assert.Equal(t, "mystery", unknown.EntryType)
	assert.Equal(t, "src", unknown.Source)
	assert.Equal(t, "dst", unknown.Dest)
	assert.Equal(t, undoActionSkip, unknown.Action)
	assert.Equal(t, "unknown entry type \"mystery\"", unknown.SkipReason)
}

func TestUndoMoveDryRunMissingAndHashMismatch(t *testing.T) {
	root := t.TempDir()
	validator, err := safepath.New(root)
	require.NoError(t, err)
	target := workflowTarget{rootDir: root, validator: validator}

	missing := undoMove(target, journal.Entry{Type: "rename", Source: "old.txt", Dest: "new.txt"}, false,
		filepath.Join(root, "new.txt"), filepath.Join(root, "old.txt"), undoActionReverseRename, "missing dest")
	assert.Equal(t, "rename", missing.EntryType)
	assert.Equal(t, "old.txt", missing.Source)
	assert.Equal(t, "new.txt", missing.Dest)
	assert.Equal(t, undoActionSkip, missing.Action)
	assert.Equal(t, "missing dest", missing.SkipReason)

	fromAbs := filepath.Join(root, "from.txt")
	toAbs := filepath.Join(root, "nested", "to.txt")
	require.NoError(t, os.WriteFile(fromAbs, []byte("content"), 0o644))

	mismatch := undoMove(target, journal.Entry{Type: "trash", Source: "nested/to.txt", Dest: "from.txt", Hash: "bad"}, false,
		fromAbs, toAbs, undoActionRestore, "missing trash")
	assert.Equal(t, undoActionSkip, mismatch.Action)
	assert.Equal(t, "content changed since original operation (hash mismatch)", mismatch.SkipReason)
	assert.FileExists(t, fromAbs)

	hash, err := deduplicator.ComputeFileHash(fromAbs)
	require.NoError(t, err)
	dryRunOp := undoMove(target, journal.Entry{Type: "trash", Source: "nested/to.txt", Dest: "from.txt", Hash: hash}, true,
		fromAbs, toAbs, undoActionRestore, "missing trash")
	assert.Equal(t, undoActionRestore, dryRunOp.Action)
	assert.Empty(t, dryRunOp.SkipReason)
	assert.NoError(t, dryRunOp.Error)
	assert.FileExists(t, fromAbs)
	assert.NoFileExists(t, toAbs)

	liveOp := undoMove(target, journal.Entry{Type: "trash", Source: "nested/to.txt", Dest: "from.txt", Hash: hash}, false,
		fromAbs, toAbs, undoActionRestore, "missing trash")
	assert.Equal(t, undoActionRestore, liveOp.Action)
	assert.NoError(t, liveOp.Error)
	assert.NoFileExists(t, fromAbs)
	assert.FileExists(t, toAbs)
}

func TestUndoMoveReportsValidatorErrors(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	validator, err := safepath.New(root)
	require.NoError(t, err)
	target := workflowTarget{rootDir: root, validator: validator}

	fromAbs := filepath.Join(root, "from.txt")
	require.NoError(t, os.WriteFile(fromAbs, []byte("content"), 0o644))

	mkdirErr := undoMove(target, journal.Entry{Type: "rename", Source: "from.txt", Dest: "outside/to.txt"}, false,
		fromAbs, filepath.Join(outside, "nested", "to.txt"), undoActionReverseRename, "missing dest")
	require.Error(t, mkdirErr.Error)
	assert.Contains(t, mkdirErr.Error.Error(), "create parent directory")
	assert.FileExists(t, fromAbs)

	outsideSource := filepath.Join(outside, "from.txt")
	require.NoError(t, os.WriteFile(outsideSource, []byte("content"), 0o644))
	renameErr := undoMove(target, journal.Entry{Type: "rename", Source: "outside/from.txt", Dest: "to.txt"}, false,
		outsideSource, filepath.Join(root, "to.txt"), undoActionReverseRename, "missing dest")
	require.Error(t, renameErr.Error)
	assert.Contains(t, renameErr.Error.Error(), "reverse-rename")
	assert.FileExists(t, outsideSource)
}

func TestUndoReplaceBranches(t *testing.T) {
	root := t.TempDir()
	validator, err := safepath.New(root)
	require.NoError(t, err)
	target := workflowTarget{rootDir: root, validator: validator}

	missing := undoReplace(target, journal.Entry{Type: "replace", Source: "file.txt", Dest: "trash/missing.txt", Success: true}, false)
	assert.Equal(t, "replace", missing.EntryType)
	assert.Equal(t, "file.txt", missing.Source)
	assert.Equal(t, "trash/missing.txt", missing.Dest)
	assert.Equal(t, undoActionSkip, missing.Action)
	assert.Equal(t, "replaced file backup not found: trash/missing.txt", missing.SkipReason)

	backupPath := filepath.Join(root, "trash", "backup.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(backupPath), 0o755))
	require.NoError(t, os.WriteFile(backupPath, []byte("backup"), 0o644))

	mismatch := undoReplace(target, journal.Entry{Type: "replace", Source: "file.txt", Dest: "trash/backup.txt", Hash: "bad", Success: true}, false)
	assert.Equal(t, undoActionSkip, mismatch.Action)
	assert.Equal(t, "content changed since original operation (hash mismatch)", mismatch.SkipReason)

	hash, err := deduplicator.ComputeFileHash(backupPath)
	require.NoError(t, err)
	require.NoError(t, os.Mkdir(filepath.Join(root, "file.txt"), 0o755))
	dirCollision := undoReplace(target, journal.Entry{Type: "replace", Source: "file.txt", Dest: "trash/backup.txt", Hash: hash, Success: true}, false)
	assert.Equal(t, undoActionSkip, dirCollision.Action)
	assert.Equal(t, "cannot restore over existing directory: file.txt", dirCollision.SkipReason)
	assert.FileExists(t, backupPath)
}

func TestUndoReplaceReportsBackupAndRestoreErrors(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	validator, err := safepath.New(root)
	require.NoError(t, err)
	target := workflowTarget{rootDir: root, validator: validator}

	backupPath := filepath.Join(root, "trash", "backup.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(backupPath), 0o755))
	require.NoError(t, os.WriteFile(backupPath, []byte("backup"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "file.txt"), []byte("current"), 0o644))

	backupErr := undoReplace(target, journal.Entry{Type: "replace", Source: "file.txt", Dest: "trash/backup.txt", Success: true}, false)
	require.Error(t, backupErr.Error)
	assert.Contains(t, backupErr.Error.Error(), "undo trasher unavailable")
	assert.FileExists(t, backupPath)

	outsideSource := filepath.Join(outside, "file.txt")
	restoreMkdirErr := undoReplace(target, journal.Entry{Type: "replace", Source: "../" + filepath.Base(outside) + "/file.txt", Dest: "trash/backup.txt", Success: true}, false)
	require.Error(t, restoreMkdirErr.Error)
	assert.Contains(t, restoreMkdirErr.Error.Error(), "create parent directory")
	assert.NoFileExists(t, outsideSource)

	outsideBackup := filepath.Join(outside, "backup.txt")
	require.NoError(t, os.WriteFile(outsideBackup, []byte("backup"), 0o644))
	restoreErr := undoReplace(target, journal.Entry{Type: "replace", Source: "restored.txt", Dest: "../" + filepath.Base(outside) + "/backup.txt", Success: true}, false)
	require.Error(t, restoreErr.Error)
	assert.Contains(t, restoreErr.Error.Error(), "restore")
	assert.FileExists(t, outsideBackup)
}

func TestBackupUndoReplaceDestinationErrorBranches(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	validator, err := safepath.New(root)
	require.NoError(t, err)
	target := workflowTarget{rootDir: root, validator: validator}

	blockedParent := filepath.Join(root, "blocked")
	require.NoError(t, os.WriteFile(blockedParent, []byte("file"), 0o644))
	statSkip, statErr := backupUndoReplaceDestination(target, filepath.Join(blockedParent, "child.txt"), "blocked/child.txt")
	assert.Empty(t, statSkip)
	require.Error(t, statErr)
	assert.Contains(t, statErr.Error(), "check restore destination")

	outsideFile := filepath.Join(outside, "outside.txt")
	require.NoError(t, os.WriteFile(outsideFile, []byte("outside"), 0o644))
	target.undoTrasher, err = initTrasher(root, validator, "undo")
	require.NoError(t, err)
	trashSkip, trashErr := backupUndoReplaceDestination(target, outsideFile, "outside.txt")
	assert.Empty(t, trashSkip)
	require.Error(t, trashErr)
	assert.Contains(t, trashErr.Error(), "backup existing destination before restore")
	assert.FileExists(t, outsideFile)
}

func TestPrepareUndoTargetInitializesUndoTrashForApply(t *testing.T) {
	root := t.TempDir()
	validator, err := safepath.New(root)
	require.NoError(t, err)
	target := workflowTarget{rootDir: root, validator: validator}

	dryTarget, err := prepareUndoTarget(target, true)
	require.NoError(t, err)
	assert.Nil(t, dryTarget.undoTrasher)

	applyTarget, err := prepareUndoTarget(target, false)
	require.NoError(t, err)
	assert.NotNil(t, applyTarget.undoTrasher)
}

func TestFindLatestJournalFiltersRolledBackAndSorts(t *testing.T) {
	root := t.TempDir()
	validator, err := safepath.New(root)
	require.NoError(t, err)
	metaDir, err := metadata.Init(root, validator)
	require.NoError(t, err)

	journalDir := filepath.Join(metaDir.Root(), "journal")
	require.NoError(t, os.MkdirAll(journalDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(journalDir, "b.jsonl"), []byte("{}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(journalDir, "c.rolled-back.jsonl"), []byte("{}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(journalDir, "a.txt"), []byte("ignored"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(journalDir, "z.txt"), []byte("ignored"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(journalDir, "a.jsonl"), []byte("{}\n"), 0o644))

	latest, err := findLatestJournal(metaDir)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(journalDir, "b.jsonl"), latest)
	assert.Equal(t, "b", extractRunID(latest))
}

func TestIsUnsafePathErrorRecognizesBothSentinels(t *testing.T) {
	assert.False(t, isUnsafePathError(nil))
	assert.False(t, isUnsafePathError(errors.New("plain")))
	assert.True(t, isUnsafePathError(fmt.Errorf("wrapped: %w", safepath.ErrPathEscape)))
	assert.True(t, isUnsafePathError(fmt.Errorf("wrapped: %w", safepath.ErrSymlinkEscape)))
}

func TestRunPurgeCountsAndProgress(t *testing.T) {
	root := t.TempDir()
	validator, err := safepath.New(root)
	require.NoError(t, err)
	metaDir, err := metadata.Init(root, validator)
	require.NoError(t, err)

	runPath := filepath.Join(metaDir.Root(), "trash", "run-a")
	require.NoError(t, os.MkdirAll(runPath, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(runPath, "one.txt"), []byte("123"), 0o644))

	var progressCalls []string
	exec, err := New(Options{}).RunPurge(PurgeRequest{
		TargetDir: root,
		All:       true,
		DryRun:    false,
		OnProgress: func(stage string, processed, total int) {
			progressCalls = append(progressCalls, fmt.Sprintf("%s:%d/%d", stage, processed, total))
		},
	})

	require.NoError(t, err)
	assert.Equal(t, root, exec.RootDir)
	assert.False(t, exec.DryRun)
	require.Len(t, exec.Runs, 1)
	require.Len(t, exec.Operations, 1)
	assert.Equal(t, 1, exec.PurgedCount)
	assert.Equal(t, int64(3), exec.PurgedSize)
	assert.Equal(t, 0, exec.ErrorCount)
	assert.Equal(t, []string{"purging:1/1"}, progressCalls)
	assert.NoDirExists(t, runPath)
}

func TestRunPurgeReportsListAndOperationErrors(t *testing.T) {
	t.Run("trash root is a file", func(t *testing.T) {
		root := t.TempDir()
		validator, err := safepath.New(root)
		require.NoError(t, err)
		metaDir, err := metadata.Init(root, validator)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(metaDir.Root(), "trash"), []byte("not a dir"), 0o644))

		_, err = New(Options{}).RunPurge(PurgeRequest{TargetDir: root, All: true})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "read trash directory")
	})

	t.Run("operation error increments execution count", func(t *testing.T) {
		root := t.TempDir()
		validator, err := safepath.New(root)
		require.NoError(t, err)
		metaDir, err := metadata.Init(root, validator)
		require.NoError(t, err)

		blockedPath := filepath.Join(root, "blocked")
		require.NoError(t, os.WriteFile(blockedPath, []byte("file"), 0o644))
		require.NoError(t, os.MkdirAll(filepath.Join(metaDir.Root(), "trash"), 0o755))
		badRun := filepath.Join(metaDir.Root(), "trash", "bad-run")
		require.NoError(t, os.Symlink(filepath.Join(blockedPath, "child"), badRun))

		exec, err := New(Options{}).RunPurge(PurgeRequest{TargetDir: root, All: true})
		require.NoError(t, err)
		require.Len(t, exec.Operations, 0)
		assert.Equal(t, 0, exec.ErrorCount)
	})

	t.Run("remove failure increments execution error count", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("root can remove from read-only directories")
		}

		root := t.TempDir()
		validator, err := safepath.New(root)
		require.NoError(t, err)
		metaDir, err := metadata.Init(root, validator)
		require.NoError(t, err)

		trashRoot := filepath.Join(metaDir.Root(), "trash")
		runPath := filepath.Join(trashRoot, "bad-run")
		testutil.CreateFile(t, filepath.Join(runPath, "file.txt"), "data")
		require.NoError(t, os.Chmod(trashRoot, 0o555))
		t.Cleanup(func() { _ = os.Chmod(trashRoot, 0o755) })

		exec, err := New(Options{}).RunPurge(PurgeRequest{TargetDir: root, All: true})
		require.NoError(t, err)
		require.Len(t, exec.Operations, 1)
		require.Error(t, exec.Operations[0].Error)
		assert.Equal(t, 1, exec.ErrorCount)
		assert.Equal(t, 0, exec.PurgedCount)
	})
}

func TestService_RunRename_DryRun(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "My Document.pdf"), "content", modTime)

	progressCalls := 0
	lastStage := ""
	s := New(Options{})
	execution, err := s.RunRename(RenameRequest{
		TargetDir: tmpDir,
		DryRun:    true,
		OnProgress: func(stage string, _, _ int) {
			progressCalls++
			lastStage = stage
		},
	})
	require.NoError(t, err)

	assert.Equal(t, tmpDir, execution.RootDir)
	assert.Equal(t, 1, execution.FileCount)
	assert.Equal(t, 1, execution.Result.TotalFiles)
	assert.Equal(t, 1, execution.Result.RenamedCount)
	assert.Equal(t, 0, execution.Result.ErrorCount)
	assert.GreaterOrEqual(t, progressCalls, 1)
	assert.NotEmpty(t, lastStage)

	_, err = os.Stat(filepath.Join(tmpDir, "My Document.pdf"))
	require.NoError(t, err, "dry-run must not rename files")

	_, err = os.Stat(filepath.Join(tmpDir, "2018-06-15_my_document.pdf"))
	assert.True(t, os.IsNotExist(err), "dry-run must not create renamed files")
}

func TestService_RunFlatten_DryRun(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "nested", "file.txt"), "content", modTime)

	progressCalls := 0
	lastStage := ""
	s := New(Options{})
	execution, err := s.RunFlatten(FlattenRequest{
		TargetDir: tmpDir,
		DryRun:    true,
		Workers:   3,
		OnProgress: func(stage string, _, _ int) {
			progressCalls++
			lastStage = stage
		},
	})
	require.NoError(t, err)

	assert.Equal(t, tmpDir, execution.RootDir)
	assert.Equal(t, 1, execution.FileCount)
	assert.Equal(t, 1, execution.Result.TotalFiles)
	assert.Equal(t, 1, execution.Result.MovedCount)
	assert.Equal(t, 0, execution.Result.ErrorCount)
	assert.GreaterOrEqual(t, progressCalls, 1)
	assert.NotEmpty(t, lastStage)

	_, err = os.Stat(filepath.Join(tmpDir, "nested", "file.txt"))
	require.NoError(t, err, "dry-run must not move files")

	_, err = os.Stat(filepath.Join(tmpDir, "file.txt"))
	assert.True(t, os.IsNotExist(err), "dry-run must not place files in root")
}

func TestService_RunDuplicate_DryRun(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "a.txt"), "same-content", modTime)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "b.txt"), "same-content", modTime)

	progressCalls := 0
	lastStage := ""
	s := New(Options{})
	execution, err := s.RunDuplicate(DuplicateRequest{
		TargetDir: tmpDir,
		DryRun:    true,
		Workers:   3,
		OnProgress: func(stage string, _, _ int) {
			progressCalls++
			lastStage = stage
		},
	})
	require.NoError(t, err)

	assert.Equal(t, tmpDir, execution.RootDir)
	assert.Equal(t, 2, execution.FileCount)
	assert.Equal(t, 2, execution.Result.TotalFiles)
	assert.Equal(t, 1, execution.Result.DuplicatesFound)
	assert.Equal(t, 1, execution.Result.DeletedCount)
	assert.Equal(t, 0, execution.Result.ErrorCount)
	assert.GreaterOrEqual(t, progressCalls, 1)
	assert.NotEmpty(t, lastStage)

	_, err = os.Stat(filepath.Join(tmpDir, "a.txt"))
	require.NoError(t, err, "dry-run must not delete files")
	_, err = os.Stat(filepath.Join(tmpDir, "b.txt"))
	require.NoError(t, err, "dry-run must not delete files")
}

func TestService_RunManifest_WithSkipFiles(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "keep.txt"), "keep", modTime)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, ".DS_Store"), "skip", modTime)

	outputPath := filepath.Join(tmpDir, "manifest.json")

	progressCalls := 0
	s := New(Options{SkipFiles: []string{".DS_Store"}})
	execution, err := s.RunManifest(ManifestRequest{
		TargetDir:  tmpDir,
		OutputPath: outputPath,
		Workers:    2,
		OnProgress: func(_ string, _, _ int) {
			progressCalls++
		},
	})
	require.NoError(t, err)

	assert.Equal(t, tmpDir, execution.RootDir)
	assert.Equal(t, outputPath, execution.OutputPath)
	require.NotNil(t, execution.Manifest)
	assert.Equal(t, 1, execution.Manifest.FileCount())
	assert.Equal(t, 1, execution.Manifest.UniqueFileCount())
	assert.GreaterOrEqual(t, progressCalls, 1)

	_, err = os.Stat(outputPath)
	require.NoError(t, err, "manifest file must be written")
}

func TestService_RunManifest_RelativeOutputResolvesInsideTarget(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	testutil.CreateFileWithModTime(
		t,
		filepath.Join(tmpDir, "keep.txt"),
		"keep",
		time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC),
	)

	s := New(Options{})
	execution, err := s.RunManifest(ManifestRequest{
		TargetDir:  tmpDir,
		OutputPath: "manifest.json",
		Workers:    1,
	})
	require.NoError(t, err)

	expectedOutputPath := filepath.Join(tmpDir, "manifest.json")
	assert.Equal(t, expectedOutputPath, execution.OutputPath)
	_, err = os.Stat(expectedOutputPath)
	require.NoError(t, err)
}

func TestService_RunManifest_OutputOutsideTargetRejected(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	testutil.CreateFileWithModTime(
		t,
		filepath.Join(tmpDir, "keep.txt"),
		"keep",
		time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC),
	)

	outsideDir := t.TempDir()
	outsideOutputPath := filepath.Join(outsideDir, "manifest.json")

	s := New(Options{})
	_, err := s.RunManifest(ManifestRequest{
		TargetDir:  tmpDir,
		OutputPath: outsideOutputPath,
		Workers:    1,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "manifest output path must stay within target directory")

	_, statErr := os.Stat(outsideOutputPath)
	assert.True(t, os.IsNotExist(statErr))
}

func TestService_RunUnzip_DryRun(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "photos.zip")
	writeZipArchive(t, archivePath, []zipFixtureEntry{
		{name: "nested/photo.jpg", content: []byte("photo-bytes")},
	})

	progressCalls := 0
	lastStage := ""
	s := New(Options{})
	execution, err := s.RunUnzip(UnzipRequest{
		TargetDir: tmpDir,
		DryRun:    true,
		OnProgress: func(stage string, _, _ int) {
			progressCalls++
			lastStage = stage
		},
	})
	require.NoError(t, err)

	assert.Equal(t, tmpDir, execution.RootDir)
	assert.Equal(t, 1, execution.FileCount)
	assert.Equal(t, 1, execution.Result.ArchivesFound)
	assert.Equal(t, 1, execution.Result.ArchivesProcessed)
	assert.Equal(t, 1, execution.Result.ExtractedArchives)
	assert.Equal(t, 0, execution.Result.DeletedArchives)
	assert.Equal(t, 1, execution.Result.ExtractedFiles)
	assert.Equal(t, 0, execution.Result.ExtractedDirs)
	assert.Equal(t, 0, execution.Result.ErrorCount)
	assert.GreaterOrEqual(t, progressCalls, 1)
	assert.NotEmpty(t, lastStage)

	_, err = os.Stat(archivePath)
	require.NoError(t, err, "dry-run must not remove archives")

	_, err = os.Stat(filepath.Join(tmpDir, "nested", "photo.jpg"))
	assert.True(t, os.IsNotExist(err), "dry-run must not extract files")
}

func TestService_RunUnzip_Deflate64(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "deflate64.zip")
	writeDeflate64ZipArchive(t, archivePath, "method9.txt", []byte("payload"))

	s := New(Options{})
	execution, err := s.RunUnzip(UnzipRequest{
		TargetDir: tmpDir,
		DryRun:    false,
	})
	require.NoError(t, err)

	assert.Equal(t, tmpDir, execution.RootDir)
	assert.Equal(t, 1, execution.FileCount)
	assert.Equal(t, 1, execution.Result.ArchivesFound)
	assert.Equal(t, 1, execution.Result.ArchivesProcessed)
	assert.Equal(t, 1, execution.Result.ExtractedArchives)
	assert.Equal(t, 1, execution.Result.DeletedArchives)
	assert.Equal(t, 1, execution.Result.ExtractedFiles)
	assert.Equal(t, 0, execution.Result.ErrorCount)

	content, err := os.ReadFile(filepath.Join(tmpDir, "method9.txt"))
	require.NoError(t, err)
	assert.Equal(t, "payload", string(content))

	_, err = os.Stat(archivePath)
	assert.True(t, os.IsNotExist(err), "deflate64 archive should be removed")
}

func TestService_RunUnzip_Deflate64DryRun(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "deflate64.zip")
	writeDeflate64ZipArchive(t, archivePath, "method9.txt", []byte("payload"))

	s := New(Options{})
	execution, err := s.RunUnzip(UnzipRequest{
		TargetDir: tmpDir,
		DryRun:    true,
	})
	require.NoError(t, err)

	assert.Empty(t, execution.JournalPath, "dry-run must not write a journal")
	assert.Equal(t, 1, execution.Result.ArchivesFound)
	assert.Equal(t, 1, execution.Result.ArchivesProcessed)
	assert.Equal(t, 1, execution.Result.ExtractedArchives)
	assert.Equal(t, 0, execution.Result.DeletedArchives)
	assert.Equal(t, 1, execution.Result.ExtractedFiles)
	assert.Equal(t, 0, execution.Result.ErrorCount)

	_, err = os.Stat(archivePath)
	require.NoError(t, err, "dry-run must not remove archives")
	_, err = os.Stat(filepath.Join(tmpDir, "method9.txt"))
	assert.True(t, os.IsNotExist(err), "dry-run must not extract files")
	_, err = os.Stat(filepath.Join(tmpDir, ".file-dedup", "trash"))
	assert.True(t, os.IsNotExist(err), "dry-run must not create trash entries")
}

func TestService_RunUnzip_RecursiveNestedArchives(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	innerArchive := zipBytes(t, []zipFixtureEntry{
		{name: "inner/final.txt", content: []byte("payload")},
	})

	outerArchivePath := filepath.Join(tmpDir, "outer.zip")
	writeZipArchive(t, outerArchivePath, []zipFixtureEntry{
		{name: "nested/inner.zip", content: innerArchive},
		{name: "outer.txt", content: []byte("outer")},
	})

	s := New(Options{})
	execution, err := s.RunUnzip(UnzipRequest{
		TargetDir: tmpDir,
		DryRun:    false,
	})
	require.NoError(t, err)

	assert.Equal(t, tmpDir, execution.RootDir)
	assert.Equal(t, 1, execution.FileCount)
	assert.Equal(t, 2, execution.Result.ArchivesFound)
	assert.Equal(t, 2, execution.Result.ArchivesProcessed)
	assert.Equal(t, 2, execution.Result.ExtractedArchives)
	assert.Equal(t, 2, execution.Result.DeletedArchives)
	assert.Equal(t, 3, execution.Result.ExtractedFiles)
	assert.Equal(t, 0, execution.Result.ExtractedDirs)
	assert.Equal(t, 0, execution.Result.ErrorCount)

	_, err = os.Stat(filepath.Join(tmpDir, "outer.zip"))
	assert.True(t, os.IsNotExist(err), "outer archive should be removed")

	_, err = os.Stat(filepath.Join(tmpDir, "nested", "inner.zip"))
	assert.True(t, os.IsNotExist(err), "nested archive should be removed")

	_, err = os.Stat(filepath.Join(tmpDir, "outer.txt"))
	require.NoError(t, err)

	_, err = os.Stat(filepath.Join(tmpDir, "nested", "inner", "final.txt"))
	require.NoError(t, err)
}

func TestService_RunUnzip_RecursiveDeflate64NestedArchives(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	innerArchive := zipBytes(t, []zipFixtureEntry{
		{name: "inner/final.txt", content: []byte("payload")},
	})
	outerArchivePath := filepath.Join(tmpDir, "outer.zip")
	writeDeflate64ZipArchive(t, outerArchivePath, "nested/inner.zip", innerArchive)

	s := New(Options{})
	execution, err := s.RunUnzip(UnzipRequest{
		TargetDir: tmpDir,
		DryRun:    false,
	})
	require.NoError(t, err)

	assert.Equal(t, 2, execution.Result.ArchivesFound)
	assert.Equal(t, 2, execution.Result.ArchivesProcessed)
	assert.Equal(t, 2, execution.Result.ExtractedArchives)
	assert.Equal(t, 2, execution.Result.DeletedArchives)
	assert.Equal(t, 2, execution.Result.ExtractedFiles)
	assert.Equal(t, 0, execution.Result.ErrorCount)

	_, err = os.Stat(outerArchivePath)
	assert.True(t, os.IsNotExist(err), "outer archive should be removed")
	_, err = os.Stat(filepath.Join(tmpDir, "nested", "inner.zip"))
	assert.True(t, os.IsNotExist(err), "nested archive should be removed")
	content, err := os.ReadFile(filepath.Join(tmpDir, "nested", "inner", "final.txt"))
	require.NoError(t, err)
	assert.Equal(t, "payload", string(content))
}

func TestService_RunDuplicate_GeneratesSnapshot(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "a.txt"), "same-content", modTime)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "b.txt"), "same-content", modTime)

	s := New(Options{})
	execution, err := s.RunDuplicate(DuplicateRequest{
		TargetDir: tmpDir,
		DryRun:    false,
		Workers:   2,
	})
	require.NoError(t, err)

	assert.NotEmpty(t, execution.SnapshotPath, "snapshot path should be set for non-dry-run")
	_, err = os.Stat(execution.SnapshotPath)
	require.NoError(t, err, "snapshot file should exist")

	// Verify the snapshot is a valid manifest with 2 entries (the original files).
	m, err := manifest.Load(execution.SnapshotPath)
	require.NoError(t, err)
	assert.Equal(t, 2, m.FileCount())
}

func TestService_RunDuplicate_DryRunSkipsSnapshot(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "a.txt"), "same-content", modTime)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "b.txt"), "same-content", modTime)

	s := New(Options{})
	execution, err := s.RunDuplicate(DuplicateRequest{
		TargetDir: tmpDir,
		DryRun:    true,
		Workers:   2,
	})
	require.NoError(t, err)

	assert.Empty(t, execution.SnapshotPath, "snapshot path should be empty for dry-run")

	// Verify .file-dedup/manifests/ does not exist.
	_, err = os.Stat(filepath.Join(tmpDir, ".file-dedup", "manifests"))
	assert.True(t, os.IsNotExist(err), "no manifests directory should be created in dry-run")
}

func TestService_RunFlatten_NoSnapshotOption(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "nested", "file.txt"), "content", modTime)

	s := New(Options{NoSnapshot: true})
	execution, err := s.RunFlatten(FlattenRequest{
		TargetDir: tmpDir,
		DryRun:    false,
		Workers:   2,
	})
	require.NoError(t, err)

	assert.Empty(t, execution.SnapshotPath, "snapshot path should be empty when NoSnapshot is true")

	// Verify .file-dedup/manifests/ does not exist.
	_, err = os.Stat(filepath.Join(tmpDir, ".file-dedup", "manifests"))
	assert.True(t, os.IsNotExist(err), "no manifests directory should be created when NoSnapshot is true")
}

func TestService_RunRename_GeneratesSnapshot(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "My Document.pdf"), "content", modTime)

	s := New(Options{})
	execution, err := s.RunRename(RenameRequest{
		TargetDir: tmpDir,
		DryRun:    false,
	})
	require.NoError(t, err)

	assert.NotEmpty(t, execution.SnapshotPath, "snapshot path should be set")
	_, err = os.Stat(execution.SnapshotPath)
	require.NoError(t, err, "snapshot file should exist")

	m, err := manifest.Load(execution.SnapshotPath)
	require.NoError(t, err)
	assert.Equal(t, 1, m.FileCount())
}

func TestService_RunDuplicate_LockReleasedAfterWorkflow(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "a.txt"), "same-content", modTime)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "b.txt"), "same-content", modTime)

	s := New(Options{NoSnapshot: true})

	// First run should succeed.
	_, err := s.RunDuplicate(DuplicateRequest{
		TargetDir: tmpDir,
		DryRun:    true,
		Workers:   2,
	})
	require.NoError(t, err)

	// Lock should be released — second run on same directory should succeed.
	_, err = s.RunDuplicate(DuplicateRequest{
		TargetDir: tmpDir,
		DryRun:    true,
		Workers:   2,
	})
	require.NoError(t, err, "second run should succeed after lock is released")

	// Lock file should be cleaned up.
	_, err = os.Stat(filepath.Join(tmpDir, ".file-dedup", "lock"))
	assert.True(t, os.IsNotExist(err), "lock file should be removed after workflow")
}

func TestService_RunRename_LockPreventsConflict(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "file.txt"), "content", modTime)

	// Manually acquire the lock to simulate a concurrent file-dedup process.
	metaDir := filepath.Join(tmpDir, ".file-dedup")
	require.NoError(t, os.MkdirAll(metaDir, 0o755))
	lockPath := filepath.Join(metaDir, "lock")

	lock, err := filelock.Acquire(lockPath)
	require.NoError(t, err, "manual lock acquisition should succeed")

	t.Cleanup(func() {
		_ = lock.Close()
	})

	s := New(Options{NoSnapshot: true})
	_, err = s.RunRename(RenameRequest{
		TargetDir: tmpDir,
		DryRun:    true,
	})
	require.Error(t, err, "should fail when lock is held")
	assert.Contains(t, err.Error(), "another file-dedup process")
}

func TestService_RunPurge_LockPreventsConflict(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	metaDir := filepath.Join(tmpDir, metadata.DirName)
	require.NoError(t, os.MkdirAll(metaDir, 0o755))
	lockPath := filepath.Join(metaDir, "lock")

	lock, err := filelock.Acquire(lockPath)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = lock.Close()
	})

	_, err = New(Options{NoSnapshot: true}).RunPurge(PurgeRequest{TargetDir: tmpDir, All: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "another file-dedup process")
}

func TestService_RunDuplicate_WritesJournal(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "a.txt"), "same-content", modTime)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "b.txt"), "same-content", modTime)

	s := New(Options{NoSnapshot: true})
	execution, err := s.RunDuplicate(DuplicateRequest{
		TargetDir: tmpDir,
		DryRun:    false,
		Workers:   2,
	})
	require.NoError(t, err)

	assert.NotEmpty(t, execution.JournalPath, "journal path should be set")
	_, err = os.Stat(execution.JournalPath)
	require.NoError(t, err, "journal file should exist")

	reader := journal.NewReader(execution.JournalPath)
	entries, err := reader.Entries()
	require.NoError(t, err)

	// Filter for confirmed entries (write-ahead journal has intent+confirmation pairs).
	confirmed := filterConfirmed(entries)

	require.Len(t, confirmed, 1, "should have one trash entry for the duplicate")
	assert.Equal(t, "trash", confirmed[0].Type)
	assert.True(t, confirmed[0].Success)
	assert.NotEmpty(t, confirmed[0].Hash, "trash entry should include content hash")
	assert.NotEmpty(t, confirmed[0].Source, "source path should not be empty")
	assert.NotEmpty(t, confirmed[0].Dest, "dest (trash path) should not be empty")

	// Verify paths are relative (not absolute).
	assert.False(t, filepath.IsAbs(confirmed[0].Source), "source should be a relative path")
	assert.False(t, filepath.IsAbs(confirmed[0].Dest), "dest should be a relative path")
}

func TestService_RunFlatten_WritesJournal(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "sub", "file.txt"), "content", modTime)

	s := New(Options{NoSnapshot: true})
	execution, err := s.RunFlatten(FlattenRequest{
		TargetDir: tmpDir,
		DryRun:    false,
		Workers:   2,
	})
	require.NoError(t, err)

	assert.NotEmpty(t, execution.JournalPath, "journal path should be set")

	reader := journal.NewReader(execution.JournalPath)
	entries, err := reader.Entries()
	require.NoError(t, err)

	confirmed := filterConfirmed(entries)

	require.Len(t, confirmed, 1, "should have one rename entry for the moved file")
	assert.Equal(t, "rename", confirmed[0].Type)
	assert.True(t, confirmed[0].Success)
	assert.Equal(t, filepath.Join("sub", "file.txt"), confirmed[0].Source)
	assert.Equal(t, "file.txt", confirmed[0].Dest)
}

func TestService_RunRename_DryRunSkipsJournal(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "My Document.pdf"), "content", modTime)

	s := New(Options{NoSnapshot: true})
	execution, err := s.RunRename(RenameRequest{
		TargetDir: tmpDir,
		DryRun:    true,
	})
	require.NoError(t, err)

	assert.Empty(t, execution.JournalPath, "journal path should be empty for dry-run")

	// Verify no journal directory was created.
	_, err = os.Stat(filepath.Join(tmpDir, ".file-dedup", "journal"))
	assert.True(t, os.IsNotExist(err), "no journal directory should be created in dry-run")
}

func TestService_RunRename_WritesJournal(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "My Document.pdf"), "content", modTime)

	s := New(Options{NoSnapshot: true})
	execution, err := s.RunRename(RenameRequest{
		TargetDir: tmpDir,
		DryRun:    false,
	})
	require.NoError(t, err)

	assert.NotEmpty(t, execution.JournalPath, "journal path should be set")

	reader := journal.NewReader(execution.JournalPath)
	entries, err := reader.Entries()
	require.NoError(t, err)

	confirmed := filterConfirmed(entries)

	require.Len(t, confirmed, 1, "should have one rename entry")
	assert.Equal(t, "rename", confirmed[0].Type)
	assert.True(t, confirmed[0].Success)
	assert.Equal(t, "My Document.pdf", confirmed[0].Source)
	assert.Equal(t, "2018-06-15_my_document.pdf", confirmed[0].Dest)
}

func TestService_RunOrganize_WritesJournal(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "photo.jpg"), "image-data", modTime)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "notes.txt"), "text-data", modTime)

	s := New(Options{NoSnapshot: true})
	execution, err := s.RunOrganize(OrganizeRequest{
		TargetDir: tmpDir,
		DryRun:    false,
	})
	require.NoError(t, err)

	assert.NotEmpty(t, execution.JournalPath, "journal path should be set")

	reader := journal.NewReader(execution.JournalPath)
	entries, err := reader.Entries()
	require.NoError(t, err)

	confirmed := filterConfirmed(entries)

	require.Len(t, confirmed, 2, "should have two rename entries")

	// Collect entries by source for stable assertions.
	entryBySource := make(map[string]journal.Entry, len(confirmed))
	for _, e := range confirmed {
		entryBySource[e.Source] = e
	}

	jpgEntry, ok := entryBySource["photo.jpg"]
	require.True(t, ok, "should have entry for photo.jpg")
	assert.Equal(t, "rename", jpgEntry.Type)
	assert.True(t, jpgEntry.Success)
	assert.Equal(t, filepath.Join("jpg", "photo.jpg"), jpgEntry.Dest)

	txtEntry, ok := entryBySource["notes.txt"]
	require.True(t, ok, "should have entry for notes.txt")
	assert.Equal(t, "rename", txtEntry.Type)
	assert.True(t, txtEntry.Success)
	assert.Equal(t, filepath.Join("txt", "notes.txt"), txtEntry.Dest)
}

func TestService_RunUnzip_WritesJournal(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "docs.zip")
	writeZipArchive(t, archivePath, []zipFixtureEntry{
		{name: "readme.txt", content: []byte("hello")},
	})

	s := New(Options{NoSnapshot: true})
	execution, err := s.RunUnzip(UnzipRequest{
		TargetDir: tmpDir,
		DryRun:    false,
	})
	require.NoError(t, err)

	assert.NotEmpty(t, execution.JournalPath, "journal path should be set")

	reader := journal.NewReader(execution.JournalPath)
	entries, err := reader.Entries()
	require.NoError(t, err)

	confirmed := filterConfirmed(entries)

	// Should have an extract entry and a trash entry (deleted archive).
	require.Len(t, confirmed, 2, "should have extract + trash entries")

	// Collect entries by type for stable assertions.
	entryByType := make(map[string]journal.Entry, len(confirmed))
	for _, e := range confirmed {
		entryByType[e.Type] = e
	}

	extractEntry, ok := entryByType["extract"]
	require.True(t, ok, "should have extract entry")
	assert.True(t, extractEntry.Success)
	assert.Equal(t, "docs.zip", extractEntry.Source)

	trashEntry, ok := entryByType["trash"]
	require.True(t, ok, "should have trash entry for deleted archive")
	assert.True(t, trashEntry.Success)
	assert.Equal(t, "docs.zip", trashEntry.Source)
	assert.NotEmpty(t, trashEntry.Dest)
}

func TestService_RunUnzip_Deflate64OverwriteTrashAndJournal(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	existingPath := filepath.Join(tmpDir, "method9.txt")
	testutil.CreateFileWithModTime(t, existingPath, "old", time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	archivePath := filepath.Join(tmpDir, "deflate64.zip")
	writeDeflate64ZipArchive(t, archivePath, "method9.txt", []byte("new"))

	s := New(Options{NoSnapshot: true})
	execution, err := s.RunUnzip(UnzipRequest{
		TargetDir: tmpDir,
		DryRun:    false,
	})
	require.NoError(t, err)

	content, err := os.ReadFile(existingPath)
	require.NoError(t, err)
	assert.Equal(t, "new", string(content))
	assert.NotEmpty(t, execution.JournalPath, "journal path should be set")

	reader := journal.NewReader(execution.JournalPath)
	entries, err := reader.Entries()
	require.NoError(t, err)
	confirmed := filterConfirmed(entries)
	require.Len(t, confirmed, 3, "should have replace + extract + trash entries")

	entryByType := make(map[string]journal.Entry, len(confirmed))
	for _, e := range confirmed {
		entryByType[e.Type] = e
	}

	replaceEntry, ok := entryByType["replace"]
	require.True(t, ok, "should have replace entry for overwritten file")
	assert.Equal(t, "method9.txt", replaceEntry.Source)
	assert.NotEmpty(t, replaceEntry.Hash)
	assert.NotEmpty(t, replaceEntry.Dest)
	oldContent, err := os.ReadFile(filepath.Join(tmpDir, replaceEntry.Dest))
	require.NoError(t, err)
	assert.Equal(t, "old", string(oldContent))

	extractEntry, ok := entryByType["extract"]
	require.True(t, ok, "should have extract entry")
	assert.Equal(t, "deflate64.zip", extractEntry.Source)

	trashEntry, ok := entryByType["trash"]
	require.True(t, ok, "should have trash entry for source archive")
	assert.Equal(t, "deflate64.zip", trashEntry.Source)
	assert.NotEmpty(t, trashEntry.Dest)
	_, err = os.Stat(filepath.Join(tmpDir, trashEntry.Dest))
	require.NoError(t, err, "source archive should be in trash")
}

func TestService_RunRename_EmptyDirectoryShortCircuits(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	s := New(Options{})
	execution, err := s.RunRename(RenameRequest{
		TargetDir: tmpDir,
		DryRun:    false,
	})
	require.NoError(t, err)

	assert.Equal(t, tmpDir, execution.RootDir)
	assert.Equal(t, 0, execution.FileCount)
	assert.Empty(t, execution.SnapshotPath)
	assert.Empty(t, execution.JournalPath)
	assert.Empty(t, execution.Result.Operations)

	_, err = os.Stat(filepath.Join(tmpDir, ".file-dedup", "manifests"))
	assert.True(t, os.IsNotExist(err), "empty workflow must not write a snapshot")
	_, err = os.Stat(filepath.Join(tmpDir, ".file-dedup", "journal"))
	assert.True(t, os.IsNotExist(err), "empty workflow must not write a journal")
}

func TestFailOnUnsafeOperation_RejectsPathContainmentErrors(t *testing.T) {
	t.Parallel()

	unsafeErr := fmt.Errorf("wrapped: %w", safepath.ErrSymlinkEscape)
	err := failOnUnsafeOperation([]string{"outside"}, "rename", func(path string) (string, error) {
		return path, unsafeErr
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsafe path detected in rename command")
	assert.True(t, errors.Is(err, safepath.ErrSymlinkEscape))

	nonSafetyErr := errors.New("ordinary operation failure")
	err = failOnUnsafeOperation([]string{"failed"}, "rename", func(path string) (string, error) {
		return path, nonSafetyErr
	})
	require.NoError(t, err)

	err = failOnUnsafeOperation([]string{"ok"}, "rename", func(path string) (string, error) {
		return path, nil
	})
	require.NoError(t, err)
}

func TestWriteJournal_EmptyEntriesDoesNotCreateJournal(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	validator, err := safepath.New(tmpDir)
	require.NoError(t, err)

	journalPath, err := writeJournal(workflowTarget{rootDir: tmpDir, validator: validator}, "rename", nil)
	require.NoError(t, err)
	assert.Empty(t, journalPath)

	_, err = os.Stat(filepath.Join(tmpDir, ".file-dedup", "journal"))
	assert.True(t, os.IsNotExist(err), "empty journal entries must not create journal metadata")
}

func TestFilterConfirmedEntriesPreservesSuccessfulEntries(t *testing.T) {
	t.Parallel()

	entries := []journal.Entry{
		{Type: "rename", Source: "a.txt", Dest: "b.txt", Success: false},
		{Type: "trash", Source: "old.txt", Dest: "trash/old.txt", Hash: "abc", Success: true},
		{Type: "extract", Source: "archive.zip", Success: false},
		{Type: "replace", Source: "same.txt", Dest: "trash/same.txt", Hash: "def", Success: true},
	}

	confirmed := filterConfirmedEntries(entries)

	require.Len(t, confirmed, 2)
	assert.Equal(t, journal.Entry{Type: "trash", Source: "old.txt", Dest: "trash/old.txt", Hash: "abc", Success: true}, confirmed[0])
	assert.Equal(t, journal.Entry{Type: "replace", Source: "same.txt", Dest: "trash/same.txt", Hash: "def", Success: true}, confirmed[1])
	assert.Empty(t, filterConfirmedEntries([]journal.Entry{{Type: "rename", Success: false}}))
}

func TestRunFileWorkflow_NilJournalBuilderDoesNotWriteJournal(t *testing.T) {
	tmpDir := t.TempDir()
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "file.txt"), "data", time.Now())
	s := New(Options{NoSnapshot: true})

	result, err := runFileWorkflow(s, tmpDir, "test", false,
		func(_ string, _ *safepath.Validator, files []collector.FileInfo) (int, error) {
			return len(files), nil
		},
		nil,
	)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Result)
	assert.Empty(t, result.JournalPath)

	_, err = os.Stat(filepath.Join(tmpDir, metadata.DirName, "journal"))
	assert.True(t, os.IsNotExist(err), "nil journal builder must not create a journal")
}

func TestRunUndo_EmptyJournalDoesNotMarkRolledBack(t *testing.T) {
	tmpDir := t.TempDir()
	validator, err := safepath.New(tmpDir)
	require.NoError(t, err)
	metaDir, err := metadata.Init(tmpDir, validator)
	require.NoError(t, err)

	journalPath := metaDir.JournalPath("empty")
	require.NoError(t, os.MkdirAll(filepath.Dir(journalPath), 0o755))
	require.NoError(t, os.WriteFile(journalPath, nil, 0o644))

	exec, err := New(Options{}).RunUndo(UndoRequest{TargetDir: tmpDir, RunID: "empty", DryRun: false})
	require.NoError(t, err)
	assert.Empty(t, exec.Operations)
	assert.FileExists(t, journalPath)
	assert.NoFileExists(t, strings.TrimSuffix(journalPath, ".jsonl")+".rolled-back.jsonl")
}

func TestJournalEntryBuilders_SkipFailedAndSkippedOperations(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	operationErr := errors.New("operation failed")

	renameEntries := renameJournalEntries(renamer.Result{Operations: []renamer.RenameOperation{
		{OriginalPath: filepath.Join(rootDir, "skipped.txt"), NewPath: filepath.Join(rootDir, "renamed-skipped.txt"), Skipped: true},
		{OriginalPath: filepath.Join(rootDir, "failed.txt"), NewPath: filepath.Join(rootDir, "renamed-failed.txt"), Error: operationErr},
		{OriginalPath: filepath.Join(rootDir, "ok.txt"), NewPath: filepath.Join(rootDir, "renamed-ok.txt")},
	}}, rootDir)
	require.Len(t, renameEntries, 1)
	assert.Equal(t, "ok.txt", renameEntries[0].Source)

	flattenEntries := flattenJournalEntries(flattener.Result{Operations: []flattener.MoveOperation{
		{OriginalPath: filepath.Join(rootDir, "skipped.txt"), NewPath: filepath.Join(rootDir, "root-skipped.txt"), Skipped: true},
		{OriginalPath: filepath.Join(rootDir, "failed.txt"), NewPath: filepath.Join(rootDir, "root-failed.txt"), Error: operationErr},
		{OriginalPath: filepath.Join(rootDir, "ok.txt"), NewPath: filepath.Join(rootDir, "root-ok.txt")},
	}}, rootDir)
	require.Len(t, flattenEntries, 1)
	assert.Equal(t, "ok.txt", flattenEntries[0].Source)

	duplicateEntries := duplicateJournalEntries(deduplicator.Result{Operations: []deduplicator.DeleteOperation{
		{Path: filepath.Join(rootDir, "skipped.txt"), TrashedTo: filepath.Join(rootDir, ".file-dedup", "trash", "run", "skipped.txt"), Skipped: true},
		{Path: filepath.Join(rootDir, "failed.txt"), TrashedTo: filepath.Join(rootDir, ".file-dedup", "trash", "run", "failed.txt"), Error: operationErr},
		{Path: filepath.Join(rootDir, "ok.txt"), TrashedTo: filepath.Join(rootDir, ".file-dedup", "trash", "run", "ok.txt")},
	}}, rootDir)
	require.Len(t, duplicateEntries, 1)
	assert.Equal(t, "ok.txt", duplicateEntries[0].Source)

	unzipEntries := unzipJournalEntries(unzipper.Result{Operations: []unzipper.ExtractOperation{
		{ArchivePath: filepath.Join(rootDir, "skipped.zip"), ExtractionComplete: true, Skipped: true},
		{ArchivePath: filepath.Join(rootDir, "failed.zip"), ExtractionComplete: true, Error: operationErr},
		{ArchivePath: filepath.Join(rootDir, "ok.zip"), ExtractionComplete: true},
	}}, rootDir)
	require.Len(t, unzipEntries, 1)
	assert.Equal(t, "ok.zip", unzipEntries[0].Source)

	organizeEntries := organizeJournalEntries(organizer.Result{Operations: []organizer.MoveOperation{
		{OriginalPath: filepath.Join(rootDir, "skipped.txt"), NewPath: filepath.Join(rootDir, "txt", "skipped.txt"), Skipped: true},
		{OriginalPath: filepath.Join(rootDir, "failed.txt"), NewPath: filepath.Join(rootDir, "txt", "failed.txt"), Error: operationErr},
		{OriginalPath: filepath.Join(rootDir, "ok.txt"), NewPath: filepath.Join(rootDir, "txt", "ok.txt")},
	}}, rootDir)
	require.Len(t, organizeEntries, 1)
	assert.Equal(t, "ok.txt", organizeEntries[0].Source)
}

func TestJournalEntryBuilders_IgnoreEmptyOrUnchangedDestinations(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()

	renameEntries := renameJournalEntries(renamer.Result{Operations: []renamer.RenameOperation{
		{OriginalPath: filepath.Join(rootDir, "empty.txt")},
		{OriginalPath: filepath.Join(rootDir, "same.txt"), NewPath: filepath.Join(rootDir, "same.txt")},
		{
			OriginalPath: filepath.Join(rootDir, "not-deleted.txt"),
			TrashedTo:    filepath.Join(rootDir, metadata.DirName, "trash", "run", "not-deleted.txt"),
		},
		{
			OriginalPath: filepath.Join(rootDir, "deleted-no-trash.txt"),
			Deleted:      true,
		},
		{
			OriginalPath: filepath.Join(rootDir, "deleted.txt"),
			Deleted:      true,
			TrashedTo:    filepath.Join(rootDir, metadata.DirName, "trash", "run", "deleted.txt"),
		},
	}}, rootDir)
	require.Len(t, renameEntries, 1)
	assert.Equal(t, "trash", renameEntries[0].Type)
	assert.Equal(t, "deleted.txt", renameEntries[0].Source)

	flattenEntries := flattenJournalEntries(flattener.Result{Operations: []flattener.MoveOperation{
		{OriginalPath: filepath.Join(rootDir, "empty.txt")},
		{OriginalPath: filepath.Join(rootDir, "same.txt"), NewPath: filepath.Join(rootDir, "same.txt")},
		{
			OriginalPath: filepath.Join(rootDir, "not-duplicate.txt"),
			TrashedTo:    filepath.Join(rootDir, metadata.DirName, "trash", "run", "not-duplicate.txt"),
		},
		{
			OriginalPath: filepath.Join(rootDir, "duplicate-no-trash.txt"),
			Duplicate:    true,
		},
		{
			OriginalPath: filepath.Join(rootDir, "duplicate.txt"),
			Duplicate:    true,
			TrashedTo:    filepath.Join(rootDir, metadata.DirName, "trash", "run", "duplicate.txt"),
			Hash:         "abc123",
		},
	}}, rootDir)
	require.Len(t, flattenEntries, 1)
	assert.Equal(t, "trash", flattenEntries[0].Type)
	assert.Equal(t, "abc123", flattenEntries[0].Hash)

	unzipEntries := unzipJournalEntries(unzipper.Result{Operations: []unzipper.ExtractOperation{
		{ArchivePath: filepath.Join(rootDir, "kept.zip"), DeletedArchive: true},
		{ArchivePath: filepath.Join(rootDir, "trashed-but-kept.zip"), TrashedTo: filepath.Join(rootDir, metadata.DirName, "trash", "run", "trashed-but-kept.zip")},
		{
			ArchivePath:        filepath.Join(rootDir, "deleted.zip"),
			DeletedArchive:     true,
			TrashedTo:          filepath.Join(rootDir, metadata.DirName, "trash", "run", "deleted.zip"),
			ReplacedFiles:      []unzipper.ReplacedFile{{OriginalPath: filepath.Join(rootDir, "old.txt"), TrashedTo: filepath.Join(rootDir, metadata.DirName, "trash", "run", "old.txt"), Hash: "oldhash"}},
			ExtractionComplete: true,
		},
	}}, rootDir)
	require.Len(t, unzipEntries, 3)
	assert.Equal(t, "replace", unzipEntries[0].Type)
	assert.Equal(t, "old.txt", unzipEntries[0].Source)
	assert.Equal(t, filepath.Join(metadata.DirName, "trash", "run", "old.txt"), unzipEntries[0].Dest)
	assert.Equal(t, "oldhash", unzipEntries[0].Hash)
	assert.True(t, unzipEntries[0].Success)
	assert.Equal(t, "extract", unzipEntries[1].Type)
	assert.Equal(t, "deleted.zip", unzipEntries[1].Source)
	assert.Empty(t, unzipEntries[1].Dest)
	assert.True(t, unzipEntries[1].Success)
	assert.Equal(t, "trash", unzipEntries[2].Type)
	assert.Equal(t, "deleted.zip", unzipEntries[2].Source)
	assert.Equal(t, filepath.Join(metadata.DirName, "trash", "run", "deleted.zip"), unzipEntries[2].Dest)
	assert.True(t, unzipEntries[2].Success)

	organizeEntries := organizeJournalEntries(organizer.Result{Operations: []organizer.MoveOperation{
		{OriginalPath: filepath.Join(rootDir, "empty.txt")},
		{OriginalPath: filepath.Join(rootDir, "same.txt"), NewPath: filepath.Join(rootDir, "same.txt")},
		{OriginalPath: filepath.Join(rootDir, "moved.txt"), NewPath: filepath.Join(rootDir, "txt", "moved.txt")},
	}}, rootDir)
	require.Len(t, organizeEntries, 1)
	assert.Equal(t, "rename", organizeEntries[0].Type)
	assert.Equal(t, "moved.txt", organizeEntries[0].Source)
}

func TestService_RunUndo_ReversesDuplicate(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "a.txt"), "same-content", modTime)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "b.txt"), "same-content", modTime)

	s := New(Options{NoSnapshot: true})

	// Run duplicate to trash one of the files.
	dupExec, err := s.RunDuplicate(DuplicateRequest{
		TargetDir: tmpDir,
		DryRun:    false,
		Workers:   2,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, dupExec.Result.DeletedCount)

	// Verify one file was trashed (only one should remain).
	remaining := 0
	for _, name := range []string{"a.txt", "b.txt"} {
		if _, statErr := os.Stat(filepath.Join(tmpDir, name)); statErr == nil {
			remaining++
		}
	}
	assert.Equal(t, 1, remaining, "only one file should remain after dedup")

	// Run undo.
	undoExec, err := s.RunUndo(UndoRequest{
		TargetDir: tmpDir,
		DryRun:    false,
	})
	require.NoError(t, err)

	assert.Equal(t, 1, undoExec.RestoredCount, "should restore one trashed file")
	assert.Equal(t, 0, undoExec.ErrorCount)
	assert.Equal(t, 0, undoExec.SkippedCount)

	// Both files should exist again.
	_, err = os.Stat(filepath.Join(tmpDir, "a.txt"))
	require.NoError(t, err, "a.txt should be restored")
	_, err = os.Stat(filepath.Join(tmpDir, "b.txt"))
	require.NoError(t, err, "b.txt should be restored")
}

func TestService_RunUndo_ReversesFlatten(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "sub", "deep", "file.txt"), "content", modTime)

	s := New(Options{NoSnapshot: true})

	// Run flatten to move file to root.
	flatExec, err := s.RunFlatten(FlattenRequest{
		TargetDir: tmpDir,
		DryRun:    false,
		Workers:   2,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, flatExec.Result.MovedCount)

	// Verify file is at root.
	_, err = os.Stat(filepath.Join(tmpDir, "file.txt"))
	require.NoError(t, err, "file should be at root after flatten")

	// Run undo.
	undoExec, err := s.RunUndo(UndoRequest{
		TargetDir: tmpDir,
		DryRun:    false,
	})
	require.NoError(t, err)

	assert.Equal(t, 1, undoExec.ReversedCount, "should reverse one rename")
	assert.Equal(t, 0, undoExec.ErrorCount)

	// File should be back in original location.
	_, err = os.Stat(filepath.Join(tmpDir, "sub", "deep", "file.txt"))
	require.NoError(t, err, "file should be restored to original path")

	// File should not be at root.
	_, err = os.Stat(filepath.Join(tmpDir, "file.txt"))
	assert.True(t, os.IsNotExist(err), "file should not remain at root")
}

func TestService_RunUndo_ReversesRename(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "My Document.pdf"), "content", modTime)

	s := New(Options{NoSnapshot: true})

	// Run rename.
	renameExec, err := s.RunRename(RenameRequest{
		TargetDir: tmpDir,
		DryRun:    false,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, renameExec.Result.RenamedCount)

	// Verify renamed file exists.
	_, err = os.Stat(filepath.Join(tmpDir, "2018-06-15_my_document.pdf"))
	require.NoError(t, err)

	// Run undo.
	undoExec, err := s.RunUndo(UndoRequest{
		TargetDir: tmpDir,
		DryRun:    false,
	})
	require.NoError(t, err)

	assert.Equal(t, 1, undoExec.ReversedCount, "should reverse one rename")
	assert.Equal(t, 0, undoExec.ErrorCount)

	// Original name should be restored.
	_, err = os.Stat(filepath.Join(tmpDir, "My Document.pdf"))
	require.NoError(t, err, "original filename should be restored")

	// Renamed file should not exist.
	_, err = os.Stat(filepath.Join(tmpDir, "2018-06-15_my_document.pdf"))
	assert.True(t, os.IsNotExist(err), "renamed file should not exist after undo")
}

func TestService_RunUndo_DryRunNoChanges(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "a.txt"), "same-content", modTime)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "b.txt"), "same-content", modTime)

	s := New(Options{NoSnapshot: true})

	// Run duplicate to trash one file.
	_, err := s.RunDuplicate(DuplicateRequest{
		TargetDir: tmpDir,
		DryRun:    false,
		Workers:   2,
	})
	require.NoError(t, err)

	// Count files before undo dry-run.
	filesBefore, readErr := os.ReadDir(tmpDir)
	require.NoError(t, readErr)

	var progressCalls []string

	// Run undo in dry-run mode.
	undoExec, err := s.RunUndo(UndoRequest{
		TargetDir: tmpDir,
		DryRun:    true,
		OnProgress: func(stage string, processed, total int) {
			progressCalls = append(progressCalls, fmt.Sprintf("%s:%d/%d", stage, processed, total))
		},
	})
	require.NoError(t, err)

	assert.True(t, undoExec.DryRun)
	assert.Equal(t, 1, undoExec.RestoredCount, "dry-run should report what would be restored")
	assert.Equal(t, 0, undoExec.ErrorCount)
	assert.Equal(t, []string{"undoing:1/1"}, progressCalls)

	// Verify no actual changes were made.
	filesAfter, readErr := os.ReadDir(tmpDir)
	require.NoError(t, readErr)
	assert.Len(t, filesAfter, len(filesBefore), "dry-run should not change file count")

	// Verify journal was NOT renamed (still active).
	journalDir := filepath.Join(tmpDir, ".file-dedup", "journal")
	journalEntries, readErr := os.ReadDir(journalDir)
	require.NoError(t, readErr)

	activeCount := 0
	for _, e := range journalEntries {
		if filepath.Ext(e.Name()) == ".jsonl" {
			activeCount++
			assert.NotContains(t, e.Name(), ".rolled-back.jsonl")
		}
	}
	assert.Equal(t, 1, activeCount, "journal should still be active after dry-run")
}

func TestService_RunUndo_SpecificRunID(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "My Document.pdf"), "content", modTime)

	s := New(Options{NoSnapshot: true})

	// Run rename.
	renameExec, err := s.RunRename(RenameRequest{
		TargetDir: tmpDir,
		DryRun:    false,
	})
	require.NoError(t, err)
	require.NotEmpty(t, renameExec.JournalPath)

	// Extract run ID from journal path.
	runID := extractRunID(renameExec.JournalPath)

	// Run undo with specific run ID.
	undoExec, err := s.RunUndo(UndoRequest{
		TargetDir: tmpDir,
		RunID:     runID,
		DryRun:    false,
	})
	require.NoError(t, err)

	assert.Equal(t, runID, undoExec.RunID)
	assert.Equal(t, 1, undoExec.ReversedCount)
	assert.Equal(t, 0, undoExec.ErrorCount)

	// Original name should be restored.
	_, err = os.Stat(filepath.Join(tmpDir, "My Document.pdf"))
	require.NoError(t, err)
}

func TestService_RunUndo_NoJournalError(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "file.txt"), "content",
		time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC))

	s := New(Options{NoSnapshot: true})

	_, err := s.RunUndo(UndoRequest{
		TargetDir: tmpDir,
		DryRun:    false,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no journals found")
}

func TestUndoReplace_DryRunDoesNotRestoreBackup(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	backupRel := filepath.Join(".file-dedup", "trash", "unzip-run", "old.txt")
	backupAbs := filepath.Join(root, backupRel)
	sourceRel := "old.txt"
	sourceAbs := filepath.Join(root, sourceRel)
	testutil.CreateFile(t, backupAbs, "original")

	v, err := safepath.New(root)
	require.NoError(t, err)
	op := undoReplace(workflowTarget{rootDir: root, validator: v}, journal.Entry{
		Type:   "replace",
		Source: sourceRel,
		Dest:   backupRel,
	}, true)

	assert.Equal(t, undoActionRestore, op.Action)
	assert.NoError(t, op.Error)
	assert.FileExists(t, backupAbs)
	assert.NoFileExists(t, sourceAbs)
}

func TestFindLatestJournalSortsActiveJournals(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	v, err := safepath.New(root)
	require.NoError(t, err)
	metaDir, err := metadata.Init(root, v)
	require.NoError(t, err)

	journalDir := filepath.Join(metaDir.Root(), "journal")
	testutil.CreateFile(t, filepath.Join(journalDir, "z-last.rolled-back.jsonl"), "{}\n")
	testutil.CreateFile(t, filepath.Join(journalDir, "b-new.jsonl"), "{}\n")
	testutil.CreateFile(t, filepath.Join(journalDir, "a-old.jsonl"), "{}\n")

	latest, err := findLatestJournal(metaDir)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(journalDir, "b-new.jsonl"), latest)
}

func TestService_RunPurge_PurgesSpecificRun(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "a.txt"), "same-content", modTime)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "b.txt"), "same-content", modTime)

	s := New(Options{NoSnapshot: true})

	// Run duplicate to trash one file.
	dupExec, err := s.RunDuplicate(DuplicateRequest{
		TargetDir: tmpDir,
		DryRun:    false,
		Workers:   2,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, dupExec.Result.DeletedCount)

	// Find the trash run ID from the journal.
	reader := journal.NewReader(dupExec.JournalPath)
	entries, err := reader.Entries()
	require.NoError(t, err)

	confirmed := filterConfirmed(entries)
	require.Len(t, confirmed, 1)

	// The trash dest is like ".file-dedup/trash/<run-id>/b.txt" — extract run ID.
	trashDest := confirmed[0].Dest
	trashParts := strings.SplitN(trashDest, string(filepath.Separator), 4)
	require.GreaterOrEqual(t, len(trashParts), 3, "expected .file-dedup/trash/<run-id>/...")
	trashRunID := trashParts[2]

	// Verify trash directory exists.
	trashDir := filepath.Join(tmpDir, ".file-dedup", "trash", trashRunID)
	_, err = os.Stat(trashDir)
	require.NoError(t, err, "trash directory should exist before purge")

	// Purge the specific run.
	purgeExec, err := s.RunPurge(PurgeRequest{
		TargetDir: tmpDir,
		RunID:     trashRunID,
		DryRun:    false,
	})
	require.NoError(t, err)

	assert.Equal(t, 1, purgeExec.PurgedCount)
	assert.Equal(t, 0, purgeExec.ErrorCount)
	assert.Positive(t, purgeExec.PurgedSize)

	// Verify trash directory is gone.
	_, err = os.Stat(trashDir)
	assert.True(t, os.IsNotExist(err), "trash directory should be removed after purge")
}

func TestService_RunPurge_DryRunDoesNotDelete(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "a.txt"), "same-content", modTime)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "b.txt"), "same-content", modTime)

	s := New(Options{NoSnapshot: true})

	// Run duplicate to trash one file.
	_, err := s.RunDuplicate(DuplicateRequest{
		TargetDir: tmpDir,
		DryRun:    false,
		Workers:   2,
	})
	require.NoError(t, err)

	// Purge all in dry-run mode.
	purgeExec, err := s.RunPurge(PurgeRequest{
		TargetDir: tmpDir,
		All:       true,
		DryRun:    true,
	})
	require.NoError(t, err)

	assert.True(t, purgeExec.DryRun)
	assert.Equal(t, 1, purgeExec.PurgedCount, "dry-run should report what would be purged")
	assert.Positive(t, purgeExec.PurgedSize)

	// Verify trash directory still exists.
	trashRoot := filepath.Join(tmpDir, ".file-dedup", "trash")
	dirEntries, readErr := os.ReadDir(trashRoot)
	require.NoError(t, readErr)
	assert.Len(t, dirEntries, 1, "trash directory should still exist after dry-run")
}

func TestService_RunPurge_PurgesAll(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "a.txt"), "same-content", modTime)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "b.txt"), "same-content", modTime)

	s := New(Options{NoSnapshot: true})

	// Run duplicate to trash one file.
	_, err := s.RunDuplicate(DuplicateRequest{
		TargetDir: tmpDir,
		DryRun:    false,
		Workers:   2,
	})
	require.NoError(t, err)

	// Purge all.
	purgeExec, err := s.RunPurge(PurgeRequest{
		TargetDir: tmpDir,
		All:       true,
		DryRun:    false,
	})
	require.NoError(t, err)

	assert.Equal(t, 1, purgeExec.PurgedCount)
	assert.Equal(t, 0, purgeExec.ErrorCount)

	// Verify trash directory is empty.
	trashRoot := filepath.Join(tmpDir, ".file-dedup", "trash")
	dirEntries, readErr := os.ReadDir(trashRoot)
	require.NoError(t, readErr)
	assert.Empty(t, dirEntries, "all trash runs should be removed")
}

func TestService_RunPurge_NoTrashReturnsEmpty(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "file.txt"), "content",
		time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC))

	s := New(Options{NoSnapshot: true})

	purgeExec, err := s.RunPurge(PurgeRequest{
		TargetDir: tmpDir,
		All:       true,
		DryRun:    false,
	})
	require.NoError(t, err)

	assert.Empty(t, purgeExec.Runs)
	assert.Empty(t, purgeExec.Operations)
	assert.Equal(t, 0, purgeExec.PurgedCount)
}

func TestService_RunPurge_OlderThanFilter(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "a.txt"), "same-content", modTime)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "b.txt"), "same-content", modTime)

	s := New(Options{NoSnapshot: true})

	// Run duplicate to trash one file.
	_, err := s.RunDuplicate(DuplicateRequest{
		TargetDir: tmpDir,
		DryRun:    false,
		Workers:   2,
	})
	require.NoError(t, err)

	// Purge with OlderThan = 1000 hours (the trash is seconds old, so it won't match).
	purgeExec, err := s.RunPurge(PurgeRequest{
		TargetDir: tmpDir,
		OlderThan: 1000 * time.Hour,
		DryRun:    false,
	})
	require.NoError(t, err)

	assert.Equal(t, 0, purgeExec.PurgedCount, "nothing should match older-than filter")
	assert.Len(t, purgeExec.Runs, 1, "should still list existing runs")

	// Purge with OlderThan = 0 seconds (everything is older than 0s effectively, but
	// we need Age > OlderThan, and OlderThan = 1ns should match anything).
	purgeExec2, err := s.RunPurge(PurgeRequest{
		TargetDir: tmpDir,
		OlderThan: time.Nanosecond,
		DryRun:    false,
	})
	require.NoError(t, err)

	assert.Equal(t, 1, purgeExec2.PurgedCount, "trash older than 1ns should be purged")
}

func TestService_RunPurge_NoFilterReturnsNothing(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "a.txt"), "same-content", modTime)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "b.txt"), "same-content", modTime)

	s := New(Options{NoSnapshot: true})

	// Run duplicate to trash one file.
	_, err := s.RunDuplicate(DuplicateRequest{
		TargetDir: tmpDir,
		DryRun:    false,
		Workers:   2,
	})
	require.NoError(t, err)

	// Purge with no filter — should match nothing.
	purgeExec, err := s.RunPurge(PurgeRequest{
		TargetDir: tmpDir,
		DryRun:    false,
	})
	require.NoError(t, err)

	assert.Equal(t, 0, purgeExec.PurgedCount)
	assert.Len(t, purgeExec.Runs, 1, "should still list existing runs")
	assert.Empty(t, purgeExec.Operations, "no operations with no filter")
}

func TestFindJournalAndLatestBranches(t *testing.T) {
	root := t.TempDir()
	validator, err := safepath.New(root)
	require.NoError(t, err)
	metaDir, err := metadata.Init(root, validator)
	require.NoError(t, err)

	_, err = findJournal(metaDir, "missing-run")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "journal not found for run")

	journalDir := filepath.Join(metaDir.Root(), "journal")
	require.NoError(t, os.MkdirAll(journalDir, 0o755))
	oldPath := filepath.Join(journalDir, "rename-20260101T000000.jsonl")
	newPath := filepath.Join(journalDir, "rename-20260102T000000.jsonl")
	rolledBackPath := filepath.Join(journalDir, "rename-20260103T000000.rolled-back.jsonl")
	require.NoError(t, os.WriteFile(newPath, []byte("{}\n"), 0o644))
	require.NoError(t, os.WriteFile(oldPath, []byte("{}\n"), 0o644))
	require.NoError(t, os.WriteFile(rolledBackPath, []byte("{}\n"), 0o644))

	found, err := findLatestJournal(metaDir)
	require.NoError(t, err)
	assert.Equal(t, newPath, found)

	found, err = findJournal(metaDir, "rename-20260101T000000")
	require.NoError(t, err)
	assert.Equal(t, oldPath, found)
}

func TestListTrashRunsBranches(t *testing.T) {
	root := t.TempDir()
	validator, err := safepath.New(root)
	require.NoError(t, err)
	metaDir, err := metadata.Init(root, validator)
	require.NoError(t, err)

	runs, err := listTrashRuns(metaDir)
	require.NoError(t, err)
	assert.Empty(t, runs)

	trashRoot := filepath.Join(metaDir.Root(), "trash")
	require.NoError(t, os.MkdirAll(trashRoot, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(trashRoot, "not-a-run.txt"), []byte("skip"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(trashRoot, "run-b", "nested"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(trashRoot, "run-a"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(trashRoot, "run-b", "nested", "file.txt"), []byte("12345"), 0o644))

	runs, err = listTrashRuns(metaDir)
	require.NoError(t, err)
	require.Len(t, runs, 2)
	assert.Equal(t, "run-a", runs[0].RunID)
	assert.Equal(t, "run-b", runs[1].RunID)
	assert.Equal(t, 0, runs[0].FileCount)
	assert.Equal(t, 1, runs[1].FileCount)
	assert.Equal(t, int64(5), runs[1].TotalSize)
}

func TestWalkTrashDirReturnsZeroOnWalkError(t *testing.T) {
	root := t.TempDir()
	missingPath := filepath.Join(root, "missing")

	fileCount, totalSize := walkTrashDir(missingPath)

	assert.Equal(t, 0, fileCount)
	assert.Equal(t, int64(0), totalSize)
}

func TestBackupUndoReplaceDestinationBranches(t *testing.T) {
	t.Run("existing directory returns skip reason", func(t *testing.T) {
		root := t.TempDir()
		validator, err := safepath.New(root)
		require.NoError(t, err)
		sourceAbs := filepath.Join(root, "restore-target")
		require.NoError(t, os.MkdirAll(sourceAbs, 0o755))

		skipReason, err := backupUndoReplaceDestination(workflowTarget{
			rootDir:   root,
			validator: validator,
		}, sourceAbs, "restore-target")

		require.NoError(t, err)
		assert.Equal(t, "cannot restore over existing directory: restore-target", skipReason)
	})

	t.Run("existing file requires undo trasher", func(t *testing.T) {
		root := t.TempDir()
		validator, err := safepath.New(root)
		require.NoError(t, err)
		sourceAbs := filepath.Join(root, "restore-target.txt")
		require.NoError(t, os.WriteFile(sourceAbs, []byte("current"), 0o644))

		skipReason, err := backupUndoReplaceDestination(workflowTarget{
			rootDir:   root,
			validator: validator,
		}, sourceAbs, "restore-target.txt")

		assert.Empty(t, skipReason)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "undo trasher unavailable")
	})

	t.Run("existing file is moved to undo trash", func(t *testing.T) {
		root := t.TempDir()
		validator, err := safepath.New(root)
		require.NoError(t, err)
		metaDir, err := metadata.Init(root, validator)
		require.NoError(t, err)
		undoTrasher, err := trash.New(metaDir, metaDir.RunID("undo"), validator)
		require.NoError(t, err)
		sourceAbs := filepath.Join(root, "restore-target.txt")
		require.NoError(t, os.WriteFile(sourceAbs, []byte("current"), 0o644))

		skipReason, err := backupUndoReplaceDestination(workflowTarget{
			rootDir:     root,
			validator:   validator,
			undoTrasher: undoTrasher,
		}, sourceAbs, "restore-target.txt")

		require.NoError(t, err)
		assert.Empty(t, skipReason)
		assert.NoFileExists(t, sourceAbs)
	})
}

func TestService_RunUndo_MarksJournalRolledBack(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "My Document.pdf"), "content", modTime)

	s := New(Options{NoSnapshot: true})

	// Run rename to create a journal.
	renameExec, err := s.RunRename(RenameRequest{
		TargetDir: tmpDir,
		DryRun:    false,
	})
	require.NoError(t, err)
	require.NotEmpty(t, renameExec.JournalPath)

	// Verify journal exists before undo.
	_, err = os.Stat(renameExec.JournalPath)
	require.NoError(t, err)

	// Run undo.
	_, err = s.RunUndo(UndoRequest{
		TargetDir: tmpDir,
		DryRun:    false,
	})
	require.NoError(t, err)

	// Original journal should no longer exist.
	_, err = os.Stat(renameExec.JournalPath)
	assert.True(t, os.IsNotExist(err), "original journal should be renamed")

	// Rolled-back journal should exist.
	rolledBackPath := renameExec.JournalPath[:len(renameExec.JournalPath)-len(".jsonl")] + ".rolled-back.jsonl"
	_, err = os.Stat(rolledBackPath)
	require.NoError(t, err, "rolled-back journal should exist")

	// Running undo again should fail (no active journals).
	_, err = s.RunUndo(UndoRequest{
		TargetDir: tmpDir,
		DryRun:    false,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no active journals")
}

func TestService_RunUndo_SkipsWhenHashChanged(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "a.txt"), "same-content", modTime)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "b.txt"), "same-content", modTime)

	s := New(Options{NoSnapshot: true})

	// Run duplicate to trash one file.
	dupExec, err := s.RunDuplicate(DuplicateRequest{
		TargetDir: tmpDir,
		DryRun:    false,
		Workers:   2,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, dupExec.Result.DeletedCount)

	// Find and modify the trashed file to simulate content change.
	reader := journal.NewReader(dupExec.JournalPath)
	entries, err := reader.Entries()
	require.NoError(t, err)

	confirmed := filterConfirmed(entries)
	require.Len(t, confirmed, 1)

	trashedAbs := filepath.Join(tmpDir, confirmed[0].Dest)
	require.NoError(t, os.WriteFile(trashedAbs, []byte("modified-content"), 0o644))

	// Run undo — should skip the entry due to hash mismatch.
	undoExec, err := s.RunUndo(UndoRequest{
		TargetDir: tmpDir,
		DryRun:    false,
	})
	require.NoError(t, err)

	assert.Equal(t, 0, undoExec.RestoredCount, "should not restore when hash changed")
	assert.Equal(t, 1, undoExec.SkippedCount, "should skip due to hash mismatch")
	require.Len(t, undoExec.Operations, 1)
	assert.Equal(t, "skip", undoExec.Operations[0].Action)
	assert.Contains(t, undoExec.Operations[0].SkipReason, "hash mismatch")
}

func TestService_RunUndo_ProceedsWhenNoHash(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "sub", "file.txt"), "content", modTime)

	s := New(Options{NoSnapshot: true})

	// Run flatten — rename entries don't have hashes.
	flatExec, err := s.RunFlatten(FlattenRequest{
		TargetDir: tmpDir,
		DryRun:    false,
		Workers:   2,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, flatExec.JournalPath)

	// Verify journal entries have no hash.
	reader := journal.NewReader(flatExec.JournalPath)
	entries, err := reader.Entries()
	require.NoError(t, err)

	confirmed := filterConfirmed(entries)
	require.NotEmpty(t, confirmed)

	hasRenameWithoutHash := false
	for _, e := range confirmed {
		if e.Type == "rename" && e.Hash == "" {
			hasRenameWithoutHash = true
		}
	}
	assert.True(t, hasRenameWithoutHash, "flatten should produce rename entries without hashes")

	// Run undo — should proceed normally since no hash to verify.
	undoExec, err := s.RunUndo(UndoRequest{
		TargetDir: tmpDir,
		DryRun:    false,
	})
	require.NoError(t, err)

	assert.Positive(t, undoExec.ReversedCount, "should reverse-rename when no hash to verify")
	assert.Equal(t, 0, undoExec.SkippedCount)
}

func TestService_RunUndo_RefusesOverwriteOnRestore(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "a.txt"), "same-content", modTime)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "b.txt"), "same-content", modTime)

	s := New(Options{NoSnapshot: true})

	// Run duplicate to trash one file.
	dupExec, err := s.RunDuplicate(DuplicateRequest{
		TargetDir: tmpDir,
		DryRun:    false,
		Workers:   2,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, dupExec.Result.DeletedCount)

	// Find which file was trashed.
	var trashedName string
	for _, name := range []string{"a.txt", "b.txt"} {
		if _, statErr := os.Stat(filepath.Join(tmpDir, name)); os.IsNotExist(statErr) {
			trashedName = name
		}
	}
	require.NotEmpty(t, trashedName, "one file should have been trashed")

	// Re-create a file at the trashed file's original path with different content.
	newContent := "new important content that must not be lost"
	testutil.CreateFile(t, filepath.Join(tmpDir, trashedName), newContent)

	// Run undo — restore should fail for this file because target exists.
	undoExec, err := s.RunUndo(UndoRequest{
		TargetDir: tmpDir,
		DryRun:    false,
	})
	require.NoError(t, err)

	assert.Equal(t, 0, undoExec.RestoredCount, "should not restore when target exists")
	assert.Positive(t, undoExec.ErrorCount, "should report error for conflicting restore")

	// Verify the new file was NOT overwritten.
	content, readErr := os.ReadFile(filepath.Join(tmpDir, trashedName))
	require.NoError(t, readErr)
	assert.Equal(t, newContent, string(content),
		"existing file content must be preserved when undo restore is refused")
}

func TestService_RunUndo_RestoresReplacedFileAndBacksUpCurrentDestination(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	replacedPath := filepath.Join(tmpDir, "docs", "report.txt")
	testutil.CreateFile(t, replacedPath, "old report")

	archivePath := filepath.Join(tmpDir, "backup.zip")
	writeZipArchive(t, archivePath, []zipFixtureEntry{{
		name:    "docs/report.txt",
		content: []byte("new report"),
	}})

	s := New(Options{NoSnapshot: true})
	unzipExec, err := s.RunUnzip(UnzipRequest{TargetDir: tmpDir, DryRun: false})
	require.NoError(t, err)
	require.NotEmpty(t, unzipExec.JournalPath)
	require.Len(t, unzipExec.Result.Operations, 1)
	assert.Equal(t, 1, unzipExec.Result.Operations[0].ExtractedFiles)
	require.Len(t, unzipExec.Result.Operations[0].ReplacedFiles, 1)

	content, err := os.ReadFile(replacedPath)
	require.NoError(t, err)
	assert.Equal(t, "new report", string(content))

	undoExec, err := s.RunUndo(UndoRequest{TargetDir: tmpDir, DryRun: false})
	require.NoError(t, err)
	assert.Equal(t, 2, undoExec.RestoredCount)
	assert.Equal(t, 1, undoExec.SkippedCount)
	assert.Equal(t, 0, undoExec.ErrorCount)

	content, err = os.ReadFile(replacedPath)
	require.NoError(t, err)
	assert.Equal(t, "old report", string(content))

	var backupFound bool
	trashRoot := filepath.Join(tmpDir, metadata.DirName, "trash")
	walkErr := filepath.Walk(trashRoot, func(path string, info os.FileInfo, err error) error {
		require.NoError(t, err)
		if info.IsDir() || filepath.Base(path) != "report.txt" {
			return nil
		}

		data, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		if string(data) == "new report" && strings.Contains(path, string(filepath.Separator)+"undo-") {
			backupFound = true
		}
		return nil
	})
	require.NoError(t, walkErr)
	assert.True(t, backupFound, "undo should trash the overwritten destination before restore")
}

func TestService_WriteAheadJournal_IntentConfirmationPairs(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "a.txt"), "same-content", modTime)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "b.txt"), "same-content", modTime)

	s := New(Options{NoSnapshot: true})

	dupExec, err := s.RunDuplicate(DuplicateRequest{
		TargetDir: tmpDir,
		DryRun:    false,
		Workers:   2,
	})
	require.NoError(t, err)
	require.NotEmpty(t, dupExec.JournalPath)

	// Read raw entries (without filtering) to verify the two-phase format.
	reader := journal.NewReader(dupExec.JournalPath)
	entries, err := reader.Entries()
	require.NoError(t, err)

	// Each operation should produce exactly 2 entries: intent (ok=false) + confirmation (ok=true).
	require.Len(t, entries, 2, "one operation should produce intent+confirmation pair")

	intent := entries[0]
	confirmation := entries[1]

	assert.False(t, intent.Success, "first entry should be intent (ok=false)")
	assert.True(t, confirmation.Success, "second entry should be confirmation (ok=true)")

	// Intent and confirmation should share the same Type, Source, and Dest.
	assert.Equal(t, confirmation.Type, intent.Type, "type should match between intent and confirmation")
	assert.Equal(t, confirmation.Source, intent.Source, "source should match")
	assert.Equal(t, confirmation.Dest, intent.Dest, "dest should match")
}

func TestService_WriteAheadJournal_ValidatePassesForCompleteJournal(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	modTime := time.Date(2018, 6, 15, 12, 0, 0, 0, time.UTC)
	testutil.CreateFileWithModTime(t, filepath.Join(tmpDir, "My Document.pdf"), "content", modTime)

	s := New(Options{NoSnapshot: true})

	renameExec, err := s.RunRename(RenameRequest{
		TargetDir: tmpDir,
		DryRun:    false,
	})
	require.NoError(t, err)
	require.NotEmpty(t, renameExec.JournalPath)

	// Validate should pass for a well-formed two-phase journal.
	reader := journal.NewReader(renameExec.JournalPath)
	require.NoError(t, reader.Validate(), "complete write-ahead journal should pass validation")
}
