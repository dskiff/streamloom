package stream

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// minRenderInterval is the minimum time between consecutive playlist renders.
// Prevents busy-looping when many segments have past timestamps.
const minRenderInterval = 50 * time.Millisecond

// renderMediaPlaylist builds the HLS media playlist string from the current
// in-memory segments. Eligibility is bounded by the per-stream look-ahead
// cap: a segment is eligible iff its Timestamp <= nowMs + s.maxLookaheadMs.
// A sliding window of at most windowSize segments is then applied to the
// tail of the eligible set. Publishing beyond wall clock lets PDT-sync'd
// clients (RFC 8216 §6.3.3) align their start position with the buffering
// heuristic instead of chasing two conflicting anchors.
//
// When s.mintToken is set, it is invoked once per render and the returned
// token is baked into every emitted URI as "?vt=<token>". When it is nil
// (or returns ""), URIs are emitted without a query string. The base64url
// alphabet produced by viewer.Mint is already URL-safe, so no escaping is
// required at render time.
//
// Returns (playlist, nextEligibleMs) where nextEligibleMs is the timestamp
// of the first segment beyond the look-ahead cap (0 if no such segment
// exists). Returns ("", nextEligibleMs) when no segments are eligible.
//
// Must be called with s.mu.RLock held.
func (s *Stream) renderMediaPlaylist(nowMs int64, windowSize int) (string, int64) {
	// Binary search: find the first segment past the look-ahead cap. All
	// segments before that index are eligible. nowMs + maxLookaheadMs can
	// exceed nowMs by up to an hour (per MaxLookaheadCeilingMs) but cannot
	// overflow int64 in any realistic configuration.
	cutoff := nowMs + s.maxLookaheadMs
	eligible := sort.Search(len(s.segments), func(i int) bool {
		return s.segments[i].Timestamp > cutoff
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

	// Contiguity gate: truncate the window at the first index gap. Segments
	// may arrive out of Index order (CommitSlot inserts by binary search), so
	// a later index can land before an earlier one is committed. HLS (RFC
	// 8216 §6.2.1) forbids mid-playlist insertions — a segment, once
	// published, must keep its position. Publishing index 7 while 6 is still
	// missing and then inserting 6 later would violate that. We stop at the
	// first gap so the tail stays where it is until the missing index
	// arrives.
	for i := 1; i < len(window); i++ {
		if window[i].Index != window[i-1].Index+1 {
			window = window[:i]
			break
		}
	}

	// Mint the playlist-scoped viewer token once per render. The renderer
	// bakes it into every URI so viewers use a single short-lived token
	// for all init/segment fetches linked from this playlist.
	var vtQuery string
	if s.mintToken != nil {
		if tok := s.mintToken(); tok != "" {
			vtQuery = "?vt=" + tok
		}
	}

	var b strings.Builder
	var scratch [64]byte

	// Estimate capacity: ~150 bytes per segment entry + ~200 bytes header.
	b.Grow(200 + len(window)*(150+len(vtQuery)))

	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:7\n")
	b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")

	// EXT-X-SERVER-CONTROL:HOLD-BACK tells PDT-sync'd clients where the
	// intended live edge sits relative to the playlist tail. Without it,
	// clients fall back to the "3 × target-duration" heuristic (RFC 8216
	// §6.3.3), which fights the shifted tail and causes rebuffering. The
	// spec requires HOLD-BACK to be at least 3 × target-duration, so clamp
	// up — a smaller configured look-ahead still produces a spec-compliant
	// playlist.
	holdBackSecs := float64(s.maxLookaheadMs) / 1000.0
	if minHoldBack := 3.0 * float64(s.metadata.TargetDurationSecs); holdBackSecs < minHoldBack {
		holdBackSecs = minHoldBack
	}
	b.WriteString("#EXT-X-SERVER-CONTROL:HOLD-BACK=")
	b.Write(strconv.AppendFloat(scratch[:0], holdBackSecs, 'f', 3, 64))
	b.WriteByte('\n')

	b.WriteString("#EXT-X-TARGETDURATION:")
	b.Write(strconv.AppendInt(scratch[:0], int64(s.metadata.TargetDurationSecs), 10))
	b.WriteByte('\n')

	b.WriteString("#EXT-X-MEDIA-SEQUENCE:")
	b.Write(strconv.AppendUint(scratch[:0], uint64(window[0].Index), 10))
	b.WriteByte('\n')

	b.WriteString("#EXT-X-MAP:URI=\"init.mp4")
	b.WriteString(vtQuery)
	b.WriteString("\"\n")

	for _, seg := range window {
		b.WriteString("#EXT-X-PROGRAM-DATE-TIME:")
		b.Write(time.UnixMilli(seg.Timestamp).UTC().AppendFormat(scratch[:0], "2006-01-02T15:04:05.000Z"))
		b.WriteByte('\n')

		dur := float64(seg.DurationMs) / 1000.0
		b.WriteString("#EXTINF:")
		b.Write(strconv.AppendFloat(scratch[:0], dur, 'f', 3, 64))
		b.WriteString(",\n")

		b.WriteString("segment_")
		b.Write(strconv.AppendUint(scratch[:0], uint64(seg.Index), 10))
		b.WriteString(".m4s")
		b.WriteString(vtQuery)
		b.WriteByte('\n')
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
