package transcode

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// MP4 keyframe-index parsing (the long-file playlist fix). The synthesized copy
// playlist needs the source's keyframe timestamps, and the obvious probe —
// `ffprobe -skip_frame nokey -show_entries frame=pts_time` — DEMUXES THE ENTIRE
// FILE to find them: fine for a test clip, but a feature-length movie on a network
// mount takes minutes, so the probe timed out and the session fell back to the
// end-only ffmpeg playlist (the 404 this whole fix exists to prevent). An MP4's
// moov box already carries the complete keyframe index — the stss sync-sample
// table plus the stts/ctts timing tables — so this parser reads JUST the moov (a
// few MB, one or two seeks even when it trails the mdat) and computes the exact
// keyframe presentation times in milliseconds of I/O instead of minutes.
//
// Scope: enough of ISO BMFF (mp4/m4v/mov) to time sync samples on the first video
// track — box walking (32/64-bit sizes), hdlr to find the video trak, mdhd for the
// media timescale, stts (decode times), ctts (composition offsets, so B-frame
// files time correctly), stss (sync samples), and elst (the edit-list shift ffmpeg
// applies when demuxing, so our times match ffmpeg's packet pts). Anything else is
// skipped by size.

// mp4KeyframeTimes returns the presentation timestamps (seconds, ascending) of the
// first video track's sync samples (keyframes). r must be an io.ReadSeeker over the
// whole file (moov may precede or follow mdat).
func mp4KeyframeTimes(r io.ReadSeeker) ([]float64, error) {
	moov, err := mp4FindBox(r, "moov")
	if err != nil {
		return nil, err
	}
	trak, err := mp4FindVideoTrak(moov)
	if err != nil {
		return nil, err
	}
	return mp4TrakKeyframes(trak)
}

// mp4MaxMoovSize caps the in-memory moov read. Real moov boxes are a few MB
// (sample tables for a movie); anything past this is malformed or hostile.
const mp4MaxMoovSize = 256 << 20

// mp4FindBox walks the top-level boxes and returns the payload of the first box
// with the given type, read fully into memory.
func mp4FindBox(r io.ReadSeeker, boxType string) ([]byte, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	var hdr [16]byte
	for {
		if _, err := io.ReadFull(r, hdr[:8]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, fmt.Errorf("transcode: mp4: no %s box", boxType)
			}
			return nil, err
		}
		size := int64(binary.BigEndian.Uint32(hdr[:4]))
		typ := string(hdr[4:8])
		payloadOff := int64(8)
		switch size {
		case 0:
			// Box extends to EOF — only valid for the last box.
			size = -1
		case 1:
			// 64-bit largesize follows the type.
			if _, err := io.ReadFull(r, hdr[8:16]); err != nil {
				return nil, err
			}
			size = int64(binary.BigEndian.Uint64(hdr[8:16]))
			payloadOff = 16
		}
		if typ == boxType {
			var payload int64
			if size < 0 {
				payload = mp4MaxMoovSize
			} else {
				payload = size - payloadOff
			}
			if payload < 0 || payload > mp4MaxMoovSize {
				return nil, fmt.Errorf("transcode: mp4: %s box size %d out of range", boxType, payload)
			}
			buf := make([]byte, payload)
			n, err := io.ReadFull(r, buf)
			if size < 0 && (errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)) {
				return buf[:n], nil // to-EOF box: whatever was there
			}
			if err != nil {
				return nil, err
			}
			return buf, nil
		}
		if size < 0 {
			return nil, fmt.Errorf("transcode: mp4: no %s box", boxType)
		}
		if size < payloadOff {
			return nil, fmt.Errorf("transcode: mp4: malformed box %q size %d", typ, size)
		}
		// Seek over the payload (cheap even over a network mount — mdat is skipped,
		// never read).
		if _, err := r.Seek(size-payloadOff, io.SeekCurrent); err != nil {
			return nil, err
		}
	}
}

// mp4Children iterates the child boxes of an in-memory container payload,
// calling fn with each child's type and payload. Malformed sizes end the walk.
func mp4Children(payload []byte, fn func(typ string, body []byte) bool) {
	for off := 0; off+8 <= len(payload); {
		size := int64(binary.BigEndian.Uint32(payload[off : off+4]))
		typ := string(payload[off+4 : off+8])
		hdrLen := int64(8)
		if size == 1 {
			if off+16 > len(payload) {
				return
			}
			size = int64(binary.BigEndian.Uint64(payload[off+8 : off+16]))
			hdrLen = 16
		} else if size == 0 {
			size = int64(len(payload) - off)
		}
		if size < hdrLen || int64(off)+size > int64(len(payload)) {
			return
		}
		if !fn(typ, payload[int64(off)+hdrLen:int64(off)+size]) {
			return
		}
		off += int(size)
	}
}

// mp4ChildBox returns the payload of the first direct child with the given type.
func mp4ChildBox(payload []byte, boxType string) []byte {
	var out []byte
	mp4Children(payload, func(typ string, body []byte) bool {
		if typ == boxType {
			out = body
			return false
		}
		return true
	})
	return out
}

// mp4FindVideoTrak returns the payload of the first trak whose handler is 'vide'.
func mp4FindVideoTrak(moov []byte) ([]byte, error) {
	var video []byte
	mp4Children(moov, func(typ string, body []byte) bool {
		if typ != "trak" || video != nil {
			return true
		}
		mdia := mp4ChildBox(body, "mdia")
		if mdia == nil {
			return true
		}
		hdlr := mp4ChildBox(mdia, "hdlr")
		// hdlr: version/flags(4) pre_defined(4) handler_type(4).
		if len(hdlr) >= 12 && string(hdlr[8:12]) == "vide" {
			video = body
			return false
		}
		return true
	})
	if video == nil {
		return nil, errors.New("transcode: mp4: no video trak")
	}
	return video, nil
}

// mp4TrakKeyframes computes the video trak's sync-sample presentation times in
// seconds: DTS from stts, plus the ctts composition offset (B-frame reorder), minus
// the elst edit-list shift (which ffmpeg's demuxer applies, so packet pts — the
// values the HLS muxer cuts on — carry it).
func mp4TrakKeyframes(trak []byte) ([]float64, error) {
	mdia := mp4ChildBox(trak, "mdia")
	if mdia == nil {
		return nil, errors.New("transcode: mp4: video trak has no mdia")
	}
	timescale, err := mp4Timescale(mp4ChildBox(mdia, "mdhd"))
	if err != nil {
		return nil, err
	}
	minf := mp4ChildBox(mdia, "minf")
	stbl := mp4ChildBox(minf, "stbl")
	if stbl == nil {
		return nil, errors.New("transcode: mp4: video trak has no stbl")
	}

	dts, err := mp4SampleDTS(mp4ChildBox(stbl, "stts"))
	if err != nil {
		return nil, err
	}
	ctts := mp4CompositionOffsets(mp4ChildBox(stbl, "ctts"), len(dts))
	shift := mp4EditShift(mp4ChildBox(trak, "edts"))

	sync, err := mp4SyncSamples(mp4ChildBox(stbl, "stss"), len(dts))
	if err != nil {
		return nil, err
	}

	out := make([]float64, 0, len(sync))
	for _, s := range sync { // 1-based sample numbers, ascending
		i := int(s) - 1
		if i < 0 || i >= len(dts) {
			continue
		}
		pts := dts[i] - shift
		if ctts != nil {
			pts += int64(ctts[i])
		}
		if pts < 0 {
			pts = 0
		}
		out = append(out, float64(pts)/float64(timescale))
	}
	if len(out) == 0 {
		return nil, errors.New("transcode: mp4: no sync samples resolved")
	}
	return out, nil
}

// mp4Timescale reads the media timescale from an mdhd payload (version 0 or 1).
func mp4Timescale(mdhd []byte) (uint32, error) {
	if len(mdhd) < 4 {
		return 0, errors.New("transcode: mp4: missing mdhd")
	}
	switch mdhd[0] {
	case 0: // v0: flags(3) creation(4) modification(4) timescale(4)
		if len(mdhd) < 16 {
			return 0, errors.New("transcode: mp4: short mdhd v0")
		}
		return binary.BigEndian.Uint32(mdhd[12:16]), nil
	case 1: // v1: flags(3) creation(8) modification(8) timescale(4)
		if len(mdhd) < 24 {
			return 0, errors.New("transcode: mp4: short mdhd v1")
		}
		return binary.BigEndian.Uint32(mdhd[20:24]), nil
	default:
		return 0, fmt.Errorf("transcode: mp4: unknown mdhd version %d", mdhd[0])
	}
}

// mp4SampleDTS expands the stts run-length table to a per-sample DTS array (media
// timescale units).
func mp4SampleDTS(stts []byte) ([]int64, error) {
	if len(stts) < 8 {
		return nil, errors.New("transcode: mp4: missing stts")
	}
	count := int(binary.BigEndian.Uint32(stts[4:8]))
	if len(stts) < 8+count*8 {
		return nil, errors.New("transcode: mp4: short stts")
	}
	// Sanity-bound the total sample count (a 4-hour 120fps stream is ~1.7M samples).
	const maxSamples = 16 << 20
	var out []int64
	var t int64
	off := 8
	for e := 0; e < count; e++ {
		n := int(binary.BigEndian.Uint32(stts[off : off+4]))
		delta := int64(binary.BigEndian.Uint32(stts[off+4 : off+8]))
		off += 8
		if n < 0 || len(out)+n > maxSamples {
			return nil, errors.New("transcode: mp4: stts sample count out of range")
		}
		for i := 0; i < n; i++ {
			out = append(out, t)
			t += delta
		}
	}
	if len(out) == 0 {
		return nil, errors.New("transcode: mp4: stts lists no samples")
	}
	return out, nil
}

// mp4CompositionOffsets expands ctts to a per-sample composition (pts-dts) offset,
// or nil when the trak has no ctts (no B-frame reorder). Offsets are read signed
// (version-1 semantics); version-0 files store small positive values that read
// identically.
func mp4CompositionOffsets(ctts []byte, samples int) []int32 {
	if len(ctts) < 8 {
		return nil
	}
	count := int(binary.BigEndian.Uint32(ctts[4:8]))
	if len(ctts) < 8+count*8 {
		return nil
	}
	out := make([]int32, 0, samples)
	off := 8
	for e := 0; e < count && len(out) < samples; e++ {
		n := int(binary.BigEndian.Uint32(ctts[off : off+4]))
		v := int32(binary.BigEndian.Uint32(ctts[off+4 : off+8]))
		off += 8
		for i := 0; i < n && len(out) < samples; i++ {
			out = append(out, v)
		}
	}
	for len(out) < samples {
		out = append(out, 0)
	}
	return out
}

// mp4EditShift returns the presentation shift (media timescale units) of the first
// non-empty edit-list entry — the offset ffmpeg's demuxer subtracts from every pts
// so presentation starts at ~0 (typically the first ctts offset on B-frame files).
// 0 when there is no edit list. Empty edits (media_time -1, a start delay) are rare
// and skipped.
func mp4EditShift(edts []byte) int64 {
	if edts == nil {
		return 0
	}
	elst := mp4ChildBox(edts, "elst")
	if len(elst) < 8 {
		return 0
	}
	version := elst[0]
	count := int(binary.BigEndian.Uint32(elst[4:8]))
	off := 8
	for e := 0; e < count; e++ {
		var mediaTime int64
		if version == 1 {
			if off+20 > len(elst) {
				return 0
			}
			mediaTime = int64(binary.BigEndian.Uint64(elst[off+8 : off+16]))
			off += 20
		} else {
			if off+12 > len(elst) {
				return 0
			}
			mediaTime = int64(int32(binary.BigEndian.Uint32(elst[off+4 : off+8])))
			off += 12
		}
		if mediaTime >= 0 {
			return mediaTime
		}
	}
	return 0
}

// mp4SyncSamples returns the stss sync-sample numbers (1-based, ascending). A trak
// with NO stss box has every sample sync (all-intra) per ISO BMFF, so all samples
// are returned.
func mp4SyncSamples(stss []byte, samples int) ([]uint32, error) {
	if stss == nil {
		out := make([]uint32, samples)
		for i := range out {
			out[i] = uint32(i + 1)
		}
		return out, nil
	}
	if len(stss) < 8 {
		return nil, errors.New("transcode: mp4: short stss")
	}
	count := int(binary.BigEndian.Uint32(stss[4:8]))
	if len(stss) < 8+count*4 {
		return nil, errors.New("transcode: mp4: short stss table")
	}
	out := make([]uint32, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, binary.BigEndian.Uint32(stss[8+i*4:12+i*4]))
	}
	return out, nil
}
