package safepath

import (
	"file-dedup/pkg/collector"
)

// ValidateReadPaths checks every file path for read safety and partitions the
// input into safe files and invalid operations. The makeErrorOp factory lets
// each caller construct its own package-specific error type.
func ValidateReadPaths[T any](
	v *Validator,
	files []collector.FileInfo,
	makeErrorOp func(file collector.FileInfo, err error) T,
) (safe []collector.FileInfo, invalid []T) {
	safe = make([]collector.FileInfo, 0, len(files))
	invalid = make([]T, 0)

	for _, file := range files {
		if err := v.ValidatePathForRead(file.Path); err != nil {
			invalid = append(invalid, makeErrorOp(file, err))
			continue
		}

		safe = append(safe, file)
	}

	return safe, invalid
}
