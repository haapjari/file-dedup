package sanitizer

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestSanitizeFilename(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "simple filename",
			input:    "KeePass.db",
			expected: "keepass.db",
		},
		{
			name:     "filename with spaces",
			input:    "My Document.pdf",
			expected: "my_document.pdf",
		},
		{
			name:     "filename with multiple spaces",
			input:    "My   Big   Document.pdf",
			expected: "my_big_document.pdf",
		},
		{
			name:     "filename with parentheses",
			input:    "CV (kesken).docx",
			expected: "cv_kesken.docx",
		},
		{
			name:     "filename with Finnish characters",
			input:    "Päätös.txt",
			expected: "paatos.txt",
		},
		{
			name:     "filename with multiple Finnish characters",
			input:    "Työpöytä.txt",
			expected: "tyopoyta.txt",
		},
		{
			name:     "filename with brackets",
			input:    "Document [draft].pdf",
			expected: "document_draft.pdf",
		},
		{
			name:     "brackets inside word are removed not replaced",
			input:    "ab(cd)ef.txt",
			expected: "abcdef.txt",
		},
		{
			name:     "filename with special characters",
			input:    "Report@2018#final!.pdf",
			expected: "report-2018-final.pdf",
		},
		{
			name:     "multiple special characters collapse to one hyphen",
			input:    "Report@@@Final.pdf",
			expected: "report-final.pdf",
		},
		{
			name:     "multiple underscores collapse to one underscore",
			input:    "Report   Final.pdf",
			expected: "report_final.pdf",
		},
		{
			name:     "filename with date already",
			input:    "2018-03-29 18-59-06.flv",
			expected: "2018-03-29_18-59-06.flv",
		},
		{
			name:     "WhatsApp image format",
			input:    "WhatsApp Image 2018-03-18 at 22.29.50.jpeg",
			expected: "whatsapp_image_2018-03-18_at_22.29.50.jpeg",
		},
		{
			name:     "filename with uppercase extension",
			input:    "Image.JPG",
			expected: "image.jpg",
		},
		{
			name:     "filename with mixed case",
			input:    "MyDocument.PDF",
			expected: "mydocument.pdf",
		},
		{
			name:     "complex Finnish filename",
			input:    "Kokouspöytäkirja (11.4.2018).pdf",
			expected: "kokouspoytakirja_11.4.2018.pdf",
		},
		{
			name:     "filename with trailing space",
			input:    "Document .pdf",
			expected: "document.pdf",
		},
		{
			name:     "filename with leading space",
			input:    " Document.pdf",
			expected: "document.pdf",
		},
		{
			name:     "VID format from phone",
			input:    "VID-20180319-WA0006 1.mp4",
			expected: "vid-20180319-wa0006_1.mp4",
		},
		{
			name:     "VID underscore format",
			input:    "VID_20180804_182407.mp4",
			expected: "vid_20180804_182407.mp4",
		},
		{
			name:     "filename with tilde",
			input:    "~$CV.pdf",
			expected: "cv.pdf",
		},
		{
			name:     "shortcut file",
			input:    "Työ.lnk",
			expected: "tyo.lnk",
		},
		{
			name:     "filename with comma",
			input:    "Report, Final.pdf",
			expected: "report_final.pdf",
		},
		{
			name:     "empty extension",
			input:    "README",
			expected: "readme",
		},
		{
			name:     "double extension",
			input:    "archive.tar.gz",
			expected: "archive.tar.gz",
		},
		{
			name:     "filename with dots",
			input:    "v1.0.0.release.zip",
			expected: "v1.0.0.release.zip",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := SanitizeFilename(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestReplaceFinnishChars(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"ä", "a"},
		{"ö", "o"},
		{"å", "a"},
		{"Ä", "a"},
		{"Ö", "o"},
		{"Å", "a"},
		{"Päivä", "Paiva"},
		{"Työpöytä", "Tyopoyta"},
		{"normal", "normal"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := replaceFinnishChars(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGenerateTimestampedName(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		modTime  time.Time
		expected string
	}{
		{
			name:     "simple file",
			filename: "KeePass.db",
			modTime:  time.Date(2018, 1, 1, 12, 0, 0, 0, time.UTC),
			expected: "2018-01-01_keepass.db",
		},
		{
			name:     "file with spaces",
			filename: "My Document.pdf",
			modTime:  time.Date(2018, 3, 15, 10, 30, 0, 0, time.UTC),
			expected: "2018-03-15_my_document.pdf",
		},
		{
			name:     "Finnish filename",
			filename: "Työpöytä.txt",
			modTime:  time.Date(2018, 12, 31, 23, 59, 59, 0, time.UTC),
			expected: "2018-12-31_tyopoyta.txt",
		},
		{
			name:     "complex filename",
			filename: "CV (kesken).docx",
			modTime:  time.Date(2018, 6, 1, 0, 0, 0, 0, time.UTC),
			expected: "2018-06-01_cv_kesken.docx",
		},
		{
			name:     "double date prefix collapsed",
			filename: "2025-01-01_2025-01-01_report.pdf",
			modTime:  time.Date(2025, 1, 1, 8, 0, 0, 0, time.UTC),
			expected: "2025-01-01_report.pdf",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GenerateTimestampedName(tt.filename, tt.modTime)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestSanitizeFilename_EdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "only special characters",
			input:    "@#$%.txt",
			expected: "unnamed.txt",
		},
		{
			name:     "only spaces before extension",
			input:    "   .pdf",
			expected: "unnamed.pdf",
		},
		{
			name:     "consecutive mixed separators",
			input:    "file_-_name.txt",
			expected: "file_name.txt",
		},
		{
			name:     "multiple dots in name",
			input:    "file...name.txt",
			expected: "file...name.txt",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := SanitizeFilename(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}
