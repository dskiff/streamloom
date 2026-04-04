package stream

import (
	"bytes"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dskiff/streamloom/pkg/clock"
	"github.com/dskiff/streamloom/pkg/pool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSegmentBytes = 1024
const testCap = 100
const testBackwardBufferSize = 99

const testPlaylistWindowSize = 12

// mustInit is a test helper that calls Store.Init and fails the test on error.
// Registers cleanup to stop the renderer goroutine.
func mustInit(t *testing.T, s *Store, id string, meta Metadata, initData []byte, segmentCapacity, segmentBytes, backwardBufferSize int) {
	t.Helper()
	err := s.Init(id, meta, initData, 0, segmentCapacity, segmentBytes, backwardBufferSize, 0, testPlaylistWindowSize)
	require.NoError(t, err, "Init(%s)", id)
	t.Cleanup(func() { s.Delete(id) })
}

// mustCommitSlot acquires a slot, fills it with data, and commits it.
// Fails the test on any error.
func mustCommitSlot(t *testing.T, s *Stream, index uint32, data []byte, ts int64, dur uint32) {
	t.Helper()
	buf, ok := s.AcquireSlot()
	require.True(t, ok, "AcquireSlot should succeed")
	_, err := buf.ReadFrom(bytes.NewReader(data))
	require.NoError(t, err, "ReadFrom should succeed")
	err = s.CommitSlot(index, buf, ts, dur, 0)
	if err != nil {
		s.ReleaseSlot(buf)
	}
	require.NoError(t, err)
}

// readSegment reads segment data using RunWithSegmentSlot and returns the bytes.
func readSegment(s *Stream, index uint32) ([]byte, error) {
	var data []byte
	err := s.RunWithSegmentSlot(index, func(slot *pool.BufferSlot) error {
		var buf bytes.Buffer
		_, err := slot.WriteTo(&buf)
		data = buf.Bytes()
		return err
	})
	return data, err
}

// commitSlot acquires a slot, fills it with data, and attempts to commit it.
// Returns the error from CommitSlot (releases the buffer on error).
func commitSlot(t *testing.T, s *Stream, index uint32, data []byte, ts int64, dur uint32) error {
	t.Helper()
	buf, ok := s.AcquireSlot()
	require.True(t, ok, "AcquireSlot should succeed")
	_, err := buf.ReadFrom(bytes.NewReader(data))
	require.NoError(t, err, "ReadFrom should succeed")
	err = s.CommitSlot(index, buf, ts, dur, 0)
	if err != nil {
		s.ReleaseSlot(buf)
	}
	return err
}

// commitSlotGen acquires a slot, fills it with data, and attempts to commit
// it with the given generation. Returns the error from CommitSlot.
func commitSlotGen(t *testing.T, s *Stream, index uint32, data []byte, ts int64, dur uint32, gen int64) error {
	t.Helper()
	buf, ok := s.AcquireSlot()
	require.True(t, ok, "AcquireSlot should succeed")
	_, err := buf.ReadFrom(bytes.NewReader(data))
	require.NoError(t, err, "ReadFrom should succeed")
	err = s.CommitSlot(index, buf, ts, dur, gen)
	if err != nil {
		s.ReleaseSlot(buf)
	}
	return err
}

// --- Store and Stream tests ---

func TestNewStore(t *testing.T) {
	s := NewStore(clock.Real{})
	require.NotNil(t, s)
}

func TestGetMissing(t *testing.T) {
	s := NewStore(clock.Real{})
	assert.Nil(t, s.Get("999"))
}

func TestInitAndGet(t *testing.T) {
	s := NewStore(clock.Real{})
	meta := Metadata{
		Bandwidth:          4_000_000,
		Codecs:             "hvc1.1.6.L120.90",
		Width:              1920,
		Height:             1080,
		FrameRate:          23.976,
		TargetDurationSecs: 2,
	}
	initData := []byte{0x00, 0x01, 0x02, 0x03}

	mustInit(t, s, "100", meta, initData, testCap, testSegmentBytes, testBackwardBufferSize)

	stream := s.Get("100")
	require.NotNil(t, stream)

	got := stream.Metadata()
	assert.Equal(t, meta.Bandwidth, got.Bandwidth)
	assert.Equal(t, meta.Codecs, got.Codecs)
	assert.Equal(t, meta.Width, got.Width)
	assert.Equal(t, meta.Height, got.Height)
	assert.Equal(t, meta.FrameRate, got.FrameRate)

	var buf bytes.Buffer
	_, err := stream.WriteInitDataForGenerationTo(&buf, 0)
	require.NoError(t, err)
	assert.Equal(t, initData, buf.Bytes())
}

func TestInitClonesData(t *testing.T) {
	s := NewStore(clock.Real{})
	original := []byte{0x01, 0x02, 0x03}
	mustInit(t, s, "1", Metadata{TargetDurationSecs: 1}, original, testCap, testSegmentBytes, testBackwardBufferSize)

	// Mutate the caller's slice after Init.
	original[0] = 0xFF

	stream := s.Get("1")
	var buf bytes.Buffer
	_, _ = stream.WriteInitDataForGenerationTo(&buf, 0)
	assert.Equal(t, byte(0x01), buf.Bytes()[0], "Init did not clone the input; caller mutation leaked into store")
}

func TestWriteInitDataIsImmutable(t *testing.T) {
	s := NewStore(clock.Real{})
	mustInit(t, s, "1", Metadata{TargetDurationSecs: 1}, []byte{0x01, 0x02, 0x03}, testCap, testSegmentBytes, testBackwardBufferSize)

	stream := s.Get("1")

	// Two reads should produce identical results.
	var buf1, buf2 bytes.Buffer
	_, _ = stream.WriteInitDataForGenerationTo(&buf1, 0)
	_, _ = stream.WriteInitDataForGenerationTo(&buf2, 0)
	assert.Equal(t, buf1.Bytes(), buf2.Bytes(), "WriteInitDataForGenerationTo returned different results on successive calls")
}

func TestReInitOverwrites(t *testing.T) {
	s := NewStore(clock.Real{})
	mustInit(t, s, "1", Metadata{Bandwidth: 1000, TargetDurationSecs: 1}, []byte{0x01}, testCap, testSegmentBytes, testBackwardBufferSize)
	mustInit(t, s, "1", Metadata{Bandwidth: 2000, TargetDurationSecs: 1}, []byte{0x02, 0x03}, testCap, testSegmentBytes, testBackwardBufferSize)

	stream := s.Get("1")
	require.NotNil(t, stream, "expected stream after re-init")

	assert.Equal(t, 2000, stream.Metadata().Bandwidth)
	var buf bytes.Buffer
	_, _ = stream.WriteInitDataForGenerationTo(&buf, 0)
	assert.Equal(t, []byte{0x02, 0x03}, buf.Bytes())
}

func TestMultipleStreams(t *testing.T) {
	s := NewStore(clock.Real{})
	mustInit(t, s, "2", Metadata{Bandwidth: 1, TargetDurationSecs: 1}, []byte{0x0A}, testCap, testSegmentBytes, testBackwardBufferSize)
	mustInit(t, s, "3", Metadata{Bandwidth: 2, TargetDurationSecs: 1}, []byte{0x0B}, testCap, testSegmentBytes, testBackwardBufferSize)

	a := s.Get("2")
	b := s.Get("3")
	require.NotNil(t, a, "expected stream a")
	require.NotNil(t, b, "expected stream b")
	assert.Equal(t, 1, a.Metadata().Bandwidth)
	assert.Equal(t, 2, b.Metadata().Bandwidth)
}

func TestConcurrentAccess(t *testing.T) {
	s := NewStore(clock.Real{})
	var wg sync.WaitGroup

	// Concurrent writers
	for i := range 50 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := s.Init("50", Metadata{Bandwidth: i, TargetDurationSecs: 1}, []byte{byte(i)}, 0, testCap, testSegmentBytes, testBackwardBufferSize, 0, testPlaylistWindowSize)
			assert.NoError(t, err, "Init(%d)", i)
		}(i)
	}

	// Concurrent readers
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stream := s.Get("50")
			if stream != nil {
				_ = stream.Metadata()
				var buf bytes.Buffer
				_, _ = stream.WriteInitDataForGenerationTo(&buf, 0)
			}
		}()
	}

	wg.Wait()

	// After all goroutines, stream should exist with some valid state
	require.NotNil(t, s.Get("50"), "expected stream to exist after concurrent writes")
	t.Cleanup(func() { s.Delete("50") })
}

func TestInitRejectsInvalidBackwardBufferSize(t *testing.T) {
	s := NewStore(clock.Real{})

	// backwardBufferSize=0 should fail.
	err := s.Init("1", Metadata{TargetDurationSecs: 1}, []byte{0x00}, 0, 10, testSegmentBytes, 0, 0, testPlaylistWindowSize)
	assert.ErrorIs(t, err, ErrInvalidBackwardBufferSize, "size=0")

	// backwardBufferSize equal to segmentCapacity should fail.
	err = s.Init("1", Metadata{TargetDurationSecs: 1}, []byte{0x00}, 0, 10, testSegmentBytes, 10, 0, testPlaylistWindowSize)
	assert.ErrorIs(t, err, ErrInvalidBackwardBufferSize, "size=cap")

	// backwardBufferSize greater than segmentCapacity should fail.
	err = s.Init("1", Metadata{TargetDurationSecs: 1}, []byte{0x00}, 0, 10, testSegmentBytes, 11, 0, testPlaylistWindowSize)
	assert.ErrorIs(t, err, ErrInvalidBackwardBufferSize, "size>cap")

	// Valid backwardBufferSize should succeed.
	err = s.Init("1", Metadata{TargetDurationSecs: 1}, []byte{0x00}, 0, 10, testSegmentBytes, 9, 0, testPlaylistWindowSize)
	assert.NoError(t, err, "valid size")
	t.Cleanup(func() { s.Delete("1") })
}

func TestInitRejectsInvalidWorkingSpace(t *testing.T) {
	s := NewStore(clock.Real{})

	// Negative workingSpace should fail.
	err := s.Init("1", Metadata{TargetDurationSecs: 1}, []byte{0x00}, 0, 10, testSegmentBytes, 9, -1, testPlaylistWindowSize)
	assert.ErrorIs(t, err, ErrInvalidWorkingSpace, "negative")

	// Overflow: segmentCapacity + workingSpace > MaxInt.
	err = s.Init("1", Metadata{TargetDurationSecs: 1}, []byte{0x00}, 0, math.MaxInt, testSegmentBytes, 1, 1, testPlaylistWindowSize)
	assert.ErrorIs(t, err, ErrInvalidWorkingSpace, "overflow")

	// Zero workingSpace should succeed.
	err = s.Init("1", Metadata{TargetDurationSecs: 1}, []byte{0x00}, 0, 10, testSegmentBytes, 9, 0, testPlaylistWindowSize)
	assert.NoError(t, err, "zero")

	// Positive workingSpace should succeed (re-init closes old goroutine).
	err = s.Init("1", Metadata{TargetDurationSecs: 1}, []byte{0x00}, 0, 10, testSegmentBytes, 9, 5, testPlaylistWindowSize)
	assert.NoError(t, err, "positive")
	t.Cleanup(func() { s.Delete("1") })
}

// --- Segment tests ---

// newTestStream creates a Store with a single initialized stream and returns
// the Store's clock and the Stream. The clock is set to 1 hour in the past so
// that real-time timestamps appear as "future" to CommitSlot.
func newTestStream(t *testing.T, clk *clock.Mock) *Stream {
	t.Helper()
	store := NewStore(clk)
	mustInit(t, store, "1", Metadata{Bandwidth: 1000, TargetDurationSecs: 1}, []byte{0x00}, testCap, testSegmentBytes, testBackwardBufferSize)
	s := store.Get("1")
	require.NotNil(t, s, "expected stream")
	return s
}

func TestCommitSlotBasic(t *testing.T) {
	clk := clock.NewMock(time.Now().Add(-1 * time.Hour))
	s := newTestStream(t, clk)
	ts := time.Now().UnixMilli()

	mustCommitSlot(t, s, 0, []byte{0xAA, 0xBB}, ts, 2000)

	data, err := readSegment(s, 0)
	require.NoError(t, err)
	assert.Equal(t, 2, len(data))
	assert.Equal(t, []byte{0xAA, 0xBB}, data)
}

func TestCommitSlotOrdering(t *testing.T) {
	fixedTime := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	clk := clock.NewMock(fixedTime)
	s := newTestStream(t, clk)

	// Add segments with monotonically increasing timestamps matching index order.
	futureBase := fixedTime.Add(10 * time.Second).UnixMilli()
	require.NoError(t, commitSlot(t, s, 0, []byte{0x01}, futureBase+1000, 2000))
	require.NoError(t, commitSlot(t, s, 1, []byte{0x02}, futureBase+2000, 2000))
	require.NoError(t, commitSlot(t, s, 2, []byte{0x03}, futureBase+3000, 2000))

	// Verify all segments are retrievable.
	for _, idx := range []uint32{0, 1, 2} {
		_, err := readSegment(s, idx)
		assert.NoError(t, err, "readSegment(%d)", idx)
	}

	// Verify sorted order by both index and timestamp.
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := 1; i < len(s.segments); i++ {
		assert.Greater(t, s.segments[i].Index, s.segments[i-1].Index,
			"buffer not sorted by index at position %d", i)
		assert.Greater(t, s.segments[i].Timestamp, s.segments[i-1].Timestamp,
			"buffer not sorted by timestamp at position %d", i)
	}
}

func TestCommitSlotRejectsTimestampOrderViolation(t *testing.T) {
	fixedTime := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	clk := clock.NewMock(fixedTime)
	s := newTestStream(t, clk)
	futureBase := fixedTime.Add(10 * time.Second).UnixMilli()

	// Commit index 0 and 2, leaving a gap for index 1.
	require.NoError(t, commitSlot(t, s, 0, []byte{0x01}, futureBase+1000, 2000))
	require.NoError(t, commitSlot(t, s, 2, []byte{0x03}, futureBase+3000, 2000))

	// index=1 with timestamp higher than right neighbor (index=2) violates invariant.
	err := commitSlot(t, s, 1, []byte{0x02}, futureBase+4000, 2000)
	assert.ErrorIs(t, err, ErrTimestampOrderViolation)

	// index=1 with timestamp equal to left neighbor (index=0) violates invariant.
	err = commitSlot(t, s, 1, []byte{0x02}, futureBase+1000, 2000)
	assert.ErrorIs(t, err, ErrTimestampOrderViolation)

	// index=1 with timestamp less than left neighbor (index=0) violates invariant.
	err = commitSlot(t, s, 1, []byte{0x02}, futureBase+500, 2000)
	assert.ErrorIs(t, err, ErrTimestampOrderViolation)

	// index=1 with timestamp equal to right neighbor (index=2) violates invariant.
	err = commitSlot(t, s, 1, []byte{0x02}, futureBase+3000, 2000)
	assert.ErrorIs(t, err, ErrTimestampOrderViolation)

	// index=1 with valid timestamp strictly between neighbors succeeds.
	require.NoError(t, commitSlot(t, s, 1, []byte{0x02}, futureBase+2000, 2000))
}

func TestCommitSlotClonesData(t *testing.T) {
	clk := clock.NewMock(time.Now().Add(-1 * time.Hour))
	s := newTestStream(t, clk)
	original := []byte{0x01, 0x02, 0x03}

	// ReadFrom copies into the pooled slab, so mutating original afterwards
	// should not affect the stored data.
	mustCommitSlot(t, s, 0, original, time.Now().UnixMilli(), 2000)

	original[0] = 0xFF

	data, _ := readSegment(s, 0)
	assert.Equal(t, byte(0x01), data[0], "CommitSlot did not isolate the input; caller mutation leaked into store")
}

func TestCommitSlotDuplicateIndex(t *testing.T) {
	clk := clock.NewMock(time.Now().Add(-1 * time.Hour))
	s := newTestStream(t, clk)
	ts := time.Now().UnixMilli()

	mustCommitSlot(t, s, 5, []byte{0x01}, ts, 2000)

	err := commitSlot(t, s, 5, []byte{0x02}, ts+1000, 2000)
	assert.ErrorIs(t, err, ErrDuplicateIndex)

	// Original data should be unchanged.
	data, _ := readSegment(s, 5)
	assert.Equal(t, []byte{0x01}, data, "segment data changed after duplicate insert")
}

func TestBufferOverwrite(t *testing.T) {
	clk := clock.NewMock(time.Now().Add(-1 * time.Hour))
	store := NewStore(clk)
	// workingSpace=1 so we can acquire a slot to attempt the 4th push.
	require.NoError(t, store.Init("1", Metadata{Bandwidth: 1000, TargetDurationSecs: 1}, []byte{0x00}, 0, 3, testSegmentBytes, 2, 1, testPlaylistWindowSize))
	t.Cleanup(func() { store.Delete("1") })
	s := store.Get("1")

	// Fill 3-slot buffer.
	ts := time.Now().UnixMilli()
	for i := range 3 {
		mustCommitSlot(t, s, uint32(i), []byte{byte(i)}, ts+int64(i)*1000, 1000)
	}

	// Buffer is full — next push should fail.
	err := commitSlot(t, s, 3, []byte{0x03}, ts+3000, 1000)
	assert.ErrorIs(t, err, ErrBufferFull)

	// Segments 0, 1, 2 should survive.
	for i := range 3 {
		_, err := readSegment(s, uint32(i))
		assert.NoError(t, err, "segment %d should survive", i)
	}
}

func TestPreservesRecentSegments(t *testing.T) {
	clk := clock.NewMock(time.Now().Add(-1 * time.Hour))
	s := newTestStream(t, clk)

	ts := time.Now().UnixMilli()
	// Add 5 segments within capacity.
	for i := range 5 {
		mustCommitSlot(t, s, uint32(i), []byte{byte(i)}, ts+int64(i)*1000, 1000)
	}

	// All 5 should survive (cap is 100).
	for i := range 5 {
		_, err := readSegment(s, uint32(i))
		assert.NoError(t, err, "segment %d should survive", i)
	}
}

func TestBufferFull(t *testing.T) {
	clk := clock.NewMock(time.Now().Add(-1 * time.Hour))
	store := NewStore(clk)
	// workingSpace=1 so we can acquire a slot to attempt the 4th push.
	require.NoError(t, store.Init("1", Metadata{Bandwidth: 1000, TargetDurationSecs: 1}, []byte{0x00}, 0, 3, testSegmentBytes, 2, 1, testPlaylistWindowSize))
	t.Cleanup(func() { store.Delete("1") })
	s := store.Get("1")

	ts := time.Now().UnixMilli()
	// Fill buffer.
	for i := range 3 {
		mustCommitSlot(t, s, uint32(i), []byte{byte(i)}, ts+int64(i)*1000, 1000)
	}

	// Next segment should fail.
	err := commitSlot(t, s, 3, []byte{0x03}, ts+3000, 1000)
	assert.ErrorIs(t, err, ErrBufferFull)
}

func TestMultipleSegmentsReadable(t *testing.T) {
	clk := clock.NewMock(time.Now().Add(-1 * time.Hour))
	s := newTestStream(t, clk)

	ts := time.Now().UnixMilli()
	mustCommitSlot(t, s, 0, []byte{0xAA}, ts, 1000)
	mustCommitSlot(t, s, 1, []byte{0xBB}, ts+1000, 1000)

	s.mu.RLock()
	assert.Equal(t, 2, len(s.segments))
	s.mu.RUnlock()

	// Both segments should be readable.
	data, err := readSegment(s, 0)
	require.NoError(t, err)
	assert.Equal(t, []byte{0xAA}, data)

	data, err = readSegment(s, 1)
	require.NoError(t, err)
	assert.Equal(t, []byte{0xBB}, data)
}

func TestSegmentCount(t *testing.T) {
	clk := clock.NewMock(time.Now().Add(-1 * time.Hour))
	s := newTestStream(t, clk)

	assert.Equal(t, int64(0), s.TotalSegmentCount())

	ts := time.Now().UnixMilli()
	for i := range 5 {
		mustCommitSlot(t, s, uint32(i), []byte{byte(i)}, ts+int64(i)*1000, 1000)
	}

	assert.Equal(t, int64(5), s.TotalSegmentCount())

	// Duplicate should not increment count.
	_ = commitSlot(t, s, 0, []byte{0xFF}, ts, 1000)
	assert.Equal(t, int64(5), s.TotalSegmentCount(), "count after dup")
}

func TestSegmentOverflow(t *testing.T) {
	clk := clock.NewMock(time.Now().Add(-1 * time.Hour))
	store := NewStore(clk)
	err := store.Init("1", Metadata{Bandwidth: 1000, TargetDurationSecs: 1}, []byte{0x00}, 0, testCap, 4, testBackwardBufferSize, 0, testPlaylistWindowSize) // 4 bytes per slot
	require.NoError(t, err)
	t.Cleanup(func() { store.Delete("1") })
	s := store.Get("1")

	// 4 bytes should fit.
	ts := time.Now().UnixMilli()
	mustCommitSlot(t, s, 0, []byte{0x01, 0x02, 0x03, 0x04}, ts, 1000)

	// 5 bytes should overflow at ReadFrom, not at CommitSlot.
	buf, ok := s.AcquireSlot()
	require.True(t, ok)
	_, err = buf.ReadFrom(bytes.NewReader([]byte{0x01, 0x02, 0x03, 0x04, 0x05}))
	assert.ErrorIs(t, err, pool.ErrOverflow)
	s.ReleaseSlot(buf)
}

// --- Past-timestamp rejection tests ---

func TestRejectPastTimestamp(t *testing.T) {
	fixedTime := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	clk := clock.NewMock(fixedTime)
	nowMs := fixedTime.UnixMilli()

	s := newTestStream(t, clk)

	// First segment (future) succeeds.
	require.NoError(t, commitSlot(t, s, 0, []byte{0x01}, nowMs+1000, 1000))

	// Second segment with past timestamp should be rejected.
	err := commitSlot(t, s, 1, []byte{0x02}, nowMs-1000, 1000)
	assert.ErrorIs(t, err, ErrTimestampInPast)

	// Original segment should be unaffected.
	_, readErr := readSegment(s, 0)
	assert.NoError(t, readErr, "first segment should still be readable")
}

func TestAllowPastTimestampOnEmptyStream(t *testing.T) {
	fixedTime := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	clk := clock.NewMock(fixedTime)
	nowMs := fixedTime.UnixMilli()

	s := newTestStream(t, clk)

	// Past timestamp on empty stream should succeed (first segment exception).
	require.NoError(t, commitSlot(t, s, 0, []byte{0x01}, nowMs-5000, 1000))

	_, readErr := readSegment(s, 0)
	assert.NoError(t, readErr, "segment should be readable")
}

func TestAllowCurrentAndFutureTimestamp(t *testing.T) {
	fixedTime := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	clk := clock.NewMock(fixedTime)
	nowMs := fixedTime.UnixMilli()

	s := newTestStream(t, clk)

	// Timestamp exactly at "now" should be accepted.
	require.NoError(t, commitSlot(t, s, 0, []byte{0x01}, nowMs, 1000))

	// Timestamp in the future should be accepted.
	require.NoError(t, commitSlot(t, s, 1, []byte{0x02}, nowMs+5000, 1000))
}

func TestRejectPastTimestampDoesNotIncrementCount(t *testing.T) {
	fixedTime := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	clk := clock.NewMock(fixedTime)
	nowMs := fixedTime.UnixMilli()

	s := newTestStream(t, clk)

	require.NoError(t, commitSlot(t, s, 0, []byte{0x01}, nowMs+1000, 1000))
	require.Equal(t, int64(1), s.TotalSegmentCount())

	// Rejected segment should not increment count.
	_ = commitSlot(t, s, 1, []byte{0x02}, nowMs-1000, 1000)
	assert.Equal(t, int64(1), s.TotalSegmentCount(), "count after rejection")
}

func TestRunWithSegmentSlotNotFound(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	s := newTestStream(t, clk)

	_, err := readSegment(s, 999)
	assert.ErrorIs(t, err, ErrSegmentNotFound)
}

func TestConcurrentSegmentAccess(t *testing.T) {
	fixedTime := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	clk := clock.NewMock(fixedTime)
	s := newTestStream(t, clk)
	var wg sync.WaitGroup

	// Concurrent writers with unique indices.
	for i := range 50 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ts := fixedTime.Add(time.Duration(i+1) * time.Second).UnixMilli()
			_ = commitSlot(t, s, uint32(i), []byte{byte(i)}, ts, 2000)
		}(i)
	}

	// Concurrent readers.
	for i := range 50 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = readSegment(s, uint32(i))
		}(i)
	}

	wg.Wait()

	// All 50 segments should be present.
	for i := range 50 {
		_, err := readSegment(s, uint32(i))
		assert.NoError(t, err, "segment %d missing after concurrent writes", i)
	}
}

func TestReInitClearsSegments(t *testing.T) {
	clk := clock.NewMock(time.Now().Add(-1 * time.Hour))
	store := NewStore(clk)
	mustInit(t, store, "1", Metadata{Bandwidth: 1000, TargetDurationSecs: 1}, []byte{0x00}, testCap, testSegmentBytes, testBackwardBufferSize)
	s := store.Get("1")

	mustCommitSlot(t, s, 0, []byte{0xAA}, time.Now().UnixMilli(), 2000)

	// Re-init should clear segments.
	mustInit(t, store, "1", Metadata{Bandwidth: 2000, TargetDurationSecs: 1}, []byte{0x01}, testCap, testSegmentBytes, testBackwardBufferSize)
	s2 := store.Get("1")

	_, err := readSegment(s2, 0)
	assert.ErrorIs(t, err, ErrSegmentNotFound, "expected segments to be cleared after re-init")
}

func TestDeleteExisting(t *testing.T) {
	clk := clock.NewMock(time.Now().Add(-1 * time.Hour))
	s := NewStore(clk)
	mustInit(t, s, "1", Metadata{Bandwidth: 1000, TargetDurationSecs: 1}, []byte{0x01}, testCap, testSegmentBytes, testBackwardBufferSize)

	stream := s.Get("1")
	mustCommitSlot(t, stream, 0, []byte{0xAA}, time.Now().UnixMilli(), 2000)

	assert.True(t, s.Delete("1"), "Delete returned false for existing stream")
	assert.Nil(t, s.Get("1"), "expected stream to be gone after Delete")
}

func TestDeleteNonExistent(t *testing.T) {
	s := NewStore(clock.Real{})
	assert.False(t, s.Delete("999"), "Delete returned true for non-existent stream")
}

func TestDeleteDoesNotAffectOthers(t *testing.T) {
	s := NewStore(clock.Real{})
	mustInit(t, s, "2", Metadata{Bandwidth: 1, TargetDurationSecs: 1}, []byte{0x0A}, testCap, testSegmentBytes, testBackwardBufferSize)
	mustInit(t, s, "3", Metadata{Bandwidth: 2, TargetDurationSecs: 1}, []byte{0x0B}, testCap, testSegmentBytes, testBackwardBufferSize)

	s.Delete("2")

	assert.Nil(t, s.Get("2"), "stream 2 should be deleted")
	require.NotNil(t, s.Get("3"), "stream 3 should still exist")
	assert.Equal(t, 2, s.Get("3").Metadata().Bandwidth, "stream 3 metadata should be unchanged")
}

// --- Eviction tests ---

func TestEvictionRemovesOldBackwardSegments(t *testing.T) {
	t0 := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	clk := clock.NewMock(t0)

	store := NewStore(clk)
	// workingSpace=1 so commitSlot can acquire a slot when ring is near-full after eviction.
	require.NoError(t, store.Init("1", Metadata{Bandwidth: 1000, TargetDurationSecs: 1}, []byte{0x00}, 0, 10, testSegmentBytes, 2, 1, testPlaylistWindowSize))
	t.Cleanup(func() { store.Delete("1") })
	s := store.Get("1")

	// Add 5 segments all in the future relative to t0.
	for i := range 5 {
		ts := t0.UnixMilli() + int64((i+1)*1000)
		require.NoError(t, commitSlot(t, s, uint32(i), []byte{byte(i)}, ts, 1000))
	}

	// Advance clock so all 5 segments are now in the past.
	t1 := t0.Add(10 * time.Second)
	clk.Set(t1)

	// Add a new future segment to trigger eviction.
	require.NoError(t, commitSlot(t, s, 5, []byte{0x05}, t1.UnixMilli()+1000, 1000))

	// backwardBufferSize=2, so only the 2 most recent backward segments (3,4) should remain,
	// plus the new future segment (5). Segments 0,1,2 should be evicted.
	for i := range 3 {
		_, err := readSegment(s, uint32(i))
		assert.Error(t, err, "segment %d should have been evicted", i)
	}
	for i := 3; i <= 5; i++ {
		_, err := readSegment(s, uint32(i))
		assert.NoError(t, err, "segment %d should survive", i)
	}
}

func TestEvictionPreservesForwardSegments(t *testing.T) {
	t0 := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	clk := clock.NewMock(t0)

	store := NewStore(clk)
	require.NoError(t, store.Init("1", Metadata{Bandwidth: 1000, TargetDurationSecs: 1}, []byte{0x00}, 0, 10, testSegmentBytes, 1, 1, testPlaylistWindowSize))
	t.Cleanup(func() { store.Delete("1") })
	s := store.Get("1")

	// Add 6 segments all in the future relative to t0.
	for i := range 6 {
		ts := t0.UnixMilli() + int64((i+1)*1000)
		require.NoError(t, commitSlot(t, s, uint32(i), []byte{byte(i)}, ts, 1000))
	}

	// Advance clock so segments 0,1,2 are backward, 3,4,5 are forward.
	t1 := t0.Add(4 * time.Second)
	clk.Set(t1)

	// Add a new future segment to trigger eviction.
	// Segment 5 has ts = t0+6000, so this must be strictly after that.
	require.NoError(t, commitSlot(t, s, 6, []byte{0x06}, t0.UnixMilli()+7000, 1000))

	// backwardBufferSize=1: only segment 2 (most recent backward) should survive.
	// Segments 0,1 evicted. Segments 2,3,4,5,6 survive.
	for i := range 2 {
		_, err := readSegment(s, uint32(i))
		assert.Error(t, err, "backward segment %d should have been evicted", i)
	}
	for i := 2; i <= 6; i++ {
		_, err := readSegment(s, uint32(i))
		assert.NoError(t, err, "segment %d should survive", i)
	}
}

func TestEvictionBufferStillFull(t *testing.T) {
	fixedTime := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	clk := clock.NewMock(fixedTime)
	nowMs := fixedTime.UnixMilli()

	store := NewStore(clk)
	// capacity=3, backwardBufferSize=2, workingSpace=1, all segments in the future.
	require.NoError(t, store.Init("1", Metadata{Bandwidth: 1000, TargetDurationSecs: 1}, []byte{0x00}, 0, 3, testSegmentBytes, 2, 1, testPlaylistWindowSize))
	t.Cleanup(func() { store.Delete("1") })
	s := store.Get("1")

	for i := range 3 {
		ts := nowMs + int64((i+1)*1000)
		require.NoError(t, commitSlot(t, s, uint32(i), []byte{byte(i)}, ts, 1000))
	}

	// Buffer full with all forward segments — nothing to evict.
	err := commitSlot(t, s, 3, []byte{0x03}, nowMs+4000, 1000)
	assert.ErrorIs(t, err, ErrBufferFull)
}

func TestEvictionBoundaryExactCount(t *testing.T) {
	t0 := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	clk := clock.NewMock(t0)

	store := NewStore(clk)
	require.NoError(t, store.Init("1", Metadata{Bandwidth: 1000, TargetDurationSecs: 1}, []byte{0x00}, 0, 10, testSegmentBytes, 3, 1, testPlaylistWindowSize))
	t.Cleanup(func() { store.Delete("1") })
	s := store.Get("1")

	// Add 3 segments in the future.
	for i := range 3 {
		ts := t0.UnixMilli() + int64((i+1)*1000)
		require.NoError(t, commitSlot(t, s, uint32(i), []byte{byte(i)}, ts, 1000))
	}

	// Advance clock so all 3 are backward.
	t1 := t0.Add(10 * time.Second)
	clk.Set(t1)

	// Add a future segment to trigger eviction check.
	require.NoError(t, commitSlot(t, s, 3, []byte{0x03}, t1.UnixMilli()+1000, 1000))

	// backwardBufferSize=3: all 3 backward segments should survive (exactly at limit).
	for i := range 3 {
		_, err := readSegment(s, uint32(i))
		assert.NoError(t, err, "segment %d should survive at exact boundary", i)
	}
}

func TestEvictionOnEveryAdd(t *testing.T) {
	t0 := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	clk := clock.NewMock(t0)

	store := NewStore(clk)
	require.NoError(t, store.Init("1", Metadata{Bandwidth: 1000, TargetDurationSecs: 1}, []byte{0x00}, 0, 10, testSegmentBytes, 1, 1, testPlaylistWindowSize))
	t.Cleanup(func() { store.Delete("1") })
	s := store.Get("1")

	// Add 5 segments, advancing the clock before each so that previous segments
	// become backward. Each add triggers eviction, keeping at most 1 backward segment.
	for i := range 5 {
		ts := clk.Now().UnixMilli() + 1000 // 1s in the future
		require.NoError(t, commitSlot(t, s, uint32(i), []byte{byte(i)}, ts, 1000))
		// Advance clock past this segment's timestamp.
		clk.Set(clk.Now().Add(2 * time.Second))
	}

	// Segment 3 (most recent backward) and segment 4 (forward) should survive.
	// backwardBufferSize=1 keeps exactly 1 backward segment; the latest add is still forward.
	s.mu.RLock()
	count := len(s.segments)
	s.mu.RUnlock()

	assert.Equal(t, 2, count, "expected 2 segments after eviction (1 backward + 1 forward)")

	// Segments 0,1,2 should be evicted.
	for i := range 3 {
		_, err := readSegment(s, uint32(i))
		assert.Error(t, err, "segment %d should have been evicted", i)
	}
	// Segments 3 (backward) and 4 (forward) should survive.
	for _, idx := range []uint32{3, 4} {
		_, err := readSegment(s, idx)
		assert.NoError(t, err, "segment %d should survive", idx)
	}
}

func TestEvictionWithAllForwardSegments(t *testing.T) {
	fixedTime := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	clk := clock.NewMock(fixedTime)
	nowMs := fixedTime.UnixMilli()

	store := NewStore(clk)
	require.NoError(t, store.Init("1", Metadata{Bandwidth: 1000, TargetDurationSecs: 1}, []byte{0x00}, 0, 5, testSegmentBytes, 1, 1, testPlaylistWindowSize))
	t.Cleanup(func() { store.Delete("1") })
	s := store.Get("1")

	// All segments in the future — nothing evictable.
	for i := range 5 {
		ts := nowMs + int64((i+1)*1000)
		require.NoError(t, commitSlot(t, s, uint32(i), []byte{byte(i)}, ts, 1000))
	}

	// All 5 should survive.
	for i := range 5 {
		_, err := readSegment(s, uint32(i))
		assert.NoError(t, err, "forward segment %d should survive", i)
	}
}

func TestPoolBufferReuse(t *testing.T) {
	currentTime := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	clk := clock.NewMock(currentTime)
	nowMs := currentTime.UnixMilli()

	const segCap = 4
	const bbs = 2 // backwardBufferSize

	store := NewStore(clk)
	// workingSpace=1 so commitSlot can acquire a slot after eviction frees some.
	require.NoError(t, store.Init("1", Metadata{Bandwidth: 1000, TargetDurationSecs: 1}, []byte{0x00}, 0, segCap, testSegmentBytes, bbs, 1, testPlaylistWindowSize))
	t.Cleanup(func() { store.Delete("1") })
	s := store.Get("1")

	// Fill the buffer with 4 future segments (no eviction).
	for i := range segCap {
		ts := nowMs + int64((i+1)*1000)
		require.NoError(t, commitSlot(t, s, uint32(i), []byte{byte(i)}, ts, 1000))
	}

	// Pool should have 1 free buffer (workingSpace=1, all 4 segment slots checked out).
	s.mu.RLock()
	poolLen := s.bufPool.FreeCount()
	segCount := len(s.segments)
	s.mu.RUnlock()
	assert.Equal(t, 1, poolLen, "pool FreeCount when segment buffer full (workingSpace=1)")
	assert.Equal(t, segCap, segCount)

	// Advance time so all segments become backward.
	clk.Set(currentTime.Add(10 * time.Second))
	nowMs = clk.Now().UnixMilli()

	// Add a new future segment — eviction should free buffers back to pool.
	ts := nowMs + 500
	require.NoError(t, commitSlot(t, s, uint32(segCap), []byte{byte(segCap)}, ts, 1000))

	// The 4 original segments are backward. backwardBufferSize=2.
	// Eviction limit = bbs = 2 (incoming is future).
	// Evict 2 of the original 4 backward segments, then push the new one.
	// Result: 3 segments occupied, pool has (segCap+1)-3 = 2 free.
	s.mu.RLock()
	poolLen = s.bufPool.FreeCount()
	segCount = len(s.segments)
	s.mu.RUnlock()

	assert.Equal(t, 3, segCount, "segments after eviction+push")
	assert.Equal(t, segCap+1-segCount, poolLen, "pool FreeCount after eviction")

	// Verify the surviving segments are readable.
	for i := range segCount {
		s.mu.RLock()
		idx := s.segments[i].Index
		s.mu.RUnlock()
		_, err := readSegment(s, idx)
		assert.NoError(t, err, "segment %d should be readable", idx)
	}
}

func TestValidateCodecs(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		// Valid codec strings
		{"valid HEVC", "hvc1.1.6.L120.90", false},
		{"valid AVC", "avc1.64001f", false},
		{"valid with comma", "hvc1.1.6.L120.90,mp4a.40.2", false},
		{"valid AV1", "av01.0.08M.08", false},
		{"valid AC-3", "ac-3", false},
		{"valid with plus", "ec+3", false},
		{"at max length", strings.Repeat("a", MaxCodecsLength), false},

		// Invalid: empty / too long
		{"empty", "", true},
		{"exceeds max length", strings.Repeat("a", MaxCodecsLength+1), true},

		// Invalid: control characters and quotes
		{"contains double quote", `hvc1"injected`, true},
		{"contains newline", "hvc1\ninjected", true},
		{"contains carriage return", "hvc1\rinjected", true},
		{"contains null byte", "hvc1\x00injected", true},
		{"contains tab", "hvc1\tinjected", true},
		{"contains DEL", "hvc1\x7Finjected", true},

		// Invalid: HTML-significant and other disallowed characters
		{"contains space", "avc1 mp4a", true},
		{"contains angle bracket open", "avc1<script>", true},
		{"contains angle bracket close", "avc1>mp4a", true},
		{"contains ampersand", "avc1&mp4a", true},
		{"contains slash", "avc1/mp4a", true},
		{"contains semicolon", "avc1;mp4a", true},
		{"contains equals", "avc1=mp4a", true},
		{"contains single quote", "avc1'mp4a", true},
		{"contains parenthesis", "avc1(mp4a)", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateCodecs(tt.input)
			if tt.wantErr {
				assert.Error(t, err, "ValidateCodecs(%q)", tt.input)
			} else {
				assert.NoError(t, err, "ValidateCodecs(%q)", tt.input)
			}
		})
	}
}

// --- Working space tests ---

func TestWorkingSpaceAllowsConcurrentAcquire(t *testing.T) {
	clk := clock.NewMock(time.Now().Add(-1 * time.Hour))
	store := NewStore(clk)
	// Ring buffer capacity=3, working space=5 → pool has 8 slots.
	err := store.Init("1", Metadata{Bandwidth: 1000, TargetDurationSecs: 1}, []byte{0x00}, 0, 3, testSegmentBytes, 2, 5, testPlaylistWindowSize)
	require.NoError(t, err)
	t.Cleanup(func() { store.Delete("1") })
	s := store.Get("1")

	ts := time.Now().UnixMilli()

	// Fill the ring buffer.
	for i := range 3 {
		mustCommitSlot(t, s, uint32(i), []byte{byte(i)}, ts+int64(i)*1000, 1000)
	}

	// All 5 working space slots should be acquirable even with ring full.
	acquired := make([]*pool.BufferSlot, 0, 5)
	for range 5 {
		buf, ok := s.AcquireSlot()
		require.True(t, ok, "should be able to acquire working space slot")
		acquired = append(acquired, buf)
	}

	// 6th should fail (all 8 slots in use: 3 ring + 5 working).
	_, ok := s.AcquireSlot()
	assert.False(t, ok, "should not be able to acquire beyond working space")

	// Release all working slots.
	for _, buf := range acquired {
		s.ReleaseSlot(buf)
	}
}

// --- Active reader eviction tests ---

func TestEvictionPreservesSegmentWithActiveReader(t *testing.T) {
	t0 := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	clk := clock.NewMock(t0)

	store := NewStore(clk)
	// backwardBufferSize=1, so eviction wants to keep only 1 backward segment.
	require.NoError(t, store.Init("1", Metadata{Bandwidth: 1000, TargetDurationSecs: 1}, []byte{0x00}, 0, 10, testSegmentBytes, 1, 1, testPlaylistWindowSize))
	t.Cleanup(func() { store.Delete("1") })
	s := store.Get("1")

	// Add 3 future segments.
	for i := range 3 {
		ts := t0.UnixMilli() + int64((i+1)*1000)
		require.NoError(t, commitSlot(t, s, uint32(i), []byte{byte(i)}, ts, 1000))
	}

	// Simulate an active reader on segment 0.
	s.mu.RLock()
	seg0Data := s.segments[0].Data
	seg0Data.ReaderInc()
	s.mu.RUnlock()

	// Advance clock so all 3 segments are backward.
	t1 := t0.Add(10 * time.Second)
	clk.Set(t1)

	// Add a new future segment — triggers eviction.
	// Without the fix, segments 0 and 1 would be evicted (backwardBufferSize=1 keeps only seg 2).
	// With the fix, eviction stops at segment 0 (refcount > 0), preserving 0, 1, 2.
	require.NoError(t, commitSlot(t, s, 3, []byte{0x03}, t1.UnixMilli()+1000, 1000))

	// Segment 0 should still exist because its buffer has an active reader.
	_, err := readSegment(s, 0)
	assert.NoError(t, err, "segment 0 should survive eviction due to active reader")

	// Segment 1 should also survive (eviction stopped before it).
	_, err = readSegment(s, 1)
	assert.NoError(t, err, "segment 1 should survive because eviction stopped at segment 0")

	// Release the reader.
	seg0Data.ReaderDec()

	// Now trigger eviction again — segments 0 and 1 should be evictable.
	require.NoError(t, commitSlot(t, s, 4, []byte{0x04}, t1.UnixMilli()+2000, 1000))

	// With backwardBufferSize=1, segments 0,1,2 are backward, only 2 should survive.
	// Segments 0 and 1 should now be evicted.
	_, err = readSegment(s, 0)
	assert.Error(t, err, "segment 0 should be evicted after reader released")
	_, err = readSegment(s, 1)
	assert.Error(t, err, "segment 1 should be evicted after reader released")

	// Segments 2 (backward, within limit), 3, 4 (forward) should survive.
	for _, idx := range []uint32{2, 3, 4} {
		_, err = readSegment(s, idx)
		assert.NoError(t, err, "segment %d should survive", idx)
	}
}

func TestEvictionSkipsOnlyLeadingActiveReaders(t *testing.T) {
	t0 := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	clk := clock.NewMock(t0)

	store := NewStore(clk)
	// backwardBufferSize=1.
	require.NoError(t, store.Init("1", Metadata{Bandwidth: 1000, TargetDurationSecs: 1}, []byte{0x00}, 0, 10, testSegmentBytes, 1, 1, testPlaylistWindowSize))
	t.Cleanup(func() { store.Delete("1") })
	s := store.Get("1")

	// Add 4 future segments.
	for i := range 4 {
		ts := t0.UnixMilli() + int64((i+1)*1000)
		require.NoError(t, commitSlot(t, s, uint32(i), []byte{byte(i)}, ts, 1000))
	}

	// Active reader on segment 1 (not segment 0).
	s.mu.RLock()
	seg1Data := s.segments[1].Data
	seg1Data.ReaderInc()
	s.mu.RUnlock()

	// Advance clock so all 4 are backward.
	t1 := t0.Add(10 * time.Second)
	clk.Set(t1)

	// Trigger eviction. evictCount would be 3 (4 backward - 1 limit).
	// Segment 0 has no readers → evicted. Segment 1 has a reader → stop.
	require.NoError(t, commitSlot(t, s, 4, []byte{0x04}, t1.UnixMilli()+1000, 1000))

	// Segment 0 should be evicted (no reader, before the active-reader segment).
	_, err := readSegment(s, 0)
	assert.Error(t, err, "segment 0 should be evicted")

	// Segments 1,2,3 survive (eviction stopped at 1).
	for _, idx := range []uint32{1, 2, 3, 4} {
		_, err = readSegment(s, idx)
		assert.NoError(t, err, "segment %d should survive", idx)
	}

	seg1Data.ReaderDec()
}

func TestConcurrentReadersAndEviction(t *testing.T) {
	t0 := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	clk := clock.NewMock(t0)

	store := NewStore(clk)
	require.NoError(t, store.Init("1", Metadata{Bandwidth: 1000, TargetDurationSecs: 1}, []byte{0x00}, 0, 20, testSegmentBytes, 2, 2, testPlaylistWindowSize))
	t.Cleanup(func() { store.Delete("1") })
	s := store.Get("1")

	// Add 10 segments.
	for i := range 10 {
		ts := t0.UnixMilli() + int64((i+1)*1000)
		require.NoError(t, commitSlot(t, s, uint32(i), []byte{byte(i)}, ts, 1000))
	}

	const numReaders = 10

	// readersReady: each reader signals when it has acquired its refcount.
	// releaseReaders: closed to tell readers they can finish.
	readersReady := make(chan struct{}, numReaders)
	releaseReaders := make(chan struct{})

	var readerWg sync.WaitGroup

	// Concurrent readers on various segments. Each reader acquires its
	// refcount, signals readersReady, then blocks on releaseReaders.
	for i := range numReaders {
		readerWg.Add(1)
		go func(i int) {
			defer readerWg.Done()
			_ = s.RunWithSegmentSlot(uint32(i), func(slot *pool.BufferSlot) error {
				readersReady <- struct{}{}
				<-releaseReaders
				var buf bytes.Buffer
				_, err := slot.WriteTo(&buf)
				return err
			})
		}(i)
	}

	// Wait for all readers to be holding their refcounts.
	for range numReaders {
		<-readersReady
	}

	// Advance time so all 10 segments become backward, then commit new
	// segments to trigger eviction while readers hold references.
	clk.Set(t0.Add(20 * time.Second))

	writerErrs := make(chan error, 5)
	var writerWg sync.WaitGroup
	writerWg.Add(1)
	go func() {
		defer writerWg.Done()
		for i := 10; i < 15; i++ {
			buf, ok := s.AcquireSlot()
			if !ok {
				writerErrs <- fmt.Errorf("AcquireSlot failed for segment %d", i)
				continue
			}
			if _, err := buf.ReadFrom(bytes.NewReader([]byte{byte(i)})); err != nil {
				s.ReleaseSlot(buf)
				writerErrs <- fmt.Errorf("ReadFrom failed for segment %d: %w", i, err)
				continue
			}
			ts := t0.Add(20*time.Second).UnixMilli() + int64(i)*1000
			if err := s.CommitSlot(uint32(i), buf, ts, 1000, 0); err != nil {
				s.ReleaseSlot(buf)
				writerErrs <- fmt.Errorf("CommitSlot failed for segment %d: %w", i, err)
			}
		}
	}()

	// Wait for writer to finish (eviction has run while readers hold refs).
	writerWg.Wait()
	close(writerErrs)
	for err := range writerErrs {
		t.Errorf("writer goroutine error: %v", err)
	}

	// Release readers and wait for them to complete.
	close(releaseReaders)
	readerWg.Wait()
	// No panics or data races = success. Run with -race to verify.
}

func TestHasSegments_ClosedOnFirstCommit(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))

	store := NewStore(clk)
	require.NoError(t, store.Init("1", Metadata{Bandwidth: 1000, TargetDurationSecs: 2}, []byte{0x00}, 0, 10, testSegmentBytes, 1, 1, testPlaylistWindowSize))
	t.Cleanup(func() { store.Delete("1") })
	s := store.Get("1")
	require.NotNil(t, s)

	// Channel should not be closed yet.
	select {
	case <-s.HasSegments():
		t.Fatal("HasSegments should not be closed before any commit")
	default:
	}

	mustCommitSlot(t, s, 0, []byte("data"), 1000, 2000)

	// Channel should be closed now.
	select {
	case <-s.HasSegments():
	default:
		t.Fatal("HasSegments should be closed after first commit")
	}

	// Still closed after second commit (idempotent).
	mustCommitSlot(t, s, 1, []byte("data"), 3000, 2000)
	select {
	case <-s.HasSegments():
	default:
		t.Fatal("HasSegments should remain closed after subsequent commits")
	}
}

func TestRendererGoroutine_UpdatesCachedPlaylist(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))

	store := NewStore(clk)
	require.NoError(t, store.Init("1", Metadata{Bandwidth: 1000, TargetDurationSecs: 2}, []byte{0x00}, 0, 10, testSegmentBytes, 1, 1, testPlaylistWindowSize))
	t.Cleanup(func() { store.Delete("1") })
	s := store.Get("1")
	require.NotNil(t, s)

	// Initially empty.
	assert.Equal(t, "", s.CachedPlaylist())

	mustCommitSlot(t, s, 0, []byte("data"), 0, 2000)

	// Wait for renderer to update (it runs asynchronously).
	require.Eventually(t, func() bool {
		return s.CachedPlaylist() != ""
	}, 2*time.Second, 10*time.Millisecond, "cached playlist should be populated after commit")

	playlist := s.CachedPlaylist()
	assert.Contains(t, playlist, "#EXTM3U")
	assert.Contains(t, playlist, "segment_0.m4s")
}

func TestRendererGoroutine_StopsOnDelete(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))

	store := NewStore(clk)
	require.NoError(t, store.Init("1", Metadata{Bandwidth: 1000, TargetDurationSecs: 2}, []byte{0x00}, 0, 10, testSegmentBytes, 1, 1, testPlaylistWindowSize))
	t.Cleanup(func() { store.Delete("1") })
	s := store.Get("1")
	require.NotNil(t, s)

	// Delete the stream — the done channel should close.
	store.Delete("1")

	// The done channel should be closed.
	select {
	case <-s.done:
	case <-time.After(1 * time.Second):
		t.Fatal("done channel should be closed after Delete")
	}
}

func TestInitRejectsZeroTargetDuration(t *testing.T) {
	store := NewStore(clock.Real{})

	// TargetDurationSecs=0 should fail.
	err := store.Init("1", Metadata{TargetDurationSecs: 0}, []byte{0x00}, 0, 10, testSegmentBytes, 9, 0, testPlaylistWindowSize)
	assert.ErrorIs(t, err, ErrInvalidTargetDuration, "zero")

	// Negative TargetDurationSecs should fail.
	err = store.Init("1", Metadata{TargetDurationSecs: -1}, []byte{0x00}, 0, 10, testSegmentBytes, 9, 0, testPlaylistWindowSize)
	assert.ErrorIs(t, err, ErrInvalidTargetDuration, "negative")

	// Valid TargetDurationSecs should succeed.
	err = store.Init("1", Metadata{TargetDurationSecs: 2}, []byte{0x00}, 0, 10, testSegmentBytes, 9, 0, testPlaylistWindowSize)
	assert.NoError(t, err, "valid")
	t.Cleanup(func() { store.Delete("1") })
}

func TestInitRejectsZeroPlaylistWindowSize(t *testing.T) {
	store := NewStore(clock.Real{})

	// playlistWindowSize=0 should fail.
	err := store.Init("1", Metadata{TargetDurationSecs: 2}, []byte{0x00}, 0, 10, testSegmentBytes, 9, 0, 0)
	assert.ErrorIs(t, err, ErrInvalidPlaylistWindowSize, "zero")

	// Negative playlistWindowSize should fail.
	err = store.Init("1", Metadata{TargetDurationSecs: 2}, []byte{0x00}, 0, 10, testSegmentBytes, 9, 0, -1)
	assert.ErrorIs(t, err, ErrInvalidPlaylistWindowSize, "negative")

	// Valid playlistWindowSize should succeed.
	err = store.Init("1", Metadata{TargetDurationSecs: 2}, []byte{0x00}, 0, 10, testSegmentBytes, 9, 0, 5)
	assert.NoError(t, err, "valid")
	t.Cleanup(func() { store.Delete("1") })
}

func TestDone_ClosedOnDelete(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))

	store := NewStore(clk)
	require.NoError(t, store.Init("1", Metadata{Bandwidth: 1000, TargetDurationSecs: 2}, []byte{0x00}, 0, 10, testSegmentBytes, 1, 1, testPlaylistWindowSize))
	s := store.Get("1")
	require.NotNil(t, s)

	// Done channel should not be closed yet.
	select {
	case <-s.Done():
		t.Fatal("Done should not be closed before delete")
	default:
	}

	store.Delete("1")

	// Done channel should be closed now.
	select {
	case <-s.Done():
	case <-time.After(1 * time.Second):
		t.Fatal("Done should be closed after delete")
	}
}

func TestRendererGoroutine_ClearsPlaylistWhenEmpty(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))

	store := NewStore(clk)
	require.NoError(t, store.Init("1", Metadata{Bandwidth: 1000, TargetDurationSecs: 2}, []byte{0x00}, 0, 10, testSegmentBytes, 1, 1, testPlaylistWindowSize))
	t.Cleanup(func() { store.Delete("1") })
	s := store.Get("1")
	require.NotNil(t, s)

	// Commit a segment at ts=0, eligible immediately.
	mustCommitSlot(t, s, 0, []byte("data"), 0, 2000)

	// Wait for renderer to populate.
	require.Eventually(t, func() bool {
		return s.CachedPlaylist() != ""
	}, 2*time.Second, 10*time.Millisecond, "cached playlist should be populated")

	// Now move clock far back so no segments are eligible (all in the future).
	clk.Set(time.UnixMilli(-10000))

	// Trigger a re-render by committing another segment with a future timestamp.
	mustCommitSlot(t, s, 1, []byte("data"), 50000, 2000)

	// Renderer should clear the playlist since no segments are eligible at now=-10000.
	require.Eventually(t, func() bool {
		return s.CachedPlaylist() == ""
	}, 2*time.Second, 10*time.Millisecond, "cached playlist should be cleared when no segments eligible")
}

func TestHasPlaylist_ClosedWhenRendererProducesPlaylist(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))

	store := NewStore(clk)
	require.NoError(t, store.Init("1", Metadata{Bandwidth: 1000, TargetDurationSecs: 2}, []byte{0x00}, 0, 10, testSegmentBytes, 1, 1, testPlaylistWindowSize))
	t.Cleanup(func() { store.Delete("1") })
	s := store.Get("1")
	require.NotNil(t, s)

	// HasPlaylist should not be closed yet (no segments, no playlist).
	select {
	case <-s.HasPlaylist():
		t.Fatal("HasPlaylist should not be closed before any playlist is rendered")
	default:
	}

	// Commit a segment eligible at ts=0 (now=0, so Timestamp <= nowMs).
	mustCommitSlot(t, s, 0, []byte("data"), 0, 2000)

	// HasPlaylist should close once the renderer produces the playlist.
	select {
	case <-s.HasPlaylist():
	case <-time.After(2 * time.Second):
		t.Fatal("HasPlaylist should be closed after renderer produces a valid playlist")
	}

	// CachedPlaylist should contain a valid HLS header.
	playlist := s.CachedPlaylist()
	assert.True(t, strings.HasPrefix(playlist, "#EXTM3U\n"))
}

func TestHasPlaylist_NotClosedWhenNoSegmentsEligible(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))

	store := NewStore(clk)
	require.NoError(t, store.Init("1", Metadata{Bandwidth: 1000, TargetDurationSecs: 2}, []byte{0x00}, 0, 10, testSegmentBytes, 1, 1, testPlaylistWindowSize))
	t.Cleanup(func() { store.Delete("1") })
	s := store.Get("1")
	require.NotNil(t, s)

	// Commit a segment in the future — no segments eligible at now=0.
	mustCommitSlot(t, s, 0, []byte("data"), 5000, 2000)

	// Give the renderer time to run.
	time.Sleep(50 * time.Millisecond)

	// HasPlaylist should still be open since renderer produces "".
	select {
	case <-s.HasPlaylist():
		t.Fatal("HasPlaylist should not be closed when no segments are eligible")
	default:
	}
}

// --- Generation tests ---

func TestCommitSlot_StaleGeneration(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	store := NewStore(clk)
	meta := Metadata{Bandwidth: 1, Codecs: "avc1.64001f", Width: 1, Height: 1, FrameRate: 30, TargetDurationSecs: 2}
	mustInit(t, store, "g", meta, []byte("init"), 10, testSegmentBytes, 5)
	s := store.Get("g")

	require.NoError(t, s.AddInitEntry(3, []byte("init3")))
	require.NoError(t, s.AddInitEntry(5, []byte("init5")))

	// Push gen=5.
	err := commitSlotGen(t, s, 0, []byte("data"), 5000, 2000, 5)
	require.NoError(t, err)
	assert.Equal(t, int64(5), s.CurrentGeneration())

	// Push gen=3 → stale.
	err = commitSlotGen(t, s, 1, []byte("data"), 7000, 2000, 3)
	assert.ErrorIs(t, err, ErrStaleGeneration)
}

func TestCommitSlot_GenerationAdvance_DropsStaleSegments(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	store := NewStore(clk)
	meta := Metadata{Bandwidth: 1, Codecs: "avc1.64001f", Width: 1, Height: 1, FrameRate: 30, TargetDurationSecs: 2}
	mustInit(t, store, "g", meta, []byte("init"), 10, testSegmentBytes, 5)
	s := store.Get("g")

	require.NoError(t, s.AddInitEntry(1, []byte("init1")))

	// Push 5 segments at gen=0.
	for i := uint32(0); i < 5; i++ {
		err := commitSlotGen(t, s, i, []byte("data"), int64(5000+i*2000), 2000, 0)
		require.NoError(t, err)
	}
	segCount, _ := s.SegmentLoad()
	assert.Equal(t, 5, segCount)

	// Push gen=1 at index=2 → segments 2,3,4 (gen=0) should be dropped.
	err := commitSlotGen(t, s, 2, []byte("new"), 9000, 2000, 1)
	require.NoError(t, err)

	segCount, _ = s.SegmentLoad()
	assert.Equal(t, 3, segCount) // indices 0,1 (gen=0 before insertion point) + index 2 (gen=1)

	// Old indices 3,4 should be gone.
	_, err = readSegment(s, 3)
	assert.ErrorIs(t, err, ErrSegmentNotFound)
	_, err = readSegment(s, 4)
	assert.ErrorIs(t, err, ErrSegmentNotFound)

	// Indices 0,1 should still be readable.
	_, err = readSegment(s, 0)
	assert.NoError(t, err)
	_, err = readSegment(s, 1)
	assert.NoError(t, err)
}

func TestCommitSlot_SameGeneration_NoDrop(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	store := NewStore(clk)
	meta := Metadata{Bandwidth: 1, Codecs: "avc1.64001f", Width: 1, Height: 1, FrameRate: 30, TargetDurationSecs: 2}
	mustInit(t, store, "g", meta, []byte("init"), 10, testSegmentBytes, 5)
	s := store.Get("g")

	for i := uint32(0); i < 5; i++ {
		err := commitSlotGen(t, s, i, []byte("data"), int64(5000+i*2000), 2000, 0)
		require.NoError(t, err)
	}
	segCount, _ := s.SegmentLoad()
	assert.Equal(t, 5, segCount)
}

func TestCommitSlot_GenerationDropFreesCapacity(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	store := NewStore(clk)
	meta := Metadata{Bandwidth: 1, Codecs: "avc1.64001f", Width: 1, Height: 1, FrameRate: 30, TargetDurationSecs: 2}
	// Capacity of 5, backward buffer 4, workingSpace 1 so AcquireSlot can
	// succeed even when the segment list is full (the extra pool slot is
	// needed to hold the new buffer before CommitSlot drops stale segments).
	err := store.Init("g", meta, []byte("init"), 0, 5, testSegmentBytes, 4, 1, testPlaylistWindowSize)
	require.NoError(t, err)
	t.Cleanup(func() { store.Delete("g") })
	s := store.Get("g")

	require.NoError(t, s.AddInitEntry(1, []byte("init1")))

	// Fill to capacity with gen=0.
	for i := uint32(0); i < 5; i++ {
		err := commitSlotGen(t, s, i, []byte("data"), int64(5000+i*2000), 2000, 0)
		require.NoError(t, err)
	}

	// Buffer is full. A same-gen push would fail.
	err = commitSlotGen(t, s, 5, []byte("data"), 15000, 2000, 0)
	assert.ErrorIs(t, err, ErrBufferFull)

	// But a gen advance at index 2 drops 2,3,4 → frees 3 slots.
	err = commitSlotGen(t, s, 2, []byte("new"), 9000, 2000, 1)
	require.NoError(t, err)

	segCount, _ := s.SegmentLoad()
	assert.Equal(t, 3, segCount) // 0,1 (gen=0) + 2 (gen=1)
}

func TestCommitSlot_GenerationAdvanceThenContinue(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	store := NewStore(clk)
	meta := Metadata{Bandwidth: 1, Codecs: "avc1.64001f", Width: 1, Height: 1, FrameRate: 30, TargetDurationSecs: 2}
	mustInit(t, store, "g", meta, []byte("init"), 10, testSegmentBytes, 5)
	s := store.Get("g")

	require.NoError(t, s.AddInitEntry(1, []byte("init1")))

	// Push gen=0 segments.
	for i := uint32(0); i < 3; i++ {
		err := commitSlotGen(t, s, i, []byte("data"), int64(5000+i*2000), 2000, 0)
		require.NoError(t, err)
	}

	// Advance to gen=1 at index 2.
	err := commitSlotGen(t, s, 2, []byte("new"), 9000, 2000, 1)
	require.NoError(t, err)

	// Continue pushing gen=1 segments.
	err = commitSlotGen(t, s, 3, []byte("new2"), 11000, 2000, 1)
	require.NoError(t, err)
	err = commitSlotGen(t, s, 4, []byte("new3"), 13000, 2000, 1)
	require.NoError(t, err)

	segCount, _ := s.SegmentLoad()
	assert.Equal(t, 5, segCount) // 0,1 (gen=0) + 2,3,4 (gen=1)
}

func TestCommitSlot_SameGenDropsStaleOnSubsequentInsert(t *testing.T) {
	// Scenario: 10 gen=0 segments, then gen=1 at index 9 (drops only the old
	// index 9), then gen=1 at index 5. The second insert must still drop the
	// stale gen=0 segments at indices 5-8 even though the generation did not
	// advance on this call.
	clk := clock.NewMock(time.UnixMilli(0))
	store := NewStore(clk)
	meta := Metadata{Bandwidth: 1, Codecs: "avc1.64001f", Width: 1, Height: 1, FrameRate: 30, TargetDurationSecs: 2}
	mustInit(t, store, "g", meta, []byte("init"), 20, testSegmentBytes, 15)
	s := store.Get("g")

	require.NoError(t, s.AddInitEntry(1, []byte("init1")))

	// Push 10 gen=0 segments (indices 0-9).
	for i := uint32(0); i < 10; i++ {
		err := commitSlotGen(t, s, i, []byte("data"), int64(5000+i*2000), 2000, 0)
		require.NoError(t, err)
	}
	segCount, _ := s.SegmentLoad()
	require.Equal(t, 10, segCount)

	// Advance to gen=1 at index 9. Only old index 9 (gen=0) is at/after
	// the insertion point, so it gets dropped and replaced.
	err := commitSlotGen(t, s, 9, []byte("new9"), 23000, 2000, 1)
	require.NoError(t, err)
	segCount, _ = s.SegmentLoad()
	assert.Equal(t, 10, segCount) // 0-8 (gen=0) + 9 (gen=1)

	// Now push gen=1 at index 5. Generation is NOT advancing (already 1),
	// but stale gen=0 segments at indices 5,6,7,8 must still be dropped.
	err = commitSlotGen(t, s, 5, []byte("new5"), 15000, 2000, 1)
	require.NoError(t, err)

	segCount, _ = s.SegmentLoad()
	assert.Equal(t, 7, segCount) // 0-4 (gen=0) + 5,9 (gen=1)

	// Verify stale indices are gone.
	for _, idx := range []uint32{6, 7, 8} {
		_, err := readSegment(s, idx)
		assert.ErrorIs(t, err, ErrSegmentNotFound, "index %d should be dropped", idx)
	}

	// Verify kept indices are readable.
	for _, idx := range []uint32{0, 1, 2, 3, 4, 5, 9} {
		_, err := readSegment(s, idx)
		assert.NoError(t, err, "index %d should be readable", idx)
	}
}

func TestCommitSlot_DefaultGenerationZero(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	store := NewStore(clk)
	meta := Metadata{Bandwidth: 1, Codecs: "avc1.64001f", Width: 1, Height: 1, FrameRate: 30, TargetDurationSecs: 2}
	mustInit(t, store, "g", meta, []byte("init"), 10, testSegmentBytes, 5)
	s := store.Get("g")

	// Passing generation 0 works like pre-feature behavior.
	err := commitSlotGen(t, s, 0, []byte("data"), 5000, 2000, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(0), s.CurrentGeneration())

	err = commitSlotGen(t, s, 1, []byte("data2"), 7000, 2000, 0)
	require.NoError(t, err)

	segCount, _ := s.SegmentLoad()
	assert.Equal(t, 2, segCount)
}

func TestCommitSlot_ZeroGenerationStaleAfterAdvance(t *testing.T) {
	// Once the stream has advanced past generation 0, a subsequent push
	// with generation=0 (the default / missing-header case) must be
	// rejected as stale — generation 0 is not special.
	clk := clock.NewMock(time.UnixMilli(0))
	store := NewStore(clk)
	meta := Metadata{Bandwidth: 1, Codecs: "avc1.64001f", Width: 1, Height: 1, FrameRate: 30, TargetDurationSecs: 2}
	mustInit(t, store, "g", meta, []byte("init"), 10, testSegmentBytes, 5)
	s := store.Get("g")

	require.NoError(t, s.AddInitEntry(3, []byte("init3")))

	// Advance to generation 3.
	err := commitSlotGen(t, s, 0, []byte("data"), 5000, 2000, 3)
	require.NoError(t, err)
	assert.Equal(t, int64(3), s.CurrentGeneration())

	// Push with generation 0 → stale.
	err = commitSlotGen(t, s, 1, []byte("data2"), 7000, 2000, 0)
	assert.ErrorIs(t, err, ErrStaleGeneration)
}

func TestStoreInit_NegativeGeneration(t *testing.T) {
	store := NewStore(clock.Real{})
	meta := Metadata{Bandwidth: 1, Codecs: "avc1.64001f", Width: 1, Height: 1, FrameRate: 30, TargetDurationSecs: 2}
	err := store.Init("s", meta, []byte("init"), -1, 10, testSegmentBytes, 5, 0, testPlaylistWindowSize)
	assert.ErrorIs(t, err, ErrNegativeGeneration)
}

func TestStoreInit_EmptyInitData(t *testing.T) {
	store := NewStore(clock.Real{})
	meta := Metadata{Bandwidth: 1, Codecs: "avc1.64001f", Width: 1, Height: 1, FrameRate: 30, TargetDurationSecs: 2}
	err := store.Init("s", meta, []byte{}, 0, 10, testSegmentBytes, 5, 0, testPlaylistWindowSize)
	assert.ErrorIs(t, err, ErrEmptyInitData)
}

func TestStoreInit_NonZeroGeneration(t *testing.T) {
	// First init at generation 5: stream should start with currentGeneration=5,
	// only init entry for gen 5, and reject segments at earlier generations.
	clk := clock.NewMock(time.UnixMilli(0))
	store := NewStore(clk)
	meta := Metadata{Bandwidth: 1, Codecs: "avc1.64001f", Width: 1, Height: 1, FrameRate: 30, TargetDurationSecs: 2}
	err := store.Init("s", meta, []byte("init-gen5"), 5, 10, testSegmentBytes, 5, 0, testPlaylistWindowSize)
	require.NoError(t, err)
	t.Cleanup(func() { store.Delete("s") })
	s := store.Get("s")
	require.NotNil(t, s)

	assert.Equal(t, int64(5), s.CurrentGeneration())

	// Init data for gen 5 should be retrievable.
	var buf bytes.Buffer
	_, err = s.WriteInitDataForGenerationTo(&buf, 5)
	require.NoError(t, err)
	assert.Equal(t, []byte("init-gen5"), buf.Bytes())

	// Gen 0 init entry should not exist.
	_, ok := s.GetInitEntry(0)
	assert.False(t, ok, "gen 0 should not have an init entry")

	// Segment at gen 4 → stale (< currentGeneration 5).
	err = commitSlotGen(t, s, 0, []byte("data"), 5000, 2000, 4)
	assert.ErrorIs(t, err, ErrStaleGeneration)

	// Segment at gen 0 → stale.
	err = commitSlotGen(t, s, 0, []byte("data"), 5000, 2000, 0)
	assert.ErrorIs(t, err, ErrStaleGeneration)

	// Segment at gen 5 → succeeds.
	err = commitSlotGen(t, s, 0, []byte("data"), 5000, 2000, 5)
	require.NoError(t, err)
}

func TestEviction_ActiveReaderStopsDiscontinuityTracking(t *testing.T) {
	// When eviction stops at a segment with an active reader, discontinuity
	// counting should only reflect segments actually evicted, not the one
	// blocked by the reader.
	clk := clock.NewMock(time.UnixMilli(0))
	store := NewStore(clk)
	meta := Metadata{Bandwidth: 1, Codecs: "avc1.64001f", Width: 1, Height: 1, FrameRate: 30, TargetDurationSecs: 2}
	// backwardBufferSize=1, workingSpace=1
	err := store.Init("s", meta, []byte("init0"), 0, 20, testSegmentBytes, 1, 1, 12)
	require.NoError(t, err)
	t.Cleanup(func() { store.Delete("s") })
	s := store.Get("s")
	require.NotNil(t, s)

	require.NoError(t, s.AddInitEntry(1, []byte("init1")))

	// Push: index 0 (gen=0), index 1 (gen=0), index 2 (gen=1), index 3 (gen=1)
	mustCommitSlot(t, s, 0, []byte("d"), 1000, 2000)
	mustCommitSlot(t, s, 1, []byte("d"), 3000, 2000)
	require.NoError(t, commitSlotGen(t, s, 2, []byte("d"), 5000, 2000, 1))
	require.NoError(t, commitSlotGen(t, s, 3, []byte("d"), 7000, 2000, 1))

	// Acquire a reader on index 1 (gen=0). This will block eviction from
	// proceeding past it.
	var readerDone bool
	err = s.RunWithSegmentSlot(1, func(slot *pool.BufferSlot) error {
		// While the reader holds the reference, advance time and push a
		// segment to trigger eviction.
		clk.Set(time.UnixMilli(8000))
		require.NoError(t, commitSlotGen(t, s, 4, []byte("d"), 9000, 2000, 1))

		// Eviction should have evicted index 0 (gen=0) but stopped at
		// index 1 because it has an active reader.
		// evictedDiscontinuities: 0 (only gen=0 segments evicted so far,
		// no generation change among them).
		// lastEvictedGeneration: 0 (index 0 was gen=0).

		// Verify index 0 is evicted and index 1 is kept.
		_, readErr := readSegment(s, 0)
		assert.ErrorIs(t, readErr, ErrSegmentNotFound, "index 0 should be evicted")
		_, readErr = readSegment(s, 1)
		assert.NoError(t, readErr, "index 1 should be kept (active reader)")

		readerDone = true
		return nil
	})
	require.NoError(t, err)
	require.True(t, readerDone)

	// Now the reader is released. Push another segment to trigger eviction again.
	clk.Set(time.UnixMilli(10000))
	require.NoError(t, commitSlotGen(t, s, 5, []byte("d"), 11000, 2000, 1))

	// Now index 1 (gen=0) and index 2 (gen=1) should be evicted.
	// The gen 0→1 transition should now be counted.
	// Render and check the DISCONTINUITY-SEQUENCE.
	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(11000, 12)
	s.mu.RUnlock()

	assert.Contains(t, playlist, "#EXT-X-DISCONTINUITY-SEQUENCE:1\n",
		"gen 0→1 transition should be counted after reader released; got:\n%s", playlist)
}

// ---------------------------------------------------------------------------
// Init entry eviction
// ---------------------------------------------------------------------------

func TestEvictStaleInitEntries_BasicEviction(t *testing.T) {
	// Gen 0 init should be evicted after gen 1 segments replace all gen 0
	// segments via dropStaleGenerationLocked.
	clk := clock.NewMock(time.UnixMilli(0))
	store := NewStore(clk)
	meta := Metadata{Bandwidth: 1, Codecs: "avc1.64001f", Width: 1, Height: 1, FrameRate: 30, TargetDurationSecs: 2}
	mustInit(t, store, "s", meta, []byte("init0"), 10, testSegmentBytes, 5)
	s := store.Get("s")

	require.NoError(t, s.AddInitEntry(1, []byte("init1")))

	// Push gen=0 segments.
	for i := uint32(0); i < 3; i++ {
		require.NoError(t, commitSlotGen(t, s, i, []byte("d"), int64(5000+i*2000), 2000, 0))
	}
	// Gen 0 init should still be present.
	_, ok := s.GetInitEntry(0)
	require.True(t, ok, "gen 0 init should exist while gen 0 segments are buffered")

	// Advance to gen=1 at index 0 — drops all gen=0 segments at/after idx 0.
	require.NoError(t, commitSlotGen(t, s, 0, []byte("n"), 5000, 2000, 1))

	// Gen 0 init should now be evicted (no gen 0 segments remain).
	_, ok = s.GetInitEntry(0)
	assert.False(t, ok, "gen 0 init should be evicted")

	// Gen 1 init remains.
	_, ok = s.GetInitEntry(1)
	assert.True(t, ok, "gen 1 init should still exist")
}

func TestEvictStaleInitEntries_RetainedWhileSegmentsExist(t *testing.T) {
	// Gen 0 init should be retained as long as gen 0 segments remain in the
	// backward buffer, and evicted once they are all gone.
	clk := clock.NewMock(time.UnixMilli(0))
	store := NewStore(clk)
	meta := Metadata{Bandwidth: 1, Codecs: "avc1.64001f", Width: 1, Height: 1, FrameRate: 30, TargetDurationSecs: 2}
	// backwardBufferSize=1 so old segments evict quickly.
	err := store.Init("s", meta, []byte("init0"), 0, 10, testSegmentBytes, 1, 1, testPlaylistWindowSize)
	require.NoError(t, err)
	t.Cleanup(func() { store.Delete("s") })
	s := store.Get("s")

	require.NoError(t, s.AddInitEntry(1, []byte("init1")))

	// Push gen=0 segments at indices 0-2.
	for i := uint32(0); i < 3; i++ {
		require.NoError(t, commitSlotGen(t, s, i, []byte("d"), int64(5000+i*2000), 2000, 0))
	}

	// Advance to gen=1 at index 3 — gen=0 segments at 0-2 remain (before insertion point).
	require.NoError(t, commitSlotGen(t, s, 3, []byte("n"), 11000, 2000, 1))

	_, ok := s.GetInitEntry(0)
	assert.True(t, ok, "gen 0 init should be retained while gen 0 segments exist in buffer")

	// Advance clock so all gen 0 segments are in the past and push gen=1
	// segments to trigger time-based eviction of gen 0 segments.
	clk.Set(time.UnixMilli(20000))
	for i := uint32(4); i < 8; i++ {
		require.NoError(t, commitSlotGen(t, s, i, []byte("n"), int64(20000+int64(i)*2000), 2000, 1))
	}

	// Gen 0 segments should have been evicted by time-based eviction.
	// Gen 0 init should now be gone.
	_, ok = s.GetInitEntry(0)
	assert.False(t, ok, "gen 0 init should be evicted after all gen 0 segments are evicted")
}

func TestEvictStaleInitEntries_CurrentGenerationRetained(t *testing.T) {
	// The current generation's init must never be evicted, even with zero segments.
	clk := clock.NewMock(time.UnixMilli(0))
	store := NewStore(clk)
	meta := Metadata{Bandwidth: 1, Codecs: "avc1.64001f", Width: 1, Height: 1, FrameRate: 30, TargetDurationSecs: 2}
	mustInit(t, store, "s", meta, []byte("init0"), 10, testSegmentBytes, 1)
	s := store.Get("s")

	require.NoError(t, s.AddInitEntry(1, []byte("init1")))

	// Push one gen=0 segment, then advance to gen=1. Gen=0 segment gets dropped.
	require.NoError(t, commitSlotGen(t, s, 0, []byte("d"), 5000, 2000, 0))
	require.NoError(t, commitSlotGen(t, s, 0, []byte("n"), 5000, 2000, 1))

	// Advance clock and push to trigger eviction of the gen=1 segment.
	clk.Set(time.UnixMilli(10000))
	require.NoError(t, commitSlotGen(t, s, 1, []byte("n2"), 12000, 2000, 1))

	// Gen 1 is current generation — init must be retained even if the first
	// gen=1 segment was evicted.
	_, ok := s.GetInitEntry(1)
	assert.True(t, ok, "current generation init must never be evicted")
}

func TestEvictStaleInitEntries_MultipleGenerations(t *testing.T) {
	// Gens 0 and 1 should be evicted when only gen 2 segments exist.
	clk := clock.NewMock(time.UnixMilli(0))
	store := NewStore(clk)
	meta := Metadata{Bandwidth: 1, Codecs: "avc1.64001f", Width: 1, Height: 1, FrameRate: 30, TargetDurationSecs: 2}
	mustInit(t, store, "s", meta, []byte("init0"), 10, testSegmentBytes, 5)
	s := store.Get("s")

	require.NoError(t, s.AddInitEntry(1, []byte("init1")))
	require.NoError(t, s.AddInitEntry(2, []byte("init2")))

	// Push gen=0, then advance to gen=1, then advance to gen=2.
	require.NoError(t, commitSlotGen(t, s, 0, []byte("d"), 5000, 2000, 0))
	require.NoError(t, commitSlotGen(t, s, 0, []byte("d"), 5000, 2000, 1))
	require.NoError(t, commitSlotGen(t, s, 0, []byte("d"), 5000, 2000, 2))

	// Only gen=2 segment remains. Both gen 0 and gen 1 inits should be evicted.
	_, ok := s.GetInitEntry(0)
	assert.False(t, ok, "gen 0 init should be evicted")
	_, ok = s.GetInitEntry(1)
	assert.False(t, ok, "gen 1 init should be evicted")
	_, ok = s.GetInitEntry(2)
	assert.True(t, ok, "gen 2 init should be retained (current)")
}

func TestEvictStaleInitEntries_SingleGeneration_NoOp(t *testing.T) {
	// With only one generation, no eviction should occur.
	clk := clock.NewMock(time.UnixMilli(0))
	store := NewStore(clk)
	meta := Metadata{Bandwidth: 1, Codecs: "avc1.64001f", Width: 1, Height: 1, FrameRate: 30, TargetDurationSecs: 2}
	mustInit(t, store, "s", meta, []byte("init0"), 10, testSegmentBytes, 5)
	s := store.Get("s")

	for i := uint32(0); i < 5; i++ {
		require.NoError(t, commitSlotGen(t, s, i, []byte("d"), int64(5000+i*2000), 2000, 0))
	}

	_, ok := s.GetInitEntry(0)
	assert.True(t, ok, "single generation init should never be evicted")
}
