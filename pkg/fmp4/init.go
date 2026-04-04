package fmp4

import (
	"encoding/binary"
	"fmt"
)

// TrackTiming holds parsed timing info for one track from an init segment.
type TrackTiming struct {
	TrackID   uint32
	Timescale uint32 // media timescale from mdhd (ticks per second)
}

// InitTiming holds timing info parsed from an fMP4 init segment.
type InitTiming struct {
	Tracks []TrackTiming
}

// ParseInitTiming extracts per-track timing info from an fMP4 init segment.
// Parse path: data → moov → trak* → (tkhd → trackID, mdia → mdhd → timescale)
func ParseInitTiming(data []byte) (InitTiming, error) {
	moov, err := findBox(data, fourcc("moov"))
	if err != nil {
		return InitTiming{}, fmt.Errorf("fmp4: init: %w", err)
	}

	traks, err := findBoxMulti(moov, fourcc("trak"))
	if err != nil {
		return InitTiming{}, fmt.Errorf("fmp4: init: scanning trak boxes: %w", err)
	}
	if len(traks) == 0 {
		return InitTiming{}, fmt.Errorf("fmp4: init: %w: trak", ErrBoxNotFound)
	}

	var timing InitTiming
	for i, trak := range traks {
		trackID, err := parseTrackID(trak)
		if err != nil {
			return InitTiming{}, fmt.Errorf("fmp4: init: trak[%d]: %w", i, err)
		}

		timescale, err := parseMediaTimescale(trak)
		if err != nil {
			return InitTiming{}, fmt.Errorf("fmp4: init: trak[%d] (trackID=%d): %w", i, trackID, err)
		}

		timing.Tracks = append(timing.Tracks, TrackTiming{
			TrackID:   trackID,
			Timescale: timescale,
		})
	}

	return timing, nil
}

// parseTrackID extracts the track ID from a tkhd box inside a trak box.
// tkhd layout (after version+flags):
//
//	v0: creation_time(4) + modification_time(4) + track_id(4)
//	v1: creation_time(8) + modification_time(8) + track_id(4)
func parseTrackID(trak []byte) (uint32, error) {
	tkhd, err := findBox(trak, fourcc("tkhd"))
	if err != nil {
		return 0, fmt.Errorf("tkhd: %w", err)
	}

	version, _, rest, err := readFullBoxHeader(tkhd)
	if err != nil {
		return 0, fmt.Errorf("tkhd: %w", err)
	}

	var trackIDOffset int
	switch version {
	case 0:
		trackIDOffset = 8 // creation(4) + modification(4)
	case 1:
		trackIDOffset = 16 // creation(8) + modification(8)
	default:
		return 0, fmt.Errorf("tkhd: unsupported version %d", version)
	}

	if len(rest) < trackIDOffset+4 {
		return 0, fmt.Errorf("tkhd: %w", ErrBoxTruncated)
	}

	return binary.BigEndian.Uint32(rest[trackIDOffset : trackIDOffset+4]), nil
}

// parseMediaTimescale extracts the timescale from mdia → mdhd inside a trak box.
// mdhd layout (after version+flags):
//
//	v0: creation_time(4) + modification_time(4) + timescale(4)
//	v1: creation_time(8) + modification_time(8) + timescale(4)
func parseMediaTimescale(trak []byte) (uint32, error) {
	mdia, err := findBox(trak, fourcc("mdia"))
	if err != nil {
		return 0, fmt.Errorf("mdia: %w", err)
	}

	mdhd, err := findBox(mdia, fourcc("mdhd"))
	if err != nil {
		return 0, fmt.Errorf("mdhd: %w", err)
	}

	version, _, rest, err := readFullBoxHeader(mdhd)
	if err != nil {
		return 0, fmt.Errorf("mdhd: %w", err)
	}

	var timescaleOffset int
	switch version {
	case 0:
		timescaleOffset = 8 // creation(4) + modification(4)
	case 1:
		timescaleOffset = 16 // creation(8) + modification(8)
	default:
		return 0, fmt.Errorf("mdhd: unsupported version %d", version)
	}

	if len(rest) < timescaleOffset+4 {
		return 0, fmt.Errorf("mdhd: %w", ErrBoxTruncated)
	}

	return binary.BigEndian.Uint32(rest[timescaleOffset : timescaleOffset+4]), nil
}
