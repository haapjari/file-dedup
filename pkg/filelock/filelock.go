// Package filelock provides advisory file locking to prevent concurrent
// file-dedup processes from operating on the same target directory.
package filelock

import "os"

// Lock represents an acquired advisory file lock.
type Lock struct {
	file *os.File
}
