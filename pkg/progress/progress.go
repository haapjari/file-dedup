// Package progress provides reusable progress-reporting helpers.
package progress

// Emit calls cb with clamped processed/total values.
// It is a no-op when cb is nil or total is non-positive.
func Emit(cb func(processed, total int), processed, total int) {
	if cb == nil || total <= 0 {
		return
	}

	if processed < 0 {
		processed = 0
	}
	if processed > total {
		processed = total
	}

	cb(processed, total)
}

// EmitStage calls cb with a stage label and clamped processed/total values.
// It is a no-op when cb is nil or total is non-positive.
func EmitStage(cb func(stage string, processed, total int), stage string, processed, total int) {
	if cb == nil || total <= 0 {
		return
	}

	if processed < 0 {
		processed = 0
	}
	if processed > total {
		processed = total
	}

	cb(stage, processed, total)
}
