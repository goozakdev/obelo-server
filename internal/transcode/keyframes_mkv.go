package transcode

import (
	"errors"
	"fmt"
	"io"
)

// Matroska keyframe-index parsing (the long-file playlist fix, MKV half). Like the
// MP4 stss path (keyframes_mp4.go), this reads the container's own seek index —
// the Cues element, which lists the video track's keyframe timestamps — instead of
// demuxing the whole file, so a feature-length remux on a network mount indexes in
// milliseconds. mkvmerge (the tool behind virtually every remux) writes a CuePoint
// for every video keyframe by default, and ffmpeg's muxer cues every cluster it
// starts at a keyframe, so the Cues are the same set of cut points ffmpeg's HLS
// copy honors.
//
// Scope: enough EBML to walk Segment > {SeekHead, Info, Tracks, Cues} — the
// TimestampScale, the video TrackNumber (to filter CuePoints), and each CuePoint's
// CueTime. Clusters are never read; when the Cues trail them (common), the
// SeekHead at the segment start points straight to them.

// Matroska/EBML element IDs (with their class markers, as stored).
const (
	ebmlIDHeader        = 0x1A45DFA3
	ebmlIDSegment       = 0x18538067
	ebmlIDSeekHead      = 0x114D9B74
	ebmlIDSeek          = 0x4DBB
	ebmlIDSeekID        = 0x53AB
	ebmlIDSeekPosition  = 0x53AC
	ebmlIDInfo          = 0x1549A966
	ebmlIDTimestampScl  = 0x2AD7B1
	ebmlIDTracks        = 0x1654AE6B
	ebmlIDTrackEntry    = 0xAE
	ebmlIDTrackNumber   = 0xD7
	ebmlIDTrackType     = 0x83
	ebmlIDCues          = 0x1C53BB6B
	ebmlIDCuePoint      = 0xBB
	ebmlIDCueTime       = 0xB3
	ebmlIDCueTrackPos   = 0xB7
	ebmlIDCueTrack      = 0xF7
	ebmlIDCluster       = 0x1F43B675
	ebmlTrackTypeVideo  = 1
	mkvDefaultTimescale = 1_000_000 // ns per timestamp unit
)

// mkvMaxElement caps in-memory reads of index elements (Cues/Tracks/Info/SeekHead).
// Real Cues for a movie are well under a few MB.
const mkvMaxElement = 256 << 20

// mkvKeyframeTimes returns the presentation timestamps (seconds, ascending) of the
// video track's cue points — its keyframes.
func mkvKeyframeTimes(r io.ReadSeeker) ([]float64, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	// EBML header, then the Segment.
	id, size, err := ebmlReadElement(r)
	if err != nil || id != ebmlIDHeader {
		return nil, errors.New("transcode: mkv: no EBML header")
	}
	if _, err := r.Seek(size, io.SeekCurrent); err != nil {
		return nil, err
	}
	id, _, err = ebmlReadElement(r)
	if err != nil || id != ebmlIDSegment {
		return nil, errors.New("transcode: mkv: no Segment")
	}
	segStart, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}

	// Walk the segment's top-level children until the pieces are found. SeekHead
	// positions are relative to segStart; a Cues element after the clusters is
	// reached by seek, never by reading them.
	timescale := int64(mkvDefaultTimescale)
	videoTrack := int64(-1)
	var cues []byte
	var cuesPos int64 = -1

	pos := segStart
	for cues == nil {
		if _, err := r.Seek(pos, io.SeekStart); err != nil {
			return nil, err
		}
		id, size, err := ebmlReadElement(r)
		if err != nil {
			break // EOF before Cues seen inline — fall through to the SeekHead position
		}
		bodyStart, _ := r.Seek(0, io.SeekCurrent)
		switch id {
		case ebmlIDSeekHead:
			body, err := ebmlReadBody(r, size)
			if err != nil {
				return nil, err
			}
			if p := mkvSeekPosition(body, ebmlIDCues); p >= 0 {
				cuesPos = segStart + p
			}
		case ebmlIDInfo:
			body, err := ebmlReadBody(r, size)
			if err != nil {
				return nil, err
			}
			if v := ebmlChildUint(body, ebmlIDTimestampScl); v > 0 {
				timescale = int64(v)
			}
		case ebmlIDTracks:
			body, err := ebmlReadBody(r, size)
			if err != nil {
				return nil, err
			}
			videoTrack = mkvVideoTrackNumber(body)
		case ebmlIDCues:
			body, err := ebmlReadBody(r, size)
			if err != nil {
				return nil, err
			}
			cues = body
		case ebmlIDCluster:
			// Media data begins. Jump straight to the SeekHead-announced Cues; without
			// one, stop (an unindexed stream — the caller falls back).
			if cuesPos < 0 {
				return nil, errors.New("transcode: mkv: no Cues (unindexed)")
			}
			if _, err := r.Seek(cuesPos, io.SeekStart); err != nil {
				return nil, err
			}
			cid, csize, err := ebmlReadElement(r)
			if err != nil || cid != ebmlIDCues {
				return nil, errors.New("transcode: mkv: SeekHead Cues position is not a Cues element")
			}
			body, err := ebmlReadBody(r, csize)
			if err != nil {
				return nil, err
			}
			cues = body
		}
		if size < 0 {
			break // unknown-size element other than the ones handled — cannot skip
		}
		pos = bodyStart + size
	}
	if cues == nil {
		return nil, errors.New("transcode: mkv: no Cues found")
	}

	// CuePoints: keep the video track's (or all, when the track filter is unknown).
	var out []float64
	ebmlChildren(cues, func(id uint64, body []byte) {
		if id != ebmlIDCuePoint {
			return
		}
		t := int64(-1)
		match := videoTrack < 0 // no Tracks parse → accept every cue
		ebmlChildren(body, func(cid uint64, cbody []byte) {
			switch cid {
			case ebmlIDCueTime:
				t = int64(ebmlUint(cbody))
			case ebmlIDCueTrackPos:
				if int64(ebmlChildUint(cbody, ebmlIDCueTrack)) == videoTrack {
					match = true
				}
			}
		})
		if t >= 0 && match {
			out = append(out, float64(t)*float64(timescale)/1e9)
		}
	})
	if len(out) == 0 {
		return nil, errors.New("transcode: mkv: Cues list no video cue points")
	}
	return out, nil
}

// ebmlReadElement reads one element header: its ID (with class marker) and body
// size. size -1 means "unknown" (streamed) — legal only on Segment/Cluster.
func ebmlReadElement(r io.Reader) (id uint64, size int64, err error) {
	id, _, err = ebmlReadVint(r, false)
	if err != nil {
		return 0, 0, err
	}
	usize, all1, err := ebmlReadVint(r, true)
	if err != nil {
		return 0, 0, err
	}
	if all1 {
		return id, -1, nil
	}
	return id, int64(usize), nil
}

// ebmlReadVint reads one EBML variable-length integer. maskMarker clears the
// leading length-marker bit (element SIZES are masked; element IDs keep it).
// all1 reports a size whose value bits are all ones — the "unknown size" sentinel.
func ebmlReadVint(r io.Reader, maskMarker bool) (v uint64, all1 bool, err error) {
	var b [1]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, false, err
	}
	first := b[0]
	if first == 0 {
		return 0, false, errors.New("transcode: mkv: invalid EBML vint")
	}
	// Length = position of the first set bit.
	n := 1
	for mask := byte(0x80); mask > 0 && first&mask == 0; mask >>= 1 {
		n++
	}
	v = uint64(first)
	marker := uint64(0x80 >> (n - 1) << ((n - 1) * 8))
	rest := make([]byte, n-1)
	if _, err := io.ReadFull(r, rest); err != nil {
		return 0, false, err
	}
	for _, rb := range rest {
		v = v<<8 | uint64(rb)
	}
	value := v &^ marker
	maxVal := marker - 1 // all value bits set
	if maskMarker {
		return value, value == maxVal, nil
	}
	return v, false, nil
}

// ebmlReadBody reads an element body of a known size into memory.
func ebmlReadBody(r io.Reader, size int64) ([]byte, error) {
	if size < 0 || size > mkvMaxElement {
		return nil, fmt.Errorf("transcode: mkv: element size %d out of range", size)
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// ebmlChildren iterates the child elements of an in-memory body.
func ebmlChildren(body []byte, fn func(id uint64, body []byte)) {
	rd := &sliceReader{b: body}
	for rd.off < len(rd.b) {
		id, size, err := ebmlReadElement(rd)
		if err != nil || size < 0 || rd.off+int(size) > len(rd.b) {
			return
		}
		fn(id, rd.b[rd.off:rd.off+int(size)])
		rd.off += int(size)
	}
}

// ebmlChildUint returns the first child with the given ID as an unsigned int, 0 if
// absent.
func ebmlChildUint(body []byte, id uint64) uint64 {
	var out uint64
	found := false
	ebmlChildren(body, func(cid uint64, cbody []byte) {
		if cid == id && !found {
			out = ebmlUint(cbody)
			found = true
		}
	})
	return out
}

// ebmlUint decodes a big-endian EBML unsigned integer body (0–8 bytes).
func ebmlUint(b []byte) uint64 {
	var v uint64
	for _, x := range b {
		v = v<<8 | uint64(x)
	}
	return v
}

// mkvSeekPosition returns the SeekPosition for the given element ID from a SeekHead
// body, -1 when absent.
func mkvSeekPosition(seekHead []byte, want uint64) int64 {
	pos := int64(-1)
	ebmlChildren(seekHead, func(id uint64, body []byte) {
		if id != ebmlIDSeek || pos >= 0 {
			return
		}
		var target uint64
		var p int64 = -1
		ebmlChildren(body, func(cid uint64, cbody []byte) {
			switch cid {
			case ebmlIDSeekID:
				target = ebmlUint(cbody)
			case ebmlIDSeekPosition:
				p = int64(ebmlUint(cbody))
			}
		})
		if target == want && p >= 0 {
			pos = p
		}
	})
	return pos
}

// mkvVideoTrackNumber returns the TrackNumber of the first video TrackEntry, -1
// when none is found.
func mkvVideoTrackNumber(tracks []byte) int64 {
	num := int64(-1)
	ebmlChildren(tracks, func(id uint64, body []byte) {
		if id != ebmlIDTrackEntry || num >= 0 {
			return
		}
		if ebmlChildUint(body, ebmlIDTrackType) == ebmlTrackTypeVideo {
			num = int64(ebmlChildUint(body, ebmlIDTrackNumber))
		}
	})
	return num
}

// sliceReader is a minimal io.Reader over a byte slice with a visible offset (so
// ebmlChildren can hand out sub-slices without copying).
type sliceReader struct {
	b   []byte
	off int
}

func (s *sliceReader) Read(p []byte) (int, error) {
	if s.off >= len(s.b) {
		return 0, io.EOF
	}
	n := copy(p, s.b[s.off:])
	s.off += n
	return n, nil
}
