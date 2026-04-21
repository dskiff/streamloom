package routes

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dskiff/streamloom/pkg/config"
	mw "github.com/dskiff/streamloom/pkg/middleware"
	"github.com/dskiff/streamloom/pkg/pool"
	"github.com/dskiff/streamloom/pkg/stream"
	"github.com/dskiff/streamloom/pkg/viewer"
	"github.com/dskiff/streamloom/pkg/watcher"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

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

// viewerTokenMsPerMinute is used to floor an expires_at_ms value to the
// minute boundary at which tokens are encoded. Kept here to avoid leaking
// the viewer package's private constant across the serde boundary.
const viewerTokenMsPerMinute = 60_000

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

// contextKey is a private type for context keys to avoid collisions.
type contextKey string

// streamIDKey is the context key for the validated string stream ID.
const streamIDKey contextKey = "streamID"

// maxResolutionDimension is the upper bound for width/height (covers up to 32K).
const maxResolutionDimension = 32768

// parseResolution parses a resolution string like "1920x1080" into width and height.
func parseResolution(s string) (width, height int, ok bool) {
	parts := strings.SplitN(s, "x", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	w, err := strconv.Atoi(parts[0])
	if err != nil || w <= 0 || w > maxResolutionDimension {
		return 0, 0, false
	}
	h, err := strconv.Atoi(parts[1])
	if err != nil || h <= 0 || h > maxResolutionDimension {
		return 0, 0, false
	}
	return w, h, true
}

// errMalformedHeader indicates a metadata header is missing or unparseable.
type errMalformedHeader struct{ detail string }

func (e *errMalformedHeader) Error() string { return e.detail }

// errMetadataConflict indicates a metadata header's value differs from
// the existing stream metadata.
type errMetadataConflict struct{ detail string }

func (e *errMetadataConflict) Error() string { return e.detail }

// parseMetadataHeaders parses and validates stream metadata from request
// headers. If existing is nil (first init), all metadata headers are required.
// If existing is non-nil (subsequent init), headers are optional but any
// present header must be valid and match the existing value.
//
// Returns *errMalformedHeader for missing or unparseable headers and
// *errMetadataConflict for value mismatches against existing metadata.
func parseMetadataHeaders(r *http.Request, existing *stream.Metadata) (stream.Metadata, error) {
	required := existing == nil
	var meta stream.Metadata
	if existing != nil {
		meta = *existing
	}

	// Bandwidth
	if v := r.Header.Get("X-SL-BANDWIDTH"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return meta, &errMalformedHeader{fmt.Sprintf("bandwidth: invalid value %q", v)}
		}
		if existing != nil && n != existing.Bandwidth {
			return meta, &errMetadataConflict{fmt.Sprintf("bandwidth: header=%d existing=%d", n, existing.Bandwidth)}
		}
		meta.Bandwidth = n
	} else if required {
		return meta, &errMalformedHeader{"missing X-SL-BANDWIDTH header"}
	}

	// Codecs
	if v := r.Header.Get("X-SL-CODECS"); v != "" {
		if err := stream.ValidateCodecs(v); err != nil {
			return meta, &errMalformedHeader{fmt.Sprintf("codecs: %s", err)}
		}
		if existing != nil && v != existing.Codecs {
			return meta, &errMetadataConflict{fmt.Sprintf("codecs: header=%q existing=%q", v, existing.Codecs)}
		}
		meta.Codecs = v
	} else if required {
		return meta, &errMalformedHeader{"missing X-SL-CODECS header"}
	}

	// Resolution
	if v := r.Header.Get("X-SL-RESOLUTION"); v != "" {
		w, h, ok := parseResolution(v)
		if !ok {
			return meta, &errMalformedHeader{fmt.Sprintf("resolution: invalid value %q", v)}
		}
		if existing != nil && (w != existing.Width || h != existing.Height) {
			return meta, &errMetadataConflict{fmt.Sprintf("resolution: header=%dx%d existing=%dx%d", w, h, existing.Width, existing.Height)}
		}
		meta.Width = w
		meta.Height = h
	} else if required {
		return meta, &errMalformedHeader{"missing X-SL-RESOLUTION header"}
	}

	// Frame rate
	if v := r.Header.Get("X-SL-FRAMERATE"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
			return meta, &errMalformedHeader{fmt.Sprintf("framerate: invalid value %q", v)}
		}
		if existing != nil && f != existing.FrameRate {
			return meta, &errMetadataConflict{fmt.Sprintf("framerate: header=%g existing=%g", f, existing.FrameRate)}
		}
		meta.FrameRate = f
	} else if required {
		return meta, &errMalformedHeader{"missing X-SL-FRAMERATE header"}
	}

	// Target duration
	if v := r.Header.Get("X-SL-TARGET-DURATION"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return meta, &errMalformedHeader{fmt.Sprintf("target-duration: invalid value %q", v)}
		}
		if existing != nil && n != existing.TargetDurationSecs {
			return meta, &errMetadataConflict{fmt.Sprintf("target-duration: header=%d existing=%d", n, existing.TargetDurationSecs)}
		}
		meta.TargetDurationSecs = n
	} else if required {
		return meta, &errMalformedHeader{"missing X-SL-TARGET-DURATION header"}
	}

	// Segment bytes
	if v := r.Header.Get("X-SL-SEGMENT-BYTES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return meta, &errMalformedHeader{fmt.Sprintf("segment-bytes: invalid value %q", v)}
		}
		if existing != nil && n != existing.SegmentByteCount {
			return meta, &errMetadataConflict{fmt.Sprintf("segment-bytes: header=%d existing=%d", n, existing.SegmentByteCount)}
		}
		meta.SegmentByteCount = n
	} else if required {
		return meta, &errMalformedHeader{"missing X-SL-SEGMENT-BYTES header"}
	}

	return meta, nil
}

// API constructs the chi router for the authenticated push API server.
func API(logger *slog.Logger, env config.Env, store *stream.Store, requestLogger *slog.Logger, tracker *watcher.Tracker) chi.Router {
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

	// dummyDigest is a SHA-256 digest used for constant-time comparison when a
	// stream has no configured token. We always compare fixed-size digests so
	// that ConstantTimeCompare never short-circuits on length mismatch.
	dummyDigest := sha256.Sum256([]byte("Bearer __dummy_comparison_target__"))

	router.Route("/api/v1/stream/{streamID}", func(r chi.Router) {
		// Per-stream token auth middleware. The middleware is inside the
		// {streamID} route group so chi.URLParam is available.
		r.Use(func(h http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				streamID := chi.URLParam(r, "streamID")
				if err := config.ValidateStreamID(streamID); err != nil {
					logger.Warn("invalid stream ID", "value", streamID, "error", err)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				tokenDigest := sha256.Sum256([]byte(r.Header.Get("Authorization")))

				// Always compare against the configured token or a dummy
				// digest so that ConstantTimeCompare is always executed and
				// participates in the auth decision — preventing timing leaks
				// and dead-code elimination. Unconfigured streams are still
				// rejected via the !ok condition.
				expected, ok := env.GetStreamToken(streamID)
				if !ok {
					expected = dummyDigest
				}
				if subtle.ConstantTimeCompare(tokenDigest[:], expected[:]) != 1 || !ok {
					if !ok {
						logger.Warn("unauthorized request: stream not configured", "method", r.Method, "path", r.URL.Path, "streamID", streamID)
					} else {
						logger.Warn("unauthorized request: invalid token", "method", r.Method, "path", r.URL.Path, "streamID", streamID)
					}
					w.WriteHeader(http.StatusUnauthorized)
					return
				}

				ctx := context.WithValue(r.Context(), streamIDKey, streamID)
				h.ServeHTTP(w, r.WithContext(ctx))
			})
		})
		r.Delete("/", func(w http.ResponseWriter, r *http.Request) {
			streamID := r.Context().Value(streamIDKey).(string)
			logger.Debug("handling stream delete request", "streamID", streamID)

			if !store.Delete(streamID) {
				w.WriteHeader(http.StatusNotFound)
				return
			}

			tracker.DeleteStream(streamID)
			logger.Info("stream deleted", "streamID", streamID)
			w.WriteHeader(http.StatusNoContent)
		})

		r.Post("/viewer_token", func(w http.ResponseWriter, r *http.Request) {
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
		})

		r.Post("/init", func(w http.ResponseWriter, r *http.Request) {
			streamID := r.Context().Value(streamIDKey).(string)
			logger.Debug("handling stream init request", "streamID", streamID)

			// Read init.mp4 body with size limit (always required).
			r.Body = http.MaxBytesReader(w, r.Body, stream.MaxInitBytes)
			initData, err := io.ReadAll(r.Body)
			if err != nil {
				var maxBytesErr *http.MaxBytesError
				if errors.As(err, &maxBytesErr) {
					logger.Warn("init body too large", "limit", stream.MaxInitBytes)
					w.WriteHeader(http.StatusRequestEntityTooLarge)
				} else {
					logger.Warn("failed to read init body", "error", err)
					w.WriteHeader(http.StatusBadRequest)
				}
				return
			}
			if len(initData) == 0 {
				logger.Warn("empty init body")
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			// Parse required metadata and capacity headers.
			meta, err := parseMetadataHeaders(r, nil)
			if err != nil {
				logger.Warn("invalid metadata header", "error", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			// Parse capacity-only headers (not part of Metadata).
			segmentCapStr := r.Header.Get("X-SL-SEGMENT-CAP")
			if segmentCapStr == "" {
				logger.Warn("missing segment-cap header")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			segmentCap, err := strconv.Atoi(segmentCapStr)
			if err != nil || segmentCap <= 0 {
				logger.Warn("invalid segment-cap header", "value", segmentCapStr, "error", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			totalSlots := int64(segmentCap) + int64(env.BUFFER_WORKING_SPACE)
			if totalSlots > math.MaxInt64/int64(meta.SegmentByteCount) {
				logger.Warn("requested buffer overflows int64",
					"segmentCap", segmentCap,
					"segmentBytes", meta.SegmentByteCount,
				)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			totalBufferBytes := totalSlots * int64(meta.SegmentByteCount)
			if totalBufferBytes > env.STREAM_MAX_BUFFER_BYTES {
				logger.Warn("requested buffer exceeds maximum",
					"segmentCap", segmentCap,
					"segmentBytes", meta.SegmentByteCount,
					"totalBufferBytes", totalBufferBytes,
					"maxBufferBytes", env.STREAM_MAX_BUFFER_BYTES,
				)
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			backwardBufferSizeStr := r.Header.Get("X-SL-BACKWARD-BUFFER-SIZE")
			if backwardBufferSizeStr == "" {
				logger.Warn("missing backward-buffer-size header")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			backwardBufferSize, err := strconv.Atoi(backwardBufferSizeStr)
			if err != nil || backwardBufferSize <= 0 {
				logger.Warn("invalid backward-buffer-size header", "value", backwardBufferSizeStr, "error", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			// When a viewer-token key is configured for this stream, wire a
			// mint callback the renderer will use once per playlist render to
			// bake a short-lived segment-class token into every emitted URI.
			// Streams without a configured key receive no option and the
			// renderer emits plain URIs (preserves public-playback parity
			// with pre-viewer-token behavior).
			var initOpts []stream.InitOption
			if viewerKeys, ok := env.GetViewerKeys(streamID); ok {
				minter := makePlaylistTokenMinter(viewerKeys.Segment, logger, streamID)
				initOpts = append(initOpts, stream.WithMintToken(minter))
			}

			if err := store.Init(streamID, meta, initData, segmentCap, meta.SegmentByteCount, backwardBufferSize, env.BUFFER_WORKING_SPACE, config.DefaultMediaWindowSize, initOpts...); err != nil {
				if errors.Is(err, stream.ErrStreamExists) {
					logger.Warn("stream already exists", "streamID", streamID)
					w.WriteHeader(http.StatusConflict)
					return
				}
				logger.Warn("invalid stream configuration", "error", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			logger.Info("stream initialized",
				"streamID", streamID,
				"bandwidth", meta.Bandwidth,
				"codecs", meta.Codecs,
				"resolution", fmt.Sprintf("%dx%d", meta.Width, meta.Height),
				"framerate", meta.FrameRate,
				"segmentCap", segmentCap,
				"segmentBytes", meta.SegmentByteCount,
				"backwardBufferSize", backwardBufferSize,
			)

			w.WriteHeader(http.StatusCreated)
		})

		r.Get("/active_watchers", func(w http.ResponseWriter, r *http.Request) {
			streamID := r.Context().Value(streamIDKey).(string)
			logger.Debug("handling active watchers request", "streamID", streamID)

			windowMs := watcher.DefaultWindowMs
			if raw := r.URL.Query().Get("window_ms"); raw != "" {
				parsed, err := strconv.ParseInt(raw, 10, 64)
				if err != nil || parsed <= 0 {
					logger.Warn("invalid window_ms parameter", "value", raw)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				windowMs = parsed
			}

			count := tracker.ActiveCount(streamID, windowMs)

			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Cache-Control", "no-store")
			if _, err := fmt.Fprintf(w, "%d", count); err != nil {
				logger.Error("failed to write response", "error", err)
			}
		})

		r.Post("/segment", func(w http.ResponseWriter, r *http.Request) {
			streamID := r.Context().Value(streamIDKey).(string)
			logger.Debug("handling stream segment upload request", "streamID", streamID)

			s := store.Get(streamID)
			if s == nil {
				logger.Warn("stream not found for segment upload", "streamID", streamID)
				w.WriteHeader(http.StatusNotFound)
				return
			}

			// Index is the segment sequence number from the transcoder.
			indexStr := r.Header.Get("X-SL-INDEX")
			if indexStr == "" {
				logger.Warn("missing index header")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			index, err := strconv.ParseUint(indexStr, 10, 32)
			if err != nil {
				logger.Warn("invalid index header", "value", indexStr, "error", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			// Timestamp is the UNIX timestamp in milliseconds indicating the start time of the segment.
			timestampStr := r.Header.Get("X-SL-TIMESTAMP")
			if timestampStr == "" {
				logger.Warn("missing timestamp header")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			timestampNum, err := strconv.ParseInt(timestampStr, 10, 64)
			if err != nil {
				logger.Warn("invalid timestamp header", "error", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			// Duration is the number of milliseconds that the segment covers.
			durationStr := r.Header.Get("X-SL-DURATION")
			if durationStr == "" {
				logger.Warn("missing duration header")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			durationMs, err := strconv.ParseUint(durationStr, 10, 32)
			if err != nil {
				logger.Warn("invalid duration header", "error", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if durationMs == 0 {
				logger.Warn("invalid duration header: zero duration", "duration", durationMs)
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			// Parse optional generation header (default 0).
			var generation int64
			if genStr := r.Header.Get("X-SL-GENERATION"); genStr != "" {
				generation, err = strconv.ParseInt(genStr, 10, 64)
				if err != nil || generation < 0 {
					logger.Warn("invalid generation header", "value", genStr, "error", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
			}

			// Acquire a buffer slot from the pool.
			buf, ok := s.AcquireSlot()
			if !ok {
				logger.Warn("buffer pool exhausted", "streamID", streamID)
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}

			// Limit request body to the pre-allocated slot capacity plus one
			// byte so the BufferSlot.ReadFrom overflow probe can detect
			// oversized bodies. Without the extra byte, MaxBytesReader
			// would silently block the probe read and mask the overflow.
			r.Body = http.MaxBytesReader(w, r.Body, int64(s.Metadata().SegmentByteCount)+1)

			// Read request body directly into the pre-allocated slot.
			n, err := buf.ReadFrom(r.Body)
			if err != nil {
				s.ReleaseSlot(buf)
				var maxBytesErr *http.MaxBytesError
				if errors.Is(err, pool.ErrOverflow) || errors.As(err, &maxBytesErr) {
					logger.Warn("segment body too large", "streamID", streamID)
					w.WriteHeader(http.StatusRequestEntityTooLarge)
				} else {
					logger.Warn("failed to read segment body", "error", err)
					w.WriteHeader(http.StatusBadRequest)
				}
				return
			}
			if n == 0 {
				s.ReleaseSlot(buf)
				logger.Warn("empty segment body")
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			if err := s.CommitSlot(uint32(index), buf, timestampNum, uint32(durationMs), generation); err != nil {
				s.ReleaseSlot(buf)
				if errors.Is(err, stream.ErrStaleGeneration) {
					logger.Warn("stale generation", "streamID", streamID, "index", index, "generation", generation)
					w.WriteHeader(http.StatusConflict)
					return
				}
				if errors.Is(err, stream.ErrTimestampInPast) {
					logger.Warn("segment timestamp is in the past", "streamID", streamID, "index", index, "timestamp", timestampNum)
					w.WriteHeader(http.StatusUnprocessableEntity)
					return
				}
				if errors.Is(err, stream.ErrDuplicateIndex) {
					logger.Warn("duplicate segment index", "streamID", streamID, "index", index)
					w.WriteHeader(http.StatusConflict)
					return
				}
				if errors.Is(err, stream.ErrTimestampOrderViolation) {
					logger.Warn("segment timestamp order violation", "streamID", streamID, "index", index, "timestamp", timestampNum, "error", err)
					w.WriteHeader(http.StatusUnprocessableEntity)
					return
				}
				if errors.Is(err, stream.ErrBufferFull) {
					logger.Warn("buffer full", "streamID", streamID, "index", index)
					w.Header().Set("Retry-After", "1")
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				logger.Error("failed to commit segment", "error", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			segLen, segCap := s.SegmentLoad()
			if segCap > 0 && segLen >= segCap*4/5 {
				leadMs := timestampNum - store.Clock().Now().UnixMilli()
				logger.Info("segment accepted near saturation",
					"streamID", streamID,
					"index", index,
					"segment_lead_ms", leadMs,
					"segments", segLen,
					"segment_cap", segCap,
				)
			}

			logger.Debug("segment uploaded", "streamID", streamID, "index", index, "timestamp", time.UnixMilli(timestampNum), "duration", durationMs, "size", n)
			w.WriteHeader(http.StatusCreated)
		})
	})

	return router
}
