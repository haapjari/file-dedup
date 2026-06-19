//go:build !windows

package filelock

import (
	"fmt"
	"os"
	"syscall"
)

// Acquire opens the file at path and obtains an exclusive advisory lock
// using flock(2). The call is non-blocking: if another process already
// holds the lock, Acquire returns an error immediately.
func Acquire(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("acquire lock: %w", err)
	}

	return &Lock{file: f}, nil
}

// Close releases the advisory lock, closes the file, and removes it.
// It is safe to call Close on a nil Lock (no-op).
func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}

	path := l.file.Name()

	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	removeErr := os.Remove(path)

	if unlockErr != nil {
		return fmt.Errorf("unlock: %w", unlockErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close lock file: %w", closeErr)
	}
	if removeErr != nil && !os.IsNotExist(removeErr) {
		return fmt.Errorf("remove lock file: %w", removeErr)
	}

	return nil
}
