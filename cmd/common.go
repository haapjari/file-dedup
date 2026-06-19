package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"file-dedup/pkg/usecase"
)

var defaultSkipFiles = []string{".DS_Store", "Thumbs.db", "file-dedup.log"}
var defaultSkipDirs = []string{".file-dedup"}

func skipFiles() []string {
	return append([]string(nil), defaultSkipFiles...)
}

func skipDirs() []string {
	return append([]string(nil), defaultSkipDirs...)
}

func newUseCaseService() *usecase.Service {
	return usecase.New(usecase.Options{
		SkipFiles:  skipFiles(),
		SkipDirs:   skipDirs(),
		NoSnapshot: noSnapshot,
	})
}

func printDryRunBanner() {
	if !dryRun {
		return
	}

	fmt.Println("=== DRY RUN - no changes will be made ===")
	fmt.Println()
}

func printCommandHeader(command, rootDir string) {
	fmt.Printf("Command: %s\n", command)
	fmt.Printf("root directory: %s\n", rootDir)
}

func printCollectingFiles() {
	fmt.Println("collecting files...")
}

func printFoundFiles(fileCount int, elapsed time.Duration, trailingBlankLine bool) {
	fmt.Printf("found %d files in %v\n", fileCount, elapsed.Round(time.Millisecond))
	if trailingBlankLine {
		fmt.Println()
	}
}

type fileCommandExecutionInfo struct {
	rootDir         string
	fileCount       int
	collectDuration time.Duration
	snapshotPath    string
	journalPath     string
}

func infoFromMeta(m usecase.WorkflowMeta) fileCommandExecutionInfo {
	return fileCommandExecutionInfo{
		rootDir:         m.RootDir,
		fileCount:       m.FileCount,
		collectDuration: m.CollectDuration,
		snapshotPath:    m.SnapshotPath,
		journalPath:     m.JournalPath,
	}
}

func runFileCommand[T any](
	command string,
	trailingBlankLine bool,
	execute func(progress *progressReporter) (T, error),
	executionInfo func(T) fileCommandExecutionInfo,
	printExtraHeader func(),
) (execution T, empty bool, err error) {
	printDryRunBanner()
	printCollectingFiles()

	progress := startProgress("collecting")
	execution, err = execute(progress)
	progress.Stop()
	if err != nil {
		return execution, false, err
	}

	info := executionInfo(execution)
	printCommandHeader(command, info.rootDir)
	if info.snapshotPath != "" {
		fmt.Printf("Snapshot: %s\n", info.snapshotPath)
	}
	if info.journalPath != "" {
		fmt.Printf("Journal: %s\n", info.journalPath)
	}
	if printExtraHeader != nil {
		printExtraHeader()
	}
	printFoundFiles(info.fileCount, info.collectDuration, trailingBlankLine)

	if info.fileCount == 0 {
		fmt.Println("No files to process.")
		return execution, true, nil
	}

	return execution, false, nil
}

func runWorkersFileCommand[T any](
	command string,
	trailingBlankLine bool,
	targetDir string,
	execute func(targetDir string, dryRun bool, workers int, onProgress usecase.ProgressCallback) (T, error),
	executionInfo func(T) fileCommandExecutionInfo,
) (execution T, empty bool, err error) {
	return runFileCommand(
		command,
		trailingBlankLine,
		func(progress *progressReporter) (T, error) {
			return execute(
				targetDir,
				dryRun,
				workers,
				func(stage string, processed, total int) {
					progress.Report(stage, processed, total)
				},
			)
		},
		executionInfo,
		func() {
			fmt.Printf("Workers: %d\n", workers)
		},
	)
}

func printSummary(lines ...string) {
	fmt.Println("=== Summary ===")
	for _, line := range lines {
		fmt.Println(line)
	}
}

func printDetailedOperations[T any](operations []T, printOperation func(T), isError func(T) bool) {
	if verbose || dryRun {
		for _, op := range operations {
			printOperation(op)
		}
		fmt.Println()
		return
	}

	// Always print errors even without --verbose so the user can diagnose failures.
	hasErrors := false
	for _, op := range operations {
		if isError(op) {
			printOperation(op)
			hasErrors = true
		}
	}
	if hasErrors {
		fmt.Println()
	}
}

func printDryRunHint() {
	if !dryRun {
		return
	}

	fmt.Println()
	fmt.Println("Run without --dry-run to apply changes.")
}

func formatBytes(bytes int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)

	switch {
	case bytes >= GB:
		return fmt.Sprintf("%.2f GB", float64(bytes)/GB)
	case bytes >= MB:
		return fmt.Sprintf("%.2f MB", float64(bytes)/MB)
	case bytes >= KB:
		return fmt.Sprintf("%.2f KB", float64(bytes)/KB)
	default:
		return fmt.Sprintf("%d bytes", bytes)
	}
}

type progressReporter struct {
	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}

	mu             sync.Mutex
	label          string
	stage          string
	startTime      time.Time
	lastPrintTime  time.Time
	lastProcessed  int
	lastTotal      int
	hasDeterminate bool
	indeterminate  int
}

const (
	progressHeartbeatInterval = 5 * time.Second
	progressPrintInterval     = time.Second
	progressBarWidth          = 24
)

func startProgress(label string) *progressReporter {
	p := &progressReporter{
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
		label:  label,
		stage:  label,
	}

	p.startTime = time.Now()
	ticker := time.NewTicker(progressHeartbeatInterval)

	go func() {
		defer close(p.doneCh)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				p.printHeartbeat()
			case <-p.stopCh:
				return
			}
		}
	}()

	return p
}

func (p *progressReporter) Report(stage string, processed, total int) {
	if total <= 0 {
		return
	}

	if processed < 0 {
		processed = 0
	}
	if processed > total {
		processed = total
	}

	now := time.Now()
	stageLabel := p.normalizedStage(stage)

	p.mu.Lock()
	changed := !p.hasDeterminate || stageLabel != p.stage || processed != p.lastProcessed || total != p.lastTotal
	p.stage = stageLabel
	p.lastProcessed = processed
	p.lastTotal = total
	p.hasDeterminate = true

	if !changed {
		p.mu.Unlock()
		return
	}

	if processed < total && now.Sub(p.lastPrintTime) < progressPrintInterval {
		p.mu.Unlock()
		return
	}

	p.lastPrintTime = now
	line := renderProgressLine(stageLabel, processed, total, time.Since(p.startTime).Round(time.Second))
	p.mu.Unlock()

	fmt.Fprintln(os.Stderr, line)
}

func (p *progressReporter) printHeartbeat() {
	p.mu.Lock()
	elapsed := time.Since(p.startTime).Round(time.Second)

	if p.hasDeterminate {
		if p.lastTotal <= 0 || p.lastProcessed >= p.lastTotal {
			p.mu.Unlock()
			return
		}

		line := renderProgressLine(p.stage, p.lastProcessed, p.lastTotal, elapsed)
		p.mu.Unlock()
		fmt.Fprintln(os.Stderr, line)
		return
	}

	label := p.stage
	indeterminate := p.indeterminate
	p.indeterminate++
	p.mu.Unlock()

	fmt.Fprintln(os.Stderr, renderIndeterminateLine(label, indeterminate, elapsed))
}

func (p *progressReporter) normalizedStage(stage string) string {
	stage = strings.TrimSpace(stage)
	if stage == "" {
		return p.label
	}

	return stage
}

func renderProgressLine(stage string, processed, total int, elapsed time.Duration) string {
	percentage := processed * 100 / total
	filled := processed * progressBarWidth / total
	if filled < 0 {
		filled = 0
	}
	if filled > progressBarWidth {
		filled = progressBarWidth
	}

	bar := strings.Repeat("#", filled) + strings.Repeat("-", progressBarWidth-filled)
	return fmt.Sprintf("%s [%s] %3d%% (%d/%d) elapsed %s", stage, bar, percentage, processed, total, elapsed)
}

func renderIndeterminateLine(stage string, tick int, elapsed time.Duration) string {
	if progressBarWidth <= 0 {
		return fmt.Sprintf("%s [------------------------] --%% (--/--) elapsed %s", stage, elapsed)
	}

	cycle := progressBarWidth * 2
	position := tick % cycle
	filled := position
	if position > progressBarWidth {
		filled = cycle - position
	}

	if filled < 0 {
		filled = 0
	}
	if filled > progressBarWidth {
		filled = progressBarWidth
	}

	bar := strings.Repeat("#", filled) + strings.Repeat("-", progressBarWidth-filled)
	return fmt.Sprintf("%s [%s] --%% (--/--) elapsed %s", stage, bar, elapsed)
}

func (p *progressReporter) Stop() {
	p.stopOnce.Do(func() {
		close(p.stopCh)
		<-p.doneCh
	})
}
