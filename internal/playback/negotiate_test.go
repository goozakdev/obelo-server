package playback

import (
	"testing"

	"github.com/marioquake/juicebox/internal/store"
)

// Unit tests for the negotiation core and edition selection — the logic the
// black-box API tests exercise end-to-end, isolated here so the merge rules and
// the auto-select-by-resolution rule are pinned directly.

func mp4File(height int, bitrate int64) store.File {
	return store.File{
		ID:         "f1",
		Path:       "/movies/x.mp4",
		Container:  "mov,mp4,m4a,3gp,3g2,mj2", // ffprobe's mp4-family format string
		VideoCodec: "h264",
		AudioCodec: "aac",
		Height:     height,
		Bitrate:    bitrate,
		Present:    true,
		Streams: []store.Stream{
			{Kind: "video", Codec: "h264", Height: height, IsDefault: true},
			{Kind: "audio", Codec: "aac", Channels: 2, Language: "en", IsDefault: true},
		},
	}
}

func h264Profile() DeviceProfile {
	return DeviceProfile{
		Containers:       []string{"mp4", "mkv"},
		VideoCodecs:      []VideoCodecSupport{{Codec: "h264", MaxResolution: "1080p"}},
		AudioCodecs:      []string{"aac", "ac3"},
		MaxAudioChannels: 8,
	}
}

func TestNegotiateDirectPlay(t *testing.T) {
	ed := store.Edition{ID: "e1", Name: "1080p", Files: []store.File{mp4File(1080, 6_000_000)}}
	dec, unsup := Negotiate(h264Profile(), Constraints{MaxBitrate: 8_000_000, MaxResolution: "1080p"}, ed, ed.Files[0])
	if unsup != nil {
		t.Fatalf("unexpected unsupported: %v", unsup)
	}
	if dec.Tier != TierDirectPlay {
		t.Errorf("tier = %q, want directPlay", dec.Tier)
	}
	if dec.VideoStream.Codec != "h264" || dec.AudioStream.Codec != "aac" {
		t.Errorf("streams = %q/%q, want h264/aac", dec.VideoStream.Codec, dec.AudioStream.Codec)
	}
	if dec.EstimatedBitrate != 6_000_000 {
		t.Errorf("estimatedBitrate = %d, want 6000000", dec.EstimatedBitrate)
	}
	// Negotiate is the pure edition/stream gate; the Subtitle-track list is attached
	// by the Service (which holds the Title detail), so it stays nil here.
	if dec.Subtitles != nil {
		t.Errorf("Subtitles = %v, want nil (attached by the Service, not Negotiate)", dec.Subtitles)
	}
}

func TestNegotiateReasons(t *testing.T) {
	ed := store.Edition{ID: "e1", Files: []store.File{mp4File(1080, 6_000_000)}}
	f := ed.Files[0]

	cases := []struct {
		name       string
		profile    DeviceProfile
		cons       Constraints
		file       store.File
		wantReason Reason
	}{
		{
			name:       "container unsupported",
			profile:    DeviceProfile{Containers: []string{"webm"}, VideoCodecs: h264Profile().VideoCodecs, AudioCodecs: []string{"aac"}},
			file:       f,
			wantReason: ReasonContainer,
		},
		{
			name:       "video codec unsupported",
			profile:    DeviceProfile{Containers: []string{"mp4"}, VideoCodecs: []VideoCodecSupport{{Codec: "hevc"}}, AudioCodecs: []string{"aac"}},
			file:       f,
			wantReason: ReasonVideoCodec,
		},
		{
			name:       "audio codec unsupported",
			profile:    DeviceProfile{Containers: []string{"mp4"}, VideoCodecs: h264Profile().VideoCodecs, AudioCodecs: []string{"flac"}},
			file:       f,
			wantReason: ReasonAudioCodec,
		},
		{
			name:       "bitrate over cap",
			profile:    h264Profile(),
			cons:       Constraints{MaxBitrate: 1_000_000},
			file:       f,
			wantReason: ReasonBitrate,
		},
		{
			name:       "resolution over device codec ceiling",
			profile:    DeviceProfile{Containers: []string{"mp4"}, VideoCodecs: []VideoCodecSupport{{Codec: "h264", MaxResolution: "720p"}}, AudioCodecs: []string{"aac"}},
			file:       mp4File(1080, 6_000_000),
			wantReason: ReasonResolution,
		},
		{
			name:       "resolution over constraint",
			profile:    h264Profile(),
			cons:       Constraints{MaxResolution: "720p"},
			file:       mp4File(1080, 6_000_000),
			wantReason: ReasonResolution,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, unsup := Negotiate(tc.profile, tc.cons, ed, tc.file)
			if unsup == nil {
				t.Fatalf("want unsupported %q, got directPlay", tc.wantReason)
			}
			if unsup.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", unsup.Reason, tc.wantReason)
			}
		})
	}
}

// mkvFile mirrors mp4File but in the matroska container with codecs the caller
// can vary — the directStream fixture shape (codecs fine, container wrong).
func mkvFile(videoCodec, audioCodec string, height int, bitrate int64) store.File {
	return store.File{
		ID:         "fm",
		Path:       "/movies/x.mkv",
		Container:  "matroska,webm", // ffprobe's mkv format string
		VideoCodec: videoCodec,
		AudioCodec: audioCodec,
		Height:     height,
		Bitrate:    bitrate,
		Present:    true,
		Streams: []store.Stream{
			{Kind: "video", Codec: videoCodec, Height: height, IsDefault: true},
			{Kind: "audio", Codec: audioCodec, Channels: 2, Language: "en", IsDefault: true},
		},
	}
}

// TestTierForReason pins the reason→tier classification directly: container is
// the only remux-able reason; everything else is transcode.
func TestTierForReason(t *testing.T) {
	cases := map[Reason]Tier{
		ReasonContainer:     TierDirectStream,
		ReasonVideoCodec:    TierTranscode,
		ReasonAudioCodec:    TierTranscode,
		ReasonResolution:    TierTranscode,
		ReasonBitrate:       TierTranscode,
		ReasonAudioChannels: TierTranscode,
		ReasonNoVideo:       TierTranscode,
		ReasonNoFile:        TierTranscode,
	}
	for r, want := range cases {
		if got := TierForReason(r); got != want {
			t.Errorf("TierForReason(%q) = %q, want %q", r, got, want)
		}
	}
}

// TestSelectEditionDirectStreamOnContainerOnly: an mkv File whose codecs the
// profile fully supports but whose CONTAINER it does not → a directStream
// (remux) Decision, not a direct play and not an error. This is the tier-2
// classification the slice delivers.
func TestSelectEditionDirectStreamOnContainerOnly(t *testing.T) {
	// Profile supports mpeg4 + mp3 codecs but only the mp4 container.
	profile := DeviceProfile{
		Containers:       []string{"mp4"},
		VideoCodecs:      []VideoCodecSupport{{Codec: "mpeg4", MaxResolution: "1080p"}, {Codec: "h264"}},
		AudioCodecs:      []string{"mp3", "aac"},
		MaxAudioChannels: 8,
	}
	editions := []store.Edition{
		{ID: "mkv", Name: "remux", Files: []store.File{mkvFile("mpeg4", "mp3", 144, 1_000_000)}},
	}
	dec, unsup := SelectEdition(profile, Constraints{MaxBitrate: 100_000_000}, editions, "")
	if unsup != nil {
		t.Fatalf("unexpected unsupported (want directStream): %v", unsup)
	}
	if dec.Tier != TierDirectStream {
		t.Errorf("tier = %q, want directStream", dec.Tier)
	}
	if dec.VideoStream.Codec != "mpeg4" || dec.AudioStream.Codec != "mp3" {
		t.Errorf("streams = %q/%q, want mpeg4/mp3", dec.VideoStream.Codec, dec.AudioStream.Codec)
	}
}

// audioOnlyFile is a Music Track fixture: a single audio Stream, no video, in the
// given container/codec — the audio-only negotiation shape (CONTEXT.md Track).
func audioOnlyFile(container, audioCodec string, bitrate int64) store.File {
	return store.File{
		ID:         "fa",
		Path:       "/music/x." + container,
		Container:  container,
		AudioCodec: audioCodec,
		Bitrate:    bitrate,
		Present:    true,
		Streams: []store.Stream{
			{Kind: "audio", Codec: audioCodec, Channels: 2, IsDefault: true},
		},
	}
}

// TestSelectEditionAudioOnlyFlacTranscodes is the regression for the silent-music
// bug: a FLAC Track whose codec the client CAN decode, but whose container it
// cannot demux, must NOT be remuxed (directStream). FLAC cannot be carried in the
// HLS segment container (MPEG-TS) — it muxes as an unplayable "private data
// stream" — so the only correct outcome is a transcode (FLAC→AAC), even though a
// naive "container is the only client mismatch" check would pick remux.
func TestSelectEditionAudioOnlyFlacTranscodes(t *testing.T) {
	profile := DeviceProfile{
		Containers:       []string{"mp4"},         // NOT "flac"
		AudioCodecs:      []string{"flac", "aac"}, // client CAN decode flac…
		MaxAudioChannels: 8,
	}
	editions := []store.Edition{
		{ID: "flac", Files: []store.File{audioOnlyFile("flac", "flac", 900_000)}},
	}
	dec, unsup := SelectEdition(profile, Constraints{MaxBitrate: 100_000_000}, editions, "")
	if unsup != nil {
		t.Fatalf("want transcode Decision, got Unsupported: %v", unsup)
	}
	if dec.Tier != TierTranscode {
		t.Errorf("tier = %q, want transcode (FLAC cannot be remuxed into MPEG-TS)", dec.Tier)
	}
	if !dec.AudioOnly {
		t.Errorf("AudioOnly = false, want true for a Music Track")
	}
}

// TestSelectEditionAudioOnlyAacRemuxes pins the other side of the copy-safe gate:
// an AAC Track in a container the client cannot demux but whose codec IS carriable
// in MPEG-TS → directStream (the genuine audio remux win, no re-encode).
func TestSelectEditionAudioOnlyAacRemuxes(t *testing.T) {
	profile := DeviceProfile{
		Containers:       []string{"mp4"}, // NOT raw "aac"/adts
		AudioCodecs:      []string{"aac"},
		MaxAudioChannels: 8,
	}
	editions := []store.Edition{
		{ID: "aac", Files: []store.File{audioOnlyFile("aac", "aac", 200_000)}},
	}
	dec, unsup := SelectEdition(profile, Constraints{MaxBitrate: 100_000_000}, editions, "")
	if unsup != nil {
		t.Fatalf("unexpected unsupported (want directStream): %v", unsup)
	}
	if dec.Tier != TierDirectStream {
		t.Errorf("tier = %q, want directStream (aac is carriable in MPEG-TS)", dec.Tier)
	}
	if !dec.AudioOnly {
		t.Errorf("AudioOnly = false, want true")
	}
}

// TestSelectEditionVideoWithFlacAudioTranscodes: the copy-safe audio gate also
// applies to a VIDEO File's audio track — an mkv with h264 video (remux-able) but
// FLAC audio (NOT carriable in MPEG-TS) must transcode, not remux, so the copied
// FLAC never reaches an unplayable private-data stream.
func TestSelectEditionVideoWithFlacAudioTranscodes(t *testing.T) {
	profile := DeviceProfile{
		Containers:       []string{"mp4"}, // mkv container unsupported → remux candidate
		VideoCodecs:      []VideoCodecSupport{{Codec: "h264", MaxResolution: "1080p"}},
		AudioCodecs:      []string{"h264", "flac", "aac"},
		MaxAudioChannels: 8,
	}
	editions := []store.Edition{
		{ID: "mkv", Files: []store.File{mkvFile("h264", "flac", 1080, 6_000_000)}},
	}
	dec, unsup := SelectEdition(profile, Constraints{MaxBitrate: 100_000_000}, editions, "")
	if unsup != nil {
		t.Fatalf("want transcode Decision, got Unsupported: %v", unsup)
	}
	if dec.Tier != TierTranscode {
		t.Errorf("tier = %q, want transcode (FLAC audio cannot be remuxed into MPEG-TS)", dec.Tier)
	}
}

// TestSelectEditionMusicWithCoverArtIsAudioOnly is the regression for the
// cover-art-MP3 playback hang: an MP3 Track with embedded album art (ffprobe
// reports the artwork as a png "video" Stream) must negotiate as an AUDIO-ONLY
// File. The cover art is not real video; if it were treated as a video stream the
// Track would skip the audio-only path and the transcoder would try to encode a
// single still as h264 → a broken HLS the player hangs on. mp3 is not HLS-remuxable
// (Safari native HLS), so it transcodes to an audio-only AAC rendition.
func TestSelectEditionMusicWithCoverArtIsAudioOnly(t *testing.T) {
	profile := DeviceProfile{
		Containers:       []string{"mp4"}, // not mp3
		VideoCodecs:      []VideoCodecSupport{{Codec: "h264", MaxResolution: "1080p"}},
		AudioCodecs:      []string{"aac"}, // mp3 not advertised → must transcode
		MaxAudioChannels: 8,
	}
	f := store.File{
		ID: "fa", Path: "/music/Audioslave/02 Show Me How To Live.mp3", Container: "mp3",
		AudioCodec: "mp3", Bitrate: 256_000, Present: true,
		Streams: []store.Stream{
			{Kind: "audio", Codec: "mp3", Channels: 2, IsDefault: true},
			{Kind: "video", Codec: "png", Width: 600, Height: 600}, // embedded cover art
		},
	}
	editions := []store.Edition{{ID: "e1", Files: []store.File{f}}}
	dec, unsup := SelectEdition(profile, Constraints{MaxBitrate: 100_000_000}, editions, "")
	if unsup != nil {
		t.Fatalf("want transcode Decision, got Unsupported: %v", unsup)
	}
	if dec.Tier != TierTranscode {
		t.Errorf("tier = %q, want transcode", dec.Tier)
	}
	if !dec.AudioOnly {
		t.Errorf("AudioOnly = false, want true (the cover-art png stream must be ignored)")
	}
	if dec.VideoStream.Codec != "" {
		t.Errorf("VideoStream codec = %q, want empty (cover art must not be selected as video)", dec.VideoStream.Codec)
	}
}

// TestPickVideoStreamPrefersRealVideoOverCoverArt: a File with BOTH cover art and a
// real video stream must still pick the real video (the cover-art skip must not
// hide genuine video, e.g. a movie whose container also embeds a poster image).
func TestPickVideoStreamPrefersRealVideoOverCoverArt(t *testing.T) {
	f := store.File{
		Streams: []store.Stream{
			{Kind: "video", Codec: "mjpeg", Width: 600, Height: 600}, // cover art first
			{Kind: "audio", Codec: "aac", Channels: 2},
			{Kind: "video", Codec: "h264", Width: 1920, Height: 1080}, // real video
		},
	}
	v, ok := pickVideoStream(f)
	if !ok {
		t.Fatal("pickVideoStream found no video, want the real h264 stream")
	}
	if v.Codec != "h264" {
		t.Errorf("picked %q, want h264 (real video, not the cover-art mjpeg)", v.Codec)
	}
}

// TestSelectEditionTranscodeOnCodecReason: an mkv File whose container AND video
// codec the profile both reject is NOT remux-able (a remux cannot fix the codec),
// so SelectEdition falls back to the transcode tier — a Decision the re-encoding
// HLS job satisfies (no longer an Unsupported error). A directStream Decision
// must NOT be produced when a non-container reason also blocks.
func TestSelectEditionTranscodeOnCodecReason(t *testing.T) {
	// Profile lacks the mkv container AND the mpeg4 video codec.
	profile := DeviceProfile{
		Containers:       []string{"mp4"},
		VideoCodecs:      []VideoCodecSupport{{Codec: "h264", MaxResolution: "1080p"}},
		AudioCodecs:      []string{"mp3", "aac"},
		MaxAudioChannels: 8,
	}
	editions := []store.Edition{
		{ID: "mkv", Files: []store.File{mkvFile("mpeg4", "mp3", 144, 1_000_000)}},
	}
	dec, unsup := SelectEdition(profile, Constraints{MaxBitrate: 100_000_000}, editions, "")
	if unsup != nil {
		t.Fatalf("want transcode Decision, got Unsupported: %v", unsup)
	}
	if dec.Tier != TierTranscode {
		t.Errorf("tier = %q, want transcode (codec mismatch a remux cannot fix)", dec.Tier)
	}
	if dec.Edition.ID != "mkv" {
		t.Errorf("edition = %q, want mkv", dec.Edition.ID)
	}
}

// TestSelectEditionPrefersDirectPlayOverRemux: when one Edition direct-plays and
// another only remuxes, direct play wins (cheapest tier).
func TestSelectEditionPrefersDirectPlayOverRemux(t *testing.T) {
	profile := DeviceProfile{
		Containers:       []string{"mp4"}, // mp4 plays directly; mkv would need remux
		VideoCodecs:      []VideoCodecSupport{{Codec: "h264", MaxResolution: "1080p"}, {Codec: "mpeg4"}},
		AudioCodecs:      []string{"aac", "mp3"},
		MaxAudioChannels: 8,
	}
	editions := []store.Edition{
		{ID: "mkv", Files: []store.File{mkvFile("mpeg4", "mp3", 144, 1_000_000)}},
		{ID: "mp4", Files: []store.File{mp4File(1080, 6_000_000)}},
	}
	dec, unsup := SelectEdition(profile, Constraints{MaxBitrate: 100_000_000}, editions, "")
	if unsup != nil {
		t.Fatalf("unexpected unsupported: %v", unsup)
	}
	if dec.Tier != TierDirectPlay || dec.Edition.ID != "mp4" {
		t.Errorf("tier=%q edition=%q, want directPlay/mp4 (direct play beats remux)", dec.Tier, dec.Edition.ID)
	}
}

func TestSelectEditionPicksHighestPlayable(t *testing.T) {
	editions := []store.Edition{
		{ID: "sd", Name: "480p", Files: []store.File{mp4File(480, 2_000_000)}},
		{ID: "hd", Name: "1080p", Files: []store.File{mp4File(1080, 6_000_000)}},
	}
	dec, unsup := SelectEdition(h264Profile(), Constraints{MaxResolution: "1080p", MaxBitrate: 8_000_000}, editions, "")
	if unsup != nil {
		t.Fatalf("unexpected unsupported: %v", unsup)
	}
	if dec.Edition.ID != "hd" {
		t.Errorf("auto-selected edition = %q, want hd (highest playable)", dec.Edition.ID)
	}
}

func TestSelectEditionFallsToLowerWhenCapped(t *testing.T) {
	editions := []store.Edition{
		{ID: "sd", Name: "480p", Files: []store.File{mp4File(480, 2_000_000)}},
		{ID: "hd", Name: "1080p", Files: []store.File{mp4File(1080, 6_000_000)}},
	}
	// A 720p cap makes the 1080p edition unplayable; the 480p edition is selected.
	dec, unsup := SelectEdition(h264Profile(), Constraints{MaxResolution: "720p"}, editions, "")
	if unsup != nil {
		t.Fatalf("unexpected unsupported: %v", unsup)
	}
	if dec.Edition.ID != "sd" {
		t.Errorf("selected edition = %q, want sd (only playable under 720p cap)", dec.Edition.ID)
	}
}

func TestSelectEditionExplicitNoFallback(t *testing.T) {
	editions := []store.Edition{
		{ID: "sd", Name: "480p", Files: []store.File{mp4File(480, 2_000_000)}},
		{ID: "hd", Name: "1080p", Files: []store.File{mp4File(1080, 6_000_000)}},
	}
	// Explicitly asking for the 1080p edition under a 720p cap must NOT silently
	// fall back to the 480p edition — it transcodes the REQUESTED edition down to
	// fit (a resolution mismatch is exactly what the transcode tier handles).
	dec, unsup := SelectEdition(h264Profile(), Constraints{MaxResolution: "720p"}, editions, "hd")
	if unsup != nil {
		t.Fatalf("want transcode Decision for the requested edition, got Unsupported: %v", unsup)
	}
	if dec.Tier != TierTranscode {
		t.Errorf("tier = %q, want transcode (1080p source under a 720p cap)", dec.Tier)
	}
	if dec.Edition.ID != "hd" {
		t.Errorf("edition = %q, want hd (no silent fallback to sd)", dec.Edition.ID)
	}
}

func TestSelectEditionSkipsMissingFiles(t *testing.T) {
	missing := mp4File(1080, 6_000_000)
	missing.Present = false
	editions := []store.Edition{
		{ID: "gone", Files: []store.File{missing}},
		{ID: "sd", Files: []store.File{mp4File(480, 2_000_000)}},
	}
	dec, unsup := SelectEdition(h264Profile(), Constraints{}, editions, "")
	if unsup != nil {
		t.Fatalf("unexpected unsupported: %v", unsup)
	}
	if dec.Edition.ID != "sd" {
		t.Errorf("selected = %q, want sd (missing-file edition skipped)", dec.Edition.ID)
	}
}

func TestPickAudioByPreferredLang(t *testing.T) {
	f := mp4File(1080, 6_000_000)
	f.Streams = []store.Stream{
		{Kind: "video", Codec: "h264", IsDefault: true},
		{Kind: "audio", Codec: "aac", Language: "en", IsDefault: true},
		{Kind: "audio", Codec: "aac", Language: "fr"},
	}
	a, ok := pickAudioStream(f, "fr")
	if !ok || a.Language != "fr" {
		t.Errorf("preferred-lang pick = %+v (ok=%v), want fr", a, ok)
	}
	// No preference → default.
	a, _ = pickAudioStream(f, "")
	if a.Language != "en" {
		t.Errorf("default pick = %q, want en (default stream)", a.Language)
	}
}

func TestNormalizeContainer(t *testing.T) {
	cases := map[string]string{
		"mov,mp4,m4a,3gp,3g2,mj2": "mp4",
		"matroska,webm":           "mkv",
		"mkv":                     "mkv",
		"MP4":                     "mp4",
		"avi":                     "avi",
	}
	for in, want := range cases {
		if got := NormalizeContainer(in); got != want {
			t.Errorf("NormalizeContainer(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBuildSubtitleTracks pins the decision's Subtitle-track assembly: embedded
// Streams are deduped by the observable (kind|language|forced), sidecar rows
// follow, languages normalize to ISO-639-1, and Convertible is set per format
// (text SRT/embedded → yes; an image track or an unconvertible codec → no).
func TestBuildSubtitleTracks(t *testing.T) {
	f := store.File{
		Streams: []store.Stream{
			{ID: "v", Kind: "video", Codec: "h264"},
			{ID: "a", Kind: "audio", Codec: "aac"},
			{ID: "s-en", Kind: "subtitle", Codec: "subrip", Language: "eng"},
			{ID: "s-fr", Kind: "subtitle", Codec: "subrip", Language: "fre", Forced: true},
			// A duplicate English text stream (e.g. a second part) collapses away.
			{ID: "s-en2", Kind: "subtitle", Codec: "subrip", Language: "en"},
			// An embedded image sub is listed but not convertible.
			{ID: "s-pgs", Kind: "subtitle", Codec: "hdmv_pgs_subtitle", Language: "ger"},
		},
	}
	subs := []store.Subtitle{
		{ID: "sc-es", Source: "sidecar", Kind: "text", Codec: "srt", Language: "es", Forced: true},
		{ID: "sc-img", Source: "sidecar", Kind: "image", Codec: "vobsub", Language: "it"},
	}
	got := buildSubtitleTracks(f, subs)

	byID := map[string]SubtitleTrack{}
	for _, tr := range got {
		byID[tr.ID] = tr
	}
	if _, dup := byID["s-en2"]; dup {
		t.Error("duplicate English embedded stream was not deduped")
	}
	if en := byID["s-en"]; en.Language != "en" || en.Kind != "text" || !en.Convertible || en.Source != "embedded" {
		t.Errorf("embedded en = %+v, want en/text/convertible/embedded", en)
	}
	if fr := byID["s-fr"]; !fr.Forced || fr.Language != "fr" {
		t.Errorf("embedded fr = %+v, want forced fr", fr)
	}
	if pgs := byID["s-pgs"]; pgs.Kind != "image" || pgs.Convertible {
		t.Errorf("embedded pgs = %+v, want image/not-convertible", pgs)
	}
	if es := byID["sc-es"]; es.Source != "sidecar" || !es.Forced || !es.Convertible {
		t.Errorf("sidecar es = %+v, want sidecar/forced/convertible", es)
	}
	if img := byID["sc-img"]; img.Kind != "image" || img.Convertible {
		t.Errorf("sidecar image = %+v, want image/not-convertible", img)
	}
}
