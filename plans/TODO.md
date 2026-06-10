# TODO

Task list for in-flight streamloom work. Completed tasks are kept briefly
for context, then pruned once a successor task exists or the work is well
past.

## X-SL-DURATION sanity bound

`X-SL-DURATION` was parsed as a uint32 (up to ~49.7 days) and only
rejected for zero. An absurd duration inflates the published tail's
`EndMs`, so the master-baked sticky `EXT-X-START` offset
(`FreshOffsetSecs`) can exceed `maxStickyOffsetSecs` — the media handler
then rejects its own `?to=` value and silently drops the tag; an
oversized `EXTINF` also dwarfs `EXT-X-TARGETDURATION` (RFC 8216). All
tasks **complete**.

- [x] Add `config.MaxSegmentDurationMs` (`60_000` ms / 60s). The value is
      chosen so the worst-case tail-to-now gap (`MaxLookaheadCeilingMs +
      MaxSegmentDurationMs = 3_660_000 ms`) lands exactly at
      `maxStickyOffsetSecs`, guaranteeing the sticky offset stays in range
      in every configuration while still catching unit confusion.
- [x] Reject `X-SL-DURATION > MaxSegmentDurationMs` with `400` in the
      segment handler (mirrors the `X-SL-MAX-LOOKAHEAD-MS` ceiling check).
- [x] Tests `TestPostSegment_DurationExceedsMax` (over-cap → 400) and
      `TestPostSegment_DurationAtMaxAccepted` (at-cap → 201). README
      `X-SL-DURATION` row documents the bound. Pre-commit: `go fmt / fix /
      vet / test` green; `gosec` clean for the change.

## media.m3u8 long-poll: single, unambiguous timeout response

`mediaPlaylistHandler` blocks pre-live until a playlist renders, the
stream is deleted, or the request ends. The router applied chi's
`middleware.Timeout` globally, so when the deadline expired BOTH the
handler (503 + Retry-After on `r.Context().Done()`) and the middleware's
`defer` (504) called `WriteHeader` — two competing intents. The second
write happens to be swallowed today by chi's `WrapResponseWriter` (added
by `requestLogMiddleware`, which sits ahead of `Timeout` in the chain), so
no superfluous-WriteHeader line actually surfaces and the client gets the
503 — but the redundant 504 is dead code that depends on that middleware
ordering, and the handler didn't distinguish a deadline from a client
disconnect. All tasks **complete**.

- [x] `Stream`: apply `middleware.Timeout` per-route instead of globally,
      carving out `/stream/{streamID}/media.m3u8`. The other routes
      (`/healthz`, `stream.m3u8`, `init.mp4`, `segment_*.m4s`) keep it via
      `r.With`/`r.Use`; only the long-poll opts out.
- [x] `mediaPlaylistHandler`: bound the wait with a handler-owned
      `context.WithTimeout(r.Context(), config.REQUEST_TIMEOUT)`. On
      `DeadlineExceeded` write the same 503 + Retry-After as the other
      not-ready paths; on `context.Canceled` (client gone) write nothing.
      Now the sole writer of the response — no 504 contention.
- [x] Tests: `TestMediaPlaylist_WaitOutcome_SoleWriter` (deadline →
      exactly one `WriteHeader(503)`; cancellation → no write, via a
      `writeHeaderProbe` on a bare router that omits the log middleware so
      a double write can't hide); `TestMediaPlaylist_PreLiveEdgeReturns503`
      unchanged (client still sees 503); renamed
      `TestMediaPlaylist_ClientCancellationWritesNothing` asserts the
      client-gone path writes nothing. `go fmt/vet/test` green, `-race`
      clean; `gosec` unchanged (the two pre-existing G705 taint findings on
      `stream.go`/`api.go` are untouched).
## Unbounded future timestamp: renderer busy-loop (CPU DoS)

Close the unbounded `X-SL-TIMESTAMP` upper bound that could wedge the
playlist renderer in a 100%-CPU busy loop. A timestamp ~292–584 years out
made `time.Duration(sleepMs) * time.Millisecond` overflow int64 nanoseconds
to a negative value, so `timer.Reset` fired immediately and re-rendered in a
tight loop (re-acquiring the stream RLock each pass); the future segment
never became "backward," so eviction never removed it. A transcoder unit bug
(seconds/µs/ns vs ms) is the likely source. All tasks **complete**.

- [x] `CommitSlot` rejects timestamps more than `stream.MaxFutureTimestampMs`
      (2h = look-ahead ceiling + 1h slack) ahead of the stream clock with the
      new `ErrTimestampTooFarInFuture` sentinel — no first-segment exception.
      The push handler maps it to `422 Unprocessable Entity`, mirroring
      `ErrTimestampInPast`. Tests: `TestRejectFarFutureTimestamp{,UnitBug}`,
      `TestAllowTimestampAtFutureHorizon`, `TestPostSegment_TimestampTooFarInFuture`.
- [x] Defense-in-depth: the renderer's sleep computation is clamped via
      `sleepForMs` at `maxRenderSleep` (24h) so the ms→ns multiply can never
      overflow regardless of how a timestamp reached the buffer. Tests:
      `TestSleepForMs_{ClampsOverflowingDelay,PassesThroughNormalDelay,Boundary}`.
- [x] README documents the accepted `X-SL-TIMESTAMP` window and the 422
      rejection. Pre-commit `go fmt / vet / test` green; `gosec` unchanged.

## IPv6 SL_BIND_ADDR can't boot the server

The config layer validates and accepts IPv6 bind addresses
(`parseBindAddr` blesses `::1`, `TestParseBindAddrValidIPv6`), but
`main.go` built listen addresses with `fmt.Sprintf("%s:%d", addr, port)`.
For `SL_BIND_ADDR=::1` that produced `"::1:8080"`, which `net.Listen`
rejects with "too many colons in address" → `os.Exit(1)`: an
accepted-config-that-can't-run bug. All tasks **complete**.

- [x] Add `listenAddr(host, port)` in `main.go` using
      `net.JoinHostPort(host, strconv.Itoa(port))`, which brackets IPv6
      hosts (`::1` → `[::1]:8080`) and leaves IPv4 / empty-host
      (all-interfaces `:<port>`) output unchanged. Both stream- and
      api-address call sites now use it; `fmt` import dropped.
- [x] Tests in `main_test.go`: `TestListenAddr` (table: ipv4, ipv6
      loopback/unspecified/full, empty host) pins the bracketing
      deterministically; `TestListenAddr_AcceptedByNetListen` boots a real
      ephemeral listener per blessed host (IPv6 subtest skips where the
      sandbox lacks IPv6 loopback). Pre-commit: `go fmt / fix / vet / test`
      green; `gosec` clean for the change (the two pre-existing G705 taint
      findings on `stream.go`/`api.go` are unchanged by this work).

## Active-watcher map: memory-DoS hardening

Close the unbounded / spoofable active-watcher map (memory DoS). All tasks
**complete**.

- [x] Cap per-stream tracked IPs at `watcher.MaxIPsPerStream` (100k). At
      capacity, previously-unseen IPs are dropped while tracked IPs still
      refresh, so counting degrades gracefully instead of exhausting memory.
      Tests: `TestRecord_CapsDistinctIPsPerStream`, `TestRecord_CapIsPerStream`.
- [x] Bound the stream dimension: `mw.RecordWatcher` now takes a
      `streamExists` predicate and records only for streams the store holds
      (wired via `store.Get` in `routes.Stream`). Without it, arbitrary
      `{streamID}` path values could mint unbounded tracker streams. Tests:
      `TestRecordWatcher_{RecordsWhenStreamExists,SkipsWhenStreamMissing}`,
      `TestActiveWatchers_UninitializedStreamNotRecorded`.
- [x] Fix client-IP trust in `mw.TrustedRealIP`: never honor `True-Client-IP`
      / `X-Real-IP` (stripped on every path); on the trusted path derive the
      client from the **rightmost non-trusted** `X-Forwarded-For` entry
      instead of chi's spoofable leftmost. Dropped the `chi/middleware.RealIP`
      dependency. Tests: `TestTrustedRealIP_*`, `TestRightmostUntrustedIP`,
      and end-to-end `TestActiveWatchers_{TrueClientIPSpoofDoesNotInflate,
      ForgedForwardedPrefixDoesNotInflate,DistinctForwardedClientsCounted}`.
- [x] README: new "Client IP resolution" section documents the trust model
      and required reverse-proxy header hygiene (strip inbound
      `True-Client-IP`/`X-Real-IP`; append/overwrite `X-Forwarded-For`), with
      a Caddy example. Updated the `SL_TRUSTED_PROXIES` row.
- [x] Pre-commit: `go fmt / vet / test` green. `gosec` clean for the change
      (the two pre-existing G705 taint findings on `stream.go`/`api.go` are
      unchanged by this work).

## HMAC key derivation (stream + type → derived key)

All tasks below are **complete**. Kept here until the change lands on
`main` so anyone reading the branch has context.

- [x] `viewer.DeriveKey(envKey, streamID, typ)` HMAC-SHA256 PRF with
      domain-separated context. New `ErrUnknownType` sentinel.
- [x] Strip type byte from `Mint` / `Verify`; token payload is now 21
      bytes / 28 chars encoded.
- [x] Rename `viewer.TypeViewer` → `viewer.TypePlaylist` with godoc
      noting the name vs route-set distinction.
- [x] `config.ViewerKeys` struct holding pre-derived `Playlist` and
      `Segment` keys; raw env secret never escapes
      `parseStreamViewerTokenKeys`.
- [x] `config.Env.GetViewerKeys(streamID)` replaces `GetViewerTokenKey`.
- [x] Middleware iterates `allowedTypes`, verifies under each class's
      derived key, first match authorizes. Callers pass
      most-expected-first.
- [x] `pkg/routes/stream.go` route wire-up: playlist group passes
      `TypePlaylist`; init/segment group passes `TypeSegment,
      TypePlaylist` (segment first, hot path).
- [x] `pkg/routes/api.go` mint sites: `POST /viewer_token` uses
      `keys.Playlist`, `makePlaylistTokenMinter` uses `keys.Segment`.
- [x] All test files updated: new `testViewerKeys(streamID)` helper,
      `mintPlaylistVT` / `mintSegmentVT`, KDF-isolation assertions
      replace type-byte tamper tests.
- [x] `plans/viewer-tokens.md` & `README.md` describe key derivation
      and revised scoping model.
- [x] Pre-commit: `go fmt ./... && go vet ./... && go test ./... && gosec ./...`.

## Playlist live-edge look-ahead

See `plans/playlist-lookahead.md` for full context and design. Goal: move
the playlist tail ahead of wall clock so PDT-sync'd clients don't fight
the "3 segments behind end" heuristic, while bounding playlist size and
preserving HLS's append-only invariant under out-of-order ingest.

- [x] Add `maxLookaheadMs` to `Stream` + `Store.Init` (threaded through
      `pkg/stream/stream.go` and call sites). Default to
      `3 × TargetDurationSecs × 1000` at the `/init` handler via a new
      `DefaultMaxLookaheadMultiplier` in `pkg/config/const.go`. Unit
      tests pass `0` for the legacy "pin tail at now" baseline; route
      tests exercise the default.

- [x] Swap `renderMediaPlaylist` filter from `Timestamp > nowMs` to
      `Timestamp > nowMs + s.maxLookaheadMs`. New
      `TestRenderMediaPlaylist_Lookahead*` tests cover future-within-cap
      inclusion, beyond-cap exclusion, stutter-repro tail, and
      `maxLookaheadMs=0` degenerate behavior. Route-level filtering
      tests updated for the shifted cutoff.

- [x] Contiguity gate in `renderMediaPlaylist`: truncate window at the
      first index gap. `TestRenderMediaPlaylist_ContiguityGate_*` and
      `TestE2E_LookaheadContiguityUnderReordering` cover out-of-order
      commit, gap-fill, and pre-window gap no-op scenarios.

- [x] `EXT-X-SERVER-CONTROL:HOLD-BACK=<secs>` emitted right after
      `EXT-X-INDEPENDENT-SEGMENTS`. Clamped up to `3 × target-duration`
      per RFC 8216 §4.4.3.8. Tests:
      `TestRenderMediaPlaylist_HoldBack{MatchesLookahead,
      ClampedToSpecMinimum, HeaderOrder}`.

- [x] `X-SL-MAX-LOOKAHEAD-MS` parsed on `/init`. Validated: non-negative,
      `0` accepted (legacy), otherwise `>= target-duration-ms`, `<=
      MaxLookaheadCeilingMs` (1 hour). Threaded into `store.Init`.
      `TestPostInit_MaxLookahead*` covers accept / default / rejections.

- [x] End-to-end test in `pkg/routes/e2e_test.go`:
      `TestE2E_LookaheadLiveEdge` pushes segments spanning several
      target durations ahead, verifies tail PDT ≈ `now + cap` and the
      `HOLD-BACK` header value.
      `TestE2E_LookaheadContiguityUnderReordering` covers the
      index-reorder scenario.
      Pre-commit: `go fmt / vet / test` green. `gosec` not available in
      this environment (run via devbox locally).

- [ ] (Optional / follow-up) Metric or log line when the contiguity
      gate truncates the window, to surface ingest reordering rather
      than silently masking it.

## Gate segment fetches to the published look-ahead

Viewer tokens authorize a time window, not a specific segment (the token
payload is `[version][expiry][MAC]`), and the segment route serves any
buffered index. A transcoder that pushes a batch ahead of wall clock leaves
segments in the buffer that the renderer has not advertised yet (timestamp
beyond `now + maxLookaheadMs`); a token holder could enumerate
`segment_<N+k>.m4s` and read ahead of the live edge. Gating the fetch to the
same cutoff the renderer publishes by shrinks that surface and matches HLS
semantics (clients only fetch advertised segments). The cutoff advances
monotonically with wall clock, so any segment that ever appeared in a
playlist stays fetchable — well-behaved clients are never refused.

- [x] `Stream.RunWithPublishedSegmentSlot` (the only public segment
      accessor) gates on `Timestamp <= now + maxLookaheadMs`, returning the
      new `ErrSegmentNotYetPublished` sentinel otherwise. The ungated raw
      read survives only as the unexported `runWithSegmentSlot(index, gate,
      fn)` helper for white-box tests, so no exported path bypasses the gate.
- [x] Public segment handler (`pkg/routes/stream.go`) uses the gated accessor
      and collapses `ErrSegmentNotYetPublished` to the same 404 as
      `ErrSegmentNotFound` so a beyond-cap segment is indistinguishable from
      a missing one (no read-ahead enumeration signal). The refusal is logged
      at warn (matching the viewer-token rejection) so a compliant-client-only
      event is visible to operators / fail2ban.
- [x] Tests: `pkg/stream/stream_test.go` covers raw-vs-gated divergence and
      the segment becoming fetchable once the clock advances;
      `pkg/routes/stream_test.go` covers the 404 → 200 transition at the
      HTTP surface. Pre-commit: `go fmt / vet / test`; `gosec` run locally.

## Cross-device player synchronization

Goal: two viewers on separate devices joining at different times should see
the same content at the same wall-clock instant, with the active segment's
PDT close to wall time.

- [x] Emit `#EXT-X-START:TIME-OFFSET=-<holdBackSecs>,PRECISE=YES` after
      `EXT-X-SERVER-CONTROL`, before `EXT-X-TARGETDURATION`. TIME-OFFSET is
      tied to the existing `holdBackSecs` (clamped min `3 × target-
      duration`) so the two server hints always agree. `PRECISE=YES`
      eliminates segment-boundary snap jitter (RFC 8216 §4.4.5.2). New
      tests: `TestRenderMediaPlaylist_StartOffset_{MatchesHoldBack,
      ClampedToSpecMinimum, HeaderOrder, PreciseAttr}`. E2E coverage
      extended in `TestE2E_LookaheadLiveEdge`.

- [x] Dynamic `EXT-X-START` per request. Static `TIME-OFFSET` left up to
      one target-duration of within-segment drift between viewers joining
      during the same cached playlist. The renderer stores a
      `PlaylistSnapshot` with body split around the EXT-X-START line; the
      HTTP handler synthesizes `TIME-OFFSET = -(EndMs − nowMs)/1000` per
      request, clamped at `MinHoldBack` = 3 × target-duration to keep the
      tag negative and spec-compliant in the degenerate case where the
      tail sits at or behind wall-clock now. The invariant: each viewer's
      start content PDT equals their own wall-clock now, so two staggered
      viewers diverge in start PDT by exactly their wall-clock gap and
      play the same content at every shared wall time. Drift cancellation
      engages whenever the tail-to-now gap beats the floor, which is the
      normal case at any configured `maxLookaheadMs` (default
      `3 × targetDuration` keeps roughly one target-duration of room
      above the floor at render time). New types: `PlaylistSnapshot` in
      `pkg/stream/playlist.go`. New tests: `TestPlaylistSnapshot_*`,
      `TestMediaPlaylist_StartOffset_*`, `TestE2E_StartOffsetTracksWallClock`.

- [ ] (Future, larger effort) Low-Latency HLS: emit `EXT-X-PART-INF`,
      per-part `EXT-X-PART`, `CAN-BLOCK-RELOAD=YES`, `PART-HOLD-BACK`,
      and support `_HLS_msn` / `_HLS_part` blocking playlist reload.
      Required for sub-second cross-device drift; spans ingest, storage,
      renderer, and routes. Out of scope until PDT-anchor + hold-back
      convergence proves insufficient in the field.

## Per-stream playlist window size

- [x] Make playlist window size configurable per stream. New
      `X-SL-PLAYLIST-WINDOW-SIZE` header on `/init`, optional, defaults
      to `DefaultMediaWindowSize` (12). Bound: `0 < windowSize <=
      backwardBufferSize` so the published window stays inside the
      retained-backward eviction guarantee. Enforced both at the API
      boundary (clear 400 with header context) and in `Store.Init`
      (`ErrInvalidPlaylistWindowSize`). Field stored on `*Stream` and
      exposed via `PlaylistWindowSize()` to mirror `MaxLookaheadMs()`;
      renderer reads from the struct rather than receiving it as a
      goroutine arg. Tests:
      `TestPostInit_PlaylistWindowSize{Default,Override,
      ZeroRejected,NegativeRejected,UnparseableRejected,
      AboveBackwardBufferRejected,DefaultExceedsBackwardBufferRejected}`,
      `TestInitRejectsPlaylistWindowSizeAboveBackwardBuffer`,
      `TestInitStoresPlaylistWindowSize`. README updated with the
      header and the `X-SL-BACKWARD-BUFFER-SIZE` interaction.

## Cap playlist window span to keep baked tokens valid

Large playlist windows advertised segments whose baked `?vt=` segment-class
tokens had already expired. A segment's token expires at
`seg.Timestamp + PlaylistTokenTTL` (~10m, minute-truncated), but a segment
stays advertised for roughly `windowSize × targetDuration − lookahead` after
its timestamp. Once that span approached the TTL (e.g. window 60 at a 10s
target → 600s, or 274 at 2s → 548s — both previously passed init validation,
whose only window bound was `windowSize <= backwardBufferSize`), the older
part of every freshly served playlist `401`'d on fetch for viewer-token
streams while the playlist itself still served `200`. The README's "segments
roll out of the window (tens of seconds) long before their tokens expire"
held only for small windows. All tasks **complete**.

- [x] `stream.MaxPlaylistWindowSpan` (5m, documented to stay strictly below
      `routes.PlaylistTokenTTL` with headroom for the token's minute
      truncation and clock skew) plus `maxPlaylistWindowSpanSecs`. New
      `ErrPlaylistWindowSpanTooLong`. `Store.Init` rejects when
      `playlistWindowSize × meta.TargetDurationSecs` exceeds the cap
      (compared via division to avoid int64 overflow on hostile inputs).
- [x] `/init` handler surfaces the same bound for a clearer 400 with header
      context (target-duration + cap), mirroring the existing
      backward-buffer surface check.
- [x] Tests: `TestInitRejectsPlaylistWindowSpanTooLong` (stream) covers the
      two issue scenarios, one-second-over, and the exact-cap accept;
      `TestPostInit_PlaylistWindowSpan{TooLongRejected,AtLimitAccepted}`
      (routes) cover the HTTP surface; `TestPlaylistWindowSpanBelowTokenTTL`
      guards the cross-package `MaxPlaylistWindowSpan < PlaylistTokenTTL`
      coupling so a future change to either constant can't silently
      reintroduce the bug.
- [x] README stale-playlist edge case + `X-SL-PLAYLIST-WINDOW-SIZE` row
      document the 5-minute span cap; `plans/viewer-tokens.md` design
      record notes it. Pre-commit: `go fmt / vet / test` green; `gosec` run
      where available.

## Stale-generation drop: no panic with active readers

Security-review fix: `dropStaleGenerationLocked` panicked when a dropped
stale-generation segment still had an in-flight reader. The plain-string
panic unwound `CommitSlot` mid-mutation under `s.mu` (generation already
advanced, slice partially compacted), leaked the new segment's buffer
(handler only releases on error returns), and `middleware.Recoverer`
turned it into a 500 — leaving the stream serving from corrupted state.
Reachable by any viewer holding a read on a segment that a generation
advance drops (the read path serves any committed index).

- [x] `dropStaleGenerationLocked`: replace the panic. A reader-held stale
      segment is still removed from the list (not servable or
      playlist-visible, index immediately reusable by the new
      generation), but its buffer is parked on a new `Stream.pendingFree`
      list instead of being returned to the pool.
- [x] `sweepPendingFreeLocked`: returns parked buffers with a drained
      reader count to the pool. Runs under the write lock on every
      `AcquireSlot` and `CommitSlot`, so the pool recovers as soon as
      readers finish. Write lock makes the `Readers() == 0` check stable:
      readers attach only under `RLock` and parked buffers are
      unreachable from `segments`.
- [x] Tests: `TestCommitSlot_GenerationAdvance_DefersBufferWithActiveReader`,
      `TestCommitSlot_GenerationAdvance_ReplacesReaderHeldIndex`,
      `TestCommitSlot_PendingFreeNotSweptWhileReaderActive`,
      `TestAcquireSlot_SweepsPendingFreeAfterReadersDrain`,
      `TestConcurrentReadersAndGenerationDrop` (passes `-race`).
