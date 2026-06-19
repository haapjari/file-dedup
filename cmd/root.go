package main

import (
	"runtime"

	"github.com/spf13/cobra"
)

// version is set at build time via -ldflags.
var version = "dev"

var (
	dryRun     bool
	verbose    bool
	workers    int
	noSnapshot bool
)

func buildRootCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "file-dedup",
		Version: version,
		Short:   "Organize backup files by unzipping, renaming, and flattening directory structures",
		Long: `file-dedup helps clean up backup directories. Every destructive operation is
reversible through soft-delete, journaling, and undo.

Commands:
  unzip      Extracts zip archives recursively and removes extracted archives
  rename     Renames files in place with consistent naming
  flatten    Moves all files to root directory, removes duplicates by content hash
  organize   Groups files into subdirectories by file extension
  duplicate  Finds and removes duplicate files by content hash
  manifest   Creates a cryptographic inventory of all files
  undo       Reverses the most recent operation using its journal
  purge      Permanently deletes trashed files (only irrecoverable command)

Examples:
  # Typical workflow: unzip, rename, flatten, organize, deduplicate
  file-dedup unzip /path/to/backup/2018
  file-dedup rename /path/to/backup/2018
  file-dedup flatten /path/to/backup/2018
  file-dedup organize /path/to/backup/2018
  file-dedup duplicate /path/to/backup/2018

  # Preview any command with --dry-run
  file-dedup unzip --dry-run /path/to/backup/2018
  file-dedup rename --dry-run /path/to/backup/2018

  # Undo the last operation
  file-dedup undo /path/to/backup/2018
  file-dedup undo --run <run-id> /path/to/backup/2018

  # Purge old trash
  file-dedup purge --older-than 30d /path/to/backup
  file-dedup purge --all --force /path/to/backup

  # Skip pre-operation manifest snapshot
  file-dedup flatten --no-snapshot /path/to/backup

  # Manual manifest workflow
  file-dedup manifest /backup -o before.json
  file-dedup flatten /backup
  file-dedup manifest /backup -o after.json

Safety:
  Files are never permanently deleted; they are moved to .file-dedup/trash/.
  Every mutation is journaled to .file-dedup/journal/ for undo support.
  A manifest snapshot is saved to .file-dedup/manifests/ before each operation.
  Advisory file locking prevents concurrent file-dedup processes.

Compression:
  ZIP methods store (0), deflate (8), and Deflate64 (9) are supported.

  The tool will NEVER modify files outside the specified directory.`,
	}

	cmd.PersistentFlags().BoolVar(&dryRun, "dry-run", false, "Show what would be done without making changes")
	cmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Verbose output")
	cmd.PersistentFlags().IntVar(&workers, "workers", runtime.NumCPU(), "Number of parallel workers for hashing")
	cmd.PersistentFlags().BoolVar(&noSnapshot, "no-snapshot", false, "Skip pre-operation manifest snapshot")

	return cmd
}
