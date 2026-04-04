# Init Segment Eviction

## Context

Init entries (`initEntries map[int64]*InitEntry`) in `pkg/stream/stream.go` previously grew unbounded. This project adds eviction of stale init entries during `CommitSlot`.

## Eviction Rule

An init entry is evicted when:
1. Its generation < `currentGeneration`
2. No buffered segment references that generation

Once both conditions are met, the init can never be used again — stale-generation segments are rejected by `CommitSlot`, and no playlist will reference the init.

## Changes

### `pkg/stream/stream.go`
- Added `evictStaleInitEntriesLocked()` — scans segments to build active generation set, deletes unreferenced stale inits
- Called from `CommitSlot` after `dropStaleGenerationLocked()` (already under write lock)
- Added `RunWithInitEntry(generation, fn)` — atomic init lookup + callback, fixing a TOCTOU race in the serving handler

### `pkg/routes/stream.go`
- Init serving handler rewritten to use `RunWithInitEntry` instead of 3 separate lock-protected calls

### Tests
- 5 eviction scenarios in `stream_test.go`: basic, retained-while-segments-exist, current-gen-retained, multi-gen, single-gen-noop
- 2 `RunWithInitEntry` tests: success and not-found
- 1 route test: evicted generation returns 404

## Risks Considered

- **TOCTOU in init serving**: Fixed by `RunWithInitEntry`
- **Client 404 on evicted init**: Same as segment eviction — normal for live HLS
- **Concurrency**: Eviction under write lock, safe by design
