# Deferred Playlist Serving for media.m3u8

## Context

It's been proposed that when `media.m3u8` is requested, the server should always wait until the **next** playlist update before responding. The rationale: if a client already has the current playlist, returning it again is wasteful. By holding the response until fresh data is available, we reduce redundant responses and ensure clients always receive new content.

This concept is well-established in the HLS ecosystem — it's the core idea behind **Blocking Playlist Reload**, formalized in the HLS spec (RFC 8216bis, Section 6.2.5.2) as part of Low-Latency HLS.

## Research Findings

### How the HLS spec handles this (Blocking Playlist Reload)

The HLS spec defines an **opt-in** mechanism:
1. Server advertises `#EXT-X-SERVER-CONTROL:CAN-BLOCK-RELOAD=YES` in the playlist
2. Client requests `media.m3u8?_HLS_msn=N` to say "I already have up to sequence N-1, hold until N is ready"
3. Server defers the response until the playlist contains media sequence N or later
4. Clients that don't send `_HLS_msn` get an immediate (non-deferred) response

This is the industry standard approach used by Apple, AWS MediaPackage, Wowza, and other major HLS implementations.

### Current streamloom behavior

The `media.m3u8` handler (`pkg/routes/stream.go:63-103`):
1. Blocks on `s.HasPlaylist()` until the first valid playlist exists
2. Returns `s.CachedPlaylist()` immediately (atomic pointer load)

The background renderer (`pkg/stream/playlist.go:137-200`) re-renders on segment commits or timer expiry, storing results in an atomic pointer.

### The "always defer" approach vs. spec-compliant approach

| Aspect | Always Defer | Spec-compliant (`_HLS_msn`) |
|--------|-------------|---------------------------|
| Client compatibility | Breaks standard HLS clients that expect immediate responses | Works with all clients; LL-HLS clients opt in |
| First request | Needs special-casing (version-0 optimization) | Natural: no `_HLS_msn` = immediate response |
| Timeout risk | High — target duration may exceed request timeout | Client controls what it asks for |
| CDN compatibility | Incompatible with caching layers | CDN can cache non-blocking requests normally |
| Spec compliance | Violates expected polling model | Follows RFC 8216bis exactly |
| Complexity | Simpler server-side | Slightly more complex (query param parsing) |

## Recommendation: Do Not Implement "Always Defer"

**I recommend against unconditionally deferring all playlist responses.** Here's why:

### 1. Standard HLS clients will break or degrade
HLS players (hls.js, AVPlayer, ExoPlayer) request `media.m3u8` and expect an immediate response. They manage their own reload schedule based on `EXT-X-TARGETDURATION`. If the server holds the response for seconds, players may:
- Hit their own internal request timeouts
- Stall playback waiting for a playlist that's being artificially delayed
- Miscompute their reload timing (they expect round-trip ≈ network latency, not network + hold time)

### 2. The request timeout is too tight
`REQUEST_TIMEOUT` is 10s. `EXT-X-TARGETDURATION` is typically 2-6s. A client requesting the playlist right after a render could wait up to a full target duration for the next update. Combined with network latency and segment download time, this creates a real risk of timeouts — especially under load or with slower encoders.

### 3. The HLS spec solved this problem already
Blocking Playlist Reload via `_HLS_msn` is the standard, battle-tested solution. It's opt-in, so legacy clients work fine. LL-HLS-aware clients (hls.js, AVPlayer) already know how to use it. Implementing a non-standard always-defer mechanism would be reinventing the wheel with worse compatibility.

### 4. "Reducing load" is better solved at other layers
If the concern is reducing redundant requests:
- The current `no-cache` + atomic pointer load is already extremely cheap (~nanosecond read)
- CDNs or reverse proxies can coalesce requests if needed
- Proper `_HLS_msn` support lets LL-HLS clients avoid redundant polls naturally

## Alternative: Implement Spec-Compliant Blocking Playlist Reload

If reducing playlist polling overhead is a goal, the right path is implementing `_HLS_msn` support per the HLS spec. This would be a larger but more valuable project:

1. Add `#EXT-X-SERVER-CONTROL:CAN-BLOCK-RELOAD=YES` to rendered playlists
2. Parse `_HLS_msn` query parameter on `media.m3u8` requests
3. If `_HLS_msn` is present: hold response until playlist contains that media sequence number
4. If absent: return immediately (current behavior, backwards compatible)
5. Optionally: support `_HLS_part` for partial segment blocking (full LL-HLS)

This is a larger scope project that should be evaluated separately as a full LL-HLS initiative.

## Conclusion

The "always defer" approach solves a real problem (redundant playlist responses) but in a way that breaks standard client expectations and is incompatible with the HLS specification's polling model. The HLS ecosystem already has a well-defined, opt-in solution for this exact problem. I recommend **not** implementing unconditional deferral, and instead considering spec-compliant Blocking Playlist Reload (`_HLS_msn`) as a future project if reducing playlist overhead is a priority.
