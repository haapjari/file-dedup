package progress_test

import (
	"testing"

	"file-dedup/pkg/progress"

	"github.com/stretchr/testify/assert"
)

func TestEmit_NilCallback(_ *testing.T) {
	// Should not panic.
	progress.Emit(nil, 1, 10)
}

func TestEmit_ZeroTotal(t *testing.T) {
	called := false
	progress.Emit(func(_, _ int) { called = true }, 1, 0)
	assert.False(t, called)
}

func TestEmit_NegativeTotal(t *testing.T) {
	called := false
	progress.Emit(func(_, _ int) { called = true }, 1, -1)
	assert.False(t, called)
}

func TestEmit_ClampsNegativeProcessed(t *testing.T) {
	var got int
	progress.Emit(func(processed, _ int) { got = processed }, -5, 10)
	assert.Equal(t, 0, got)
}

func TestEmit_ClampsOverflowProcessed(t *testing.T) {
	var got int
	progress.Emit(func(processed, _ int) { got = processed }, 15, 10)
	assert.Equal(t, 10, got)
}

func TestEmit_PassesThrough(t *testing.T) {
	var gotP, gotT int
	progress.Emit(func(processed, total int) {
		gotP = processed
		gotT = total
	}, 5, 10)
	assert.Equal(t, 5, gotP)
	assert.Equal(t, 10, gotT)
}

func TestEmitStage_NilCallback(_ *testing.T) {
	// Should not panic.
	progress.EmitStage(nil, "test", 1, 10)
}

func TestEmitStage_ZeroTotal(t *testing.T) {
	called := false
	progress.EmitStage(func(_ string, _, _ int) { called = true }, "test", 1, 0)
	assert.False(t, called)
}

func TestEmitStage_ClampsNegativeProcessed(t *testing.T) {
	var got int
	progress.EmitStage(func(_ string, processed, _ int) { got = processed }, "test", -5, 10)
	assert.Equal(t, 0, got)
}

func TestEmitStage_ClampsOverflowProcessed(t *testing.T) {
	var got int
	progress.EmitStage(func(_ string, processed, _ int) { got = processed }, "test", 15, 10)
	assert.Equal(t, 10, got)
}

func TestEmitStage_PassesThrough(t *testing.T) {
	var gotStage string
	var gotP, gotT int
	progress.EmitStage(func(stage string, processed, total int) {
		gotStage = stage
		gotP = processed
		gotT = total
	}, "hashing", 5, 10)
	assert.Equal(t, "hashing", gotStage)
	assert.Equal(t, 5, gotP)
	assert.Equal(t, 10, gotT)
}
