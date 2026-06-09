// Package watcher tracks distinct client IPs per stream for active viewer counting.
package watcher

import (
	"sync"
	"time"

	"github.com/dskiff/streamloom/pkg/clock"
)

// MaxWindowMs is the maximum allowed query window (60 minutes).
const MaxWindowMs int64 = 3_600_000

// DefaultWindowMs is the default query window when none is specified (1 minute).
const DefaultWindowMs int64 = 60_000

// CleanupInterval is how often the background goroutine purges stale entries.
const CleanupInterval = 5 * time.Minute

// MaxIPsPerStream caps the number of distinct client IPs tracked per stream.
// Active-watcher counting is best-effort, so once a stream is already tracking
// this many distinct IPs, a previously-unseen IP is dropped rather than
// inserted (already-tracked IPs still refresh their last-seen timestamp). This
// bounds per-stream memory: a flood of distinct IPs — whether a genuinely huge
// audience or an attacker cycling source addresses — can no longer grow the
// map without limit. The periodic Cleanup frees slots again as entries age
// past MaxWindowMs. The ceiling is far above any realistic simultaneous
// audience, so legitimate counts saturate rather than degrade in practice.
const MaxIPsPerStream = 100_000

// streamWatchers holds per-stream IP tracking data.
type streamWatchers struct {
	ips map[string]int64 // IP -> last-seen UnixMilli
}

// Tracker records client IPs per stream and counts distinct active watchers.
type Tracker struct {
	mu      sync.RWMutex
	clock   clock.Clock
	streams map[string]*streamWatchers
}

// NewTracker creates a Tracker with the given clock.
func NewTracker(clk clock.Clock) *Tracker {
	return &Tracker{
		clock:   clk,
		streams: make(map[string]*streamWatchers),
	}
}

// Record updates the last-seen timestamp for a client IP on a stream.
//
// At most MaxIPsPerStream distinct IPs are retained per stream: once that
// ceiling is reached, a previously-unseen IP is dropped instead of inserted.
// Already-tracked IPs always refresh, so steady-state viewers are unaffected
// and the count only degrades (saturating at the cap) under an abnormally
// large or adversarial set of distinct IPs.
func (t *Tracker) Record(streamID, ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	sw := t.streams[streamID]
	if sw == nil {
		sw = &streamWatchers{ips: make(map[string]int64)}
		t.streams[streamID] = sw
	}
	if _, exists := sw.ips[ip]; !exists && len(sw.ips) >= MaxIPsPerStream {
		return
	}
	sw.ips[ip] = t.clock.Now().UnixMilli()
}

// ActiveCount returns the number of distinct IPs seen within the last windowMs
// milliseconds for the given stream. windowMs is capped at MaxWindowMs.
// If windowMs <= 0, DefaultWindowMs is used.
func (t *Tracker) ActiveCount(streamID string, windowMs int64) int {
	if windowMs <= 0 {
		windowMs = DefaultWindowMs
	}
	if windowMs > MaxWindowMs {
		windowMs = MaxWindowMs
	}

	t.mu.RLock()
	defer t.mu.RUnlock()

	sw := t.streams[streamID]
	if sw == nil {
		return 0
	}

	cutoff := t.clock.Now().UnixMilli() - windowMs
	count := 0
	for _, lastSeen := range sw.ips {
		if lastSeen >= cutoff {
			count++
		}
	}
	return count
}

// Cleanup removes entries older than MaxWindowMs across all streams
// and deletes stream entries that have no remaining IPs.
func (t *Tracker) Cleanup() {
	t.mu.Lock()
	defer t.mu.Unlock()

	cutoff := t.clock.Now().UnixMilli() - MaxWindowMs
	for id, sw := range t.streams {
		for ip, lastSeen := range sw.ips {
			if lastSeen < cutoff {
				delete(sw.ips, ip)
			}
		}
		if len(sw.ips) == 0 {
			delete(t.streams, id)
		}
	}
}

// DeleteStream removes all tracking data for a stream.
func (t *Tracker) DeleteStream(streamID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.streams, streamID)
}
