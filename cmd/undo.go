package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"file-dedup/pkg/usecase"
)

var undoRunID string

func buildUndoCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "undo [path]",
		Short: "Reverse the most recent operation using its journal",
		Long: `Reverses the most recent file-dedup operation by replaying its journal in reverse:
  - Trashed files are restored to their original locations
  - Renamed files are moved back to their original names
  - Extract operations are skipped (archive is restored via its trash entry)

The journal is marked as rolled back after a successful undo, preventing
it from being undone again.

Examples:
  file-dedup undo --dry-run ./backup       # Preview what would be undone
  file-dedup undo ./backup                 # Undo the most recent operation
  file-dedup undo --run <run-id> ./backup  # Undo a specific operation`,
		Args: cobra.ExactArgs(1),
		RunE: runUndo,
	}

	cmd.Flags().StringVar(&undoRunID, "run", "", "Undo a specific operation by run ID")

	return cmd
}

func runUndo(_ *cobra.Command, args []string) error {
	printDryRunBanner()

	progress := startProgress("undoing")

	execution, err := newUseCaseService().RunUndo(usecase.UndoRequest{
		TargetDir: args[0],
		RunID:     undoRunID,
		DryRun:    dryRun,
		OnProgress: func(stage string, processed, total int) {
			progress.Report(stage, processed, total)
		},
	})
	progress.Stop()

	if err != nil {
		return err
	}

	printCommandHeader("UNDO", execution.RootDir)
	fmt.Printf("Journal: %s\n", execution.JournalPath)
	fmt.Printf("Run ID:  %s\n", execution.RunID)
	fmt.Println()

	printDetailedOperations(execution.Operations, printUndoOperation, func(op usecase.UndoOperation) bool {
		return op.Error != nil
	})

	printSummary(
		fmt.Sprintf("Restored:  %d", execution.RestoredCount),
		fmt.Sprintf("Reversed:  %d", execution.ReversedCount),
		fmt.Sprintf("Skipped:   %d", execution.SkippedCount),
		fmt.Sprintf("Errors:    %d", execution.ErrorCount),
	)
	printDryRunHint()

	return nil
}

func printUndoOperation(op usecase.UndoOperation) {
	switch {
	case op.Error != nil:
		fmt.Printf("ERROR: [%s] %s: %v\n", op.EntryType, op.Source, op.Error)
	case op.Action == "skip":
		fmt.Printf("SKIP: [%s] %s (%s)\n", op.EntryType, op.Source, op.SkipReason)
	case op.Action == "restore":
		fmt.Printf("RESTORE: %s\n", op.Source)
		fmt.Printf("   FROM: %s\n", op.Dest)
	case op.Action == "reverse-rename":
		fmt.Printf("REVERSE: %s\n", op.Dest)
		fmt.Printf("     TO: %s\n", op.Source)
	}
}
