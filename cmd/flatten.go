package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"file-dedup/pkg/flattener"
	"file-dedup/pkg/usecase"
)

func buildFlattenCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "flatten [path]",
		Short: "Move all files to root directory, remove duplicates",
		Long: `Moves all files to root directory:
  - Removes true duplicates (same content hash)
  - Adds suffix for name conflicts
  - Deletes empty directories

Examples:
  file-dedup flatten --dry-run ./backup   # Preview changes
  file-dedup flatten ./backup             # Apply changes
  file-dedup flatten -v ./backup          # Verbose output

Before:
  backup/
    Documents/Work/report.pdf
    Photos/Vacation/photo.jpg
    Music/song.mp3

After:
  backup/
    report.pdf
    photo.jpg
    song.mp3`,
		Args: cobra.ExactArgs(1),
		RunE: runFlatten,
	}
}

func runFlatten(_ *cobra.Command, args []string) error {
	execution, empty, err := runWorkersFileCommand(
		"FLATTEN",
		true,
		args[0],
		func(targetDir string, isDryRun bool, workerCount int, onProgress usecase.ProgressCallback) (usecase.FlattenExecution, error) {
			return newUseCaseService().RunFlatten(usecase.FlattenRequest{
				TargetDir:  targetDir,
				DryRun:     isDryRun,
				Workers:    workerCount,
				OnProgress: onProgress,
			})
		},
		func(execution usecase.FlattenExecution) fileCommandExecutionInfo {
			return infoFromMeta(execution.Meta())
		},
	)
	if err != nil {
		return err
	}
	if empty {
		return nil
	}

	result := execution.Result

	printDetailedOperations(result.Operations, printFlattenOperation, func(op flattener.MoveOperation) bool {
		return op.Error != nil
	})

	lines := []string{
		fmt.Sprintf("Total files:     %d", result.TotalFiles),
		fmt.Sprintf("Moved:           %d", result.MovedCount),
		fmt.Sprintf("Duplicates:      %d", result.DuplicatesCount),
		fmt.Sprintf("Skipped:         %d", result.SkippedCount),
		fmt.Sprintf("Errors:          %d", result.ErrorCount),
	}
	if !dryRun {
		lines = append(lines, fmt.Sprintf("Dirs removed:    %d", result.DeletedDirsCount))
	}

	printSummary(lines...)
	printDryRunHint()

	return nil
}

func printFlattenOperation(op flattener.MoveOperation) {
	switch {
	case op.Error != nil:
		fmt.Printf("ERROR: %s: %v\n", op.OriginalPath, op.Error)
	case op.Duplicate:
		fmt.Printf("DUPLICATE: %s\n", op.OriginalPath)
		fmt.Printf("   KEPT: %s\n", op.NewPath)
	case op.Skipped:
		fmt.Printf("SKIP: %s (%s)\n", op.OriginalPath, op.SkipReason)
	default:
		fmt.Printf("MOVE: %s\n", op.OriginalPath)
		fmt.Printf("  TO: %s\n", op.NewPath)
	}
}
