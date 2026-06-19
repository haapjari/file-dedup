#!/bin/bash
# Create a comprehensive test directory structure for testing file-dedup

set -e

TEST_DIR="${1:-/tmp/file-dedup-test}"

echo "Creating test structure in: $TEST_DIR"

# Clean up if exists
rm -rf "$TEST_DIR"
mkdir -p "$TEST_DIR"

# Level 1 files
echo "level1 content" > "$TEST_DIR/My Document.pdf"
echo "level1 content" > "$TEST_DIR/Työpöytä Backup.txt"
echo "level1 content" > "$TEST_DIR/Report (Final).docx"

# Level 2 - Documents
mkdir -p "$TEST_DIR/Documents"
echo "docs content" > "$TEST_DIR/Documents/Tärkeitä Muistioita.txt"
echo "docs content" > "$TEST_DIR/Documents/Meeting Notes 2018.pdf"
echo "duplicate content" > "$TEST_DIR/Documents/duplicate_test.txt"

# Level 3 - Documents/Work
mkdir -p "$TEST_DIR/Documents/Work"
echo "work content" > "$TEST_DIR/Documents/Work/Project Ääkköset.xlsx"
echo "work content" > "$TEST_DIR/Documents/Work/Presentation (v2).pptx"
echo "duplicate content" > "$TEST_DIR/Documents/Work/duplicate_test.txt"  # Same content = duplicate

# Level 4 - Documents/Work/2018
mkdir -p "$TEST_DIR/Documents/Work/2018"
echo "2018 content" > "$TEST_DIR/Documents/Work/2018/Q1 Report.pdf"
echo "2018 content" > "$TEST_DIR/Documents/Work/2018/Q2 Report.pdf"
echo "2018 content" > "$TEST_DIR/Documents/Work/2018/Käyttöohje.txt"

# Level 5 - Documents/Work/2018/Archives
mkdir -p "$TEST_DIR/Documents/Work/2018/Archives"
echo "archive content" > "$TEST_DIR/Documents/Work/2018/Archives/Old File.bak"
echo "archive content" > "$TEST_DIR/Documents/Work/2018/Archives/Vanhät Tiedostöt.zip"
echo "unique content 1" > "$TEST_DIR/Documents/Work/2018/Archives/file.txt"  # Same name, different content

# Level 2 - Photos
mkdir -p "$TEST_DIR/Photos"
echo "photo data" > "$TEST_DIR/Photos/Valokuva 001.jpg"
echo "photo data" > "$TEST_DIR/Photos/Kesä 2018.png"

# Level 3 - Photos/Vacation
mkdir -p "$TEST_DIR/Photos/Vacation"
echo "vacation photo" > "$TEST_DIR/Photos/Vacation/Beach (1).jpg"
echo "vacation photo" > "$TEST_DIR/Photos/Vacation/Beach (2).jpg"

# Level 4 - Photos/Vacation/Finland
mkdir -p "$TEST_DIR/Photos/Vacation/Finland"
echo "finland photo" > "$TEST_DIR/Photos/Vacation/Finland/Helsinki Näkymä.jpg"
echo "finland photo" > "$TEST_DIR/Photos/Vacation/Finland/Turku.jpg"

# Level 5 - Photos/Vacation/Finland/Lapland
mkdir -p "$TEST_DIR/Photos/Vacation/Finland/Lapland"
echo "lapland photo" > "$TEST_DIR/Photos/Vacation/Finland/Lapland/Aurora Borealis.jpg"
echo "lapland photo" > "$TEST_DIR/Photos/Vacation/Finland/Lapland/Ski Resort.jpg"

# Level 2 - Music (another branch)
mkdir -p "$TEST_DIR/Music"
echo "music data" > "$TEST_DIR/Music/Kappale 1.mp3"
echo "music data" > "$TEST_DIR/Music/Kappale 2.mp3"

# Level 3 - Music/Finnish
mkdir -p "$TEST_DIR/Music/Finnish"
echo "finnish music" > "$TEST_DIR/Music/Finnish/Sävel.mp3"
echo "finnish music" > "$TEST_DIR/Music/Finnish/Laulu Äidille.mp3"

# Level 4 - Music/Finnish/Rock
mkdir -p "$TEST_DIR/Music/Finnish/Rock"
echo "rock music" > "$TEST_DIR/Music/Finnish/Rock/Heavy Riff.mp3"
echo "unique content 2" > "$TEST_DIR/Music/Finnish/Rock/file.txt"  # Same name, different content

# Level 5 - Music/Finnish/Rock/2018
mkdir -p "$TEST_DIR/Music/Finnish/Rock/2018"
echo "2018 rock" > "$TEST_DIR/Music/Finnish/Rock/2018/Concert.mp3"
echo "2018 rock" > "$TEST_DIR/Music/Finnish/Rock/2018/Live Recording.mp3"

# Set modification times to 2018 for all files
find "$TEST_DIR" -type f -exec touch -d "2018-06-15 12:00:00" {} \;

# Show structure
echo ""
echo "Test structure created:"
find "$TEST_DIR" -type f | wc -l
echo "files created"
echo ""
echo "Directory tree:"
find "$TEST_DIR" -type d | head -20
echo ""
echo "Sample files:"
find "$TEST_DIR" -type f | head -10
