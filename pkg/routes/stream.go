package routes

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/dskiff/streamloom/pkg/config"
	mw "github.com/dskiff/streamloom/pkg/middleware"
	"github.com/dskiff/streamloom/pkg/pool"
	"github.com/dskiff/streamloom/pkg/stream"
	"github.com/dskiff/streamloom/pkg/viewer"
	"github.com/dskiff/streamloom/pkg/watcher"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// writeStreamUnavailable sends a 503 with Retry-After for configured-but-uninitialized streams.
func writeStreamUnavailable(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "2")
	w.WriteHeader(http.StatusServiceUnavailable)
}

// getStream validates the stream ID and looks up the stream. Returns the stream
// and 0 on success, or nil and an HTTP status code:
// 404 if the stream ID is invalid, or 503 if the stream is not yet active.
// All valid-format IDs that lack an active stream return 503 uniformly to
// prevent enumerating configured stream IDs via response-code differentiation.
func getStream(store *stream.Store, streamID string) (*stream.Stream, int) {
	if err := config.ValidateStreamID(streamID); err != nil {
		return nil, http.StatusNotFound
	}
	s := store.Get(streamID)
	if s != nil {
		return s, 0
	}
	return nil, http.StatusServiceUnavailable
}

// Stream constructs the chi router for the public HLS stream server.
func Stream(logger *slog.Logger, env config.Env, store *stream.Store, requestLogger *slog.Logger, tracker *watcher.Tracker) chi.Router {
	router := chi.NewRouter()
	router.Use(middleware.RequestID)
	router.Use(mw.TrustedRealIP(env.TRUSTED_PROXIES))
	router.Use(requestLogMiddleware(logger, requestLogger))
	router.Use(middleware.Recoverer)
	router.Use(mw.UnrecoverableGuard)
	router.Use(middleware.SetHeader("X-Content-Type-Options", "nosniff"))
	router.Use(middleware.SetHeader("X-Frame-Options", "DENY"))
	router.Use(middleware.SetHeader("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'"))
	router.Use(middleware.Timeout(config.REQUEST_TIMEOUT))

	router.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// streamExists gates watcher recording to streams the store actually
	// holds, so a client requesting arbitrary {streamID} values cannot grow
	// the tracker's map without bound (see mw.RecordWatcher).
	streamExists := func(streamID string) bool { return store.Get(streamID) != nil }

	router.Route("/stream/{streamID}", func(r chi.Router) {
		// Playlist routes accept only TypePlaylist tokens. Segment-class
		// tokens are deliberately refused here (they fail MAC under the
		// playlist-derived key) so a holder of a baked segment token
		// cannot refetch the media playlist to rotate into a fresh token
		// and defeat the TTL.
		//
		// ViewerTokenAuth runs BEFORE RecordWatcher so that 401 responses
		// do not inflate the active-viewer count.
		r.Group(func(r chi.Router) {
			r.Use(mw.ViewerTokenAuth(store.Clock(), env.STREAM_VIEWER_TOKEN_KEYS, logger, viewer.TypePlaylist))
			r.Use(mw.RecordWatcher(tracker, streamExists))

			r.Get("/media.m3u8", mediaPlaylistHandler(logger, store))
			r.Get("/stream.m3u8", masterPlaylistHandler(logger, store))
		})

		// Init and segment routes accept both TypeSegment (short-lived,
		// baked into playlist URIs — the overwhelmingly common case) and
		// TypePlaylist (direct operator grant). TypeSegment is listed
		// first so the hot path verifies with a single HMAC.
		r.Group(func(r chi.Router) {
			r.Use(mw.ViewerTokenAuth(store.Clock(), env.STREAM_VIEWER_TOKEN_KEYS, logger, viewer.TypeSegment, viewer.TypePlaylist))
			r.Use(mw.RecordWatcher(tracker, streamExists))

			r.Get("/init.mp4", initHandler(logger, store))
			r.Get("/segment_{segmentID}.m4s", segmentHandler(logger, store))
		})
	})

	return router
}

// maxStickyOffsetSecs is the upper bound on a client-supplied ?to=
// magnitude. Anything beyond this is treated as malformed and the
// handler falls back to rendering without an EXT-X-START tag. The
// bound is the configured maximum look-ahead cap
// (config.MaxLookaheadCeilingMs) converted to seconds — the largest
// tail-to-now gap any stream can emit — with a small slack so
// legitimate edge cases at the limit aren't bounced.
const maxStickyOffsetSecs = float64(config.MaxLookaheadCeilingMs)/1000.0 + 60.0

// parseStickyOffset reads the sticky `?to=` magnitude from a request.
// Returns (value, true) only for a finite, non-negative, in-range
// float. Missing/malformed/out-of-range all return (_, false) so the
// caller can fall back to the "no EXT-X-START" path uniformly.
func parseStickyOffset(r *http.Request) (float64, bool) {
	raw := r.URL.Query().Get("to")
	if raw == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, false
	}
	// Reject NaN, +Inf, -Inf, and any negative magnitude. The server
	// emits the leading "-" on the rendered TIME-OFFSET line; the URL
	// param carries the magnitude only.
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0, false
	}
	if v > maxStickyOffsetSecs {
		return 0, false
	}
	return v, true
}

// buildStickyMediaURL constructs the master-baked media URI:
// `media.m3u8?to=<magnitude>[&vt=<...>]`. The magnitude is formatted
// with the same "%.3f" layout StartLineFromOffset uses, so the EXT-X-START
// tag the media playlist emits is byte-identical for every reload of the
// same URL.
func buildStickyMediaURL(offsetSecs float64, vt string) string {
	var b strings.Builder
	var scratch [64]byte
	b.Grow(32 + len(vt))
	b.WriteString("media.m3u8?to=")
	b.Write(strconv.AppendFloat(scratch[:0], offsetSecs, 'f', 3, 64))
	if vt != "" {
		b.WriteString("&vt=")
		b.WriteString(url.QueryEscape(vt))
	}
	return b.String()
}

// mediaPlaylistHandler returns the handler for GET /stream/{streamID}/media.m3u8.
//
// EXT-X-START is sticky via a master-baked `?to=<magnitude>` query
// param. Two paths:
//
//   - **`?to=` present and valid**: render the playlist with that
//     offset baked into EXT-X-START, verbatim. The same URL on every
//     reload returns a byte-identical EXT-X-START line even as the
//     snapshot tail advances, which is what current iOS/macOS clients
//     require — they stall when TIME-OFFSET mutates between reloads.
//
//   - **`?to=` absent or malformed**: render WITHOUT EXT-X-START.
//     Clients fall back to EXT-X-SERVER-CONTROL:HOLD-BACK for live-edge
//     positioning. This path covers direct media.m3u8 fetches that
//     bypassed the master playlist; the common flow is
//     stream.m3u8 → media.m3u8?to=<X> which preserves cross-device
//     sync. We deliberately don't 302 here: HLS reload semantics let
//     players re-fetch the URL stored from the master, so a redirect
//     would be re-followed on every reload and re-bake a new offset,
//     re-introducing the iOS stall.
func mediaPlaylistHandler(logger *slog.Logger, store *stream.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		streamID := chi.URLParam(r, "streamID")
		logger.Debug("handling media request", "streamID", streamID)

		s, status := getStream(store, streamID)
		if s == nil {
			if status == http.StatusServiceUnavailable {
				writeStreamUnavailable(w)
			} else {
				w.WriteHeader(status)
			}
			return
		}

		// Block until a valid playlist is available, the stream is
		// deleted, or the request is cancelled.
		select {
		case <-s.HasPlaylist():
		case <-s.Done():
			writeStreamUnavailable(w)
			return
		case <-r.Context().Done():
			writeStreamUnavailable(w)
			return
		}

		snap := s.CachedPlaylistSnapshot()
		if snap == nil {
			// All segments were evicted between the HasPlaylist gate
			// and now. Tell the player to retry rather than serving
			// an empty body.
			writeStreamUnavailable(w)
			return
		}

		// Sticky render when the master baked a `?to=` value into the
		// URL; otherwise omit EXT-X-START and let HOLD-BACK position
		// the client.
		var startLine string
		if offsetSecs, ok := parseStickyOffset(r); ok {
			startLine = snap.StartLineFromOffset(offsetSecs)
		}
		total := len(snap.Prefix) + len(startLine) + len(snap.Suffix)

		w.Header().Set("Content-Type", config.M3U8_MIME_TYPE)
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Length", strconv.Itoa(total))
		// #nosec G705 -- server-generated playlist body: HLS tags and
		// numeric fields built in pkg/stream/playlist.go, segment URIs
		// of the form "segment_<uint32>.m4s", and optional viewer
		// tokens minted server-side (base64url of an HMAC over fixed
		// server-controlled fields, see pkg/viewer/viewer.go). No path
		// carries user-supplied request data into this body.
		if _, err := io.WriteString(w, snap.Prefix); err != nil {
			logger.Error("failed to write response", "error", err)
			return
		}
		// #nosec G705 -- formatted from a server-clamped float
		// (TIME-OFFSET magnitude) via strconv.AppendFloat with a fixed
		// "%.3f" layout. The input magnitude comes from a validated
		// URL param (parseStickyOffset rejects NaN/Inf/negative/over-
		// range) and is then floored at MinHoldBackSecs in
		// StartLineFromOffset, so the emitted line is always a
		// well-formed EXT-X-START tag.
		if _, err := io.WriteString(w, startLine); err != nil {
			logger.Error("failed to write response", "error", err)
			return
		}
		// #nosec G705 -- see Prefix justification; same renderer output.
		if _, err := io.WriteString(w, snap.Suffix); err != nil {
			logger.Error("failed to write response", "error", err)
			return
		}
	}
}

// masterPlaylistHandler returns the handler for GET /stream/{streamID}/stream.m3u8.
//
// Bakes a fresh `?to=<magnitude>` (the tail-to-now offset) into the
// emitted media URI when a playlist snapshot is available. The player
// parses that URL once from the master and reuses it on every reload,
// so the rendered EXT-X-START stays byte-identical for the session.
//
// Cache-Control is no-store: each session needs its own master fetch
// to capture a fresh wall-clock-aligned offset. Operators who want CDN
// caching of stream.m3u8 can override this in their proxy and accept
// the cross-device-sync drift bounded by the cache TTL.
//
// Pre-live-edge (no snapshot yet) the handler still serves a valid
// master with a bare `media.m3u8` URI — the player polls until media
// becomes available, then plays in the no-EXT-X-START fallback path.
// The common case is master fetched after the stream is live, which
// gets the full sticky-offset behavior.
func masterPlaylistHandler(logger *slog.Logger, store *stream.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		streamID := chi.URLParam(r, "streamID")
		logger.Debug("handling stream request", "streamID", streamID)

		s, status := getStream(store, streamID)
		if s == nil {
			if status == http.StatusServiceUnavailable {
				writeStreamUnavailable(w)
			} else {
				w.WriteHeader(status)
			}
			return
		}

		meta := s.Metadata()

		// HLS players do not carry parent query strings over to
		// relative URIs, so each emitted URI needs its own copy of
		// any propagated param (?vt= for viewer-token auth; ?to= for
		// the sticky EXT-X-START offset).
		vt := r.URL.Query().Get("vt")
		var mediaURI string
		if snap := s.CachedPlaylistSnapshot(); snap != nil {
			nowMs := store.Clock().Now().UnixMilli()
			mediaURI = buildStickyMediaURL(snap.FreshOffsetSecs(nowMs), vt)
		} else {
			mediaURI = "media.m3u8"
			if vt != "" {
				mediaURI += "?vt=" + url.QueryEscape(vt)
			}
		}

		builder := strings.Builder{}
		builder.WriteString("#EXTM3U\n")
		builder.WriteString("#EXT-X-VERSION:7\n")
		builder.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
		builder.WriteString(
			fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d,RESOLUTION=%dx%d,CODECS=\"%s\",FRAME-RATE=%.3f\n", meta.Bandwidth, meta.Width, meta.Height, meta.Codecs, meta.FrameRate),
		)
		builder.WriteString(mediaURI)
		builder.WriteByte('\n')

		w.Header().Set("Content-Type", config.M3U8_MIME_TYPE)
		w.Header().Set("Cache-Control", "no-store")
		body := builder.String()
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if _, err := io.WriteString(w, body); err != nil {
			logger.Error("failed to write response", "error", err)
		}
	}
}

// initHandler returns the handler for GET /stream/{streamID}/init.mp4.
func initHandler(logger *slog.Logger, store *stream.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		streamID := chi.URLParam(r, "streamID")
		logger.Debug("handling init segment request", "streamID", streamID)

		s, status := getStream(store, streamID)
		if s == nil {
			if status == http.StatusServiceUnavailable {
				writeStreamUnavailable(w)
			} else {
				w.WriteHeader(status)
			}
			return
		}

		initData, ok := s.GetInit()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", config.MP4_MIME_TYPE)
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Length", strconv.Itoa(len(initData)))
		if _, err := w.Write(initData); err != nil {
			logger.Error("failed to write response", "error", err)
		}
	}
}

// segmentHandler returns the handler for GET /stream/{streamID}/segment_{segmentID}.m4s.
func segmentHandler(logger *slog.Logger, store *stream.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		streamID := chi.URLParam(r, "streamID")
		segmentIDStr := chi.URLParam(r, "segmentID")
		logger.Debug("handling segment request", "streamID", streamID, "segmentID", segmentIDStr)

		s, status := getStream(store, streamID)
		if s == nil {
			if status == http.StatusServiceUnavailable {
				writeStreamUnavailable(w)
			} else {
				w.WriteHeader(status)
			}
			return
		}

		segmentID, err := strconv.ParseUint(segmentIDStr, 10, 32)
		if err != nil {
			logger.Warn("invalid segmentID", "value", segmentIDStr, "error", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		err = s.RunWithPublishedSegmentSlot(uint32(segmentID), func(slot *pool.BufferSlot) error {
			w.Header().Set("Content-Type", config.MP4_MIME_TYPE)
			w.Header().Set("Content-Length", strconv.Itoa(slot.Len()))
			w.Header().Set("Cache-Control", "no-cache")
			_, err := slot.WriteTo(w)
			return err
		})
		if err != nil {
			if errors.Is(err, stream.ErrSegmentNotFound) {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if errors.Is(err, stream.ErrSegmentNotYetPublished) {
				// The segment is buffered but sits beyond the published
				// look-ahead horizon, so it has not appeared in any media
				// playlist. Collapse to the same 404 as a missing segment so
				// a client cannot distinguish "no such segment" from "future
				// segment not yet live" and read ahead of the live edge.
				//
				// Logged at warn (matching the viewer-token rejection): a
				// compliant client only fetches advertised segments, so this
				// fires only on a misconfigured/buggy client or a deliberate
				// read-ahead probe — both worth surfacing, and the signal a
				// log-driven blocker (e.g. fail2ban) keys on.
				logger.Warn("refusing segment fetch beyond published look-ahead",
					"streamID", streamID, "segmentID", segmentIDStr)
				w.WriteHeader(http.StatusNotFound)
				return
			}
			logger.Error("failed to write response", "error", err)
		}
	}
}
