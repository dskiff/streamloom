# Segment Discontinuity Support

## Context

Streamloom currently supports generation tracking on segments but not on init data. When an encoder restarts, it needs to push a new `init.mp4` and the HLS playlist must inject `EXT-X-DISCONTINUITY` tags at generation boundaries. This change enables seamless encoder transitions without requiring a full stream re-initialization.

The HLS spec requires:
- `#EXT-X-DISCONTINUITY` before the first segment of a new generation
- `#EXT-X-MAP:URI=...` after the discontinuity to point to the new init segment
- `#EXT-X-DISCONTINUITY-SEQUENCE` header counting discontinuities that scrolled out of the playlist window

## Key Design Decisions

- **All metadata is stream-level and immutable** (Bandwidth, Codecs, Resolution, FrameRate, TargetDuration, SegmentByteCount, capacity params). The `Metadata` type stays as-is. Only init data (the raw init.mp4 bytes) varies per generation.
- **Subsequent `/init` calls only need `X-SL-GENERATION` + body**. Metadata and capacity headers are only required on the first `/init` (stream creation).
- **`currentGeneration` is only bumped by segment commits**, not by `/init`. New init data is not exposed until the first segment for that generation arrives.
- **Init URLs change** from `init-{timestamp}.mp4` to `init_{generation}.mp4`.

## Overview of Changes

### 1. Data Model Changes (`pkg/stream/stream.go`)

**New type**:
```go
// InitEntry holds the init segment data for a single encoding generation.
type InitEntry struct {
    InitData []byte // cloned, immutable after creation
}
```

**`Metadata` type**: Unchanged. Remains stream-level, set once at first init.

**Stream struct changes**:
- Remove: `initData []byte`, `initTimestampMs int64`
- Add: `initEntries map[int64]*InitEntry` (keyed by generation)
- Add eviction-based discontinuity tracking:
  - `evictedDiscontinuities int` (generation transitions among evicted segments)
  - `lastEvictedGeneration int64` (initialized to `-1`; since generations are validated as non-negative, `-1` serves as "no evictions yet" sentinel — no extra bool needed)
- Keep: `metadata Metadata` (unchanged, stream-level)
- Keep: `currentGeneration int64` (bumped only by segment commits)

**Accessor changes**:
- `Metadata()` unchanged (stream-level)
- Remove: `InitTimestampMs()`, `InitDataLen()`, `WriteInitDataTo()`
- Add: `GetInitEntry(gen int64) (*InitEntry, bool)` — returns the init entry for a generation
- Add: `WriteInitDataForGenerationTo(w io.Writer, gen int64) (int, error)` — writes init data under read lock
- Add: `InitDataLenForGeneration(gen int64) int`

### 2. Store.Init Changes (`pkg/stream/stream.go`)

**First init** (stream doesn't exist): Requires all current headers + optional `X-SL-GENERATION`. Creates Stream as today, but stores init data in `initEntries[generation]` instead of `initData` field. Sets `currentGeneration = generation` so that the stream's initial generation matches the init (prevents segments at gen 0 from being accepted when the first init was at gen 5).

**Subsequent init** (stream exists): Only requires `X-SL-GENERATION` + body. New method on Stream:
```go
func (s *Stream) AddInitEntry(generation int64, initData []byte) error
```
- Validates generation is not already in `initEntries` → `ErrDuplicateGeneration`
- Clones and stores init data under stream's lock
- Does NOT touch segments, renderer, buffer pool, or `currentGeneration`

**Store.Init signature change**: Add `generation int64` parameter.
```go
func (s *Store) Init(id string, meta Metadata, initData []byte, generation int64,
    segmentCapacity, segmentBytes, backwardBufferSize, workingSpace, playlistWindowSize int) error
```

**Route-level branching** (`pkg/routes/api.go`): The `/init` handler checks if the stream already exists. If so, it skips metadata/capacity header parsing and calls `s.AddInitEntry(generation, initData)` directly. If not, it proceeds with full parsing and `store.Init(...)`.

**New error**: `ErrDuplicateGeneration`

### 3. CommitSlot Validation (`pkg/stream/stream.go`)

Add check: if the segment's `generation` has no entry in `initEntries`, return `ErrMissingInitForGeneration`. Prevents segments from referencing non-existent init data.

### 4. Init URL Scheme Change (`pkg/routes/stream.go`)

**Old**: `/init-{initID}.mp4` where initID is timestamp
**New**: `/init_{initID}.mp4` where initID is generation

- Parse `initID` as generation (int64)
- Look up via `stream.GetInitEntry(gen)`
- Return 404 if generation not found
- Change to `Cache-Control: no-cache` (generation IDs like 0, 1, 2 are not globally unique across server restarts, so immutable caching is unsafe)

### 5. Playlist Rendering (`pkg/stream/playlist.go`)

**Header**:
```
#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:{s.metadata.TargetDurationSecs}
#EXT-X-MEDIA-SEQUENCE:{window[0].Index}
#EXT-X-DISCONTINUITY-SEQUENCE:{discSeq}
```

`EXT-X-TARGETDURATION` stays single-valued from stream-level metadata (all generations share the same target duration).

**Per-segment**: for each segment in window:
- If first segment OR `seg.Generation != prevGeneration`:
  - If not first segment: emit `#EXT-X-DISCONTINUITY`
  - Emit `#EXT-X-MAP:URI="init_{seg.Generation}.mp4"`
- Emit `#EXT-X-PROGRAM-DATE-TIME:...`
- Emit `#EXTINF:...`
- Emit `segment_{index}.m4s`

**Discontinuity sequence** (transitions scrolled out before window):
```go
discSeq := s.evictedDiscontinuities
// Check boundary between last evicted segment and first buffered segment.
// lastEvictedGeneration == -1 means no segments have ever been evicted.
if start > 0 && s.lastEvictedGeneration >= 0 && s.segments[0].Generation != s.lastEvictedGeneration {
    discSeq++
}
for i := 1; i < start; i++ {
    if s.segments[i].Generation != s.segments[i-1].Generation {
        discSeq++
    }
}
```

### 6. Eviction Tracking (`pkg/stream/stream.go`)

**`evictOldLocked`**: Before returning each segment's buffer to the pool, check if `lastEvictedGeneration >= 0` (not the sentinel) and the segment's generation differs from it. If so, increment `evictedDiscontinuities`. Then set `lastEvictedGeneration = segment.Generation`.

**`dropStaleGenerationLocked`**: No changes. These are future segments that were never in the window.

### 7. API Route Changes (`pkg/routes/api.go`)

**`/init` handler restructure**:
1. Parse `X-SL-GENERATION` header (optional, default 0) — always, regardless of first/subsequent init
2. Read init.mp4 body (always required, max 1MB)
3. Check if stream already exists (`store.Get(streamID)`)
   - **Exists**: call `s.AddInitEntry(generation, initData)`. Skip all metadata/capacity parsing.
   - **Doesn't exist**: parse all metadata/capacity headers as today, call `store.Init(..., generation, ...)`
4. Handle `ErrDuplicateGeneration` → 409 Conflict

**`/segment` handler**: Add `ErrMissingInitForGeneration` → 422.

### 8. Master Playlist (`pkg/routes/stream.go`)

No changes. `s.Metadata()` returns stream-level metadata, which is correct and unchanged.

## Implementation Order

Each step keeps tests passing:

1. **Add `InitEntry` type and new Stream fields** (`initEntries` map, eviction counters). Keep old `initData`/`initTimestampMs` temporarily. Populate both in `Store.Init`. Add `generation` param to `Store.Init` with default 0. Update all callers (routes + tests) to pass generation 0.

2. **Add `Stream.AddInitEntry` method and route branching**. The `/init` handler checks if stream exists. Add `ErrDuplicateGeneration`. Add tests for second init preserving segments, duplicate generation rejection.

3. **Add `GetInitEntry` accessor and generation-based init serving**. Change route to `/init_{gen}.mp4`. Update route tests for new URL pattern.

4. **Add `CommitSlot` init validation** (`ErrMissingInitForGeneration`). Update segment handler error mapping. Add tests.

5. **Update `renderMediaPlaylist`** with discontinuity tags, per-generation `EXT-X-MAP`, `EXT-X-DISCONTINUITY-SEQUENCE`. Update playlist test assertions (`init-0.mp4` → `init_0.mp4`). Add discontinuity-specific tests.

6. **Update eviction** to track discontinuities. Add eviction + discontinuity sequence tests.

7. **Remove old fields** (`initData`, `initTimestampMs`). Remove old accessors (`InitTimestampMs`, `InitDataLen`, `WriteInitDataTo`). Switch all code to new accessors. Verify all tests pass.

8. **Add E2E tests**: full discontinuity flow.

9. **Parse `X-SL-GENERATION` on `/init` route** (may be done earlier alongside step 1-2 depending on ordering).

## Edge Cases & Risks

1. **Race between init gen B and last segment gen A**: Safe. `currentGeneration` only bumps on `CommitSlot`. Gen A segments after init gen B explicitly allowed per protocol.

2. **Init data grows unbounded**: By design. Reaping deferred to separate project.

3. **Stale init URLs**: `init_0.mp4` valid for the lifetime of the stream (not reaped). After server restart, old init data is gone. Init responses use `Cache-Control: no-cache` so clients won't serve stale data.

4. **Multiple /init without segments**: e.g. init gen 0, init gen 1, init gen 2, then segments at gen 2. Only gen 0→gen 2 discontinuity appears. Unused gen 1 sits in map.

5. **Stale-generation drops**: `dropStaleGenerationLocked` removes future segments, not past. Never in window, don't affect discontinuity count.

6. **Missing init for generation**: `CommitSlot` rejects with `ErrMissingInitForGeneration`.

7. **First init at non-zero generation**: `Store.Init` sets `currentGeneration = generation`. If first init is at gen 5, segments at gen 0 are correctly rejected as stale.

8. **Metadata mismatch concern eliminated**: Since subsequent `/init` calls don't accept metadata headers at all, there's no possibility of conflicting metadata.

## Files to Modify

| File | Changes |
|------|---------|
| `pkg/stream/stream.go` | InitEntry type, Stream fields, Store.Init with generation, AddInitEntry, GetInitEntry, eviction tracking, CommitSlot init validation |
| `pkg/stream/playlist.go` | Discontinuity tags, per-generation EXT-X-MAP, discontinuity sequence |
| `pkg/routes/api.go` | Restructure /init handler (first vs subsequent), parse X-SL-GENERATION, handle new errors |
| `pkg/routes/stream.go` | Init serving route /init_{gen}.mp4, generation-based lookup |
| `pkg/stream/stream_test.go` | Update mustInit helper, add generation init tests, eviction counter tests, missing-init tests |
| `pkg/stream/playlist_test.go` | Update URL assertions, add discontinuity rendering tests |
| `pkg/routes/api_test.go` | Generation on /init tests, duplicate generation tests, subsequent init tests |
| `pkg/routes/stream_test.go` | Init URL pattern tests |
| `pkg/routes/e2e_test.go` | Full discontinuity E2E test |
| `pkg/routes/helpers_test.go` | Update initStream/postInit/commitSegment helpers |

## Verification

```bash
go fmt ./...
go fix ./...
go vet ./...
go test ./...
gosec ./...
```

Key test scenarios:
- Single generation: no discontinuity tags, single EXT-X-MAP
- Two generations in window: EXT-X-DISCONTINUITY + new EXT-X-MAP
- Discontinuity scrolls out of window: EXT-X-DISCONTINUITY-SEQUENCE increments
- Three+ generations: multiple discontinuities
- Subsequent /init preserves segments, only needs generation + body
- Duplicate generation on /init returns 409
- Segment push for generation without init returns error
- Init serving by generation (200 for valid, 404 for unknown)
- E2E: init gen 0 → segments → init gen 1 → gen 1 segments → verify playlist + init serving
