package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"file-dedup/pkg/deduplicator"
	"file-dedup/pkg/usecase"
)

func buildDuplicateCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "duplicate [path]",
		Short: "Find and remove duplicate files by content hash",
		Long: `Finds and removes duplicate files using content hashing:
  - Groups files by size (fast pre-filter)
  - Computes SHA256 hash to identify true duplicates
  - Uses partial hashing for large files (performance optimization)
  - Keeps one copy, removes the rest

This is safe and reliable - files are only considered duplicates
if their content is byte-for-byte identical (verified by SHA256).

Examples:
  file-dedup duplicate --dry-run ./backup   # Preview (recommended!)
  file-dedup duplicate ./backup             # Apply changes
  file-dedup duplicate -v ./backup          # Verbose output

Use --dry-run first to review what would be deleted!`,
		Args: cobra.ExactArgs(1),
		RunE: runDuplicate,
	}
}

func runDuplicate(_ *cobra.Command, args []string) error {
	execution, empty, err := runWorkersFileCommand(
		"DUPLICATE",
		false,
		args[0],
		func(targetDir string, isDryRun bool, workerCount int, onProgress usecase.ProgressCallback) (usecase.DuplicateExecution, error) {
			return newUseCaseService().RunDuplicate(usecase.DuplicateRequest{
				TargetDir:  targetDir,
				DryRun:     isDryRun,
				Workers:    workerCount,
				OnProgress: onProgress,
			})
		},
		func(execution usecase.DuplicateExecution) fileCommandExecutionInfo {
			return infoFromMeta(execution.Meta())
		},
	)
	if err != nil {
		return err
	}
	if empty {
		return nil
	}

	fmt.Println("Computing hashes and finding duplicates...")

	result := execution.Result

	printDetailedOperations(result.Operations, printDuplicateOperation, func(op deduplicator.DeleteOperation) bool {
		return op.Error != nil
	})

	printSummary(
		fmt.Sprintf("Total files:      %d", result.TotalFiles),
		fmt.Sprintf("Duplicates found: %d", result.DuplicatesFound),
		fmt.Sprintf("Deleted:          %d", result.DeletedCount),
		fmt.Sprintf("Skipped:          %d", result.SkippedCount),
		fmt.Sprintf("Errors:           %d", result.ErrorCount),
		"Space recovered:  "+formatBytes(result.BytesRecovered),
	)
	printDryRunHint()

	return nil
}

func printDuplicateOperation(op deduplicator.DeleteOperation) {
	switch {
	case op.Error != nil:
		fmt.Printf("ERROR: %s: %v\n", op.Path, op.Error)
	case op.Skipped:
		fmt.Printf("SKIP: %s (%s)\n", op.Path, op.SkipReason)
	default:
		fmt.Printf("DELETE: %s\n", op.Path)
		fmt.Printf("   KEPT: %s\n", op.OriginalOf)
		if verbose {
			fmt.Printf("   HASH: %s\n", op.Hash)
		}
	}
}
