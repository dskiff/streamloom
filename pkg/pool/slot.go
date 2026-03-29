package pool

import (
	"fmt"
	"io"
	"sync/atomic"
)

// ErrOverflow is returned by BufferSlot.ReadFrom when the reader provides
// more data than the slot's capacity.
var ErrOverflow = fmt.Errorf("pool: read exceeds slot capacity")

// BufferSlot is a handle to a pooled byte buffer. The underlying slice has
// len == data written and cap == pool buffer size. Use ReadFrom to fill it
// from an io.Reader and WriteTo to drain it to an io.Writer.
type BufferSlot struct {
	buf     []byte // len = data written, cap = pool bufSize
	readers atomic.Int32
}

// Len returns the number of valid data bytes in the slot.
func (s *BufferSlot) Len() int {
	return len(s.buf)
}

// WriteTo writes the slot's data to w. Implements io.WriterTo.
func (s *BufferSlot) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write(s.buf)
	if n < len(s.buf) && err == nil {
		err = io.ErrShortWrite
	}
	return int64(n), err
}

// ReaderInc increments the active reader count. The caller must call ReaderDec when done.
func (s *BufferSlot) ReaderInc() {
	s.readers.Add(1)
}

// ReaderDec decrements the active reader count. Panics if the count goes negative.
func (s *BufferSlot) ReaderDec() {
	if v := s.readers.Add(-1); v < 0 {
		panic(Unrecoverable{"pool: BufferSlot.ReaderDec called more times than ReaderInc"})
	}
}

// Readers returns the current active reader count.
func (s *BufferSlot) Readers() int32 {
	return s.readers.Load()
}

// ReadFrom reads from r into the slot's buffer up to its capacity.
// Returns ErrOverflow if r has more data than the slot can hold.
func (s *BufferSlot) ReadFrom(r io.Reader) (int64, error) {
	capacity := cap(s.buf)
	s.buf = s.buf[:capacity]
	n, err := io.ReadFull(r, s.buf)
	s.buf = s.buf[:n]

	switch err {
	case nil:
		// Exactly capacity bytes read. Check if reader has more data.
		var probe [1]byte
		nProbe, _ := r.Read(probe[:])
		if nProbe > 0 {
			return int64(n), ErrOverflow
		}
		return int64(n), nil
	case io.ErrUnexpectedEOF, io.EOF:
		// Reader had fewer bytes than capacity — that's fine.
		return int64(n), nil
	default:
		return int64(n), err
	}
}
