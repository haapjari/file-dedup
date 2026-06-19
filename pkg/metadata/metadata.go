// Package metadata manages the .file-dedup/ directory used for safety infrastructure.
package metadata

import (
	"fmt"
	"path/filepath"
	"time"

	"file-dedup/pkg/safepath"
)

// DirName is the name of the metadata directory inside the target.
const DirName = ".file-dedup"

// Dir provides access to the .file-dedup/ metadata directory structure.
type Dir struct {
	root      string              // absolute path to .file-dedup/
	validator *safepath.Validator // parent target's validator
}

// Init creates and returns a Dir for the given target root.
// It creates the .file-dedup/ directory if it does not already exist.
func Init(targetRoot string, validator *safepath.Validator) (*Dir, error) {
	metaRoot := filepath.Join(targetRoot, DirName)

	if err := validator.SafeMkdirAll(metaRoot); err != nil {
		return nil, fmt.Errorf("create metadata directory: %w", err)
	}

	return &Dir{
		root:      metaRoot,
		validator: validator,
	}, nil
}

// Root returns the absolute path to the .file-dedup/ directory.
func (d *Dir) Root() string {
	return d.root
}

// TrashDir returns the trash directory path for a given run ID.
func (d *Dir) TrashDir(runID string) string {
	return filepath.Join(d.root, "trash", runID)
}

// JournalPath returns the journal file path for a given run ID.
func (d *Dir) JournalPath(runID string) string {
	return filepath.Join(d.root, "journal", runID+".jsonl")
}

// ManifestPath returns the manifest file path for a given run ID.
func (d *Dir) ManifestPath(runID string) string {
	return filepath.Join(d.root, "manifests", runID+".json")
}

// LockPath returns the advisory lock file path.
func (d *Dir) LockPath() string {
	return filepath.Join(d.root, "lock")
}

// RunID generates a timestamped run ID for the given command.
// Format: <command>-<YYYYMMDDTHHmmss>.
func (d *Dir) RunID(command string) string {
	return command + "-" + time.Now().UTC().Format("20060102T150405")
}
