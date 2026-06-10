package routes

import (
	"log/slog"
	"time"

	"github.com/dskiff/streamloom/pkg/viewer"
)

// This file holds the renderer-side token minter: the component that bakes
// short-lived segment-class viewer tokens into the init/segment URIs emitted
// by the media-playlist renderer. Token minting is security-sensitive, so it
// lives apart from the request-routing glue in its own file (and is covered
// by playlist_token_minter_test.go). The operator-facing mint endpoint lives in
// viewer_token.go.

// PlaylistTokenTTL is the lifetime applied to tokens the renderer bakes
// into init/segment URIs. For segments it is a grace period past the
// segment's own presentation timestamp (`exp = seg.Timestamp + TTL`); for
// the init URI it is a grace period past the end of the current hour
// bucket (`exp = bucketEnd + TTL`). Using intrinsic anchors (segment
// timestamp, hour boundary) rather than the render wall clock keeps a
// given URI byte-identical across re-renders, which HLS clients require
// for correct dedup behavior (RFC 8216 §6.2.2). The TTL itself bounds
// how long a scraped URL can be replayed: ~10 minutes past the segment
// timestamp for segments, and up to one hour + TTL for init. Comfortably
// above MinViewerTokenTTLMs so the token always passes the public
// mint-endpoint floor.
const PlaylistTokenTTL = 10 * time.Minute

// playlistTokenMinter implements stream.PlaylistTokenMinter. It produces
// segment-class viewer tokens that the media-playlist renderer bakes into
// emitted URIs.
//
// The segment-derived key binds these tokens to the segment capability
// class via the KDF, so a refetch of the media playlist carrying one of
// these tokens fails MAC under the playlist-derived key (tried alone on
// playlist routes) and is rejected. This preserves the infinite-rotation
// defense without spending a payload byte on a type marker.
//
// Per-URI minting (one call per segment) with a deterministic expiry
// derived from the segment's own timestamp guarantees that a given
// segment's URL is byte-identical across playlist renders, satisfying
// the URI-stability expectation of HLS clients (RFC 8216 §6.2.2). The
// init URI's expiry is bucketed to the hour so EXT-X-MAP is stable for
// ~1 h at a time rather than flipping on every render.
type playlistTokenMinter struct {
	segmentKey []byte
	logger     *slog.Logger
	streamID   string
}

// initTokenBucketMs is the quantum the init-segment token's expiry is
// bucketed to. One hour matches the init segment's role as a
// stream-lifetime artifact (rather than a per-segment one). Applies only
// to the init URI; segment URIs use a tighter per-segment anchor
// (seg.Timestamp + PlaylistTokenTTL). The init-URI scraping replay bound
// is therefore up to 1h + PlaylistTokenTTL, strictly wider than the
// segment bound.
const initTokenBucketMs = int64(time.Hour / time.Millisecond)

// makePlaylistTokenMinter returns a PlaylistTokenMinter bound to the given
// per-stream segment-derived signing key. It returns "" from either mint
// method on failure so the renderer emits that single URI plain; the
// middleware then 401s the fetch (fail-closed).
func makePlaylistTokenMinter(segmentKey []byte, logger *slog.Logger, streamID string) *playlistTokenMinter {
	return &playlistTokenMinter{
		segmentKey: segmentKey,
		logger:     logger,
		streamID:   streamID,
	}
}

// SegmentToken mints a token whose expiry is anchored to the segment's own
// presentation timestamp. Two renders that both include the same segment
// therefore bake the same token string into that segment's URI, keeping
// the URL stable for the life of the segment in the window.
//
// Note: for the first segment of an empty stream, the commit logic exempts
// the "timestamp must be >= now" check (see CommitSlot in pkg/stream). If
// an operator commits a first segment whose timestamp is older than
// now - PlaylistTokenTTL, the baked token's expiry (ts + TTL) is already
// in the past and viewer.Verify immediately returns ErrExpired. This
// mirrors the stream's own "stale content" posture: such a segment is
// unlikely to be a useful live asset anyway. Subsequent commits are
// required to have timestamps >= the store clock, so the issue is
// confined to the exempt first segment.
func (m *playlistTokenMinter) SegmentToken(segmentTimestampMs int64) string {
	expMs := segmentTimestampMs + PlaylistTokenTTL.Milliseconds()
	tok, err := viewer.Mint(m.segmentKey, expMs)
	if err != nil {
		m.logger.Error("failed to mint segment viewer token",
			"streamID", m.streamID,
			"error", err,
		)
		return ""
	}
	return tok
}

// InitToken mints a token whose expiry is bucketed to the current hour.
// All renders within the same hour produce the same token, so EXT-X-MAP's
// URI does not churn every render. The expiry is set one TTL past the
// bucket's end so a client that loads the playlist near the boundary
// still has a valid init URI.
func (m *playlistTokenMinter) InitToken(nowMs int64) string {
	bucketStart := (nowMs / initTokenBucketMs) * initTokenBucketMs
	expMs := bucketStart + initTokenBucketMs + PlaylistTokenTTL.Milliseconds()
	tok, err := viewer.Mint(m.segmentKey, expMs)
	if err != nil {
		m.logger.Error("failed to mint init viewer token",
			"streamID", m.streamID,
			"error", err,
		)
		return ""
	}
	return tok
}
