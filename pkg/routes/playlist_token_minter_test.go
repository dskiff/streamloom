package routes

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dskiff/streamloom/pkg/viewer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testMinter returns a playlistTokenMinter wired with a real segment-derived
// key and a test logger so the assertions exercise the same code path as the
// production wiring.
func testMinter(t *testing.T, streamID string) (*playlistTokenMinter, []byte) {
	t.Helper()
	segKey, err := viewer.DeriveKey([]byte(strings.Repeat("k", 32)), streamID, viewer.TypeSegment)
	require.NoError(t, err)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	return makePlaylistTokenMinter(segKey, logger, streamID), segKey
}

// TestPlaylistTokenMinter_SegmentToken_Deterministic asserts the production
// minter returns the same token for the same timestamp. This is the crux of
// the URI-stability contract — a regression here would reintroduce the
// HLS client-misbehavior bug even if the renderer is unchanged.
func TestPlaylistTokenMinter_SegmentToken_Deterministic(t *testing.T) {
	m, _ := testMinter(t, "1")
	a := m.SegmentToken(5000)
	b := m.SegmentToken(5000)
	require.NotEmpty(t, a)
	assert.Equal(t, a, b, "same segmentTimestampMs must yield the same token")
}

// TestPlaylistTokenMinter_SegmentToken_DiffersByTimestamp asserts distinct
// segment timestamps that fall into different minute buckets produce
// distinct tokens. (Tokens are minute-truncated internally; two timestamps
// in the same minute bucket may collide — that's fine, and tested below.)
func TestPlaylistTokenMinter_SegmentToken_DiffersByTimestamp(t *testing.T) {
	m, _ := testMinter(t, "1")
	// Inputs spaced by PlaylistTokenTTL guarantee different exp-minute
	// encodings regardless of minute truncation.
	a := m.SegmentToken(0)
	b := m.SegmentToken(int64(PlaylistTokenTTL / time.Millisecond))
	require.NotEmpty(t, a)
	require.NotEmpty(t, b)
	assert.NotEqual(t, a, b,
		"distinct timestamps mapping to distinct minute expiries must produce distinct tokens")
}

// TestPlaylistTokenMinter_SegmentToken_SameMinuteBucketCollides documents
// that the minute-granularity encoding intentionally collapses tokens
// within the same exp-minute bucket. This keeps scraped-URL reuse bounded
// by the minute resolution rather than by ms drift.
func TestPlaylistTokenMinter_SegmentToken_SameMinuteBucketCollides(t *testing.T) {
	m, _ := testMinter(t, "1")
	// Two timestamps whose (ts + TTL) / 60_000 rounds to the same minute.
	a := m.SegmentToken(5000)
	b := m.SegmentToken(5001)
	assert.Equal(t, a, b,
		"timestamps within the same exp-minute bucket produce identical tokens (by design)")
}

// TestPlaylistTokenMinter_SegmentToken_VerifiesUnderSegmentKey asserts
// the baked token can be verified with the same segment-derived key —
// i.e. the minter and middleware agree on the signing key.
func TestPlaylistTokenMinter_SegmentToken_VerifiesUnderSegmentKey(t *testing.T) {
	m, segKey := testMinter(t, "1")
	tsMs := int64(5000)
	tok := m.SegmentToken(tsMs)
	require.NotEmpty(t, tok)
	// "now" chosen before exp (= ts + TTL).
	now := time.UnixMilli(tsMs)
	assert.NoError(t, viewer.Verify(segKey, now, tok))
	// After exp, the token must be rejected.
	afterExp := time.UnixMilli(tsMs + int64(PlaylistTokenTTL/time.Millisecond) + 2*60_000)
	assert.ErrorIs(t, viewer.Verify(segKey, afterExp, tok), viewer.ErrExpired)
}

// TestPlaylistTokenMinter_InitToken_HourBucketed asserts all nowMs values
// within the same hour bucket produce the same token and that crossing
// the bucket boundary changes the token.
func TestPlaylistTokenMinter_InitToken_HourBucketed(t *testing.T) {
	m, _ := testMinter(t, "1")
	hour := int64(time.Hour / time.Millisecond)

	// Three nowMs values in the same hour bucket (0..hour-1).
	a := m.InitToken(0)
	b := m.InitToken(hour / 2)
	c := m.InitToken(hour - 1)
	require.NotEmpty(t, a)
	assert.Equal(t, a, b, "early/mid nowMs in same bucket must match")
	assert.Equal(t, a, c, "early/late nowMs in same bucket must match")

	// Cross the boundary — token MUST change.
	d := m.InitToken(hour)
	assert.NotEqual(t, a, d, "nowMs in next bucket must produce a new token")
}

// TestPlaylistTokenMinter_InitToken_ExpIsBucketEndPlusTTL asserts the
// init token's expiry is (bucketStart + bucketMs + TTL), minute-aligned.
// This is the arithmetic most likely to have an off-by-one — pin it.
func TestPlaylistTokenMinter_InitToken_ExpIsBucketEndPlusTTL(t *testing.T) {
	m, segKey := testMinter(t, "1")
	hour := int64(time.Hour / time.Millisecond)
	ttl := int64(PlaylistTokenTTL / time.Millisecond)

	tok := m.InitToken(hour / 2) // inside bucket 0
	require.NotEmpty(t, tok)

	// Should be valid just before (bucket_end + TTL) and expired at/after.
	validAt := time.UnixMilli(hour + ttl - 60_000) // one minute before exp
	expiredAt := time.UnixMilli(hour + ttl)        // exp boundary (>=, rejected)
	assert.NoError(t, viewer.Verify(segKey, validAt, tok))
	assert.ErrorIs(t, viewer.Verify(segKey, expiredAt, tok), viewer.ErrExpired)
}

// TestPlaylistTokenMinter_VerifiesAsSegmentClass asserts the tokens
// returned are the SEGMENT class (fail MAC under the playlist-derived
// key). This is a regression-test for the infinite-rotation defense.
func TestPlaylistTokenMinter_VerifiesAsSegmentClass(t *testing.T) {
	m, _ := testMinter(t, "1")
	plKey, err := viewer.DeriveKey([]byte(strings.Repeat("k", 32)), "1", viewer.TypePlaylist)
	require.NoError(t, err)

	seg := m.SegmentToken(5000)
	init := m.InitToken(0)
	require.NotEmpty(t, seg)
	require.NotEmpty(t, init)

	now := time.UnixMilli(5000)
	assert.ErrorIs(t, viewer.Verify(plKey, now, seg), viewer.ErrBadMAC,
		"segment tokens must NOT verify under the playlist-derived key")
	assert.ErrorIs(t, viewer.Verify(plKey, now, init), viewer.ErrBadMAC,
		"init tokens must NOT verify under the playlist-derived key")
}

// TestPlaylistTokenMinter_EmptyKey_ReturnsEmpty asserts that a minter
// built with an empty key returns "" (fail-closed) rather than panicking
// or returning a garbage string. The renderer then emits a bare URI and
// the middleware 401s the fetch.
func TestPlaylistTokenMinter_EmptyKey_ReturnsEmpty(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	m := makePlaylistTokenMinter(nil, logger, "1")
	assert.Equal(t, "", m.SegmentToken(5000))
	assert.Equal(t, "", m.InitToken(0))
	assert.Contains(t, buf.String(), "failed to mint",
		"mint failures must be logged")
}

// TestPlaylistTokenMinter_ConcurrentCallsSafe asserts the production
// minter is safe for concurrent use, matching the PlaylistTokenMinter
// interface contract (see pkg/stream docs). Run under -race to catch
// data races; the determinism assertion doubles as a smoke test.
func TestPlaylistTokenMinter_ConcurrentCallsSafe(t *testing.T) {
	m, _ := testMinter(t, "1")
	expectedSeg := m.SegmentToken(5000)
	expectedInit := m.InitToken(0)
	require.NotEmpty(t, expectedSeg)
	require.NotEmpty(t, expectedInit)

	const goroutines = 32
	const itersPerGoroutine = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range itersPerGoroutine {
				assert.Equal(t, expectedSeg, m.SegmentToken(5000))
				assert.Equal(t, expectedInit, m.InitToken(0))
			}
		}()
	}
	wg.Wait()
}
