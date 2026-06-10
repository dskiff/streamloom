package routes

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/dskiff/streamloom/pkg/config"
	"github.com/dskiff/streamloom/pkg/stream"
	"github.com/dskiff/streamloom/pkg/viewer"
)

// This file holds the operator-facing viewer-token mint endpoint
// (POST /viewer_token). It is the push-authenticated path by which an
// operator mints a long-lived, playlist-class token to hand to a viewer.
// Token minting is security-sensitive, so it lives apart from the
// request-routing glue in its own file (covered by viewer_token_test.go).
// The renderer-side minter that bakes per-URI segment tokens lives in
// playlist_token_minter.go.

// MaxViewerTokenRequestBytes is the hard upper bound on a viewer-token mint
// request body. The body is a tiny JSON object; 1 KiB is generous.
const MaxViewerTokenRequestBytes = 1 << 10

// MinViewerTokenTTLMs is the minimum viewer-token lifetime (measured from
// mint time to the minute-aligned expiry). Sub-5-minute tokens are rejected
// to make the encoding's minute precision a non-issue for callers — a
// reasonable floor for share-link semantics and comfortably larger than a
// typical client-clock drift budget.
const MinViewerTokenTTLMs = 5 * 60 * 1000

// MaxViewerTokenDefaultTTLMs is a soft upper bound on viewer-token
// lifetime. Mint requests whose minute-aligned TTL exceeds this are
// rejected unless the caller sets allow_long_token: true. It exists as a
// failsafe against operator typos and misconfigured automation that would
// otherwise produce share links valid for months or years; callers with a
// legitimate long-lived use case opt in explicitly.
const MaxViewerTokenDefaultTTLMs = 7 * 24 * 60 * 60 * 1000

// viewerTokenMsPerMinute is used to floor an expires_at_ms value to the
// minute boundary at which tokens are encoded. Kept here to avoid leaking
// the viewer package's private constant across the serde boundary.
const viewerTokenMsPerMinute = 60_000

// viewerTokenHandler returns the POST /viewer_token handler: the
// push-authenticated endpoint that mints an operator-granted, playlist-class
// viewer token. It is wired into the {streamID} route group in API, so the
// validated stream ID is available on the request context (streamIDKey) and
// push-token auth has already run.
func viewerTokenHandler(logger *slog.Logger, env config.Env, store *stream.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		streamID := r.Context().Value(streamIDKey).(string)
		logger.Debug("handling viewer token mint request", "streamID", streamID)

		keys, ok := env.GetViewerKeys(streamID)
		if !ok {
			// No viewer-token key configured for this stream; the feature
			// is opt-in per stream. Signal that the caller's request
			// conflicts with the current server configuration.
			logger.Warn("viewer token mint for unconfigured stream", "streamID", streamID)
			w.WriteHeader(http.StatusConflict)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, MaxViewerTokenRequestBytes)
		var req struct {
			ExpiresAtMs    int64 `json:"expires_at_ms"`
			AllowLongToken bool  `json:"allow_long_token"`
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			// Distinguish body-size overflow (413) from parse errors
			// (400) so misbehaving clients get an accurate signal and
			// the status matches the other authenticated endpoints.
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				logger.Warn("viewer token request body too large",
					"streamID", streamID, "limit", MaxViewerTokenRequestBytes)
				w.WriteHeader(http.StatusRequestEntityTooLarge)
				return
			}
			logger.Warn("invalid viewer token request body", "error", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// Reject trailing data after the JSON object. Decode consumes a
		// single value and ignores anything after it; without this
		// check, an input like `{"expires_at_ms": N}{"extra":1}` would
		// silently succeed.
		if dec.More() {
			logger.Warn("trailing data in viewer token request body", "streamID", streamID)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		nowMs := store.Clock().Now().UnixMilli()
		// Truncate the requested expiry to the minute boundary at which
		// tokens are encoded, then enforce a minimum TTL on the encoded
		// value. This surfaces the wire format's minute precision as an
		// explicit contract rather than letting callers receive tokens
		// that silently expire earlier than they expected.
		//
		// Go integer division truncates toward zero (not floor toward
		// negative infinity), so negative inputs align toward zero
		// rather than away from it. The TTL check below rejects any
		// such value, so truncation semantics never surface to callers.
		alignedExpMs := (req.ExpiresAtMs / viewerTokenMsPerMinute) * viewerTokenMsPerMinute
		if alignedExpMs-nowMs < MinViewerTokenTTLMs {
			logger.Warn("viewer token TTL below minimum",
				"streamID", streamID,
				"requested_expires_at_ms", req.ExpiresAtMs,
				"aligned_expires_at_ms", alignedExpMs,
				"now_ms", nowMs,
				"min_ttl_ms", MinViewerTokenTTLMs)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if !req.AllowLongToken && alignedExpMs-nowMs > MaxViewerTokenDefaultTTLMs {
			logger.Warn("viewer token TTL above default maximum",
				"streamID", streamID,
				"requested_expires_at_ms", req.ExpiresAtMs,
				"aligned_expires_at_ms", alignedExpMs,
				"now_ms", nowMs,
				"max_ttl_ms", MaxViewerTokenDefaultTTLMs)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		// The operator-facing endpoint mints TypePlaylist tokens (signed
		// under the playlist-derived key); these are accepted on every
		// stream route, including playlists. Segment/init routes also
		// accept them as an operator-grant fallback.
		token, err := viewer.Mint(keys.Playlist, alignedExpMs)
		if err != nil {
			// ErrMalformed here is client-triggerable (e.g. an
			// expires_at_ms so large its minute value overflows
			// uint32), so surface it as 400 rather than 500.
			// Anything else is a server-side failure.
			if errors.Is(err, viewer.ErrMalformed) {
				logger.Warn("viewer token exp out of encodable range",
					"streamID", streamID,
					"aligned_expires_at_ms", alignedExpMs,
					"error", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			logger.Error("failed to mint viewer token", "error", err, "streamID", streamID)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		resp := struct {
			Token       string `json:"token"`
			ExpiresAtMs int64  `json:"expires_at_ms"`
		}{
			Token:       token,
			ExpiresAtMs: alignedExpMs,
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusCreated)
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			logger.Error("failed to write viewer token response", "error", err)
		}
	}
}
