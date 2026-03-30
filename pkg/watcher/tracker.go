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
func (t *Tracker) Record(streamID, ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	sw := t.streams[streamID]
	if sw == nil {
		sw = &streamWatchers{ips: make(map[string]int64)}
		t.streams[streamID] = sw
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
