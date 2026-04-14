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

## Open

(none)
