# Playlist Live-Edge Look-Ahead

## Context

Viewers are stuttering on live streams. The root cause is that the media
playlist is currently truncated at wall-clock now: `renderMediaPlaylist`
in `pkg/stream/playlist.go:30-44` keeps only segments with
`Timestamp <= nowMs`, so the last listed segment has
`EXT-X-PROGRAM-DATE-TIME ≈ now`.

HLS clients (RFC 8216 §6.3.3) start playback "at least three target
durations from the end" of a live playlist. Clients that also use
`EXT-X-PROGRAM-DATE-TIME` to align playback with wall clock then chase
two anchors — "play at now" (PDT) vs "stay 3 behind the end" (buffering) —
which conflict and cause rebuffer / hunting.

The transcoder already pushes segments with `timestamp >= now`
(`pkg/stream/stream.go:350`), so the server holds bytes for segments whose
PDT is in the near future; we just choose not to advertise them. Two
ingest realities constrain what we can safely publish:

1. **Transcoder look-ahead is unbounded.** An encoder may run minutes
   ahead of wall clock. Publishing every committed segment would produce
   an unbounded playlist.
2. **Segments can arrive out of Index order.** `CommitSlot`
   (`pkg/stream/stream.go:362-399`) inserts by binary search over
   `Index`, so index 7 may land before index 6. HLS (RFC 8216 §6.2.1)
   requires published segments to stay at the same position and new
   segments to append only — so we cannot advertise 7 while 6 is still
   missing and then insert 6 later.

This project moves the playlist tail ahead of wall clock to fix the
stutter, while bounding playlist size and honoring the append-only
invariant.

## Key Design Decisions

- **Look-ahead is duration-based, not segment-count-based.** The cap is
  expressed as a target-duration multiplier (`maxLookaheadMs ≈ 3 ×
  targetDurationMs`). This naturally tracks segment duration and matches
  the form of `EXT-X-SERVER-CONTROL:HOLD-BACK`.
- **Sliding window still caps playlist length.** `DefaultMediaWindowSize
  = 12` (`pkg/config/const.go:25`) stays. Look-ahead controls *where*
  the tail sits; window controls *how long* the list is.
- **Contiguity is enforced in the renderer, not on commit.** `CommitSlot`
  keeps its current behavior (insert out-of-order is fine, index ordering
  invariant stays). The renderer decides what is safe to publish. This
  keeps the ingest path unchanged and makes the fix observable at the
  HTTP surface.
- **`HOLD-BACK` is emitted explicitly.** Once the tail sits ahead of now,
  we tell clients the intended latency target via
  `EXT-X-SERVER-CONTROL:HOLD-BACK=<maxLookaheadSecs>` so their start
  position doesn't rely on the "3 × target-duration" heuristic alone.
- **Backward buffer is unchanged.** `evictOldLocked`
  (`pkg/stream/stream.go:271-303`) already keys off `Timestamp < now`
  and is orthogonal to playlist visibility.

## Overview of Changes

### 1. Look-ahead cap (`pkg/stream/playlist.go`)

Replace the `Timestamp > nowMs` binary-search predicate with
`Timestamp > nowMs + maxLookaheadMs`:

```go
cutoff := nowMs + s.maxLookaheadMs
eligible := sort.Search(len(s.segments), func(i int) bool {
    return s.segments[i].Timestamp > cutoff
})
```

`nextEligibleMs` keeps the same shape (timestamp of first
non-eligible segment), it just reflects the shifted cutoff. The renderer
timer plumbing in `runPlaylistRenderer` (`pkg/stream/playlist.go:109-172`)
stays as-is.

### 2. Contiguity gate (`pkg/stream/playlist.go`)

After the look-ahead cap and the sliding-window trim, walk the window
forward and truncate at the first index gap:

```go
for i := 1; i < len(window); i++ {
    if window[i].Index != window[i-1].Index+1 {
        window = window[:i]
        break
    }
}
```

If the leading segment of the window is not the next expected index
after what was already published, we still emit it — that's a fresh
render, not a mid-playlist insertion — so the gate only protects against
gaps *within* the current render. Tests cover the case where an earlier
render listed `[..., 5]` and the next render would otherwise leap to
`[..., 5, 7]`; the contiguity gate keeps it at `[..., 5]` until 6 arrives.

### 3. Per-stream configuration (`pkg/stream/stream.go`, `pkg/config/const.go`)

Add `maxLookaheadMs int64` to `Stream` and to `Store.Init` alongside
`backwardBufferSize` / `playlistWindowSize`. Default via a new
`DefaultMaxLookaheadMultiplier = 3` in `pkg/config/const.go`, computed
against the stream's `TargetDurationSecs` at Init time. Optional header
`X-SL-MAX-LOOKAHEAD-MS` on `/init` (consistent with the existing
`X-SL-BACKWARD-BUFFER-SIZE` pattern).

### 4. `EXT-X-SERVER-CONTROL` emission (`pkg/stream/playlist.go`)

Add one header line right after `EXT-X-INDEPENDENT-SEGMENTS`:

```
#EXT-X-SERVER-CONTROL:HOLD-BACK=<maxLookaheadSecs>
```

with `HOLD-BACK` = `maxLookaheadMs / 1000.0` (min 3 × target-duration per
spec). This is legal HLS v7 and ignored by clients that don't understand
it.

### 5. Optional: truncation metric (`pkg/stream/playlist.go`)

Emit a counter / log line when the contiguity gate actually truncates
the window. Persistent truncation indicates ingest reordering or loss —
worth surfacing rather than silently masking. Deferred to a follow-up if
it adds friction to the initial change.

## Implementation Order

Each step keeps the test suite green.

1. **Plumb `maxLookaheadMs` through `Stream` + `Store.Init`.** Default to
   `3 × targetDurationSecs × 1000` so existing callers/tests observe
   identical-shape playlists when the filter logic is updated.
2. **Swap the renderer filter** from `Timestamp > now` to
   `Timestamp > now + maxLookaheadMs`. Update
   `TestRenderMediaPlaylist_WallClockFiltering` and
   `TestRenderMediaPlaylist_NextEligibleMs` to reflect the shifted
   cutoff. Add a test that a future-PDT segment within the cap appears
   immediately, and one that a segment beyond the cap does not.
3. **Add the contiguity gate.** New tests: out-of-order arrival
   `[5,7]` → playlist ends at 5; after 6 arrives → playlist extends to
   7; gap entirely before the window → gate is a no-op.
4. **Emit `EXT-X-SERVER-CONTROL:HOLD-BACK`.** Update header assertions
   in `playlist_test.go`.
5. **Expose `X-SL-MAX-LOOKAHEAD-MS` on `/init`.** Validate
   (`>= targetDurationMs`, `<= reasonable ceiling`), thread through
   `store.Init`, add route tests.
6. **End-to-end test.** Push segments with timestamps spanning several
   target durations into the future, poll the playlist, verify tail PDT
   is `~now + maxLookaheadMs`, verify `HOLD-BACK` header, verify
   contiguity gate under induced reordering.

## Edge Cases & Risks

1. **`maxLookaheadMs = 0`.** Degenerates to current behavior (tail at
   `now`). Accepted as a valid (if not recommended) configuration;
   documented.
2. **Transcoder doesn't push ahead.** If ingest only pushes at PDT≈now,
   there are no future segments to expose and the playlist looks like
   today's. The fix is harmless in that regime; the client anchor
   conflict only reappears if look-ahead is also zero.
3. **Contiguity gate truncation masks ingest bugs.** Mitigated by the
   optional counter/log in step 5. Without it, persistent gaps would
   appear only as a stale-looking tail — recognizable but not
   self-diagnosing.
4. **First render after startup.** No segments yet → renderer returns
   empty playlist (unchanged). First segment arrives in the future → the
   look-ahead cap lets it in immediately if within
   `maxLookaheadMs`, otherwise the existing `nextEligibleMs` timer wakes
   the renderer when it crosses the cap.
5. **Interaction with discontinuity support** (`plans/discontinuity-
   support.md`). Orthogonal: discontinuity handling walks the window
   emitting `EXT-X-DISCONTINUITY` / per-generation `EXT-X-MAP` tags. The
   look-ahead cap and contiguity gate operate on the same window before
   the discontinuity walk runs. No ordering conflict.
6. **Viewer-token minting cadence** (`pkg/routes/api.go:77-90`). Unchanged.
   `mintToken` still fires once per render; render frequency is driven
   by `notifyCh` and the look-ahead-shifted `nextEligibleMs` timer.
7. **Client compatibility.** `EXT-X-SERVER-CONTROL` is HLS v7 (we
   already advertise `#EXT-X-VERSION:7`). Unknown-tag handling is
   required of compliant clients. No regression expected.

## Files to Modify

| File | Changes |
|------|---------|
| `pkg/stream/playlist.go` | Look-ahead cap, contiguity gate, `EXT-X-SERVER-CONTROL` emission |
| `pkg/stream/stream.go` | `maxLookaheadMs` field, `Store.Init` param, plumbing |
| `pkg/config/const.go` | `DefaultMaxLookaheadMultiplier` (or absolute default) |
| `pkg/routes/api.go` | Parse `X-SL-MAX-LOOKAHEAD-MS` on `/init` |
| `pkg/stream/playlist_test.go` | Update existing filter / nextEligible tests; add look-ahead, contiguity, `HOLD-BACK` header tests |
| `pkg/stream/stream_test.go` | Update `mustInit` helper, add per-stream `maxLookaheadMs` tests |
| `pkg/routes/api_test.go` | Header validation, default behavior |
| `pkg/routes/e2e_test.go` | End-to-end stutter-repro scenario |
| `pkg/routes/helpers_test.go` | Update init helpers |
| `README.md` | Document `X-SL-MAX-LOOKAHEAD-MS` alongside `X-SL-BACKWARD-BUFFER-SIZE` |

## Verification

```bash
go fmt ./...
go fix ./...
go vet ./...
go test ./...
gosec ./...
```

Key test scenarios:

- Future-PDT segment within `maxLookaheadMs` appears in the playlist
  immediately after `CommitSlot`.
- Segment beyond `maxLookaheadMs` does not appear; appears on the next
  render after the cap advances past it.
- Out-of-order commit `[0,1,2,4]` → playlist ends at 2; commit 3 →
  playlist extends to 4.
- 50 segments pushed 5 minutes ahead of now → playlist size equals
  `min(windowSize, maxLookaheadMs/duration)`, tail PDT ≈
  `now + maxLookaheadMs`.
- `EXT-X-SERVER-CONTROL:HOLD-BACK=<maxLookaheadSecs>` present in header
  and matches configured value.
- `X-SL-MAX-LOOKAHEAD-MS` on `/init` accepted within bounds, rejected
  otherwise; unset → default applies.
- Manual: `hls.js` / AVPlayer / ffplay against a live stream, confirm
  PDT-sync'd playback no longer rebuffers on the "3 back" cadence.
