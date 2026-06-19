package safepath_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"file-dedup/pkg/safepath"
)

// TestContainment_ParentDirectoryProtection verifies that parent directories
// are NEVER accessible through the validator.
func TestContainment_ParentDirectoryProtection(t *testing.T) {
	t.Parallel()

	// Create a nested structure: /tmp/XXX/parent/child/target
	// We'll set target as root and verify parent/child can't be accessed.
	baseDir := t.TempDir()
	parentDir := filepath.Join(baseDir, "parent")
	childDir := filepath.Join(parentDir, "child")
	targetDir := filepath.Join(childDir, "target")

	require.NoError(t, os.MkdirAll(targetDir, 0o755))

	// Create files at each level to verify they're not touched.
	parentFile := filepath.Join(parentDir, "parent_secret.txt")
	childFile := filepath.Join(childDir, "child_secret.txt")
	targetFile := filepath.Join(targetDir, "target_file.txt")

	require.NoError(t, os.WriteFile(parentFile, []byte("parent secret"), 0o644))
	require.NoError(t, os.WriteFile(childFile, []byte("child secret"), 0o644))
	require.NoError(t, os.WriteFile(targetFile, []byte("target content"), 0o644))

	// Create validator rooted at targetDir.
	v, err := safepath.New(targetDir)
	require.NoError(t, err)

	// Verify parent paths are NOT contained.
	assert.False(t, v.Contains(parentDir), "parent dir should not be contained")
	assert.False(t, v.Contains(childDir), "child dir (parent of root) should not be contained")
	assert.False(t, v.Contains(parentFile), "parent file should not be contained")
	assert.False(t, v.Contains(childFile), "child file should not be contained")

	// Verify target paths ARE contained.
	assert.True(t, v.Contains(targetDir), "target dir should be contained")
	assert.True(t, v.Contains(targetFile), "target file should be contained")

	// Verify operations on parent fail.
	err = v.SafeRemove(parentFile)
	require.Error(t, err, "SafeRemove on parent file should fail")
	assert.FileExists(t, parentFile, "parent file should still exist")

	err = v.SafeRemove(childFile)
	require.Error(t, err, "SafeRemove on child file should fail")
	assert.FileExists(t, childFile, "child file should still exist")

	// Verify we can't rename to parent.
	err = v.SafeRename(targetFile, parentFile+"_moved")
	require.Error(t, err, "SafeRename to parent should fail")
	assert.FileExists(t, targetFile, "target file should still exist")

	// Verify we can't rename from parent.
	err = v.SafeRename(parentFile, filepath.Join(targetDir, "imported.txt"))
	require.Error(t, err, "SafeRename from parent should fail")
}

// TestContainment_DotDotTraversal verifies that .. path components
// can't be used to escape the root directory.
func TestContainment_DotDotTraversal(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	targetDir := filepath.Join(baseDir, "target")
	siblingDir := filepath.Join(baseDir, "sibling")

	require.NoError(t, os.MkdirAll(targetDir, 0o755))
	require.NoError(t, os.MkdirAll(siblingDir, 0o755))

	siblingFile := filepath.Join(siblingDir, "sibling.txt")
	require.NoError(t, os.WriteFile(siblingFile, []byte("sibling"), 0o644))

	v, err := safepath.New(targetDir)
	require.NoError(t, err)

	// All these attempts to escape via .. should fail.
	escapePaths := []string{
		filepath.Join(targetDir, ".."),
		filepath.Join(targetDir, "..", "sibling"),
		filepath.Join(targetDir, "..", "sibling", "sibling.txt"),
		filepath.Join(targetDir, "subdir", "..", ".."),
		filepath.Join(targetDir, "subdir", "..", "..", "sibling"),
		filepath.Join(targetDir, "a", "b", "..", "..", "..", "sibling"),
	}

	for _, path := range escapePaths {
		assert.False(t, v.Contains(path), "path %q should not be contained", path)
		require.Error(t, v.ValidatePath(path), "ValidatePath(%q) should error", path)
	}

	// Verify sibling file is still untouched.
	assert.FileExists(t, siblingFile)
}

// TestContainment_SymlinkEscape verifies that symlinks pointing outside
// the root directory are detected and blocked.
func TestContainment_SymlinkEscape(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	targetDir := filepath.Join(baseDir, "target")
	outsideDir := filepath.Join(baseDir, "outside")

	require.NoError(t, os.MkdirAll(targetDir, 0o755))
	require.NoError(t, os.MkdirAll(outsideDir, 0o755))

	outsideFile := filepath.Join(outsideDir, "secret.txt")
	require.NoError(t, os.WriteFile(outsideFile, []byte("secret"), 0o644))

	v, err := safepath.New(targetDir)
	require.NoError(t, err)

	// Create a symlink inside target that points outside.
	symlinkPath := filepath.Join(targetDir, "escape_link")
	if symlinkErr := os.Symlink(outsideFile, symlinkPath); symlinkErr != nil {
		t.Skip("symlinks not supported on this platform")
	}

	// ValidateSymlink should detect this escape.
	err = v.ValidateSymlink(symlinkPath)
	require.Error(t, err, "symlink to outside should be detected")

	// Create a symlink with relative path that escapes.
	relativeEscapeLink := filepath.Join(targetDir, "relative_escape")
	if symlinkErr := os.Symlink("../outside/secret.txt", relativeEscapeLink); symlinkErr != nil {
		t.Skip("symlinks not supported")
	}

	err = v.ValidateSymlink(relativeEscapeLink)
	require.Error(t, err, "relative symlink escape should be detected")
}

// TestContainment_AbsolutePathOutside verifies that absolute paths
// outside root are blocked.
func TestContainment_AbsolutePathOutside(t *testing.T) {
	t.Parallel()

	targetDir := t.TempDir()

	v, err := safepath.New(targetDir)
	require.NoError(t, err)

	outsidePaths := []string{
		"/etc/passwd",
		"/etc/shadow",
		"/tmp",
		"/home",
		"/var/log",
		"/",
	}

	for _, path := range outsidePaths {
		assert.False(t, v.Contains(path), "absolute path %q should not be contained", path)
	}
}

// TestContainment_RootCannotBeRemoved verifies that the root directory
// itself cannot be deleted.
func TestContainment_RootCannotBeRemoved(t *testing.T) {
	t.Parallel()

	targetDir := t.TempDir()

	v, err := safepath.New(targetDir)
	require.NoError(t, err)

	// Attempt to remove root should fail.
	err = v.SafeRemoveDir(targetDir)
	require.Error(t, err, "removing root dir should fail")

	// Root should still exist.
	assert.DirExists(t, targetDir)
}

// TestContainment_OperationsStayWithinRoot verifies that all safe operations
// only affect files within the root directory.
func TestContainment_OperationsStayWithinRoot(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	targetDir := filepath.Join(baseDir, "target")
	outsideDir := filepath.Join(baseDir, "outside")

	require.NoError(t, os.MkdirAll(filepath.Join(targetDir, "subdir"), 0o755))
	require.NoError(t, os.MkdirAll(outsideDir, 0o755))

	// Create files.
	insideFile := filepath.Join(targetDir, "inside.txt")
	subFile := filepath.Join(targetDir, "subdir", "nested.txt")
	outsideFile := filepath.Join(outsideDir, "outside.txt")

	require.NoError(t, os.WriteFile(insideFile, []byte("inside"), 0o644))
	require.NoError(t, os.WriteFile(subFile, []byte("nested"), 0o644))
	require.NoError(t, os.WriteFile(outsideFile, []byte("outside"), 0o644))

	v, err := safepath.New(targetDir)
	require.NoError(t, err)

	// Operations within root should work.
	newInsideFile := filepath.Join(targetDir, "inside_renamed.txt")
	require.NoError(t, v.SafeRename(insideFile, newInsideFile))
	assert.FileExists(t, newInsideFile)
	assert.NoFileExists(t, insideFile)

	// Recreate for next test.
	require.NoError(t, os.WriteFile(insideFile, []byte("inside"), 0o644))

	// Operations targeting outside should fail.
	err = v.SafeRename(insideFile, outsideFile+"_new")
	require.Error(t, err)
	assert.FileExists(t, insideFile, "inside file should still exist after failed rename")

	err = v.SafeRename(outsideFile, filepath.Join(targetDir, "imported.txt"))
	require.Error(t, err)
	assert.FileExists(t, outsideFile, "outside file should still exist")

	err = v.SafeRemove(outsideFile)
	require.Error(t, err)
	assert.FileExists(t, outsideFile, "outside file should still exist after failed remove")
}

// TestContainment_MultiLevelNesting verifies containment works at all nesting levels.
func TestContainment_MultiLevelNesting(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()

	// Create 10-level deep structure.
	levels := []string{"l1", "l2", "l3", "l4", "l5", "l6", "l7", "l8", "l9", "l10"}
	currentPath := baseDir
	for _, level := range levels {
		currentPath = filepath.Join(currentPath, level)
	}
	require.NoError(t, os.MkdirAll(currentPath, 0o755))

	// Set root at level 5.
	rootPath := baseDir
	for i := range 5 {
		rootPath = filepath.Join(rootPath, levels[i])
	}

	v, err := safepath.New(rootPath)
	require.NoError(t, err)

	// Paths above level 5 should not be contained.
	aboveRoot := baseDir
	for i := range 4 {
		aboveRoot = filepath.Join(aboveRoot, levels[i])
		assert.False(t, v.Contains(aboveRoot), "level %d should not be contained", i+1)
	}

	// Paths at and below level 5 should be contained.
	atOrBelowRoot := rootPath
	assert.True(t, v.Contains(atOrBelowRoot), "root (level 5) should be contained")

	for i := 5; i < len(levels); i++ {
		atOrBelowRoot = filepath.Join(atOrBelowRoot, levels[i])
		assert.True(t, v.Contains(atOrBelowRoot), "level %d should be contained", i+1)
	}
}

// TestContainment_ConcurrentAccess verifies thread safety of containment checks.
func TestContainment_ConcurrentAccess(t *testing.T) {
	t.Parallel()

	targetDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(targetDir, "subdir"), 0o755))

	v, err := safepath.New(targetDir)
	require.NoError(t, err)

	// Run many concurrent containment checks.
	done := make(chan bool, 100)

	for range 100 {
		go func() {
			// Mix of valid and invalid paths.
			paths := []string{
				filepath.Join(targetDir, "file.txt"),
				filepath.Join(targetDir, "subdir", "nested.txt"),
				filepath.Join(targetDir, "..", "escape"),
				"/etc/passwd",
				targetDir,
			}

			for _, path := range paths {
				_ = v.Contains(path)
				_ = v.ValidatePath(path)
			}

			done <- true
		}()
	}

	// Wait for all goroutines.
	for range 100 {
		<-done
	}
}

// TestContainment_EmptyAndDotPaths verifies handling of edge case paths.
func TestContainment_EmptyAndDotPaths(t *testing.T) {
	t.Parallel()

	targetDir := t.TempDir()

	v, err := safepath.New(targetDir)
	require.NoError(t, err)

	// Single dot should resolve to current directory context.
	result, err := v.ResolveSafePath(targetDir, ".")
	require.NoError(t, err)
	assert.Equal(t, targetDir, result)

	// Empty path should resolve to base.
	result, err = v.ResolveSafePath(targetDir, "")
	require.NoError(t, err)
	assert.Equal(t, targetDir, result)

	// Double dot should fail when it escapes.
	_, err = v.ResolveSafePath(targetDir, "..")
	require.Error(t, err)
}

// TestContainment_SpecialCharactersInPaths verifies paths with special chars.
func TestContainment_SpecialCharactersInPaths(t *testing.T) {
	t.Parallel()

	targetDir := t.TempDir()

	// Create dirs with special characters.
	specialDirs := []string{
		"spaces in name",
		"täällä-öäå",
		"special!@#$%",
		"dots...multiple",
	}

	for _, name := range specialDirs {
		dir := filepath.Join(targetDir, name)
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}

	v, err := safepath.New(targetDir)
	require.NoError(t, err)

	// All special dirs should be contained.
	for _, name := range specialDirs {
		path := filepath.Join(targetDir, name)
		assert.True(t, v.Contains(path), "special dir %q should be contained", name)
	}

	// Test encoded-looking paths that might be misused.
	encodedPaths := []string{
		filepath.Join(targetDir, "..%2f.."),
		targetDir + "/..%00..",
	}

	for _, path := range encodedPaths {
		// These might resolve weirdly but should still be safe.
		if !v.Contains(path) {
			// Good - properly rejected.
			continue
		}
		// If contained, verify it's actually within root after resolution.
		resolved, absErr := filepath.Abs(path)
		if absErr == nil {
			cleaned := filepath.Clean(resolved)
			// Verify it's really inside.
			assert.True(t, v.Contains(cleaned), "resolved path should be inside root")
		}
	}
}
