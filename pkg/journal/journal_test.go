package journal

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriter_Log_WritesEntries(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "test.jsonl")
	w, err := NewWriter(path)
	require.NoError(t, err)
	defer w.Close()

	ts := time.Date(2026, 2, 8, 14, 30, 0, 0, time.UTC)

	require.NoError(t, w.Log(Entry{
		Timestamp: ts,
		Type:      "trash",
		Source:    "photos/vacation.jpg",
		Dest:      ".file-dedup/trash/run-1/photos/vacation.jpg",
	}))

	require.NoError(t, w.Log(Entry{
		Timestamp: ts,
		Type:      "trash",
		Source:    "photos/vacation.jpg",
		Dest:      ".file-dedup/trash/run-1/photos/vacation.jpg",
		Success:   true,
	}))

	r := NewReader(path)
	entries, err := r.Entries()
	require.NoError(t, err)
	require.Len(t, entries, 2)

	assert.Equal(t, "trash", entries[0].Type)
	assert.Equal(t, "photos/vacation.jpg", entries[0].Source)
	assert.False(t, entries[0].Success)

	assert.Equal(t, "trash", entries[1].Type)
	assert.True(t, entries[1].Success)
}

func TestWriter_Log_SetsTimestampWhenZero(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "test.jsonl")
	w, err := NewWriter(path)
	require.NoError(t, err)
	defer w.Close()

	before := time.Now().UTC()
	require.NoError(t, w.Log(Entry{Type: "rename", Source: "a.txt", Dest: "b.txt"}))
	after := time.Now().UTC()

	r := NewReader(path)
	entries, err := r.Entries()
	require.NoError(t, err)
	require.Len(t, entries, 1)

	assert.False(t, entries[0].Timestamp.Before(before), "timestamp should be >= before")
	assert.False(t, entries[0].Timestamp.After(after), "timestamp should be <= after")
}

func TestReader_Entries_EmptyFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "empty.jsonl")
	require.NoError(t, os.WriteFile(path, nil, 0o600))

	r := NewReader(path)
	entries, err := r.Entries()
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestReader_Entries_NonexistentFile(t *testing.T) {
	t.Parallel()

	r := NewReader(filepath.Join(t.TempDir(), "missing.jsonl"))
	_, err := r.Entries()
	require.Error(t, err)
}

func TestReader_EntriesReverse(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "test.jsonl")
	w, err := NewWriter(path)
	require.NoError(t, err)

	ts := time.Date(2026, 2, 8, 14, 30, 0, 0, time.UTC)

	require.NoError(t, w.Log(Entry{Timestamp: ts, Type: "rename", Source: "first.txt"}))
	require.NoError(t, w.Log(Entry{Timestamp: ts, Type: "rename", Source: "second.txt"}))
	require.NoError(t, w.Log(Entry{Timestamp: ts, Type: "rename", Source: "third.txt"}))
	require.NoError(t, w.Close())

	r := NewReader(path)
	entries, err := r.EntriesReverse()
	require.NoError(t, err)
	require.Len(t, entries, 3)

	assert.Equal(t, "third.txt", entries[0].Source)
	assert.Equal(t, "second.txt", entries[1].Source)
	assert.Equal(t, "first.txt", entries[2].Source)
}

func TestReader_Validate_Complete(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "test.jsonl")
	w, err := NewWriter(path)
	require.NoError(t, err)

	// Log a complete operation: intent + success.
	require.NoError(t, w.Log(Entry{Type: "trash", Source: "file.txt"}))
	require.NoError(t, w.Log(Entry{Type: "trash", Source: "file.txt", Success: true}))
	require.NoError(t, w.Close())

	r := NewReader(path)
	require.NoError(t, r.Validate())
}

func TestReader_Validate_Partial(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "test.jsonl")
	w, err := NewWriter(path)
	require.NoError(t, err)

	// Log only the intent, no success confirmation (simulates crash).
	require.NoError(t, w.Log(Entry{Type: "trash", Source: "file.txt"}))
	require.NoError(t, w.Close())

	r := NewReader(path)
	err = r.Validate()
	require.ErrorIs(t, err, ErrPartialWrite)
}

func TestReader_Validate_EmptyJournal(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "empty.jsonl")
	require.NoError(t, os.WriteFile(path, nil, 0o600))

	r := NewReader(path)
	require.NoError(t, r.Validate())
}

func TestReader_Validate_MultipleOperations(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "test.jsonl")
	w, err := NewWriter(path)
	require.NoError(t, err)

	// Two complete operations and one incomplete.
	require.NoError(t, w.Log(Entry{Type: "trash", Source: "a.txt"}))
	require.NoError(t, w.Log(Entry{Type: "trash", Source: "a.txt", Success: true}))
	require.NoError(t, w.Log(Entry{Type: "rename", Source: "b.txt", Dest: "c.txt"}))
	require.NoError(t, w.Log(Entry{Type: "rename", Source: "b.txt", Dest: "c.txt", Success: true}))
	require.NoError(t, w.Log(Entry{Type: "trash", Source: "d.txt"}))
	// d.txt never confirmed — simulates crash.
	require.NoError(t, w.Close())

	r := NewReader(path)
	err = r.Validate()
	require.ErrorIs(t, err, ErrPartialWrite)
}

func TestWriter_Log_HashAndOptionalFields(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "test.jsonl")
	w, err := NewWriter(path)
	require.NoError(t, err)
	defer w.Close()

	require.NoError(t, w.Log(Entry{
		Type:   "trash",
		Source: "file.txt",
		Hash:   "abc123",
	}))

	r := NewReader(path)
	entries, err := r.Entries()
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "abc123", entries[0].Hash)
}

func TestReader_Entries_CorruptedLine(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "corrupt.jsonl")
	content := "{\"type\":\"trash\",\"src\":\"ok.txt\",\"ok\":true}\nnot-json\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	r := NewReader(path)
	entries, err := r.Entries()
	require.Error(t, err, "should fail on corrupted line")
	assert.Contains(t, err.Error(), "line 2")
	// Should have parsed the first valid entry.
	require.Len(t, entries, 1)
	assert.Equal(t, "trash", entries[0].Type)
}

func TestReader_Entries_SkipsBlankLinesAndCountsLineNumbers(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "blank.jsonl")
	content := "\n{\"type\":\"trash\",\"src\":\"ok.txt\",\"ok\":true}\n\nnot-json\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	r := NewReader(path)
	entries, err := r.Entries()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "line 4")
	require.Len(t, entries, 1)
	assert.Equal(t, "ok.txt", entries[0].Source)
}

func TestReader_EntriesReverseReturnsEntriesError(t *testing.T) {
	t.Parallel()

	entries, err := NewReader(filepath.Join(t.TempDir(), "missing.jsonl")).EntriesReverse()
	require.Error(t, err)
	assert.Nil(t, entries)
}

func TestReader_ValidateReturnsEntriesError(t *testing.T) {
	t.Parallel()

	err := NewReader(filepath.Join(t.TempDir(), "missing.jsonl")).Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "open journal")
}

func TestReverseEntriesEvenLength(t *testing.T) {
	t.Parallel()

	entries := []Entry{
		{Source: "first.txt"},
		{Source: "second.txt"},
		{Source: "third.txt"},
		{Source: "fourth.txt"},
	}
	reverseEntries(entries)

	assert.Equal(t, []Entry{
		{Source: "fourth.txt"},
		{Source: "third.txt"},
		{Source: "second.txt"},
		{Source: "first.txt"},
	}, entries)
}

func TestWriter_ConcurrentWrites(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "concurrent.jsonl")
	w, err := NewWriter(path)
	require.NoError(t, err)
	defer w.Close()

	const numWriters = 10
	const entriesPerWriter = 20

	done := make(chan struct{})
	for range numWriters {
		go func() {
			defer func() { done <- struct{}{} }()
			for range entriesPerWriter {
				_ = w.Log(Entry{Type: "rename", Source: "file.txt"})
			}
		}()
	}

	for range numWriters {
		<-done
	}

	r := NewReader(path)
	entries, err := r.Entries()
	require.NoError(t, err)
	assert.Len(t, entries, numWriters*entriesPerWriter)
}
