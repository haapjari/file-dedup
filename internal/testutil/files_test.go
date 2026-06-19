package testutil

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTempDir(t *testing.T) {
	dir := TempDir(t)
	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

func TestCreateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "file.txt")
	CreateFile(t, path, "hello")

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(content))
}

func TestCreateFileWithModTime(t *testing.T) {
	modTime := time.Date(2024, 2, 1, 10, 30, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "nested", "file.txt")
	CreateFileWithModTime(t, path, "content", modTime)

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "content", string(content))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.True(t, info.ModTime().Equal(modTime))
}

func TestCreateFileBytesWithModTime(t *testing.T) {
	modTime := time.Date(2025, 3, 5, 8, 15, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "nested", "file.bin")
	CreateFileBytesWithModTime(t, path, []byte{0x00, 0x01, 0x02}, modTime)

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, []byte{0x00, 0x01, 0x02}, content)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.True(t, info.ModTime().Equal(modTime))
}
