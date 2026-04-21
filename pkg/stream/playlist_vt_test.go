package stream

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dskiff/streamloom/pkg/clock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupStreamWithMintToken initializes a stream whose renderer uses the given
// mint function and returns the Stream. Cleanup of the underlying Store is
// registered via t.Cleanup so the renderer goroutine is stopped at test end.
func setupStreamWithMintToken(t *testing.T, mintFn func() string) *Stream {
	t.Helper()
	clk := clock.NewMock(time.UnixMilli(0))
	store := NewStore(clk)
	meta := Metadata{
		Bandwidth:          4000000,
		Codecs:             "avc1.64001f",
		Width:              1920,
		Height:             1080,
		FrameRate:          23.976,
		TargetDurationSecs: 2,
	}
	err := store.Init("1", meta, []byte("init"), 50, testSegmentBytes, 20, 5, 12, testMaxLookaheadMs,
		WithMintToken(mintFn))
	require.NoError(t, err)
	s := store.Get("1")
	require.NotNil(t, s)
	t.Cleanup(func() { store.Delete("1") })
	return s
}

// TestRenderMediaPlaylist_NoMintToken_PlainURIs asserts that a stream with no
// mint callback renders URIs without any ?vt= query (public-playback parity).
func TestRenderMediaPlaylist_NoMintToken_PlainURIs(t *testing.T) {
	_, s := setupStreamForPlaylist(t, 2)
	for i := range uint32(3) {
		mustCommitSlot(t, s, i, []byte("data"), int64(i)*2000, 2000)
	}
	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(20000, 12)
	s.mu.RUnlock()

	assert.Contains(t, playlist, "#EXT-X-MAP:URI=\"init.mp4\"")
	assert.NotContains(t, playlist, "?vt=")
	for i := range 3 {
		assert.Contains(t, playlist, fmt.Sprintf("segment_%d.m4s\n", i),
			"segment URIs must be bare when no mintToken is configured")
	}
}

// TestRenderMediaPlaylist_WithMintToken_BakesTokenAtEveryURI asserts the mint
// callback fires once per render and the token is baked into EXT-X-MAP and
// every segment URI.
func TestRenderMediaPlaylist_WithMintToken_BakesTokenAtEveryURI(t *testing.T) {
	const token = "TEST_TOKEN_VALUE"
	var mintCalls atomic.Int32
	s := setupStreamWithMintToken(t, func() string {
		mintCalls.Add(1)
		return token
	})

	for i := range uint32(3) {
		mustCommitSlot(t, s, i, []byte("data"), int64(i)*2000, 2000)
	}

	mintCalls.Store(0)
	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(20000, 12)
	s.mu.RUnlock()

	assert.EqualValues(t, 1, mintCalls.Load(), "mint callback must fire exactly once per render")

	// One vt in EXT-X-MAP and one per segment.
	assert.Equal(t, 1+3, strings.Count(playlist, "?vt="+token))
	assert.Contains(t, playlist, "#EXT-X-MAP:URI=\"init.mp4?vt="+token+"\"")
	for i := range 3 {
		assert.Contains(t, playlist, fmt.Sprintf("segment_%d.m4s?vt=%s\n", i, token))
	}
}

// TestRenderMediaPlaylist_EmptyMintReturn_PlainURIs asserts that a mint
// callback returning "" suppresses the ?vt= query for that render. The
// middleware still enforces vt on the resulting request, so this degrades
// to 401 (fail-closed) rather than leaking unauthorized access.
func TestRenderMediaPlaylist_EmptyMintReturn_PlainURIs(t *testing.T) {
	s := setupStreamWithMintToken(t, func() string { return "" })

	for i := range uint32(2) {
		mustCommitSlot(t, s, i, []byte("data"), int64(i)*2000, 2000)
	}

	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(20000, 12)
	s.mu.RUnlock()

	assert.NotContains(t, playlist, "?vt=")
	assert.Contains(t, playlist, "#EXT-X-MAP:URI=\"init.mp4\"")
}

// TestRenderMediaPlaylist_MintRefreshesOnEveryRender asserts that the same
// Stream instance observes different mint values across consecutive renders,
// confirming the renderer does not cache the token.
func TestRenderMediaPlaylist_MintRefreshesOnEveryRender(t *testing.T) {
	var counter atomic.Int32
	s := setupStreamWithMintToken(t, func() string {
		return fmt.Sprintf("tok%d", counter.Add(1))
	})
	mustCommitSlot(t, s, 0, []byte("data"), 0, 2000)

	counter.Store(0)
	s.mu.RLock()
	first, _ := s.renderMediaPlaylist(20000, 12)
	s.mu.RUnlock()
	s.mu.RLock()
	second, _ := s.renderMediaPlaylist(20000, 12)
	s.mu.RUnlock()

	assert.Contains(t, first, "?vt=tok1")
	assert.Contains(t, second, "?vt=tok2")
	assert.NotEqual(t, first, second, "consecutive renders must embed distinct tokens")
}
