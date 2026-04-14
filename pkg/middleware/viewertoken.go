package middleware

import (
	"log/slog"
	"net/http"

	"github.com/dskiff/streamloom/pkg/clock"
	"github.com/dskiff/streamloom/pkg/config"
	"github.com/dskiff/streamloom/pkg/viewer"
	"github.com/go-chi/chi/v5"
)

// ViewerTokenAuth returns a middleware that enforces stateless viewer
// tokens on stream routes. It must be mounted inside a {streamID} route
// group so chi.URLParam("streamID") resolves.
//
// The capability class of each accepted token is now bound into the
// signing key via viewer.DeriveKey rather than carried in the token
// payload. allowedTypes selects which per-(stream, type) derived keys
// this middleware tries when verifying. Callers should list the most-
// expected type FIRST: the first derived key that verifies the token
// authorizes the request, and a failing request always runs every
// candidate verify. For example, init/segment routes pass
// (TypeSegment, TypePlaylist) so the hot path (playlist-baked segment
// tokens) completes with a single HMAC.
//
// Playlist routes pass only TypePlaylist so that short-lived segment-
// scoped tokens cannot be used to refetch the media playlist (which
// would otherwise let a holder rotate into a freshly baked playlist
// token indefinitely, defeating the TTL). A segment-class token fails
// MAC under the playlist-derived key and is rejected as 401.
//
// Behavior:
//   - Invalid stream ID format: 404.
//   - Stream has no configured viewer-token key: pass through (feature
//     is opt-in per stream; preserves backward-compatible public
//     playback).
//   - Stream has keys and no candidate derived key verifies the `vt`
//     query param: 401. Individual verify failure modes (missing,
//     malformed, bad MAC, expired, unsupported version) are all logged
//     at warn but collapsed to a single 401 so attackers can't
//     distinguish failure modes.
//
// This middleware must be placed BEFORE any watcher-recording middleware
// so that unauthorized requests do not inflate the active-viewer count.
func ViewerTokenAuth(clk clock.Clock, keys map[string]config.ViewerKeys, logger *slog.Logger, allowedTypes ...viewer.Type) func(http.Handler) http.Handler {
	// Requiring at least one allowed type makes mis-configuration (a
	// permission-less route group) a panic rather than silent 401s.
	if len(allowedTypes) == 0 {
		panic("middleware: ViewerTokenAuth requires at least one allowed token type")
	}
	// Validate that every allowedType has a known string representation so
	// the hot-path key-selection switch never falls through at runtime.
	for _, t := range allowedTypes {
		switch t {
		case viewer.TypePlaylist, viewer.TypeSegment:
		default:
			panic("middleware: ViewerTokenAuth received unknown viewer.Type")
		}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			streamID := chi.URLParam(r, "streamID")
			if err := config.ValidateStreamID(streamID); err != nil {
				logger.Warn("invalid stream ID", "value", streamID, "error", err)
				w.WriteHeader(http.StatusNotFound)
				return
			}

			streamKeys, ok := keys[streamID]
			if !ok {
				// Viewer-token auth is opt-in per stream. Unconfigured
				// streams remain publicly accessible.
				next.ServeHTTP(w, r)
				return
			}

			vt := r.URL.Query().Get("vt")
			now := clk.Now()

			var authorized bool
			var lastErr error
			for _, typ := range allowedTypes {
				var k []byte
				switch typ {
				case viewer.TypePlaylist:
					k = streamKeys.Playlist
				case viewer.TypeSegment:
					k = streamKeys.Segment
				}
				if err := viewer.Verify(k, now, vt); err == nil {
					authorized = true
					break
				} else {
					lastErr = err
				}
			}

			if !authorized {
				logger.Warn("viewer token rejected",
					"streamID", streamID,
					"method", r.Method,
					"path", r.URL.Path,
					"reason", lastErr.Error(),
				)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
