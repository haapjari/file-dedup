package sanitizer_test

import (
	"testing"

	"file-dedup/pkg/sanitizer"

	"github.com/stretchr/testify/assert"
)

func TestResolveNameConflict_ZeroCount(t *testing.T) {
	assert.Equal(t, "photo.jpg", sanitizer.ResolveNameConflict("photo.jpg", 0))
}

func TestResolveNameConflict_FirstConflict(t *testing.T) {
	assert.Equal(t, "photo_1.jpg", sanitizer.ResolveNameConflict("photo.jpg", 1))
}

func TestResolveNameConflict_HigherCount(t *testing.T) {
	assert.Equal(t, "photo_5.jpg", sanitizer.ResolveNameConflict("photo.jpg", 5))
}

func TestResolveNameConflict_NoExtension(t *testing.T) {
	assert.Equal(t, "readme_1", sanitizer.ResolveNameConflict("readme", 1))
}

func TestResolveNameConflict_MultipleExtensionDots(t *testing.T) {
	// filepath.Ext returns ".gz" for "archive.tar.gz"
	assert.Equal(t, "archive.tar_2.gz", sanitizer.ResolveNameConflict("archive.tar.gz", 2))
}

func TestResolveNameConflict_DotFile(t *testing.T) {
	assert.Equal(t, "_1.gitignore", sanitizer.ResolveNameConflict(".gitignore", 1))
}
