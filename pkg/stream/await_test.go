package stream

import (
	"context"
	"testing"
	"time"

	"github.com/dskiff/streamloom/pkg/clock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAwaitMSN_ImmediateReturn(t *testing.T) {
	// When lastRenderedMSN already >= requested MSN, AwaitMSN returns immediately.
	clk := clock.NewMock(time.UnixMilli(0))
	store := NewStore(clk)
	meta := Metadata{Bandwidth: 1, Codecs: "avc1.64001f", Width: 1, Height: 1, FrameRate: 30, TargetDurationSecs: 2}
	require.NoError(t, store.Init("1", meta, []byte{0x00}, 0, 10, testSegmentBytes, 1, 1, testPlaylistWindowSize))
	t.Cleanup(func() { store.Delete("1") })

	s := store.Get("1")
	require.NotNil(t, s)

	// Commit segments and advance clock so a playlist renders.
	mustCommitSlot(t, s, 0, []byte("d"), 1000, 2000)
	mustCommitSlot(t, s, 1, []byte("d"), 3000, 2000)
	mustCommitSlot(t, s, 2, []byte("d"), 5000, 2000)
	clk.Set(time.UnixMilli(6000))

	require.Eventually(t, func() bool {
		return s.LastRenderedMSN() >= 2
	}, 2*time.Second, 10*time.Millisecond)

	ctx := context.Background()
	playlist, ok := s.AwaitMSN(ctx, 2)
	assert.True(t, ok)
	assert.Contains(t, playlist, "segment_2.m4s")
}

func TestAwaitMSN_BlocksUntilSegmentAppears(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	store := NewStore(clk)
	meta := Metadata{Bandwidth: 1, Codecs: "avc1.64001f", Width: 1, Height: 1, FrameRate: 30, TargetDurationSecs: 2}
	require.NoError(t, store.Init("1", meta, []byte{0x00}, 0, 10, testSegmentBytes, 1, 1, testPlaylistWindowSize))
	t.Cleanup(func() { store.Delete("1") })

	s := store.Get("1")
	require.NotNil(t, s)

	// Commit initial segments so a playlist exists.
	mustCommitSlot(t, s, 0, []byte("d"), 1000, 2000)
	clk.Set(time.UnixMilli(2000))

	require.Eventually(t, func() bool {
		return s.LastRenderedMSN() >= 0
	}, 2*time.Second, 10*time.Millisecond)

	// AwaitMSN(3) should block because lastRenderedMSN == 0.
	ctx := context.Background()
	done := make(chan struct{})
	var playlist string
	var ok bool
	go func() {
		defer close(done)
		playlist, ok = s.AwaitMSN(ctx, 3)
	}()

	// Verify it hasn't returned yet.
	select {
	case <-done:
		t.Fatal("AwaitMSN returned before segment 3 was committed")
	case <-time.After(50 * time.Millisecond):
	}

	// Now commit segments up to 3 and advance clock.
	mustCommitSlot(t, s, 1, []byte("d"), 3000, 2000)
	mustCommitSlot(t, s, 2, []byte("d"), 5000, 2000)
	mustCommitSlot(t, s, 3, []byte("d"), 7000, 2000)
	clk.Set(time.UnixMilli(8000))

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("AwaitMSN did not return after segment 3 appeared")
	}

	assert.True(t, ok)
	assert.Contains(t, playlist, "segment_3.m4s")
}

func TestAwaitMSN_StreamDeletion(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	store := NewStore(clk)
	meta := Metadata{Bandwidth: 1, Codecs: "avc1.64001f", Width: 1, Height: 1, FrameRate: 30, TargetDurationSecs: 2}
	require.NoError(t, store.Init("1", meta, []byte{0x00}, 0, 10, testSegmentBytes, 1, 1, testPlaylistWindowSize))

	s := store.Get("1")
	require.NotNil(t, s)

	// AwaitMSN(99) — no segments committed, will block.
	ctx := context.Background()
	done := make(chan struct{})
	var ok bool
	go func() {
		defer close(done)
		_, ok = s.AwaitMSN(ctx, 99)
	}()

	// Delete the stream — should unblock AwaitMSN.
	time.Sleep(50 * time.Millisecond)
	store.Delete("1")

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("AwaitMSN did not return after stream deletion")
	}

	assert.False(t, ok)
}

func TestAwaitMSN_ContextCancellation(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	store := NewStore(clk)
	meta := Metadata{Bandwidth: 1, Codecs: "avc1.64001f", Width: 1, Height: 1, FrameRate: 30, TargetDurationSecs: 2}
	require.NoError(t, store.Init("1", meta, []byte{0x00}, 0, 10, testSegmentBytes, 1, 1, testPlaylistWindowSize))
	t.Cleanup(func() { store.Delete("1") })

	s := store.Get("1")
	require.NotNil(t, s)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var ok bool
	go func() {
		defer close(done)
		_, ok = s.AwaitMSN(ctx, 99)
	}()

	// Cancel — should unblock AwaitMSN promptly.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("AwaitMSN did not return after context cancellation")
	}

	assert.False(t, ok)
}
