// Package trash provides soft-delete capability for files within a target directory.
// Files are moved to .file-dedup/trash/<run-id>/ instead of being permanently deleted,
// preserving their relative directory structure for later restore or purge.
package trash

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"file-dedup/pkg/metadata"
	"file-dedup/pkg/safepath"
)

// ErrNotInTrash is returned when attempting to restore a path that is not
// inside the trash directory for this run.
var ErrNotInTrash = errors.New("path is not in the trash directory")

// Trasher moves files to a run-specific trash directory instead of deleting them.
type Trasher struct {
	trashRoot  string // .file-dedup/trash/<run-id>/
	targetRoot string // the target directory
	validator  *safepath.Validator
}

// New creates a Trasher for the given run. It creates the trash directory.
func New(metaDir *metadata.Dir, runID string, validator *safepath.Validator) (*Trasher, error) {
	trashRoot := metaDir.TrashDir(runID)

	if err := validator.SafeMkdirAll(trashRoot); err != nil {
		return nil, fmt.Errorf("create trash directory: %w", err)
	}

	return &Trasher{
		trashRoot:  trashRoot,
		targetRoot: validator.Root(),
		validator:  validator,
	}, nil
}

// Trash moves a file from its current location into the trash directory,
// preserving the relative path from the target root.
func (t *Trasher) Trash(path string) error {
	if err := t.validator.ValidatePathForWrite(path); err != nil {
		return fmt.Errorf("validate source for trash: %w", err)
	}

	dest, err := t.trashDest(path)
	if err != nil {
		return err
	}

	destDir := filepath.Dir(dest)
	if err := t.validator.SafeMkdirAll(destDir); err != nil {
		return fmt.Errorf("create trash subdirectory: %w", err)
	}

	return os.Rename(path, dest)
}

// TrashWithDest moves a file to trash and returns the trash destination path.
// This is a convenience method combining TrashPath and Trash in a single call.
func (t *Trasher) TrashWithDest(path string) (string, error) {
	dest, err := t.trashDest(path)
	if err != nil {
		return "", err
	}

	if err := t.Trash(path); err != nil {
		return "", err
	}

	return dest, nil
}

// TrashPath returns the trash destination for a file without moving it.
func (t *Trasher) TrashPath(path string) (string, error) {
	return t.trashDest(path)
}

// Restore moves a trashed file back to its original location.
func (t *Trasher) Restore(trashedPath string) error {
	if err := t.validator.ValidatePathForWrite(trashedPath); err != nil {
		return fmt.Errorf("validate trashed path: %w", err)
	}

	rel, err := filepath.Rel(t.trashRoot, trashedPath)
	if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("%w: %s", ErrNotInTrash, trashedPath)
	}

	originalPath := filepath.Join(t.targetRoot, rel)

	if err := t.validator.ValidatePathForWrite(originalPath); err != nil {
		return fmt.Errorf("validate restore destination: %w", err)
	}

	originalDir := filepath.Dir(originalPath)
	if err := t.validator.SafeMkdirAll(originalDir); err != nil {
		return fmt.Errorf("create restore directory: %w", err)
	}

	return t.validator.SafeRename(trashedPath, originalPath)
}

// RestoreAll restores all files from this run's trash back to their original locations.
func (t *Trasher) RestoreAll() error {
	return filepath.Walk(t.trashRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		return t.Restore(path)
	})
}

// Purge permanently deletes all files in this run's trash directory.
func (t *Trasher) Purge() error {
	return os.RemoveAll(t.trashRoot)
}

// trashDest computes the trash destination for a file.
func (t *Trasher) trashDest(path string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}

	rel, err := filepath.Rel(t.targetRoot, absPath)
	if err != nil {
		return "", fmt.Errorf("compute relative path: %w", err)
	}

	return filepath.Join(t.trashRoot, rel), nil
}
