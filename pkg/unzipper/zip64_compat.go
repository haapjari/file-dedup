// Some ZIP64 archives produced by OneDrive and built-in Windows ZIP tooling
// write the ZIP64 End of Central Directory Locator field
// "total number of disks" as 0 instead of 1 for a single-disk archive.
// Go's archive/zip rejects that as zip.ErrFormat, even though the archive
// is often otherwise readable by more permissive tools.
//
// References:
// - https://github.com/python/cpython/issues/66300
// - https://github.com/python/cpython/commit/ab0716ed1ea2957396054730afbb80c1825f9786
// - https://sourceforge.net/p/infozip/patches/28/
// - https://www.bitsgalore.org/2020/03/11/does-microsoft-onedrive-export-large-ZIP-files-that-are-corrupt
//
// Workaround:
// If this exact anomaly is detected, we apply a byte-level patch overlay in
// memory (ReaderAt wrapper) and re-open via archive/zip. The original archive
// file on disk is never modified.
package unzipper

import (
	"archive/zip"
	"encoding/binary"
	"errors"
	"io"
	"os"

	"github.com/haapjari/flate/pkg/flate"
)

const (
	// ZIP End of Central Directory record signature (PKZIP spec §4.3.16).
	zipEndOfCentralDirSignature = 0x06054b50

	// ZIP64 End of Central Directory Locator signature (PKZIP spec §4.3.15).
	zip64EndOfCentralDirLocSignature = 0x07064b50

	// Fixed size in bytes of the ZIP End of Central Directory record.
	zipEndOfCentralDirLen = 22

	// Fixed size in bytes of the ZIP64 End of Central Directory Locator record.
	zip64EndOfCentralDirLocatorLen = 20

	// Maximum backward search window from EOF to locate the End of Central
	// Directory record, accounting for the maximum 0xffff-byte comment field.
	zipEndOfCentralDirSearchWindowSize = zipEndOfCentralDirLen + 0xffff

	// Sentinel value (0xFFFF) indicating a field requires ZIP64 extensions
	// when found in a 16-bit EOCD field.
	zip16Marker = 0xffff

	// Sentinel value (0xFFFFFFFF) indicating a field requires ZIP64 extensions
	// when found in a 32-bit EOCD field.
	zip32Marker = 0xffffffff

	// Maximum file size representable by standard (non-ZIP64) ZIP fields.
	zip32SizeLimit = int64(zip32Marker)

	// Byte offset within the ZIP64 End of Central Directory Locator where
	// the "number of the disk with the start of the ZIP64 EOCD" field begins.
	zip64LocatorDiskStartFieldOffset = 4

	// Byte offset within the ZIP64 End of Central Directory Locator where
	// the "total number of disks" field begins.
	zip64LocatorTotalDisksFieldOffset = 16
)

// zip64LocatorSingleDiskValue is the correct little-endian encoding of the
// value 1 for the "total number of disks" field in the ZIP64 End of Central
// Directory Locator. It is used to patch archives where this field is
// erroneously written as 0 by OneDrive and Windows built-in ZIP tooling.
var zip64LocatorSingleDiskValue = [4]byte{1, 0, 0, 0}

// archiveReader wraps a slice of [zip.File] entries with a closer,
// providing a uniform interface for both standard and compatibility-mode
// ZIP archive access.
type archiveReader struct {
	files   []*zip.File
	closeFn func() error
}

// Close releases any resources held by the archiveReader.
// It is safe to call on a nil receiver or when no closer was provided.
func (r *archiveReader) Close() error {
	if r == nil || r.closeFn == nil {
		return nil
	}

	return r.closeFn()
}

// openArchiveReader opens a ZIP archive at filePath and returns an [archiveReader].
//
// It first attempts a standard open via [zip.OpenReader]. If that fails with
// [zip.ErrFormat], it checks whether the archive exhibits the known ZIP64
// "total disks == 0" anomaly produced by OneDrive and Windows built-in ZIP
// tooling. When detected, the archive is re-opened with a byte-level patch
// overlay that corrects the malformed field in memory without modifying the
// file on disk.
//
// Any non-format error from the initial open is returned immediately.
func openArchiveReader(filePath string) (*archiveReader, error) {
	r, err := zip.OpenReader(filePath)
	if err == nil {
		r.RegisterDecompressor(deflate64Method, flate.NewReader64)
		return &archiveReader{files: r.File, closeFn: r.Close}, nil
	}

	if !errors.Is(err, zip.ErrFormat) {
		return nil, err
	}

	locatorOffset, needsCompat, detectErr := detectZip64LocatorTotalDisksZero(filePath)
	if detectErr != nil || !needsCompat {
		return nil, err
	}

	compatReader, compatErr := openArchiveReaderWithZip64Compatibility(filePath, locatorOffset)
	if compatErr != nil {
		return nil, errors.Join(err, compatErr)
	}

	return compatReader, nil
}

// openArchiveReaderWithZip64Compatibility opens a ZIP archive using a patched
// [io.ReaderAt] that corrects the ZIP64 End of Central Directory Locator
// "total number of disks" field in memory. The locatorOffset must point to the
// start of the ZIP64 locator record. The original file on disk is never modified.
// The caller must call Close on the returned [archiveReader] to release the
// underlying file handle.
func openArchiveReaderWithZip64Compatibility(filePath string, locatorOffset int64) (*archiveReader, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}

	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}

	patchedReader := &patchedReaderAt{
		base:        f,
		patchOffset: locatorOffset + zip64LocatorTotalDisksFieldOffset,
		patchBytes:  zip64LocatorSingleDiskValue[:],
	}

	zr, err := zip.NewReader(patchedReader, info.Size())
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	zr.RegisterDecompressor(deflate64Method, flate.NewReader64)

	return &archiveReader{files: zr.File, closeFn: f.Close}, nil
}

// detectZip64LocatorTotalDisksZero checks whether the ZIP archive at filePath
// exhibits the known ZIP64 anomaly where the "total number of disks" field in
// the ZIP64 End of Central Directory Locator is written as 0 instead of 1.
// This anomaly is produced by OneDrive and Windows built-in ZIP tooling.
//
// Archives smaller than the ZIP32 size limit are skipped, since they cannot
// contain ZIP64 structures. When the anomaly is detected, the byte offset of
// the locator record and needsCompat == true are returned.
func detectZip64LocatorTotalDisksZero(filePath string) (locatorOffset int64, needsCompat bool, err error) {
	info, err := os.Stat(filePath)
	if err != nil {
		return 0, false, err
	}

	if info.Size() <= zip32SizeLimit {
		return 0, false, nil
	}

	f, err := os.Open(filePath)
	if err != nil {
		return 0, false, err
	}
	defer func() {
		_ = f.Close()
	}()

	locator, found, err := readZip64LocatorRecord(f, info.Size())
	if err != nil || !found {
		return 0, false, err
	}

	if locator.diskStart == 0 && locator.totalDisks == 0 {
		return locator.offset, true, nil
	}

	return 0, false, nil
}

// zip64LocatorRecord holds the parsed fields of interest from a ZIP64 End of
// Central Directory Locator record: the byte offset of the locator within the
// file, the disk number on which the ZIP64 EOCD starts, and the total number
// of disks.
type zip64LocatorRecord struct {
	offset     int64
	diskStart  uint32
	totalDisks uint32
}

// readZip64LocatorRecord locates and parses the ZIP64 End of Central Directory
// Locator from r, which must have the given total size in bytes. It first finds
// the standard End of Central Directory record, checks whether ZIP64 extensions
// are required, and then reads the locator that immediately precedes the EOCD.
// Returns found == false (with nil error) when the archive has no ZIP64
// locator or does not need one.
func readZip64LocatorRecord(r io.ReaderAt, size int64) (zip64LocatorRecord, bool, error) {
	// Locate the standard End of Central Directory record by scanning backwards from EOF.
	eocdOffset, found, err := findZipEndOfCentralDirectory(r, size)
	if err != nil || !found {
		return zip64LocatorRecord{}, false, err
	}

	// Read the full EOCD record into memory for field inspection.
	eocd := make([]byte, zipEndOfCentralDirLen)
	if _, err := r.ReadAt(eocd, eocdOffset); err != nil {
		return zip64LocatorRecord{}, false, err
	}

	// Check whether any EOCD fields contain sentinel values that indicate ZIP64 extensions are needed.
	if !zipEndOfCentralDirectoryRequiresZip64(eocd) {
		return zip64LocatorRecord{}, false, nil
	}

	// The ZIP64 locator sits immediately before the EOCD; compute its expected offset.
	locatorOffset := eocdOffset - zip64EndOfCentralDirLocatorLen
	if locatorOffset < 0 {
		return zip64LocatorRecord{}, false, nil
	}

	// Read the candidate ZIP64 End of Central Directory Locator record.
	locatorRaw := make([]byte, zip64EndOfCentralDirLocatorLen)
	if _, err := r.ReadAt(locatorRaw, locatorOffset); err != nil {
		return zip64LocatorRecord{}, false, err
	}

	// Verify the locator's magic signature to confirm it is a valid ZIP64 locator.
	if binary.LittleEndian.Uint32(locatorRaw[0:4]) != zip64EndOfCentralDirLocSignature {
		return zip64LocatorRecord{}, false, nil
	}

	// Parse and return the locator fields: offset, disk start number, and total disk count.
	return zip64LocatorRecord{
		offset:     locatorOffset,
		diskStart:  binary.LittleEndian.Uint32(locatorRaw[zip64LocatorDiskStartFieldOffset : zip64LocatorDiskStartFieldOffset+4]),
		totalDisks: binary.LittleEndian.Uint32(locatorRaw[zip64LocatorTotalDisksFieldOffset : zip64LocatorTotalDisksFieldOffset+4]),
	}, true, nil
}

// zipEndOfCentralDirectoryRequiresZip64 reports whether the given End of
// Central Directory record contains sentinel values (0xFFFF or 0xFFFFFFFF)
// indicating that the real values are stored in the ZIP64 extended structures.
func zipEndOfCentralDirectoryRequiresZip64(eocd []byte) bool {
	recordsThisDisk := binary.LittleEndian.Uint16(eocd[8:10])
	recordsTotal := binary.LittleEndian.Uint16(eocd[10:12])
	directorySize := binary.LittleEndian.Uint32(eocd[12:16])
	directoryOffset := binary.LittleEndian.Uint32(eocd[16:20])

	return recordsThisDisk == zip16Marker ||
		recordsTotal == zip16Marker ||
		directorySize == zip32Marker ||
		directoryOffset == zip32Marker
}

// findZipEndOfCentralDirectory searches backwards from the end of r for the
// ZIP End of Central Directory signature. It reads at most
// [zipEndOfCentralDirSearchWindowSize] bytes from the tail of the file.
// Returns the absolute byte offset of the EOCD record, or found == false if
// the signature is not present.
func findZipEndOfCentralDirectory(r io.ReaderAt, size int64) (offset int64, found bool, err error) {
	if size < zipEndOfCentralDirLen {
		return 0, false, nil
	}

	windowSize := min(size, zipEndOfCentralDirSearchWindowSize)

	buf := make([]byte, windowSize)
	if _, err := r.ReadAt(buf, size-windowSize); err != nil && !errors.Is(err, io.EOF) {
		return 0, false, err
	}

	idx := findZipEndOfCentralDirectoryInBuffer(buf)
	if idx < 0 {
		return 0, false, nil
	}

	return size - windowSize + int64(idx), true, nil
}

// findZipEndOfCentralDirectoryInBuffer scans buf backwards for the ZIP End of
// Central Directory signature (0x06054b50). It validates each candidate by
// checking that the declared comment length accounts for exactly the remaining
// bytes in buf. Returns the byte index of the signature within buf, or -1 if
// not found.
func findZipEndOfCentralDirectoryInBuffer(buf []byte) int {
	for i := len(buf) - zipEndOfCentralDirLen; i >= 0; i-- {
		if binary.LittleEndian.Uint32(buf[i:i+4]) != zipEndOfCentralDirSignature {
			continue
		}

		commentLen := int(binary.LittleEndian.Uint16(buf[i+zipEndOfCentralDirLen-2 : i+zipEndOfCentralDirLen]))
		if i+zipEndOfCentralDirLen+commentLen != len(buf) {
			continue
		}

		return i
	}

	return -1
}

// patchedReaderAt wraps a base [io.ReaderAt] and transparently overlays a
// small byte patch at a fixed offset. Reads that span the patched region see
// the replacement bytes; all other reads are forwarded unchanged. This allows
// correcting malformed ZIP metadata in memory without modifying the underlying
// file.
type patchedReaderAt struct {
	base        io.ReaderAt
	patchOffset int64
	patchBytes  []byte
}

// ReadAt implements [io.ReaderAt]. It delegates to the base reader and then
// overwrites any portion of the returned data that overlaps with the patch
// region.
func (p *patchedReaderAt) ReadAt(buf []byte, off int64) (int, error) {
	n, err := p.base.ReadAt(buf, off)
	if n <= 0 {
		return n, err
	}

	readEnd := off + int64(n)
	patchStart := p.patchOffset
	patchEnd := p.patchOffset + int64(len(p.patchBytes))

	if readEnd <= patchStart || off >= patchEnd {
		return n, err
	}

	start := max(off, patchStart)
	end := min(readEnd, patchEnd)

	copy(
		buf[start-off:end-off],
		p.patchBytes[start-patchStart:end-patchStart],
	)

	return n, err
}
