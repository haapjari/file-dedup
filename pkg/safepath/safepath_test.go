package safepath_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"file-dedup/pkg/safepath"
)

func TestNew(t *testing.T) {
	t.Parallel()

	t.Run("valid directory", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()

		v, err := safepath.New(tmpDir)
		require.NoError(t, err)
		assert.Equal(t, tmpDir, v.Root())
	})

	t.Run("non-existent directory", func(t *testing.T) {
		t.Parallel()
		_, err := safepath.New("/nonexistent/path/12345")
		assert.Error(t, err)
	})

	t.Run("file instead of directory", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		tmpFile := filepath.Join(tmpDir, "file.txt")
		require.NoError(t, os.WriteFile(tmpFile, []byte("test"), 0o644))

		_, err := safepath.New(tmpFile)
		assert.Error(t, err)
	})

	t.Run("relative path converted to absolute", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		subDir := filepath.Join(tmpDir, "subdir")
		require.NoError(t, os.Mkdir(subDir, 0o755))

		v, err := safepath.New(subDir)
		require.NoError(t, err)
		assert.True(t, filepath.IsAbs(v.Root()))
		resolved, err := filepath.EvalSymlinks(subDir)
		require.NoError(t, err)
		assert.Equal(t, resolved, v.Root())
	})

	t.Run("symlink root resolves to real path", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		targetDir := filepath.Join(tmpDir, "target")
		require.NoError(t, os.MkdirAll(targetDir, 0o755))

		linkPath := filepath.Join(tmpDir, "root_link")
		if err := os.Symlink(targetDir, linkPath); err != nil {
			t.Skip("symlinks not supported")
		}

		v, err := safepath.New(linkPath)
		require.NoError(t, err)
		resolved, err := filepath.EvalSymlinks(linkPath)
		require.NoError(t, err)
		assert.Equal(t, resolved, v.Root())
	})
}

func TestContains(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	subDir := filepath.Join(tmpDir, "sub")
	deepDir := filepath.Join(subDir, "deep")
	require.NoError(t, os.MkdirAll(deepDir, 0o755))

	v, err := safepath.New(tmpDir)
	require.NoError(t, err)

	tests := []struct {
		name     string
		path     string
		expected bool
	}{
		{"root itself", tmpDir, true},
		{"subdirectory", subDir, true},
		{"deep subdirectory", deepDir, true},
		{"file in root", filepath.Join(tmpDir, "file.txt"), true},
		{"file in subdir", filepath.Join(subDir, "file.txt"), true},
		{"parent directory", filepath.Dir(tmpDir), false},
		{"sibling directory", filepath.Join(filepath.Dir(tmpDir), "sibling"), false},
		{"absolute outside path", "/etc/passwd", false},
		{"path with dot-dot", filepath.Join(tmpDir, "sub", "..", "..", "outside"), false},
		{"path with dots staying inside", filepath.Join(tmpDir, "sub", "..", "sub"), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, v.Contains(tt.path))
		})
	}
}

func TestValidatePath(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	v, err := safepath.New(tmpDir)
	require.NoError(t, err)

	t.Run("valid path returns nil", func(t *testing.T) {
		t.Parallel()
		err := v.ValidatePath(filepath.Join(tmpDir, "valid.txt"))
		assert.NoError(t, err)
	})

	t.Run("escaping path returns error", func(t *testing.T) {
		t.Parallel()
		err := v.ValidatePath(filepath.Join(tmpDir, "..", "escape.txt"))
		assert.Error(t, err)
	})
}

func TestValidatePath_ErrorIdentity(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	v, err := safepath.New(tmpDir)
	require.NoError(t, err)

	err = v.ValidatePath(filepath.Join(tmpDir, "..", "escape.txt"))
	require.ErrorIs(t, err, safepath.ErrPathEscape)
}

func TestValidateSymlink(t *testing.T) {
	t.Parallel()

	t.Run("symlink within root", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		subDir := filepath.Join(tmpDir, "sub")
		require.NoError(t, os.Mkdir(subDir, 0o755))
		targetFile := filepath.Join(tmpDir, "target.txt")
		require.NoError(t, os.WriteFile(targetFile, []byte("target"), 0o644))

		v, err := safepath.New(tmpDir)
		require.NoError(t, err)

		linkPath := filepath.Join(subDir, "link_inside")
		if err := os.Symlink(targetFile, linkPath); err != nil {
			t.Skip("symlinks not supported")
		}

		assert.NoError(t, v.ValidateSymlink(linkPath))
	})

	t.Run("symlink pointing outside root", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		subDir := filepath.Join(tmpDir, "sub")
		require.NoError(t, os.Mkdir(subDir, 0o755))

		v, err := safepath.New(tmpDir)
		require.NoError(t, err)

		linkPath := filepath.Join(subDir, "link_outside")
		if err := os.Symlink("/etc/passwd", linkPath); err != nil {
			t.Skip("symlinks not supported")
		}

		err = v.ValidateSymlink(linkPath)
		require.ErrorIs(t, err, safepath.ErrSymlinkEscape)
		assert.Contains(t, err.Error(), linkPath)
	})

	t.Run("symlink outside root rejected before stat", func(t *testing.T) {
		t.Parallel()
		rootDir := t.TempDir()
		outsideDir := t.TempDir()
		linkPath := filepath.Join(outsideDir, "link")
		if err := os.Symlink(filepath.Join(rootDir, "target"), linkPath); err != nil {
			t.Skip("symlinks not supported")
		}

		v, err := safepath.New(rootDir)
		require.NoError(t, err)

		err = v.ValidateSymlink(linkPath)
		require.ErrorIs(t, err, safepath.ErrPathEscape)
	})

	t.Run("symlink with relative path staying inside", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		subDir := filepath.Join(tmpDir, "sub")
		require.NoError(t, os.Mkdir(subDir, 0o755))
		targetFile := filepath.Join(tmpDir, "target.txt")
		require.NoError(t, os.WriteFile(targetFile, []byte("target"), 0o644))

		v, err := safepath.New(tmpDir)
		require.NoError(t, err)

		linkPath := filepath.Join(subDir, "link_relative")
		if err := os.Symlink("../target.txt", linkPath); err != nil {
			t.Skip("symlinks not supported")
		}

		assert.NoError(t, v.ValidateSymlink(linkPath))
	})

	t.Run("symlink with relative path escaping", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		subDir := filepath.Join(tmpDir, "sub")
		require.NoError(t, os.Mkdir(subDir, 0o755))

		v, err := safepath.New(tmpDir)
		require.NoError(t, err)

		linkPath := filepath.Join(subDir, "link_escape")
		if err := os.Symlink("../../../../../../etc/passwd", linkPath); err != nil {
			t.Skip("symlinks not supported")
		}

		assert.Error(t, v.ValidateSymlink(linkPath))
	})

	t.Run("regular file (not symlink)", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		targetFile := filepath.Join(tmpDir, "target.txt")
		require.NoError(t, os.WriteFile(targetFile, []byte("target"), 0o644))

		v, err := safepath.New(tmpDir)
		require.NoError(t, err)

		assert.NoError(t, v.ValidateSymlink(targetFile))
	})
}

func TestValidatePathForRead(t *testing.T) {
	t.Parallel()

	t.Run("regular file within root", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		filePath := filepath.Join(tmpDir, "file.txt")
		require.NoError(t, os.WriteFile(filePath, []byte("content"), 0o644))

		v, err := safepath.New(tmpDir)
		require.NoError(t, err)

		assert.NoError(t, v.ValidatePathForRead(filePath))
	})

	t.Run("path escape", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		v, err := safepath.New(tmpDir)
		require.NoError(t, err)

		err = v.ValidatePathForRead(filepath.Join(tmpDir, "..", "outside.txt"))
		require.ErrorIs(t, err, safepath.ErrPathEscape)
	})

	t.Run("symlink escape", func(t *testing.T) {
		t.Parallel()

		baseDir := t.TempDir()
		rootDir := filepath.Join(baseDir, "root")
		outsideDir := filepath.Join(baseDir, "outside")
		require.NoError(t, os.MkdirAll(rootDir, 0o755))
		require.NoError(t, os.MkdirAll(outsideDir, 0o755))

		outsideFile := filepath.Join(outsideDir, "secret.txt")
		require.NoError(t, os.WriteFile(outsideFile, []byte("secret"), 0o644))

		linkPath := filepath.Join(rootDir, "escape_link")
		if err := os.Symlink(outsideFile, linkPath); err != nil {
			t.Skip("symlinks not supported")
		}

		v, err := safepath.New(rootDir)
		require.NoError(t, err)

		err = v.ValidatePathForRead(linkPath)
		assert.Error(t, err)
	})
}

func TestSafeRename(t *testing.T) {
	t.Parallel()

	t.Run("rename within root", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		src := filepath.Join(tmpDir, "source.txt")
		dst := filepath.Join(tmpDir, "dest.txt")

		require.NoError(t, os.WriteFile(src, []byte("content"), 0o644))

		v, err := safepath.New(tmpDir)
		require.NoError(t, err)

		require.NoError(t, v.SafeRename(src, dst))

		assert.NoFileExists(t, src)
		assert.FileExists(t, dst)
	})

	t.Run("rename to outside root blocked", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		src := filepath.Join(tmpDir, "inside.txt")
		require.NoError(t, os.WriteFile(src, []byte("content"), 0o644))

		v, err := safepath.New(tmpDir)
		require.NoError(t, err)

		dst := filepath.Join(tmpDir, "..", "outside.txt")
		require.Error(t, v.SafeRename(src, dst))
		assert.FileExists(t, src)
	})

	t.Run("rename from outside root blocked", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		outsideDir := t.TempDir()
		outsideFile := filepath.Join(outsideDir, "outside.txt")
		require.NoError(t, os.WriteFile(outsideFile, []byte("content"), 0o644))

		v, err := safepath.New(tmpDir)
		require.NoError(t, err)

		dst := filepath.Join(tmpDir, "imported.txt")
		assert.Error(t, v.SafeRename(outsideFile, dst))
	})

	t.Run("rename refuses to overwrite existing file", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		src := filepath.Join(tmpDir, "source.txt")
		dst := filepath.Join(tmpDir, "existing.txt")

		require.NoError(t, os.WriteFile(src, []byte("source content"), 0o644))
		require.NoError(t, os.WriteFile(dst, []byte("existing content"), 0o644))

		v, err := safepath.New(tmpDir)
		require.NoError(t, err)

		err = v.SafeRename(src, dst)
		require.Error(t, err, "SafeRename must refuse to overwrite existing file")
		require.ErrorIs(t, err, safepath.ErrTargetExists)

		// Both files must survive with original contents.
		assert.FileExists(t, src)
		assert.FileExists(t, dst)
		srcData, _ := os.ReadFile(src)
		dstData, _ := os.ReadFile(dst)
		assert.Equal(t, "source content", string(srcData))
		assert.Equal(t, "existing content", string(dstData))
	})

	t.Run("rename refuses to overwrite symlink", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		src := filepath.Join(tmpDir, "source.txt")
		target := filepath.Join(tmpDir, "target.txt")
		link := filepath.Join(tmpDir, "link.txt")

		require.NoError(t, os.WriteFile(src, []byte("source"), 0o644))
		require.NoError(t, os.WriteFile(target, []byte("target"), 0o644))
		if err := os.Symlink(target, link); err != nil {
			t.Skip("symlinks not supported")
		}

		v, err := safepath.New(tmpDir)
		require.NoError(t, err)

		err = v.SafeRename(src, link)
		require.Error(t, err, "SafeRename must refuse to overwrite symlink")
		require.ErrorIs(t, err, safepath.ErrTargetExists)
		assert.FileExists(t, src)
	})

	t.Run("rename through symlink outside root blocked", func(t *testing.T) {
		t.Parallel()
		baseDir := t.TempDir()
		rootDir := filepath.Join(baseDir, "root")
		outsideDir := filepath.Join(baseDir, "outside")
		require.NoError(t, os.MkdirAll(rootDir, 0o755))
		require.NoError(t, os.MkdirAll(outsideDir, 0o755))

		outsideFile := filepath.Join(outsideDir, "secret.txt")
		require.NoError(t, os.WriteFile(outsideFile, []byte("secret"), 0o644))

		linkPath := filepath.Join(rootDir, "escape_link")
		if err := os.Symlink(outsideFile, linkPath); err != nil {
			t.Skip("symlinks not supported")
		}

		v, err := safepath.New(rootDir)
		require.NoError(t, err)

		dest := filepath.Join(rootDir, "dest.txt")
		err = v.SafeRename(linkPath, dest)
		require.Error(t, err)

		assert.FileExists(t, outsideFile)
		assert.FileExists(t, linkPath)
		assert.NoFileExists(t, dest)
	})
}

func TestSafeRemove(t *testing.T) {
	t.Parallel()

	t.Run("remove within root", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		file := filepath.Join(tmpDir, "remove_me.txt")
		require.NoError(t, os.WriteFile(file, []byte("content"), 0o644))

		v, err := safepath.New(tmpDir)
		require.NoError(t, err)

		require.NoError(t, v.SafeRemove(file))
		assert.NoFileExists(t, file)
	})

	t.Run("remove outside root blocked", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()

		v, err := safepath.New(tmpDir)
		require.NoError(t, err)

		outsideFile := filepath.Join(tmpDir, "..", "should_not_delete.txt")
		assert.Error(t, v.SafeRemove(outsideFile))
	})

	t.Run("remove through symlink outside root blocked", func(t *testing.T) {
		t.Parallel()
		baseDir := t.TempDir()
		rootDir := filepath.Join(baseDir, "root")
		outsideDir := filepath.Join(baseDir, "outside")
		require.NoError(t, os.MkdirAll(rootDir, 0o755))
		require.NoError(t, os.MkdirAll(outsideDir, 0o755))

		outsideFile := filepath.Join(outsideDir, "secret.txt")
		require.NoError(t, os.WriteFile(outsideFile, []byte("secret"), 0o644))

		linkPath := filepath.Join(rootDir, "escape_link")
		if err := os.Symlink(outsideFile, linkPath); err != nil {
			t.Skip("symlinks not supported")
		}

		v, err := safepath.New(rootDir)
		require.NoError(t, err)

		err = v.SafeRemove(linkPath)
		require.Error(t, err)

		assert.FileExists(t, outsideFile)
		assert.FileExists(t, linkPath)
	})
}

func TestSafeMkdirAll(t *testing.T) {
	t.Parallel()

	t.Run("create directory within root", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()

		v, err := safepath.New(tmpDir)
		require.NoError(t, err)

		dir := filepath.Join(tmpDir, "new_dir", "sub_dir")
		require.NoError(t, v.SafeMkdirAll(dir))
		assert.DirExists(t, dir)
	})

	t.Run("create directory outside root blocked", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()

		v, err := safepath.New(tmpDir)
		require.NoError(t, err)

		outsideDir := filepath.Join(tmpDir, "..", "should_not_create")
		assert.Error(t, v.SafeMkdirAll(outsideDir))
	})

	t.Run("existing directory is no-op", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		existingDir := filepath.Join(tmpDir, "existing")
		require.NoError(t, os.Mkdir(existingDir, 0o755))

		v, err := safepath.New(tmpDir)
		require.NoError(t, err)

		require.NoError(t, v.SafeMkdirAll(existingDir))
		assert.DirExists(t, existingDir)
	})
}

func TestSafeRemoveDir(t *testing.T) {
	t.Parallel()

	t.Run("remove empty directory within root", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		dir := filepath.Join(tmpDir, "empty_dir")
		require.NoError(t, os.Mkdir(dir, 0o755))

		v, err := safepath.New(tmpDir)
		require.NoError(t, err)

		require.NoError(t, v.SafeRemoveDir(dir))
		assert.NoDirExists(t, dir)
	})

	t.Run("cannot remove root directory itself", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()

		v, err := safepath.New(tmpDir)
		require.NoError(t, err)

		err = v.SafeRemoveDir(filepath.Join(tmpDir, "."))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot remove root directory")
	})

	t.Run("remove directory outside root blocked", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()

		v, err := safepath.New(tmpDir)
		require.NoError(t, err)

		outsideDir := filepath.Join(tmpDir, "..", "should_not_delete_dir")
		assert.Error(t, v.SafeRemoveDir(outsideDir))
	})
}

func TestSafeRemoveDir_RejectsSymlinkEscape(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	rootDir := filepath.Join(baseDir, "root")
	outsideDir := filepath.Join(baseDir, "outside")
	require.NoError(t, os.MkdirAll(rootDir, 0o755))
	require.NoError(t, os.MkdirAll(outsideDir, 0o755))

	linkPath := filepath.Join(rootDir, "escape_link")
	if err := os.Symlink(outsideDir, linkPath); err != nil {
		t.Skip("symlinks not supported")
	}

	v, err := safepath.New(rootDir)
	require.NoError(t, err)

	err = v.SafeRemoveDir(linkPath)
	require.ErrorIs(t, err, safepath.ErrSymlinkEscape)
	assert.DirExists(t, outsideDir)
	assert.FileExists(t, linkPath)
}

func TestResolveSafePath(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	subDir := filepath.Join(tmpDir, "sub")
	require.NoError(t, os.Mkdir(subDir, 0o755))

	v, err := safepath.New(tmpDir)
	require.NoError(t, err)

	t.Run("resolve relative path within root", func(t *testing.T) {
		t.Parallel()
		result, err := v.ResolveSafePath(subDir, "file.txt")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(subDir, "file.txt"), result)
	})

	t.Run("resolve absolute path within root", func(t *testing.T) {
		t.Parallel()
		absPath := filepath.Join(tmpDir, "absolute.txt")
		result, err := v.ResolveSafePath(subDir, absPath)
		require.NoError(t, err)
		assert.Equal(t, absPath, result)
	})

	t.Run("reject escaping relative path", func(t *testing.T) {
		t.Parallel()
		_, err := v.ResolveSafePath(subDir, "../../escape.txt")
		assert.Error(t, err)
	})

	t.Run("reject escaping absolute path", func(t *testing.T) {
		t.Parallel()
		_, err := v.ResolveSafePath(subDir, "/etc/passwd")
		assert.Error(t, err)
	})
}

func TestPathTraversalAttacks(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	v, err := safepath.New(tmpDir)
	require.NoError(t, err)

	attackPaths := []string{
		"../etc/passwd",
		"..\\etc\\passwd",
		"....//....//etc/passwd",
		"..%2f..%2fetc/passwd",
		"..%252f..%252fetc/passwd",
		"/etc/passwd",
		"sub/../../../etc/passwd",
		"sub/./../../etc/passwd",
		filepath.Join(tmpDir, "..", "escape"),
		filepath.Join(tmpDir, "sub", "..", "..", "escape"),
	}

	for _, attack := range attackPaths {
		t.Run(attack, func(t *testing.T) {
			t.Parallel()
			assert.False(t, v.Contains(attack), "path %q should not be contained in root", attack)
		})
	}
}

func TestEdgeCases(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	v, err := safepath.New(tmpDir)
	require.NoError(t, err)

	t.Run("empty relative path resolves to base", func(t *testing.T) {
		t.Parallel()
		result, err := v.ResolveSafePath(tmpDir, "")
		require.NoError(t, err)
		assert.Equal(t, tmpDir, result)
	})

	t.Run("single dot path", func(t *testing.T) {
		t.Parallel()
		result, err := v.ResolveSafePath(tmpDir, ".")
		require.NoError(t, err)
		assert.Equal(t, tmpDir, result)
	})

	t.Run("path with multiple slashes", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(tmpDir, "sub", "file.txt")
		assert.True(t, v.Contains(path))
	})
}
