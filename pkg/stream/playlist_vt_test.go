package stream

import (
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dskiff/streamloom/pkg/clock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeMinter is a test stub implementing PlaylistTokenMinter. It records
// the arguments it was called with and returns tokens from caller-supplied
// functions so each test can target its own behavior.
type fakeMinter struct {
	segmentFn func(ts int64) string
	initFn    func(now int64) string

	segmentCalls atomic.Int32
	initCalls    atomic.Int32

	segmentTimestamps []int64
	initNows          []int64
}

func (f *fakeMinter) SegmentToken(ts int64) string {
	f.segmentCalls.Add(1)
	f.segmentTimestamps = append(f.segmentTimestamps, ts)
	if f.segmentFn == nil {
		return ""
	}
	return f.segmentFn(ts)
}

func (f *fakeMinter) InitToken(now int64) string {
	f.initCalls.Add(1)
	f.initNows = append(f.initNows, now)
	if f.initFn == nil {
		return ""
	}
	return f.initFn(now)
}

// setupStreamWithMintToken initializes a stream whose renderer uses the given
// PlaylistTokenMinter and returns the Stream. Cleanup of the underlying Store
// is registered via t.Cleanup so the renderer goroutine is stopped at test end.
func setupStreamWithMintToken(t *testing.T, m PlaylistTokenMinter) *Stream {
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
	err := store.Init("1", meta, []byte("init"), 50, testSegmentBytes, 20, 5, 12,
		WithMintToken(m))
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

// TestRenderMediaPlaylist_WithMintToken_BakesTokenAtEveryURI asserts the
// minter is invoked once per emitted URI (one InitToken, one SegmentToken
// per segment in the window) and the returned tokens are baked into the
// corresponding URIs.
func TestRenderMediaPlaylist_WithMintToken_BakesTokenAtEveryURI(t *testing.T) {
	m := &fakeMinter{
		segmentFn: func(ts int64) string { return fmt.Sprintf("SEG_%d", ts) },
		initFn:    func(now int64) string { return fmt.Sprintf("INIT_%d", now) },
	}
	s := setupStreamWithMintToken(t, m)

	for i := range uint32(3) {
		mustCommitSlot(t, s, i, []byte("data"), int64(i)*2000, 2000)
	}

	// Reset counters and any calls accumulated during the renderer-goroutine
	// auto-renders that fire as segments commit.
	m.segmentCalls.Store(0)
	m.initCalls.Store(0)
	m.segmentTimestamps = nil
	m.initNows = nil

	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(20000, 12)
	s.mu.RUnlock()

	assert.EqualValues(t, 1, m.initCalls.Load(), "InitToken must fire once per render")
	assert.EqualValues(t, 3, m.segmentCalls.Load(), "SegmentToken must fire once per segment in the window")
	assert.Equal(t, []int64{0, 2000, 4000}, m.segmentTimestamps,
		"SegmentToken must receive the segment's timestamp")
	assert.Equal(t, []int64{20000}, m.initNows, "InitToken must receive the render's nowMs")

	assert.Contains(t, playlist, `#EXT-X-MAP:URI="init.mp4?vt=INIT_20000"`)
	for i := range 3 {
		assert.Contains(t, playlist,
			fmt.Sprintf("segment_%d.m4s?vt=SEG_%d\n", i, i*2000))
	}
}

// TestRenderMediaPlaylist_EmptyMintReturn_PlainURIs asserts that a minter
// returning "" suppresses the ?vt= query per-URI. The middleware still
// enforces vt on the resulting requests, so this degrades to 401
// (fail-closed) rather than leaking unauthorized access.
func TestRenderMediaPlaylist_EmptyMintReturn_PlainURIs(t *testing.T) {
	s := setupStreamWithMintToken(t, &fakeMinter{
		segmentFn: func(int64) string { return "" },
		initFn:    func(int64) string { return "" },
	})

	for i := range uint32(2) {
		mustCommitSlot(t, s, i, []byte("data"), int64(i)*2000, 2000)
	}

	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(20000, 12)
	s.mu.RUnlock()

	assert.NotContains(t, playlist, "?vt=")
	assert.Contains(t, playlist, "#EXT-X-MAP:URI=\"init.mp4\"")
}

// TestRenderMediaPlaylist_StableSegmentURIsAcrossRenders is the direct
// regression test for the HLS client-misbehavior bug: a segment whose
// contents have not changed must keep the same URI (including ?vt=)
// across playlist renders. Without this guarantee, clients that track
// segments by URI re-download them on every reload.
//
// The minter used here returns a token that is a pure function of the
// segment's timestamp — mirroring the production implementation's
// deterministic derivation.
func TestRenderMediaPlaylist_StableSegmentURIsAcrossRenders(t *testing.T) {
	m := &fakeMinter{
		// Deterministic per-timestamp — same input, same output.
		segmentFn: func(ts int64) string { return fmt.Sprintf("tok_seg_%d", ts) },
		initFn:    func(int64) string { return "tok_init" },
	}
	s := setupStreamWithMintToken(t, m)

	mustCommitSlot(t, s, 0, []byte("data"), 0, 2000)
	mustCommitSlot(t, s, 1, []byte("data"), 2000, 2000)

	s.mu.RLock()
	first, _ := s.renderMediaPlaylist(20000, 12)
	s.mu.RUnlock()

	// Commit an additional segment between renders — this is what happens
	// in production on every segment commit.
	mustCommitSlot(t, s, 2, []byte("data"), 4000, 2000)

	s.mu.RLock()
	second, _ := s.renderMediaPlaylist(20000, 12)
	s.mu.RUnlock()

	// Each segment URI present in both renders must be byte-identical.
	seg0Re := regexp.MustCompile(`segment_0\.m4s\?vt=[A-Za-z0-9_-]+`)
	seg1Re := regexp.MustCompile(`segment_1\.m4s\?vt=[A-Za-z0-9_-]+`)
	assert.Equal(t, seg0Re.FindString(first), seg0Re.FindString(second),
		"segment_0 URI must be byte-identical across renders")
	assert.Equal(t, seg1Re.FindString(first), seg1Re.FindString(second),
		"segment_1 URI must be byte-identical across renders")

	// The new segment is only present in the second render.
	assert.NotContains(t, first, "segment_2.m4s")
	assert.Contains(t, second, "segment_2.m4s?vt=tok_seg_4000")
}

// TestRenderMediaPlaylist_InitTokenRefreshesOnEveryRender confirms that
// InitToken is called on each render with the current nowMs so the minter
// can rotate the init URI at its own cadence (e.g. hourly buckets). The
// renderer itself does not cache or reuse the init token between calls.
func TestRenderMediaPlaylist_InitTokenRefreshesOnEveryRender(t *testing.T) {
	m := &fakeMinter{
		segmentFn: func(ts int64) string { return fmt.Sprintf("seg%d", ts) },
		initFn:    func(now int64) string { return fmt.Sprintf("init_at_%d", now) },
	}
	s := setupStreamWithMintToken(t, m)
	mustCommitSlot(t, s, 0, []byte("data"), 0, 2000)

	s.mu.RLock()
	first, _ := s.renderMediaPlaylist(20000, 12)
	s.mu.RUnlock()
	s.mu.RLock()
	second, _ := s.renderMediaPlaylist(25000, 12)
	s.mu.RUnlock()

	assert.Contains(t, first, `#EXT-X-MAP:URI="init.mp4?vt=init_at_20000"`)
	assert.Contains(t, second, `#EXT-X-MAP:URI="init.mp4?vt=init_at_25000"`)
	// Segment URI stays stable — only the init URI reflects the new nowMs.
	seg0 := regexp.MustCompile(`segment_0\.m4s\?vt=[A-Za-z0-9_-]+`)
	assert.Equal(t, seg0.FindString(first), seg0.FindString(second))
	assert.True(t, strings.Contains(first, "segment_0.m4s?vt=seg0"))
}
