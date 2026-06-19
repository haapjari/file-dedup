package testutil

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TempDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func CreateFile(t *testing.T, path, content string) {
	t.Helper()
	createFileBytes(t, path, []byte(content), 0o644, false, time.Time{})
}

func CreateFileWithModTime(t *testing.T, path, content string, modTime time.Time) {
	t.Helper()
	createFileBytes(t, path, []byte(content), 0o600, true, modTime)
}

func CreateFileBytesWithModTime(t *testing.T, path string, content []byte, modTime time.Time) {
	t.Helper()
	createFileBytes(t, path, content, 0o600, true, modTime)
}

func createFileBytes(t *testing.T, path string, content []byte, mode os.FileMode, setModTime bool, modTime time.Time) {
	t.Helper()

	err := os.MkdirAll(filepath.Dir(path), 0o755)
	require.NoError(t, err)

	err = os.WriteFile(path, content, mode)
	require.NoError(t, err)

	if !setModTime {
		return
	}

	err = os.Chtimes(path, modTime, modTime)
	require.NoError(t, err)
}
