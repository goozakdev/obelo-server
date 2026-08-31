package scanner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// MediaInfo is the technical attributes the scanner extracts from one video
// file: the container-level summary plus its elementary Streams. It is the
// ffprobe-shaped result the scanner maps onto a store.File and store.Streams.
// Keeping it a plain struct (no ffprobe JSON tags) is the seam: the Prober
// interface returns this, so tests can fake it without spawning a process.
type MediaInfo struct {
	Container  string
	DurationMs int64
	Bitrate    int64
	SizeBytes  int64
	Streams    []StreamInfo
	// Tags are the container/stream metadata tags ffprobe already normalizes from
	// ID3v2 / Vorbis comments / MP4 atoms (issue tv-music/03). Keys are
	// lower-cased; the music identity mapper reads artist/album/album_artist/
	// track/disc/date/title/genre from here. Empty for files with no tags. The
	// existing Prober seam is reused — no new dependency, no new interface.
	Tags map[string]string
}

// Tag returns a metadata tag by (lower-cased) key, "" when absent.
func (m MediaInfo) Tag(key string) string {
	if m.Tags == nil {
		return ""
	}
	return m.Tags[strings.ToLower(key)]
}

// StreamInfo is one elementary stream's attributes.
type StreamInfo struct {
	Index     int
	Kind      string // "video" | "audio" | "subtitle"
	Codec     string
	Language  string
	Width     int
	Height    int
	Channels  int
	IsDefault bool
	// Forced is the ffprobe "forced" disposition (subtitle streams): a forced
	// text subtitle auto-displays (ADR-0020). False for video/audio.
	Forced bool
	// Title is the stream's embedded title tag (ffprobe tags.title), e.g.
	// "Director's Commentary" on an audio Stream — the label a viewer's Audio
	// menu shows to disambiguate it (audio-streams/01). "" when untagged.
	Title string
	// Commentary and HearingImpaired are ffprobe dispositions on an audio Stream
	// (the "comment" and "hearing_impaired" flags). They let the menu label a
	// commentary or SDH mix even when the file carried no title tag. Both false
	// for video/subtitle and ordinary audio.
	Commentary      bool
	HearingImpaired bool
}

// PrimaryVideo returns the first video stream, or false if the file has none.
func (m MediaInfo) PrimaryVideo() (StreamInfo, bool) {
	for _, s := range m.Streams {
		if s.Kind == "video" {
			return s, true
		}
	}
	return StreamInfo{}, false
}

// PrimaryAudio returns the first audio stream, or false if the file has none.
func (m MediaInfo) PrimaryAudio() (StreamInfo, bool) {
	for _, s := range m.Streams {
		if s.Kind == "audio" {
			return s, true
		}
	}
	return StreamInfo{}, false
}

// Prober extracts technical attributes from a video file. The real
// implementation shells out to the ffprobe binary; this interface is the test
// seam so scanner unit tests can substitute a fake without a real process or
// real media (integration tests use the real binary against fixtures).
type Prober interface {
	Probe(ctx context.Context, path string) (MediaInfo, error)
}

// FFprobe is the production Prober: it invokes the `ffprobe` binary on PATH with
//
//	-v quiet -print_format json -show_format -show_streams
//
// and parses the JSON. Determinism/offline (ADR-0002) holds: ffprobe reads only
// the local file. Binary names the executable so a test or deployment can point
// at an absolute path; empty defaults to "ffprobe".
type FFprobe struct {
	Binary string
}

// ProbeError is a probe that failed on a file that IS there: ffprobe ran and
// refused the bytes. Detail is ffprobe's own verdict, which is the only part of
// this an Admin can act on ("Invalid data found when processing input" means
// replace the file; "Permission denied" means fix the mount).
//
// It is a type rather than a formatted string because the scanner has to tell
// this failure from every other reason a Title might not assemble: an unreadable
// File is not an Unmatched one (store.UnmatchedUnreadable), and no identity
// correction will ever clear it.
type ProbeError struct {
	Path   string
	Detail string
	Err    error
}

func (e *ProbeError) Error() string {
	return fmt.Sprintf("scanner: ffprobe %q: %s", e.Path, e.Detail)
}

func (e *ProbeError) Unwrap() error { return e.Err }

// Probe runs ffprobe against path and maps its JSON onto a MediaInfo.
func (f FFprobe) Probe(ctx context.Context, path string) (MediaInfo, error) {
	bin := f.Binary
	if bin == "" {
		bin = "ffprobe"
	}
	cmd := exec.CommandContext(ctx, bin,
		// -v error, not -v quiet: on a file ffprobe refuses, its complaint is the
		// whole diagnosis, and quiet threw it away and left an Admin with nothing but
		// "exit status 1". Errors go to stderr, so stdout stays the JSON this parses.
		"-v", "error",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		return MediaInfo{}, &ProbeError{Path: path, Detail: probeDetail(err), Err: err}
	}
	return parseFFprobe(out)
}

// probeDetail is the one line of a failed probe worth showing an Admin.
//
// ffprobe writes its verdict LAST ("...: Invalid data found when processing
// input"); the lines above it are demuxer-internal noise carrying a memory
// address that changes every run. The verdict is prefixed with the file's own
// path, which is stripped: every surface that shows this reason already names the
// file, and repeating it truncates the part that matters.
func probeDetail(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		lines := strings.Split(strings.TrimSpace(string(ee.Stderr)), "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			line := strings.TrimSpace(lines[i])
			if line == "" {
				continue
			}
			if _, rest, found := strings.Cut(line, ": "); found && rest != "" {
				return rest
			}
			return line
		}
	}
	return err.Error()
}

// ffprobe JSON shapes — only the fields we consume. Numeric fields arrive as
// strings in ffprobe's JSON, so they are parsed leniently.
type ffprobeOutput struct {
	Streams []ffprobeStream `json:"streams"`
	Format  ffprobeFormat   `json:"format"`
}

type ffprobeFormat struct {
	FormatName string            `json:"format_name"`
	Duration   string            `json:"duration"`
	BitRate    string            `json:"bit_rate"`
	Size       string            `json:"size"`
	Tags       map[string]string `json:"tags"`
}

type ffprobeStream struct {
	Index       int               `json:"index"`
	CodecType   string            `json:"codec_type"`
	CodecName   string            `json:"codec_name"`
	Width       int               `json:"width"`
	Height      int               `json:"height"`
	Channels    int               `json:"channels"`
	Disposition map[string]int    `json:"disposition"`
	Tags        map[string]string `json:"tags"`
}

func parseFFprobe(data []byte) (MediaInfo, error) {
	var raw ffprobeOutput
	if err := json.Unmarshal(data, &raw); err != nil {
		return MediaInfo{}, fmt.Errorf("scanner: parsing ffprobe json: %w", err)
	}

	info := MediaInfo{
		Container:  normalizeContainer(raw.Format.FormatName),
		DurationMs: secondsToMs(raw.Format.Duration),
		Bitrate:    parseInt64(raw.Format.BitRate),
		SizeBytes:  parseInt64(raw.Format.Size),
		Tags:       collectTags(raw),
	}

	for _, s := range raw.Streams {
		kind := s.CodecType
		switch kind {
		case "video", "audio", "subtitle":
		default:
			// Skip data/attachment streams: this slice models only the three
			// playable elementary kinds (CONTEXT.md).
			continue
		}
		info.Streams = append(info.Streams, StreamInfo{
			Index:     s.Index,
			Kind:      kind,
			Codec:     s.CodecName,
			Language:  s.Tags["language"],
			Width:     s.Width,
			Height:    s.Height,
			Channels:  s.Channels,
			IsDefault: s.Disposition["default"] == 1,
			Forced:    s.Disposition["forced"] == 1,
			// tags.title + the commentary/hearing-impaired dispositions feed the
			// Audio menu's label and later slices' Remembered-audio trait matching
			// (audio-streams/01). Captured for every stream; only audio consumes them.
			Title:           strings.TrimSpace(s.Tags["title"]),
			Commentary:      s.Disposition["comment"] == 1,
			HearingImpaired: s.Disposition["hearing_impaired"] == 1,
		})
	}
	return info, nil
}

// collectTags merges the metadata tags ffprobe surfaces, keyed lower-case. The
// container-level format.tags are authoritative (FLAC/MP3/MP4 atoms land there);
// stream-level tags (Vorbis comments in OGG/Opus land on the audio stream) fill
// any key not already set by format. Returns nil when there are no tags so a
// tagless file carries no map (the music mapper then falls back to the path).
func collectTags(raw ffprobeOutput) map[string]string {
	out := map[string]string{}
	add := func(tags map[string]string) {
		for k, v := range tags {
			lk := strings.ToLower(strings.TrimSpace(k))
			if lk == "" || v == "" {
				continue
			}
			if _, seen := out[lk]; !seen {
				out[lk] = v
			}
		}
	}
	add(raw.Format.Tags)
	for _, s := range raw.Streams {
		// Only audio streams carry the music identity tags (Vorbis comments); a
		// container handler/encoder tag on a video stream is irrelevant here.
		if s.CodecType == "audio" {
			add(s.Tags)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// normalizeContainer reduces ffprobe's comma-joined format_name (e.g.
// "mov,mp4,m4a,3gp,3g2,mj2" or "matroska,webm") to a single representative
// token for display. The first element is ffprobe's primary name.
func normalizeContainer(formatName string) string {
	if formatName == "" {
		return ""
	}
	first := formatName
	if i := strings.IndexByte(formatName, ','); i >= 0 {
		first = formatName[:i]
	}
	return strings.TrimSpace(first)
}

// secondsToMs parses ffprobe's fractional-seconds duration string into integer
// milliseconds, returning 0 when absent or unparseable.
func secondsToMs(s string) int64 {
	if s == "" {
		return 0
	}
	secs, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return int64(secs * 1000)
}

func parseInt64(s string) int64 {
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// ProbeDetailForTest exposes probeDetail to the catalog-side test that pins what
// an Admin reads when ffprobe refuses a file. It is here rather than in an
// export_test.go because the assertion belongs beside the rest of the unreadable
// behaviour, which spans both packages.
func ProbeDetailForTest(err error) string { return probeDetail(err) }
