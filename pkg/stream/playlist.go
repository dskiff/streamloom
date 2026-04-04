package stream

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// minRenderInterval is the minimum time between consecutive playlist renders.
// Prevents busy-looping when many segments have past timestamps.
const minRenderInterval = 50 * time.Millisecond

// discontinuitySequence counts the number of generation transitions that have
// scrolled out before the playlist window. This includes transitions among
// evicted segments and transitions among buffered segments before the window.
//
// Must be called with s.mu.RLock held.
func (s *Stream) discontinuitySequence(start int) int {
	discSeq := s.evictedDiscontinuities

	// Check boundary between last evicted segment and first buffered segment.
	// This applies regardless of whether start is 0: if eviction removed all
	// pre-window segments, the transition still scrolled out of the window.
	// lastEvictedGeneration == -1 means no segments have been evicted yet.
	if s.lastEvictedGeneration >= 0 && len(s.segments) > 0 && s.segments[0].Generation != s.lastEvictedGeneration {
		discSeq++
	}

	// Count transitions among buffered pre-window segments and the boundary
	// between the last pre-window segment and the first window segment.
	// Using <= start (not < start) ensures the transition at segments[start-1]
	// → segments[start] is counted when windowing pushes it out.
	for i := 1; i <= start; i++ {
		if s.segments[i].Generation != s.segments[i-1].Generation {
			discSeq++
		}
	}

	return discSeq
}

// renderMediaPlaylist builds the HLS media playlist string from the current
// in-memory segments. Only segments with Timestamp <= nowMs are eligible.
// A sliding window of at most windowSize segments is applied to the tail of
// the eligible set.
//
// Generation transitions within the window produce #EXT-X-DISCONTINUITY tags
// and per-generation #EXT-X-MAP directives. The #EXT-X-DISCONTINUITY-SEQUENCE
// header counts transitions that have scrolled out before the window.
//
// Returns (playlist, nextEligibleMs) where nextEligibleMs is the timestamp
// of the first segment not yet eligible (0 if no such segment exists).
// Returns ("", nextEligibleMs) when no segments are eligible.
//
// Must be called with s.mu.RLock held.
func (s *Stream) renderMediaPlaylist(nowMs int64, windowSize int) (string, int64) {
	// Binary search: find the first segment with Timestamp > nowMs.
	// All segments before that index are eligible (Timestamp <= nowMs).
	eligible := sort.Search(len(s.segments), func(i int) bool {
		return s.segments[i].Timestamp > nowMs
	})

	var nextEligibleMs int64
	if eligible < len(s.segments) {
		nextEligibleMs = s.segments[eligible].Timestamp
	}

	if eligible == 0 {
		return "", nextEligibleMs
	}

	// Apply sliding window: take the last windowSize eligible segments.
	start := max(eligible-windowSize, 0)
	window := s.segments[start:eligible]

	discSeq := s.discontinuitySequence(start)

	var b strings.Builder

	// Estimate capacity: ~150 bytes per segment entry + ~200 bytes header.
	b.Grow(200 + len(window)*150)

	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:7\n")
	b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", s.metadata.TargetDurationSecs)
	fmt.Fprintf(&b, "#EXT-X-MEDIA-SEQUENCE:%d\n", window[0].Index)
	fmt.Fprintf(&b, "#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", discSeq)

	var prevGeneration int64
	for i, seg := range window {
		if i == 0 || seg.Generation != prevGeneration {
			if i > 0 {
				b.WriteString("#EXT-X-DISCONTINUITY\n")
			}
			fmt.Fprintf(&b, "#EXT-X-MAP:URI=\"init_%d.mp4\"\n", seg.Generation)
		}
		prevGeneration = seg.Generation

		ts := time.UnixMilli(seg.Timestamp).UTC().Format("2006-01-02T15:04:05.000Z")
		dur := float64(seg.DurationMs) / 1000.0
		fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n", ts)
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n", dur)
		fmt.Fprintf(&b, "segment_%d.m4s\n", seg.Index)
	}

	result := b.String()
	return result, nextEligibleMs
}

// runPlaylistRenderer is the background goroutine that maintains the cached
// media playlist. It re-renders when:
//   - a new segment is committed (notifyCh signal)
//   - the next future segment becomes eligible (timed sleep)
//
// It exits when the done channel is closed (stream deletion).
func (s *Stream) runPlaylistRenderer(windowSize int) {
	defer close(s.stopped)

	timer := s.clock.NewTimer(0)
	// Drain the initial fire so the timer starts in a stopped state.
	<-timer.C()

	for {
		nowMs := s.clock.Now().UnixMilli()

		s.mu.RLock()
		playlist, nextEligibleMs := s.renderMediaPlaylist(nowMs, windowSize)
		s.mu.RUnlock()

		p := playlist // allocate a distinct string per iteration for the atomic pointer
		s.cachedPlaylist.Store(&p)

		if playlist != "" {
			s.hasPlaylistOnce.Do(func() { close(s.hasPlaylist) })
		}

		// Determine how long to sleep before the next segment becomes eligible.
		if nextEligibleMs > 0 {
			sleepMs := nextEligibleMs - s.clock.Now().UnixMilli()
			if sleepMs <= 0 {
				// Next segment is already eligible — re-render after a short
				// minimum interval to prevent busy-looping when many segments
				// have past timestamps.
				timer.Reset(minRenderInterval)
				select {
				case <-timer.C():
				case <-s.notifyCh:
					timer.Stop()
				case <-s.done:
					timer.Stop()
					return
				}
				continue
			}
			timer.Reset(time.Duration(sleepMs) * time.Millisecond)
			select {
			case <-timer.C():
			case <-s.notifyCh:
				timer.Stop()
			case <-s.done:
				timer.Stop()
				return
			}
		} else {
			// No future segments — wait for a commit or shutdown.
			select {
			case <-s.notifyCh:
			case <-s.done:
				return
			}
		}

		// Drain any extra notification to avoid an immediate no-op re-render.
		select {
		case <-s.notifyCh:
		default:
		}
	}
}
