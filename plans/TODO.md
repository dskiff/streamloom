# TODO

Task list for in-flight streamloom work. Completed tasks are kept briefly
for context, then pruned once a successor task exists or the work is well
past.

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
