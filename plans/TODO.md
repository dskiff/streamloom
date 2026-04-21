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

- [ ] Add `maxLookaheadMs` to `Stream` + `Store.Init` (threaded through
      `pkg/stream/stream.go` and call sites). Default to
      `3 × TargetDurationSecs × 1000` via a new
      `DefaultMaxLookaheadMultiplier` in `pkg/config/const.go`. Update
      test helpers to pass through.
      *Validation: existing tests green; new unit test confirms the
      default value on an Init with no override.*

- [ ] Swap `renderMediaPlaylist` filter from `Timestamp > nowMs` to
      `Timestamp > nowMs + maxLookaheadMs`. Update
      `TestRenderMediaPlaylist_WallClockFiltering` and
      `TestRenderMediaPlaylist_NextEligibleMs`. Add tests: future-PDT
      segment within cap appears immediately; segment beyond cap does
      not; `nextEligibleMs` reflects the shifted cutoff.
      *Validation: stutter-repro scenario (tail PDT ≈ now + cap) in a
      unit test.*

- [ ] Add contiguity gate to `renderMediaPlaylist` (truncate at first
      index gap within the window). Tests: `[0,1,2,4]` → playlist ends
      at 2; after 4→3 arrival → extends to 4; gap entirely before the
      window is a no-op.
      *Validation: out-of-order commit test passes; append-only
      invariant held across successive renders.*

- [ ] Emit `EXT-X-SERVER-CONTROL:HOLD-BACK=<maxLookaheadSecs>` in the
      playlist header. Update `playlist_test.go` header assertions.
      *Validation: header present, value matches configured
      `maxLookaheadMs`.*

- [ ] Accept `X-SL-MAX-LOOKAHEAD-MS` on `/init` in `pkg/routes/api.go`.
      Validate `>= TargetDurationMs` and `<= reasonable ceiling`. Thread
      into `Store.Init`. Route tests for accept / reject / default.
      *Validation: route tests cover bounds; README documents the
      header alongside `X-SL-BACKWARD-BUFFER-SIZE`.*

- [ ] End-to-end test in `pkg/routes/e2e_test.go`: push segments with
      PDTs spanning several target durations into the future, poll
      playlist, verify tail PDT ≈ `now + maxLookaheadMs`, verify
      `HOLD-BACK` header, induce reordering and verify contiguity gate
      holds.
      *Validation: pre-commit gauntlet green
      (`go fmt && go vet && go test && gosec`).*

- [ ] (Optional / follow-up) Metric or log line when contiguity gate
      truncates the window, to surface ingest reordering rather than
      silently masking it.
