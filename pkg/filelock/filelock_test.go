package filelock

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAcquire_AndClose(t *testing.T) {
	t.Parallel()

	lockPath := filepath.Join(t.TempDir(), "test.lock")

	lock, err := Acquire(lockPath)
	require.NoError(t, err, "first acquire should succeed")
	require.NotNil(t, lock)

	// Lock file should exist while held.
	_, err = os.Stat(lockPath)
	require.NoError(t, err, "lock file should exist while held")

	err = lock.Close()
	require.NoError(t, err, "release should succeed")

	// Lock file should be removed after release.
	_, err = os.Stat(lockPath)
	assert.True(t, os.IsNotExist(err), "lock file should be removed after release")
}

func TestAcquire_SecondAcquireFails(t *testing.T) {
	t.Parallel()

	lockPath := filepath.Join(t.TempDir(), "test.lock")

	lock1, err := Acquire(lockPath)
	require.NoError(t, err, "first acquire should succeed")

	t.Cleanup(func() {
		_ = lock1.Close()
	})

	lock2, err := Acquire(lockPath)
	require.Error(t, err, "second acquire should fail while first is held")
	assert.Nil(t, lock2)
	assert.Contains(t, err.Error(), "acquire lock")
}

func TestClose_NilLock(t *testing.T) {
	t.Parallel()

	var lock *Lock
	err := lock.Close()
	assert.NoError(t, err, "release on nil lock should be no-op")
}

func TestClose_NilFile(t *testing.T) {
	t.Parallel()

	lock := &Lock{file: nil}
	err := lock.Close()
	assert.NoError(t, err, "release on lock with nil file should be no-op")
}

func TestAcquire_AfterClose(t *testing.T) {
	t.Parallel()

	lockPath := filepath.Join(t.TempDir(), "test.lock")

	lock1, err := Acquire(lockPath)
	require.NoError(t, err, "first acquire should succeed")

	err = lock1.Close()
	require.NoError(t, err, "release should succeed")

	// Re-acquire should work after release.
	lock2, err := Acquire(lockPath)
	require.NoError(t, err, "re-acquire after release should succeed")
	require.NotNil(t, lock2)

	err = lock2.Close()
	require.NoError(t, err, "second release should succeed")
}

func TestAcquire_CreatesParentDirectory(t *testing.T) {
	t.Parallel()

	// Acquire should work when the parent directory already exists
	// but the lock file does not.
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "lock")

	lock, err := Acquire(lockPath)
	require.NoError(t, err)

	err = lock.Close()
	require.NoError(t, err)
}

func TestAcquire_InvalidPath(t *testing.T) {
	t.Parallel()

	// Try acquiring a lock in a non-existent directory.
	lockPath := filepath.Join(t.TempDir(), "nonexistent", "subdir", "test.lock")

	lock, err := Acquire(lockPath)
	require.Error(t, err, "acquire should fail for invalid path")
	assert.Nil(t, lock)
	assert.Contains(t, err.Error(), "open lock file")
}
