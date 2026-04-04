package fmp4

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildTfhd builds a tfhd full box.
func buildTfhd(trackID uint32, flags uint32, opts tfhdOpts) []byte {
	var payload []byte
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, trackID)
	payload = append(payload, buf...)

	if flags&tfhdBaseDataOffset != 0 {
		payload = append(payload, make([]byte, 8)...)
	}
	if flags&tfhdSampleDescIndex != 0 {
		payload = append(payload, make([]byte, 4)...)
	}
	if flags&tfhdDefaultSampleDuration != 0 {
		b := make([]byte, 4)
		binary.BigEndian.PutUint32(b, opts.defaultSampleDuration)
		payload = append(payload, b...)
	}
	return makeFullBox("tfhd", 0, flags, payload)
}

type tfhdOpts struct {
	defaultSampleDuration uint32
}

// buildTfdt builds a tfdt full box.
func buildTfdt(version uint8, baseMediaDecodeTime uint64) []byte {
	var payload []byte
	switch version {
	case 0:
		payload = make([]byte, 4)
		binary.BigEndian.PutUint32(payload, uint32(baseMediaDecodeTime))
	case 1:
		payload = make([]byte, 8)
		binary.BigEndian.PutUint64(payload, baseMediaDecodeTime)
	}
	return makeFullBox("tfdt", version, 0, payload)
}

// buildTrunWithDurations builds a trun box with per-sample durations.
func buildTrunWithDurations(durations []uint32) []byte {
	var payload []byte
	// sample_count
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, uint32(len(durations)))
	payload = append(payload, buf...)
	// per-sample durations
	for _, d := range durations {
		b := make([]byte, 4)
		binary.BigEndian.PutUint32(b, d)
		payload = append(payload, b...)
	}
	return makeFullBox("trun", 0, trunSampleDuration, payload)
}

// buildTrunNoDurations builds a trun box without per-sample durations.
func buildTrunNoDurations(sampleCount uint32) []byte {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, sampleCount)
	return makeFullBox("trun", 0, 0, payload)
}

// buildTrunWithDurationsAndSizes builds a trun with both duration and size per sample.
func buildTrunWithDurationsAndSizes(durations []uint32, sizes []uint32) []byte {
	var payload []byte
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, uint32(len(durations)))
	payload = append(payload, buf...)
	for i := range durations {
		b := make([]byte, 4)
		binary.BigEndian.PutUint32(b, durations[i])
		payload = append(payload, b...)
		binary.BigEndian.PutUint32(b, sizes[i])
		payload = append(payload, b...)
	}
	return makeFullBox("trun", 0, trunSampleDuration|trunSampleSize, payload)
}

// buildTrunWithDataOffset builds a trun with data_offset and per-sample durations.
func buildTrunWithDataOffset(dataOffset int32, durations []uint32) []byte {
	var payload []byte
	// sample_count
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, uint32(len(durations)))
	payload = append(payload, buf...)
	// data_offset
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(dataOffset))
	payload = append(payload, b...)
	// per-sample durations
	for _, d := range durations {
		b := make([]byte, 4)
		binary.BigEndian.PutUint32(b, d)
		payload = append(payload, b...)
	}
	return makeFullBox("trun", 0, trunDataOffset|trunSampleDuration, payload)
}

// buildTraf builds a traf box containing tfhd, tfdt, and trun.
func buildTrafBox(tfhd, tfdt, trun []byte) []byte {
	inner := append(append(tfhd, tfdt...), trun...)
	return makeBox("traf", inner)
}

func TestParseSegmentTiming_PerSampleDurations(t *testing.T) {
	tfhd := buildTfhd(1, 0, tfhdOpts{})
	tfdt := buildTfdt(1, 180000)
	trun := buildTrunWithDurations([]uint32{3003, 3003, 3003})
	traf := buildTrafBox(tfhd, tfdt, trun)
	moof := makeBox("moof", traf)

	timing, err := ParseSegmentTiming(moof)
	require.NoError(t, err)
	require.Len(t, timing.Tracks, 1)

	track := timing.Tracks[0]
	assert.Equal(t, uint32(1), track.TrackID)
	assert.Equal(t, uint64(180000), track.BaseMediaDecodeTime)
	assert.Equal(t, uint32(3), track.SampleCount)
	assert.Equal(t, uint64(9009), track.TotalDuration)
}

func TestParseSegmentTiming_DefaultSampleDuration(t *testing.T) {
	tfhd := buildTfhd(1, tfhdDefaultSampleDuration, tfhdOpts{defaultSampleDuration: 3003})
	tfdt := buildTfdt(0, 0)
	trun := buildTrunNoDurations(48)
	traf := buildTrafBox(tfhd, tfdt, trun)
	moof := makeBox("moof", traf)

	timing, err := ParseSegmentTiming(moof)
	require.NoError(t, err)
	require.Len(t, timing.Tracks, 1)

	track := timing.Tracks[0]
	assert.Equal(t, uint32(48), track.SampleCount)
	assert.Equal(t, uint64(48*3003), track.TotalDuration)
}

func TestParseSegmentTiming_TwoTrafs(t *testing.T) {
	// Video track
	tfhd1 := buildTfhd(1, 0, tfhdOpts{})
	tfdt1 := buildTfdt(1, 180180)
	trun1 := buildTrunWithDurations([]uint32{3003, 3003})
	traf1 := buildTrafBox(tfhd1, tfdt1, trun1)

	// Audio track
	tfhd2 := buildTfhd(2, tfhdDefaultSampleDuration, tfhdOpts{defaultSampleDuration: 1024})
	tfdt2 := buildTfdt(1, 96000)
	trun2 := buildTrunNoDurations(94)
	traf2 := buildTrafBox(tfhd2, tfdt2, trun2)

	moof := makeBox("moof", append(traf1, traf2...))

	timing, err := ParseSegmentTiming(moof)
	require.NoError(t, err)
	require.Len(t, timing.Tracks, 2)

	assert.Equal(t, uint32(1), timing.Tracks[0].TrackID)
	assert.Equal(t, uint64(180180), timing.Tracks[0].BaseMediaDecodeTime)
	assert.Equal(t, uint64(6006), timing.Tracks[0].TotalDuration)

	assert.Equal(t, uint32(2), timing.Tracks[1].TrackID)
	assert.Equal(t, uint64(96000), timing.Tracks[1].BaseMediaDecodeTime)
	assert.Equal(t, uint64(94*1024), timing.Tracks[1].TotalDuration)
}

func TestParseSegmentTiming_TfdtV0(t *testing.T) {
	tfhd := buildTfhd(1, tfhdDefaultSampleDuration, tfhdOpts{defaultSampleDuration: 1000})
	tfdt := buildTfdt(0, 12345)
	trun := buildTrunNoDurations(10)
	traf := buildTrafBox(tfhd, tfdt, trun)
	moof := makeBox("moof", traf)

	timing, err := ParseSegmentTiming(moof)
	require.NoError(t, err)
	assert.Equal(t, uint64(12345), timing.Tracks[0].BaseMediaDecodeTime)
}

func TestParseSegmentTiming_TfhdWithOptionalFields(t *testing.T) {
	// tfhd with base_data_offset + sample_desc_index + default_sample_duration
	flags := uint32(tfhdBaseDataOffset | tfhdSampleDescIndex | tfhdDefaultSampleDuration)
	tfhd := buildTfhd(1, flags, tfhdOpts{defaultSampleDuration: 2002})
	tfdt := buildTfdt(0, 0)
	trun := buildTrunNoDurations(24)
	traf := buildTrafBox(tfhd, tfdt, trun)
	moof := makeBox("moof", traf)

	timing, err := ParseSegmentTiming(moof)
	require.NoError(t, err)
	assert.Equal(t, uint32(1), timing.Tracks[0].TrackID)
	assert.Equal(t, uint64(24*2002), timing.Tracks[0].TotalDuration)
}

func TestParseSegmentTiming_TrunWithDataOffset(t *testing.T) {
	tfhd := buildTfhd(1, 0, tfhdOpts{})
	tfdt := buildTfdt(0, 0)
	trun := buildTrunWithDataOffset(100, []uint32{1001, 1001})
	traf := buildTrafBox(tfhd, tfdt, trun)
	moof := makeBox("moof", traf)

	timing, err := ParseSegmentTiming(moof)
	require.NoError(t, err)
	assert.Equal(t, uint64(2002), timing.Tracks[0].TotalDuration)
}

func TestParseSegmentTiming_TrunWithDurationsAndSizes(t *testing.T) {
	tfhd := buildTfhd(1, 0, tfhdOpts{})
	tfdt := buildTfdt(0, 0)
	trun := buildTrunWithDurationsAndSizes(
		[]uint32{3003, 3003},
		[]uint32{50000, 60000},
	)
	traf := buildTrafBox(tfhd, tfdt, trun)
	moof := makeBox("moof", traf)

	timing, err := ParseSegmentTiming(moof)
	require.NoError(t, err)
	assert.Equal(t, uint64(6006), timing.Tracks[0].TotalDuration)
}

func TestParseSegmentTiming_NoMoof(t *testing.T) {
	data := makeBox("mdat", []byte{0, 0, 0, 0})
	_, err := ParseSegmentTiming(data)
	assert.ErrorIs(t, err, ErrBoxNotFound)
}

func TestParseSegmentTiming_NoDurationInfo(t *testing.T) {
	tfhd := buildTfhd(1, 0, tfhdOpts{}) // no default_sample_duration
	tfdt := buildTfdt(0, 0)
	trun := buildTrunNoDurations(10) // no per-sample durations
	traf := buildTrafBox(tfhd, tfdt, trun)
	moof := makeBox("moof", traf)

	_, err := ParseSegmentTiming(moof)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no sample durations")
}

func TestParseSegmentTiming_GarbageData(t *testing.T) {
	_, err := ParseSegmentTiming([]byte("not a segment"))
	assert.Error(t, err)
}
