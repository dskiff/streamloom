// Package stream provides in-memory storage for HLS stream state.
package stream

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/dskiff/streamloom/pkg/clock"
	"github.com/dskiff/streamloom/pkg/pool"
)

// MaxInitBytes is the maximum allowed size for an init.mp4 upload (1 MB).
const MaxInitBytes = 1 << 20

// ErrSegmentNotFound is returned when a segment index does not exist.
var ErrSegmentNotFound = errors.New("segment not found")

// ErrDuplicateIndex is returned when a segment with the same index already exists.
var ErrDuplicateIndex = errors.New("duplicate segment index")

// ErrBufferFull is returned when the buffer has reached capacity. The caller should back off and retry.
var ErrBufferFull = errors.New("buffer full")

// ErrTimestampInPast is returned when a segment's timestamp is before the
// current time and the stream already contains at least one segment.
// The first segment on an empty stream is exempt from this check.
var ErrTimestampInPast = errors.New("segment timestamp is in the past")

// ErrTimestampOrderViolation is returned when inserting a segment would
// break the ordering invariant: segments sorted by index must also be
// sorted by timestamp (index_1 > index_2 iff timestamp_1 > timestamp_2).
var ErrTimestampOrderViolation = errors.New("segment timestamp order violation")

// ErrStaleGeneration is returned when a segment's generation is older
// than the stream's current generation. The caller should drop the segment.
var ErrStaleGeneration = errors.New("stale generation")

// ErrDuplicateGeneration is returned when an init entry for the given
// generation already exists on the stream.
var ErrDuplicateGeneration = errors.New("duplicate generation for init")

// ErrMissingInitForGeneration is returned when a segment references a
// generation that has no init entry. The streamer must push /init for
// a generation before pushing segments at that generation.
var ErrMissingInitForGeneration = errors.New("no init entry for segment generation")

// ErrNegativeGeneration is returned when a generation value is negative.
// Generations must be non-negative integers; -1 is reserved as an
// internal sentinel.
var ErrNegativeGeneration = errors.New("generation must be non-negative")

// ErrEmptyInitData is returned when init data is empty.
var ErrEmptyInitData = errors.New("init data must not be empty")

// ErrGenerationNotMonotonic is returned when a new init generation is not
// strictly greater than the highest existing init generation. Init entries
// must advance forward; pushing a generation less than an already-registered
// generation is rejected.
var ErrGenerationNotMonotonic = errors.New("init generation must be greater than all existing init generations")

// MaxCodecsLength is the maximum allowed length for a codecs string.
// Real HLS codec strings are typically under 50 bytes (e.g. "avc1.640029,mp4a.40.2").
const MaxCodecsLength = 256

// ValidateCodecs checks that a codecs string is safe for interpolation into
// an HLS playlist. It allows only characters found in valid HLS codec
// identifiers: ASCII letters, digits, dots, commas, hyphens, and plus signs.
func ValidateCodecs(s string) error {
	if s == "" {
		return fmt.Errorf("empty codecs string")
	}
	if len(s) > MaxCodecsLength {
		return fmt.Errorf("codecs string too long: %d bytes (max %d)", len(s), MaxCodecsLength)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isValidCodecChar(c) {
			return fmt.Errorf("invalid character %q (0x%02X) at position %d", c, c, i)
		}
	}
	return nil
}

// isValidCodecChar returns true for characters allowed in HLS codec strings:
// ASCII letters, digits, dot, comma, hyphen, and plus.
func isValidCodecChar(c byte) bool {
	return (c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') ||
		c == '.' || c == ',' || c == '-' || c == '+'
}

// Slot holds a single fMP4 media segment.
// Data is acquired from a BufferPool on commit and returned on eviction.
type Slot struct {
	Index      uint32
	Timestamp  int64            // Unix ms
	DurationMs uint32           // milliseconds
	Data       *pool.BufferSlot // buffer from pool; nil when slot is unoccupied
	Generation int64            // encoding generation; segments from older generations are dropped on advance
}

// Metadata holds HLS manifest metadata received during stream initialization.
type Metadata struct {
	Bandwidth          int     // bits/sec, for EXT-X-STREAM-INF BANDWIDTH
	Codecs             string  // e.g. "hvc1.1.6.L120.90", for CODECS
	Width              int     // pixels
	Height             int     // pixels
	FrameRate          float64 // e.g. 23.976
	SegmentByteCount   int     // pre-allocated byte capacity for each segment slot
	TargetDurationSecs int     // EXT-X-TARGETDURATION value (seconds)
}

// initEntry holds the init segment data for a single encoding generation.
// initData is cloned on creation and must not be modified.
type initEntry struct {
	initData []byte
}

// Stream holds the complete in-memory state for a single HLS stream.
type Stream struct {
	mu       sync.RWMutex
	clock    clock.Clock
	metadata Metadata

	// initEntries holds per-generation init segment data, keyed by generation.
	// Entries are added via Init (first generation) and AddInitEntry (subsequent).
	// Stale entries are evicted during CommitSlot when their generation is below
	// currentGeneration and no buffered segments reference them.
	initEntries map[int64]*initEntry

	segments          []Slot // sorted by Index
	segmentCap        int    // maximum number of segments
	currentGeneration int64  // latest generation seen; segments from older generations are dropped
	bufPool           *pool.BufferPool

	// backwardBufferSize is the maximum number of backward segments to
	// retain. A segment is "backward" when its Timestamp is before the
	// current wall-clock time, regardless of whether it has appeared in a
	// playlist. On each CommitSlot call, backward segments are counted and
	// the oldest are evicted until the backward count is within this limit.
	backwardBufferSize int

	// totalSegmentCount is the total number of segments ever added to this stream.
	// Useful for deriving EXT-X-MEDIA-SEQUENCE in playlist generation.
	totalSegmentCount int64

	// evictedDiscontinuities counts generation transitions among segments that
	// have been evicted from the buffer. Used to compute EXT-X-DISCONTINUITY-SEQUENCE.
	evictedDiscontinuities int

	// lastEvictedGeneration is the generation of the most recently evicted segment.
	// Initialized to -1 (sentinel) meaning no segments have been evicted yet.
	// Since generations are validated as non-negative, -1 is unambiguous.
	lastEvictedGeneration int64

	// notifyCh is signaled (non-blocking) on each successful CommitSlot to
	// wake the playlist renderer goroutine.
	notifyCh chan struct{}

	// done is closed when the stream is deleted, stopping the renderer goroutine.
	done chan struct{}

	// stopped is closed by the renderer goroutine when it exits.
	stopped chan struct{}

	// hasSegments is closed when the first segment is committed.
	// Readers can select on this to block until content is available.
	hasSegments chan struct{}

	// hasPlaylist is closed when the renderer first produces a valid playlist
	// (one starting with "#EXTM3U"). Readers can select on this to block
	// until a serveable playlist is cached.
	hasPlaylist     chan struct{}
	hasPlaylistOnce sync.Once

	// cachedPlaylist holds the most recently rendered media playlist string.
	// Written by the renderer goroutine, read by HTTP handlers.
	cachedPlaylist atomic.Pointer[string]
}

// Metadata returns a copy of the stream's metadata.
func (s *Stream) Metadata() Metadata {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.metadata
}

// TotalSegmentCount returns the total number of segments ever added.
func (s *Stream) TotalSegmentCount() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.totalSegmentCount
}

// SegmentLoad returns the current number of buffered segments and the capacity.
func (s *Stream) SegmentLoad() (int, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.segments), s.segmentCap
}

// HasSegments returns a channel that is closed when the first segment is
// committed. Callers can select on this to block until content is available.
func (s *Stream) HasSegments() <-chan struct{} {
	return s.hasSegments
}

// Done returns a channel that is closed when the stream is deleted.
// Callers can select on this to detect stream shutdown.
func (s *Stream) Done() <-chan struct{} {
	return s.done
}

// HasPlaylist returns a channel that is closed when the renderer first
// produces a valid playlist (starting with "#EXTM3U"). Callers can select
// on this to block until a serveable playlist is cached.
func (s *Stream) HasPlaylist() <-chan struct{} {
	return s.hasPlaylist
}

// CachedPlaylist returns the most recently rendered media playlist string.
// Returns "" if no playlist has been rendered yet.
func (s *Stream) CachedPlaylist() string {
	p := s.cachedPlaylist.Load()
	if p == nil {
		return ""
	}
	return *p
}

// GetInit returns the init segment bytes for the given generation, or nil and
// false if no init has been pushed for that generation. The returned slice is
// shared state; callers must not modify it.
func (s *Stream) GetInit(generation int64) ([]byte, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.initEntries[generation]
	if !ok {
		return nil, false
	}
	return e.initData, true
}

// CurrentGeneration returns the stream's current generation value.
func (s *Stream) CurrentGeneration() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.currentGeneration
}

// dropStaleGenerationLocked removes segments at or after position fromPos
// whose generation is older than the stream's current generation. Freed
// buffers are returned to the pool. The segment slice is compacted in-place.
//
// After CommitSlot's pre-drop ordering and duplicate checks, segments
// reaching this function are stale future segments that have never appeared
// in a playlist (assuming a monotonic clock; a backward clock adjustment
// could cause a previously-future segment to appear in a playlist before
// being dropped here). If a segment has active readers (extremely unlikely for
// unreferenced future segments), it is retained rather than dropped,
// matching evictOldLocked's pattern.
//
// Must be called with s.mu held.
func (s *Stream) dropStaleGenerationLocked(fromPos int) {
	w := fromPos // write cursor
	for r := fromPos; r < len(s.segments); r++ {
		if s.segments[r].Generation < s.currentGeneration {
			if s.segments[r].Data.Readers() > 0 {
				// Retain segment until readers finish, matching
				// evictOldLocked's skip-and-retain pattern.
				if w != r {
					s.segments[w] = s.segments[r]
				}
				w++
				continue
			}
			s.bufPool.AssertCheckedOut(s.segments[r].Data)
			s.bufPool.Put(s.segments[r].Data)
			s.segments[r].Data = nil
			continue
		}
		if w != r {
			s.segments[w] = s.segments[r]
		}
		w++
	}
	// Clear trailing slots to avoid retaining stale pointers.
	for i := w; i < len(s.segments); i++ {
		s.segments[i] = Slot{}
	}
	s.segments = s.segments[:w]
}

// evictOldLocked removes backward (past) segments that exceed backwardBufferSize.
// Segments are sorted by index, and the invariant index_1 > index_2 iff
// timestamp_1 > timestamp_2 means they are also sorted by timestamp.
// Binary-search for the forward/backward split point, then remove the oldest
// until backward count is within the limit. Eviction stops early if a segment
// has active readers, temporarily allowing the backward count to exceed the
// limit until those readers finish.
//
// Must be called with s.mu held.
func (s *Stream) evictOldLocked() {
	nowMs := s.clock.Now().UnixMilli()
	backwardCount := sort.Search(len(s.segments), func(i int) bool {
		return s.segments[i].Timestamp >= nowMs
	})
	limit := s.backwardBufferSize
	if backwardCount <= limit {
		return
	}
	evictCount := backwardCount - limit
	for i := range evictCount {
		if s.segments[i].Data.Readers() > 0 {
			// An active reader holds a reference to this segment's buffer.
			// Stop eviction here: evict only segments before this one, and
			// preserve this segment and all newer ones that the slice shift
			// keeps. The buffer will temporarily exceed the backward limit
			// until readers finish.
			evictCount = i
			break
		}

		// Track generation transitions for EXT-X-DISCONTINUITY-SEQUENCE.
		if s.lastEvictedGeneration >= 0 && s.segments[i].Generation != s.lastEvictedGeneration {
			s.evictedDiscontinuities++
		}
		s.lastEvictedGeneration = s.segments[i].Generation

		s.bufPool.AssertCheckedOut(s.segments[i].Data)
		s.bufPool.Put(s.segments[i].Data)
		s.segments[i].Data = nil
	}
	// Shift remaining segments to the front, reusing the backing array.
	n := copy(s.segments, s.segments[evictCount:])
	// Clear trailing slots to avoid retaining stale pointers.
	for i := n; i < len(s.segments); i++ {
		s.segments[i] = Slot{}
	}
	s.segments = s.segments[:n]
}

// evictStaleInitEntriesLocked removes init entries whose generation is below
// currentGeneration and not referenced by any buffered segment. This prevents
// unbounded growth of the initEntries map across encoder restarts.
//
// Must be called with s.mu held.
func (s *Stream) evictStaleInitEntriesLocked() {
	if len(s.initEntries) <= 1 {
		return
	}
	for gen := range s.initEntries {
		if gen >= s.currentGeneration {
			continue
		}
		// Check if any buffered segment still references this generation.
		referenced := false
		for i := range s.segments {
			if s.segments[i].Generation == gen {
				referenced = true
				break
			}
		}
		if !referenced {
			delete(s.initEntries, gen)
		}
	}
}

// AcquireSlot obtains a BufferSlot from the pool. The caller must either
// pass the slot to CommitSlot (on success) or return it via ReleaseSlot
// (on error / abort). Returns (nil, false) if the pool is exhausted.
func (s *Stream) AcquireSlot() (*pool.BufferSlot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bufPool.Get()
}

// ReleaseSlot returns a BufferSlot to the pool without committing it.
// Use this on error paths after AcquireSlot.
func (s *Stream) ReleaseSlot(buf *pool.BufferSlot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bufPool.AssertCheckedOut(buf)
	s.bufPool.Put(buf)
}

// CommitSlot inserts a pre-filled BufferSlot into the stream's segment list,
// maintaining sorted order by index. On success, ownership of buf transfers
// to the stream. On error, the caller retains ownership and must call
// ReleaseSlot.
//
// generation identifies the encoding session. Segments from older generations
// at or after the insertion point are dropped. A generation of 0 is the
// default; it participates in generation comparisons the same as any other
// value (e.g. a generation-1 segment will cause generation-0 segments to be
// dropped).
//
// Returns ErrStaleGeneration if generation is older than the stream's current,
// ErrMissingInitForGeneration if no init entry exists for the generation,
// ErrDuplicateIndex if a segment with the same index already exists,
// ErrBufferFull if the segment list is at capacity, ErrTimestampInPast if
// the timestamp is before the current time and the stream is non-empty, or
// ErrTimestampOrderViolation if the timestamp would break the index/timestamp
// ordering invariant.
func (s *Stream) CommitSlot(index uint32, buf *pool.BufferSlot, timestamp int64, durationMs uint32, generation int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Reject segments from older generations.
	if s.currentGeneration > generation {
		return ErrStaleGeneration
	}

	// Reject segments whose generation has no init entry.
	if _, ok := s.initEntries[generation]; !ok {
		return ErrMissingInitForGeneration
	}

	// Reject past timestamps unless the stream is empty (first segment exception).
	if timestamp < s.clock.Now().UnixMilli() && len(s.segments) > 0 {
		return ErrTimestampInPast
	}

	if s.currentGeneration < generation {
		s.currentGeneration = generation
	}

	s.bufPool.AssertCheckedOut(buf)

	s.evictOldLocked()

	// Binary search for insertion point.
	idx := sort.Search(len(s.segments), func(i int) bool {
		return s.segments[i].Index >= index
	})

	// Enforce ordering invariant BEFORE any mutation. The ordering check
	// must see the pre-drop segment list to catch commits that would
	// require removing playlist-visible segments. Without this ordering,
	// dropStaleGenerationLocked would erase the evidence and the post-drop
	// check would pass on a truncated list.
	if idx > 0 && s.segments[idx-1].Timestamp >= timestamp {
		return fmt.Errorf("%w: left neighbor index=%d ts=%d >= new ts=%d",
			ErrTimestampOrderViolation, s.segments[idx-1].Index, s.segments[idx-1].Timestamp, timestamp)
	}
	if idx < len(s.segments) && s.segments[idx].Timestamp <= timestamp {
		return fmt.Errorf("%w: right neighbor index=%d ts=%d <= new ts=%d",
			ErrTimestampOrderViolation, s.segments[idx].Index, s.segments[idx].Timestamp, timestamp)
	}

	// Check for duplicate before drop. A stale segment at the same index
	// with a higher timestamp passes the ordering check above but is still
	// invalid — same-index reuse across generations is rejected defensively.
	if idx < len(s.segments) && s.segments[idx].Index == index {
		return ErrDuplicateIndex
	}

	// Drop segments at/after the insertion point whose generation is older
	// than the stream's current generation. After the ordering and duplicate
	// checks above, this only encounters stale segments at higher indices
	// with timestamps greater than the new segment's timestamp — i.e. future
	// stale segments that have never appeared in a playlist.
	s.dropStaleGenerationLocked(idx)
	s.evictStaleInitEntriesLocked()

	if len(s.segments) >= s.segmentCap {
		return ErrBufferFull
	}

	s.segments = slices.Insert(s.segments, idx, Slot{
		Index:      index,
		Timestamp:  timestamp,
		DurationMs: durationMs,
		Data:       buf,
		Generation: generation,
	})

	s.totalSegmentCount++

	// Signal the renderer goroutine that new data is available.
	if s.totalSegmentCount == 1 {
		close(s.hasSegments)
	}
	select {
	case s.notifyCh <- struct{}{}:
	default:
	}

	return nil
}

// RunWithSegmentSlot finds the segment with the given index, acquires a
// reference to its BufferSlot, and calls fn outside the lock. The reference
// prevents eviction from reclaiming the buffer while the callback runs.
// The slot must not be retained or used after fn returns.
func (s *Stream) RunWithSegmentSlot(index uint32, fn func(slot *pool.BufferSlot) error) error {
	s.mu.RLock()

	idx := sort.Search(len(s.segments), func(i int) bool {
		return s.segments[i].Index >= index
	})
	if idx >= len(s.segments) || s.segments[idx].Index != index {
		s.mu.RUnlock()
		return ErrSegmentNotFound
	}

	buf := s.segments[idx].Data
	s.bufPool.AssertCheckedOut(buf)

	buf.ReaderInc()
	defer buf.ReaderDec()
	s.mu.RUnlock()

	return fn(buf)
}

// Store is a concurrent-safe map of streamID to *Stream.
type Store struct {
	mu      sync.RWMutex
	streams map[string]*Stream
	clock   clock.Clock
}

// NewStore creates an empty Store with the given clock.
func NewStore(clk clock.Clock) *Store {
	return &Store{streams: make(map[string]*Stream), clock: clk}
}

// ErrInvalidTargetDuration is returned when TargetDurationSecs is not positive.
var ErrInvalidTargetDuration = errors.New("metadata.TargetDurationSecs must be > 0")

// ErrInvalidPlaylistWindowSize is returned when playlistWindowSize is not positive.
var ErrInvalidPlaylistWindowSize = errors.New("playlistWindowSize must be > 0")

// ErrInvalidBackwardBufferSize is returned when backwardBufferSize is less than 1
// or not less than the segment capacity.
var ErrInvalidBackwardBufferSize = errors.New("backwardBufferSize must be >= 1 and < segmentCapacity")

// ErrInvalidWorkingSpace is returned when workingSpace is negative or would
// overflow when added to segmentCapacity.
var ErrInvalidWorkingSpace = errors.New("workingSpace must be >= 0 and segmentCapacity + workingSpace must not overflow")

// Init creates or replaces a stream's init state, clearing any existing segments.
// The initData slice is cloned so that the caller cannot mutate the stored bytes.
// generation identifies the encoding generation for this init entry; it must be
// non-negative (returns ErrNegativeGeneration otherwise). initData must be
// non-empty (returns ErrEmptyInitData otherwise).
// workingSpace extra slots are added to the BufferPool beyond segmentCapacity to
// allow concurrent handlers to hold buffers before committing.
// backwardBufferSize controls how many past segments are retained during eviction;
// it must be >= 1 and < segmentCapacity.
// playlistWindowSize is the maximum number of segments in the media playlist.
func (s *Store) Init(id string, meta Metadata, initData []byte, generation int64, segmentCapacity, segmentBytes, backwardBufferSize, workingSpace, playlistWindowSize int) error {
	if generation < 0 {
		return ErrNegativeGeneration
	}
	if len(initData) == 0 {
		return ErrEmptyInitData
	}
	if meta.TargetDurationSecs <= 0 {
		return ErrInvalidTargetDuration
	}
	if playlistWindowSize <= 0 {
		return ErrInvalidPlaylistWindowSize
	}
	if backwardBufferSize < 1 || backwardBufferSize >= segmentCapacity {
		return ErrInvalidBackwardBufferSize
	}
	if workingSpace < 0 || segmentCapacity > math.MaxInt-workingSpace {
		return ErrInvalidWorkingSpace
	}

	cloned := make([]byte, len(initData))
	copy(cloned, initData)

	meta.SegmentByteCount = segmentBytes

	s.mu.Lock()

	// Stop the previous stream's renderer goroutine if replacing.
	var prev *Stream
	if p, ok := s.streams[id]; ok {
		close(p.done)
		prev = p
	}

	st := &Stream{
		clock:    s.clock,
		metadata: meta,
		initEntries: map[int64]*initEntry{
			generation: {initData: cloned},
		},
		segments:              make([]Slot, 0, segmentCapacity),
		segmentCap:            segmentCapacity,
		currentGeneration:     generation,
		bufPool:               pool.NewBufferPool(segmentCapacity+workingSpace, segmentBytes),
		backwardBufferSize:    backwardBufferSize,
		lastEvictedGeneration: -1, // sentinel: no evictions yet
		notifyCh:              make(chan struct{}, 1),
		done:                  make(chan struct{}),
		stopped:               make(chan struct{}),
		hasSegments:           make(chan struct{}),
		hasPlaylist:           make(chan struct{}),
	}
	s.streams[id] = st
	s.mu.Unlock()

	// Wait for the previous renderer to fully exit before starting the new one.
	if prev != nil {
		<-prev.stopped
	}

	go st.runPlaylistRenderer(playlistWindowSize)

	return nil
}

// AddInitEntry adds a new init segment for the given generation to an existing
// stream. The initData slice is cloned. This does NOT modify segments, the
// buffer pool, the renderer, or currentGeneration.
//
// Returns ErrNegativeGeneration if generation < 0, ErrEmptyInitData if
// initData is empty, ErrGenerationNotMonotonic if generation is not strictly
// greater than the highest existing init generation, or ErrDuplicateGeneration
// if an init entry for this generation already exists.
func (s *Stream) AddInitEntry(generation int64, initData []byte) error {
	if generation < 0 {
		return ErrNegativeGeneration
	}
	if len(initData) == 0 {
		return ErrEmptyInitData
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.initEntries[generation]; ok {
		return ErrDuplicateGeneration
	}

	// The new generation must be strictly greater than all existing init
	// generations to maintain forward-only ordering.
	for existingGen := range s.initEntries {
		if generation < existingGen {
			return ErrGenerationNotMonotonic
		}
	}

	cloned := make([]byte, len(initData))
	copy(cloned, initData)
	s.initEntries[generation] = &initEntry{initData: cloned}
	return nil
}

// Clock returns the clock used by this store and its streams.
func (s *Store) Clock() clock.Clock {
	return s.clock
}

// Get returns the stream for the given ID, or nil if not found.
func (s *Store) Get(id string) *Stream {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.streams[id]
}

// Delete removes a stream and all its data. Returns true if the stream existed.
// Blocks until the renderer goroutine has exited.
func (s *Store) Delete(id string) bool {
	s.mu.Lock()
	st, ok := s.streams[id]
	if ok {
		close(st.done)
		delete(s.streams, id)
	}
	s.mu.Unlock()

	if ok {
		<-st.stopped
	}
	return ok
}
