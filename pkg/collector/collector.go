// Package collector provides file metadata collection utilities.
package collector

import (
	"os"
	"path/filepath"
	"time"
)

// FileInfo holds metadata about a file.
type FileInfo struct {
	Path    string    // Full path to the file
	Dir     string    // Directory containing the file
	Name    string    // Original filename
	Size    int64     // File size in bytes
	ModTime time.Time // Modification time
}

// Options configures the collector behavior.
type Options struct {
	// SkipFiles is a list of filenames to skip (e.g., script files, logs)
	SkipFiles []string
	// SkipDirs is a list of directory names to skip
	SkipDirs []string
}

// Collector collects file metadata from a directory tree.
type Collector struct {
	skipFiles map[string]bool
	skipDirs  map[string]bool
}

// New creates a new Collector with the given options.
func New(opts Options) *Collector {
	c := &Collector{
		skipFiles: make(map[string]bool),
		skipDirs:  make(map[string]bool),
	}

	for _, f := range opts.SkipFiles {
		c.skipFiles[f] = true
	}
	for _, d := range opts.SkipDirs {
		c.skipDirs[d] = true
	}

	return c
}

// Collect walks the directory tree and collects metadata for all files.
func (c *Collector) Collect(rootDir string) ([]FileInfo, error) {
	var files []FileInfo

	err := filepath.Walk(rootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip directories in skip list
		if info.IsDir() {
			if c.skipDirs[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip files in skip list
		if c.skipFiles[info.Name()] {
			return nil
		}

		files = append(files, FileInfo{
			Path:    path,
			Dir:     filepath.Dir(path),
			Name:    info.Name(),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		})

		return nil
	})

	if err != nil {
		return nil, err
	}

	return files, nil
}

// CollectFromDir collects files only from a specific directory (non-recursive).
func (c *Collector) CollectFromDir(dir string) ([]FileInfo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	files := make([]FileInfo, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		if c.skipFiles[entry.Name()] {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			return nil, err
		}

		fullPath := filepath.Join(dir, entry.Name())
		files = append(files, FileInfo{
			Path:    fullPath,
			Dir:     dir,
			Name:    info.Name(),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		})
	}

	return files, nil
}
