package fmp4

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeBox constructs an fMP4 box with a 4-character type and payload.
func makeBox(typ string, payload []byte) []byte {
	size := uint32(8 + len(payload))
	b := make([]byte, size)
	binary.BigEndian.PutUint32(b[0:4], size)
	copy(b[4:8], typ)
	copy(b[8:], payload)
	return b
}

// makeFullBox constructs a full box (with version and flags) from payload.
func makeFullBox(typ string, version uint8, flags uint32, payload []byte) []byte {
	inner := make([]byte, 4+len(payload))
	inner[0] = version
	inner[1] = byte(flags >> 16)
	inner[2] = byte(flags >> 8)
	inner[3] = byte(flags)
	copy(inner[4:], payload)
	return makeBox(typ, inner)
}

func TestReadBoxHeader_Normal(t *testing.T) {
	data := makeBox("test", []byte{0xAA, 0xBB})
	hdr, err := readBoxHeader(data, 0)
	require.NoError(t, err)
	assert.Equal(t, fourcc("test"), hdr.boxType)
	assert.Equal(t, 8, hdr.dataOffset)
	assert.Equal(t, 2, hdr.dataSize)
	assert.Equal(t, 10, hdr.totalSize)
}

func TestReadBoxHeader_Extended(t *testing.T) {
	// Build an extended-size box manually: size=1, type, then 8-byte real size.
	payload := []byte{0xDE, 0xAD}
	totalSize := uint64(16 + len(payload)) // 16-byte header + payload
	b := make([]byte, totalSize)
	binary.BigEndian.PutUint32(b[0:4], 1) // size=1 signals extended
	copy(b[4:8], "extd")
	binary.BigEndian.PutUint64(b[8:16], totalSize)
	copy(b[16:], payload)

	hdr, err := readBoxHeader(b, 0)
	require.NoError(t, err)
	assert.Equal(t, fourcc("extd"), hdr.boxType)
	assert.Equal(t, 16, hdr.dataOffset)
	assert.Equal(t, len(payload), hdr.dataSize)
	assert.Equal(t, int(totalSize), hdr.totalSize)
}

func TestReadBoxHeader_ToEnd(t *testing.T) {
	// size=0 means box extends to end of data.
	data := make([]byte, 20)
	binary.BigEndian.PutUint32(data[0:4], 0) // size=0
	copy(data[4:8], "zero")

	hdr, err := readBoxHeader(data, 0)
	require.NoError(t, err)
	assert.Equal(t, fourcc("zero"), hdr.boxType)
	assert.Equal(t, 8, hdr.dataOffset)
	assert.Equal(t, 12, hdr.dataSize)
	assert.Equal(t, 20, hdr.totalSize)
}

func TestReadBoxHeader_TooSmall(t *testing.T) {
	// Less than 8 bytes available.
	_, err := readBoxHeader([]byte{0, 0, 0, 8, 't'}, 0)
	assert.ErrorIs(t, err, ErrBoxTruncated)
}

func TestReadBoxHeader_SizeTooSmall(t *testing.T) {
	data := make([]byte, 8)
	binary.BigEndian.PutUint32(data[0:4], 4) // size < 8
	copy(data[4:8], "bad!")
	_, err := readBoxHeader(data, 0)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "too small")
}

func TestReadBoxHeader_Truncated(t *testing.T) {
	// Declared size exceeds available data.
	data := make([]byte, 8)
	binary.BigEndian.PutUint32(data[0:4], 100) // claims 100 bytes
	copy(data[4:8], "truc")
	_, err := readBoxHeader(data, 0)
	assert.ErrorIs(t, err, ErrBoxTruncated)
}

func TestFindBox(t *testing.T) {
	box1 := makeBox("aaaa", []byte{1, 2, 3})
	box2 := makeBox("bbbb", []byte{4, 5})
	data := append(box1, box2...)

	payload, err := findBox(data, fourcc("bbbb"))
	require.NoError(t, err)
	assert.Equal(t, []byte{4, 5}, payload)
}

func TestFindBox_NotFound(t *testing.T) {
	data := makeBox("aaaa", []byte{1})
	_, err := findBox(data, fourcc("zzzz"))
	assert.ErrorIs(t, err, ErrBoxNotFound)
}

func TestFindBoxMulti(t *testing.T) {
	box1 := makeBox("trak", []byte{1})
	box2 := makeBox("skip", []byte{2})
	box3 := makeBox("trak", []byte{3})
	data := append(append(box1, box2...), box3...)

	results, err := findBoxMulti(data, fourcc("trak"))
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, []byte{1}, results[0])
	assert.Equal(t, []byte{3}, results[1])
}

func TestFindBoxMulti_None(t *testing.T) {
	data := makeBox("aaaa", []byte{1})
	results, err := findBoxMulti(data, fourcc("zzzz"))
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestReadFullBoxHeader(t *testing.T) {
	payload := []byte{1, 0x00, 0x01, 0x08, 0xAA, 0xBB}
	version, flags, rest, err := readFullBoxHeader(payload)
	require.NoError(t, err)
	assert.Equal(t, uint8(1), version)
	assert.Equal(t, uint32(0x000108), flags)
	assert.Equal(t, []byte{0xAA, 0xBB}, rest)
}

func TestReadFullBoxHeader_TooShort(t *testing.T) {
	_, _, _, err := readFullBoxHeader([]byte{1, 2})
	assert.ErrorIs(t, err, ErrBoxTruncated)
}

func TestFourcc(t *testing.T) {
	assert.Equal(t, [4]byte{'m', 'o', 'o', 'v'}, fourcc("moov"))
}
