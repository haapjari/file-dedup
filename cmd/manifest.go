package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"file-dedup/pkg/usecase"
)

func buildManifestCommand() *cobra.Command {
	var outputPath string

	cmd := &cobra.Command{
		Use:   "manifest [path]",
		Short: "Create a cryptographic inventory of all files",
		Long: `Creates a manifest (JSON file) containing SHA256 hashes of all files.

Safety:
  - All manifest reads are contained within the target directory
  - Output path must resolve within the target directory
  - Relative -o paths are resolved from the target directory root

The manifest can be used to:
  - Verify no data was lost after operations (compare manifests)
  - Track file inventory over time
  - Detect changes or corruption

Examples:
  file-dedup manifest ./backup -o inventory.json
  file-dedup manifest /path/to/photos -o before.json
  file-dedup manifest --workers 8 ./backup -o manifest.json

Typical safe workflow:
  1. file-dedup manifest /backup -o before.json
  2. file-dedup flatten /backup
  3. file-dedup manifest /backup -o after.json
  4. compare hashes between manifests`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runManifest(args, outputPath)
		},
	}

	cmd.Flags().StringVarP(&outputPath, "output", "o", "manifest.json", "Output path inside target directory")

	return cmd
}

func runManifest(args []string, outputPath string) error {
	progress := startProgress("collecting")
	fmt.Println("Collecting files and computing hashes...")

	execution, err := newUseCaseService().RunManifest(usecase.ManifestRequest{
		TargetDir:  args[0],
		OutputPath: outputPath,
		Workers:    workers,
		OnProgress: func(stage string, processed, total int) {
			progress.Report(stage, processed, total)
		},
	})
	progress.Stop()
	if err != nil {
		return err
	}

	printCommandHeader("MANIFEST", execution.RootDir)
	fmt.Printf("Output file: %s\n", execution.OutputPath)
	fmt.Printf("Workers: %d\n", workers)

	fmt.Printf("\nCompleted in %v\n", execution.Duration.Round(time.Millisecond))
	fmt.Println()
	printSummary(
		fmt.Sprintf("Total files:    %d", execution.Manifest.FileCount()),
		fmt.Sprintf("Unique files:   %d", execution.Manifest.UniqueFileCount()),
		"Total size:     "+formatBytes(execution.Manifest.TotalSize()),
		"Manifest saved: "+execution.OutputPath,
	)

	return nil
}
