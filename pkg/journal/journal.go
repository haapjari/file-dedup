// Package journal provides append-only mutation logging for undo support
// and operation auditing. Filesystem mutations are recorded after execution
// completes, with intent and confirmation entries written as pairs.
// The journal enables reversal of completed operations via the undo command.
// Note: the journal does not provide crash recovery for in-flight operations;
// pre-operation manifest snapshots serve that forensic role.
package journal

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// Entry represents a single filesystem mutation logged to the journal.
type Entry struct {
	Timestamp time.Time `json:"ts"`
	Type      string    `json:"type"`           // "trash", "replace", "rename", "mkdir", "extract"
	Source    string    `json:"src"`            // original path (relative to root)
	Dest      string    `json:"dst,omitempty"`  // new path (relative to root)
	Hash      string    `json:"hash,omitempty"` // content hash at time of operation
	Success   bool      `json:"ok"`             // true after mutation completes
}

// Writer appends journal entries to a JSONL file. Each Log call writes one
// JSON line and calls file.Sync() to ensure durability.
//
// Writer is safe for concurrent use.
type Writer struct {
	file    *os.File
	encoder *json.Encoder
	mu      sync.Mutex
}

// NewWriter creates a journal writer at the given path. The parent directory
// must already exist. The file is created if it does not exist, or appended to
// if it does.
func NewWriter(path string) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open journal: %w", err)
	}

	return &Writer{
		file:    f,
		encoder: json.NewEncoder(f),
	}, nil
}

// Log writes an entry to the journal and syncs to disk.
func (w *Writer) Log(entry Entry) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now().UTC()
	}

	if err := w.encoder.Encode(entry); err != nil {
		return fmt.Errorf("encode journal entry: %w", err)
	}

	if err := w.file.Sync(); err != nil {
		return fmt.Errorf("sync journal: %w", err)
	}

	return nil
}

// Close closes the underlying file.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.file.Close()
}

// Reader reads journal entries from a JSONL file.
type Reader struct {
	path string
}

// NewReader creates a journal reader for the given path.
func NewReader(path string) *Reader {
	return &Reader{path: path}
}

// Entries reads all entries from the journal in order.
func (r *Reader) Entries() ([]Entry, error) {
	f, err := os.Open(r.path)
	if err != nil {
		return nil, fmt.Errorf("open journal: %w", err)
	}
	defer f.Close()

	var entries []Entry
	scanner := bufio.NewScanner(f)
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var entry Entry
		if err := json.Unmarshal(line, &entry); err != nil {
			return entries, fmt.Errorf("decode journal line %d: %w", lineNum, err)
		}

		entries = append(entries, entry)
	}

	if err := scanner.Err(); err != nil {
		return entries, fmt.Errorf("read journal: %w", err)
	}

	return entries, nil
}

// EntriesReverse reads all entries and returns them in reverse order,
// suitable for rollback operations.
func (r *Reader) EntriesReverse() ([]Entry, error) {
	entries, err := r.Entries()
	if err != nil {
		return nil, err
	}

	reverseEntries(entries)
	return entries, nil
}

// ErrPartialWrite is returned when the journal contains entries without a
// corresponding Success: true confirmation, indicating a possible crash
// during mutation.
var ErrPartialWrite = errors.New("journal contains unconfirmed entries")

// Validate checks journal integrity. It returns ErrPartialWrite if any
// mutation was logged without a subsequent success confirmation.
func (r *Reader) Validate() error {
	entries, err := r.Entries()
	if err != nil {
		return err
	}

	// Track pending operations by source+type. An operation is pending if
	// we see a non-success entry but never see a matching success entry.
	type opKey struct {
		typ string
		src string
	}

	pending := make(map[opKey]bool)

	for i := range entries {
		key := opKey{typ: entries[i].Type, src: entries[i].Source}
		if entries[i].Success {
			delete(pending, key)
		} else {
			pending[key] = true
		}
	}

	if len(pending) > 0 {
		return ErrPartialWrite
	}

	return nil
}

func reverseEntries(entries []Entry) {
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
}
