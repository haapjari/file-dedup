package hasher

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createTestFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	err := os.WriteFile(path, []byte(content), 0644)
	require.NoError(t, err)
	return path
}

func expectedHash(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

func TestNew(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		option      Option
		wantWorkers int
	}{
		{name: "default workers"},
		{name: "custom workers", option: WithWorkers(4), wantWorkers: 4},
		{name: "zero workers uses default", option: WithWorkers(0)},
		{name: "negative workers uses default", option: WithWorkers(-1)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var h *Hasher
			if tc.option == nil {
				h = New()
			} else {
				h = New(tc.option)
			}

			if tc.wantWorkers > 0 {
				assert.Equal(t, tc.wantWorkers, h.Workers())
				return
			}

			assert.Positive(t, h.Workers())
		})
	}
}

func TestHasher_ComputeHash(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
	}{
		{"empty file", ""},
		{"small file", "hello world"},
		{"medium file", string(make([]byte, 1024))},
		{"large file", string(make([]byte, 10*1024))},
		{"binary content", "\x00\x01\x02\x03\xff\xfe\xfd"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tmpDir := t.TempDir()
			path := createTestFile(t, tmpDir, "test.txt", tt.content)

			h := New()
			hash, err := h.ComputeHash(path)

			require.NoError(t, err)
			assert.Equal(t, expectedHash(tt.content), hash)
			assert.Len(t, hash, 64) // SHA256 hex is 64 chars
		})
	}
}

func TestHasher_ComputeHash_ErrorScenarios(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	tests := []struct {
		name string
		path string
	}{
		{name: "non-existent file", path: "/nonexistent/path/file.txt"},
		{name: "directory read error", path: tmpDir},
	}

	h := New()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := h.ComputeHash(tc.path)
			assert.Error(t, err)
		})
	}
}

func TestHasher_ComputePartialHash(t *testing.T) {
	t.Parallel()

	t.Run("small file hashes entirely", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		content := "small content"
		path := createTestFile(t, tmpDir, "small.txt", content)

		h := New()
		hash, err := h.ComputePartialHash(path, int64(len(content)))

		require.NoError(t, err)
		assert.Equal(t, expectedHash(content), hash)
		assert.Len(t, hash, 64)
	})

	t.Run("partial hash size boundary hashes once", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		content := string(make([]byte, PartialHashSize))
		path := createTestFile(t, tmpDir, "boundary.bin", content)

		h := New()
		hash, err := h.ComputePartialHash(path, int64(len(content)))

		require.NoError(t, err)
		assert.Equal(t, expectedHash(content), hash)
	})

	t.Run("large file uses start and end", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()

		// Create a file larger than 2*PartialHashSize
		content := make([]byte, PartialHashSize*3)
		for i := range content {
			content[i] = byte(i % 256)
		}
		path := createTestFile(t, tmpDir, "large.bin", string(content))

		h := New()
		hash, err := h.ComputePartialHash(path, int64(len(content)))

		require.NoError(t, err)
		assert.Len(t, hash, 64)

		// Partial hash should differ from full hash for large files
		fullHash, err := h.ComputeHash(path)
		require.NoError(t, err)
		assert.NotEqual(t, fullHash, hash, "partial hash should differ from full hash")
	})

	t.Run("same content same partial hash", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()

		content := make([]byte, PartialHashSize*3)
		path1 := createTestFile(t, tmpDir, "file1.bin", string(content))
		path2 := createTestFile(t, tmpDir, "file2.bin", string(content))

		h := New()
		hash1, err := h.ComputePartialHash(path1, int64(len(content)))
		require.NoError(t, err)
		hash2, err := h.ComputePartialHash(path2, int64(len(content)))
		require.NoError(t, err)

		assert.Equal(t, hash1, hash2)
	})

	t.Run("different endings different partial hash", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()

		content1 := make([]byte, PartialHashSize*3)
		content2 := make([]byte, PartialHashSize*3)
		// Same start, different end
		content2[len(content2)-1] = 0xFF

		path1 := createTestFile(t, tmpDir, "file1.bin", string(content1))
		path2 := createTestFile(t, tmpDir, "file2.bin", string(content2))

		h := New()
		hash1, err := h.ComputePartialHash(path1, int64(len(content1)))
		require.NoError(t, err)
		hash2, err := h.ComputePartialHash(path2, int64(len(content2)))
		require.NoError(t, err)

		assert.NotEqual(t, hash1, hash2)
	})
}

func TestHasher_ComputePartialHash_NonExistentFile(t *testing.T) {
	t.Parallel()

	h := New()
	_, err := h.ComputePartialHash("/nonexistent/path/file.txt", 10)
	assert.Error(t, err)
}

func TestHasher_ComputePartialHash_InvalidSizeTriggersSeekError(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	path := createTestFile(t, tmpDir, "small.txt", "abc")

	h := New()
	_, err := h.ComputePartialHash(path, PartialHashSize+1)
	assert.Error(t, err)
}

func TestHasher_HashFiles(t *testing.T) {
	t.Parallel()

	t.Run("multiple files", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()

		files := []struct {
			name    string
			content string
		}{
			{"file1.txt", "content 1"},
			{"file2.txt", "content 2"},
			{"file3.txt", "content 3"},
		}

		paths := make([]string, 0, len(files))
		expectedHashes := make(map[string]string)
		for _, f := range files {
			path := createTestFile(t, tmpDir, f.name, f.content)
			paths = append(paths, path)
			expectedHashes[path] = expectedHash(f.content)
		}

		h := New(WithWorkers(2))
		results := h.HashFiles(paths)

		gotHashes := make(map[string]string)
		gotSizes := make(map[string]int64)
		for result := range results {
			require.NoError(t, result.Error)
			gotHashes[result.Path] = result.Hash
			gotSizes[result.Path] = result.Size
		}

		assert.Equal(t, expectedHashes, gotHashes)
		for _, f := range files {
			path := filepath.Join(tmpDir, f.name)
			assert.Equal(t, int64(len(f.content)), gotSizes[path])
		}
	})

	t.Run("empty input", func(t *testing.T) {
		t.Parallel()
		h := New()
		results := h.HashFiles(nil)

		count := 0
		for range results {
			count++
		}
		assert.Equal(t, 0, count)
	})

	t.Run("handles errors", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		goodPath := createTestFile(t, tmpDir, "good.txt", "content")
		badPath := filepath.Join(tmpDir, "nonexistent.txt")

		h := New()
		results := h.HashFiles([]string{goodPath, badPath})

		var goodResult, badResult HashResult
		for result := range results {
			if result.Path == goodPath {
				goodResult = result
			} else {
				badResult = result
			}
		}

		require.NoError(t, goodResult.Error)
		assert.Equal(t, expectedHash("content"), goodResult.Hash)
		assert.Equal(t, int64(len("content")), goodResult.Size)

		require.Error(t, badResult.Error)
		assert.Empty(t, badResult.Hash)
		assert.Zero(t, badResult.Size)
	})
}

func TestHasher_HashFilesWithSizes(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	files := []struct {
		name    string
		content string
	}{
		{"a.txt", "aaa"},
		{"b.txt", "bbb"},
	}

	toHash := make([]FileToHash, 0, len(files))
	for _, f := range files {
		path := createTestFile(t, tmpDir, f.name, f.content)
		toHash = append(toHash, FileToHash{
			Path: path,
			Size: int64(len(f.content)),
		})
	}

	h := New()
	results := h.HashFilesWithSizes(toHash)

	count := 0
	for result := range results {
		require.NoError(t, result.Error)
		assert.Len(t, result.Hash, 64)
		assert.Equal(t, int64(3), result.Size)
		count++
	}
	assert.Equal(t, 2, count)
}

func TestHasher_HashPartialFilesWithSizes(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	files := []struct {
		name    string
		content string
	}{
		{"small.txt", "small content"},
		{"large.bin", string(make([]byte, PartialHashSize*3))},
	}

	h := New()
	toHash := make([]FileToHash, 0, len(files))
	expected := make(map[string]string, len(files))

	for _, f := range files {
		path := createTestFile(t, tmpDir, f.name, f.content)
		toHash = append(toHash, FileToHash{
			Path: path,
			Size: int64(len(f.content)),
		})

		hash, err := h.ComputePartialHash(path, int64(len(f.content)))
		require.NoError(t, err)
		expected[path] = hash
	}

	results := h.HashPartialFilesWithSizes(toHash)

	got := make(map[string]string)
	for result := range results {
		require.NoError(t, result.Error)
		got[result.Path] = result.Hash
	}

	assert.Equal(t, expected, got)
}

func TestHasher_HashFiles_Parallel_Correctness(t *testing.T) {
	t.Parallel()

	// Create many files to stress parallel processing
	tmpDir := t.TempDir()
	numFiles := 100

	paths := make([]string, 0, numFiles)
	expectedHashes := make(map[string]string)

	for i := range numFiles {
		content := string(rune('a'+i%26)) + string(make([]byte, i))
		name := filepath.Join(tmpDir, "file_"+string(rune('0'+i/10))+string(rune('0'+i%10))+".txt")

		err := os.WriteFile(name, []byte(content), 0644)
		require.NoError(t, err)

		paths = append(paths, name)
		expectedHashes[name] = expectedHash(content)
	}

	h := New(WithWorkers(8))
	results := h.HashFiles(paths)

	gotHashes := make(map[string]string)
	for result := range results {
		require.NoError(t, result.Error, "file: %s", result.Path)
		gotHashes[result.Path] = result.Hash
	}

	assert.Len(t, gotHashes, len(expectedHashes))
	for path, expected := range expectedHashes {
		assert.Equal(t, expected, gotHashes[path], "hash mismatch for %s", path)
	}
}

func TestHasher_HashFiles_NoRaceCondition(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	numFiles := 50

	paths := make([]string, 0, numFiles)
	for i := range numFiles {
		content := make([]byte, 1024)
		for j := range content {
			content[j] = byte((i + j) % 256)
		}
		path := filepath.Join(tmpDir, "file_"+string(rune('0'+i/10))+string(rune('0'+i%10))+".bin")
		err := os.WriteFile(path, content, 0644)
		require.NoError(t, err)
		paths = append(paths, path)
	}

	// Run multiple times to increase chance of catching race conditions
	for range 5 {
		h := New(WithWorkers(16))
		results := h.HashFiles(paths)

		var mu sync.Mutex
		hashes := make(map[string]string)

		for result := range results {
			require.NoError(t, result.Error)
			mu.Lock()
			hashes[result.Path] = result.Hash
			mu.Unlock()
		}

		assert.Len(t, hashes, numFiles)
	}
}

func TestHasher_SameContentSameHash(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	content := "identical content for both files"

	path1 := createTestFile(t, tmpDir, "file1.txt", content)
	path2 := createTestFile(t, tmpDir, "file2.txt", content)

	h := New()
	hash1, err := h.ComputeHash(path1)
	require.NoError(t, err)

	hash2, err := h.ComputeHash(path2)
	require.NoError(t, err)

	assert.Equal(t, hash1, hash2, "same content must produce same hash")
}

func TestHasher_DifferentContentDifferentHash(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	path1 := createTestFile(t, tmpDir, "file1.txt", "content A")
	path2 := createTestFile(t, tmpDir, "file2.txt", "content B")

	h := New()
	hash1, err := h.ComputeHash(path1)
	require.NoError(t, err)

	hash2, err := h.ComputeHash(path2)
	require.NoError(t, err)

	assert.NotEqual(t, hash1, hash2, "different content must produce different hash")
}

func BenchmarkHasher_ComputeHash_SmallFile(b *testing.B) {
	tmpDir := b.TempDir()
	content := make([]byte, 1024) // 1KB
	path := filepath.Join(tmpDir, "small.bin")
	_ = os.WriteFile(path, content, 0644)

	h := New()
	b.ResetTimer()

	for range b.N {
		_, _ = h.ComputeHash(path)
	}
}

func BenchmarkHasher_ComputeHash_LargeFile(b *testing.B) {
	tmpDir := b.TempDir()
	content := make([]byte, 10*1024*1024) // 10MB
	path := filepath.Join(tmpDir, "large.bin")
	_ = os.WriteFile(path, content, 0644)

	h := New()
	b.ResetTimer()

	for range b.N {
		_, _ = h.ComputeHash(path)
	}
}

func BenchmarkHasher_HashFiles_Parallel(b *testing.B) {
	tmpDir := b.TempDir()
	numFiles := 100
	content := make([]byte, 100*1024) // 100KB each

	paths := make([]string, 0, numFiles)
	for i := range numFiles {
		path := filepath.Join(tmpDir, "file_"+string(rune('0'+i/10))+string(rune('0'+i%10))+".bin")
		_ = os.WriteFile(path, content, 0644)
		paths = append(paths, path)
	}

	h := New(WithWorkers(8))
	b.ResetTimer()

	for range b.N {
		for result := range h.HashFiles(paths) {
			_ = result // drain results
		}
	}
}
