// Package fmp4 provides minimal parsing of fMP4 (fragmented MP4) box
// structures for timing extraction and validation.
package fmp4

import (
	"encoding/binary"
	"errors"
	"fmt"
)

var (
	// ErrBoxTruncated is returned when a box header or payload extends
	// beyond the available data.
	ErrBoxTruncated = errors.New("fmp4: box truncated")

	// ErrBoxNotFound is returned when a box with the requested type
	// was not found at the current level.
	ErrBoxNotFound = errors.New("fmp4: box not found")
)

// boxHeader describes a parsed box header.
type boxHeader struct {
	boxType    [4]byte
	dataOffset int // byte offset of payload start within input slice
	dataSize   int // payload size (excluding header)
	totalSize  int // header + payload
}

// readBoxHeader parses the box header starting at data[offset].
// It handles normal (32-bit), extended (64-bit), and to-end-of-data sizes.
func readBoxHeader(data []byte, offset int) (boxHeader, error) {
	remaining := len(data) - offset
	if remaining < 8 {
		return boxHeader{}, ErrBoxTruncated
	}

	size := int64(binary.BigEndian.Uint32(data[offset : offset+4]))
	var fourcc [4]byte
	copy(fourcc[:], data[offset+4:offset+8])

	headerSize := 8

	switch {
	case size == 1:
		// Extended size: 8-byte uint64 follows the type field.
		if remaining < 16 {
			return boxHeader{}, ErrBoxTruncated
		}
		size = int64(binary.BigEndian.Uint64(data[offset+8 : offset+16]))
		headerSize = 16
		if size < 16 {
			return boxHeader{}, fmt.Errorf("fmp4: extended box size %d too small", size)
		}
	case size == 0:
		// Box extends to end of data.
		size = int64(remaining)
	default:
		if size < 8 {
			return boxHeader{}, fmt.Errorf("fmp4: box size %d too small", size)
		}
	}

	if int64(remaining) < size {
		return boxHeader{}, ErrBoxTruncated
	}

	return boxHeader{
		boxType:    fourcc,
		dataOffset: offset + headerSize,
		dataSize:   int(size) - headerSize,
		totalSize:  int(size),
	}, nil
}

// findBox scans data for the first box matching fourcc and returns its payload.
func findBox(data []byte, fourcc [4]byte) ([]byte, error) {
	offset := 0
	for offset < len(data) {
		hdr, err := readBoxHeader(data, offset)
		if err != nil {
			return nil, err
		}
		if hdr.boxType == fourcc {
			return data[hdr.dataOffset : hdr.dataOffset+hdr.dataSize], nil
		}
		offset += hdr.totalSize
	}
	return nil, fmt.Errorf("%w: %s", ErrBoxNotFound, string(fourcc[:]))
}

// findBoxMulti returns payloads of all boxes matching fourcc at the current level.
func findBoxMulti(data []byte, fourcc [4]byte) ([][]byte, error) {
	var results [][]byte
	offset := 0
	for offset < len(data) {
		hdr, err := readBoxHeader(data, offset)
		if err != nil {
			return results, err
		}
		if hdr.boxType == fourcc {
			results = append(results, data[hdr.dataOffset:hdr.dataOffset+hdr.dataSize])
		}
		offset += hdr.totalSize
	}
	return results, nil
}

// readFullBoxHeader reads the version byte and 3-byte flags from a full box
// payload, returning (version, flags, remaining payload after version+flags).
func readFullBoxHeader(payload []byte) (version uint8, flags uint32, rest []byte, err error) {
	if len(payload) < 4 {
		return 0, 0, nil, ErrBoxTruncated
	}
	version = payload[0]
	flags = uint32(payload[1])<<16 | uint32(payload[2])<<8 | uint32(payload[3])
	return version, flags, payload[4:], nil
}

// fourcc converts a 4-character string to a [4]byte array.
func fourcc(s string) [4]byte {
	var b [4]byte
	copy(b[:], s)
	return b
}
