package fmp4

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildTkhd builds a tkhd full box with the given version and track ID.
func buildTkhd(version uint8, trackID uint32) []byte {
	var payload []byte
	switch version {
	case 0:
		// creation(4) + modification(4) + track_id(4) + reserved(4) + duration(4)
		payload = make([]byte, 20)
		binary.BigEndian.PutUint32(payload[8:12], trackID)
	case 1:
		// creation(8) + modification(8) + track_id(4) + reserved(4) + duration(8)
		payload = make([]byte, 32)
		binary.BigEndian.PutUint32(payload[16:20], trackID)
	}
	return makeFullBox("tkhd", version, 0, payload)
}

// buildMdhd builds an mdhd full box with the given version and timescale.
func buildMdhd(version uint8, timescale uint32) []byte {
	var payload []byte
	switch version {
	case 0:
		// creation(4) + modification(4) + timescale(4) + duration(4)
		payload = make([]byte, 16)
		binary.BigEndian.PutUint32(payload[8:12], timescale)
	case 1:
		// creation(8) + modification(8) + timescale(4) + duration(8)
		payload = make([]byte, 28)
		binary.BigEndian.PutUint32(payload[16:20], timescale)
	}
	return makeFullBox("mdhd", version, 0, payload)
}

// buildTrak builds a trak box containing tkhd and mdia→mdhd.
func buildTrak(tkhdVersion uint8, trackID uint32, mdhdVersion uint8, timescale uint32) []byte {
	tkhd := buildTkhd(tkhdVersion, trackID)
	mdhd := buildMdhd(mdhdVersion, timescale)
	mdia := makeBox("mdia", mdhd)
	trak := makeBox("trak", append(tkhd, mdia...))
	return trak
}

func TestParseInitTiming_SingleTrackV0(t *testing.T) {
	trak := buildTrak(0, 1, 0, 90000)
	moov := makeBox("moov", trak)

	timing, err := ParseInitTiming(moov)
	require.NoError(t, err)
	require.Len(t, timing.Tracks, 1)
	assert.Equal(t, uint32(1), timing.Tracks[0].TrackID)
	assert.Equal(t, uint32(90000), timing.Tracks[0].Timescale)
}

func TestParseInitTiming_SingleTrackV1(t *testing.T) {
	trak := buildTrak(1, 2, 1, 48000)
	moov := makeBox("moov", trak)

	timing, err := ParseInitTiming(moov)
	require.NoError(t, err)
	require.Len(t, timing.Tracks, 1)
	assert.Equal(t, uint32(2), timing.Tracks[0].TrackID)
	assert.Equal(t, uint32(48000), timing.Tracks[0].Timescale)
}

func TestParseInitTiming_TwoTracks(t *testing.T) {
	videoTrak := buildTrak(0, 1, 0, 90000)
	audioTrak := buildTrak(0, 2, 0, 48000)
	moov := makeBox("moov", append(videoTrak, audioTrak...))

	timing, err := ParseInitTiming(moov)
	require.NoError(t, err)
	require.Len(t, timing.Tracks, 2)

	assert.Equal(t, uint32(1), timing.Tracks[0].TrackID)
	assert.Equal(t, uint32(90000), timing.Tracks[0].Timescale)

	assert.Equal(t, uint32(2), timing.Tracks[1].TrackID)
	assert.Equal(t, uint32(48000), timing.Tracks[1].Timescale)
}

func TestParseInitTiming_NoMoov(t *testing.T) {
	data := makeBox("ftyp", []byte{0, 0, 0, 0})
	_, err := ParseInitTiming(data)
	assert.ErrorIs(t, err, ErrBoxNotFound)
}

func TestParseInitTiming_NoTrak(t *testing.T) {
	mvhd := makeFullBox("mvhd", 0, 0, make([]byte, 20))
	moov := makeBox("moov", mvhd)
	_, err := ParseInitTiming(moov)
	assert.ErrorIs(t, err, ErrBoxNotFound)
}

func TestParseInitTiming_NoMdia(t *testing.T) {
	tkhd := buildTkhd(0, 1)
	trak := makeBox("trak", tkhd) // no mdia
	moov := makeBox("moov", trak)
	_, err := ParseInitTiming(moov)
	assert.ErrorIs(t, err, ErrBoxNotFound)
}

func TestParseInitTiming_MixedVersions(t *testing.T) {
	// Track 1: v0 tkhd, v1 mdhd
	trak1 := buildTrak(0, 1, 1, 90000)
	// Track 2: v1 tkhd, v0 mdhd
	trak2 := buildTrak(1, 2, 0, 44100)
	moov := makeBox("moov", append(trak1, trak2...))

	timing, err := ParseInitTiming(moov)
	require.NoError(t, err)
	require.Len(t, timing.Tracks, 2)
	assert.Equal(t, uint32(90000), timing.Tracks[0].Timescale)
	assert.Equal(t, uint32(44100), timing.Tracks[1].Timescale)
}

func TestParseInitTiming_GarbageData(t *testing.T) {
	_, err := ParseInitTiming([]byte("not an mp4 file"))
	assert.Error(t, err)
}
