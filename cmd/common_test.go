package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"file-dedup/pkg/deduplicator"
	"file-dedup/pkg/flattener"
	"file-dedup/pkg/organizer"
	"file-dedup/pkg/renamer"
	"file-dedup/pkg/unzipper"
	"file-dedup/pkg/usecase"
)

func TestPrintDryRunBanner_OnlyWhenDryRun(t *testing.T) {
	originalDryRun := dryRun
	t.Cleanup(func() { dryRun = originalDryRun })

	dryRun = false
	assert.Empty(t, captureStdout(t, printDryRunBanner))

	dryRun = true
	assert.Equal(t,
		"=== DRY RUN - no changes will be made ===\n\n",
		captureStdout(t, printDryRunBanner),
	)
}

func TestDefaultSkipListsAreDefensiveCopies(t *testing.T) {
	files := skipFiles()
	dirs := skipDirs()

	assert.Equal(t, []string{".DS_Store", "Thumbs.db", "file-dedup.log"}, files)
	assert.Equal(t, []string{".file-dedup"}, dirs)

	files[0] = "changed"
	dirs[0] = "changed"

	assert.Equal(t, ".DS_Store", skipFiles()[0])
	assert.Equal(t, ".file-dedup", skipDirs()[0])
}

func TestInfoFromMetaCopiesAllFields(t *testing.T) {
	info := infoFromMeta(usecase.WorkflowMeta{
		RootDir:         "/tmp/root",
		FileCount:       7,
		CollectDuration: 3 * time.Second,
		SnapshotPath:    "/tmp/root/snapshot.json",
		JournalPath:     "/tmp/root/journal.json",
	})

	assert.Equal(t, "/tmp/root", info.rootDir)
	assert.Equal(t, 7, info.fileCount)
	assert.Equal(t, 3*time.Second, info.collectDuration)
	assert.Equal(t, "/tmp/root/snapshot.json", info.snapshotPath)
	assert.Equal(t, "/tmp/root/journal.json", info.journalPath)
}

func TestPrintFoundFiles_TrailingBlankLine(t *testing.T) {
	output := captureStdout(t, func() {
		printFoundFiles(3, 1500*time.Microsecond, true)
	})

	assert.Equal(t, "found 3 files in 2ms\n\n", output)
}

func TestRunFileCommand_PrintsMetadataAndEmptyResult(t *testing.T) {
	originalDryRun := dryRun
	dryRun = true
	t.Cleanup(func() { dryRun = originalDryRun })

	type execution struct {
		meta fileCommandExecutionInfo
	}

	var extraHeaderCalled bool
	var gotProgress bool
	var empty bool
	var err error
	var result execution

	output := captureStdout(t, func() {
		result, empty, err = runFileCommand(
			"rename",
			true,
			func(progress *progressReporter) (execution, error) {
				gotProgress = progress != nil
				return execution{meta: fileCommandExecutionInfo{
					rootDir:         "/tmp/root",
					fileCount:       0,
					collectDuration: 2 * time.Millisecond,
					snapshotPath:    "/tmp/root/snapshot.json",
					journalPath:     "/tmp/root/journal.json",
				}}, nil
			},
			func(e execution) fileCommandExecutionInfo { return e.meta },
			func() {
				extraHeaderCalled = true
				printCommandHeader("extra", "/tmp/extra")
			},
		)
	})

	require.NoError(t, err)
	assert.True(t, gotProgress)
	assert.True(t, empty)
	assert.True(t, extraHeaderCalled)
	assert.Equal(t, "/tmp/root", result.meta.rootDir)
	assert.Contains(t, output, "=== DRY RUN - no changes will be made ===\n\n")
	assert.Contains(t, output, "collecting files...\n")
	assert.Contains(t, output, "Command: rename\n")
	assert.Contains(t, output, "root directory: /tmp/root\n")
	assert.Contains(t, output, "Snapshot: /tmp/root/snapshot.json\n")
	assert.Contains(t, output, "Journal: /tmp/root/journal.json\n")
	assert.Contains(t, output, "Command: extra\n")
	assert.Contains(t, output, "found 0 files in 2ms\n\n")
	assert.Contains(t, output, "No files to process.\n")
}

func TestRunFileCommand_ReturnsExecuteError(t *testing.T) {
	expectedErr := errors.New("collect failed")

	var empty bool
	var err error
	output := captureStdout(t, func() {
		_, empty, err = runFileCommand(
			"flatten",
			false,
			func(_ *progressReporter) (int, error) { return 7, expectedErr },
			func(int) fileCommandExecutionInfo {
				t.Fatal("execution info must not be called on error")
				return fileCommandExecutionInfo{}
			},
			nil,
		)
	})

	assert.ErrorIs(t, err, expectedErr)
	assert.False(t, empty)
	assert.Equal(t, "collecting files...\n", output)
}

func TestRunWorkersFileCommandPassesGlobalsAndProgress(t *testing.T) {
	originalDryRun := dryRun
	originalWorkers := workers
	dryRun = true
	workers = 3
	t.Cleanup(func() {
		dryRun = originalDryRun
		workers = originalWorkers
	})

	type execution struct {
		meta fileCommandExecutionInfo
	}

	var gotTarget string
	var gotDryRun bool
	var gotWorkers int
	var result execution
	var empty bool
	var err error
	output := captureStdout(t, func() {
		result, empty, err = runWorkersFileCommand(
			"duplicate",
			true,
			"/tmp/target",
			func(targetDir string, dryRun bool, workers int, onProgress usecase.ProgressCallback) (execution, error) {
				gotTarget = targetDir
				gotDryRun = dryRun
				gotWorkers = workers
				onProgress("hashing", 1, 1)
				return execution{meta: fileCommandExecutionInfo{
					rootDir:         targetDir,
					fileCount:       1,
					collectDuration: time.Millisecond,
				}}, nil
			},
			func(e execution) fileCommandExecutionInfo { return e.meta },
		)
	})

	require.NoError(t, err)
	assert.False(t, empty)
	assert.Equal(t, "/tmp/target", gotTarget)
	assert.True(t, gotDryRun)
	assert.Equal(t, 3, gotWorkers)
	assert.Equal(t, "/tmp/target", result.meta.rootDir)
	assert.Contains(t, output, "Workers: 3\n")
	assert.Contains(t, output, "found 1 files in 1ms\n\n")
}

func TestPrintDetailedOperations_NormalModePrintsOnlyErrors(t *testing.T) {
	originalVerbose := verbose
	originalDryRun := dryRun
	verbose = false
	dryRun = false
	t.Cleanup(func() {
		verbose = originalVerbose
		dryRun = originalDryRun
	})

	output := captureStdout(t, func() {
		printDetailedOperations(
			[]string{"ok", "failed"},
			func(operation string) { printlnForTest(operation) },
			func(operation string) bool { return operation == "failed" },
		)
	})

	assert.Equal(t, "failed\n\n", output)
}

func TestPrintDetailedOperations_VerbosePrintsAll(t *testing.T) {
	originalVerbose := verbose
	originalDryRun := dryRun
	verbose = true
	dryRun = false
	t.Cleanup(func() {
		verbose = originalVerbose
		dryRun = originalDryRun
	})

	output := captureStdout(t, func() {
		printDetailedOperations(
			[]string{"ok", "failed"},
			func(operation string) { printlnForTest(operation) },
			func(string) bool { return false },
		)
	})

	assert.Equal(t, "ok\nfailed\n\n", output)
}

func TestPrintDetailedOperations_DryRunPrintsAll(t *testing.T) {
	originalVerbose := verbose
	originalDryRun := dryRun
	verbose = false
	dryRun = true
	t.Cleanup(func() {
		verbose = originalVerbose
		dryRun = originalDryRun
	})

	output := captureStdout(t, func() {
		printDetailedOperations(
			[]string{"ok", "failed"},
			func(operation string) { printlnForTest(operation) },
			func(string) bool { return false },
		)
	})

	assert.Equal(t, "ok\nfailed\n\n", output)
}

func TestPrintSummary_PrintsHeaderAndLines(t *testing.T) {
	output := captureStdout(t, func() {
		printSummary("renamed: 2", "errors: 0")
	})

	assert.Equal(t, "=== Summary ===\nrenamed: 2\nerrors: 0\n", output)
}

func TestPrintDryRunHint_OnlyWhenDryRun(t *testing.T) {
	originalDryRun := dryRun
	t.Cleanup(func() { dryRun = originalDryRun })

	dryRun = false
	assert.Empty(t, captureStdout(t, printDryRunHint))

	dryRun = true
	assert.Equal(t,
		"\nRun without --dry-run to apply changes.\n",
		captureStdout(t, printDryRunHint),
	)
}

func TestProgressReporterReport_NormalizesAndSkipsUnchanged(t *testing.T) {
	reporter := newStoppedProgressReporter("collecting")

	invalidTotal := captureStderr(t, func() {
		reporter.Report("ignored", 1, 0)
	})
	assert.Empty(t, invalidTotal)

	negativeProcessed := captureStderr(t, func() {
		reporter.Report(" ", -5, 10)
	})
	assert.Contains(t, negativeProcessed, "collecting [------------------------]   0% (0/10)")

	reporter.lastPrintTime = time.Now().Add(-2 * progressPrintInterval)
	unchanged := captureStderr(t, func() {
		reporter.Report("collecting", 0, 10)
	})
	assert.Empty(t, unchanged)

	overTotal := captureStderr(t, func() {
		reporter.Report("done", 15, 10)
	})
	assert.Contains(t, overTotal, "done [########################] 100% (10/10)")
}

func TestProgressReporterReport_PrintsFirstDeterminateEvenWhenStageMatches(t *testing.T) {
	reporter := newStoppedProgressReporter("copying")
	reporter.stage = "copying"
	reporter.lastProcessed = 1
	reporter.lastTotal = 3
	reporter.lastPrintTime = time.Now().Add(-2 * progressPrintInterval)

	output := captureStderr(t, func() {
		reporter.Report("copying", 1, 3)
	})

	assert.Contains(t, output, "copying [########----------------]  33% (1/3)")
}

func TestProgressReporterReport_PrintsAtClampBoundaries(t *testing.T) {
	reporter := newStoppedProgressReporter("copying")

	zero := captureStderr(t, func() {
		reporter.Report("copying", 0, 10)
	})
	assert.Contains(t, zero, "copying [------------------------]   0% (0/10)")

	reporter.lastPrintTime = time.Now().Add(-2 * progressPrintInterval)
	complete := captureStderr(t, func() {
		reporter.Report("copying", 10, 10)
	})
	assert.Contains(t, complete, "copying [########################] 100% (10/10)")
}

func TestProgressReporterReport_UpdatesStateForEachChangeKind(t *testing.T) {
	reporter := newStoppedProgressReporter("collecting")

	first := captureStderr(t, func() {
		reporter.Report("copying", 1, 3)
	})
	assert.Contains(t, first, "copying [########----------------]  33% (1/3)")
	assert.Equal(t, "copying", reporter.stage)
	assert.Equal(t, 1, reporter.lastProcessed)
	assert.Equal(t, 3, reporter.lastTotal)
	assert.True(t, reporter.hasDeterminate)

	reporter.lastPrintTime = time.Now().Add(-2 * progressPrintInterval)
	processedChanged := captureStderr(t, func() {
		reporter.Report("copying", 2, 3)
	})
	assert.Contains(t, processedChanged, "copying [################--------]  66% (2/3)")
	assert.Equal(t, 2, reporter.lastProcessed)

	reporter.lastPrintTime = time.Now().Add(-2 * progressPrintInterval)
	totalChanged := captureStderr(t, func() {
		reporter.Report("copying", 2, 4)
	})
	assert.Contains(t, totalChanged, "copying [############------------]  50% (2/4)")
	assert.Equal(t, 4, reporter.lastTotal)

	reporter.lastPrintTime = time.Now().Add(-2 * progressPrintInterval)
	stageChanged := captureStderr(t, func() {
		reporter.Report("verifying", 2, 4)
	})
	assert.Contains(t, stageChanged, "verifying [############------------]  50% (2/4)")
	assert.Equal(t, "verifying", reporter.stage)
}

func TestProgressReporterReport_ThrottlesIncompleteUnchangedInterval(t *testing.T) {
	reporter := newStoppedProgressReporter("copying")
	reporter.hasDeterminate = true
	reporter.stage = "copying"
	reporter.lastProcessed = 1
	reporter.lastTotal = 3
	reporter.lastPrintTime = time.Now()

	output := captureStderr(t, func() {
		reporter.Report("copying", 2, 3)
	})

	assert.Empty(t, output)
	assert.Equal(t, 2, reporter.lastProcessed)
	assert.Equal(t, 3, reporter.lastTotal)
}

func TestProgressReporterHeartbeat_SkipsInvalidDeterminateTotal(t *testing.T) {
	reporter := newStoppedProgressReporter("copying")
	reporter.stage = "copying"
	reporter.lastProcessed = 0
	reporter.lastTotal = 0
	reporter.hasDeterminate = true

	assert.Empty(t, captureStderr(t, reporter.printHeartbeat))
}

func TestProgressReporterHeartbeat_PrintsZeroTotalOnlyWhenIndeterminate(t *testing.T) {
	reporter := newStoppedProgressReporter("copying")
	reporter.stage = "copying"
	reporter.lastTotal = 0

	output := captureStderr(t, reporter.printHeartbeat)

	assert.Contains(t, output, "copying [------------------------] --% (--/--)")
	assert.Equal(t, 1, reporter.indeterminate)
}

func TestProgressReporterHeartbeat_DeterminateAndIndeterminate(t *testing.T) {
	determinate := newStoppedProgressReporter("copying")
	determinate.stage = "copying"
	determinate.lastProcessed = 2
	determinate.lastTotal = 4
	determinate.hasDeterminate = true

	inProgress := captureStderr(t, determinate.printHeartbeat)
	assert.Contains(t, inProgress, "copying [############------------]  50% (2/4)")

	determinate.lastProcessed = 4
	complete := captureStderr(t, determinate.printHeartbeat)
	assert.Empty(t, complete)

	indeterminate := newStoppedProgressReporter("scanning")
	first := captureStderr(t, indeterminate.printHeartbeat)
	second := captureStderr(t, indeterminate.printHeartbeat)
	assert.Contains(t, first, "scanning [------------------------] --% (--/--)")
	assert.Contains(t, second, "scanning [#-----------------------] --% (--/--)")
}

func TestRenderProgressLine_ClampsBarBounds(t *testing.T) {
	assert.Equal(t,
		"stage [------------------------]   0% (0/10) elapsed 1s",
		renderProgressLine("stage", 0, 10, time.Second),
	)
	assert.Equal(t,
		"stage [########################] 100% (10/10) elapsed 1s",
		renderProgressLine("stage", 10, 10, time.Second),
	)

	assert.NotPanics(t, func() {
		line := renderProgressLine("stage", -1, 10, time.Second)
		assert.Contains(t, line, "stage [------------------------] -10% (-1/10) elapsed 1s")
	})

	assert.NotPanics(t, func() {
		line := renderProgressLine("stage", 12, 10, time.Second)
		assert.Contains(t, line, "stage [########################] 120% (12/10) elapsed 1s")
	})
}

func TestRenderIndeterminateLine_BounceBoundaries(t *testing.T) {
	assert.Equal(t,
		"scan [------------------------] --% (--/--) elapsed 1s",
		renderIndeterminateLine("scan", 0, time.Second),
	)
	assert.Equal(t,
		"scan [########################] --% (--/--) elapsed 1s",
		renderIndeterminateLine("scan", progressBarWidth, time.Second),
	)
	assert.Equal(t,
		"scan [#######################-] --% (--/--) elapsed 1s",
		renderIndeterminateLine("scan", progressBarWidth+1, time.Second),
	)
	assert.Equal(t,
		"scan [------------------------] --% (--/--) elapsed 1s",
		renderIndeterminateLine("scan", -1, time.Second),
	)
}

func TestFormatBytes_Boundaries(t *testing.T) {
	assert.Equal(t, "1023 bytes", formatBytes(1023))
	assert.Equal(t, "1.00 KB", formatBytes(1024))
	assert.Equal(t, "1024.00 KB", formatBytes(1024*1024-1))
	assert.Equal(t, "1.00 MB", formatBytes(1024*1024))
	assert.Equal(t, "1024.00 MB", formatBytes(1024*1024*1024-1))
	assert.Equal(t, "1.00 GB", formatBytes(1024*1024*1024))
}

func TestCommandOperationPrinters_AllBranches(t *testing.T) {
	originalVerbose := verbose
	originalDryRun := dryRun
	t.Cleanup(func() {
		verbose = originalVerbose
		dryRun = originalDryRun
	})

	verbose = true
	dryRun = false
	assert.Equal(t,
		"ERROR: dup.txt: denied\n",
		captureStdout(t, func() {
			printDuplicateOperation(deduplicator.DeleteOperation{
				Path:  "dup.txt",
				Error: errors.New("denied"),
			})
		}),
	)
	assert.Equal(t,
		"SKIP: dup.txt (kept)\n",
		captureStdout(t, func() {
			printDuplicateOperation(deduplicator.DeleteOperation{
				Path:       "dup.txt",
				Skipped:    true,
				SkipReason: "kept",
			})
		}),
	)
	assert.Equal(t,
		"DELETE: dup.txt\n   KEPT: original.txt\n   HASH: abc123\n",
		captureStdout(t, func() {
			printDuplicateOperation(deduplicator.DeleteOperation{
				Path:       "dup.txt",
				OriginalOf: "original.txt",
				Hash:       "abc123",
			})
		}),
	)

	assert.Equal(t,
		"ERROR: old.txt: move failed\n",
		captureStdout(t, func() {
			printFlattenOperation(flattener.MoveOperation{
				OriginalPath: "old.txt",
				Error:        errors.New("move failed"),
			})
		}),
	)
	assert.Equal(t,
		"DUPLICATE: old.txt\n   KEPT: kept.txt\n",
		captureStdout(t, func() {
			printFlattenOperation(flattener.MoveOperation{
				OriginalPath: "old.txt",
				NewPath:      "kept.txt",
				Duplicate:    true,
			})
		}),
	)
	assert.Equal(t,
		"SKIP: old.txt (same path)\n",
		captureStdout(t, func() {
			printFlattenOperation(flattener.MoveOperation{
				OriginalPath: "old.txt",
				Skipped:      true,
				SkipReason:   "same path",
			})
		}),
	)
	assert.Equal(t,
		"MOVE: old.txt\n  TO: new.txt\n",
		captureStdout(t, func() {
			printFlattenOperation(flattener.MoveOperation{
				OriginalPath: "old.txt",
				NewPath:      "new.txt",
			})
		}),
	)

	assert.Equal(t,
		"ERROR: src.txt: organize failed\n",
		captureStdout(t, func() {
			printOrganizeOperation(organizer.MoveOperation{
				OriginalPath: "src.txt",
				Error:        errors.New("organize failed"),
			})
		}),
	)
	assert.Equal(t,
		"SKIP: src.txt (exists)\n",
		captureStdout(t, func() {
			printOrganizeOperation(organizer.MoveOperation{
				OriginalPath: "src.txt",
				Skipped:      true,
				SkipReason:   "exists",
			})
		}),
	)
	assert.Equal(t,
		"MOVE: src.txt\n  TO: txt/src.txt\n",
		captureStdout(t, func() {
			printOrganizeOperation(organizer.MoveOperation{
				OriginalPath: "src.txt",
				NewPath:      "txt/src.txt",
			})
		}),
	)

	assert.Equal(t,
		"SKIP: old.txt (already normalized)\n",
		captureStdout(t, func() {
			printRenameOperationForTest(renamer.RenameOperation{
				OriginalName: "old.txt",
				Skipped:      true,
				SkipReason:   "already normalized",
			})
		}),
	)
	assert.Equal(t,
		"ERROR: old.txt -> new.txt: rename failed\n",
		captureStdout(t, func() {
			printRenameOperationForTest(renamer.RenameOperation{
				OriginalName: "old.txt",
				NewName:      "new.txt",
				Error:        errors.New("rename failed"),
			})
		}),
	)
	assert.Equal(t,
		"RENAME: /root/old.txt\n    TO: /root/new.txt\n",
		captureStdout(t, func() {
			printRenameOperationForTest(renamer.RenameOperation{
				OriginalPath: "/root/old.txt",
				NewPath:      "/root/new.txt",
			})
		}),
	)
}

func TestUnzipOperationPrinter_AllBranches(t *testing.T) {
	originalDryRun := dryRun
	t.Cleanup(func() { dryRun = originalDryRun })

	assert.Equal(t,
		"ERROR: archive.zip: corrupt\n",
		captureStdout(t, func() {
			printUnzipOperation(unzipper.ExtractOperation{
				ArchivePath: "archive.zip",
				Error:       errors.New("corrupt"),
			})
		}),
	)
	assert.Equal(t,
		"SKIP: archive.zip (empty)\n",
		captureStdout(t, func() {
			printUnzipOperation(unzipper.ExtractOperation{
				ArchivePath: "archive.zip",
				Skipped:     true,
				SkipReason:  "empty",
			})
		}),
	)

	dryRun = true
	assert.Equal(t,
		strings.Join([]string{
			"UNZIP: archive.zip",
			" FILES: 2",
			"  DIRS: 1",
			"SKIPPED ENTRIES: 2",
			"  ENTRY ERROR: bad path",
			"  ENTRY ERROR: symlink",
			"NESTED: 3",
			"DELETE: source archive (dry-run)",
			"",
		}, "\n"),
		captureStdout(t, func() {
			printUnzipOperation(unzipper.ExtractOperation{
				ArchivePath:    "archive.zip",
				ExtractedFiles: 2,
				ExtractedDirs:  1,
				SkippedEntries: 2,
				EntryErrors:    []string{"bad path", "symlink"},
				NestedArchives: 3,
				DeletedArchive: true,
			})
		}),
	)

	dryRun = false
	assert.Equal(t,
		"UNZIP: archive.zip\n FILES: 0\n  DIRS: 0\nDELETE: source archive\n",
		captureStdout(t, func() {
			printUnzipOperation(unzipper.ExtractOperation{
				ArchivePath:    "archive.zip",
				DeletedArchive: true,
			})
		}),
	)
	assert.Equal(t,
		"UNZIP: archive.zip\n FILES: 0\n  DIRS: 0\nDELETE: source archive not removed\n",
		captureStdout(t, func() {
			printUnzipOperation(unzipper.ExtractOperation{ArchivePath: "archive.zip"})
		}),
	)
}

func TestUndoAndPurgePrinters_AllBranches(t *testing.T) {
	assert.Equal(t,
		"ERROR: [trash] src.txt: restore failed\n",
		captureStdout(t, func() {
			printUndoOperation(usecase.UndoOperation{
				EntryType: "trash",
				Source:    "src.txt",
				Error:     errors.New("restore failed"),
			})
		}),
	)
	assert.Equal(t,
		"SKIP: [extract] archive.zip (not reversible)\n",
		captureStdout(t, func() {
			printUndoOperation(usecase.UndoOperation{
				EntryType:  "extract",
				Source:     "archive.zip",
				Action:     "skip",
				SkipReason: "not reversible",
			})
		}),
	)
	assert.Equal(t,
		"RESTORE: src.txt\n   FROM: trash/src.txt\n",
		captureStdout(t, func() {
			printUndoOperation(usecase.UndoOperation{
				Action: "restore",
				Source: "src.txt",
				Dest:   "trash/src.txt",
			})
		}),
	)
	assert.Equal(t,
		"REVERSE: new.txt\n     TO: old.txt\n",
		captureStdout(t, func() {
			printUndoOperation(usecase.UndoOperation{
				Action: "reverse-rename",
				Source: "old.txt",
				Dest:   "new.txt",
			})
		}),
	)

	dryRunExec := usecase.PurgeExecution{DryRun: true}
	assert.Equal(t,
		"No runs match the filter criteria.\n\n",
		captureStdout(t, func() { printPurgeOperations(dryRunExec) }),
	)
	applyExec := usecase.PurgeExecution{}
	assert.Equal(t,
		"No runs matched the filter criteria. Nothing purged.\n\n",
		captureStdout(t, func() { printPurgeOperations(applyExec) }),
	)
	operationsExec := usecase.PurgeExecution{
		DryRun: true,
		Operations: []usecase.PurgeOperation{
			{RunID: "run-error", Error: errors.New("remove failed")},
			{RunID: "run-purge", FileCount: 2, TotalSize: 2048, Purged: true},
		},
	}
	assert.Equal(t,
		"ERROR: run-error: remove failed\nWOULD PURGE: run-purge  2 file(s), 2.00 KB\n\n",
		captureStdout(t, func() { printPurgeOperations(operationsExec) }),
	)

	applyOperationsExec := usecase.PurgeExecution{
		Operations: []usecase.PurgeOperation{{RunID: "run-purge", FileCount: 1, TotalSize: 3, Purged: true}},
	}
	assert.Equal(t,
		"PURGE: run-purge  1 file(s), 3 bytes\n\n",
		captureStdout(t, func() { printPurgeOperations(applyOperationsExec) }),
	)

	runsExec := usecase.PurgeExecution{Runs: []usecase.TrashRunInfo{
		{RunID: "run-a", FileCount: 2, TotalSize: 1536, Age: 2*time.Second + 500*time.Millisecond},
	}}
	assert.Equal(t,
		"Trash runs: 1 total\n  run-a  2 file(s), 1.50 KB, age 2s\n\n",
		captureStdout(t, func() { printTrashRuns(runsExec) }),
	)
}

func TestParseOlderThan(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want time.Duration
	}{
		{name: "empty", in: "", want: 0},
		{name: "hours", in: "36h", want: 36 * time.Hour},
		{name: "days", in: "7d", want: 7 * 24 * time.Hour},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseOlderThan(tc.in)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	_, err := parseOlderThan("bad")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid duration")

	_, err = parseOlderThan("xd")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid duration")
}

func TestRunCommands_PrintSummariesForRealExecutions(t *testing.T) {
	originalDryRun := dryRun
	originalVerbose := verbose
	originalWorkers := workers
	originalNoSnapshot := noSnapshot
	t.Cleanup(func() {
		dryRun = originalDryRun
		verbose = originalVerbose
		workers = originalWorkers
		noSnapshot = originalNoSnapshot
	})

	dryRun = true
	verbose = true
	workers = 1
	noSnapshot = true

	t.Run("unzip no archives", func(t *testing.T) {
		root := t.TempDir()
		writeTestFile(t, filepath.Join(root, "plain.txt"), "plain")

		output := captureStdout(t, func() {
			require.NoError(t, runUnzip(nil, []string{root}))
		})

		assert.Contains(t, output, "Command: UNZIP\n")
		assert.Contains(t, output, "no zip archives to process\n\n")
		assert.Contains(t, output, "Archives Found:     0")
		assert.Contains(t, output, "Run without --dry-run to apply changes.\n")
	})

	t.Run("organize dry run", func(t *testing.T) {
		root := t.TempDir()
		writeTestFile(t, filepath.Join(root, "photo.JPG"), "image")

		output := captureStdout(t, func() {
			require.NoError(t, runOrganize(nil, []string{root}))
		})

		assert.Contains(t, output, "Command: ORGANIZE\n")
		assert.Contains(t, output, "MOVE: ")
		assert.Contains(t, output, "  TO: ")
		assert.Contains(t, output, "Total files:     1")
		assert.Contains(t, output, "Dirs created:    1")
		assert.Contains(t, output, "Run without --dry-run to apply changes.\n")
	})

	t.Run("flatten dry run", func(t *testing.T) {
		root := t.TempDir()
		writeTestFile(t, filepath.Join(root, "nested", "doc.txt"), "doc")

		output := captureStdout(t, func() {
			require.NoError(t, runFlatten(nil, []string{root}))
		})

		assert.Contains(t, output, "Command: FLATTEN\n")
		assert.Contains(t, output, "Workers: 1\n")
		assert.Contains(t, output, "MOVE: ")
		assert.Contains(t, output, "Moved:           1")
		assert.NotContains(t, output, "Dirs removed:")
	})

	t.Run("duplicate dry run", func(t *testing.T) {
		root := t.TempDir()
		writeTestFile(t, filepath.Join(root, "a.txt"), "same")
		writeTestFile(t, filepath.Join(root, "b.txt"), "same")

		output := captureStdout(t, func() {
			require.NoError(t, runDuplicate(nil, []string{root}))
		})

		assert.Contains(t, output, "Command: DUPLICATE\n")
		assert.Contains(t, output, "Computing hashes and finding duplicates...\n")
		assert.Contains(t, output, "DELETE: ")
		assert.Contains(t, output, "   KEPT: ")
		assert.Contains(t, output, "Duplicates found: 1")
		assert.Contains(t, output, "Space recovered:  4 bytes")
		assert.Contains(t, output, "Run without --dry-run to apply changes.\n")
	})

	t.Run("rename dry run", func(t *testing.T) {
		root := t.TempDir()
		writeTestFile(t, filepath.Join(root, "My Document.txt"), "doc")
		alreadyNamed := filepath.Join(root, "2026-06-17_already_named.txt")
		writeTestFile(t, alreadyNamed, "skip")
		modTime := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
		require.NoError(t, os.Chtimes(alreadyNamed, modTime, modTime))

		output := captureStdout(t, func() {
			require.NoError(t, runRename(nil, []string{root}))
		})

		assert.Contains(t, output, "Command: RENAME\n")
		assert.Contains(t, output, "RENAME: ")
		assert.Contains(t, output, "SKIP: 2026-06-17_already_named.txt (name unchanged)\n")
		assert.Contains(t, output, "Total files:  2")
		assert.Contains(t, output, "Renamed:      1")
		assert.Contains(t, output, "Skipped:      1")
		assert.Contains(t, output, "Run without --dry-run to apply changes.\n")
	})

	t.Run("manifest", func(t *testing.T) {
		root := t.TempDir()
		writeTestFile(t, filepath.Join(root, "keep.txt"), "keep")

		output := captureStdout(t, func() {
			require.NoError(t, runManifest([]string{root}, "manifest.json"))
		})

		assert.Contains(t, output, "Collecting files and computing hashes...\n")
		assert.Contains(t, output, "Command: MANIFEST\n")
		assert.Contains(t, output, "Output file: "+filepath.Join(root, "manifest.json")+"\n")
		assert.Contains(t, output, "Workers: 1\n")
		assert.Contains(t, output, "\nCompleted in ")
		assert.Contains(t, output, "Total files:    1")
		assert.Contains(t, output, "Unique files:   1")
		assert.Contains(t, output, "Manifest saved: "+filepath.Join(root, "manifest.json"))
		assert.FileExists(t, filepath.Join(root, "manifest.json"))
	})
}

func TestRunPurge_PrintsDryRunTrashRunAndSummary(t *testing.T) {
	originalDryRun := dryRun
	originalRunID := purgeRunID
	originalOlder := purgeOlderStr
	originalAll := purgeAll
	originalForce := purgeForce
	originalWorkers := workers
	originalNoSnapshot := noSnapshot
	t.Cleanup(func() {
		dryRun = originalDryRun
		purgeRunID = originalRunID
		purgeOlderStr = originalOlder
		purgeAll = originalAll
		purgeForce = originalForce
		workers = originalWorkers
		noSnapshot = originalNoSnapshot
	})

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "a.txt"), "same-content")
	writeTestFile(t, filepath.Join(root, "b.txt"), "same-content")
	_, err := usecase.New(usecase.Options{NoSnapshot: true}).RunDuplicate(usecase.DuplicateRequest{
		TargetDir: root,
		DryRun:    false,
		Workers:   1,
	})
	require.NoError(t, err)

	dryRun = true
	purgeRunID = ""
	purgeOlderStr = ""
	purgeAll = true
	purgeForce = false
	workers = 1
	noSnapshot = true

	output := captureStdout(t, func() {
		require.NoError(t, runPurge(nil, []string{root}))
	})

	assert.Contains(t, output, "=== DRY RUN - no changes will be made ===\n\n")
	assert.Contains(t, output, "Command: PURGE\n")
	assert.Contains(t, output, "Trash runs: 1 total\n")
	assert.Contains(t, output, "WOULD PURGE: ")
	assert.Contains(t, output, "=== Summary ===\n")
	assert.Contains(t, output, "Purged:    1 run(s)")
	assert.Contains(t, output, "Errors:    0")
	assert.Contains(t, output, "Run without --dry-run to apply changes.\n")
}

func TestRunFlatten_PrintsRemovedDirsAndNoDryRunHintInApplyMode(t *testing.T) {
	originalDryRun := dryRun
	originalVerbose := verbose
	originalWorkers := workers
	originalNoSnapshot := noSnapshot
	t.Cleanup(func() {
		dryRun = originalDryRun
		verbose = originalVerbose
		workers = originalWorkers
		noSnapshot = originalNoSnapshot
	})

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "nested", "doc.txt"), "doc")
	dryRun = false
	verbose = false
	workers = 1
	noSnapshot = true

	output := captureStdout(t, func() {
		require.NoError(t, runFlatten(nil, []string{root}))
	})

	assert.Contains(t, output, "Command: FLATTEN\n")
	assert.Contains(t, output, "Dirs removed:    ")
	assert.NotContains(t, output, "Run without --dry-run to apply changes.")
}

func TestRunFlatten_PrintsDryRunHint(t *testing.T) {
	originalDryRun := dryRun
	originalVerbose := verbose
	originalWorkers := workers
	originalNoSnapshot := noSnapshot
	t.Cleanup(func() {
		dryRun = originalDryRun
		verbose = originalVerbose
		workers = originalWorkers
		noSnapshot = originalNoSnapshot
	})

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "nested", "doc.txt"), "doc")
	dryRun = true
	verbose = false
	workers = 1
	noSnapshot = true

	output := captureStdout(t, func() {
		require.NoError(t, runFlatten(nil, []string{root}))
	})

	assert.Contains(t, output, "\nRun without --dry-run to apply changes.\n")
}

func TestRunUnzip_ReturnsInvalidEntryErrorInApplyMode(t *testing.T) {
	originalDryRun := dryRun
	originalVerbose := verbose
	originalNoSnapshot := noSnapshot
	t.Cleanup(func() {
		dryRun = originalDryRun
		verbose = originalVerbose
		noSnapshot = originalNoSnapshot
	})

	root := t.TempDir()
	writeZipForCommandTest(t, filepath.Join(root, "bad.zip"), map[string]string{
		"../escape.txt": "escape",
	})
	dryRun = false
	verbose = false
	noSnapshot = true

	var err error
	captureStdout(t, func() {
		err = runUnzip(nil, []string{root})
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "illegal entry path")
}

func TestRunFileCommands_ReturnErrorsForInvalidTargets(t *testing.T) {
	originalDryRun := dryRun
	originalWorkers := workers
	originalNoSnapshot := noSnapshot
	t.Cleanup(func() {
		dryRun = originalDryRun
		workers = originalWorkers
		noSnapshot = originalNoSnapshot
	})

	dryRun = true
	workers = 1
	noSnapshot = true
	missing := filepath.Join(t.TempDir(), "missing")

	commands := []struct {
		name string
		run  func() error
	}{
		{name: "unzip", run: func() error { return runUnzip(nil, []string{missing}) }},
		{name: "rename", run: func() error { return runRename(nil, []string{missing}) }},
		{name: "flatten", run: func() error { return runFlatten(nil, []string{missing}) }},
		{name: "duplicate", run: func() error { return runDuplicate(nil, []string{missing}) }},
		{name: "organize", run: func() error { return runOrganize(nil, []string{missing}) }},
		{name: "manifest", run: func() error { return runManifest([]string{missing}, "manifest.json") }},
	}

	for _, command := range commands {
		t.Run(command.name, func(t *testing.T) {
			err := command.run()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "cannot access directory")
		})
	}
}

func TestRunFileCommands_EmptyDirectoriesStopBeforeSummaries(t *testing.T) {
	originalDryRun := dryRun
	originalWorkers := workers
	originalNoSnapshot := noSnapshot
	t.Cleanup(func() {
		dryRun = originalDryRun
		workers = originalWorkers
		noSnapshot = originalNoSnapshot
	})

	dryRun = true
	workers = 1
	noSnapshot = true

	commands := []struct {
		name string
		run  func(string) error
	}{
		{name: "rename", run: func(root string) error { return runRename(nil, []string{root}) }},
		{name: "flatten", run: func(root string) error { return runFlatten(nil, []string{root}) }},
		{name: "duplicate", run: func(root string) error { return runDuplicate(nil, []string{root}) }},
		{name: "organize", run: func(root string) error { return runOrganize(nil, []string{root}) }},
	}

	for _, command := range commands {
		t.Run(command.name, func(t *testing.T) {
			output := captureStdout(t, func() {
				require.NoError(t, command.run(t.TempDir()))
			})

			assert.Contains(t, output, "No files to process.\n")
			assert.NotContains(t, output, "=== Summary ===")
		})
	}
}

func TestBuildCommandsMetadataAndFlags(t *testing.T) {
	rootCmd := buildRootCommand()
	assert.Equal(t, "file-dedup", rootCmd.Use)
	assert.NotNil(t, rootCmd.PersistentFlags().Lookup("dry-run"))
	assert.NotNil(t, rootCmd.PersistentFlags().Lookup("verbose"))
	assert.NotNil(t, rootCmd.PersistentFlags().Lookup("workers"))
	assert.NotNil(t, rootCmd.PersistentFlags().Lookup("no-snapshot"))

	cliCmd := buildCLICommand()
	commandNames := make([]string, 0, len(cliCmd.Commands()))
	for _, cmd := range cliCmd.Commands() {
		commandNames = append(commandNames, cmd.Name())
	}
	assert.ElementsMatch(t,
		[]string{"duplicate", "flatten", "manifest", "organize", "purge", "rename", "undo", "unzip"},
		commandNames,
	)

	renameCmd := buildRenameCommand()
	assert.Equal(t, "rename [path]", renameCmd.Use)
	assert.NotNil(t, renameCmd.RunE)
	require.NoError(t, renameCmd.Args(renameCmd, []string{"root"}))
	require.Error(t, renameCmd.Args(renameCmd, nil))

	manifestCmd := buildManifestCommand()
	assert.Equal(t, "manifest [path]", manifestCmd.Use)
	flag := manifestCmd.Flags().Lookup("output")
	require.NotNil(t, flag)
	assert.Equal(t, "manifest.json", flag.DefValue)
	require.NoError(t, manifestCmd.Args(manifestCmd, []string{"root"}))
	require.Error(t, manifestCmd.Args(manifestCmd, []string{"a", "b"}))

	purgeCmd := buildPurgeCommand()
	assert.Equal(t, "purge [path]", purgeCmd.Use)
	assert.NotNil(t, purgeCmd.Flags().Lookup("run"))
	assert.NotNil(t, purgeCmd.Flags().Lookup("older-than"))
	assert.NotNil(t, purgeCmd.Flags().Lookup("all"))
	assert.NotNil(t, purgeCmd.Flags().Lookup("force"))
	require.NoError(t, purgeCmd.Args(purgeCmd, []string{"root"}))
	require.Error(t, purgeCmd.Args(purgeCmd, nil))

	undoCmd := buildUndoCommand()
	assert.Equal(t, "undo [path]", undoCmd.Use)
	assert.NotNil(t, undoCmd.Flags().Lookup("run"))
	require.NoError(t, undoCmd.Args(undoCmd, []string{"root"}))
	require.Error(t, undoCmd.Args(undoCmd, nil))
}

func TestRunPurgeValidationErrors(t *testing.T) {
	originalDryRun := dryRun
	originalRunID := purgeRunID
	originalOlder := purgeOlderStr
	originalAll := purgeAll
	originalForce := purgeForce
	t.Cleanup(func() {
		dryRun = originalDryRun
		purgeRunID = originalRunID
		purgeOlderStr = originalOlder
		purgeAll = originalAll
		purgeForce = originalForce
	})

	dryRun = false
	purgeRunID = ""
	purgeOlderStr = "bad"
	purgeAll = false
	purgeForce = false
	err := runPurge(nil, []string{t.TempDir()})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid duration")

	purgeOlderStr = ""
	err = runPurge(nil, []string{t.TempDir()})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one of")

	purgeAll = true
	err = runPurge(nil, []string{t.TempDir()})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--all requires --force")
}

func TestRunPurgeValidationAllowsExplicitFilters(t *testing.T) {
	originalDryRun := dryRun
	originalRunID := purgeRunID
	originalOlder := purgeOlderStr
	originalAll := purgeAll
	originalForce := purgeForce
	t.Cleanup(func() {
		dryRun = originalDryRun
		purgeRunID = originalRunID
		purgeOlderStr = originalOlder
		purgeAll = originalAll
		purgeForce = originalForce
	})

	tests := []struct {
		name     string
		dryRun   bool
		runID    string
		older    string
		all      bool
		force    bool
		contains string
	}{
		{name: "run id", runID: "missing-run", contains: "No trash runs found."},
		{name: "older than", older: "1h", contains: "No trash runs found."},
		{name: "all with force", all: true, force: true, contains: "No trash runs found."},
		{name: "all dry run without force", dryRun: true, all: true, contains: "No trash runs found."},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dryRun = tc.dryRun
			purgeRunID = tc.runID
			purgeOlderStr = tc.older
			purgeAll = tc.all
			purgeForce = tc.force

			output := captureStdout(t, func() {
				require.NoError(t, runPurge(nil, []string{t.TempDir()}))
			})
			assert.Contains(t, output, tc.contains)
		})
	}
}

func TestRunPurge_DryRunWithNoTrashRuns(t *testing.T) {
	originalDryRun := dryRun
	originalRunID := purgeRunID
	originalOlder := purgeOlderStr
	originalAll := purgeAll
	originalForce := purgeForce
	t.Cleanup(func() {
		dryRun = originalDryRun
		purgeRunID = originalRunID
		purgeOlderStr = originalOlder
		purgeAll = originalAll
		purgeForce = originalForce
	})

	dryRun = true
	purgeRunID = ""
	purgeOlderStr = ""
	purgeAll = false
	purgeForce = false

	output := captureStdout(t, func() {
		require.NoError(t, runPurge(nil, []string{t.TempDir()}))
	})

	assert.Contains(t, output, "Command: PURGE\n")
	assert.Contains(t, output, "No trash runs found.\n")
}

func TestRunUndo_DryRunPrintsJournalOperationsAndSummary(t *testing.T) {
	originalDryRun := dryRun
	originalVerbose := verbose
	originalRunID := undoRunID
	t.Cleanup(func() {
		dryRun = originalDryRun
		verbose = originalVerbose
		undoRunID = originalRunID
	})

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "a.txt"), "same")
	writeTestFile(t, filepath.Join(root, "b.txt"), "same")

	duplicateExec, err := usecase.New(usecase.Options{NoSnapshot: true}).RunDuplicate(usecase.DuplicateRequest{
		TargetDir: root,
		DryRun:    false,
		Workers:   1,
	})
	require.NoError(t, err)
	require.NotEmpty(t, duplicateExec.JournalPath)

	dryRun = true
	verbose = true
	undoRunID = ""

	output := captureStdout(t, func() {
		require.NoError(t, runUndo(nil, []string{root}))
	})

	assert.Contains(t, output, "=== DRY RUN - no changes will be made ===\n\n")
	assert.Contains(t, output, "Command: UNDO\n")
	assert.Contains(t, output, "Journal: "+duplicateExec.JournalPath+"\n")
	assert.Contains(t, output, "Run ID:  duplicate-")
	assert.Contains(t, output, "\n\nRESTORE: ")
	assert.Contains(t, output, "RESTORE: ")
	assert.Contains(t, output, "=== Summary ===\n")
	assert.Contains(t, output, "Restored:  1\n")
	assert.Contains(t, output, "Reversed:  0\n")
	assert.Contains(t, output, "Skipped:   0\n")
	assert.Contains(t, output, "Errors:    0\n")
	assert.Contains(t, output, "Run without --dry-run to apply changes.\n")
}

func newStoppedProgressReporter(label string) *progressReporter {
	return &progressReporter{
		label:     label,
		stage:     label,
		startTime: time.Now().Add(-2 * time.Second),
	}
}

func captureStdout(t *testing.T, run func()) string {
	t.Helper()

	return captureOutput(t, &os.Stdout, run)
}

func captureStderr(t *testing.T, run func()) string {
	t.Helper()

	return captureOutput(t, &os.Stderr, run)
}

func captureOutput(t *testing.T, target **os.File, run func()) string {
	t.Helper()

	original := *target
	readFile, writeFile, err := os.Pipe()
	require.NoError(t, err)

	*target = writeFile
	run()
	require.NoError(t, writeFile.Close())
	*target = original

	var output bytes.Buffer
	_, err = io.Copy(&output, readFile)
	require.NoError(t, err)
	require.NoError(t, readFile.Close())

	return strings.ReplaceAll(output.String(), "\r\n", "\n")
}

func printlnForTest(line string) {
	fmtLine := line + "\n"
	_, _ = os.Stdout.WriteString(fmtLine)
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o644))
}

func writeZipForCommandTest(t *testing.T, path string, files map[string]string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	zipFile, err := os.Create(path)
	require.NoError(t, err)
	zw := zip.NewWriter(zipFile)
	for name, contents := range files {
		writer, createErr := zw.Create(name)
		require.NoError(t, createErr)
		_, writeErr := writer.Write([]byte(contents))
		require.NoError(t, writeErr)
	}
	require.NoError(t, zw.Close())
	require.NoError(t, zipFile.Close())
}

func printRenameOperationForTest(op renamer.RenameOperation) {
	if op.Skipped {
		printlnForTest("SKIP: " + op.OriginalName + " (" + op.SkipReason + ")")
		return
	}
	if op.Error != nil {
		printlnForTest("ERROR: " + op.OriginalName + " -> " + op.NewName + ": " + op.Error.Error())
		return
	}
	printlnForTest("RENAME: " + op.OriginalPath)
	printlnForTest("    TO: " + op.NewPath)
}
