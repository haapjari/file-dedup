package unzipper

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPatchedReaderAt(t *testing.T) {
	base := bytes.NewReader([]byte("abcdefghijkl"))
	r := &patchedReaderAt{
		base:        base,
		patchOffset: 3,
		patchBytes:  []byte("XYZ"),
	}

	buf := make([]byte, 12)
	n, err := r.ReadAt(buf, 0)
	require.NoError(t, err)
	require.Equal(t, 12, n)
	assert.Equal(t, "abcXYZghijkl", string(buf))

	buf = make([]byte, 4)
	n, err = r.ReadAt(buf, 2)
	require.NoError(t, err)
	require.Equal(t, 4, n)
	assert.Equal(t, "cXYZ", string(buf))
}

func TestPatchedReaderAt_BoundariesAndEOF(t *testing.T) {
	base := bytes.NewReader([]byte("abcdefghijkl"))
	r := &patchedReaderAt{
		base:        base,
		patchOffset: 4,
		patchBytes:  []byte("WXYZ"),
	}

	buf := make([]byte, 2)
	n, err := r.ReadAt(buf, 0)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.Equal(t, "ab", string(buf))

	buf = make([]byte, 2)
	n, err = r.ReadAt(buf, 8)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.Equal(t, "ij", string(buf))

	buf = make([]byte, 6)
	n, err = r.ReadAt(buf, 6)
	require.NoError(t, err)
	assert.Equal(t, 6, n)
	assert.Equal(t, "YZijkl", string(buf))

	buf = make([]byte, 4)
	n, err = r.ReadAt(buf, int64(len("abcdefghijkl")))
	assert.Equal(t, 0, n)
	assert.ErrorIs(t, err, io.EOF)
}

func TestPatchedReaderAt_ExactPatchBoundaries(t *testing.T) {
	base := bytes.NewReader([]byte("abcdefghijkl"))
	r := &patchedReaderAt{
		base:        base,
		patchOffset: 4,
		patchBytes:  []byte("WXYZ"),
	}

	buf := make([]byte, 4)
	n, err := r.ReadAt(buf, 0)
	require.NoError(t, err)
	assert.Equal(t, 4, n)
	assert.Equal(t, "abcd", string(buf))

	buf = make([]byte, 4)
	n, err = r.ReadAt(buf, 8)
	require.NoError(t, err)
	assert.Equal(t, 4, n)
	assert.Equal(t, "ijkl", string(buf))

	buf = make([]byte, 4)
	n, err = r.ReadAt(buf, 2)
	require.NoError(t, err)
	assert.Equal(t, 4, n)
	assert.Equal(t, "cdWX", string(buf))

	buf = make([]byte, 4)
	n, err = r.ReadAt(buf, 6)
	require.NoError(t, err)
	assert.Equal(t, 4, n)
	assert.Equal(t, "YZij", string(buf))
}

func TestArchiveReaderCloseNilAndProvidedCloser(t *testing.T) {
	var nilReader *archiveReader
	assert.NoError(t, nilReader.Close())
	assert.NoError(t, (&archiveReader{}).Close())

	closed := false
	reader := &archiveReader{closeFn: func() error {
		closed = true
		return nil
	}}

	assert.NoError(t, reader.Close())
	assert.True(t, closed)
}

func TestDetectZip64LocatorTotalDisksZero(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "zip64.zip")
	createSparseZip64Archive(t, archivePath)

	offset, found, err := detectZip64LocatorTotalDisksZero(archivePath)
	require.NoError(t, err)
	assert.False(t, found)
	assert.Zero(t, offset)

	locatorOffset := setZip64LocatorTotalDisksZero(t, archivePath)

	offset, found, err = detectZip64LocatorTotalDisksZero(archivePath)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, locatorOffset, offset)
}

func TestZipEndOfCentralDirectoryRequiresZip64Sentinels(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func([]byte)
		require bool
	}{
		{
			name:    "none",
			mutate:  func([]byte) {},
			require: false,
		},
		{
			name: "records this disk",
			mutate: func(eocd []byte) {
				binary.LittleEndian.PutUint16(eocd[8:10], zip16Marker)
			},
			require: true,
		},
		{
			name: "records total",
			mutate: func(eocd []byte) {
				binary.LittleEndian.PutUint16(eocd[10:12], zip16Marker)
			},
			require: true,
		},
		{
			name: "directory size",
			mutate: func(eocd []byte) {
				binary.LittleEndian.PutUint32(eocd[12:16], zip32Marker)
			},
			require: true,
		},
		{
			name: "directory offset",
			mutate: func(eocd []byte) {
				binary.LittleEndian.PutUint32(eocd[16:20], zip32Marker)
			},
			require: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			eocd := make([]byte, zipEndOfCentralDirLen)
			tc.mutate(eocd)

			assert.Equal(t, tc.require, zipEndOfCentralDirectoryRequiresZip64(eocd))
		})
	}
}

func TestFindZipEndOfCentralDirectoryInBuffer_RequiresExactCommentLength(t *testing.T) {
	assert.Equal(t, 0, findZipEndOfCentralDirectoryInBuffer(makeEOCD(0)))

	buf := append([]byte("prefix"), makeEOCD(3)...)
	buf = append(buf, []byte("hey")...)
	assert.Equal(t, len("prefix"), findZipEndOfCentralDirectoryInBuffer(buf))

	falseSignature := append([]byte("prefix"), makeEOCD(5)...)
	falseSignature = append(falseSignature, append([]byte("middle"), makeEOCD(0)...)...)
	assert.Equal(t, len("prefix")+zipEndOfCentralDirLen+len("middle"), findZipEndOfCentralDirectoryInBuffer(falseSignature))

	wrongComment := append([]byte("prefix"), makeEOCD(2)...)
	wrongComment = append(wrongComment, []byte("hey")...)
	assert.Equal(t, -1, findZipEndOfCentralDirectoryInBuffer(wrongComment))

	tooShort := make([]byte, zipEndOfCentralDirLen-1)
	assert.Equal(t, -1, findZipEndOfCentralDirectoryInBuffer(tooShort))
}

func TestFindZipEndOfCentralDirectory_SizeAndReadErrors(t *testing.T) {
	offset, found, err := findZipEndOfCentralDirectory(bytes.NewReader([]byte("short")), int64(len("short")))
	require.NoError(t, err)
	assert.False(t, found)
	assert.Zero(t, offset)

	payload := append([]byte("prefix"), makeEOCD(0)...)
	offset, found, err = findZipEndOfCentralDirectory(bytes.NewReader(payload), int64(len(payload)))
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, int64(len("prefix")), offset)

	_, _, err = findZipEndOfCentralDirectory(errorReaderAt{}, zipEndOfCentralDirLen)
	require.Error(t, err)
}

func TestFindZipEndOfCentralDirectory_OffsetZeroAndEOF(t *testing.T) {
	payload := makeEOCD(0)
	offset, found, err := findZipEndOfCentralDirectory(bytes.NewReader(payload), int64(len(payload)))
	require.NoError(t, err)
	assert.True(t, found)
	assert.Zero(t, offset)

	offset, found, err = findZipEndOfCentralDirectory(eofAfterReadReaderAt{data: payload}, int64(len(payload)))
	require.NoError(t, err)
	assert.True(t, found)
	assert.Zero(t, offset)
}

func TestReadZip64LocatorRecord_MalformedAndValidRecords(t *testing.T) {
	nonZip64 := makeEOCD(0)
	record, found, err := readZip64LocatorRecord(bytes.NewReader(nonZip64), int64(len(nonZip64)))
	require.NoError(t, err)
	assert.False(t, found)
	assert.Zero(t, record)

	missingLocator := makeEOCD(0)
	binary.LittleEndian.PutUint16(missingLocator[8:10], zip16Marker)
	record, found, err = readZip64LocatorRecord(bytes.NewReader(missingLocator), int64(len(missingLocator)))
	require.NoError(t, err)
	assert.False(t, found)
	assert.Zero(t, record)

	invalidLocator := append(make([]byte, zip64EndOfCentralDirLocatorLen), missingLocator...)
	record, found, err = readZip64LocatorRecord(bytes.NewReader(invalidLocator), int64(len(invalidLocator)))
	require.NoError(t, err)
	assert.False(t, found)
	assert.Zero(t, record)

	validLocator := make([]byte, zip64EndOfCentralDirLocatorLen)
	binary.LittleEndian.PutUint32(validLocator[0:4], zip64EndOfCentralDirLocSignature)
	binary.LittleEndian.PutUint32(validLocator[zip64LocatorDiskStartFieldOffset:zip64LocatorDiskStartFieldOffset+4], 2)
	binary.LittleEndian.PutUint32(validLocator[zip64LocatorTotalDisksFieldOffset:zip64LocatorTotalDisksFieldOffset+4], 3)
	payload := append(validLocator, missingLocator...)
	record, found, err = readZip64LocatorRecord(bytes.NewReader(payload), int64(len(payload)))
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, zip64LocatorRecord{offset: 0, diskStart: 2, totalDisks: 3}, record)
}

func TestOpenArchiveReaderWithZip64LocatorCompatibility(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "zip64_disks_zero.zip")
	createSparseZip64Archive(t, archivePath)
	setZip64LocatorTotalDisksZero(t, archivePath)

	standardReader, standardErr := zip.OpenReader(archivePath)
	if standardReader != nil {
		_ = standardReader.Close()
	}
	require.Error(t, standardErr)
	require.ErrorIs(t, standardErr, zip.ErrFormat)

	reader, err := openArchiveReader(archivePath)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, reader.Close())
	})

	if assert.Len(t, reader.files, 1) {
		assert.Equal(t, "hello.txt", reader.files[0].Name)
	}

	rc, err := reader.files[0].Open()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, rc.Close())
	})

	content, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "hello zip64", string(content))

	assert.True(t, isArchive(archivePath))
}

func TestExtractArchivesWithZip64LocatorCompatibilityAndDeflate64IsExtracted(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "zip64_disks_zero_deflate64.zip")
	createSparseZip64Deflate64Archive(t, archivePath, []byte("hello zip64 deflate64"))
	setZip64LocatorTotalDisksZero(t, archivePath)

	uz, err := New(root, false)
	require.NoError(t, err)

	files, err := getAllFilesRecursively(root)
	require.NoError(t, err)

	result, err := uz.ExtractArchivesWithProgressRecursively(files, nil)
	require.NoError(t, err)

	require.Len(t, result.Operations, 1)
	op := result.Operations[0]
	assert.False(t, op.Skipped)
	assert.True(t, op.ExtractionComplete)
	assert.Equal(t, 0, result.SkippedCount)
	assert.Equal(t, 1, result.ExtractedArchives)
	assert.Equal(t, 1, result.DeletedArchives)
	assert.Equal(t, 1, result.ExtractedFiles)

	content, readErr := os.ReadFile(filepath.Join(root, "hello.txt"))
	require.NoError(t, readErr)
	assert.Equal(t, "hello zip64 deflate64", string(content))

	_, statErr := os.Stat(archivePath)
	assert.True(t, os.IsNotExist(statErr), "deflate64 archive should be removed")
}

func TestOpenArchiveReaderWithZip64LocatorCompatibilityAndDeflate64DoesNotPatchFile(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "zip64_disks_zero_deflate64.zip")
	createSparseZip64Deflate64Archive(t, archivePath, []byte("hello zip64 deflate64"))
	locatorOffset := setZip64LocatorTotalDisksZero(t, archivePath)

	before := readFileRange(t, archivePath, locatorOffset, zip64EndOfCentralDirLocatorLen)

	reader, err := openArchiveReader(archivePath)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, reader.Close())
	})

	rc, err := reader.files[0].Open()
	require.NoError(t, err)
	content, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	assert.Equal(t, "hello zip64 deflate64", string(content))

	after := readFileRange(t, archivePath, locatorOffset, zip64EndOfCentralDirLocatorLen)
	assert.Equal(t, before, after, "ZIP64 compatibility patch must stay in memory")
}

func TestDetectZip64LocatorTotalDisksZeroErrorsAndSmallFiles(t *testing.T) {
	root := t.TempDir()

	offset, found, err := detectZip64LocatorTotalDisksZero(filepath.Join(root, "missing.zip"))
	require.Error(t, err)
	assert.False(t, found)
	assert.Zero(t, offset)

	smallPath := filepath.Join(root, "small.zip")
	require.NoError(t, os.WriteFile(smallPath, makeEOCD(0), 0o644))
	offset, found, err = detectZip64LocatorTotalDisksZero(smallPath)
	require.NoError(t, err)
	assert.False(t, found)
	assert.Zero(t, offset)
}

func TestOpenArchiveReaderErrors(t *testing.T) {
	root := t.TempDir()

	_, err := openArchiveReader(filepath.Join(root, "missing.zip"))
	require.Error(t, err)
	assert.False(t, errors.Is(err, zip.ErrFormat))

	corruptPath := filepath.Join(root, "corrupt.zip")
	require.NoError(t, os.WriteFile(corruptPath, []byte("not a zip"), 0o644))
	_, err = openArchiveReader(corruptPath)
	require.Error(t, err)
	require.ErrorIs(t, err, zip.ErrFormat)

	validPath := filepath.Join(root, "valid.zip")
	createSimpleZip(t, validPath, "hello.txt", []byte("hello"))
	reader, err := openArchiveReader(validPath)
	require.NoError(t, err)
	require.Len(t, reader.files, 1)
	assert.Equal(t, "hello.txt", reader.files[0].Name)
	require.NoError(t, reader.Close())
}

func TestOpenArchiveReaderWithZip64CompatibilityErrors(t *testing.T) {
	root := t.TempDir()

	_, err := openArchiveReaderWithZip64Compatibility(filepath.Join(root, "missing.zip"), 0)
	require.Error(t, err)

	corruptPath := filepath.Join(root, "corrupt.zip")
	require.NoError(t, os.WriteFile(corruptPath, []byte("not a zip"), 0o644))
	_, err = openArchiveReaderWithZip64Compatibility(corruptPath, 0)
	require.Error(t, err)
}

func TestReadZip64LocatorRecordReadErrors(t *testing.T) {
	_, found, err := readZip64LocatorRecord(errorReaderAt{}, zipEndOfCentralDirLen)
	require.Error(t, err)
	assert.False(t, found)

	eocdRequiresZip64 := makeEOCD(0)
	binary.LittleEndian.PutUint16(eocdRequiresZip64[8:10], zip16Marker)
	_, found, err = readZip64LocatorRecord(shortReaderAt{data: eocdRequiresZip64, failOffset: 0}, int64(len(eocdRequiresZip64)))
	require.Error(t, err)
	assert.False(t, found)

	payload := append(make([]byte, zip64EndOfCentralDirLocatorLen), eocdRequiresZip64...)
	_, found, err = readZip64LocatorRecord(shortReaderAt{data: payload, failOffset: 0}, int64(len(payload)))
	require.Error(t, err)
	assert.False(t, found)
}

func readFileRange(t *testing.T, filePath string, offset int64, length int) []byte {
	t.Helper()

	f, err := os.Open(filePath)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, f.Close())
	}()

	buf := make([]byte, length)
	_, err = f.ReadAt(buf, offset)
	require.NoError(t, err)

	return buf
}

func createSparseZip64Archive(t *testing.T, archivePath string) {
	t.Helper()

	const sparseOffset = int64(zip32Marker) + 128

	f, err := os.Create(archivePath)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, f.Close())
	}()

	_, err = f.Seek(sparseOffset, io.SeekStart)
	require.NoError(t, err)

	zw := zip.NewWriter(f)
	zw.SetOffset(sparseOffset)

	w, err := zw.Create("hello.txt")
	require.NoError(t, err)

	_, err = w.Write([]byte("hello zip64"))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
}

func createSparseZip64Deflate64Archive(t *testing.T, archivePath string, payload []byte) {
	t.Helper()

	const sparseOffset = int64(zip32Marker) + 256

	f, err := os.Create(archivePath)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, f.Close())
	}()

	_, err = f.Seek(sparseOffset, io.SeekStart)
	require.NoError(t, err)

	zw := zip.NewWriter(f)
	zw.SetOffset(sparseOffset)

	compressed := deflate64StoredBlock(t, payload)
	fh := &zip.FileHeader{
		Name:               "hello.txt",
		Method:             deflate64Method,
		CRC32:              crc32.ChecksumIEEE(payload),
		UncompressedSize64: uint64(len(payload)),
		CompressedSize64:   uint64(len(compressed)),
	}

	w, err := zw.CreateRaw(fh)
	require.NoError(t, err)

	_, err = w.Write(compressed)
	require.NoError(t, err)

	require.NoError(t, zw.Close())
}

func setZip64LocatorTotalDisksZero(t *testing.T, archivePath string) int64 {
	t.Helper()

	f, err := os.OpenFile(archivePath, os.O_RDWR, 0)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, f.Close())
	}()

	info, err := f.Stat()
	require.NoError(t, err)

	locator, found, err := readZip64LocatorRecord(f, info.Size())
	require.NoError(t, err)
	require.True(t, found)

	var raw [4]byte
	binary.LittleEndian.PutUint32(raw[:], 0)

	_, err = f.WriteAt(raw[:], locator.offset+zip64LocatorTotalDisksFieldOffset)
	require.NoError(t, err)

	return locator.offset
}

func makeEOCD(commentLen uint16) []byte {
	eocd := make([]byte, zipEndOfCentralDirLen)
	binary.LittleEndian.PutUint32(eocd[0:4], zipEndOfCentralDirSignature)
	binary.LittleEndian.PutUint16(eocd[zipEndOfCentralDirLen-2:zipEndOfCentralDirLen], commentLen)
	return eocd
}

func createSimpleZip(t *testing.T, archivePath, entryName string, payload []byte) {
	t.Helper()

	f, err := os.Create(archivePath)
	require.NoError(t, err)
	zw := zip.NewWriter(f)
	w, err := zw.Create(entryName)
	require.NoError(t, err)
	_, err = w.Write(payload)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	require.NoError(t, f.Close())
}

type errorReaderAt struct{}

func (errorReaderAt) ReadAt([]byte, int64) (int, error) {
	return 0, assert.AnError
}

type eofAfterReadReaderAt struct {
	data []byte
}

func (r eofAfterReadReaderAt) ReadAt(buf []byte, off int64) (int, error) {
	if off >= int64(len(r.data)) {
		return 0, io.EOF
	}

	n := copy(buf, r.data[off:])
	return n, io.EOF
}

type shortReaderAt struct {
	data       []byte
	failOffset int64
}

func (r shortReaderAt) ReadAt(buf []byte, off int64) (int, error) {
	if off == r.failOffset {
		return 0, assert.AnError
	}
	if off >= int64(len(r.data)) {
		return 0, io.EOF
	}
	n := copy(buf, r.data[off:])
	return n, nil
}
