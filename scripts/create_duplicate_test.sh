#!/bin/bash
# Create a test directory structure specifically for testing the duplicate command.
# This simulates the scenario after flatten/rename where duplicate files exist.

set -e

TEST_DIR="${1:-/tmp/duplicate-test}"

echo "Creating duplicate test structure in: $TEST_DIR"

# Clean up if exists
rm -rf "$TEST_DIR"
mkdir -p "$TEST_DIR"

# ============================================================
# Scenario 1: Exact duplicates with _N suffix (common after flatten)
# ============================================================
echo "video content 12345" > "$TEST_DIR/2017-08-03_4_cs_frags_eco_round.flv"
echo "video content 12345" > "$TEST_DIR/2017-08-03_4_cs_frags_eco_round_1.flv"
echo "video content 12345" > "$TEST_DIR/2017-08-03_4_cs_frags_eco_round_2.flv"

# ============================================================
# Scenario 2: Duplicates with different names entirely
# ============================================================
echo "document content xyz" > "$TEST_DIR/report.pdf"
echo "document content xyz" > "$TEST_DIR/report_backup.pdf"
echo "document content xyz" > "$TEST_DIR/report_copy.pdf"
echo "document content xyz" > "$TEST_DIR/old_report.pdf"

# ============================================================
# Scenario 3: Same name pattern but DIFFERENT content (should NOT be deleted)
# ============================================================
echo "unique content A" > "$TEST_DIR/notes.txt"
echo "unique content B" > "$TEST_DIR/notes_1.txt"
echo "unique content C" > "$TEST_DIR/notes_2.txt"

# ============================================================
# Scenario 4: Duplicates in subdirectories
# ============================================================
mkdir -p "$TEST_DIR/subdir1"
mkdir -p "$TEST_DIR/subdir2"
echo "photo data same" > "$TEST_DIR/subdir1/photo.jpg"
echo "photo data same" > "$TEST_DIR/subdir2/photo.jpg"
echo "photo data same" > "$TEST_DIR/photo_from_backup.jpg"

# ============================================================
# Scenario 5: Large file simulation (will trigger partial hash)
# ============================================================
# Create 20KB files with identical content
dd if=/dev/zero bs=1024 count=20 2>/dev/null | tr '\0' 'A' > "$TEST_DIR/large_file.bin"
cp "$TEST_DIR/large_file.bin" "$TEST_DIR/large_file_copy.bin"

# Create another 20KB file with DIFFERENT content (same size)
dd if=/dev/zero bs=1024 count=20 2>/dev/null | tr '\0' 'B' > "$TEST_DIR/large_different.bin"

# ============================================================
# Scenario 6: Empty files (edge case)
# ============================================================
touch "$TEST_DIR/empty1.txt"
touch "$TEST_DIR/empty2.txt"

# ============================================================
# Scenario 7: Unique files (should NOT be touched)
# ============================================================
echo "completely unique 1" > "$TEST_DIR/unique_file_1.txt"
echo "completely unique 2" > "$TEST_DIR/unique_file_2.txt"
echo "completely unique 3" > "$TEST_DIR/unique_file_3.txt"

# Set consistent modification times
find "$TEST_DIR" -type f -exec touch -d "2018-06-15 12:00:00" {} \;

# Show structure
echo ""
echo "============================================================"
echo "Test structure created in: $TEST_DIR"
echo "============================================================"
echo ""
echo "Total files:"
find "$TEST_DIR" -type f | wc -l
echo ""
echo "Expected duplicates to remove:"
echo "  - 2x 2017-08-03_4_cs_frags_eco_round*.flv (keep 1 of 3)"
echo "  - 3x report*.pdf (keep 1 of 4)"
echo "  - 2x photo*.jpg (keep 1 of 3)"
echo "  - 1x large_file*.bin (keep 1 of 2)"
echo "  - 1x empty*.txt (keep 1 of 2)"
echo "  Total: ~9 duplicates should be found"
echo ""
echo "Files that should NOT be deleted:"
echo "  - notes.txt, notes_1.txt, notes_2.txt (different content)"
echo "  - large_different.bin (different content)"
echo "  - unique_file_*.txt (unique files)"
echo ""
echo "File listing:"
find "$TEST_DIR" -type f | sort
echo ""
echo "============================================================"
echo "Test commands:"
echo "============================================================"
echo ""
echo "# Build the tool:"
echo "make build"
echo ""
echo "# Preview duplicates (DRY RUN - recommended first!):"
echo "./file-dedup duplicate --dry-run $TEST_DIR"
echo ""
echo "# Actually remove duplicates:"
echo "./file-dedup duplicate $TEST_DIR"
echo ""
echo "# Verbose output:"
echo "./file-dedup duplicate -v --dry-run $TEST_DIR"
