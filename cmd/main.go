package main

import (
	"os"

	"github.com/spf13/cobra"
)

func main() {
	rootCmd := buildCLICommand()

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func buildCLICommand() *cobra.Command {
	rootCmd := buildRootCommand()
	rootCmd.AddCommand(buildUnzipCommand())
	rootCmd.AddCommand(buildRenameCommand())
	rootCmd.AddCommand(buildFlattenCommand())
	rootCmd.AddCommand(buildDuplicateCommand())
	rootCmd.AddCommand(buildManifestCommand())
	rootCmd.AddCommand(buildOrganizeCommand())
	rootCmd.AddCommand(buildUndoCommand())
	rootCmd.AddCommand(buildPurgeCommand())
	return rootCmd
}
