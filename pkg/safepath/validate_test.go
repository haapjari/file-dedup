package safepath_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"file-dedup/pkg/collector"
	"file-dedup/pkg/safepath"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testErrorOp struct {
	Path string
	Err  error
}

func createTestFile(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, []byte("test"), 0o600))
	return p
}

func TestValidateReadPaths_AllSafe(t *testing.T) {
	dir := t.TempDir()
	v, err := safepath.New(dir)
	require.NoError(t, err)

	aPath := createTestFile(t, dir, "a.txt")
	bPath := createTestFile(t, dir, "b.txt")

	files := []collector.FileInfo{
		{Path: aPath, Dir: dir, Name: "a.txt", Size: 4, ModTime: time.Now()},
		{Path: bPath, Dir: dir, Name: "b.txt", Size: 4, ModTime: time.Now()},
	}

	safe, invalid := safepath.ValidateReadPaths(v, files, func(f collector.FileInfo, err error) testErrorOp {
		return testErrorOp{Path: f.Path, Err: err}
	})

	assert.Len(t, safe, 2)
	assert.Empty(t, invalid)
}

func TestValidateReadPaths_InvalidPath(t *testing.T) {
	dir := t.TempDir()
	v, err := safepath.New(dir)
	require.NoError(t, err)

	files := []collector.FileInfo{
		{Path: "/etc/passwd", Dir: "/etc", Name: "passwd", Size: 100, ModTime: time.Now()},
	}

	safe, invalid := safepath.ValidateReadPaths(v, files, func(f collector.FileInfo, err error) testErrorOp {
		return testErrorOp{Path: f.Path, Err: err}
	})

	assert.Empty(t, safe)
	assert.Len(t, invalid, 1)
	assert.Equal(t, "/etc/passwd", invalid[0].Path)
	assert.Error(t, invalid[0].Err)
}

func TestValidateReadPaths_MixedPaths(t *testing.T) {
	dir := t.TempDir()
	v, err := safepath.New(dir)
	require.NoError(t, err)

	goodPath := createTestFile(t, dir, "good.txt")

	files := []collector.FileInfo{
		{Path: goodPath, Dir: dir, Name: "good.txt", Size: 4, ModTime: time.Now()},
		{Path: "/outside/bad.txt", Dir: "/outside", Name: "bad.txt", Size: 20, ModTime: time.Now()},
	}

	safe, invalid := safepath.ValidateReadPaths(v, files, func(f collector.FileInfo, err error) testErrorOp {
		return testErrorOp{Path: f.Path, Err: err}
	})

	assert.Len(t, safe, 1)
	assert.Equal(t, "good.txt", safe[0].Name)
	assert.Len(t, invalid, 1)
	assert.Equal(t, "/outside/bad.txt", invalid[0].Path)
}

func TestValidateReadPaths_EmptyInput(t *testing.T) {
	dir := t.TempDir()
	v, err := safepath.New(dir)
	require.NoError(t, err)

	safe, invalid := safepath.ValidateReadPaths(v, nil, func(f collector.FileInfo, err error) testErrorOp {
		return testErrorOp{Path: f.Path, Err: err}
	})

	assert.Empty(t, safe)
	assert.Empty(t, invalid)
}
