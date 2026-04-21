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

// PlaylistSnapshot is the renderer's cached output for one published window.
// The body is stored split around the EXT-X-START line: the handler
// synthesizes that single line per request from the current wall clock so
// TIME-OFFSET stays anchored to now (not to the last commit). Everything
// else — PDTs, segment URIs, MEDIA-SEQUENCE, HOLD-BACK — is baked into
// Prefix/Suffix at render time and reused verbatim until the next commit.
//
// A nil *PlaylistSnapshot means "no eligible segments yet"; the handler
// treats that the same as the pre-live-edge case and returns 503.
//
// Instances are shared immutably between the renderer goroutine and any
// number of concurrent handler goroutines via an atomic pointer swap.
// Callers must treat every field as read-only: a single in-place mutation
// would race with every in-flight StartLine / Assemble call. The fields
// are exported only to keep the HTTP handler allocation-free; prefer the
// StartLine, Assemble, and CachedPlaylistSnapshot entry points in code
// outside this package.
type PlaylistSnapshot struct {
	// Prefix is the playlist bytes from "#EXTM3U" through the trailing
	// "\n" of the EXT-X-SERVER-CONTROL line (the line immediately
	// preceding EXT-X-START).
	Prefix string
	// Suffix is the playlist bytes from EXT-X-TARGETDURATION through the
	// last segment line (inclusive of its trailing "\n").
	Suffix string

	// EndMs is the PDT of the end of the last segment in the published
	// window (Timestamp + DurationMs). StartLine uses it to compute how
	// stale the cached body is relative to the request's wall clock.
	EndMs int64

	// HoldBackSecs is the intended client latency target in seconds —
	// the value emitted as EXT-X-SERVER-CONTROL:HOLD-BACK and also the
	// baseline magnitude of TIME-OFFSET when the cache is fresh.
	HoldBackSecs float64
	// MinHoldBackSecs is the spec floor (3 × target-duration) that
	// TIME-OFFSET must not fall below; going tighter would advertise a
	// shorter latency than the HOLD-BACK header promises.
	MinHoldBackSecs float64
}

// StartLine formats the EXT-X-START tag for a request arriving at nowMs.
// The offset magnitude shrinks by the amount of wall-clock time that has
// passed since EndMs, which cancels the same amount of drift at the
// client's start position. It is clamped from below at MinHoldBackSecs
// so we never advertise a tighter latency than the HOLD-BACK header
// promises.
//
// The shrink only produces a visible change when HoldBackSecs is strictly
// greater than MinHoldBackSecs. At the default configuration
// (maxLookaheadMs = 3 × targetDuration × 1000) the two are equal, the
// clamp fires on any positive staleness, and the emitted offset is
// always -HoldBackSecs — identical to the pre-split static behavior.
// Operators who want cross-device convergence within a target-duration
// must configure a larger X-SL-MAX-LOOKAHEAD-MS.
//
// Safe to call on a nil receiver (returns ""), mirroring Assemble.
func (snap *PlaylistSnapshot) StartLine(nowMs int64) string {
	if snap == nil {
		return ""
	}
	staleSecs := float64(nowMs-snap.EndMs) / 1000.0
	if staleSecs < 0 {
		staleSecs = 0
	}
	offsetSecs := snap.HoldBackSecs - staleSecs
	if offsetSecs < snap.MinHoldBackSecs {
		offsetSecs = snap.MinHoldBackSecs
	}

	var b strings.Builder
	var scratch [64]byte
	b.Grow(48)
	b.WriteString("#EXT-X-START:TIME-OFFSET=-")
	b.Write(strconv.AppendFloat(scratch[:0], offsetSecs, 'f', 3, 64))
	b.WriteString(",PRECISE=YES\n")
	return b.String()
}

// Assemble returns the full playlist string for nowMs. Allocates the
// concatenated body; the HTTP handler avoids this by writing Prefix,
// StartLine(nowMs), and Suffix directly to the response.
func (snap *PlaylistSnapshot) Assemble(nowMs int64) string {
	if snap == nil {
		return ""
	}
	startLine := snap.StartLine(nowMs)
	var b strings.Builder
	b.Grow(len(snap.Prefix) + len(startLine) + len(snap.Suffix))
	b.WriteString(snap.Prefix)
	b.WriteString(startLine)
	b.WriteString(snap.Suffix)
	return b.String()
}

// renderPlaylistCache builds a PlaylistSnapshot from the current in-memory
// segments. Eligibility, sliding window, and contiguity rules match the
// previous single-string renderer; the only shape change is that the
// EXT-X-START line is omitted from the body and its ingredients
// (EndMs, HoldBackSecs, MinHoldBackSecs) are captured on the snapshot
// for per-request synthesis in StartLine.
//
// Returns (snapshot, nextEligibleMs). snapshot is nil when no segments are
// eligible; nextEligibleMs is the timestamp of the first segment beyond
// the look-ahead cap (0 if no such segment exists).
//
// Must be called with s.mu.RLock held.
func (s *Stream) renderPlaylistCache(nowMs int64, windowSize int) (*PlaylistSnapshot, int64) {
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
		return nil, nextEligibleMs
	}

	// Apply sliding window: take the last windowSize eligible segments.
	start := max(eligible-windowSize, 0)
	window := s.segments[start:eligible]

	// Contiguity gate: truncate the window at the first index gap. Segments
	// may arrive out of Index order (CommitSlot inserts by binary search), so
	// a later index can land before an earlier one is committed. HLS (RFC
	// 8216 §6.2.1) requires a published segment to keep its position and
	// URI; new entries append-only. Publishing index 7 while 6 is still
	// missing and then inserting 6 later would violate that. We stop at the
	// first gap so the tail stays where it is until the missing index
	// arrives.
	for i := 1; i < len(window); i++ {
		if window[i].Index != window[i-1].Index+1 {
			window = window[:i]
			break
		}
	}

	// HOLD-BACK, and the EXT-X-START magnitude when the snapshot is fresh.
	// Clamp up so a smaller configured look-ahead still produces a
	// spec-compliant playlist (draft-pantos-hls-rfc8216bis: HOLD-BACK MUST
	// be at least 3 × target-duration).
	holdBackSecs := float64(s.maxLookaheadMs) / 1000.0
	minHoldBackSecs := 3.0 * float64(s.metadata.TargetDurationSecs)
	if holdBackSecs < minHoldBackSecs {
		holdBackSecs = minHoldBackSecs
	}

	last := window[len(window)-1]
	endMs := last.Timestamp + int64(last.DurationMs)

	var b strings.Builder
	var scratch [64]byte

	// Estimate capacity: ~150 bytes per segment entry + a base64url vt query.
	// Tokens are 28 chars + "?vt=" = 32 bytes; round to 40 for slack.
	b.Grow(200 + len(window)*(150+40))

	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:7\n")
	b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")

	// EXT-X-SERVER-CONTROL:HOLD-BACK tells PDT-sync'd clients where the
	// intended live edge sits relative to the playlist tail. Without it,
	// clients fall back to the "3 × target-duration" heuristic (RFC 8216
	// §6.3.3), which fights the shifted tail and causes rebuffering.
	b.WriteString("#EXT-X-SERVER-CONTROL:HOLD-BACK=")
	b.Write(strconv.AppendFloat(scratch[:0], holdBackSecs, 'f', 3, 64))
	b.WriteByte('\n')

	// The EXT-X-START line itself is intentionally omitted from the cache
	// body. Snap the byte offset here: Prefix is everything written so far;
	// Suffix starts at whatever we write next. The handler synthesizes
	// the missing line from the current wall clock per request.
	splitAt := b.Len()

	b.WriteString("#EXT-X-TARGETDURATION:")
	b.Write(strconv.AppendInt(scratch[:0], int64(s.metadata.TargetDurationSecs), 10))
	b.WriteByte('\n')

	b.WriteString("#EXT-X-MEDIA-SEQUENCE:")
	b.Write(strconv.AppendUint(scratch[:0], uint64(window[0].Index), 10))
	b.WriteByte('\n')

	b.WriteString("#EXT-X-MAP:URI=\"init.mp4")
	if s.mintToken != nil {
		if tok := s.mintToken.InitToken(nowMs); tok != "" {
			b.WriteString("?vt=")
			b.WriteString(tok)
		}
	}
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
		if s.mintToken != nil {
			if tok := s.mintToken.SegmentToken(seg.Timestamp); tok != "" {
				b.WriteString("?vt=")
				b.WriteString(tok)
			}
		}
		b.WriteByte('\n')
	}

	full := b.String()
	return &PlaylistSnapshot{
		Prefix:          full[:splitAt],
		Suffix:          full[splitAt:],
		EndMs:           endMs,
		HoldBackSecs:    holdBackSecs,
		MinHoldBackSecs: minHoldBackSecs,
	}, nextEligibleMs
}

// renderMediaPlaylist is a test and back-compat wrapper around
// renderPlaylistCache. It assembles the snapshot at the same nowMs used
// for rendering, reproducing the "fresh cache" output (TIME-OFFSET ==
// -HoldBackSecs) that the pre-split renderer produced. Returns "" when
// no segments are eligible.
//
// Must be called with s.mu.RLock held.
func (s *Stream) renderMediaPlaylist(nowMs int64, windowSize int) (string, int64) {
	snap, nextEligibleMs := s.renderPlaylistCache(nowMs, windowSize)
	if snap == nil {
		return "", nextEligibleMs
	}
	return snap.Assemble(nowMs), nextEligibleMs
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
		snap, nextEligibleMs := s.renderPlaylistCache(nowMs, windowSize)
		s.mu.RUnlock()

		s.cachedPlaylist.Store(snap)

		if snap != nil {
			s.hasPlaylistOnce.Do(func() { close(s.hasPlaylist) })
		}

		// Determine how long to sleep before the next segment becomes eligible.
		// nextEligibleMs is the raw timestamp of the first segment past the
		// look-ahead cap; that segment crosses the cap at
		// nextEligibleMs - maxLookaheadMs. Sleeping until the raw timestamp
		// instead of the crossing time would delay playlist visibility by
		// maxLookaheadMs when no new commits arrive to wake the renderer via
		// notifyCh (e.g. transcoder pushes a batch ahead of now and pauses).
		if nextEligibleMs > 0 {
			sleepMs := nextEligibleMs - s.maxLookaheadMs - s.clock.Now().UnixMilli()
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
