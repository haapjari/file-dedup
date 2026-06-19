// Package hasher provides SHA256 file hashing utilities with parallel processing support.
package hasher

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"runtime"
	"sync"
)

const (
	// PartialHashSize is the number of bytes to read from start and end for partial hash.
	// 4 KiB aligns with common filesystem block size and keeps partial-hash reads small.
	PartialHashSize = 4096
	// SmallFileThreshold - files smaller than this skip partial hash and go straight to full hash.
	SmallFileThreshold = PartialHashSize * 2
)

// HashResult contains the result of hashing a single file.
type HashResult struct {
	Path  string
	Hash  string
	Size  int64
	Error error
}

// Hasher computes SHA256 hashes of files with optional parallel processing.
type Hasher struct {
	workers int
}

// Option configures a Hasher.
type Option func(*Hasher)

// WithWorkers sets the number of worker goroutines for parallel hashing.
// Default is runtime.NumCPU().
func WithWorkers(n int) Option {
	return func(h *Hasher) {
		if n > 0 {
			h.workers = n
		}
	}
}

// New creates a new Hasher with the given options.
func New(opts ...Option) *Hasher {
	h := &Hasher{
		workers: runtime.NumCPU(),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// ComputeHash computes the full SHA256 hash of a file.
func (h *Hasher) ComputeHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", err
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

// ComputePartialHash computes hash of first and last PartialHashSize bytes.
// This is much faster than full hash for large files and catches most differences.
// For files smaller than SmallFileThreshold, it hashes the entire file.
func (h *Hasher) ComputePartialHash(path string, size int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	hash := sha256.New()

	// Read first chunk.
	buf := make([]byte, PartialHashSize)
	n, err := f.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	hash.Write(buf[:n])

	// Read last chunk (if file is large enough to have distinct last chunk).
	if size > PartialHashSize {
		_, err = f.Seek(-PartialHashSize, io.SeekEnd)
		if err != nil {
			return "", err
		}
		n, err = f.Read(buf)
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		hash.Write(buf[:n])
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

// HashFiles computes hashes for multiple files concurrently.
// Returns a channel that will receive HashResult for each file.
// The channel is closed when all files have been processed.
func (h *Hasher) HashFiles(paths []string) <-chan HashResult {
	files := make([]FileToHash, len(paths))
	for i, path := range paths {
		files[i] = FileToHash{Path: path}
	}

	return h.hashFilesConcurrently(files, func(file FileToHash) HashResult {
		hash, err := h.ComputeHash(file.Path)
		var size int64
		if err == nil {
			if info, statErr := os.Stat(file.Path); statErr == nil {
				size = info.Size()
			}
		}

		return HashResult{
			Path:  file.Path,
			Hash:  hash,
			Size:  size,
			Error: err,
		}
	})
}

// HashFilesWithInfo computes hashes for files with known sizes concurrently.
// Uses partial hashing for pre-filtering when files have the same size.
type FileToHash struct {
	Path string
	Size int64
}

// HashFilesWithSizes computes hashes for files with known sizes.
// This allows the hasher to use size information for optimizations.
func (h *Hasher) HashFilesWithSizes(files []FileToHash) <-chan HashResult {
	return h.hashFilesConcurrently(files, func(file FileToHash) HashResult {
		hash, err := h.ComputeHash(file.Path)
		return HashResult{
			Path:  file.Path,
			Hash:  hash,
			Size:  file.Size,
			Error: err,
		}
	})
}

// HashPartialFilesWithSizes computes partial hashes for files with known sizes.
// This allows parallel pre-filtering for large file comparisons.
func (h *Hasher) HashPartialFilesWithSizes(files []FileToHash) <-chan HashResult {
	return h.hashFilesConcurrently(files, func(file FileToHash) HashResult {
		hash, err := h.ComputePartialHash(file.Path, file.Size)
		return HashResult{
			Path:  file.Path,
			Hash:  hash,
			Size:  file.Size,
			Error: err,
		}
	})
}

func (h *Hasher) hashFilesConcurrently(files []FileToHash, hashFn func(FileToHash) HashResult) <-chan HashResult {
	results := make(chan HashResult, h.workers)

	go func() {
		defer close(results)

		work := make(chan FileToHash, h.workers)

		var wg sync.WaitGroup
		for range h.workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for file := range work {
					results <- hashFn(file)
				}
			}()
		}

		for _, file := range files {
			work <- file
		}
		close(work)

		wg.Wait()
	}()

	return results
}

// Workers returns the number of worker goroutines configured.
func (h *Hasher) Workers() int {
	return h.workers
}
