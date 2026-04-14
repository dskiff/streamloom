# Stateless Viewer Tokens for Stream Playback

## Context

streamloom's push path (`POST /api/v1/stream/<id>/...`) was already protected
by a per-stream bearer token (`SL_STREAM_<id>_TOKEN`), but the playback path
(`/stream/<id>/*`) was fully unauthenticated by design. This project adds an
**optional, opt-in-per-stream** stateless viewer-token scheme so deployments
can gate playback with short-lived, self-contained share tokens without
introducing a session store.

- Per-stream signing key: `SL_STREAM_<id>_VIEWER_TOKEN_KEY` (minimum 32 chars).
- When configured, the API exposes `POST /api/v1/stream/<id>/viewer_token`
  (push-authenticated) which mints a token with a caller-specified expiration.
- The stream server requires `?vt=<token>` on every stream URL for that stream
  and rejects expired / malformed / bad-MAC tokens with `401`.
- Tokens are stateless: validation is a MAC check + `exp > now`.
- Streams with no `VIEWER_TOKEN_KEY` configured behave exactly as before
  (fully public playback). Fully backward-compatible.

## Wire format

```
[1 byte version=1] [4 bytes uint32 big-endian unix minutes] [16 bytes truncated HMAC-SHA256]
```

21 bytes → `base64.RawURLEncoding` → **28-char token**. The MAC is computed
over `version_byte || exp_minutes_bytes` and truncated to the leftmost 16
bytes (128-bit, RFC 2104 §5). The env-var value is used directly as the HMAC
key. Stream ID is NOT bound into the MAC — per-stream keys already isolate
streams.

Expiration is encoded as unsigned unix minutes (uint32), giving ~8170 years
of range from the epoch — practical overflow is impossible. Minute precision
is justified by the domain (viewer share links are minutes to hours long) and
keeps `?vt=` short wherever it appears in a playlist; the media playlist
emits one token per init URI and one per segment URI, so the 6-char saving
multiplies across every segment window.

The ms-granularity public API (`expires_at_ms`) is preserved; the minute unit
is an encoding detail isolated to `pkg/viewer/`. The handler floors the
requested `expires_at_ms` to the minute boundary before encoding and echoes
the aligned value in the response so callers see exactly what was encoded.

## Changes

### New: `pkg/viewer/`
- `Mint(key, expiresAtMs)` and `Verify(key, now, token)` with explicit error
  sentinels (`ErrMalformed`, `ErrBadMAC`, `ErrExpired`,
  `ErrUnsupportedVersion`). `Verify` always runs the MAC computation, even on
  malformed input, to keep timing uniform.

### `pkg/config/env.go`
- Added `STREAM_VIEWER_TOKEN_KEYS map[string][]byte` on `Env`.
- Added `GetViewerTokenKey(streamID)` method.
- Added `parseStreamViewerTokenKeys()` mirroring `parseStreamTokens()`;
  validates stream ID, enforces min key length, `os.Unsetenv` after read.
- Defensive skip in `parseStreamTokens` to avoid misclassifying
  `SL_STREAM_<id>_VIEWER_TOKEN_KEY` as a push token.

### `pkg/middleware/viewertoken.go` (new)
- `ViewerTokenAuth(clock, keys, logger)` middleware: opt-in per stream. When
  a key is configured, `vt` query is verified; any failure → 401. When no key
  is configured, requests pass through untouched.

### `pkg/routes/api.go`
- `POST /api/v1/stream/{id}/viewer_token` handler under the push-token-
  protected subrouter. Returns `201 Created` with `{token, expires_at_ms}`,
  `409 Conflict` if no key is configured, `400` if the minute-aligned TTL is
  below the 5-minute minimum (`MinViewerTokenTTLMs`), malformed body, or
  expiry in the past. Cache-Control: no-store. Floors the requested
  `expires_at_ms` to a minute boundary and echoes the aligned value so
  clients see exactly what was encoded in the token.

### `pkg/routes/stream.go`
- Wired `ViewerTokenAuth` **before** `RecordWatcher` so 401 responses do not
  inflate watcher counts.
- Master playlist (`stream.m3u8`): appends `?vt=<escaped>` to the emitted
  `media.m3u8` URI when the incoming request carried `vt`. Master playlists
  are not cached and use the viewer's own long-lived token because viewers
  refresh `media.m3u8` repeatedly (not `stream.m3u8`), so baking a short-
  lived token here would break playback as soon as that token expired.
- Media playlist (`media.m3u8`): serves the renderer's cached body verbatim.
  The renderer has already baked a playlist-scoped short-lived token into
  every emitted URI, so no per-request substitution (and no per-viewer
  per-fetch allocation) is required.

### `pkg/stream/playlist.go`
- The media-playlist renderer calls a per-stream `mintToken() string`
  callback once per render and bakes the returned token into every emitted
  URI as `?vt=<token>`. When the callback is nil (stream without a viewer
  key) or returns `""`, URIs are emitted plain. This replaces the earlier
  `{VT}` placeholder + serve-time substitution scheme — the playlist is
  now a fully-formed artifact that can be served verbatim. The cached
  playlist remains per-stream (not per-viewer): every viewer that fetches
  `media.m3u8` in the same render window sees the same short-lived token
  embedded in its URIs.

### `pkg/stream/stream.go`
- Added a `mintToken func() string` field to `Stream` and an `InitOption` /
  `WithMintToken(fn)` functional-options pattern on `Store.Init`. The
  option is applied before the renderer goroutine is launched, so the
  renderer observes a fully-configured Stream without additional locking.
  Streams without a viewer key are initialized with no options and run
  identically to the pre-viewer-token path.

### `pkg/routes/api.go` (playlist-token plumbing)
- Added `PlaylistTokenTTL = 10 * time.Minute`. The `/init` handler builds a
  `makePlaylistTokenMinter` closure when the stream has a configured viewer
  key, capturing the key, store clock, and logger. The closure is passed
  to `store.Init` via `stream.WithMintToken(...)`. Streams without a key
  receive no option, preserving fully-public playback.

### `main.go`
- Logs `viewer token key configured` for each configured stream at startup.

### `README.md`
- Added `SL_STREAM_<id>_VIEWER_TOKEN_KEY` configuration row.
- Documented `POST /api/v1/stream/{id}/viewer_token` and the `?vt=` playback
  query param.

## Testing

- `pkg/viewer/viewer_test.go` — round-trip, tampered MAC/exp/version, expired,
  wrong key, malformed base64, wrong length, empty string, URL-safety,
  determinism.
- `pkg/config/viewer_key_test.go` — parsing happy paths, prefix/suffix
  ambiguity regression (both directions), min-length, invalid stream ID,
  secret-hygiene (env unset after parse).
- `pkg/routes/viewer_token_test.go` — mint endpoint: success, no-key 409,
  missing/invalid push auth 401, invalid stream ID 404, expiry in past 400,
  empty / invalid / unknown-field JSON 400.
- `pkg/middleware/viewertoken_test.go` — pass-through when no key, 401 on
  missing / malformed / expired / tampered / wrong-key, 200 on valid.
- `pkg/routes/viewer_stream_test.go` — per-route integration including the
  critical `TestStream_UnauthorizedRequest_DoesNotRecordWatcher` which locks
  in the middleware ordering.
- `pkg/stream/playlist_vt_test.go` — asserts the renderer's mint callback
  fires exactly once per render, its token is baked into EXT-X-MAP and
  every segment URI, distinct renders embed distinct tokens, an empty
  return from the callback produces plain URIs (fail-closed via the
  middleware), and streams with no mint callback emit no `?vt=`.
- `pkg/routes/viewer_e2e_test.go` — full flow: mint → master → media → init
  → segment with `vt` threaded through, and a post-expiry rejection test.

## Risks & mitigations

1. **Key leak** — a leaked key mints arbitrary-expiry tokens (no cap).
   Mitigation: rotating the env var invalidates all outstanding tokens.
2. **Timing attacks** — `hmac.Equal` over fixed-length inputs; `Verify`
   always runs the MAC path even for malformed input.
3. **Watcher-count inflation** — auth middleware placed before
   `RecordWatcher`; verified by a dedicated test.
4. **Query-string propagation** — server rewrites every emitted URI since
   HLS players don't propagate parent query strings to relative URIs.
5. **Forward compatibility** — version byte lets future format revisions
   coexist; unknown version → 401.
6. **Secret hygiene** — keys are `os.Unsetenv`'d after parsing (mirrors push
   tokens), never logged, never included in responses.
7. **Playlist URL scraping** — the playlist embeds a short-lived
   (`PlaylistTokenTTL = 10m`) token rather than the viewer's long-lived
   one, bounding the replay window of URLs scraped from a playlist body
   while keeping the cached playlist per-stream (not per-viewer). If a
   stream stops producing segments for longer than this TTL, the cached
   playlist's embedded token will expire; subsequent segment fetches 401
   until the next render refreshes the token. For live streams this is a
   non-issue because each segment commit triggers a re-render.
