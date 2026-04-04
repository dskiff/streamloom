package fmp4

import (
	"encoding/binary"
	"fmt"
)

// TrackSegmentTiming holds parsed timing info for one track in a media segment.
type TrackSegmentTiming struct {
	TrackID             uint32
	BaseMediaDecodeTime uint64 // from tfdt (in timescale units)
	TotalDuration       uint64 // sum of sample durations (in timescale units)
	SampleCount         uint32
}

// SegmentTiming holds timing info parsed from an fMP4 media segment.
type SegmentTiming struct {
	Tracks []TrackSegmentTiming
}

// ParseSegmentTiming extracts per-track timing info from an fMP4 media segment.
// Parse path: data → moof → traf* → (tfhd, tfdt, trun)
func ParseSegmentTiming(data []byte) (SegmentTiming, error) {
	moof, err := findBox(data, fourcc("moof"))
	if err != nil {
		return SegmentTiming{}, fmt.Errorf("fmp4: segment: %w", err)
	}

	trafs, err := findBoxMulti(moof, fourcc("traf"))
	if err != nil {
		return SegmentTiming{}, fmt.Errorf("fmp4: segment: scanning traf boxes: %w", err)
	}
	if len(trafs) == 0 {
		return SegmentTiming{}, fmt.Errorf("fmp4: segment: %w: traf", ErrBoxNotFound)
	}

	var timing SegmentTiming
	for i, traf := range trafs {
		track, err := parseTraf(traf)
		if err != nil {
			return SegmentTiming{}, fmt.Errorf("fmp4: segment: traf[%d]: %w", i, err)
		}
		timing.Tracks = append(timing.Tracks, track)
	}

	return timing, nil
}

// parseTraf parses a single traf box to extract timing information.
func parseTraf(traf []byte) (TrackSegmentTiming, error) {
	trackID, defaultSampleDuration, err := parseTfhd(traf)
	if err != nil {
		return TrackSegmentTiming{}, err
	}

	baseDecodeTime, err := parseTfdt(traf)
	if err != nil {
		return TrackSegmentTiming{}, err
	}

	sampleCount, totalDuration, err := parseTrun(traf, defaultSampleDuration)
	if err != nil {
		return TrackSegmentTiming{}, err
	}

	return TrackSegmentTiming{
		TrackID:             trackID,
		BaseMediaDecodeTime: baseDecodeTime,
		TotalDuration:       totalDuration,
		SampleCount:         sampleCount,
	}, nil
}

// tfhd flags for optional fields.
const (
	tfhdBaseDataOffset        = 0x000001
	tfhdSampleDescIndex       = 0x000002
	tfhdDefaultSampleDuration = 0x000008
)

// parseTfhd extracts track ID and optional default sample duration from tfhd.
//
// tfhd layout (after version+flags):
//
//	track_id(4)
//	[base_data_offset(8)]    if flag 0x01
//	[sample_desc_index(4)]   if flag 0x02
//	[default_sample_dur(4)]  if flag 0x08
func parseTfhd(traf []byte) (trackID uint32, defaultSampleDuration uint32, err error) {
	tfhd, err := findBox(traf, fourcc("tfhd"))
	if err != nil {
		return 0, 0, fmt.Errorf("tfhd: %w", err)
	}

	_, flags, rest, err := readFullBoxHeader(tfhd)
	if err != nil {
		return 0, 0, fmt.Errorf("tfhd: %w", err)
	}

	if len(rest) < 4 {
		return 0, 0, fmt.Errorf("tfhd: %w", ErrBoxTruncated)
	}
	trackID = binary.BigEndian.Uint32(rest[0:4])
	off := 4

	if flags&tfhdBaseDataOffset != 0 {
		off += 8
	}
	if flags&tfhdSampleDescIndex != 0 {
		off += 4
	}
	if flags&tfhdDefaultSampleDuration != 0 {
		if len(rest) < off+4 {
			return 0, 0, fmt.Errorf("tfhd: default_sample_duration: %w", ErrBoxTruncated)
		}
		defaultSampleDuration = binary.BigEndian.Uint32(rest[off : off+4])
	}

	return trackID, defaultSampleDuration, nil
}

// parseTfdt extracts baseMediaDecodeTime from tfdt.
//
// tfdt layout (after version+flags):
//
//	v0: baseMediaDecodeTime(4)
//	v1: baseMediaDecodeTime(8)
func parseTfdt(traf []byte) (uint64, error) {
	tfdt, err := findBox(traf, fourcc("tfdt"))
	if err != nil {
		return 0, fmt.Errorf("tfdt: %w", err)
	}

	version, _, rest, err := readFullBoxHeader(tfdt)
	if err != nil {
		return 0, fmt.Errorf("tfdt: %w", err)
	}

	switch version {
	case 0:
		if len(rest) < 4 {
			return 0, fmt.Errorf("tfdt: %w", ErrBoxTruncated)
		}
		return uint64(binary.BigEndian.Uint32(rest[0:4])), nil
	case 1:
		if len(rest) < 8 {
			return 0, fmt.Errorf("tfdt: %w", ErrBoxTruncated)
		}
		return binary.BigEndian.Uint64(rest[0:8]), nil
	default:
		return 0, fmt.Errorf("tfdt: unsupported version %d", version)
	}
}

// trun flags for optional fields.
const (
	trunDataOffset       = 0x000001
	trunFirstSampleFlags = 0x000004
	trunSampleDuration   = 0x000100
	trunSampleSize       = 0x000200
	trunSampleFlags      = 0x000400
	trunSampleCTO        = 0x000800 // composition time offset
)

// parseTrun extracts sample count and computes total duration from trun.
// If per-sample durations are not present (flag 0x100), falls back to
// defaultSampleDuration from tfhd.
//
// trun layout (after version+flags):
//
//	sample_count(4)
//	[data_offset(4)]         if flag 0x01
//	[first_sample_flags(4)]  if flag 0x04
//	per-sample records (sample_count times):
//	  [sample_duration(4)]   if flag 0x100
//	  [sample_size(4)]       if flag 0x200
//	  [sample_flags(4)]      if flag 0x400
//	  [sample_cto(4)]        if flag 0x800
func parseTrun(traf []byte, defaultSampleDuration uint32) (sampleCount uint32, totalDuration uint64, err error) {
	trun, err := findBox(traf, fourcc("trun"))
	if err != nil {
		return 0, 0, fmt.Errorf("trun: %w", err)
	}

	_, flags, rest, err := readFullBoxHeader(trun)
	if err != nil {
		return 0, 0, fmt.Errorf("trun: %w", err)
	}

	if len(rest) < 4 {
		return 0, 0, fmt.Errorf("trun: %w", ErrBoxTruncated)
	}
	sampleCount = binary.BigEndian.Uint32(rest[0:4])
	off := 4

	if flags&trunDataOffset != 0 {
		off += 4
	}
	if flags&trunFirstSampleFlags != 0 {
		off += 4
	}

	// Calculate per-sample record size based on flags.
	recordSize := 0
	var durationOffsetInRecord int
	if flags&trunSampleDuration != 0 {
		durationOffsetInRecord = recordSize
		recordSize += 4
	}
	if flags&trunSampleSize != 0 {
		recordSize += 4
	}
	if flags&trunSampleFlags != 0 {
		recordSize += 4
	}
	if flags&trunSampleCTO != 0 {
		recordSize += 4
	}

	if flags&trunSampleDuration != 0 {
		// Sum per-sample durations.
		needed := off + recordSize*int(sampleCount)
		if len(rest) < needed {
			return 0, 0, fmt.Errorf("trun: sample records: %w", ErrBoxTruncated)
		}
		for i := 0; i < int(sampleCount); i++ {
			sampleOff := off + i*recordSize + durationOffsetInRecord
			dur := binary.BigEndian.Uint32(rest[sampleOff : sampleOff+4])
			totalDuration += uint64(dur)
		}
	} else if defaultSampleDuration > 0 {
		// Fall back to tfhd default_sample_duration.
		totalDuration = uint64(sampleCount) * uint64(defaultSampleDuration)
	} else {
		return sampleCount, 0, fmt.Errorf("trun: no sample durations and no default_sample_duration")
	}

	return sampleCount, totalDuration, nil
}
