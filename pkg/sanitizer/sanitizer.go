// Package sanitizer provides filename sanitization utilities.
// It converts filenames to a consistent format: lowercase, kebab-case with underscores for spaces.
package sanitizer

import (
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var (
	// bracketsRegex matches parentheses, brackets, braces.
	bracketsRegex = regexp.MustCompile(`[()[\]{}]`)
	// specialCharsRegex matches any character that's not alphanumeric, underscore, hyphen, or dot.
	specialCharsRegex = regexp.MustCompile(`[^a-z0-9_\-.]`)
	// multiHyphenRegex matches multiple consecutive hyphens.
	multiHyphenRegex = regexp.MustCompile(`-+`)
	// multiUnderscoreRegex matches multiple consecutive underscores.
	multiUnderscoreRegex = regexp.MustCompile(`_+`)
	// trailingRegex matches trailing hyphens or underscores (before extension).
	trailingRegex = regexp.MustCompile(`[-_]+$`)
	// leadingRegex matches leading hyphens or underscores.
	leadingRegex = regexp.MustCompile(`^[-_]+`)
	// mixedSeparatorRegex matches underscore-hyphen or hyphen-underscore combinations.
	mixedSeparatorRegex = regexp.MustCompile(`(_-)|(-_)`)
)

// finnishReplacements maps Finnish characters to ASCII equivalents.
var finnishReplacements = map[rune]rune{
	'ä': 'a', 'Ä': 'a',
	'ö': 'o', 'Ö': 'o',
	'å': 'a', 'Å': 'a',
}

// SanitizeFilename converts a filename to the standard format:
// lowercase, spaces become underscores, special characters become hyphens,
// Finnish characters converted to ASCII, file extension preserved.
func SanitizeFilename(filename string) string {
	ext := filepath.Ext(filename)
	name := strings.TrimSuffix(filename, ext)

	name = strings.ToLower(name)
	ext = strings.ToLower(ext)

	name = replaceFinnishChars(name)
	name = strings.ReplaceAll(name, " ", "_")
	name = bracketsRegex.ReplaceAllString(name, "")
	name = specialCharsRegex.ReplaceAllString(name, "-")
	name = multiHyphenRegex.ReplaceAllString(name, "-")
	name = multiUnderscoreRegex.ReplaceAllString(name, "_")

	// Apply mixed separator cleanup multiple times to catch all patterns.
	for range 3 {
		name = mixedSeparatorRegex.ReplaceAllString(name, "_")
		name = multiUnderscoreRegex.ReplaceAllString(name, "_")
	}

	name = leadingRegex.ReplaceAllString(name, "")
	name = trailingRegex.ReplaceAllString(name, "")

	if name == "" {
		name = "unnamed"
	}

	return name + ext
}

// replaceFinnishChars replaces Finnish characters with ASCII equivalents.
func replaceFinnishChars(s string) string {
	var result strings.Builder
	result.Grow(len(s))

	for _, r := range s {
		if replacement, ok := finnishReplacements[r]; ok {
			result.WriteRune(replacement)
		} else {
			result.WriteRune(r)
		}
	}

	return result.String()
}

// GenerateTimestampedName creates a filename with date prefix in format YYYY-MM-DD_sanitized-name.ext.
func GenerateTimestampedName(filename string, modTime time.Time) string {
	sanitized := SanitizeFilename(filename)
	datePrefix := modTime.Format("2006-01-02")
	doublePrefix := datePrefix + "_" + datePrefix + "_"
	if strings.HasPrefix(sanitized, doublePrefix) {
		sanitized = strings.TrimPrefix(sanitized, datePrefix+"_")
	}
	if strings.HasPrefix(sanitized, datePrefix+"_") {
		return sanitized
	}
	return datePrefix + "_" + sanitized
}
