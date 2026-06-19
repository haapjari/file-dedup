package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"file-dedup/pkg/usecase"
)

var (
	purgeRunID    string
	purgeOlderStr string
	purgeAll      bool
	purgeForce    bool
)

func buildPurgeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "purge [path]",
		Short: "Permanently delete trashed files",
		Long: `Permanently removes files from the .file-dedup/trash/ directory.

This is the only file-dedup command that irrecoverably deletes data.
At least one filter flag is required unless --dry-run is used alone.
Using --all requires --force to confirm.

Flags:
  --run <run-id>      Purge a specific run's trash
  --older-than <dur>  Only purge runs older than the given duration (e.g. 7d, 24h)
  --all               Purge all trash runs (requires --force)
  --force             Confirm destructive --all purge

Examples:
  file-dedup purge --dry-run ./backup           # List all trash runs with sizes
  file-dedup purge --older-than 7d ./backup     # Purge trash older than 7 days
  file-dedup purge --run <run-id> ./backup      # Purge a specific run's trash
  file-dedup purge --all --force ./backup       # Purge all trash`,
		Args: cobra.ExactArgs(1),
		RunE: runPurge,
	}

	cmd.Flags().StringVar(&purgeRunID, "run", "", "Purge a specific run's trash by run ID")
	cmd.Flags().StringVar(&purgeOlderStr, "older-than", "", "Only purge runs older than this duration (e.g. 7d, 24h, 1h30m)")
	cmd.Flags().BoolVar(&purgeAll, "all", false, "Purge all trash runs")
	cmd.Flags().BoolVar(&purgeForce, "force", false, "Confirm destructive --all purge")

	return cmd
}

func runPurge(_ *cobra.Command, args []string) error {
	olderThan, parseErr := parseOlderThan(purgeOlderStr)
	if parseErr != nil {
		return parseErr
	}

	// Require at least one filter unless in dry-run mode (listing).
	if purgeRunID == "" && !purgeAll && olderThan == 0 && !dryRun {
		return errors.New("at least one of --run, --older-than, or --all is required (use --dry-run to list trash)")
	}

	// Require --force when using --all outside of dry-run mode.
	if purgeAll && !purgeForce && !dryRun {
		return errors.New("--all requires --force to confirm (this permanently deletes all trashed files)")
	}

	printDryRunBanner()

	prog := startProgress("purging")

	execution, err := newUseCaseService().RunPurge(usecase.PurgeRequest{
		TargetDir: args[0],
		RunID:     purgeRunID,
		OlderThan: olderThan,
		All:       purgeAll,
		DryRun:    dryRun,
		OnProgress: func(stage string, processed, total int) {
			prog.Report(stage, processed, total)
		},
	})
	prog.Stop()

	if err != nil {
		return err
	}

	printCommandHeader("PURGE", execution.RootDir)
	fmt.Println()

	if len(execution.Runs) == 0 {
		fmt.Println("No trash runs found.")
		return nil
	}

	printTrashRuns(execution)
	printPurgeOperations(execution)

	printSummary(
		fmt.Sprintf("Purged:    %d run(s)", execution.PurgedCount),
		"Size:      "+formatBytes(execution.PurgedSize),
		fmt.Sprintf("Errors:    %d", execution.ErrorCount),
	)
	printDryRunHint()

	return nil
}

func printTrashRuns(exec usecase.PurgeExecution) {
	fmt.Printf("Trash runs: %d total\n", len(exec.Runs))
	for _, run := range exec.Runs {
		age := run.Age.Truncate(time.Second)
		fmt.Printf("  %s  %d file(s), %s, age %s\n", run.RunID, run.FileCount, formatBytes(run.TotalSize), age)
	}
	fmt.Println()
}

func printPurgeOperations(exec usecase.PurgeExecution) {
	if len(exec.Operations) == 0 {
		if exec.DryRun {
			fmt.Println("No runs match the filter criteria.")
		} else {
			fmt.Println("No runs matched the filter criteria. Nothing purged.")
		}
		fmt.Println()
		return
	}

	for _, op := range exec.Operations {
		switch {
		case op.Error != nil:
			fmt.Printf("ERROR: %s: %v\n", op.RunID, op.Error)
		case op.Purged:
			action := "PURGE"
			if exec.DryRun {
				action = "WOULD PURGE"
			}
			fmt.Printf("%s: %s  %d file(s), %s\n", action, op.RunID, op.FileCount, formatBytes(op.TotalSize))
		}
	}
	fmt.Println()
}

// parseOlderThan parses a duration string that supports day suffixes (e.g. "7d").
func parseOlderThan(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}

	// Support "Nd" shorthand for days.
	if len(s) > 1 && s[len(s)-1] == 'd' {
		dayStr := s[:len(s)-1]
		d, err := time.ParseDuration(dayStr + "h")
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q: %w", s, err)
		}
		return d * 24, nil
	}

	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", s, err)
	}

	return d, nil
}
