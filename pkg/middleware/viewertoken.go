package middleware

import (
	"log/slog"
	"net/http"

	"github.com/dskiff/streamloom/pkg/clock"
	"github.com/dskiff/streamloom/pkg/config"
	"github.com/dskiff/streamloom/pkg/viewer"
	"github.com/go-chi/chi/v5"
)

// ViewerTokenAuth returns a middleware that enforces stateless viewer tokens
// on stream routes. It must be mounted inside a {streamID} route group so
// chi.URLParam("streamID") resolves.
//
// allowedTypes restricts which token Type classes may be accepted on the
// routes this middleware guards. Playlist routes pass only viewer.TypeViewer
// so that short-lived segment-scoped tokens cannot be used to refetch the
// media playlist (which would otherwise let a holder rotate into a freshly
// baked playlist token indefinitely, defeating the TTL). Init/segment routes
// accept both TypeViewer and TypeSegment.
//
// Behavior:
//   - Invalid stream ID format: 404.
//   - Stream has no configured viewer-token key: pass through (feature is
//     opt-in per stream; preserves backward-compatible public playback).
//   - Stream has a key and the request lacks a valid `vt` query param: 401.
//     Validation failures (missing, malformed, bad MAC, expired, unsupported
//     version, disallowed type) are all logged at warn but collapsed to a
//     single 401 so attackers can't distinguish failure modes.
//
// This middleware must be placed BEFORE any watcher-recording middleware so
// that unauthorized requests do not inflate the active-viewer count.
func ViewerTokenAuth(clk clock.Clock, keys map[string][]byte, logger *slog.Logger, allowedTypes ...viewer.Type) func(http.Handler) http.Handler {
	// Build a lookup set once at construction so the hot path is a single
	// map probe rather than a linear scan. Requiring at least one allowed
	// type makes mis-configuration (a permission-less route group) a panic
	// rather than silent 401s.
	if len(allowedTypes) == 0 {
		panic("middleware: ViewerTokenAuth requires at least one allowed token type")
	}
	allowed := make(map[viewer.Type]struct{}, len(allowedTypes))
	for _, t := range allowedTypes {
		allowed[t] = struct{}{}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			streamID := chi.URLParam(r, "streamID")
			if err := config.ValidateStreamID(streamID); err != nil {
				logger.Warn("invalid stream ID", "value", streamID, "error", err)
				w.WriteHeader(http.StatusNotFound)
				return
			}

			key, ok := keys[streamID]
			if !ok {
				// Viewer-token auth is opt-in per stream. Unconfigured
				// streams remain publicly accessible.
				next.ServeHTTP(w, r)
				return
			}

			vt := r.URL.Query().Get("vt")
			typ, err := viewer.Verify(key, clk.Now(), vt)
			if err != nil {
				logger.Warn("viewer token rejected",
					"streamID", streamID,
					"method", r.Method,
					"path", r.URL.Path,
					"reason", err.Error(),
				)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if _, okType := allowed[typ]; !okType {
				// Token is cryptographically valid but not accepted on
				// this route class. This specifically defends against a
				// segment-scoped token being replayed on a playlist route
				// (which would let a client rotate their token forever).
				logger.Warn("viewer token type not allowed on route",
					"streamID", streamID,
					"method", r.Method,
					"path", r.URL.Path,
					"type", typ,
				)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
