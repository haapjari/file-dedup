// Package safepath provides path containment validation to ensure
// file operations never escape a designated root directory.
package safepath

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// errCannotRemoveRoot is returned when attempting to remove the root directory.
var errCannotRemoveRoot = errors.New("cannot remove root directory")

var (
	// ErrPathEscape indicates an attempt to access a path outside the root.
	ErrPathEscape = errors.New("path escapes root directory")
	// ErrSymlinkEscape indicates a symlink points outside the root.
	ErrSymlinkEscape = errors.New("symlink target escapes root directory")
	// ErrInvalidRoot indicates the root path is invalid.
	ErrInvalidRoot = errors.New("invalid root directory")
	// ErrTargetExists indicates a rename target already exists.
	ErrTargetExists = errors.New("target already exists")
)

// Validator ensures all paths are contained within a root directory.
type Validator struct {
	root string // Absolute, cleaned path to root directory.
}

// New creates a new Validator for the given root directory.
// The root must be an existing directory.
func New(root string) (*Validator, error) {
	// Convert to absolute path.
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRoot, err)
	}

	resolvedRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRoot, err)
	}

	// Clean the path to remove any . or .. components.
	cleanRoot := filepath.Clean(resolvedRoot)

	// Verify it's an existing directory.
	info, err := os.Stat(cleanRoot)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRoot, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: not a directory", ErrInvalidRoot)
	}

	return &Validator{root: cleanRoot}, nil
}

// Root returns the absolute path to the root directory.
func (v *Validator) Root() string {
	return v.root
}

// Contains checks if the given path is within the root directory.
// It resolves the path to absolute form but does NOT follow symlinks.
func (v *Validator) Contains(path string) bool {
	return v.containsPath(path) == nil
}

// containsPath checks if path is within root and returns error if not.
func (v *Validator) containsPath(path string) error {
	// Convert to absolute path.
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("%w: cannot resolve path", ErrPathEscape)
	}

	// Clean to resolve . and .. components.
	cleanPath := filepath.Clean(absPath)

	// Check if it's within root.
	if !isSubPath(v.root, cleanPath) {
		return ErrPathEscape
	}

	return nil
}

// ValidatePath checks if a path is safely contained within root.
// Returns an error if the path escapes the root directory.
func (v *Validator) ValidatePath(path string) error {
	return v.containsPath(path)
}

// ValidateSymlink checks if a symlink's target is within the root directory.
// Returns ErrSymlinkEscape if the symlink points outside root.
func (v *Validator) ValidateSymlink(symlinkPath string) error {
	// First check the symlink itself is within root.
	if err := v.containsPath(symlinkPath); err != nil {
		return err
	}

	// Get symlink info to verify it is a symlink.
	info, err := os.Lstat(symlinkPath)
	if err != nil {
		return fmt.Errorf("cannot stat symlink: %w", err)
	}

	// If not a symlink, nothing to check.
	if info.Mode()&os.ModeSymlink == 0 {
		return nil
	}

	// Read the symlink target.
	target, err := os.Readlink(symlinkPath)
	if err != nil {
		return fmt.Errorf("cannot read symlink: %w", err)
	}

	// If target is relative, resolve it relative to symlink's directory.
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(symlinkPath), target)
	}

	// Check if target is within root.
	if err := v.containsPath(target); err != nil {
		return fmt.Errorf("%w: %s -> %s", ErrSymlinkEscape, symlinkPath, target)
	}

	return nil
}

// ValidatePathForRead checks if a path is safely contained within root and
// verifies symlink targets do not escape the root directory.
func (v *Validator) ValidatePathForRead(path string) error {
	if err := v.ValidatePath(path); err != nil {
		return err
	}

	return v.ValidateSymlink(path)
}

// ValidatePathForWrite checks if a path is safely contained within root and
// verifies existing path components do not resolve through escaping symlinks.
func (v *Validator) ValidatePathForWrite(path string) error {
	return v.validatePathForMutation(path)
}

// SafeRename renames a file only if both source and destination are within root.
// It refuses to overwrite an existing target to prevent silent data loss.
func (v *Validator) SafeRename(oldPath, newPath string) error {
	if err := v.validatePathForMutation(oldPath); err != nil {
		return fmt.Errorf("source %w: %s", err, oldPath)
	}
	if err := v.validatePathForMutation(newPath); err != nil {
		return fmt.Errorf("destination %w: %s", err, newPath)
	}

	// Refuse to overwrite an existing file or symlink.
	if _, err := os.Lstat(newPath); err == nil {
		return fmt.Errorf("%w: %s", ErrTargetExists, newPath)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("failed to check rename target: %w", err)
	}

	return os.Rename(oldPath, newPath)
}

// SafeRemove removes a file only if it's within root.
func (v *Validator) SafeRemove(path string) error {
	if err := v.validatePathForMutation(path); err != nil {
		return fmt.Errorf("%w: %s", err, path)
	}

	return os.Remove(path)
}

// SafeMkdirAll creates a directory path only if it's within root.
func (v *Validator) SafeMkdirAll(path string) error {
	if err := v.validatePathForMutation(path); err != nil {
		return fmt.Errorf("%w: %s", err, path)
	}

	return os.MkdirAll(path, 0o755)
}

// SafeRemoveDir removes an empty directory only if it's within root
// and is not the root directory itself.
func (v *Validator) SafeRemoveDir(path string) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("%w: cannot resolve path", ErrPathEscape)
	}

	cleanPath := filepath.Clean(absPath)

	// Never remove the root directory itself.
	if cleanPath == v.root {
		return errCannotRemoveRoot
	}

	if err := v.validatePathForMutation(path); err != nil {
		return fmt.Errorf("%w: %s", err, path)
	}

	return os.Remove(path)
}

// isSubPath checks if child is a subpath of parent.
// Both paths must be absolute and clean.
func isSubPath(parent, child string) bool {
	// Equal paths are considered contained.
	if parent == child {
		return true
	}

	// Ensure parent has trailing separator for proper prefix matching.
	parentWithSep := parent
	if !strings.HasSuffix(parentWithSep, string(filepath.Separator)) {
		parentWithSep += string(filepath.Separator)
	}

	return strings.HasPrefix(child, parentWithSep)
}

// ResolveSafePath resolves a potentially relative path to an absolute path
// within the root directory. Returns error if result would escape root.
func (v *Validator) ResolveSafePath(basePath, relativePath string) (string, error) {
	var fullPath string
	if filepath.IsAbs(relativePath) {
		fullPath = relativePath
	} else {
		fullPath = filepath.Join(basePath, relativePath)
	}

	cleanPath := filepath.Clean(fullPath)

	if err := v.containsPath(cleanPath); err != nil {
		return "", err
	}

	return cleanPath, nil
}

func (v *Validator) validatePathForMutation(path string) error {
	if err := v.containsPath(path); err != nil {
		return err
	}

	resolvedPath, err := resolveExistingPath(path)
	if err != nil {
		return err
	}

	if err := v.containsPath(resolvedPath); err != nil {
		return fmt.Errorf("%w: %s -> %s", ErrSymlinkEscape, path, resolvedPath)
	}

	return nil
}

func resolveExistingPath(path string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("cannot resolve path: %w", err)
	}

	resolved, err := filepath.EvalSymlinks(absPath)
	if err == nil {
		return resolved, nil
	}
	if !os.IsNotExist(err) {
		return "", fmt.Errorf("cannot resolve symlinks: %w", err)
	}

	parent := filepath.Dir(absPath)
	if parent == absPath {
		return "", fmt.Errorf("cannot resolve symlinks: %w", err)
	}

	return resolveExistingPath(parent)
}
