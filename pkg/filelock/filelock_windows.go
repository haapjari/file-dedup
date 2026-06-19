//go:build windows

package filelock

import (
	"fmt"
	"os"
	"unsafe"

	"syscall"
)

var (
	modkernel32      = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = modkernel32.NewProc("LockFileEx")
	procUnlockFileEx = modkernel32.NewProc("UnlockFileEx")
)

const (
	// lockfileExclusiveLock requests an exclusive lock.
	lockfileExclusiveLock = 0x02
	// lockfileFailImmediately makes the call non-blocking.
	lockfileFailImmediately = 0x01
)

// Acquire opens the file at path and obtains an exclusive advisory lock
// using LockFileEx. The call is non-blocking: if another process already
// holds the lock, Acquire returns an error immediately.
func Acquire(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}

	if err := lockFileEx(syscall.Handle(f.Fd())); err != nil {
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

	unlockErr := unlockFileEx(syscall.Handle(l.file.Fd()))
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

func lockFileEx(h syscall.Handle) error {
	var ol syscall.Overlapped
	r1, _, err := procLockFileEx.Call(
		uintptr(h),
		uintptr(lockfileExclusiveLock|lockfileFailImmediately),
		0,
		1,
		0,
		uintptr(unsafe.Pointer(&ol)),
	)
	if r1 == 0 {
		return err
	}
	return nil
}

func unlockFileEx(h syscall.Handle) error {
	var ol syscall.Overlapped
	r1, _, err := procUnlockFileEx.Call(
		uintptr(h),
		0,
		1,
		0,
		uintptr(unsafe.Pointer(&ol)),
	)
	if r1 == 0 {
		return err
	}
	return nil
}
