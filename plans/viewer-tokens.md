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
bytes (128-bit, RFC 2104 §5). The signing key is a **derived** key, not the
raw env-var value — see "Key derivation and route scoping" below.

### Key derivation and route scoping

A token's capability class (formerly encoded as a payload `type` byte) is
now bound into the **signing key** via a KDF:

```
derivedKey = HMAC-SHA256(envSecret, "streamloom/viewer-token/v1" || 0x00 || streamID || 0x00 || typeString)
```

where `typeString` is `"playlist"` or `"segment"`. Zero-byte separators
prevent concatenation ambiguity at the streamID/typeString boundary. Two
capability classes exist:

- `TypePlaylist` (1) — minted via the push-authenticated
  `POST /viewer_token` endpoint. Represents direct operator intent to grant
  a named viewer access. Accepted on **all** stream routes (playlists,
  init, segments). (Named "playlist" because playlist routes are the
  exclusive class that accept only this type; init/segment routes also
  accept it as an operator-grant fallback.)
- `TypeSegment` (2) — minted internally by the media-playlist renderer and
  baked into init/segment URIs. Accepted **only** on init/segment routes —
  never on playlists.

Each route class passes the set of candidate `Type`s to the middleware;
the middleware iterates them in order and verifies the token against each
class's derived key. The first match authorizes the request. Init/segment
routes list `TypeSegment` first so the overwhelmingly common case
(playlist-baked segment tokens) completes with a single HMAC.

Scoping `TypeSegment` out of playlist routes defends against an infinite
token-rotation attack: without it, a holder of a baked playlist token
could refetch `media.m3u8` to receive a freshly-minted short-lived token,
rotating indefinitely and defeating the TTL. Because the class is bound
into the signing key itself, a cryptographically valid token minted with
the segment-derived key cannot be "upgraded" to playlist capability by
any byte flip — it fails MAC under the playlist-derived key, which is the
only key playlist routes try.

The same KDF structure enforces cross-stream binding: two streams sharing
the same raw env secret (an operator misconfiguration) still derive
distinct signing keys because the streamID is mixed into the KDF input.
This is defense-in-depth above the existing per-stream env-var isolation.

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
- `DeriveKey(envKey, streamID, typ) ([]byte, error)` derives the per-
  (stream, type) HMAC signing key from the raw env secret using
  HMAC-SHA256 as a PRF with a domain-separated context string.
- `Mint(key, expiresAtMs)` and `Verify(key, now, token) error` with
  explicit error sentinels (`ErrMalformed`, `ErrBadMAC`, `ErrExpired`,
  `ErrUnsupportedVersion`, `ErrEmptyKey`, `ErrUnknownType`). `Verify`
  always runs the MAC computation, even on malformed input, to keep
  timing uniform. A successful Verify implies the token was minted under
  the same derived key — i.e. for the same (stream, type) pair.

### `pkg/config/env.go`
- Added `ViewerKeys` struct bundling the per-stream derived `Playlist`
  and `Segment` signing keys.
- Added `STREAM_VIEWER_TOKEN_KEYS map[string]ViewerKeys` on `Env`.
- Added `GetViewerKeys(streamID)` method.
- `parseStreamViewerTokenKeys()` derives both class keys from the raw env
  secret at parse time, stores only the derived keys, then clears the
  raw secret and `os.Unsetenv`s the variable. The raw secret never
  leaves the parse function — a hardening win on top of env-var hygiene.
- Defensive skip in `parseStreamTokens` to avoid misclassifying
  `SL_STREAM_<id>_VIEWER_TOKEN_KEY` as a push token.

### `pkg/middleware/viewertoken.go`
- `ViewerTokenAuth(clock, keys, logger, allowedTypes...)` middleware:
  opt-in per stream. When derived keys are configured, the `vt` query is
  verified under each class's derived key in the order specified by
  `allowedTypes`; the first verify that succeeds authorizes the request.
  When no keys are configured, requests pass through untouched. Callers
  order `allowedTypes` from most-expected to least-expected so the common
  case is a single HMAC. A zero-length allow list (or an unknown `Type`)
  is a configuration error and panics at wiring time rather than silently
  rejecting every request.

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
- The `/stream/{streamID}` routes are split into two `chi.Group`s with
  distinct allowed-type sets:
  - **Playlist group** (`media.m3u8`, `stream.m3u8`) — passes only
    `TypePlaylist`, so the middleware verifies solely under the
    playlist-derived key. Segment-class tokens fail MAC here, the
    central defense against infinite token rotation.
  - **Init/segment group** (`init.mp4`, `segment_*.m4s`) — passes
    `TypeSegment` first (hot path: playlist-baked tokens) then
    `TypePlaylist` (operator-grant fallback), so playlist-baked tokens
    authorize with a single HMAC and operator tokens still work.
- Master playlist (`stream.m3u8`): appends `?vt=<escaped>` to the emitted
  `media.m3u8` URI when the incoming request carried `vt`. Master playlists
  are not cached and use the viewer's own long-lived (`TypePlaylist`)
  token because viewers refresh `media.m3u8` repeatedly (not
  `stream.m3u8`), so baking a short-lived token here would break playback
  as soon as that token expired.
- Media playlist (`media.m3u8`): serves the renderer's cached body
  verbatim. The renderer has already baked a playlist-scoped short-lived
  (`TypeSegment`) token into every emitted URI, so no per-request
  substitution (and no per-viewer per-fetch allocation) is required.

### `pkg/stream/playlist.go`
- The media-playlist renderer consults a per-stream `PlaylistTokenMinter`
  interface and bakes a token into every emitted URI as `?vt=<token>`.
  `InitToken(nowMs)` fires once per render; `SegmentToken(segmentTsMs)`
  fires once per segment in the window. Per-URI minting (rather than one
  token reused across the playlist) lets the minter return tokens that
  are a pure function of the URI's identity, so a segment's URL is
  byte-identical across renders — required by HLS clients (RFC 8216
  §6.2.2). When the minter is nil (stream without a viewer key) or a
  method returns `""`, the corresponding URI is emitted plain (fail-closed
  via the middleware). This replaces the earlier `{VT}` placeholder +
  serve-time substitution scheme — the playlist is now a fully-formed
  artifact that can be served verbatim. The cached playlist remains
  per-stream (not per-viewer).

### `pkg/stream/stream.go`
- Added the `PlaylistTokenMinter` interface (`SegmentToken(tsMs int64) string`
  + `InitToken(nowMs int64) string`), a `mintToken PlaylistTokenMinter`
  field on `Stream`, and an `InitOption` / `WithMintToken(m)`
  functional-options pattern on `Store.Init`. The option is applied before
  the renderer goroutine is launched, so the renderer observes a
  fully-configured Stream without additional locking. Streams without a
  viewer key are initialized with no options and run identically to the
  pre-viewer-token path.

### `pkg/routes/api.go` (playlist-token plumbing)
- Added `PlaylistTokenTTL = 10 * time.Minute`. The `/init` handler builds
  a `playlistTokenMinter` struct when the stream has configured viewer
  keys, capturing the **segment-derived** key, logger, and streamID.
  Both methods mint a segment-class token so the playlist middleware
  refuses it (the playlist route only tries the playlist-derived key).
  - `SegmentToken(tsMs)` sets `exp = tsMs + PlaylistTokenTTL`; since the
    input is intrinsic to the segment, the token for a given segment is
    deterministic and thus stable across playlist renders — the HLS
    URI-stability requirement.
  - `InitToken(nowMs)` buckets the expiry to the current hour (exp =
    hour_bucket_end + PlaylistTokenTTL), so EXT-X-MAP's URI is stable
    within each hour rather than flipping every render. The init
    segment has no natural per-URI anchor; hourly rotation is a
    reasonable cap on URL-scraping replay for that URI.
  The minter is passed to `store.Init` via `stream.WithMintToken(...)`.
  Streams without keys receive no option, preserving fully-public
  playback. The operator-facing `POST /viewer_token` endpoint uses the
  **playlist-derived** key, producing tokens accepted on every stream
  route.

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
- `pkg/stream/playlist_vt_test.go` — asserts the renderer calls `InitToken`
  once per render and `SegmentToken` once per segment in the window with
  the correct arguments (nowMs / segment timestamp); asserts that a
  deterministic minter yields byte-identical segment URIs across
  renders; asserts an empty return per URI fails closed to plain URIs;
  asserts streams with no minter emit no `?vt=`.
- `pkg/routes/viewer_e2e_test.go` — full flow: mint → master → media → init
  → segment with `vt` threaded through; asserts each baked token verifies
  under the segment-derived key and fails under the playlist-derived key;
  asserts init and segment tokens differ (derived from different inputs);
  `TestE2E_ViewerToken_SegmentURIsStableAcrossRenders` locks in the
  URI-stability regression (RFC 8216 §6.2.2); post-expiry rejection test.

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
7. **Playlist URL scraping** — the playlist embeds short-lived
   (`PlaylistTokenTTL = 10m`) tokens rather than the viewer's long-lived
   one, bounding the replay window of URLs scraped from a playlist body
   while keeping the cached playlist per-stream (not per-viewer). Each
   segment URI's token expires ~10 minutes after that segment's own
   timestamp (per-URI anchor), not 10 minutes after the enclosing
   render; the init URI's token is hour-bucketed (exp =
   bucket_end + 10m). Tokens are deterministic per URI so segment URLs
   don't churn across renders (HLS URI-stability requirement). If a
   stream stops producing segments for longer than ~10 minutes, scraped
   segment URLs for segments already past their TTL will 401 — same
   bound as the prior design, just applied per-segment.
8. **Token rotation via playlist refetch** — without route scoping, a
   client holding any valid token could refetch `media.m3u8` to harvest a
   freshly-minted token and repeat indefinitely, making the 10-minute TTL
   ineffective. Mitigation: the renderer bakes segment-class tokens
   (signed under the segment-derived key), and the playlist route group
   tries only the playlist-derived key. The class is bound into the
   signing key by the KDF, so a segment-class token cannot be re-typed to
   a playlist-class token without the raw env secret — not just without
   the payload type byte.

9. **Wire-format change vs old binaries** — the v1 wire format changed
   from 22 bytes (with payload type byte) to 21 bytes (without). Tokens
   minted by an older binary decode to 22 bytes and fail the length check
   in Verify. streamloom is fully in-memory so a restart already wipes
   all streams and tokens; no parallel-verify period is needed. This
   does mean a rolling deployment that terminates mid-update will serve
   401s for tokens crossing the version boundary — same operational
   envelope as any other state-resetting change.
