// Package pool provides a fixed-size, pre-allocated byte buffer pool.
// Buffers are allocated once at creation and recycled via Get/Put.
// The pool is not safe for concurrent use; callers must synchronize externally.
package pool

import (
	"fmt"
	"math"
)

// Unrecoverable is the panic value for pool invariant violations that must not
// be caught by recovery middleware. These indicate programming bugs (e.g.
// double-free, foreign buffer, invalid construction) where continuing would
// corrupt pool state or operate on invalid assumptions.
type Unrecoverable struct{ Msg string }

func (u Unrecoverable) Error() string  { return u.Msg }
func (u Unrecoverable) String() string { return u.Msg }

// BufferPool is a fixed-size free-list of pre-allocated BufferSlots backed by a
// single contiguous slab allocation. A tracking map keyed by slot pointer
// detects double-Put and foreign-buffer bugs, and enables future monitoring of
// unreturned buffers.
// It is not safe for concurrent use.
type BufferPool struct {
	free   []*BufferSlot
	isFree map[*BufferSlot]bool
}

// NewBufferPool creates a pool of count buffers, each bufSize bytes.
// All buffers are sub-slices of a single contiguous slab allocation.
// Panics if count or bufSize is not positive, or if the total size overflows int.
func NewBufferPool(count, bufSize int) *BufferPool {
	if count <= 0 || bufSize <= 0 {
		panic(Unrecoverable{fmt.Sprintf("pool: invalid NewBufferPool(%d, %d): count and bufSize must be positive", count, bufSize)})
	}
	if count > math.MaxInt/bufSize {
		panic(Unrecoverable{fmt.Sprintf("pool: NewBufferPool(%d, %d): total size overflows int", count, bufSize)})
	}

	slab := make([]byte, count*bufSize)
	free := make([]*BufferSlot, count)
	tracking := make(map[*BufferSlot]bool, count)
	for i := range free {
		buf := slab[i*bufSize : (i+1)*bufSize : (i+1)*bufSize]
		slot := &BufferSlot{buf: buf}
		free[i] = slot
		tracking[slot] = true
	}
	return &BufferPool{free: free, isFree: tracking}
}

// Get pops a BufferSlot from the free list and marks it as checked out.
// The returned slot has len 0 and cap equal to the pool's bufSize.
// Returns (nil, false) if the pool is exhausted.
func (p *BufferPool) Get() (*BufferSlot, bool) {
	n := len(p.free)
	if n == 0 {
		return nil, false
	}
	slot := p.free[n-1]
	p.free[n-1] = nil // avoid retaining reference in underlying array
	p.free = p.free[:n-1]
	p.isFree[slot] = false
	slot.buf = slot.buf[:0]
	return slot, true
}

// Put returns a BufferSlot to the free list.
// Panics if the slot is not owned by this pool or was already returned (double Put).
func (p *BufferPool) Put(slot *BufferSlot) {
	isFree, isOwned := p.isFree[slot]
	if !isOwned {
		panic(Unrecoverable{"pool: Put called with buffer not owned by this pool"})
	}
	if isFree {
		panic(Unrecoverable{"pool: Put called on an already-free buffer; possible double-return"})
	}
	p.isFree[slot] = true
	p.free = append(p.free, slot)
}

// AssertCheckedOut panics if slot is nil, not owned by this pool, or not
// currently checked out. Use at points where a checked-out buffer is expected
// to catch use-after-return and foreign-buffer bugs at the point of misuse.
func (p *BufferPool) AssertCheckedOut(slot *BufferSlot) {
	if slot == nil {
		panic(Unrecoverable{"pool: AssertCheckedOut called with nil slot"})
	}
	isFree, isOwned := p.isFree[slot]
	if !isOwned {
		panic(Unrecoverable{"pool: AssertCheckedOut called with buffer not owned by this pool"})
	}
	if isFree {
		panic(Unrecoverable{"pool: AssertCheckedOut called on a free buffer; possible use-after-return"})
	}
}

// FreeCount returns the number of available (free) buffers.
func (p *BufferPool) FreeCount() int {
	return len(p.free)
}
