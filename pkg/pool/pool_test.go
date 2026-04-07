package pool

import (
	"bytes"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shortWriter accepts at most limit bytes per Write, returning (limit, nil)
// to simulate a writer that performs a short write without an error.
type shortWriter struct {
	limit int
}

func (w *shortWriter) Write(p []byte) (int, error) {
	if len(p) <= w.limit {
		return len(p), nil
	}
	return w.limit, nil
}

func TestNewBufferPoolPreallocates(t *testing.T) {
	p := NewBufferPool(5, 1024)
	assert.Equal(t, 5, p.FreeCount())
}

func TestGetReturnsSlotWithCorrectCapacity(t *testing.T) {
	const bufSize = 256
	p := NewBufferPool(1, bufSize)
	slot, ok := p.Get()

	require.True(t, ok, "Get should succeed")
	assert.Equal(t, 0, slot.Len(), "new slot should have zero data length")
	assert.Equal(t, 0, p.FreeCount(), "expected FreeCount()=0 after Get")

	// Verify the slot can hold exactly bufSize bytes.
	data := make([]byte, bufSize)
	n, err := slot.ReadFrom(bytes.NewReader(data))
	require.NoError(t, err)
	assert.Equal(t, int64(bufSize), n)
	assert.Equal(t, bufSize, slot.Len())
}

func TestGetExhaustsPool(t *testing.T) {
	p := NewBufferPool(2, 64)
	_, ok := p.Get()
	require.True(t, ok, "first Get should succeed")
	_, ok = p.Get()
	require.True(t, ok, "second Get should succeed")

	_, ok = p.Get()
	assert.False(t, ok, "Get on exhausted pool should return false")
}

func TestPutReturnsBuffer(t *testing.T) {
	p := NewBufferPool(2, 64)
	slot1, _ := p.Get()
	slot2, _ := p.Get()

	require.Equal(t, 0, p.FreeCount())

	p.Put(slot1)
	assert.Equal(t, 1, p.FreeCount())

	p.Put(slot2)
	assert.Equal(t, 2, p.FreeCount())
}

func TestPutPanicsOnDoublePut(t *testing.T) {
	p := NewBufferPool(1, 64)
	slot, _ := p.Get()
	p.Put(slot)

	defer func() {
		r := recover()
		require.NotNil(t, r, "double Put should panic")
		u, ok := r.(Unrecoverable)
		require.True(t, ok, "panic value should be Unrecoverable, got %T", r)
		assert.Contains(t, u.Msg, "already-free")
	}()

	p.Put(slot)
}

func TestPutPanicsOnForeignBuffer(t *testing.T) {
	p := NewBufferPool(1, 64)

	defer func() {
		r := recover()
		require.NotNil(t, r, "Put of foreign buffer should panic")
		u, ok := r.(Unrecoverable)
		require.True(t, ok, "panic value should be Unrecoverable, got %T", r)
		assert.Contains(t, u.Msg, "not owned")
	}()

	foreign := &BufferSlot{buf: make([]byte, 64)}
	p.Put(foreign)
}

func TestGetPutCycle(t *testing.T) {
	p := NewBufferPool(3, 64)

	// Drain pool.
	slots := make([]*BufferSlot, 3)
	for i := range slots {
		s, ok := p.Get()
		require.True(t, ok, "Get(%d) should succeed", i)
		slots[i] = s
	}

	// Return all.
	for _, s := range slots {
		p.Put(s)
	}
	assert.Equal(t, 3, p.FreeCount(), "expected FreeCount()=3 after returning all")

	// Drain and return again — pool should be fully reusable.
	for i := range slots {
		s, ok := p.Get()
		require.True(t, ok, "second Get(%d) should succeed", i)
		slots[i] = s
	}
	for _, s := range slots {
		p.Put(s)
	}
	assert.Equal(t, 3, p.FreeCount(), "expected FreeCount()=3 after second cycle")
}

func TestBufferSlotWriteTo(t *testing.T) {
	p := NewBufferPool(1, 64)
	slot, _ := p.Get()

	data := []byte("hello world")
	_, err := slot.ReadFrom(bytes.NewReader(data))
	require.NoError(t, err)
	assert.Equal(t, len(data), slot.Len())

	var out bytes.Buffer
	n, err := slot.WriteTo(&out)
	require.NoError(t, err)
	assert.Equal(t, int64(len(data)), n)
	assert.Equal(t, data, out.Bytes())
}

func TestBufferSlotWriteToShortWrite(t *testing.T) {
	p := NewBufferPool(1, 64)
	slot, _ := p.Get()

	data := []byte("hello world")
	_, err := slot.ReadFrom(bytes.NewReader(data))
	require.NoError(t, err)

	w := &shortWriter{limit: 5}
	n, err := slot.WriteTo(w)
	assert.ErrorIs(t, err, io.ErrShortWrite)
	assert.Equal(t, int64(5), n)
}

func TestBufferSlotReadFromExact(t *testing.T) {
	p := NewBufferPool(1, 8)
	slot, _ := p.Get()

	data := []byte("12345678") // exactly capacity
	n, err := slot.ReadFrom(bytes.NewReader(data))
	require.NoError(t, err)
	assert.Equal(t, int64(8), n)
	assert.Equal(t, 8, slot.Len())
}

func TestBufferSlotReadFromOverflow(t *testing.T) {
	p := NewBufferPool(1, 8)
	slot, _ := p.Get()

	data := []byte("123456789") // one byte over capacity
	_, err := slot.ReadFrom(bytes.NewReader(data))
	assert.ErrorIs(t, err, ErrOverflow)

	// Slot should contain exactly capacity bytes.
	assert.Equal(t, 8, slot.Len(), "slot should hold capacity bytes on overflow")
}

func TestAssertCheckedOutPassesForCheckedOutSlot(t *testing.T) {
	p := NewBufferPool(1, 64)
	slot, _ := p.Get()

	// Should not panic.
	p.AssertCheckedOut(slot)
}

func TestAssertCheckedOutPanicsOnNil(t *testing.T) {
	p := NewBufferPool(1, 64)

	defer func() {
		r := recover()
		require.NotNil(t, r, "AssertCheckedOut(nil) should panic")
		u, ok := r.(Unrecoverable)
		require.True(t, ok, "panic value should be Unrecoverable, got %T", r)
		assert.Contains(t, u.Msg, "nil slot")
	}()

	p.AssertCheckedOut(nil)
}

func TestAssertCheckedOutPanicsOnForeignBuffer(t *testing.T) {
	p := NewBufferPool(1, 64)

	defer func() {
		r := recover()
		require.NotNil(t, r, "AssertCheckedOut on foreign buffer should panic")
		u, ok := r.(Unrecoverable)
		require.True(t, ok, "panic value should be Unrecoverable, got %T", r)
		assert.Contains(t, u.Msg, "not owned")
	}()

	foreign := &BufferSlot{buf: make([]byte, 64)}
	p.AssertCheckedOut(foreign)
}

func TestAssertCheckedOutPanicsOnFreeBuffer(t *testing.T) {
	p := NewBufferPool(1, 64)
	slot, _ := p.Get()
	p.Put(slot)

	defer func() {
		r := recover()
		require.NotNil(t, r, "AssertCheckedOut on free buffer should panic")
		u, ok := r.(Unrecoverable)
		require.True(t, ok, "panic value should be Unrecoverable, got %T", r)
		assert.Contains(t, u.Msg, "use-after-return")
	}()

	p.AssertCheckedOut(slot)
}

func TestReaderDecPanicsOnUnderflow(t *testing.T) {
	p := NewBufferPool(1, 64)
	slot, _ := p.Get()

	defer func() {
		r := recover()
		require.NotNil(t, r, "ReaderDec underflow should panic")
		u, ok := r.(Unrecoverable)
		require.True(t, ok, "panic value should be Unrecoverable, got %T", r)
		assert.Contains(t, u.Msg, "ReaderDec")
	}()

	slot.ReaderDec() // no matching ReaderInc — should panic
}

func TestPutPanicsOnNonZeroReaderCount(t *testing.T) {
	p := NewBufferPool(1, 64)
	slot, _ := p.Get()
	slot.ReaderInc() // simulate leaked reader

	defer func() {
		r := recover()
		require.NotNil(t, r, "Put with active readers should panic")
		u, ok := r.(Unrecoverable)
		require.True(t, ok, "panic value should be Unrecoverable, got %T", r)
		assert.Contains(t, u.Msg, "active readers")
	}()

	p.Put(slot)
}

func TestBufferSlotReadFromEmpty(t *testing.T) {
	p := NewBufferPool(1, 64)
	slot, _ := p.Get()

	n, err := slot.ReadFrom(bytes.NewReader(nil))
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)
	assert.Equal(t, 0, slot.Len())
}

func TestBufferSlotWriteToEmpty(t *testing.T) {
	// WriteTo on a freshly acquired slot (Len()==0) should write 0 bytes.
	p := NewBufferPool(1, 64)
	slot, ok := p.Get()
	require.True(t, ok)

	var buf bytes.Buffer
	n, err := slot.WriteTo(&buf)
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)
	assert.Empty(t, buf.Bytes())
}

func TestBufferSlotReadFromTwice(t *testing.T) {
	// A second ReadFrom should overwrite the first read's data.
	p := NewBufferPool(1, 64)
	slot, ok := p.Get()
	require.True(t, ok)

	_, err := slot.ReadFrom(bytes.NewReader([]byte("first")))
	require.NoError(t, err)
	assert.Equal(t, 5, slot.Len())

	_, err = slot.ReadFrom(bytes.NewReader([]byte("second-longer")))
	require.NoError(t, err)
	assert.Equal(t, len("second-longer"), slot.Len())

	var buf bytes.Buffer
	_, err = slot.WriteTo(&buf)
	require.NoError(t, err)
	assert.Equal(t, []byte("second-longer"), buf.Bytes())
}
