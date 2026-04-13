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
[1 byte version=1] [8 bytes int64 big-endian exp_ms] [16 bytes truncated HMAC-SHA256]
```

25 bytes → `base64.RawURLEncoding` → **34-char token**. The MAC is computed
over `version_byte || exp_ms_bytes` and truncated to the leftmost 16 bytes
(128-bit, RFC 2104 §5). The env-var value is used directly as the HMAC key.
Stream ID is NOT bound into the MAC — per-stream keys already isolate streams.

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
  `409 Conflict` if no key is configured, `400` on missing / past expiry /
  malformed body. Cache-Control: no-store.

### `pkg/routes/stream.go`
- Wired `ViewerTokenAuth` **before** `RecordWatcher` so 401 responses do not
  inflate watcher counts.
- Master playlist (`stream.m3u8`): appends `?vt=<escaped>` to the emitted
  `media.m3u8` URI when the incoming request carried `vt`.
- Media playlist (`media.m3u8`): calls `stream.ResolveViewerToken` to
  substitute the per-viewer query placeholder at serve time.

### `pkg/stream/playlist.go`
- Introduced `vtPlaceholder = "{VT}"`. The playlist renderer emits this
  placeholder after every URI (EXT-X-MAP init URI and each segment URI). The
  HTTP handler substitutes the placeholder at serve time via
  `ResolveViewerToken(playlist, vt)`. When `vt` is empty, placeholders are
  stripped. This keeps the cached playlist per-stream rather than per-viewer.

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
- `pkg/stream/playlist_vt_test.go` — `ResolveViewerToken` unit tests plus
  a test asserting the placeholder is emitted at every URI location.
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
