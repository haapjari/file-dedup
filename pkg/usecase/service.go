// Package usecase provides application-level orchestration for CLI workflows.
package usecase

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"file-dedup/pkg/collector"
	"file-dedup/pkg/deduplicator"
	"file-dedup/pkg/filelock"
	"file-dedup/pkg/flattener"
	"file-dedup/pkg/hasher"
	"file-dedup/pkg/journal"
	"file-dedup/pkg/manifest"
	"file-dedup/pkg/metadata"
	"file-dedup/pkg/organizer"
	"file-dedup/pkg/progress"
	"file-dedup/pkg/renamer"
	"file-dedup/pkg/safepath"
	"file-dedup/pkg/trash"
	"file-dedup/pkg/unzipper"
)

// Options configures a Service.
type Options struct {
	SkipFiles  []string
	SkipDirs   []string
	NoSnapshot bool
}

// ProgressCallback receives workflow stage progress updates.
// Stage names are command-specific and intended for user-facing progress output.
type ProgressCallback func(stage string, processed, total int)

// Service orchestrates command workflows without Cobra dependencies.
type Service struct {
	skipFiles  []string
	skipDirs   []string
	noSnapshot bool
}

// New creates a use-case service.
func New(opts Options) *Service {
	return &Service{
		skipFiles:  append([]string(nil), opts.SkipFiles...),
		skipDirs:   append([]string(nil), opts.SkipDirs...),
		noSnapshot: opts.NoSnapshot,
	}
}

// RenameRequest contains inputs for the rename workflow.
type RenameRequest struct {
	TargetDir  string
	DryRun     bool
	OnProgress ProgressCallback
}

// RenameExecution contains rename workflow outputs.
type RenameExecution struct {
	RootDir         string
	FileCount       int
	CollectDuration time.Duration
	Result          renamer.Result
	SnapshotPath    string
	JournalPath     string
}

// FlattenRequest contains inputs for the flatten workflow.
type FlattenRequest struct {
	TargetDir  string
	DryRun     bool
	Workers    int
	OnProgress ProgressCallback
}

// FlattenExecution contains flatten workflow outputs.
type FlattenExecution struct {
	RootDir         string
	FileCount       int
	CollectDuration time.Duration
	Result          flattener.Result
	SnapshotPath    string
	JournalPath     string
}

// DuplicateRequest contains inputs for the duplicate workflow.
type DuplicateRequest struct {
	TargetDir  string
	DryRun     bool
	Workers    int
	OnProgress ProgressCallback
}

// DuplicateExecution contains duplicate workflow outputs.
type DuplicateExecution struct {
	RootDir         string
	FileCount       int
	CollectDuration time.Duration
	Result          deduplicator.Result
	SnapshotPath    string
	JournalPath     string
}

// UnzipRequest contains inputs for the unzip workflow.
type UnzipRequest struct {
	TargetDir  string
	DryRun     bool
	OnProgress ProgressCallback
}

// UnzipExecution contains unzip workflow outputs.
type UnzipExecution struct {
	RootDir         string
	FileCount       int
	CollectDuration time.Duration
	Result          unzipper.Result
	SnapshotPath    string
	JournalPath     string
}

// ManifestRequest contains inputs for the manifest workflow.
type ManifestRequest struct {
	TargetDir  string
	OutputPath string
	Workers    int
	OnProgress ProgressCallback
}

// ManifestExecution contains manifest workflow outputs.
type ManifestExecution struct {
	RootDir    string
	Duration   time.Duration
	Manifest   *manifest.Manifest
	OutputPath string
	Workers    int
}

// OrganizeRequest contains inputs for the organize workflow.
type OrganizeRequest struct {
	TargetDir  string
	DryRun     bool
	OnProgress ProgressCallback
}

// OrganizeExecution contains organize workflow outputs.
type OrganizeExecution struct {
	RootDir         string
	FileCount       int
	CollectDuration time.Duration
	Result          organizer.Result
	SnapshotPath    string
	JournalPath     string
}

// WorkflowMeta contains the common metadata fields shared by all file workflow executions.
type WorkflowMeta struct {
	RootDir         string
	FileCount       int
	CollectDuration time.Duration
	SnapshotPath    string
	JournalPath     string
}

// Meta returns the common workflow metadata for rename executions.
func (e RenameExecution) Meta() WorkflowMeta {
	return WorkflowMeta{
		RootDir: e.RootDir, FileCount: e.FileCount,
		CollectDuration: e.CollectDuration, SnapshotPath: e.SnapshotPath, JournalPath: e.JournalPath,
	}
}

// Meta returns the common workflow metadata for flatten executions.
func (e FlattenExecution) Meta() WorkflowMeta {
	return WorkflowMeta{
		RootDir: e.RootDir, FileCount: e.FileCount,
		CollectDuration: e.CollectDuration, SnapshotPath: e.SnapshotPath, JournalPath: e.JournalPath,
	}
}

// Meta returns the common workflow metadata for duplicate executions.
func (e DuplicateExecution) Meta() WorkflowMeta {
	return WorkflowMeta{
		RootDir: e.RootDir, FileCount: e.FileCount,
		CollectDuration: e.CollectDuration, SnapshotPath: e.SnapshotPath, JournalPath: e.JournalPath,
	}
}

// Meta returns the common workflow metadata for unzip executions.
func (e UnzipExecution) Meta() WorkflowMeta {
	return WorkflowMeta{
		RootDir: e.RootDir, FileCount: e.FileCount,
		CollectDuration: e.CollectDuration, SnapshotPath: e.SnapshotPath, JournalPath: e.JournalPath,
	}
}

// Meta returns the common workflow metadata for organize executions.
func (e OrganizeExecution) Meta() WorkflowMeta {
	return WorkflowMeta{
		RootDir: e.RootDir, FileCount: e.FileCount,
		CollectDuration: e.CollectDuration, SnapshotPath: e.SnapshotPath, JournalPath: e.JournalPath,
	}
}

// RunOrganize executes the organize workflow.
func (s *Service) RunOrganize(req OrganizeRequest) (OrganizeExecution, error) {
	return runCheckedExecution(
		s,
		req.TargetDir,
		req.DryRun,
		organizeExecutor(req.DryRun, req.OnProgress),
		organizeExecutionFromWorkflow,
		"organize",
		func(execution OrganizeExecution) []organizer.MoveOperation {
			return execution.Result.Operations
		},
		func(op organizer.MoveOperation) (string, error) {
			return op.OriginalPath, op.Error
		},
		organizeJournalEntries,
	)
}

// RunRename executes the rename workflow.
func (s *Service) RunRename(req RenameRequest) (RenameExecution, error) {
	return runCheckedExecution(
		s,
		req.TargetDir,
		req.DryRun,
		renameExecutor(req.DryRun, req.OnProgress),
		renameExecutionFromWorkflow,
		"rename",
		func(execution RenameExecution) []renamer.RenameOperation {
			return execution.Result.Operations
		},
		func(op renamer.RenameOperation) (string, error) {
			return op.OriginalPath, op.Error
		},
		renameJournalEntries,
	)
}

// RunFlatten executes the flatten workflow.
func (s *Service) RunFlatten(req FlattenRequest) (FlattenExecution, error) {
	return runCheckedExecution(
		s,
		req.TargetDir,
		req.DryRun,
		flattenExecutor(req.DryRun, req.Workers, req.OnProgress),
		flattenExecutionFromWorkflow,
		"flatten",
		func(execution FlattenExecution) []flattener.MoveOperation {
			return execution.Result.Operations
		},
		func(op flattener.MoveOperation) (string, error) {
			return op.OriginalPath, op.Error
		},
		flattenJournalEntries,
	)
}

// RunDuplicate executes the duplicate workflow.
func (s *Service) RunDuplicate(req DuplicateRequest) (DuplicateExecution, error) {
	return runCheckedExecution(
		s,
		req.TargetDir,
		req.DryRun,
		duplicateExecutor(req.DryRun, req.Workers, req.OnProgress),
		duplicateExecutionFromWorkflow,
		"duplicate",
		func(execution DuplicateExecution) []deduplicator.DeleteOperation {
			return execution.Result.Operations
		},
		func(op deduplicator.DeleteOperation) (string, error) {
			return op.Path, op.Error
		},
		duplicateJournalEntries,
	)
}

// RunUnzip executes the unzip workflow.
func (s *Service) RunUnzip(req UnzipRequest) (UnzipExecution, error) {
	return runCheckedExecution(
		s,
		req.TargetDir,
		req.DryRun,
		unzipExecutor(req.DryRun, req.OnProgress),
		unzipExecutionFromWorkflow,
		"unzip",
		func(execution UnzipExecution) []unzipper.ExtractOperation {
			return execution.Result.Operations
		},
		func(op unzipper.ExtractOperation) (string, error) {
			return op.ArchivePath, op.Error
		},
		unzipJournalEntries,
	)
}

func renameExecutionFromWorkflow(workflowResult fileWorkflowResult[renamer.Result]) RenameExecution {
	return RenameExecution(workflowResult)
}

func flattenExecutionFromWorkflow(workflowResult fileWorkflowResult[flattener.Result]) FlattenExecution {
	return FlattenExecution(workflowResult)
}

func duplicateExecutionFromWorkflow(workflowResult fileWorkflowResult[deduplicator.Result]) DuplicateExecution {
	return DuplicateExecution(workflowResult)
}

func unzipExecutionFromWorkflow(workflowResult fileWorkflowResult[unzipper.Result]) UnzipExecution {
	return UnzipExecution(workflowResult)
}

func organizeExecutionFromWorkflow(workflowResult fileWorkflowResult[organizer.Result]) OrganizeExecution {
	return OrganizeExecution(workflowResult)
}

// RunManifest executes the manifest workflow.
func (s *Service) RunManifest(req ManifestRequest) (ManifestExecution, error) {
	target, err := resolveWorkflowTarget(req.TargetDir)
	if err != nil {
		return ManifestExecution{}, err
	}

	resolvedOutputPath, err := resolveManifestOutputPath(target, req.OutputPath)
	if err != nil {
		return ManifestExecution{}, err
	}

	startTime := time.Now()

	g, err := manifest.NewGeneratorWithValidator(target.validator, req.Workers)
	if err != nil {
		return ManifestExecution{}, fmt.Errorf("failed to create manifest generator: %w", err)
	}

	generatedManifest, err := g.Generate(manifest.GenerateOptions{
		SkipFiles: s.skipFileList(),
		SkipDirs:  s.skipDirList(),
		OnProgress: func(processed, total int, _ string) {
			progress.EmitStage(req.OnProgress, "hashing", processed, total)
		},
	})
	if err != nil {
		return ManifestExecution{}, fmt.Errorf("failed to generate manifest: %w", err)
	}

	if err := generatedManifest.Save(resolvedOutputPath); err != nil {
		return ManifestExecution{}, fmt.Errorf("failed to save manifest: %w", err)
	}

	return ManifestExecution{
		RootDir:    target.rootDir,
		Duration:   time.Since(startTime),
		Manifest:   generatedManifest,
		OutputPath: resolvedOutputPath,
		Workers:    req.Workers,
	}, nil
}

func (s *Service) collectFiles(rootDir string) ([]collector.FileInfo, time.Duration, error) {
	startTime := time.Now()

	c := collector.New(collector.Options{
		SkipFiles: s.skipFileList(),
		SkipDirs:  s.skipDirList(),
	})

	files, err := c.Collect(rootDir)
	if err != nil {
		return nil, 0, err
	}

	return files, time.Since(startTime), nil
}

type fileWorkflowResult[T any] struct {
	RootDir         string
	FileCount       int
	CollectDuration time.Duration
	Result          T
	SnapshotPath    string
	JournalPath     string
}

// Workflow invariant: no path is opened or mutated before validator approval.
type workflowTarget struct {
	rootDir     string
	validator   *safepath.Validator
	undoTrasher *trash.Trasher
}

func runFileWorkflow[T any](
	s *Service,
	targetDir, command string,
	dryRun bool,
	execute func(rootDir string, validator *safepath.Validator, files []collector.FileInfo) (T, error),
	toJournalEntries func(T, string) []journal.Entry,
) (fileWorkflowResult[T], error) {
	target, err := resolveWorkflowTarget(targetDir)
	if err != nil {
		return fileWorkflowResult[T]{}, err
	}

	// Acquire advisory lock to prevent concurrent file-dedup processes on the same directory.
	lock, lockErr := acquireWorkflowLock(target)
	if lockErr != nil {
		return fileWorkflowResult[T]{}, lockErr
	}
	defer lock.Close()

	files, collectDuration, err := s.collectFiles(target.rootDir)
	if err != nil {
		return fileWorkflowResult[T]{}, fmt.Errorf("failed to collect files: %w", err)
	}

	workflowResult := fileWorkflowResult[T]{
		RootDir:         target.rootDir,
		FileCount:       len(files),
		CollectDuration: collectDuration,
	}
	if len(files) == 0 {
		return workflowResult, nil
	}

	// Generate pre-operation snapshot unless disabled or in dry-run mode.
	if !s.noSnapshot && !dryRun {
		snapshotPath, snapshotErr := s.generateSnapshot(target, command)
		if snapshotErr != nil {
			return fileWorkflowResult[T]{}, fmt.Errorf("failed to generate pre-operation snapshot: %w", snapshotErr)
		}
		workflowResult.SnapshotPath = snapshotPath
	}

	operationResult, err := execute(target.rootDir, target.validator, files)
	if err != nil {
		return fileWorkflowResult[T]{}, err
	}

	workflowResult.Result = operationResult

	// Write operation journal unless in dry-run mode.
	if !dryRun && toJournalEntries != nil {
		journalPath, journalErr := writeJournal(target, command, toJournalEntries(operationResult, target.rootDir))
		if journalErr != nil {
			return fileWorkflowResult[T]{}, fmt.Errorf("failed to write operation journal: %w", journalErr)
		}
		workflowResult.JournalPath = journalPath
	}

	return workflowResult, nil
}

func runCheckedExecution[T any, E any, O any](
	s *Service,
	targetDir string,
	dryRun bool,
	execute func(rootDir string, validator *safepath.Validator, files []collector.FileInfo) (T, error),
	toExecution func(fileWorkflowResult[T]) E,
	command string,
	operations func(E) []O,
	operationData func(O) (path string, err error),
	toJournalEntries func(T, string) []journal.Entry,
) (E, error) {
	workflowResult, err := runFileWorkflow(s, targetDir, command, dryRun, execute, toJournalEntries)
	if err != nil {
		var zero E
		return zero, err
	}

	execution := toExecution(workflowResult)
	if err := failOnUnsafeOperation(operations(execution), command, operationData); err != nil {
		return execution, err
	}

	return execution, nil
}

func renameExecutor(dryRun bool, onProgress ProgressCallback) func(rootDir string, validator *safepath.Validator, files []collector.FileInfo) (renamer.Result, error) {
	return func(rootDir string, validator *safepath.Validator, files []collector.FileInfo) (renamer.Result, error) {
		trasher, err := initTrasher(rootDir, validator, "rename")
		if err != nil {
			return renamer.Result{}, fmt.Errorf("failed to initialize trash: %w", err)
		}

		r, err := renamer.NewWithValidator(validator, dryRun, trasher)
		if err != nil {
			return renamer.Result{}, fmt.Errorf("failed to create renamer: %w", err)
		}

		return r.RenameFilesWithProgress(files, func(processed, total int) {
			progress.EmitStage(onProgress, "renaming", processed, total)
		}), nil
	}
}

func flattenExecutor(dryRun bool, workers int, onProgress ProgressCallback) func(rootDir string, validator *safepath.Validator, files []collector.FileInfo) (flattener.Result, error) {
	return trashedWorkerExecutor(
		dryRun, workers, onProgress, "flatten",
		flattener.NewWithValidator,
		"failed to create flattener",
		func(f *flattener.Flattener, files []collector.FileInfo, cb func(string, int, int)) flattener.Result {
			return f.FlattenFilesWithProgress(files, cb)
		},
	)
}

func duplicateExecutor(dryRun bool, workers int, onProgress ProgressCallback) func(rootDir string, validator *safepath.Validator, files []collector.FileInfo) (deduplicator.Result, error) {
	return trashedWorkerExecutor(
		dryRun, workers, onProgress, "duplicate",
		deduplicator.NewWithValidator,
		"failed to create deduplicator",
		func(d *deduplicator.Deduplicator, files []collector.FileInfo, cb func(string, int, int)) deduplicator.Result {
			return d.FindDuplicatesWithProgress(files, cb)
		},
	)
}

// trashedWorkerExecutor creates an executor for domain packages that accept
// (validator, dryRun, workers, trasher) and produce staged progress.
func trashedWorkerExecutor[Worker any, Result any](
	dryRun bool,
	workers int,
	onProgress ProgressCallback,
	command string,
	newWorker func(*safepath.Validator, bool, int, *trash.Trasher) (Worker, error),
	createErrContext string,
	run func(Worker, []collector.FileInfo, func(string, int, int)) Result,
) func(string, *safepath.Validator, []collector.FileInfo) (Result, error) {
	return func(rootDir string, validator *safepath.Validator, files []collector.FileInfo) (Result, error) {
		trasher, err := initTrasher(rootDir, validator, command)
		if err != nil {
			var zero Result
			return zero, fmt.Errorf("failed to initialize trash: %w", err)
		}

		w, err := newWorker(validator, dryRun, workers, trasher)
		if err != nil {
			var zero Result
			return zero, fmt.Errorf("%s: %w", createErrContext, err)
		}

		return run(w, files, func(stage string, processed, total int) {
			progress.EmitStage(onProgress, stage, processed, total)
		}), nil
	}
}

func unzipExecutor(dryRun bool, onProgress ProgressCallback) func(rootDir string, validator *safepath.Validator, files []collector.FileInfo) (unzipper.Result, error) {
	return func(rootDir string, validator *safepath.Validator, files []collector.FileInfo) (unzipper.Result, error) {
		var trasher *trash.Trasher
		if !dryRun {
			var err error
			trasher, err = initTrasher(rootDir, validator, "unzip")
			if err != nil {
				return unzipper.Result{}, fmt.Errorf("failed to initialize trash: %w", err)
			}
		}

		u, err := unzipper.NewWithValidator(validator, dryRun, trasher)
		if err != nil {
			return unzipper.Result{}, fmt.Errorf("failed to create unzipper: %w", err)
		}

		return u.ExtractArchivesWithProgressRecursively(files, func(stage string, processed, total int) {
			progress.EmitStage(onProgress, stage, processed, total)
		})
	}
}

func organizeExecutor(dryRun bool, onProgress ProgressCallback) func(rootDir string, validator *safepath.Validator, files []collector.FileInfo) (organizer.Result, error) {
	return simpleExecutor(
		dryRun,
		onProgress,
		organizer.NewWithValidator,
		"failed to create organizer",
		"organizing",
		func(w *organizer.Organizer, files []collector.FileInfo, cb func(processed, total int)) organizer.Result {
			return w.OrganizeFilesWithProgress(files, cb)
		},
	)
}

func simpleExecutor[Worker any, Result any](
	dryRun bool,
	onProgress ProgressCallback,
	newWorker func(*safepath.Validator, bool) (Worker, error),
	createErrContext string,
	stageLabel string,
	run func(Worker, []collector.FileInfo, func(processed, total int)) Result,
) func(string, *safepath.Validator, []collector.FileInfo) (Result, error) {
	return func(_ string, validator *safepath.Validator, files []collector.FileInfo) (Result, error) {
		w, err := newWorker(validator, dryRun)
		if err != nil {
			var zero Result
			return zero, fmt.Errorf("%s: %w", createErrContext, err)
		}

		return run(w, files, func(processed, total int) {
			progress.EmitStage(onProgress, stageLabel, processed, total)
		}), nil
	}
}

// initTrasher creates a metadata directory and trasher for a command run.
// In dry-run mode this still initializes the trasher; the domain packages
// skip mutations themselves when dryRun is true.
func initTrasher(rootDir string, validator *safepath.Validator, command string) (*trash.Trasher, error) {
	metaDir, err := metadata.Init(rootDir, validator)
	if err != nil {
		return nil, fmt.Errorf("initialize metadata: %w", err)
	}

	runID := metaDir.RunID(command)

	trasher, err := trash.New(metaDir, runID, validator)
	if err != nil {
		return nil, fmt.Errorf("initialize trasher: %w", err)
	}

	return trasher, nil
}

func (s *Service) skipFileList() []string {
	return append([]string(nil), s.skipFiles...)
}

func (s *Service) skipDirList() []string {
	dirs := append([]string(nil), s.skipDirs...)
	// Always skip the .file-dedup metadata directory regardless of caller configuration.
	dirs = append(dirs, metadata.DirName)
	return dirs
}

// generateSnapshot creates a pre-operation manifest in .file-dedup/manifests/.
func (s *Service) generateSnapshot(target workflowTarget, command string) (string, error) {
	metaDir, err := metadata.Init(target.rootDir, target.validator)
	if err != nil {
		return "", fmt.Errorf("initialize metadata: %w", err)
	}

	runID := metaDir.RunID(command)
	snapshotPath := metaDir.ManifestPath(runID)

	// Ensure the manifests directory exists.
	mkdirErr := target.validator.SafeMkdirAll(filepath.Dir(snapshotPath))
	if mkdirErr != nil {
		return "", fmt.Errorf("create manifests directory: %w", mkdirErr)
	}

	gen, err := manifest.NewGeneratorWithValidator(target.validator, runtime.NumCPU())
	if err != nil {
		return "", fmt.Errorf("create manifest generator: %w", err)
	}

	m, err := gen.Generate(manifest.GenerateOptions{
		SkipFiles: s.skipFileList(),
		SkipDirs:  s.skipDirList(),
	})
	if err != nil {
		return "", fmt.Errorf("generate manifest: %w", err)
	}

	if saveErr := m.Save(snapshotPath); saveErr != nil {
		return "", fmt.Errorf("save manifest: %w", saveErr)
	}

	return snapshotPath, nil
}

func resolveWorkflowTarget(targetDir string) (workflowTarget, error) {
	info, err := os.Stat(targetDir)
	if err != nil {
		return workflowTarget{}, fmt.Errorf("cannot access directory: %w", err)
	}
	if !info.IsDir() {
		return workflowTarget{}, fmt.Errorf("%s is not a directory", targetDir)
	}

	validator, err := safepath.New(targetDir)
	if err != nil {
		return workflowTarget{}, fmt.Errorf("cannot create path validator: %w", err)
	}

	return workflowTarget{
		rootDir:   validator.Root(),
		validator: validator,
	}, nil
}

// acquireWorkflowLock initializes the metadata directory and acquires an
// advisory file lock to prevent concurrent file-dedup processes on the same target.
func acquireWorkflowLock(target workflowTarget) (*filelock.Lock, error) {
	metaDir, err := metadata.Init(target.rootDir, target.validator)
	if err != nil {
		return nil, fmt.Errorf("initialize metadata for lock: %w", err)
	}

	lock, lockErr := filelock.Acquire(metaDir.LockPath())
	if lockErr != nil {
		return nil, fmt.Errorf("another file-dedup process is operating on this directory: %w", lockErr)
	}

	return lock, nil
}

func resolveManifestOutputPath(target workflowTarget, outputPath string) (string, error) {
	resolvedPath, err := target.validator.ResolveSafePath(target.rootDir, outputPath)
	if err != nil {
		return "", fmt.Errorf("manifest output path must stay within target directory: %w", err)
	}

	if err := target.validator.ValidatePathForWrite(resolvedPath); err != nil {
		return "", fmt.Errorf("manifest output path must stay within target directory: %w", err)
	}

	return resolvedPath, nil
}

func failOnUnsafeOperation[T any](operations []T, command string, operationData func(T) (path string, err error)) error {
	for _, op := range operations {
		path, err := operationData(op)
		if isUnsafePathError(err) {
			return fmt.Errorf("unsafe path detected in %s command for %q: %w", command, path, err)
		}
	}

	return nil
}

func isUnsafePathError(err error) bool {
	if err == nil {
		return false
	}

	return errors.Is(err, safepath.ErrPathEscape) || errors.Is(err, safepath.ErrSymlinkEscape)
}

// writeJournal creates a journal file in .file-dedup/journal/ and writes entries to it.
// Each operation is written as a two-phase entry: first an intent entry
// (Success=false) followed by a confirmation entry (Success=true).
// Note: entries are written after operations have completed, not before.
// The journal serves undo support and audit logging. It does not provide
// crash recovery for in-flight operations.
func writeJournal(target workflowTarget, command string, entries []journal.Entry) (string, error) {
	if len(entries) == 0 {
		return "", nil
	}

	metaDir, initErr := metadata.Init(target.rootDir, target.validator)
	if initErr != nil {
		return "", fmt.Errorf("initialize metadata: %w", initErr)
	}

	runID := metaDir.RunID(command)
	journalPath := metaDir.JournalPath(runID)

	mkdirErr := target.validator.SafeMkdirAll(filepath.Dir(journalPath))
	if mkdirErr != nil {
		return "", fmt.Errorf("create journal directory: %w", mkdirErr)
	}

	writer, writerErr := journal.NewWriter(journalPath)
	if writerErr != nil {
		return "", fmt.Errorf("create journal writer: %w", writerErr)
	}
	defer writer.Close()

	for i := range entries {
		// Write intent entry (Success=false).
		intent := entries[i]
		intent.Success = false
		if logErr := writer.Log(intent); logErr != nil {
			return journalPath, fmt.Errorf("write journal intent: %w", logErr)
		}

		// Write confirmation entry (Success=true).
		if logErr := writer.Log(entries[i]); logErr != nil {
			return journalPath, fmt.Errorf("write journal confirmation: %w", logErr)
		}
	}

	return journalPath, nil
}

// relPath computes a relative path from rootDir, returning the absolute path
// on error as a fallback.
func relPath(rootDir, absPath string) string {
	rel, relErr := filepath.Rel(rootDir, absPath)
	if relErr != nil {
		return absPath
	}
	return rel
}

// filterConfirmedEntries returns only entries with Success=true, filtering out
// intent entries from the two-phase journal format.
func filterConfirmedEntries(entries []journal.Entry) []journal.Entry {
	confirmed := make([]journal.Entry, 0, len(entries)/2)
	for i := range entries {
		if entries[i].Success {
			confirmed = append(confirmed, entries[i])
		}
	}
	return confirmed
}

// renameJournalEntries converts rename operations to journal entries.
func renameJournalEntries(result renamer.Result, rootDir string) []journal.Entry {
	var entries []journal.Entry
	for i := range result.Operations {
		op := &result.Operations[i]
		if op.Error != nil || op.Skipped {
			continue
		}
		if op.NewPath != "" && op.NewPath != op.OriginalPath {
			entries = append(entries, journal.Entry{
				Type:    "rename",
				Source:  relPath(rootDir, op.OriginalPath),
				Dest:    relPath(rootDir, op.NewPath),
				Success: true,
			})
		}
		if op.Deleted && op.TrashedTo != "" {
			entries = append(entries, journal.Entry{
				Type:    "trash",
				Source:  relPath(rootDir, op.OriginalPath),
				Dest:    relPath(rootDir, op.TrashedTo),
				Success: true,
			})
		}
	}
	return entries
}

// flattenJournalEntries converts flatten operations to journal entries.
func flattenJournalEntries(result flattener.Result, rootDir string) []journal.Entry {
	var entries []journal.Entry
	for _, op := range result.Operations {
		if op.Error != nil || op.Skipped {
			continue
		}
		if op.Duplicate && op.TrashedTo != "" {
			entries = append(entries, journal.Entry{
				Type:    "trash",
				Source:  relPath(rootDir, op.OriginalPath),
				Dest:    relPath(rootDir, op.TrashedTo),
				Hash:    op.Hash,
				Success: true,
			})
			continue
		}
		if op.NewPath != "" && op.NewPath != op.OriginalPath {
			entries = append(entries, journal.Entry{
				Type:    "rename",
				Source:  relPath(rootDir, op.OriginalPath),
				Dest:    relPath(rootDir, op.NewPath),
				Success: true,
			})
		}
	}
	return entries
}

// duplicateJournalEntries converts duplicate operations to journal entries.
func duplicateJournalEntries(result deduplicator.Result, rootDir string) []journal.Entry {
	var entries []journal.Entry
	for _, op := range result.Operations {
		if op.Error != nil || op.Skipped {
			continue
		}
		if op.TrashedTo != "" {
			entries = append(entries, journal.Entry{
				Type:    "trash",
				Source:  relPath(rootDir, op.Path),
				Dest:    relPath(rootDir, op.TrashedTo),
				Hash:    op.Hash,
				Success: true,
			})
		}
	}
	return entries
}

// unzipJournalEntries converts unzip operations to journal entries.
func unzipJournalEntries(result unzipper.Result, rootDir string) []journal.Entry {
	var entries []journal.Entry
	for i := range result.Operations {
		op := &result.Operations[i]
		if op.Error != nil || op.Skipped {
			continue
		}
		for _, replaced := range op.ReplacedFiles {
			entries = append(entries, journal.Entry{
				Type:    "replace",
				Source:  relPath(rootDir, replaced.OriginalPath),
				Dest:    relPath(rootDir, replaced.TrashedTo),
				Hash:    replaced.Hash,
				Success: true,
			})
		}
		if op.ExtractionComplete {
			entries = append(entries, journal.Entry{
				Type:    "extract",
				Source:  relPath(rootDir, op.ArchivePath),
				Success: true,
			})
		}
		if op.DeletedArchive && op.TrashedTo != "" {
			entries = append(entries, journal.Entry{
				Type:    "trash",
				Source:  relPath(rootDir, op.ArchivePath),
				Dest:    relPath(rootDir, op.TrashedTo),
				Success: true,
			})
		}
	}
	return entries
}

// organizeJournalEntries converts organize operations to journal entries.
func organizeJournalEntries(result organizer.Result, rootDir string) []journal.Entry {
	var entries []journal.Entry
	for _, op := range result.Operations {
		if op.Error != nil || op.Skipped {
			continue
		}
		if op.NewPath != "" && op.NewPath != op.OriginalPath {
			entries = append(entries, journal.Entry{
				Type:    "rename",
				Source:  relPath(rootDir, op.OriginalPath),
				Dest:    relPath(rootDir, op.NewPath),
				Success: true,
			})
		}
	}
	return entries
}

// UndoRequest contains inputs for the undo workflow.
type UndoRequest struct {
	TargetDir  string
	RunID      string // empty = most recent journal
	DryRun     bool
	OnProgress ProgressCallback
}

// Undo action constants for UndoOperation.Action.
const (
	undoActionRestore       = "restore"
	undoActionReverseRename = "reverse-rename"
	undoActionSkip          = "skip"
)

// UndoOperation describes a single undo step.
type UndoOperation struct {
	EntryType  string // original journal entry type
	Source     string // original source path (relative)
	Dest       string // original dest path (relative)
	Action     string // undoActionRestore, undoActionReverseRename, undoActionSkip
	SkipReason string // why this entry was skipped
	Error      error
}

// UndoExecution contains undo workflow outputs.
type UndoExecution struct {
	RootDir       string
	JournalPath   string
	RunID         string
	Operations    []UndoOperation
	RestoredCount int
	ReversedCount int
	SkippedCount  int
	ErrorCount    int
	DryRun        bool
}

// RunUndo reverses the most recent (or specified) operation using its journal.
func (s *Service) RunUndo(req UndoRequest) (UndoExecution, error) {
	target, err := resolveWorkflowTarget(req.TargetDir)
	if err != nil {
		return UndoExecution{}, err
	}

	lock, lockErr := acquireWorkflowLock(target)
	if lockErr != nil {
		return UndoExecution{}, lockErr
	}
	defer lock.Close()

	metaDir, initErr := metadata.Init(target.rootDir, target.validator)
	if initErr != nil {
		return UndoExecution{}, fmt.Errorf("initialize metadata: %w", initErr)
	}

	journalPath, findErr := findJournal(metaDir, req.RunID)
	if findErr != nil {
		return UndoExecution{}, findErr
	}

	reader := journal.NewReader(journalPath)
	entries, readErr := reader.EntriesReverse()
	if readErr != nil {
		return UndoExecution{}, fmt.Errorf("read journal: %w", readErr)
	}

	// Filter out intent entries (Success=false) — they exist as part of the
	// two-phase journal format, not for undo processing.
	confirmed := filterConfirmedEntries(entries)

	runID := extractRunID(journalPath)

	target, err = prepareUndoTarget(target, req.DryRun)
	if err != nil {
		return UndoExecution{}, err
	}

	exec := UndoExecution{
		RootDir:     target.rootDir,
		JournalPath: journalPath,
		RunID:       runID,
		DryRun:      req.DryRun,
	}

	for i, entry := range confirmed {
		op := undoEntry(target, entry, req.DryRun)
		exec.Operations = append(exec.Operations, op)

		switch {
		case op.Error != nil:
			exec.ErrorCount++
		case op.Action == undoActionSkip:
			exec.SkippedCount++
		case op.Action == undoActionRestore:
			exec.RestoredCount++
		case op.Action == undoActionReverseRename:
			exec.ReversedCount++
		}

		progress.EmitStage(req.OnProgress, "undoing", i+1, len(confirmed))
	}

	// Mark journal as rolled back by renaming to .rolled-back.jsonl.
	if !req.DryRun && len(entries) > 0 {
		rolledBackPath := strings.TrimSuffix(journalPath, ".jsonl") + ".rolled-back.jsonl"
		if renameErr := os.Rename(journalPath, rolledBackPath); renameErr != nil {
			return exec, fmt.Errorf("mark journal as rolled back: %w", renameErr)
		}
	}

	return exec, nil
}

// undoEntry reverses a single journal entry.
func undoEntry(target workflowTarget, entry journal.Entry, dryRun bool) UndoOperation {
	if !entry.Success {
		return UndoOperation{
			EntryType:  entry.Type,
			Source:     entry.Source,
			Dest:       entry.Dest,
			Action:     undoActionSkip,
			SkipReason: "original operation was not successful",
		}
	}

	switch entry.Type {
	case "trash":
		return undoTrash(target, entry, dryRun)
	case "replace":
		return undoReplace(target, entry, dryRun)
	case "rename":
		return undoRename(target, entry, dryRun)
	case "extract":
		return UndoOperation{
			EntryType:  entry.Type,
			Source:     entry.Source,
			Action:     undoActionSkip,
			SkipReason: "extract operations cannot be automatically undone",
		}
	default:
		return UndoOperation{
			EntryType:  entry.Type,
			Source:     entry.Source,
			Dest:       entry.Dest,
			Action:     undoActionSkip,
			SkipReason: fmt.Sprintf("unknown entry type %q", entry.Type),
		}
	}
}

// verifyHashBeforeUndo checks whether the file at path still matches the
// expected hash. Returns a skip reason and true if the hash does not match,
// indicating the undo step should be skipped. When expectedHash is empty
// (e.g. rename/organize entries that don't record hashes), verification is
// skipped and the function returns ("", false).
func verifyHashBeforeUndo(path, expectedHash string) (reason string, changed bool) {
	if expectedHash == "" {
		return "", false
	}

	h := hasher.New()
	currentHash, err := h.ComputeHash(path)
	if err != nil {
		return "cannot verify content: " + err.Error(), true
	}

	if currentHash != expectedHash {
		return "content changed since original operation (hash mismatch)", true
	}

	return "", false
}

// undoMove is a shared helper for undoTrash and undoRename. It verifies the
// file at fromAbs exists with correct content, then safely renames it to toAbs.
// The action string (e.g. "restore", "reverse-rename") and notFoundMsg are
// customized per caller.
func undoMove(
	target workflowTarget,
	entry journal.Entry,
	dryRun bool,
	fromAbs, toAbs, action, notFoundMsg string,
) UndoOperation {
	base := UndoOperation{
		EntryType: entry.Type,
		Source:    entry.Source,
		Dest:      entry.Dest,
		Action:    action,
	}

	// Verify the source file still exists.
	if _, statErr := os.Lstat(fromAbs); statErr != nil {
		base.Action = undoActionSkip
		base.SkipReason = notFoundMsg
		return base
	}

	// Verify content integrity if hash is available.
	if reason, changed := verifyHashBeforeUndo(fromAbs, entry.Hash); changed {
		base.Action = undoActionSkip
		base.SkipReason = reason
		return base
	}

	if dryRun {
		return base
	}

	// Create parent directory for the destination.
	parentDir := filepath.Dir(toAbs)
	if mkdirErr := target.validator.SafeMkdirAll(parentDir); mkdirErr != nil {
		base.Error = fmt.Errorf("create parent directory: %w", mkdirErr)
		return base
	}

	if renameErr := target.validator.SafeRename(fromAbs, toAbs); renameErr != nil {
		base.Error = fmt.Errorf("%s: %w", action, renameErr)
		return base
	}

	return base
}

// undoTrash restores a trashed file back to its original location.
func undoTrash(target workflowTarget, entry journal.Entry, dryRun bool) UndoOperation {
	trashedAbs := filepath.Join(target.rootDir, entry.Dest)
	sourceAbs := filepath.Join(target.rootDir, entry.Source)

	return undoMove(target, entry, dryRun, trashedAbs, sourceAbs, undoActionRestore,
		"trashed file not found: "+entry.Dest)
}

// undoReplace restores a file that was replaced during unzip extraction.
// If the destination path currently exists, it is first moved to undo trash
// so the original pre-extraction file can be restored without data loss.
func undoReplace(target workflowTarget, entry journal.Entry, dryRun bool) UndoOperation {
	trashedAbs := filepath.Join(target.rootDir, entry.Dest)
	sourceAbs := filepath.Join(target.rootDir, entry.Source)

	base := UndoOperation{
		EntryType: entry.Type,
		Source:    entry.Source,
		Dest:      entry.Dest,
		Action:    undoActionRestore,
	}

	if _, statErr := os.Lstat(trashedAbs); statErr != nil {
		base.Action = undoActionSkip
		base.SkipReason = "replaced file backup not found: " + entry.Dest
		return base
	}

	if reason, changed := verifyHashBeforeUndo(trashedAbs, entry.Hash); changed {
		base.Action = undoActionSkip
		base.SkipReason = reason
		return base
	}

	if dryRun {
		return base
	}

	skipReason, backupErr := backupUndoReplaceDestination(target, sourceAbs, entry.Source)
	if backupErr != nil {
		base.Error = backupErr
		return base
	}
	if skipReason != "" {
		base.Action = undoActionSkip
		base.SkipReason = skipReason
		return base
	}

	parentDir := filepath.Dir(sourceAbs)
	if mkdirErr := target.validator.SafeMkdirAll(parentDir); mkdirErr != nil {
		base.Error = fmt.Errorf("create parent directory: %w", mkdirErr)
		return base
	}

	if renameErr := target.validator.SafeRename(trashedAbs, sourceAbs); renameErr != nil {
		base.Error = fmt.Errorf("restore: %w", renameErr)
		return base
	}

	return base
}

func prepareUndoTarget(target workflowTarget, dryRun bool) (workflowTarget, error) {
	if dryRun {
		return target, nil
	}

	undoTrasher, undoTrashErr := initTrasher(target.rootDir, target.validator, "undo")
	if undoTrashErr != nil {
		return workflowTarget{}, fmt.Errorf("failed to initialize undo trash: %w", undoTrashErr)
	}
	target.undoTrasher = undoTrasher

	return target, nil
}

func backupUndoReplaceDestination(
	target workflowTarget,
	sourceAbs, sourceRel string,
) (skipReason string, err error) {
	sourceInfo, statErr := os.Lstat(sourceAbs)
	if os.IsNotExist(statErr) {
		return "", nil
	}
	if statErr != nil {
		return "", fmt.Errorf("check restore destination: %w", statErr)
	}

	if sourceInfo.IsDir() {
		return "cannot restore over existing directory: " + sourceRel, nil
	}

	if target.undoTrasher == nil {
		return "", errors.New("backup existing destination before restore: undo trasher unavailable")
	}

	if trashErr := target.undoTrasher.Trash(sourceAbs); trashErr != nil {
		return "", fmt.Errorf("backup existing destination before restore: %w", trashErr)
	}

	return "", nil
}

// undoRename reverses a rename by moving the file from dest back to source.
func undoRename(target workflowTarget, entry journal.Entry, dryRun bool) UndoOperation {
	destAbs := filepath.Join(target.rootDir, entry.Dest)
	sourceAbs := filepath.Join(target.rootDir, entry.Source)

	return undoMove(target, entry, dryRun, destAbs, sourceAbs, undoActionReverseRename,
		"file not found at dest: "+entry.Dest)
}

// findJournal locates a journal file by run ID or finds the most recent one.
func findJournal(metaDir *metadata.Dir, runID string) (string, error) {
	if runID != "" {
		journalPath := metaDir.JournalPath(runID)
		if _, statErr := os.Stat(journalPath); statErr != nil {
			return "", fmt.Errorf("journal not found for run %q: %w", runID, statErr)
		}
		return journalPath, nil
	}

	return findLatestJournal(metaDir)
}

// findLatestJournal finds the most recent active journal file.
func findLatestJournal(metaDir *metadata.Dir) (string, error) {
	journalDir := filepath.Join(metaDir.Root(), "journal")

	dirEntries, readErr := os.ReadDir(journalDir)
	if readErr != nil {
		return "", fmt.Errorf("no journals found: %w", readErr)
	}

	var activeJournals []string
	for _, entry := range dirEntries {
		name := entry.Name()
		if strings.HasSuffix(name, ".jsonl") && !strings.HasSuffix(name, ".rolled-back.jsonl") {
			activeJournals = append(activeJournals, name)
		}
	}

	if len(activeJournals) == 0 {
		return "", fmt.Errorf("no active journals found in %s", journalDir)
	}

	sort.Strings(activeJournals)
	latestName := activeJournals[len(activeJournals)-1]

	return filepath.Join(journalDir, latestName), nil
}

// extractRunID extracts the run ID from a journal file path.
// For example, ".file-dedup/journal/duplicate-20260208T143022.jsonl" returns "duplicate-20260208T143022".
func extractRunID(journalPath string) string {
	base := filepath.Base(journalPath)
	return strings.TrimSuffix(base, ".jsonl")
}

// PurgeRequest contains inputs for the purge workflow.
type PurgeRequest struct {
	TargetDir  string
	RunID      string        // purge a specific run's trash
	OlderThan  time.Duration // only purge runs older than this duration
	All        bool          // purge all trash runs
	DryRun     bool
	OnProgress ProgressCallback
}

// TrashRunInfo describes a single trash run's metadata for display.
type TrashRunInfo struct {
	RunID     string
	Path      string
	FileCount int
	TotalSize int64
	Age       time.Duration
	ModTime   time.Time
}

// PurgeOperation describes the result of purging a single trash run.
type PurgeOperation struct {
	RunID     string
	Path      string
	FileCount int
	TotalSize int64
	Purged    bool
	Error     error
}

// PurgeExecution contains purge workflow outputs.
type PurgeExecution struct {
	RootDir     string
	Runs        []TrashRunInfo
	Operations  []PurgeOperation
	PurgedCount int
	PurgedSize  int64
	ErrorCount  int
	DryRun      bool
}

// RunPurge permanently deletes trashed files based on filter criteria.
func (s *Service) RunPurge(req PurgeRequest) (PurgeExecution, error) {
	target, err := resolveWorkflowTarget(req.TargetDir)
	if err != nil {
		return PurgeExecution{}, err
	}

	lock, lockErr := acquireWorkflowLock(target)
	if lockErr != nil {
		return PurgeExecution{}, lockErr
	}
	defer lock.Close()

	metaDir, initErr := metadata.Init(target.rootDir, target.validator)
	if initErr != nil {
		return PurgeExecution{}, fmt.Errorf("initialize metadata: %w", initErr)
	}

	runs, listErr := listTrashRuns(metaDir)
	if listErr != nil {
		return PurgeExecution{}, listErr
	}

	exec := PurgeExecution{
		RootDir: target.rootDir,
		Runs:    runs,
		DryRun:  req.DryRun,
	}

	filtered := filterTrashRuns(runs, req)
	for i, run := range filtered {
		op := purgeRun(run, req.DryRun)
		exec.Operations = append(exec.Operations, op)

		if op.Error != nil {
			exec.ErrorCount++
		} else if op.Purged {
			exec.PurgedCount++
			exec.PurgedSize += op.TotalSize
		}

		progress.EmitStage(req.OnProgress, "purging", i+1, len(filtered))
	}

	return exec, nil
}

// listTrashRuns enumerates all trash run directories under .file-dedup/trash/.
func listTrashRuns(metaDir *metadata.Dir) ([]TrashRunInfo, error) {
	trashRoot := filepath.Join(metaDir.Root(), "trash")

	dirEntries, readErr := os.ReadDir(trashRoot)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return nil, nil
		}
		return nil, fmt.Errorf("read trash directory: %w", readErr)
	}

	now := time.Now()
	var runs []TrashRunInfo
	for _, entry := range dirEntries {
		if !entry.IsDir() {
			continue
		}

		runPath := filepath.Join(trashRoot, entry.Name())
		fileCount, totalSize := walkTrashDir(runPath)

		info, infoErr := entry.Info()
		var modTime time.Time
		if infoErr == nil {
			modTime = info.ModTime()
		}

		runs = append(runs, TrashRunInfo{
			RunID:     entry.Name(),
			Path:      runPath,
			FileCount: fileCount,
			TotalSize: totalSize,
			Age:       now.Sub(modTime),
			ModTime:   modTime,
		})
	}

	sort.Slice(runs, func(i, j int) bool {
		return runs[i].RunID < runs[j].RunID
	})

	return runs, nil
}

// walkTrashDir counts files and total size in a trash run directory.
func walkTrashDir(dirPath string) (fileCount int, totalSize int64) {
	walkErr := filepath.Walk(dirPath, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		fileCount++
		totalSize += info.Size()
		return nil
	})
	if walkErr != nil {
		return 0, 0
	}
	return fileCount, totalSize
}

// filterTrashRuns selects trash runs matching the purge request criteria.
func filterTrashRuns(runs []TrashRunInfo, req PurgeRequest) []TrashRunInfo {
	if req.RunID != "" {
		for i := range runs {
			if runs[i].RunID == req.RunID {
				return []TrashRunInfo{runs[i]}
			}
		}
		return nil
	}

	if req.All {
		return runs
	}

	if req.OlderThan > 0 {
		var filtered []TrashRunInfo
		for i := range runs {
			if runs[i].Age > req.OlderThan {
				filtered = append(filtered, runs[i])
			}
		}
		return filtered
	}

	// No filter specified — return nothing (require explicit filter).
	return nil
}

// purgeRun permanently deletes a single trash run directory.
func purgeRun(run TrashRunInfo, dryRun bool) PurgeOperation {
	op := PurgeOperation{
		RunID:     run.RunID,
		Path:      run.Path,
		FileCount: run.FileCount,
		TotalSize: run.TotalSize,
	}

	if dryRun {
		op.Purged = true
		return op
	}

	if removeErr := os.RemoveAll(run.Path); removeErr != nil {
		op.Error = fmt.Errorf("remove trash directory: %w", removeErr)
		return op
	}

	op.Purged = true
	return op
}
