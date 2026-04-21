package stream

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dskiff/streamloom/pkg/clock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeMinter is a test stub implementing PlaylistTokenMinter. Its methods
// delegate to caller-supplied pure functions, so a single fakeMinter is
// safe to call concurrently (the renderer goroutine may invoke it in
// parallel with the test's direct renderMediaPlaylist call). Tests assert
// behavior by inspecting the returned playlist string rather than
// recording calls — the playlist is the test's own local value and is
// unaffected by any background renders.
type fakeMinter struct {
	segmentFn func(ts int64) string
	initFn    func(now int64) string
}

func (f *fakeMinter) SegmentToken(ts int64) string {
	if f.segmentFn == nil {
		return ""
	}
	return f.segmentFn(ts)
}

func (f *fakeMinter) InitToken(now int64) string {
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

// TestRenderMediaPlaylist_WithMintToken_BakesTokenAtEveryURI asserts that
// the renderer calls InitToken with the render's nowMs and SegmentToken
// with each segment's timestamp, and bakes the returned tokens into the
// corresponding URIs. Behavior is verified purely from the playlist
// string so the assertions are unaffected by any concurrent background
// renders the renderer goroutine may perform.
func TestRenderMediaPlaylist_WithMintToken_BakesTokenAtEveryURI(t *testing.T) {
	m := &fakeMinter{
		segmentFn: func(ts int64) string { return fmt.Sprintf("SEG_%d", ts) },
		initFn:    func(now int64) string { return fmt.Sprintf("INIT_%d", now) },
	}
	s := setupStreamWithMintToken(t, m)

	for i := range uint32(3) {
		mustCommitSlot(t, s, i, []byte("data"), int64(i)*2000, 2000)
	}

	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(20000, 12)
	s.mu.RUnlock()

	// InitToken received the render's nowMs.
	assert.Contains(t, playlist, `#EXT-X-MAP:URI="init.mp4?vt=INIT_20000"`)
	// SegmentToken received each segment's own timestamp.
	for i := range 3 {
		assert.Contains(t, playlist,
			fmt.Sprintf("segment_%d.m4s?vt=SEG_%d\n", i, i*2000))
	}
	// One init URI and one per segment — no duplicate emissions.
	assert.Equal(t, 1, strings.Count(playlist, "init.mp4?vt="))
	assert.Equal(t, 3, strings.Count(playlist, ".m4s?vt="))
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

// TestRenderMediaPlaylist_InitTokenReceivesRenderNowMs confirms that
// InitToken is invoked with the nowMs of its render — this is what lets a
// production minter rotate the init URI at its own cadence (e.g. hourly
// buckets) while keeping segment URIs stable. Asserted via the playlist
// string so the check is unaffected by background renders.
func TestRenderMediaPlaylist_InitTokenReceivesRenderNowMs(t *testing.T) {
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
	assert.Contains(t, first, "segment_0.m4s?vt=seg0")
}

// TestRenderMediaPlaylist_PartialEmptyMintReturn_BareOnlyAffectedURI asserts
// that each URI's ?vt= is suppressed independently: when InitToken returns
// "" but SegmentToken returns real tokens (or vice versa), only the
// affected URI becomes bare. This is the contract that keeps per-URI
// mint failures from cascading into whole-render failures.
func TestRenderMediaPlaylist_PartialEmptyMintReturn_BareOnlyAffectedURI(t *testing.T) {
	// InitToken empty; SegmentToken returns a real value.
	s := setupStreamWithMintToken(t, &fakeMinter{
		segmentFn: func(ts int64) string { return fmt.Sprintf("tok%d", ts) },
		initFn:    func(int64) string { return "" },
	})
	for i := range uint32(2) {
		mustCommitSlot(t, s, i, []byte("data"), int64(i)*2000, 2000)
	}
	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(20000, 12)
	s.mu.RUnlock()

	assert.Contains(t, playlist, "#EXT-X-MAP:URI=\"init.mp4\"",
		"bare init URI when InitToken returns empty")
	for i := range 2 {
		assert.Contains(t, playlist,
			fmt.Sprintf("segment_%d.m4s?vt=tok%d\n", i, i*2000),
			"segment URIs still carry ?vt= when SegmentToken returns non-empty")
	}

	// Inverse: InitToken returns a real value; SegmentToken returns "".
	s2 := setupStreamWithMintToken(t, &fakeMinter{
		segmentFn: func(int64) string { return "" },
		initFn:    func(int64) string { return "INIT_OK" },
	})
	for i := range uint32(2) {
		mustCommitSlot(t, s2, i, []byte("data"), int64(i)*2000, 2000)
	}
	s2.mu.RLock()
	playlist2, _ := s2.renderMediaPlaylist(20000, 12)
	s2.mu.RUnlock()

	assert.Contains(t, playlist2, "#EXT-X-MAP:URI=\"init.mp4?vt=INIT_OK\"",
		"init URI carries ?vt= when InitToken returns non-empty")
	for i := range 2 {
		assert.Contains(t, playlist2, fmt.Sprintf("segment_%d.m4s\n", i),
			"bare segment URIs when SegmentToken returns empty")
	}
}
