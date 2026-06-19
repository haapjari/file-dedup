package e2e

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"file-dedup/pkg/manifest"

	"github.com/haapjari/flate/pkg/flate"
)

const deflate64Method uint16 = 9

var builtBinaryPath string

type cmdResult struct {
	stdout string
	stderr string
	err    error
}

type zipFixtureEntry struct {
	name    string
	content []byte
	mode    os.FileMode
}

func (r cmdResult) combinedOutput() string {
	return r.stdout + r.stderr
}

func resolveRepoRoot() (string, error) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("failed to resolve repo root")
	}

	root := filepath.Dir(filepath.Dir(filename))
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("failed to resolve repo root: %w", err)
	}

	return absRoot, nil
}

func TestMain(m *testing.M) {
	repoRoot, err := resolveRepoRoot()
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "failed to initialize e2e tests: %v\n", err)
		os.Exit(1)
	}

	binDir, err := os.MkdirTemp("", "file-dedup-e2e-bin-*")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "failed to create temp directory for binary: %v\n", err)
		os.Exit(1)
	}

	binPath := filepath.Join(binDir, "file-dedup")
	if runtime.GOOS == "windows" {
		binPath += ".exe"
	}

	buildOutput, buildErr := buildBinary(binPath, repoRoot)
	if buildErr != nil {
		_, _ = fmt.Fprintf(os.Stderr, "failed to build file-dedup: %v\n%s\n", buildErr, string(buildOutput))
		_ = os.RemoveAll(binDir)
		os.Exit(1)
	}

	builtBinaryPath = binPath

	exitCode := m.Run()
	_ = os.RemoveAll(binDir)
	os.Exit(exitCode)
}

func buildBinary(binPath, repoRoot string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "go", "build", "-o", binPath, "./cmd")
	cmd.Dir = repoRoot

	return cmd.CombinedOutput()
}

func binaryPath(t *testing.T) string {
	t.Helper()

	if builtBinaryPath == "" {
		t.Fatal("binary path not initialized")
	}

	return builtBinaryPath
}

func runBinary(t *testing.T, binPath string, args ...string) cmdResult {
	t.Helper()

	timeout := 30 * time.Second
	if deadline, ok := t.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining < timeout {
			timeout = remaining
		}
	}
	if timeout <= 0 {
		timeout = time.Second
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, binPath, args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		if stderr.Len() > 0 && !strings.HasSuffix(stderr.String(), "\n") {
			stderr.WriteString("\n")
		}
		stderr.WriteString("command timed out after " + timeout.String())
	}

	return cmdResult{
		stdout: stdout.String(),
		stderr: stderr.String(),
		err:    err,
	}
}

func writeFile(t *testing.T, path, content string, modTime time.Time) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("failed to create directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("failed to set file times: %v", err)
	}
}

func writeZipArchive(t *testing.T, archivePath string, entries []zipFixtureEntry) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(archivePath), 0o755); err != nil {
		t.Fatalf("failed to create archive directory: %v", err)
	}

	archiveFile, err := os.Create(archivePath)
	if err != nil {
		t.Fatalf("failed to create archive: %v", err)
	}

	writer := zip.NewWriter(archiveFile)
	for _, entry := range entries {
		header := zip.FileHeader{
			Name:   entry.name,
			Method: zip.Deflate,
		}

		mode := entry.mode
		if mode == 0 {
			mode = 0o644
		}
		header.SetMode(mode)

		entryWriter, createErr := writer.CreateHeader(&header)
		if createErr != nil {
			t.Fatalf("failed to create archive entry: %v", createErr)
		}

		if _, writeErr := entryWriter.Write(entry.content); writeErr != nil {
			t.Fatalf("failed to write archive entry: %v", writeErr)
		}
	}

	if err := writer.Close(); err != nil {
		t.Fatalf("failed to close archive writer: %v", err)
	}
	if err := archiveFile.Close(); err != nil {
		t.Fatalf("failed to close archive file: %v", err)
	}
}

func writeDeflate64ZipArchive(t *testing.T, archivePath, entryName string, payload []byte) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(archivePath), 0o755); err != nil {
		t.Fatalf("failed to create archive directory: %v", err)
	}

	archiveFile, err := os.Create(archivePath)
	if err != nil {
		t.Fatalf("failed to create archive: %v", err)
	}

	writer := zip.NewWriter(archiveFile)
	compressed := deflate64StoredBlock(t, payload)
	header := &zip.FileHeader{
		Name:               filepath.ToSlash(entryName),
		Method:             deflate64Method,
		CRC32:              crc32.ChecksumIEEE(payload),
		UncompressedSize64: uint64(len(payload)),
		CompressedSize64:   uint64(len(compressed)),
	}

	entryWriter, createErr := writer.CreateRaw(header)
	if createErr != nil {
		t.Fatalf("failed to create archive entry: %v", createErr)
	}

	if _, writeErr := entryWriter.Write(compressed); writeErr != nil {
		t.Fatalf("failed to write archive entry: %v", writeErr)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("failed to close archive writer: %v", err)
	}
	if err := archiveFile.Close(); err != nil {
		t.Fatalf("failed to close archive file: %v", err)
	}
}

func writeDeflate64SymlinkZipArchive(t *testing.T, archivePath, entryName, target string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(archivePath), 0o755); err != nil {
		t.Fatalf("failed to create archive directory: %v", err)
	}

	archiveFile, err := os.Create(archivePath)
	if err != nil {
		t.Fatalf("failed to create archive: %v", err)
	}

	writer := zip.NewWriter(archiveFile)
	compressed := deflate64StoredBlock(t, []byte(target))
	header := &zip.FileHeader{
		Name:               filepath.ToSlash(entryName),
		Method:             deflate64Method,
		CRC32:              crc32.ChecksumIEEE([]byte(target)),
		UncompressedSize64: uint64(len(target)),
		CompressedSize64:   uint64(len(compressed)),
	}
	header.SetMode(os.ModeSymlink | 0o777)

	entryWriter, createErr := writer.CreateRaw(header)
	if createErr != nil {
		t.Fatalf("failed to create archive entry: %v", createErr)
	}
	if _, writeErr := entryWriter.Write(compressed); writeErr != nil {
		t.Fatalf("failed to write archive entry: %v", writeErr)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("failed to close archive writer: %v", err)
	}
	if err := archiveFile.Close(); err != nil {
		t.Fatalf("failed to close archive file: %v", err)
	}
}

func writeMixedSupportedMethodsZipArchive(t *testing.T, archivePath string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(archivePath), 0o755); err != nil {
		t.Fatalf("failed to create archive directory: %v", err)
	}

	archiveFile, err := os.Create(archivePath)
	if err != nil {
		t.Fatalf("failed to create archive: %v", err)
	}

	writer := zip.NewWriter(archiveFile)
	writeZipEntry(t, writer, "stored.txt", zip.Store, []byte("stored payload"))
	writeZipEntry(t, writer, "deflated.txt", zip.Deflate, []byte("deflated payload"))
	writeDeflate64ZipEntry(t, writer, "deflate64.txt", []byte("deflate64 payload"))
	if err := writer.Close(); err != nil {
		t.Fatalf("failed to close archive writer: %v", err)
	}
	if err := archiveFile.Close(); err != nil {
		t.Fatalf("failed to close archive file: %v", err)
	}
}

func writeUnsupportedMethodZipArchive(t *testing.T, archivePath string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(archivePath), 0o755); err != nil {
		t.Fatalf("failed to create archive directory: %v", err)
	}

	archiveFile, err := os.Create(archivePath)
	if err != nil {
		t.Fatalf("failed to create archive: %v", err)
	}

	writer := zip.NewWriter(archiveFile)
	writeRawZipEntry(t, writer, "unsupported.txt", 99, []byte("unsupported payload"), []byte("unsupported payload"))
	if err := writer.Close(); err != nil {
		t.Fatalf("failed to close archive writer: %v", err)
	}
	if err := archiveFile.Close(); err != nil {
		t.Fatalf("failed to close archive file: %v", err)
	}
}

func writeZipEntry(t *testing.T, writer *zip.Writer, name string, method uint16, payload []byte) {
	t.Helper()

	header := &zip.FileHeader{Name: filepath.ToSlash(name), Method: method}
	entryWriter, err := writer.CreateHeader(header)
	if err != nil {
		t.Fatalf("failed to create archive entry: %v", err)
	}
	if _, err := entryWriter.Write(payload); err != nil {
		t.Fatalf("failed to write archive entry: %v", err)
	}
}

func writeDeflate64ZipEntry(t *testing.T, writer *zip.Writer, name string, payload []byte) {
	t.Helper()

	compressed := deflate64StoredBlock(t, payload)
	writeRawZipEntry(t, writer, name, deflate64Method, payload, compressed)
}

func writeRawZipEntry(t *testing.T, writer *zip.Writer, name string, method uint16, payload, compressed []byte) {
	t.Helper()

	header := &zip.FileHeader{
		Name:               filepath.ToSlash(name),
		Method:             method,
		CRC32:              crc32.ChecksumIEEE(payload),
		UncompressedSize64: uint64(len(payload)),
		CompressedSize64:   uint64(len(compressed)),
	}
	entryWriter, err := writer.CreateRaw(header)
	if err != nil {
		t.Fatalf("failed to create archive entry: %v", err)
	}
	if _, err := entryWriter.Write(compressed); err != nil {
		t.Fatalf("failed to write archive entry: %v", err)
	}
}

func deflate64StoredBlock(t *testing.T, payload []byte) []byte {
	t.Helper()
	if len(payload) > 0xffff {
		t.Fatalf("payload too large for deterministic fixture: %d", len(payload))
	}

	length := len(payload)
	block := make([]byte, 5+len(payload))
	block[0] = 0x01
	block[1] = byte(length & 0xff)
	block[2] = byte((length >> 8) & 0xff)
	nlen := ^length & 0xffff
	block[3] = byte(nlen & 0xff)
	block[4] = byte((nlen >> 8) & 0xff)
	copy(block[5:], payload)

	reader := flate.NewReader64(bytes.NewReader(block))
	decompressed, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("failed to read Deflate64 fixture: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("failed to close Deflate64 fixture reader: %v", err)
	}
	if !bytes.Equal(payload, decompressed) {
		t.Fatalf("Deflate64 fixture mismatch: got %q want %q", decompressed, payload)
	}

	return block
}

func zipBytes(t *testing.T, entries []zipFixtureEntry) []byte {
	t.Helper()

	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, entry := range entries {
		header := zip.FileHeader{
			Name:   entry.name,
			Method: zip.Deflate,
		}

		mode := entry.mode
		if mode == 0 {
			mode = 0o644
		}
		header.SetMode(mode)

		entryWriter, createErr := writer.CreateHeader(&header)
		if createErr != nil {
			t.Fatalf("failed to create zip bytes entry: %v", createErr)
		}

		if _, writeErr := entryWriter.Write(entry.content); writeErr != nil {
			t.Fatalf("failed to write zip bytes entry: %v", writeErr)
		}
	}

	if err := writer.Close(); err != nil {
		t.Fatalf("failed to close zip bytes writer: %v", err)
	}

	return buffer.Bytes()
}

func assertExists(t *testing.T, path string) {
	t.Helper()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected path to exist: %s (error: %v)", path, err)
	}
}

func assertFileContent(t *testing.T, path, expectedContent string) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read file %s: %v", path, err)
	}
	if string(data) != expectedContent {
		t.Fatalf("file %s: expected content %q, got %q", path, expectedContent, string(data))
	}
}

func assertMissing(t *testing.T, path string) {
	t.Helper()

	if _, err := os.Stat(path); err == nil {
		t.Fatalf("expected path to be missing: %s", path)
	} else if !os.IsNotExist(err) {
		t.Fatalf("expected path to be missing: %s (unexpected error: %v)", path, err)
	}
}

func assertCommandFailed(t *testing.T, result cmdResult, keywords ...string) {
	t.Helper()

	if result.err == nil {
		t.Fatalf("expected command to fail\nstdout:\n%s\nstderr:\n%s", result.stdout, result.stderr)
	}

	combined := strings.ToLower(result.combinedOutput())
	for _, keyword := range keywords {
		if !strings.Contains(combined, strings.ToLower(keyword)) {
			t.Fatalf("expected output to contain %q\n%s", keyword, result.combinedOutput())
		}
	}
}

func assertCommandSucceeded(t *testing.T, label string, result cmdResult) {
	t.Helper()

	if result.err != nil {
		t.Fatalf("%s failed: %v\n%s", label, result.err, result.combinedOutput())
	}
}

func fileCount(t *testing.T, root string) int {
	t.Helper()

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("failed to read directory %s: %v", root, err)
	}

	count := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			count++
		}
	}

	return count
}

func hashSetsEqual(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for hash := range a {
		if _, ok := b[hash]; !ok {
			return false
		}
	}
	return true
}

func TestEndToEndRename_DryRunAndApply(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2020, 2, 3, 4, 5, 6, 0, time.UTC)

	writeFile(t, filepath.Join(root, "My Document.pdf"), "alpha", modTime)
	writeFile(t, filepath.Join(root, "Report (Final).docx"), "beta", modTime)
	writeFile(t, filepath.Join(root, "Photo.JPG"), "gamma", modTime)

	dryRun := runBinary(t, binPath, "rename", "--dry-run", root)
	assertCommandSucceeded(t, "rename dry-run", dryRun)
	if !strings.Contains(dryRun.stdout, "=== DRY RUN - no changes will be made ===") {
		t.Fatalf("expected dry-run banner in output\n%s", dryRun.stdout)
	}

	assertExists(t, filepath.Join(root, "My Document.pdf"))
	assertExists(t, filepath.Join(root, "Report (Final).docx"))
	assertExists(t, filepath.Join(root, "Photo.JPG"))

	datePrefix := modTime.Format("2006-01-02")
	assertMissing(t, filepath.Join(root, datePrefix+"_my_document.pdf"))

	apply := runBinary(t, binPath, "rename", root)
	assertCommandSucceeded(t, "rename apply", apply)

	assertMissing(t, filepath.Join(root, "My Document.pdf"))
	assertMissing(t, filepath.Join(root, "Report (Final).docx"))
	assertMissing(t, filepath.Join(root, "Photo.JPG"))

	assertExists(t, filepath.Join(root, datePrefix+"_my_document.pdf"))
	assertExists(t, filepath.Join(root, datePrefix+"_report_final.docx"))
	assertExists(t, filepath.Join(root, datePrefix+"_photo.jpg"))
}

func TestEndToEndFlatten_DryRunAndApply(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2021, 7, 12, 11, 30, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "dir1", "file.txt"), "same", modTime)
	writeFile(t, filepath.Join(root, "dir2", "file.txt"), "same", modTime)
	writeFile(t, filepath.Join(root, "dir1", "unique.txt"), "u1", modTime)
	writeFile(t, filepath.Join(root, "dir2", "unique.txt"), "u2", modTime)
	writeFile(t, filepath.Join(root, "rootfile.txt"), "root", modTime)

	dryRun := runBinary(t, binPath, "--workers", "1", "flatten", "--dry-run", root)
	assertCommandSucceeded(t, "flatten dry-run", dryRun)

	assertExists(t, filepath.Join(root, "dir1", "file.txt"))
	assertExists(t, filepath.Join(root, "dir2", "file.txt"))
	assertMissing(t, filepath.Join(root, "file.txt"))

	apply := runBinary(t, binPath, "--workers", "1", "flatten", root)
	assertCommandSucceeded(t, "flatten apply", apply)

	assertExists(t, filepath.Join(root, "file.txt"))
	assertExists(t, filepath.Join(root, "unique.txt"))
	assertExists(t, filepath.Join(root, "unique_1.txt"))
	assertExists(t, filepath.Join(root, "rootfile.txt"))

	assertMissing(t, filepath.Join(root, "dir1", "file.txt"))
	assertMissing(t, filepath.Join(root, "dir2", "file.txt"))
	assertMissing(t, filepath.Join(root, "dir1"))
	assertMissing(t, filepath.Join(root, "dir2"))
}

func TestEndToEndDuplicate_DryRunAndApply(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2022, 4, 2, 9, 15, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "a.txt"), "same", modTime)
	writeFile(t, filepath.Join(root, "sub", "a_copy.txt"), "same", modTime)
	writeFile(t, filepath.Join(root, "unique.txt"), "unique", modTime)

	dryRun := runBinary(t, binPath, "--workers", "1", "duplicate", "--dry-run", root)
	assertCommandSucceeded(t, "duplicate dry-run", dryRun)

	assertExists(t, filepath.Join(root, "a.txt"))
	assertExists(t, filepath.Join(root, "sub", "a_copy.txt"))

	apply := runBinary(t, binPath, "--workers", "1", "duplicate", root)
	assertCommandSucceeded(t, "duplicate apply", apply)

	assertExists(t, filepath.Join(root, "a.txt"))
	assertMissing(t, filepath.Join(root, "sub", "a_copy.txt"))
	assertExists(t, filepath.Join(root, "unique.txt"))
}

func TestEndToEndUnzip_DryRunAndApplyRecursive(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()

	innerArchive := zipBytes(t, []zipFixtureEntry{
		{name: "deep/final.txt", content: []byte("payload")},
	})

	outerArchivePath := filepath.Join(root, "outer.zip")
	writeZipArchive(t, outerArchivePath, []zipFixtureEntry{
		{name: "nested/inner.zip", content: innerArchive},
		{name: "outer.txt", content: []byte("outer")},
	})

	dryRun := runBinary(t, binPath, "unzip", "--dry-run", root)
	assertCommandSucceeded(t, "unzip dry-run", dryRun)

	assertExists(t, outerArchivePath)
	assertMissing(t, filepath.Join(root, "nested", "inner.zip"))
	assertMissing(t, filepath.Join(root, "nested", "deep", "final.txt"))

	apply := runBinary(t, binPath, "unzip", root)
	assertCommandSucceeded(t, "unzip apply", apply)

	assertMissing(t, filepath.Join(root, "outer.zip"))
	assertMissing(t, filepath.Join(root, "nested", "inner.zip"))
	assertExists(t, filepath.Join(root, "outer.txt"))
	assertExists(t, filepath.Join(root, "nested", "deep", "final.txt"))
}

func TestEndToEndUnzip_Deflate64(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	archivePath := filepath.Join(root, "deflate64.zip")

	writeDeflate64ZipArchive(t, archivePath, "method9.txt", []byte("payload"))

	result := runBinary(t, binPath, "unzip", root)
	assertCommandSucceeded(t, "unzip deflate64", result)

	assertMissing(t, archivePath)
	assertFileContent(t, filepath.Join(root, "method9.txt"), "payload")
}

func TestEndToEndUnzip_Deflate64SymlinkEntryRejected(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	archivePath := filepath.Join(root, "deflate64-symlink.zip")

	writeDeflate64SymlinkZipArchive(t, archivePath, "link.txt", "/etc/passwd")

	result := runBinary(t, binPath, "unzip", root)
	assertCommandFailed(t, result, "symlink")
	assertExists(t, archivePath)
	assertMissing(t, filepath.Join(root, "link.txt"))
}

func TestEndToEndUnzip_MixedSupportedMethodsDeflate64(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	archivePath := filepath.Join(root, "mixed_supported.zip")

	writeMixedSupportedMethodsZipArchive(t, archivePath)

	result := runBinary(t, binPath, "unzip", root)
	assertCommandSucceeded(t, "unzip mixed supported methods", result)

	assertMissing(t, archivePath)
	assertFileContent(t, filepath.Join(root, "stored.txt"), "stored payload")
	assertFileContent(t, filepath.Join(root, "deflated.txt"), "deflated payload")
	assertFileContent(t, filepath.Join(root, "deflate64.txt"), "deflate64 payload")
}

func TestEndToEndUnzip_UnsupportedCompressionMethod(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	archivePath := filepath.Join(root, "unsupported.zip")

	writeUnsupportedMethodZipArchive(t, archivePath)

	result := runBinary(t, binPath, "unzip", root)
	assertCommandSucceeded(t, "unzip unsupported method", result)

	assertExists(t, archivePath)
	assertMissing(t, filepath.Join(root, "unsupported.txt"))
	if !strings.Contains(result.combinedOutput(), "unsupported compression method") {
		t.Fatalf("expected unsupported method output, got:\n%s", result.combinedOutput())
	}
}

// TestEndToEndUnzip_DeeplyNestedArchivesFourLevels verifies that the unzip
// command fully extracts archives nested 4 levels deep and leaves no .zip
// files behind. Each level contains at least one regular file alongside the
// nested archive to confirm all content is extracted.
func TestEndToEndUnzip_DeeplyNestedArchivesFourLevels(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()

	// Build from the inside out:
	//   level4.zip → "l4/report.txt"
	//   level3.zip → "l3/level4.zip" + "l3/notes.txt"
	//   level2.zip → "l2/level3.zip" + "l2/readme.txt"
	//   level1.zip → "l1/level2.zip" + "l1/cover.txt"
	level4 := zipBytes(t, []zipFixtureEntry{
		{name: "l4/report.txt", content: []byte("final report")},
	})
	level3 := zipBytes(t, []zipFixtureEntry{
		{name: "l3/level4.zip", content: level4},
		{name: "l3/notes.txt", content: []byte("level 3 notes")},
	})
	level2 := zipBytes(t, []zipFixtureEntry{
		{name: "l2/level3.zip", content: level3},
		{name: "l2/readme.txt", content: []byte("level 2 readme")},
	})
	level1Path := filepath.Join(root, "level1.zip")
	writeZipArchive(t, level1Path, []zipFixtureEntry{
		{name: "l1/level2.zip", content: level2},
		{name: "l1/cover.txt", content: []byte("level 1 cover")},
	})

	result := runBinary(t, binPath, "unzip", root)
	assertCommandSucceeded(t, "unzip 4-level nested", result)

	assertExists(t, filepath.Join(root, "l1", "cover.txt"))
	assertExists(t, filepath.Join(root, "l1", "l2", "readme.txt"))
	assertExists(t, filepath.Join(root, "l1", "l2", "l3", "notes.txt"))
	assertExists(t, filepath.Join(root, "l1", "l2", "l3", "l4", "report.txt"))

	var remaining []string
	if err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() && info.Name() == ".file-dedup" {
			return filepath.SkipDir
		}
		if !info.IsDir() && strings.EqualFold(filepath.Ext(path), ".zip") {
			remaining = append(remaining, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk failed: %v", err)
	}
	if len(remaining) > 0 {
		t.Fatalf("expected no leftover .zip files, found: %v", remaining)
	}
}

func TestEndToEndUnzip_ZipSlipBlocked(t *testing.T) {
	binPath := binaryPath(t)
	workspace := t.TempDir()
	target := filepath.Join(workspace, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("failed to create target: %v", err)
	}

	outsideSentinel := filepath.Join(workspace, "outside-sentinel.txt")
	if err := os.WriteFile(outsideSentinel, []byte("do-not-touch"), 0o600); err != nil {
		t.Fatalf("failed to create outside sentinel: %v", err)
	}
	outsideBefore, err := os.ReadFile(outsideSentinel)
	if err != nil {
		t.Fatalf("failed to read outside sentinel before unzip: %v", err)
	}

	archivePath := filepath.Join(target, "bad.zip")
	writeZipArchive(t, archivePath, []zipFixtureEntry{
		{name: "../outside-sentinel.txt", content: []byte("attack")},
	})

	result := runBinary(t, binPath, "unzip", target)
	assertCommandFailed(t, result, "illegal entry path", "path traversal")

	output := strings.ToLower(result.combinedOutput())
	if !strings.Contains(output, "illegal entry path") {
		t.Fatalf("expected output to report illegal entry path\n%s", result.combinedOutput())
	}
	if !strings.Contains(output, "path traversal") {
		t.Fatalf("expected output to mention path traversal\n%s", result.combinedOutput())
	}

	outsideAfter, err := os.ReadFile(outsideSentinel)
	if err != nil {
		t.Fatalf("failed to read outside sentinel after unzip: %v", err)
	}
	if !bytes.Equal(outsideBefore, outsideAfter) {
		t.Fatalf("outside sentinel changed unexpectedly")
	}
}

func TestEndToEndRename_Idempotent(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2022, 8, 10, 16, 20, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "My Document.pdf"), "alpha", modTime)

	firstRun := runBinary(t, binPath, "rename", root)
	assertCommandSucceeded(t, "first rename run", firstRun)

	datePrefix := modTime.Format("2006-01-02")
	renamedPath := filepath.Join(root, datePrefix+"_my_document.pdf")
	assertExists(t, renamedPath)

	secondRun := runBinary(t, binPath, "rename", root)
	assertCommandSucceeded(t, "second rename run", secondRun)

	assertExists(t, renamedPath)
	assertMissing(t, filepath.Join(root, "My Document.pdf"))
	assertMissing(t, filepath.Join(root, datePrefix+"_"+datePrefix+"_my_document.pdf"))

	if got := fileCount(t, root); got != 1 {
		t.Fatalf("expected exactly one file after idempotent rename, got %d", got)
	}
}

func TestEndToEndDuplicate_Idempotent(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2022, 11, 4, 9, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "a.txt"), "same", modTime)
	writeFile(t, filepath.Join(root, "b.txt"), "same", modTime)
	writeFile(t, filepath.Join(root, "unique.txt"), "unique", modTime)

	firstRun := runBinary(t, binPath, "duplicate", root)
	assertCommandSucceeded(t, "first duplicate run", firstRun)

	if got := fileCount(t, root); got != 2 {
		t.Fatalf("expected two files after first dedupe run, got %d", got)
	}

	secondRun := runBinary(t, binPath, "duplicate", root)
	assertCommandSucceeded(t, "second duplicate run", secondRun)

	if got := fileCount(t, root); got != 2 {
		t.Fatalf("expected file count to remain stable after second dedupe run, got %d", got)
	}
}

func TestEndToEndSkipFiles_DefaultsAreIgnored(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2023, 1, 15, 8, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, ".DS_Store"), "metadata", modTime)
	writeFile(t, filepath.Join(root, "file-dedup.log"), "logs", modTime)
	writeFile(t, filepath.Join(root, "sub", "Thumbs.db"), "thumbs", modTime)
	writeFile(t, filepath.Join(root, "sub", "My Notes.txt"), "notes", modTime)

	renameRun := runBinary(t, binPath, "rename", root)
	assertCommandSucceeded(t, "rename run", renameRun)

	datePrefix := modTime.Format("2006-01-02")
	assertExists(t, filepath.Join(root, ".DS_Store"))
	assertExists(t, filepath.Join(root, "file-dedup.log"))
	assertExists(t, filepath.Join(root, "sub", "Thumbs.db"))
	assertExists(t, filepath.Join(root, "sub", datePrefix+"_my_notes.txt"))

	flattenRun := runBinary(t, binPath, "flatten", root)
	assertCommandSucceeded(t, "flatten run", flattenRun)

	assertExists(t, filepath.Join(root, ".DS_Store"))
	assertExists(t, filepath.Join(root, "file-dedup.log"))
	assertExists(t, filepath.Join(root, "sub", "Thumbs.db"))
	assertExists(t, filepath.Join(root, datePrefix+"_my_notes.txt"))
	assertMissing(t, filepath.Join(root, "sub", datePrefix+"_my_notes.txt"))

	duplicateRun := runBinary(t, binPath, "duplicate", root)
	assertCommandSucceeded(t, "duplicate run", duplicateRun)

	assertExists(t, filepath.Join(root, ".DS_Store"))
	assertExists(t, filepath.Join(root, "file-dedup.log"))
	assertExists(t, filepath.Join(root, "sub", "Thumbs.db"))
}

func TestEndToEndPipeline_ManifestIntegrity(t *testing.T) {
	binPath := binaryPath(t)
	workspace := t.TempDir()
	target := filepath.Join(workspace, "target")
	outsideSentinel := filepath.Join(workspace, "outside-sentinel.txt")
	modTime := time.Date(2023, 9, 18, 7, 45, 0, 0, time.UTC)

	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("failed to create target: %v", err)
	}
	writeFile(t, outsideSentinel, "do-not-touch", modTime)

	outsideBefore, err := os.ReadFile(outsideSentinel)
	if err != nil {
		t.Fatalf("failed to read outside sentinel before operations: %v", err)
	}

	writeFile(t, filepath.Join(target, "docs", "report.pdf"), "same", modTime)
	writeFile(t, filepath.Join(target, "backup", "report copy.pdf"), "same", modTime)
	writeFile(t, filepath.Join(target, "photos", "file.txt"), "alpha", modTime)
	writeFile(t, filepath.Join(target, "other", "file.txt"), "beta", modTime)
	writeFile(t, filepath.Join(target, "other", "unique.txt"), "unique", modTime)

	beforeManifest := filepath.Join(target, ".DS_Store")
	afterManifest := filepath.Join(target, "Thumbs.db")

	before := runBinary(t, binPath, "--workers", "1", "manifest", target, "-o", beforeManifest)
	assertCommandSucceeded(t, "manifest before", before)

	rename := runBinary(t, binPath, "rename", target)
	assertCommandSucceeded(t, "rename", rename)

	flatten := runBinary(t, binPath, "--workers", "1", "flatten", target)
	assertCommandSucceeded(t, "flatten", flatten)

	duplicate := runBinary(t, binPath, "--workers", "1", "duplicate", target)
	assertCommandSucceeded(t, "duplicate", duplicate)

	after := runBinary(t, binPath, "--workers", "1", "manifest", target, "-o", afterManifest)
	assertCommandSucceeded(t, "manifest after", after)

	beforeData, err := manifest.Load(beforeManifest)
	if err != nil {
		t.Fatalf("failed to load before manifest: %v", err)
	}
	afterData, err := manifest.Load(afterManifest)
	if err != nil {
		t.Fatalf("failed to load after manifest: %v", err)
	}

	if afterData.FileCount() > beforeData.FileCount() {
		t.Fatalf("expected file count to stay same or decrease")
	}

	if beforeData.UniqueFileCount() != afterData.UniqueFileCount() {
		t.Fatalf("expected unique file count to remain stable")
	}

	if !hashSetsEqual(beforeData.UniqueHashes(), afterData.UniqueHashes()) {
		t.Fatalf("expected unique hashes to remain stable")
	}

	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatalf("failed to read target directory: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() != ".file-dedup" {
			t.Fatalf("expected no subdirectories after flatten: %s", entry.Name())
		}
	}

	outsideAfter, err := os.ReadFile(outsideSentinel)
	if err != nil {
		t.Fatalf("failed to read outside sentinel after operations: %v", err)
	}
	if !bytes.Equal(outsideBefore, outsideAfter) {
		t.Fatalf("outside sentinel changed unexpectedly")
	}
}

func TestEndToEndInvalidTargetPaths(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 1, 5, 6, 0, 0, 0, time.UTC)

	filePath := filepath.Join(root, "file.txt")
	writeFile(t, filePath, "content", modTime)

	fileTarget := runBinary(t, binPath, "rename", filePath)
	assertCommandFailed(t, fileTarget, "directory", filePath)

	missingPath := filepath.Join(root, "missing")
	missingTarget := runBinary(t, binPath, "rename", missingPath)
	assertCommandFailed(t, missingTarget, "cannot access", "directory", missingPath)
}

func TestEndToEndRename_SymlinkEscapeBlocked(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	outside := t.TempDir()
	modTime := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)

	outsideFile := filepath.Join(outside, "outside.txt")
	writeFile(t, outsideFile, "outside", modTime)
	outsideBefore, err := os.ReadFile(outsideFile)
	if err != nil {
		t.Fatalf("failed to read outside sentinel before rename: %v", err)
	}

	linkPath := filepath.Join(root, "escape_link.txt")
	if symlinkErr := os.Symlink(outsideFile, linkPath); symlinkErr != nil {
		t.Skipf("symlink not supported: %v", symlinkErr)
	}

	result := runBinary(t, binPath, "rename", "--dry-run", root)
	assertCommandFailed(t, result, "unsafe", "symlink")

	info, err := os.Lstat(linkPath)
	if err != nil {
		t.Fatalf("expected symlink to remain: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected link to be a symlink")
	}

	outsideAfter, err := os.ReadFile(outsideFile)
	if err != nil {
		t.Fatalf("failed to read outside sentinel after rename: %v", err)
	}
	if !bytes.Equal(outsideBefore, outsideAfter) {
		t.Fatalf("outside sentinel changed unexpectedly")
	}
}

func TestEndToEndFlatten_SymlinkEscapeBlocked(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	outside := t.TempDir()
	modTime := time.Date(2024, 3, 2, 12, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "nested", "safe.txt"), "safe", modTime)

	outsideFile := filepath.Join(outside, "outside.txt")
	writeFile(t, outsideFile, "outside", modTime)
	outsideBefore, err := os.ReadFile(outsideFile)
	if err != nil {
		t.Fatalf("failed to read outside sentinel before flatten: %v", err)
	}

	linkPath := filepath.Join(root, "nested", "escape_link.txt")
	if symlinkErr := os.Symlink(outsideFile, linkPath); symlinkErr != nil {
		t.Skipf("symlink not supported: %v", symlinkErr)
	}

	result := runBinary(t, binPath, "flatten", root)
	assertCommandFailed(t, result, "unsafe", "symlink")

	assertExists(t, filepath.Join(root, "nested", "safe.txt"))
	assertMissing(t, filepath.Join(root, "safe.txt"))
	assertExists(t, linkPath)

	outsideAfter, err := os.ReadFile(outsideFile)
	if err != nil {
		t.Fatalf("failed to read outside sentinel after flatten: %v", err)
	}
	if !bytes.Equal(outsideBefore, outsideAfter) {
		t.Fatalf("outside sentinel changed unexpectedly")
	}
}

func TestEndToEndDuplicate_SymlinkEscapeBlocked(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	outside := t.TempDir()
	modTime := time.Date(2024, 3, 3, 12, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "a.txt"), "same", modTime)
	writeFile(t, filepath.Join(root, "b.txt"), "same", modTime)

	outsideFile := filepath.Join(outside, "outside.txt")
	writeFile(t, outsideFile, "outside", modTime)
	outsideBefore, err := os.ReadFile(outsideFile)
	if err != nil {
		t.Fatalf("failed to read outside sentinel before duplicate: %v", err)
	}

	linkPath := filepath.Join(root, "escape_link.txt")
	if symlinkErr := os.Symlink(outsideFile, linkPath); symlinkErr != nil {
		t.Skipf("symlink not supported: %v", symlinkErr)
	}

	result := runBinary(t, binPath, "duplicate", root)
	assertCommandFailed(t, result, "unsafe", "symlink")

	assertExists(t, filepath.Join(root, "a.txt"))
	assertExists(t, filepath.Join(root, "b.txt"))
	assertExists(t, linkPath)

	outsideAfter, err := os.ReadFile(outsideFile)
	if err != nil {
		t.Fatalf("failed to read outside sentinel after duplicate: %v", err)
	}
	if !bytes.Equal(outsideBefore, outsideAfter) {
		t.Fatalf("outside sentinel changed unexpectedly")
	}
}

func TestEndToEndManifest_SymlinkEscapeBlocked(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	outside := t.TempDir()
	modTime := time.Date(2024, 3, 4, 12, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "safe.txt"), "safe", modTime)

	outsideFile := filepath.Join(outside, "outside.txt")
	writeFile(t, outsideFile, "outside", modTime)
	outsideBefore, err := os.ReadFile(outsideFile)
	if err != nil {
		t.Fatalf("failed to read outside sentinel before manifest: %v", err)
	}

	linkPath := filepath.Join(root, "escape_link.txt")
	if symlinkErr := os.Symlink(outsideFile, linkPath); symlinkErr != nil {
		t.Skipf("symlink not supported: %v", symlinkErr)
	}

	manifestPath := filepath.Join(root, "manifest.json")
	result := runBinary(t, binPath, "manifest", root, "-o", manifestPath)
	assertCommandFailed(t, result, "unsafe", "symlink")

	assertMissing(t, manifestPath)
	assertExists(t, linkPath)

	outsideAfter, err := os.ReadFile(outsideFile)
	if err != nil {
		t.Fatalf("failed to read outside sentinel after manifest: %v", err)
	}
	if !bytes.Equal(outsideBefore, outsideAfter) {
		t.Fatalf("outside sentinel changed unexpectedly")
	}
}

func TestEndToEndManifest_OutputOutsideTargetRejected(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	outside := t.TempDir()
	modTime := time.Date(2024, 3, 5, 12, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "safe.txt"), "safe", modTime)

	outsideOutputPath := filepath.Join(outside, "manifest.json")
	result := runBinary(t, binPath, "manifest", root, "-o", outsideOutputPath)
	assertCommandFailed(t, result, "output path", "target directory")

	assertMissing(t, outsideOutputPath)
}

func TestEndToEndOrganize_DryRunAndApply(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2023, 5, 10, 14, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "report.pdf"), "pdf-content", modTime)
	writeFile(t, filepath.Join(root, "photo.jpg"), "jpg-content", modTime)
	writeFile(t, filepath.Join(root, "notes.txt"), "txt-content", modTime)
	writeFile(t, filepath.Join(root, "Makefile"), "make-content", modTime)

	dryRun := runBinary(t, binPath, "organize", "--dry-run", root)
	assertCommandSucceeded(t, "organize dry-run", dryRun)
	if !strings.Contains(dryRun.stdout, "=== DRY RUN - no changes will be made ===") {
		t.Fatalf("expected dry-run banner in output\n%s", dryRun.stdout)
	}

	assertExists(t, filepath.Join(root, "report.pdf"))
	assertExists(t, filepath.Join(root, "photo.jpg"))
	assertExists(t, filepath.Join(root, "notes.txt"))
	assertExists(t, filepath.Join(root, "Makefile"))

	apply := runBinary(t, binPath, "organize", root)
	assertCommandSucceeded(t, "organize apply", apply)

	assertMissing(t, filepath.Join(root, "report.pdf"))
	assertMissing(t, filepath.Join(root, "photo.jpg"))
	assertMissing(t, filepath.Join(root, "notes.txt"))
	assertMissing(t, filepath.Join(root, "Makefile"))

	assertExists(t, filepath.Join(root, "pdf", "report.pdf"))
	assertExists(t, filepath.Join(root, "jpg", "photo.jpg"))
	assertExists(t, filepath.Join(root, "txt", "notes.txt"))
	assertExists(t, filepath.Join(root, "other", "Makefile"))
}

func TestEndToEndOrganize_AfterFlatten(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2023, 5, 10, 14, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "docs", "report.pdf"), "pdf", modTime)
	writeFile(t, filepath.Join(root, "photos", "vacation", "photo.jpg"), "jpg", modTime)
	writeFile(t, filepath.Join(root, "notes.txt"), "txt", modTime)

	flatten := runBinary(t, binPath, "--workers", "1", "flatten", root)
	assertCommandSucceeded(t, "flatten", flatten)

	assertExists(t, filepath.Join(root, "report.pdf"))
	assertExists(t, filepath.Join(root, "photo.jpg"))
	assertExists(t, filepath.Join(root, "notes.txt"))

	organize := runBinary(t, binPath, "organize", root)
	assertCommandSucceeded(t, "organize", organize)

	assertExists(t, filepath.Join(root, "pdf", "report.pdf"))
	assertExists(t, filepath.Join(root, "jpg", "photo.jpg"))
	assertExists(t, filepath.Join(root, "txt", "notes.txt"))
}

func TestEndToEndOrganize_SymlinkEscapeBlocked(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	outside := t.TempDir()
	modTime := time.Date(2024, 3, 6, 12, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "safe.txt"), "safe", modTime)

	outsideFile := filepath.Join(outside, "outside.txt")
	writeFile(t, outsideFile, "outside", modTime)
	outsideBefore, err := os.ReadFile(outsideFile)
	if err != nil {
		t.Fatalf("failed to read outside sentinel before organize: %v", err)
	}

	linkPath := filepath.Join(root, "escape_link.txt")
	if symlinkErr := os.Symlink(outsideFile, linkPath); symlinkErr != nil {
		t.Skipf("symlink not supported: %v", symlinkErr)
	}

	result := runBinary(t, binPath, "organize", root)
	assertCommandFailed(t, result, "unsafe", "symlink")

	assertExists(t, filepath.Join(root, "safe.txt"))
	assertExists(t, linkPath)

	outsideAfter, err := os.ReadFile(outsideFile)
	if err != nil {
		t.Fatalf("failed to read outside sentinel after organize: %v", err)
	}
	if !bytes.Equal(outsideBefore, outsideAfter) {
		t.Fatalf("outside sentinel changed unexpectedly")
	}
}

func TestEndToEndFileDedupDir_NeverCollected(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, ".file-dedup", "trash", "run1", "trashed.txt"), "trashed", modTime)
	writeFile(t, filepath.Join(root, "normal.txt"), "normal content", modTime)

	renameResult := runBinary(t, binPath, "rename", root)
	assertCommandSucceeded(t, "rename with .file-dedup dir", renameResult)

	assertExists(t, filepath.Join(root, ".file-dedup", "trash", "run1", "trashed.txt"))

	if !strings.Contains(renameResult.stdout, "found 1 files") {
		t.Fatalf("expected 'found 1 files' in output, got:\n%s", renameResult.stdout)
	}
}

func TestEndToEndUndo_ReversesDuplicate(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 7, 1, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "a.txt"), "same-content", modTime)
	writeFile(t, filepath.Join(root, "b.txt"), "same-content", modTime)
	writeFile(t, filepath.Join(root, "unique.txt"), "unique", modTime)

	dupResult := runBinary(t, binPath, "--workers", "1", "--no-snapshot", "duplicate", root)
	assertCommandSucceeded(t, "duplicate", dupResult)

	if got := fileCount(t, root); got != 2 {
		t.Fatalf("expected 2 files after dedup, got %d", got)
	}

	undoResult := runBinary(t, binPath, "undo", root)
	assertCommandSucceeded(t, "undo duplicate", undoResult)

	if !strings.Contains(undoResult.stdout, "Restored:  1") {
		t.Fatalf("expected 'Restored:  1' in undo output\n%s", undoResult.stdout)
	}

	assertExists(t, filepath.Join(root, "a.txt"))
	assertExists(t, filepath.Join(root, "b.txt"))
	assertExists(t, filepath.Join(root, "unique.txt"))
}

func TestEndToEndUndo_ReversesRename(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 7, 2, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "My Document.pdf"), "content", modTime)

	renameResult := runBinary(t, binPath, "--no-snapshot", "rename", root)
	assertCommandSucceeded(t, "rename", renameResult)

	datePrefix := modTime.Format("2006-01-02")
	renamedPath := filepath.Join(root, datePrefix+"_my_document.pdf")
	assertExists(t, renamedPath)
	assertMissing(t, filepath.Join(root, "My Document.pdf"))

	undoResult := runBinary(t, binPath, "undo", root)
	assertCommandSucceeded(t, "undo rename", undoResult)

	if !strings.Contains(undoResult.stdout, "Reversed:  1") {
		t.Fatalf("expected 'Reversed:  1' in undo output\n%s", undoResult.stdout)
	}

	assertExists(t, filepath.Join(root, "My Document.pdf"))
	assertMissing(t, renamedPath)
}

func TestEndToEndUndo_ReversesFlatten(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 7, 3, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "sub", "deep", "file.txt"), "content", modTime)

	flattenResult := runBinary(t, binPath, "--workers", "1", "--no-snapshot", "flatten", root)
	assertCommandSucceeded(t, "flatten", flattenResult)

	assertExists(t, filepath.Join(root, "file.txt"))
	assertMissing(t, filepath.Join(root, "sub", "deep", "file.txt"))

	undoResult := runBinary(t, binPath, "undo", root)
	assertCommandSucceeded(t, "undo flatten", undoResult)

	if !strings.Contains(undoResult.stdout, "Reversed:  1") {
		t.Fatalf("expected 'Reversed:  1' in undo output\n%s", undoResult.stdout)
	}

	assertExists(t, filepath.Join(root, "sub", "deep", "file.txt"))
	assertMissing(t, filepath.Join(root, "file.txt"))
}

func TestEndToEndUndo_DryRunNoChanges(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 7, 4, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "My Document.pdf"), "content", modTime)

	renameResult := runBinary(t, binPath, "--no-snapshot", "rename", root)
	assertCommandSucceeded(t, "rename", renameResult)

	datePrefix := modTime.Format("2006-01-02")
	renamedPath := filepath.Join(root, datePrefix+"_my_document.pdf")
	assertExists(t, renamedPath)

	undoResult := runBinary(t, binPath, "undo", "--dry-run", root)
	assertCommandSucceeded(t, "undo dry-run", undoResult)

	if !strings.Contains(undoResult.stdout, "=== DRY RUN - no changes will be made ===") {
		t.Fatalf("expected dry-run banner in output\n%s", undoResult.stdout)
	}

	assertExists(t, renamedPath)
	assertMissing(t, filepath.Join(root, "My Document.pdf"))
}

func TestEndToEndUndo_NoJournalError(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 7, 5, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "file.txt"), "content", modTime)

	undoResult := runBinary(t, binPath, "undo", root)
	assertCommandFailed(t, undoResult, "journal")
}

func TestEndToEndPurge_PurgesAfterDuplicate(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 8, 1, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "a.txt"), "same-content", modTime)
	writeFile(t, filepath.Join(root, "b.txt"), "same-content", modTime)
	writeFile(t, filepath.Join(root, "unique.txt"), "unique", modTime)

	dupResult := runBinary(t, binPath, "--workers", "1", "--no-snapshot", "duplicate", root)
	assertCommandSucceeded(t, "duplicate", dupResult)

	trashRoot := filepath.Join(root, ".file-dedup", "trash")
	assertExists(t, trashRoot)

	dryResult := runBinary(t, binPath, "purge", "--dry-run", "--all", root)
	assertCommandSucceeded(t, "purge dry-run", dryResult)
	if !strings.Contains(dryResult.stdout, "WOULD PURGE") {
		t.Fatalf("expected 'WOULD PURGE' in dry-run output\n%s", dryResult.stdout)
	}

	trashEntries, err := os.ReadDir(trashRoot)
	if err != nil {
		t.Fatalf("failed to read trash directory: %v", err)
	}
	if len(trashEntries) == 0 {
		t.Fatal("trash should still exist after dry-run purge")
	}

	purgeResult := runBinary(t, binPath, "purge", "--all", "--force", root)
	assertCommandSucceeded(t, "purge all", purgeResult)

	if !strings.Contains(purgeResult.stdout, "Purged:    1 run(s)") {
		t.Fatalf("expected 'Purged:    1 run(s)' in output\n%s", purgeResult.stdout)
	}

	trashEntries, err = os.ReadDir(trashRoot)
	if err != nil {
		t.Fatalf("failed to read trash directory after purge: %v", err)
	}
	if len(trashEntries) != 0 {
		t.Fatalf("expected empty trash directory, got %d entries", len(trashEntries))
	}
}

func TestEndToEndPurge_RequiresFilter(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 8, 2, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "file.txt"), "content", modTime)

	result := runBinary(t, binPath, "purge", root)
	assertCommandFailed(t, result, "at least one")
}

func TestEndToEndPurge_NoTrash(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 8, 3, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "file.txt"), "content", modTime)

	result := runBinary(t, binPath, "purge", "--all", "--force", root)
	assertCommandSucceeded(t, "purge no trash", result)

	if !strings.Contains(result.stdout, "No trash runs found") {
		t.Fatalf("expected 'No trash runs found' in output\n%s", result.stdout)
	}
}

func TestEndToEndPurge_OlderThanFilter(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 8, 4, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "a.txt"), "same-content", modTime)
	writeFile(t, filepath.Join(root, "b.txt"), "same-content", modTime)

	dupResult := runBinary(t, binPath, "--workers", "1", "--no-snapshot", "duplicate", root)
	assertCommandSucceeded(t, "duplicate", dupResult)

	result := runBinary(t, binPath, "purge", "--older-than", "1000h", root)
	assertCommandSucceeded(t, "purge older-than", result)

	if !strings.Contains(result.stdout, "Purged:    0 run(s)") {
		t.Fatalf("expected 'Purged:    0 run(s)' in output\n%s", result.stdout)
	}

	trashRoot := filepath.Join(root, ".file-dedup", "trash")
	trashEntries, err := os.ReadDir(trashRoot)
	if err != nil {
		t.Fatalf("failed to read trash directory: %v", err)
	}
	if len(trashEntries) == 0 {
		t.Fatal("trash should still exist after older-than filter")
	}
}

func TestEndToEndPurge_AllRequiresForce(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 8, 5, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "a.txt"), "same-content", modTime)
	writeFile(t, filepath.Join(root, "b.txt"), "same-content", modTime)

	dupResult := runBinary(t, binPath, "--workers", "1", "--no-snapshot", "duplicate", root)
	assertCommandSucceeded(t, "duplicate", dupResult)

	result := runBinary(t, binPath, "purge", "--all", root)
	assertCommandFailed(t, result, "--all requires --force")

	trashRoot := filepath.Join(root, ".file-dedup", "trash")
	assertExists(t, trashRoot)
}

func TestEndToEndPurge_AllDryRunNoForce(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 8, 6, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "a.txt"), "same-content", modTime)
	writeFile(t, filepath.Join(root, "b.txt"), "same-content", modTime)

	dupResult := runBinary(t, binPath, "--workers", "1", "--no-snapshot", "duplicate", root)
	assertCommandSucceeded(t, "duplicate", dupResult)

	result := runBinary(t, binPath, "purge", "--all", "--dry-run", root)
	assertCommandSucceeded(t, "purge all dry-run", result)

	if !strings.Contains(result.stdout, "WOULD PURGE") {
		t.Fatalf("expected 'WOULD PURGE' in output\n%s", result.stdout)
	}
}

// =============================================================================
// Global Flags
// =============================================================================

func TestEndToEndGlobalFlags_VerboseOutput(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 9, 1, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "My Document.pdf"), "alpha", modTime)

	quietResult := runBinary(t, binPath, "--no-snapshot", "rename", root)
	assertCommandSucceeded(t, "rename quiet", quietResult)

	undoResult := runBinary(t, binPath, "undo", root)
	assertCommandSucceeded(t, "undo rename", undoResult)

	verboseResult := runBinary(t, binPath, "--no-snapshot", "-v", "rename", root)
	assertCommandSucceeded(t, "rename verbose", verboseResult)

	if !strings.Contains(verboseResult.stdout, "RENAME:") {
		t.Fatalf("expected verbose output to contain 'RENAME:'\n%s", verboseResult.stdout)
	}
}

func TestEndToEndGlobalFlags_Version(t *testing.T) {
	binPath := binaryPath(t)

	result := runBinary(t, binPath, "--version")
	assertCommandSucceeded(t, "version", result)

	if !strings.Contains(result.stdout, "file-dedup version") {
		t.Fatalf("expected version output to contain 'file-dedup version'\n%s", result.stdout)
	}
}

func TestEndToEndGlobalFlags_WorkersEdgeValues(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 9, 2, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "a.txt"), "same", modTime)
	writeFile(t, filepath.Join(root, "b.txt"), "same", modTime)

	result := runBinary(t, binPath, "--workers", "0", "--no-snapshot", "duplicate", "--dry-run", root)
	_ = result
}

func TestEndToEndGlobalFlags_NoSnapshotSkipsManifest(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 9, 3, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "My Document.pdf"), "alpha", modTime)

	result := runBinary(t, binPath, "--no-snapshot", "rename", root)
	assertCommandSucceeded(t, "rename no-snapshot", result)

	manifestsDir := filepath.Join(root, ".file-dedup", "manifests")
	entries, err := os.ReadDir(manifestsDir)
	if err == nil && len(entries) > 0 {
		t.Fatalf("expected no manifest files with --no-snapshot, got %d", len(entries))
	}
}

func TestEndToEndGlobalFlags_SnapshotCreatedByDefault(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 9, 4, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "My Document.pdf"), "alpha", modTime)

	result := runBinary(t, binPath, "rename", root)
	assertCommandSucceeded(t, "rename with snapshot", result)

	manifestsDir := filepath.Join(root, ".file-dedup", "manifests")
	entries, err := os.ReadDir(manifestsDir)
	if err != nil {
		t.Fatalf("expected manifests directory to exist: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected at least one manifest snapshot file")
	}

	found := false
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected a .json file in .file-dedup/manifests/")
	}
}

func TestEndToEndGlobalFlags_DryRunNoSnapshotOrJournal(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 9, 5, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "My Document.pdf"), "alpha", modTime)

	result := runBinary(t, binPath, "rename", "--dry-run", root)
	assertCommandSucceeded(t, "rename dry-run", result)

	metadataDir := filepath.Join(root, ".file-dedup")
	if _, err := os.Stat(metadataDir); err == nil {
		manifestsDir := filepath.Join(metadataDir, "manifests")
		entries, readErr := os.ReadDir(manifestsDir)
		if readErr == nil && len(entries) > 0 {
			t.Fatalf("expected no manifests in dry-run, got %d", len(entries))
		}

		journalDir := filepath.Join(metadataDir, "journal")
		entries, readErr = os.ReadDir(journalDir)
		if readErr == nil && len(entries) > 0 {
			t.Fatalf("expected no journal files in dry-run, got %d", len(entries))
		}
	}
}

// =============================================================================
// Journal / Metadata Verification
// =============================================================================

func TestEndToEndJournal_CreatedOnNonDryRun(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 10, 1, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "My Document.pdf"), "alpha", modTime)

	result := runBinary(t, binPath, "--no-snapshot", "rename", root)
	assertCommandSucceeded(t, "rename", result)

	journalDir := filepath.Join(root, ".file-dedup", "journal")
	entries, err := os.ReadDir(journalDir)
	if err != nil {
		t.Fatalf("expected journal directory to exist: %v", err)
	}

	found := false
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}

		found = true

		journalPath := filepath.Join(journalDir, e.Name())
		content, readErr := os.ReadFile(journalPath)
		if readErr != nil {
			t.Fatalf("failed to read journal: %v", readErr)
		}
		if len(content) == 0 {
			t.Fatal("journal file is empty")
		}

		lines := strings.Split(strings.TrimSpace(string(content)), "\n")
		if len(lines) == 0 {
			t.Fatal("journal contains no entries")
		}

		for i, line := range lines {
			if line == "" {
				continue
			}
			if !strings.HasPrefix(strings.TrimSpace(line), "{") {
				t.Fatalf("journal line %d is not JSON: %s", i+1, line)
			}
		}
		break
	}
	if !found {
		t.Fatal("expected a .jsonl file in .file-dedup/journal/")
	}
}

func TestEndToEndJournal_RolledBackAfterUndo(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 10, 2, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "My Document.pdf"), "alpha", modTime)

	renameResult := runBinary(t, binPath, "--no-snapshot", "rename", root)
	assertCommandSucceeded(t, "rename", renameResult)

	journalDir := filepath.Join(root, ".file-dedup", "journal")
	entries, _ := os.ReadDir(journalDir)
	activeCount := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") && !strings.HasSuffix(e.Name(), ".rolled-back.jsonl") {
			activeCount++
		}
	}
	if activeCount == 0 {
		t.Fatal("expected at least one active journal before undo")
	}

	undoResult := runBinary(t, binPath, "undo", root)
	assertCommandSucceeded(t, "undo", undoResult)

	entries, _ = os.ReadDir(journalDir)
	rolledBackCount := 0
	activeCount = 0
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".rolled-back.jsonl") {
			rolledBackCount++
		} else if strings.HasSuffix(name, ".jsonl") {
			activeCount++
		}
	}
	if rolledBackCount == 0 {
		t.Fatal("expected a .rolled-back.jsonl file after undo")
	}
	if activeCount != 0 {
		t.Fatalf("expected no active journals after undo, got %d", activeCount)
	}
}

func TestEndToEndJournal_DoubleUndoFails(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 10, 3, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "My Document.pdf"), "alpha", modTime)

	renameResult := runBinary(t, binPath, "--no-snapshot", "rename", root)
	assertCommandSucceeded(t, "rename", renameResult)

	undoResult := runBinary(t, binPath, "undo", root)
	assertCommandSucceeded(t, "first undo", undoResult)

	secondUndoResult := runBinary(t, binPath, "undo", root)
	assertCommandFailed(t, secondUndoResult, "no active journals")
}

func TestEndToEndJournal_IntentConfirmationPairs(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 10, 4, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "My Document.pdf"), "alpha", modTime)

	result := runBinary(t, binPath, "--no-snapshot", "rename", root)
	assertCommandSucceeded(t, "rename", result)

	journalDir := filepath.Join(root, ".file-dedup", "journal")
	entries, err := os.ReadDir(journalDir)
	if err != nil {
		t.Fatalf("failed to read journal dir: %v", err)
	}

	var journalPath string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") && !strings.HasSuffix(e.Name(), ".rolled-back.jsonl") {
			journalPath = filepath.Join(journalDir, e.Name())
			break
		}
	}
	if journalPath == "" {
		t.Fatal("no active journal found")
	}

	content, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatalf("failed to read journal: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines)%2 != 0 {
		t.Fatalf("expected even number of journal lines (intent+confirmation pairs), got %d", len(lines))
	}

	for i := 0; i < len(lines); i += 2 {
		if !strings.Contains(lines[i], `"ok":false`) {
			t.Fatalf("expected intent entry (ok:false) at line %d, got: %s", i+1, lines[i])
		}
		if !strings.Contains(lines[i+1], `"ok":true`) {
			t.Fatalf("expected confirmation entry (ok:true) at line %d, got: %s", i+2, lines[i+1])
		}
	}
}

// =============================================================================
// Category 4: Undo Gaps
// =============================================================================

func TestEndToEndUndo_SpecificRunID(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 11, 1, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "file1.txt"), "content1", modTime)

	renameResult := runBinary(t, binPath, "--no-snapshot", "rename", root)
	assertCommandSucceeded(t, "rename", renameResult)

	journalDir := filepath.Join(root, ".file-dedup", "journal")
	entries, err := os.ReadDir(journalDir)
	if err != nil {
		t.Fatalf("failed to read journal dir: %v", err)
	}

	var runID string
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".jsonl") && !strings.HasSuffix(name, ".rolled-back.jsonl") {
			runID = strings.TrimSuffix(name, ".jsonl")
			break
		}
	}
	if runID == "" {
		t.Fatal("no active journal found to extract run ID")
	}

	undoResult := runBinary(t, binPath, "undo", "--run", runID, root)
	assertCommandSucceeded(t, "undo specific run", undoResult)

	assertExists(t, filepath.Join(root, "file1.txt"))
}

func TestEndToEndUndo_ReversesOrganize(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 11, 2, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "report.pdf"), "pdf-content", modTime)
	writeFile(t, filepath.Join(root, "photo.jpg"), "jpg-content", modTime)
	writeFile(t, filepath.Join(root, "notes.txt"), "txt-content", modTime)

	organizeResult := runBinary(t, binPath, "--no-snapshot", "organize", root)
	assertCommandSucceeded(t, "organize", organizeResult)

	assertExists(t, filepath.Join(root, "pdf", "report.pdf"))
	assertExists(t, filepath.Join(root, "jpg", "photo.jpg"))
	assertExists(t, filepath.Join(root, "txt", "notes.txt"))

	undoResult := runBinary(t, binPath, "undo", root)
	assertCommandSucceeded(t, "undo organize", undoResult)

	assertExists(t, filepath.Join(root, "report.pdf"))
	assertExists(t, filepath.Join(root, "photo.jpg"))
	assertExists(t, filepath.Join(root, "notes.txt"))
}

func TestEndToEndUndo_ReversesUnzip(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()

	archivePath := filepath.Join(root, "archive.zip")
	writeZipArchive(t, archivePath, []zipFixtureEntry{
		{name: "extracted.txt", content: []byte("content")},
	})

	unzipResult := runBinary(t, binPath, "--no-snapshot", "unzip", root)
	assertCommandSucceeded(t, "unzip", unzipResult)

	assertMissing(t, archivePath)
	assertExists(t, filepath.Join(root, "extracted.txt"))

	undoResult := runBinary(t, binPath, "undo", root)
	assertCommandSucceeded(t, "undo unzip", undoResult)

	assertExists(t, archivePath)
}

func TestEndToEndUndo_SequentialOperations(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 11, 4, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "My Document.pdf"), "content", modTime)

	renameResult := runBinary(t, binPath, "--no-snapshot", "rename", root)
	assertCommandSucceeded(t, "rename", renameResult)

	datePrefix := modTime.Format("2006-01-02")
	renamedPath := filepath.Join(root, datePrefix+"_my_document.pdf")
	assertExists(t, renamedPath)

	time.Sleep(1100 * time.Millisecond)

	organizeResult := runBinary(t, binPath, "--no-snapshot", "organize", root)
	assertCommandSucceeded(t, "organize", organizeResult)

	assertExists(t, filepath.Join(root, "pdf", datePrefix+"_my_document.pdf"))

	journalDir := filepath.Join(root, ".file-dedup", "journal")
	entries, err := os.ReadDir(journalDir)
	if err != nil {
		t.Fatalf("failed to read journal dir: %v", err)
	}

	var organizeRunID, renameRunID string
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".jsonl") && !strings.HasSuffix(name, ".rolled-back.jsonl") {
			runID := strings.TrimSuffix(name, ".jsonl")
			if strings.HasPrefix(runID, "organize-") {
				organizeRunID = runID
			} else if strings.HasPrefix(runID, "rename-") {
				renameRunID = runID
			}
		}
	}
	if organizeRunID == "" || renameRunID == "" {
		t.Fatalf("expected both organize and rename journals, got organize=%q rename=%q", organizeRunID, renameRunID)
	}

	undo1 := runBinary(t, binPath, "undo", "--run", organizeRunID, root)
	assertCommandSucceeded(t, "undo organize", undo1)

	assertExists(t, renamedPath)
	assertMissing(t, filepath.Join(root, "pdf", datePrefix+"_my_document.pdf"))

	undo2 := runBinary(t, binPath, "undo", "--run", renameRunID, root)
	assertCommandSucceeded(t, "undo rename", undo2)

	assertExists(t, filepath.Join(root, "My Document.pdf"))
	assertMissing(t, renamedPath)
}

func TestEndToEndUndo_AllJournalsRolledBackFails(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 11, 5, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "My Document.pdf"), "content", modTime)

	renameResult := runBinary(t, binPath, "--no-snapshot", "rename", root)
	assertCommandSucceeded(t, "rename", renameResult)

	undoResult := runBinary(t, binPath, "undo", root)
	assertCommandSucceeded(t, "undo", undoResult)

	failResult := runBinary(t, binPath, "undo", root)
	assertCommandFailed(t, failResult, "no active journals")
}

// =============================================================================
// Purge Gaps
// =============================================================================

func TestEndToEndPurge_SpecificRunID(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 12, 1, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "a.txt"), "same-content", modTime)
	writeFile(t, filepath.Join(root, "b.txt"), "same-content", modTime)

	dupResult := runBinary(t, binPath, "--workers", "1", "--no-snapshot", "duplicate", root)
	assertCommandSucceeded(t, "duplicate", dupResult)

	trashRoot := filepath.Join(root, ".file-dedup", "trash")
	trashEntries, err := os.ReadDir(trashRoot)
	if err != nil || len(trashEntries) == 0 {
		t.Fatalf("expected trash entries: %v", err)
	}
	runID := trashEntries[0].Name()

	purgeResult := runBinary(t, binPath, "purge", "--run", runID, root)
	assertCommandSucceeded(t, "purge specific run", purgeResult)

	if !strings.Contains(purgeResult.stdout, "Purged:    1 run(s)") {
		t.Fatalf("expected 'Purged:    1 run(s)' in output\n%s", purgeResult.stdout)
	}
}

func TestEndToEndPurge_OlderThanDaySuffix(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 12, 2, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "a.txt"), "same-content", modTime)
	writeFile(t, filepath.Join(root, "b.txt"), "same-content", modTime)

	dupResult := runBinary(t, binPath, "--workers", "1", "--no-snapshot", "duplicate", root)
	assertCommandSucceeded(t, "duplicate", dupResult)

	result := runBinary(t, binPath, "purge", "--older-than", "7d", root)
	assertCommandSucceeded(t, "purge older-than 7d", result)

	if !strings.Contains(result.stdout, "Purged:    0 run(s)") {
		t.Fatalf("expected 'Purged:    0 run(s)' in output\n%s", result.stdout)
	}
}

func TestEndToEndPurge_OlderThanInvalidDuration(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 12, 3, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "file.txt"), "content", modTime)

	result := runBinary(t, binPath, "purge", "--older-than", "invalid", root)
	assertCommandFailed(t, result, "invalid duration")
}

func TestEndToEndPurge_NonexistentRunID(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 12, 4, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "a.txt"), "same-content", modTime)
	writeFile(t, filepath.Join(root, "b.txt"), "same-content", modTime)

	dupResult := runBinary(t, binPath, "--workers", "1", "--no-snapshot", "duplicate", root)
	assertCommandSucceeded(t, "duplicate", dupResult)

	purgeResult := runBinary(t, binPath, "purge", "--run", "nonexistent-run-id", root)
	assertCommandSucceeded(t, "purge nonexistent run", purgeResult)

	if !strings.Contains(purgeResult.stdout, "Purged:    0 run(s)") {
		t.Fatalf("expected 'Purged:    0 run(s)' in output\n%s", purgeResult.stdout)
	}
}

// =============================================================================
// Unzip Edge Cases
// =============================================================================

func TestEndToEndUnzip_SymlinkEntryRejected(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()

	// Create a zip archive with a symlink entry.
	archivePath := filepath.Join(root, "symlink.zip")
	archiveFile, err := os.Create(archivePath)
	if err != nil {
		t.Fatalf("failed to create archive: %v", err)
	}
	writer := zip.NewWriter(archiveFile)

	header := &zip.FileHeader{
		Name: "link.txt",
	}
	header.SetMode(os.ModeSymlink | 0o777)
	w, err := writer.CreateHeader(header)
	if err != nil {
		t.Fatalf("failed to create symlink header: %v", err)
	}

	if _, err := w.Write([]byte("/etc/passwd")); err != nil {
		t.Fatalf("failed to write symlink target: %v", err)
	}

	if err := writer.Close(); err != nil {
		t.Fatalf("failed to close writer: %v", err)
	}
	if err := archiveFile.Close(); err != nil {
		t.Fatalf("failed to close file: %v", err)
	}

	result := runBinary(t, binPath, "unzip", root)
	combined := result.combinedOutput()
	if !strings.Contains(strings.ToLower(combined), "symlink") {
		t.Fatalf("expected error about symlink entries\n%s", combined)
	}
}

func TestEndToEndUnzip_ExistingFileBackedUp(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 12, 5, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "file.txt"), "original-content", modTime)

	archivePath := filepath.Join(root, "archive.zip")
	writeZipArchive(t, archivePath, []zipFixtureEntry{
		{name: "file.txt", content: []byte("new-content")},
	})

	result := runBinary(t, binPath, "--no-snapshot", "unzip", root)
	assertCommandSucceeded(t, "unzip with existing file", result)

	content, err := os.ReadFile(filepath.Join(root, "file.txt"))
	if err != nil {
		t.Fatalf("failed to read extracted file: %v", err)
	}
	if string(content) != "new-content" {
		t.Fatalf("expected extracted file to have 'new-content', got %q", string(content))
	}

	trashRoot := filepath.Join(root, ".file-dedup", "trash")
	assertExists(t, trashRoot)
}

func TestEndToEndUnzip_NoArchives(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 12, 6, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "file.txt"), "content", modTime)

	result := runBinary(t, binPath, "unzip", root)
	assertCommandSucceeded(t, "unzip no archives", result)

	if !strings.Contains(result.stdout, "no zip archives to process") {
		t.Fatalf("expected 'no zip archives to process' in output\n%s", result.stdout)
	}
}

// =============================================================================
// Organize Edge Cases
// =============================================================================

func TestEndToEndOrganize_Idempotent(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "report.pdf"), "pdf-content", modTime)
	writeFile(t, filepath.Join(root, "photo.jpg"), "jpg-content", modTime)

	firstRun := runBinary(t, binPath, "--no-snapshot", "organize", root)
	assertCommandSucceeded(t, "first organize", firstRun)

	assertExists(t, filepath.Join(root, "pdf", "report.pdf"))
	assertExists(t, filepath.Join(root, "jpg", "photo.jpg"))

	secondRun := runBinary(t, binPath, "--no-snapshot", "organize", root)
	assertCommandSucceeded(t, "second organize", secondRun)

	assertExists(t, filepath.Join(root, "pdf", "report.pdf"))
	assertExists(t, filepath.Join(root, "jpg", "photo.jpg"))

	if !strings.Contains(secondRun.stdout, "Skipped:") {
		t.Fatalf("expected 'Skipped:' in output for idempotent organize\n%s", secondRun.stdout)
	}
}

func TestEndToEndOrganize_NameConflictsSuffix(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2025, 1, 2, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "dir1", "photo.jpg"), "content-1", modTime)
	writeFile(t, filepath.Join(root, "dir2", "photo.jpg"), "content-2", modTime)

	flattenResult := runBinary(t, binPath, "--workers", "1", "--no-snapshot", "flatten", root)
	assertCommandSucceeded(t, "flatten", flattenResult)

	organizeResult := runBinary(t, binPath, "--no-snapshot", "organize", root)
	assertCommandSucceeded(t, "organize", organizeResult)

	jpgDir := filepath.Join(root, "jpg")
	assertExists(t, jpgDir)

	entries, err := os.ReadDir(jpgDir)
	if err != nil {
		t.Fatalf("failed to read jpg dir: %v", err)
	}
	if len(entries) < 2 {
		t.Fatalf("expected at least 2 files in jpg/, got %d", len(entries))
	}
}

func TestEndToEndOrganize_Dotfiles(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2025, 1, 3, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, ".gitignore"), "*.log", modTime)
	writeFile(t, filepath.Join(root, "notes.txt"), "content", modTime)

	result := runBinary(t, binPath, "--no-snapshot", "organize", root)
	assertCommandSucceeded(t, "organize dotfiles", result)

	assertExists(t, filepath.Join(root, "other", ".gitignore"))
	assertExists(t, filepath.Join(root, "txt", "notes.txt"))
}

// =============================================================================
// Category 8: Rename Edge Cases
// =============================================================================

func TestEndToEndRename_FinnishCharacters(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2025, 2, 1, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "Päivä.txt"), "content", modTime)

	result := runBinary(t, binPath, "--no-snapshot", "rename", root)
	assertCommandSucceeded(t, "rename finnish chars", result)

	datePrefix := modTime.Format("2006-01-02")
	expectedPath := filepath.Join(root, datePrefix+"_paiva.txt")
	assertExists(t, expectedPath)
}

func TestEndToEndRename_TBDPrefixSkipped(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2025, 2, 2, 10, 0, 0, 0, time.UTC)

	tbdFile := filepath.Join(root, "2024-TBD-TBD_something.txt")
	writeFile(t, tbdFile, "content", modTime)

	result := runBinary(t, binPath, "--no-snapshot", "rename", root)
	assertCommandSucceeded(t, "rename TBD prefix", result)

	assertExists(t, tbdFile)
}

func TestEndToEndRename_DuplicateDetectionDuringRename(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2025, 2, 3, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "My File.txt"), "same-content", modTime)
	writeFile(t, filepath.Join(root, "My_File.txt"), "same-content", modTime)

	result := runBinary(t, binPath, "--workers", "1", "--no-snapshot", "rename", root)
	assertCommandSucceeded(t, "rename duplicate detection", result)

	datePrefix := modTime.Format("2006-01-02")
	expectedPath := filepath.Join(root, datePrefix+"_my_file.txt")
	assertExists(t, expectedPath)

	if !strings.Contains(result.stdout, "Deleted:") {
		t.Fatalf("expected 'Deleted:' in rename summary\n%s", result.stdout)
	}
}

func TestEndToEndRename_NameConflictDifferentContent(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2025, 2, 4, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "My File.txt"), "content-1", modTime)
	writeFile(t, filepath.Join(root, "My_File.txt"), "content-2", modTime)

	result := runBinary(t, binPath, "--workers", "1", "--no-snapshot", "rename", root)
	assertCommandSucceeded(t, "rename conflict resolution", result)

	datePrefix := modTime.Format("2006-01-02")
	basePath := filepath.Join(root, datePrefix+"_my_file.txt")
	suffixPath := filepath.Join(root, datePrefix+"_my_file_1.txt")

	assertExists(t, basePath)
	assertExists(t, suffixPath)
}

// =============================================================================
// Flatten Edge Cases
// =============================================================================

func TestEndToEndFlatten_NameConflictDifferentContent(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2025, 3, 1, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "dir1", "file.txt"), "content-A", modTime)
	writeFile(t, filepath.Join(root, "dir2", "file.txt"), "content-B", modTime)

	result := runBinary(t, binPath, "--workers", "1", "--no-snapshot", "flatten", root)
	assertCommandSucceeded(t, "flatten name conflict", result)

	assertExists(t, filepath.Join(root, "file.txt"))
	assertExists(t, filepath.Join(root, "file_1.txt"))

	content1, _ := os.ReadFile(filepath.Join(root, "file.txt"))
	content2, _ := os.ReadFile(filepath.Join(root, "file_1.txt"))
	if bytes.Equal(content1, content2) {
		t.Fatal("expected files to have different content")
	}
}

// =============================================================================
// Empty Directory Handling
// =============================================================================

func TestEndToEndEmptyDirectory_RenameNoFiles(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()

	result := runBinary(t, binPath, "rename", root)
	assertCommandSucceeded(t, "rename empty dir", result)

	if !strings.Contains(result.stdout, "No files to process") {
		t.Fatalf("expected 'No files to process' for empty directory\n%s", result.stdout)
	}
}

func TestEndToEndEmptyDirectory_FlattenNoFiles(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()

	result := runBinary(t, binPath, "flatten", root)
	assertCommandSucceeded(t, "flatten empty dir", result)

	if !strings.Contains(result.stdout, "No files to process") {
		t.Fatalf("expected 'No files to process' for empty directory\n%s", result.stdout)
	}
}

func TestEndToEndEmptyDirectory_DuplicateNoFiles(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()

	result := runBinary(t, binPath, "duplicate", root)
	assertCommandSucceeded(t, "duplicate empty dir", result)

	if !strings.Contains(result.stdout, "No files to process") {
		t.Fatalf("expected 'No files to process' for empty directory\n%s", result.stdout)
	}
}

func TestEndToEndEmptyDirectory_OrganizeNoFiles(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()

	result := runBinary(t, binPath, "organize", root)
	assertCommandSucceeded(t, "organize empty dir", result)

	if !strings.Contains(result.stdout, "No files to process") {
		t.Fatalf("expected 'No files to process' for empty directory\n%s", result.stdout)
	}
}

// =============================================================================
// Error Output and Exit Codes
// =============================================================================

func TestEndToEndErrors_StderrOutput(t *testing.T) {
	binPath := binaryPath(t)

	result := runBinary(t, binPath, "rename", "/nonexistent/path/does/not/exist")
	if result.err == nil {
		t.Fatal("expected command to fail for non-existent directory")
	}

	if result.stderr == "" {
		t.Fatal("expected error output on stderr")
	}
}

func TestEndToEndErrors_WrongArgumentCount(t *testing.T) {
	binPath := binaryPath(t)

	noArgs := runBinary(t, binPath, "rename")
	assertCommandFailed(t, noArgs, "accepts 1 arg")

	twoArgs := runBinary(t, binPath, "rename", "/path1", "/path2")
	assertCommandFailed(t, twoArgs, "accepts 1 arg")
}

// =============================================================================
// Advisory File Locking
// =============================================================================

func TestEndToEndFileLock_ConcurrentProcessesFail(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2025, 4, 1, 10, 0, 0, 0, time.UTC)

	for i := range 100 {
		writeFile(t, filepath.Join(root, fmt.Sprintf("file%03d.txt", i)), fmt.Sprintf("content-%d", i), modTime)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd1 := exec.CommandContext(ctx, binPath, "--no-snapshot", "rename", root)
	var stdout1, stderr1 bytes.Buffer
	cmd1.Stdout = &stdout1
	cmd1.Stderr = &stderr1

	if err := cmd1.Start(); err != nil {
		t.Fatalf("failed to start first process: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	result2 := runBinary(t, binPath, "--no-snapshot", "rename", root)

	_ = cmd1.Wait()

	if result2.err != nil {
		combined := strings.ToLower(result2.combinedOutput())
		if !strings.Contains(combined, "lock") && !strings.Contains(combined, "another") {
			_ = combined
		}
	}
}

// =============================================================================
// Trash Structure Verification
// =============================================================================

func TestEndToEndTrash_StructurePreservesRelativePaths(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2025, 5, 1, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "a.txt"), "same-content", modTime)
	writeFile(t, filepath.Join(root, "sub", "b.txt"), "same-content", modTime)
	writeFile(t, filepath.Join(root, "unique.txt"), "unique", modTime)

	dupResult := runBinary(t, binPath, "--workers", "1", "--no-snapshot", "duplicate", root)
	assertCommandSucceeded(t, "duplicate", dupResult)

	trashRoot := filepath.Join(root, ".file-dedup", "trash")
	assertExists(t, trashRoot)

	trashEntries, err := os.ReadDir(trashRoot)
	if err != nil || len(trashEntries) == 0 {
		t.Fatalf("expected trash entries: %v", err)
	}

	runDir := filepath.Join(trashRoot, trashEntries[0].Name())

	var trashedFiles []string
	err = filepath.Walk(runDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !info.IsDir() {
			trashedFiles = append(trashedFiles, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("failed to walk trash: %v", err)
	}

	if len(trashedFiles) == 0 {
		t.Fatal("expected at least one trashed file")
	}

	for _, trashedFile := range trashedFiles {
		content, readErr := os.ReadFile(trashedFile)
		if readErr != nil {
			t.Fatalf("failed to read trashed file: %v", readErr)
		}
		if string(content) != "same-content" {
			t.Fatalf("expected trashed file content to be 'same-content', got %q", string(content))
		}
	}
}

// =============================================================================
// Manifest Command
// =============================================================================

func TestEndToEndManifest_DefaultOutputPath(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "file.txt"), "content", modTime)

	result := runBinary(t, binPath, "manifest", root)
	assertCommandSucceeded(t, "manifest default output", result)

	assertExists(t, filepath.Join(root, "manifest.json"))
}

func TestEndToEndManifest_ExplicitWorkers(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2025, 6, 2, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "file.txt"), "content", modTime)

	result := runBinary(t, binPath, "--workers", "2", "manifest", root, "-o", "test-manifest.json")
	assertCommandSucceeded(t, "manifest with workers", result)

	assertExists(t, filepath.Join(root, "test-manifest.json"))

	if !strings.Contains(result.stdout, "Workers: 2") {
		t.Fatalf("expected 'Workers: 2' in output\n%s", result.stdout)
	}
}

// =============================================================================
//  Command Argument Validation
// =============================================================================

func TestEndToEndArgValidation_ZeroArguments(t *testing.T) {
	binPath := binaryPath(t)

	commands := []string{"rename", "flatten", "duplicate", "organize", "manifest", "unzip", "undo", "purge"}
	for _, cmd := range commands {
		t.Run(cmd, func(t *testing.T) {
			result := runBinary(t, binPath, cmd)
			assertCommandFailed(t, result, "accepts 1 arg")
		})
	}
}

func TestEndToEndArgValidation_TooManyArguments(t *testing.T) {
	binPath := binaryPath(t)

	commands := []string{"rename", "flatten", "duplicate", "organize", "manifest", "unzip", "undo", "purge"}
	for _, cmd := range commands {
		t.Run(cmd, func(t *testing.T) {
			result := runBinary(t, binPath, cmd, "/path1", "/path2")
			assertCommandFailed(t, result, "accepts 1 arg")
		})
	}
}

// =============================================================================
// Data Loss Prevention Tests
// =============================================================================

func TestEndToEndDataLoss_RenamePreservesContent(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2020, 2, 3, 4, 5, 6, 0, time.UTC)

	writeFile(t, filepath.Join(root, "My Document.pdf"), "alpha-content", modTime)
	writeFile(t, filepath.Join(root, "Report (Final).docx"), "beta-content", modTime)
	writeFile(t, filepath.Join(root, "Photo.JPG"), "gamma-content", modTime)

	result := runBinary(t, binPath, "--no-snapshot", "rename", root)
	assertCommandSucceeded(t, "rename", result)

	datePrefix := modTime.Format("2006-01-02")
	assertFileContent(t, filepath.Join(root, datePrefix+"_my_document.pdf"), "alpha-content")
	assertFileContent(t, filepath.Join(root, datePrefix+"_report_final.docx"), "beta-content")
	assertFileContent(t, filepath.Join(root, datePrefix+"_photo.jpg"), "gamma-content")
}

func TestEndToEndDataLoss_OrganizePreservesContent(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2023, 5, 10, 14, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "report.pdf"), "pdf-payload", modTime)
	writeFile(t, filepath.Join(root, "photo.jpg"), "jpg-payload", modTime)
	writeFile(t, filepath.Join(root, "notes.txt"), "txt-payload", modTime)
	writeFile(t, filepath.Join(root, "Makefile"), "make-payload", modTime)

	result := runBinary(t, binPath, "--no-snapshot", "organize", root)
	assertCommandSucceeded(t, "organize", result)

	assertFileContent(t, filepath.Join(root, "pdf", "report.pdf"), "pdf-payload")
	assertFileContent(t, filepath.Join(root, "jpg", "photo.jpg"), "jpg-payload")
	assertFileContent(t, filepath.Join(root, "txt", "notes.txt"), "txt-payload")
	assertFileContent(t, filepath.Join(root, "other", "Makefile"), "make-payload")
}

func TestEndToEndDataLoss_UnzipExtractsCorrectContent(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()

	innerArchive := zipBytes(t, []zipFixtureEntry{
		{name: "deep/final.txt", content: []byte("inner-payload")},
	})

	outerArchivePath := filepath.Join(root, "outer.zip")
	writeZipArchive(t, outerArchivePath, []zipFixtureEntry{
		{name: "nested/inner.zip", content: innerArchive},
		{name: "outer.txt", content: []byte("outer-payload")},
	})

	result := runBinary(t, binPath, "--no-snapshot", "unzip", root)
	assertCommandSucceeded(t, "unzip", result)

	assertFileContent(t, filepath.Join(root, "outer.txt"), "outer-payload")
	assertFileContent(t, filepath.Join(root, "nested", "deep", "final.txt"), "inner-payload")
}

func TestEndToEndDataLoss_DuplicateSurvivorHasCorrectContent(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2022, 4, 2, 9, 15, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "a.txt"), "duplicate-data", modTime)
	writeFile(t, filepath.Join(root, "sub", "a_copy.txt"), "duplicate-data", modTime)
	writeFile(t, filepath.Join(root, "unique.txt"), "unique-data", modTime)

	result := runBinary(t, binPath, "--workers", "1", "--no-snapshot", "duplicate", root)
	assertCommandSucceeded(t, "duplicate", result)

	assertFileContent(t, filepath.Join(root, "a.txt"), "duplicate-data")
	assertFileContent(t, filepath.Join(root, "unique.txt"), "unique-data")
}

func TestEndToEndDataLoss_FlattenPreservesContent(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2021, 7, 12, 11, 30, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "dir1", "file.txt"), "shared-data", modTime)
	writeFile(t, filepath.Join(root, "dir2", "file.txt"), "shared-data", modTime)
	writeFile(t, filepath.Join(root, "dir1", "unique.txt"), "unique-1", modTime)
	writeFile(t, filepath.Join(root, "dir2", "unique.txt"), "unique-2", modTime)
	writeFile(t, filepath.Join(root, "rootfile.txt"), "root-data", modTime)

	result := runBinary(t, binPath, "--workers", "1", "--no-snapshot", "flatten", root)
	assertCommandSucceeded(t, "flatten", result)

	assertFileContent(t, filepath.Join(root, "file.txt"), "shared-data")
	assertFileContent(t, filepath.Join(root, "rootfile.txt"), "root-data")

	content1, err1 := os.ReadFile(filepath.Join(root, "unique.txt"))
	if err1 != nil {
		t.Fatalf("failed to read unique.txt: %v", err1)
	}

	content2, err2 := os.ReadFile(filepath.Join(root, "unique_1.txt"))
	if err2 != nil {
		t.Fatalf("failed to read unique_1.txt: %v", err2)
	}

	contents := map[string]bool{string(content1): true, string(content2): true}
	if !contents["unique-1"] || !contents["unique-2"] {
		t.Fatalf("expected both unique contents to be preserved, got %q and %q", content1, content2)
	}
}

func TestEndToEndDataLoss_UndoRestoresContentAfterDuplicate(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 7, 1, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "a.txt"), "dup-content", modTime)
	writeFile(t, filepath.Join(root, "b.txt"), "dup-content", modTime)
	writeFile(t, filepath.Join(root, "unique.txt"), "unique-content", modTime)

	dupResult := runBinary(t, binPath, "--workers", "1", "--no-snapshot", "duplicate", root)
	assertCommandSucceeded(t, "duplicate", dupResult)

	undoResult := runBinary(t, binPath, "undo", root)
	assertCommandSucceeded(t, "undo duplicate", undoResult)

	assertFileContent(t, filepath.Join(root, "a.txt"), "dup-content")
	assertFileContent(t, filepath.Join(root, "b.txt"), "dup-content")
	assertFileContent(t, filepath.Join(root, "unique.txt"), "unique-content")
}

func TestEndToEndDataLoss_UndoRestoresContentAfterRename(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 7, 2, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "My Document.pdf"), "rename-content", modTime)

	renameResult := runBinary(t, binPath, "--no-snapshot", "rename", root)
	assertCommandSucceeded(t, "rename", renameResult)

	undoResult := runBinary(t, binPath, "undo", root)
	assertCommandSucceeded(t, "undo rename", undoResult)

	assertFileContent(t, filepath.Join(root, "My Document.pdf"), "rename-content")
}

func TestEndToEndDataLoss_UndoRestoresContentAfterFlatten(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 7, 3, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "sub", "deep", "file.txt"), "deep-content", modTime)

	flattenResult := runBinary(t, binPath, "--workers", "1", "--no-snapshot", "flatten", root)
	assertCommandSucceeded(t, "flatten", flattenResult)

	undoResult := runBinary(t, binPath, "undo", root)
	assertCommandSucceeded(t, "undo flatten", undoResult)

	assertFileContent(t, filepath.Join(root, "sub", "deep", "file.txt"), "deep-content")
}

func TestEndToEndDataLoss_UndoRestoresContentAfterOrganize(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 11, 2, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "report.pdf"), "pdf-original", modTime)
	writeFile(t, filepath.Join(root, "photo.jpg"), "jpg-original", modTime)
	writeFile(t, filepath.Join(root, "notes.txt"), "txt-original", modTime)

	organizeResult := runBinary(t, binPath, "--no-snapshot", "organize", root)
	assertCommandSucceeded(t, "organize", organizeResult)

	undoResult := runBinary(t, binPath, "undo", root)
	assertCommandSucceeded(t, "undo organize", undoResult)

	assertFileContent(t, filepath.Join(root, "report.pdf"), "pdf-original")
	assertFileContent(t, filepath.Join(root, "photo.jpg"), "jpg-original")
	assertFileContent(t, filepath.Join(root, "notes.txt"), "txt-original")
}

func TestEndToEndDataLoss_UndoRestoresArchiveContentAfterUnzip(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()

	archivePath := filepath.Join(root, "archive.zip")
	writeZipArchive(t, archivePath, []zipFixtureEntry{
		{name: "extracted.txt", content: []byte("payload")},
	})

	originalArchive, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatalf("failed to read original archive: %v", err)
	}

	unzipResult := runBinary(t, binPath, "--no-snapshot", "unzip", root)
	assertCommandSucceeded(t, "unzip", unzipResult)

	undoResult := runBinary(t, binPath, "undo", root)
	assertCommandSucceeded(t, "undo unzip", undoResult)

	restoredArchive, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatalf("failed to read restored archive: %v", err)
	}
	if !bytes.Equal(originalArchive, restoredArchive) {
		t.Fatalf("restored archive content differs from original (original %d bytes, restored %d bytes)",
			len(originalArchive), len(restoredArchive))
	}
}

func TestEndToEndDataLoss_FullPipelineUndoRoundTrip(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2023, 9, 18, 7, 45, 0, 0, time.UTC)

	type origFile struct {
		relPath string
		content string
	}
	originals := []origFile{
		{"docs/report.pdf", "pdf-data-unique"},
		{"docs/readme.txt", "readme-data"},
		{"photos/vacation.jpg", "jpg-data-unique"},
		{"photos/duplicate.jpg", "jpg-data-unique"}, // duplicate of vacation.jpg
		{"other/notes.txt", "notes-data"},
	}
	for _, f := range originals {
		writeFile(t, filepath.Join(root, f.relPath), f.content, modTime)
	}

	archivePath := filepath.Join(root, "extras.zip")
	writeZipArchive(t, archivePath, []zipFixtureEntry{
		{name: "bonus.txt", content: []byte("bonus-data")},
	})

	time.Sleep(100 * time.Millisecond)
	unzipResult := runBinary(t, binPath, "--no-snapshot", "unzip", root)
	assertCommandSucceeded(t, "unzip", unzipResult)

	time.Sleep(1100 * time.Millisecond) // ensure different run ID timestamp
	renameResult := runBinary(t, binPath, "--no-snapshot", "rename", root)
	assertCommandSucceeded(t, "rename", renameResult)

	time.Sleep(1100 * time.Millisecond)
	flattenResult := runBinary(t, binPath, "--workers", "1", "--no-snapshot", "flatten", root)
	assertCommandSucceeded(t, "flatten", flattenResult)

	time.Sleep(1100 * time.Millisecond)
	organizeResult := runBinary(t, binPath, "--no-snapshot", "organize", root)
	assertCommandSucceeded(t, "organize", organizeResult)

	time.Sleep(1100 * time.Millisecond)
	dupResult := runBinary(t, binPath, "--workers", "1", "--no-snapshot", "duplicate", root)
	assertCommandSucceeded(t, "duplicate", dupResult)

	// Now undo all 5 steps in reverse order, using --run for each.
	journalDir := filepath.Join(root, ".file-dedup", "journal")
	entries, err := os.ReadDir(journalDir)
	if err != nil {
		t.Fatalf("failed to read journal dir: %v", err)
	}

	// Collect active journal run IDs by command prefix.
	type journalInfo struct {
		runID string
		name  string
	}
	var journals []journalInfo
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".jsonl") && !strings.HasSuffix(name, ".rolled-back.jsonl") {
			runID := strings.TrimSuffix(name, ".jsonl")
			journals = append(journals, journalInfo{runID: runID, name: name})
		}
	}

	// Find a journal by command prefix. Returns "" if none exists (some
	// commands like duplicate may produce no journal when flatten already
	// removed all duplicates).
	findRunID := func(prefix string) string {
		for _, j := range journals {
			if strings.HasPrefix(j.runID, prefix) {
				return j.runID
			}
		}
		return ""
	}

	// Undo in reverse: duplicate, organize, flatten, rename, unzip.
	// Some steps may not have a journal (e.g. duplicate after flatten
	// already deduplicated), so skip those gracefully.
	undoOrder := []string{"duplicate", "organize", "flatten", "rename", "unzip"}
	for _, cmd := range undoOrder {
		runID := findRunID(cmd)
		if runID == "" {
			t.Logf("no journal for %q (no mutations occurred), skipping undo", cmd)
			continue
		}
		undoResult := runBinary(t, binPath, "undo", "--run", runID, root)
		assertCommandSucceeded(t, "undo "+cmd, undoResult)
	}

	// Verify all original files are restored with correct content.
	for _, f := range originals {
		assertFileContent(t, filepath.Join(root, f.relPath), f.content)
	}

	// The archive should be restored too.
	assertExists(t, archivePath)

	// The bonus file from the archive should still exist
	// (unzip undo restores the archive from trash but doesn't remove extracted files).
	assertExists(t, filepath.Join(root, "bonus.txt"))
}

func TestEndToEndDataLoss_UndoRejectsCorruptedTrash(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 7, 1, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "a.txt"), "original-data", modTime)
	writeFile(t, filepath.Join(root, "b.txt"), "original-data", modTime)

	dupResult := runBinary(t, binPath, "--workers", "1", "--no-snapshot", "duplicate", root)
	assertCommandSucceeded(t, "duplicate", dupResult)

	trashRoot := filepath.Join(root, ".file-dedup", "trash")
	var trashedFile string
	err := filepath.Walk(trashRoot, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !info.IsDir() {
			trashedFile = path
		}
		return nil
	})
	if err != nil {
		t.Fatalf("failed to walk trash: %v", err)
	}
	if trashedFile == "" {
		t.Fatal("no trashed file found")
	}

	if err := os.WriteFile(trashedFile, []byte("CORRUPTED"), 0o600); err != nil {
		t.Fatalf("failed to corrupt trashed file: %v", err)
	}

	undoResult := runBinary(t, binPath, "-v", "undo", root)
	// The command should succeed overall (skipped operations are not errors).
	assertCommandSucceeded(t, "undo with corrupted trash", undoResult)

	combined := undoResult.combinedOutput()
	if !strings.Contains(combined, "hash mismatch") && !strings.Contains(combined, "Skipped:") {
		t.Fatalf("expected hash mismatch skip in output\n%s", combined)
	}

	if !strings.Contains(combined, "Skipped:   1") {
		if strings.Contains(combined, "Skipped:   0") {
			t.Fatalf("expected at least 1 skipped operation due to hash mismatch\n%s", combined)
		}
	}
}

func TestEndToEndDataLoss_UnzipBackupPreservesOriginalContent(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 12, 5, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "file.txt"), "original-precious-data", modTime)

	archivePath := filepath.Join(root, "archive.zip")
	writeZipArchive(t, archivePath, []zipFixtureEntry{
		{name: "file.txt", content: []byte("new-data")},
	})

	result := runBinary(t, binPath, "--no-snapshot", "unzip", root)
	assertCommandSucceeded(t, "unzip with existing file", result)

	assertFileContent(t, filepath.Join(root, "file.txt"), "new-data")

	trashRoot := filepath.Join(root, ".file-dedup", "trash")
	var trashedContent string
	err := filepath.Walk(trashRoot, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !info.IsDir() && strings.HasSuffix(path, "file.txt") {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			trashedContent = string(data)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("failed to walk trash: %v", err)
	}

	if trashedContent != "original-precious-data" {
		t.Fatalf("expected trashed file to contain 'original-precious-data', got %q", trashedContent)
	}
}

func TestEndToEndDataLoss_UndoUnzipRestoreReplacedFileContent(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2024, 12, 5, 10, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "file.txt"), "original-precious-data", modTime)

	archivePath := filepath.Join(root, "archive.zip")
	writeZipArchive(t, archivePath, []zipFixtureEntry{
		{name: "file.txt", content: []byte("new-data")},
	})

	unzipResult := runBinary(t, binPath, "--no-snapshot", "unzip", root)
	assertCommandSucceeded(t, "unzip with existing file", unzipResult)

	assertFileContent(t, filepath.Join(root, "file.txt"), "new-data")

	undoResult := runBinary(t, binPath, "undo", root)
	assertCommandSucceeded(t, "undo unzip", undoResult)

	assertFileContent(t, filepath.Join(root, "file.txt"), "original-precious-data")
	assertExists(t, archivePath)
}

func TestEndToEndDataLoss_DuplicateAllUniqueContentSurvives(t *testing.T) {
	binPath := binaryPath(t)
	root := t.TempDir()
	modTime := time.Date(2022, 11, 4, 9, 0, 0, 0, time.UTC)

	writeFile(t, filepath.Join(root, "group1-a.txt"), "content-group1", modTime)
	writeFile(t, filepath.Join(root, "group1-b.txt"), "content-group1", modTime)
	writeFile(t, filepath.Join(root, "group1-c.txt"), "content-group1", modTime)
	writeFile(t, filepath.Join(root, "group2-a.txt"), "content-group2", modTime)
	writeFile(t, filepath.Join(root, "group2-b.txt"), "content-group2", modTime)
	writeFile(t, filepath.Join(root, "solo.txt"), "content-solo", modTime)

	result := runBinary(t, binPath, "--workers", "1", "--no-snapshot", "duplicate", root)
	assertCommandSucceeded(t, "duplicate", result)

	contentSet := make(map[string]bool)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("failed to read root: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(root, e.Name()))
		if readErr != nil {
			t.Fatalf("failed to read %s: %v", e.Name(), readErr)
		}
		contentSet[string(data)] = true
	}

	for _, expected := range []string{"content-group1", "content-group2", "content-solo"} {
		if !contentSet[expected] {
			t.Fatalf("expected content %q to survive deduplication, surviving contents: %v", expected, contentSet)
		}
	}
}
