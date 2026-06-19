package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"file-dedup/pkg/renamer"
	"file-dedup/pkg/usecase"
)

func buildRenameCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "rename [path]",
		Short: "Rename files with date prefix and sanitized names",
		Long: `Renames files in place with consistent naming:
  - Adds modification date prefix (YYYY-MM-DD_)
  - Converts to lowercase
  - Replaces spaces with underscores
  - Converts Finnish characters (ä→a, ö→o, å→a)

Examples:
  file-dedup rename --dry-run ./backup    # Preview changes
  file-dedup rename ./backup              # Apply changes
  file-dedup rename -v ./backup           # Verbose output

Before: "My Document.pdf" (modified 2018-06-15)
After:  "2018-06-15_my_document.pdf"`,
		Args: cobra.ExactArgs(1),
		RunE: runRename,
	}
}

func runRename(_ *cobra.Command, args []string) error {
	execution, empty, err := runFileCommand(
		"RENAME",
		true,
		func(progress *progressReporter) (usecase.RenameExecution, error) {
			return newUseCaseService().RunRename(usecase.RenameRequest{
				TargetDir: args[0],
				DryRun:    dryRun,
				OnProgress: func(stage string, processed, total int) {
					progress.Report(stage, processed, total)
				},
			})
		},
		func(execution usecase.RenameExecution) fileCommandExecutionInfo {
			return infoFromMeta(execution.Meta())
		},
		nil,
	)
	if err != nil {
		return err
	}
	if empty {
		return nil
	}

	result := execution.Result

	printDetailedOperations(result.Operations, func(op renamer.RenameOperation) {
		if op.Skipped {
			fmt.Printf("SKIP: %s (%s)\n", op.OriginalName, op.SkipReason)
		} else if op.Error != nil {
			fmt.Printf("ERROR: %s -> %s: %v\n", op.OriginalName, op.NewName, op.Error)
		} else {
			fmt.Printf("RENAME: %s\n", op.OriginalPath)
			fmt.Printf("    TO: %s\n", op.NewPath)
		}
	}, func(op renamer.RenameOperation) bool {
		return op.Error != nil
	})

	printSummary(
		fmt.Sprintf("Total files:  %d", result.TotalFiles),
		fmt.Sprintf("Renamed:      %d", result.RenamedCount),
		fmt.Sprintf("Skipped:      %d", result.SkippedCount),
		fmt.Sprintf("Deleted:      %d", result.DeletedCount),
		fmt.Sprintf("Errors:       %d", result.ErrorCount),
	)
	printDryRunHint()

	return nil
}
