package sanitizer

import (
	"fmt"
	"path/filepath"
)

// ResolveNameConflict returns name unchanged when usageCount is 0.
// For usageCount > 0 it inserts "_N" before the file extension,
// e.g. "photo.jpg" with usageCount 2 becomes "photo_2.jpg".
// Callers are responsible for tracking and incrementing their own count maps.
func ResolveNameConflict(name string, usageCount int) string {
	if usageCount == 0 {
		return name
	}

	ext := filepath.Ext(name)
	base := name[:len(name)-len(ext)]

	return fmt.Sprintf("%s_%d%s", base, usageCount, ext)
}
