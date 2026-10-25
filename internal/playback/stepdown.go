package playback

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/goozakdev/obelo-server/internal/access"
	"github.com/goozakdev/obelo-server/internal/store"
	"github.com/goozakdev/obelo-server/internal/transcode"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The Step-down ladder (ADR-0071, CONTEXT.md): every successful playback decision
// lists every rung this client may play, best first, with the one it delivers marked
// current, so a client that must step down (the network cannot hold the stream, a codec
// fails) or up has somewhere to go. A rung is only a choice to send back plus a label —
// the Server keeps no ladder state, and the client decides when and how far to step.
//
// The ladder is derived, never predicted. Each candidate request — the one with no
// editionId and no constraints, each Edition by its editionId, each fixed rung's
// constraints — is clamped to the User's Playback ceiling exactly as a negotiation
// clamps it and run through the real planner (SelectEdition, then the ffmpeg plan), and
// the rung is what that request delivers: its label, its bitrate, its height, and its
// identity, the stream itself. Candidates that deliver the same stream are one rung,
// listed under the first of them in the order {}, an Edition alone, the fixed rung of
// the lowest constraints. A candidate the planner refuses (or needs ffmpeg for, when
// there is none) is not a rung, so requesting any rung offered cannot come back
// Unsupported. Rungs are ordered by picture height, then bitrate, a rung whose value is
// unknown below the known ones of its height. Requesting a rung means sending its
// editionId and constraints and nothing of the client's own; the rung that request names
// is the current one.

// StepRung is one rung of the Step-down ladder: a choice a client may send back to play
// at lower quality, plus the label to show for it.
type StepRung struct {
	Label string
	// Kind is StepKindEdition or StepKindTranscode.
	Kind string
	// EditionID is set for an Edition rung: the client sends it as its editionId.
	EditionID string
	// MaxResolution and MaxBitrate are set for a Transcode rung: the client sends them
	// as its constraints. Zero values are absent.
	MaxResolution string
	MaxBitrate    int64
	// Bitrate is the rung's estimated delivered bitrate in bits/sec: an Edition's,
	// a Transcode rung's target, a music original's. 0 is unknown.
	Bitrate int64
	// Height is the delivered video height in pixels: an encode's output, a copy's or
	// remux's selected stream, direct play's video. 0 is unknown or no video (music).
	// Never a cap.
	Height int
	// Current marks the one rung that describes what the decision actually delivers.
	Current bool
}

const (
	StepKindEdition   = "edition"
	StepKindTranscode = "transcode"
)

// transcodeLadder is the constraints of the fixed H.264 Transcode rungs, highest first.
var transcodeLadder = []struct {
	resolution string
	bitrate    int64
}{
	{"1080p", 8_000_000},
	{"720p", 4_000_000},
	{"480p", 1_500_000},
}

// musicLadder is the bitrate caps of the fixed AAC rungs for an audio-only play, highest first.
var musicLadder = []struct {
	bitrate int64
}{
	{256_000},
	{128_000},
}

// Audio encode bitrates. What ffmpeg's AAC encoder produces with no -b:a depends on the
// channel layout. Measured with ffmpeg 9.0.1 (aac, white noise, 20 s per layout, the
// stream bit_rate ffprobe reports):
//
//	mono 69.2k   stereo 128.2k   2.1 144.2k   3.0 197.3k
//	4.0 266.1k   5.0 324.8k       5.1 341.2k   7.1 469.1k
//
// The count alone cannot tell 2.1 from 3.0, so 3 channels reads as the lower. A count
// with no measured layout (7, or over 8) reads as the nearest measured count below it. A
// cap at or under audioEncodeBitrateMax is always passed to the encoder as -b:a; a
// larger one only when the channel default would go over it.
const audioEncodeBitrateMax = 320_000

var audioEncodeDefaults = [...]int64{0, 69_000, 128_000, 144_000, 266_000, 325_000, 341_000, 341_000, 469_000}

// audioEncodeDefaultFor is the bitrate an AAC encode of channels channels comes out at
// with no -b:a. An unknown count reads as stereo.
func audioEncodeDefaultFor(channels int) int64 {
	switch {
	case channels <= 0:
		channels = 2
	case channels >= len(audioEncodeDefaults):
		channels = len(audioEncodeDefaults) - 1
	}
	return audioEncodeDefaults[channels]
}

// audioEncodeUncapped is the -b:a an audio-only AAC transcode with no bitrate cap gets
// before the source's own bitrate is considered: 256 kbps (the top music rung, ADR-0071),
// held to ffmpeg's own default for the layout where that is lower (mono, 3 channels).
// Stereo, and an unknown count, take the 256 kbps rung whole: its default of 128 kbps is
// the next rung down.
func audioEncodeUncapped(channels int) int64 {
	if channels <= 0 || channels == 2 {
		return musicLadder[0].bitrate
	}
	return min(musicLadder[0].bitrate, audioEncodeDefaultFor(channels))
}

// audioScaleCap is whether a bitrate cap is one an audio stream is held to (a music
// rung's, up to 320 kbps) rather than a video-scale one.
func audioScaleCap(cap int64) bool { return cap > 0 && cap <= 320_000 }

// audioEncodeCap is the -b:a an AAC encode of channels channels needs for a bitrate cap,
// 0 when the encoder's own default is already under it (or there is no cap).
func audioEncodeCap(cap int64, channels int) int64 {
	if cap > 0 && (cap <= audioEncodeBitrateMax || audioEncodeDefaultFor(channels) > cap) {
		return cap
	}
	return 0
}

// audioEncodeTarget is the -b:a an audio-only AAC transcode carries: the cap when one
// binds the encoder, else the uncapped target, never above the source's own bitrate
// (when known). The ladder's rung and the ffmpeg args both read it, so they agree.
func audioEncodeTarget(cap int64, channels int, source int64) int64 {
	enc := audioEncodeCap(cap, channels)
	if enc == 0 {
		enc = audioEncodeUncapped(channels)
	}
	if source > 0 && source < enc {
		return source
	}
	return enc
}

// delivery is the stream a request delivers, which is what makes two candidates the same
// rung: the tier and the File (or Online variant), whether the video is copied or
// re-encoded and to what height at what -b:v, and whether the audio is copied or
// encoded at what -b:a. Estimated bitrates never decide sameness.
type delivery struct {
	tier         Tier
	source       string
	videoCopy    bool
	videoStream  string // the video stream copied or remuxed (not an encode's)
	audioStream  string // the audio stream copied or remuxed
	height       int
	videoBitrate int64
	audioCopy    bool
	audioBitrate int64
}

// requestKey is a request a client may send for a rung: an editionId and the
// constraints it carries.
type requestKey struct {
	editionID  string
	resolution string
	bitrate    int64
}

// ladderCandidate is one request the ladder tries on the planner. bare is the request
// with no editionId and no constraints.
type ladderCandidate struct {
	requestKey
	bare bool
}

func (c ladderCandidate) constraints() Constraints {
	return Constraints{MaxResolution: c.resolution, MaxBitrate: c.bitrate}
}

// ladderRung is a rung with the quality it stands for, so rungs from different requests
// sort into one list. A zero height or bitrate is unknown.
type ladderRung struct {
	StepRung
	height int
	// unconstrained marks the rung a request with no editionId and no constraints gets.
	// Within its height it outranks every bounded rung: a constrained rung can never be
	// better than the unconstrained one.
	unconstrained bool
	// codec is the Edition's video (or, for music, audio) codec, kept to tell apart two
	// rungs that would otherwise carry the same label.
	codec string
	// id is the stream the rung delivers; edition is the Edition it plays untouched, ""
	// for a Transcode.
	id      delivery
	edition string
	// requests are the candidate requests that deliver this rung's stream, the one it is
	// listed under first.
	requests []requestKey
}

// higher says whether a is a better rung than b: taller, then the unconstrained rung,
// then higher bitrate, a rung whose value is unknown below any known one of the same
// height.
func (a ladderRung) higher(b ladderRung) bool {
	if a.height != b.height {
		return a.height > b.height
	}
	if a.unconstrained != b.unconstrained {
		return a.unconstrained
	}
	return a.Bitrate > b.Bitrate
}

// asks says whether a request carrying editionID and c is a request for this rung: its
// editionId and constraints equal the rung's, or it carries neither and this is the rung
// the Server picks unasked. (The relay, which has only the rung as it travels, asks this.)
func (r ladderRung) asks(editionID string, c Constraints) bool {
	if editionID == "" && c.MaxResolution == "" && c.MaxBitrate == 0 && r.unconstrained {
		return true
	}
	return r.EditionID == editionID && r.MaxResolution == c.MaxResolution && r.MaxBitrate == c.MaxBitrate
}

func (r ladderRung) requested(k requestKey) bool {
	for _, q := range r.requests {
		if q == k {
			return true
		}
	}
	return false
}

// audioEncode says whether the rung is an audio-only AAC encode.
func (r ladderRung) audioEncode() bool {
	return r.id.tier == TierTranscode && !r.id.audioCopy && r.id.audioBitrate > 0
}

// sortRungs puts rungs in ladder order, keeping the order of equals.
func sortRungs(rungs []ladderRung) {
	sort.SliceStable(rungs, func(i, j int) bool { return rungs[i].higher(rungs[j]) })
}

func plainRungs(rungs []ladderRung) []StepRung {
	uniqueLabels(rungs)
	out := make([]StepRung, 0, len(rungs))
	for _, r := range rungs {
		r.StepRung.Height = r.height
		out = append(out, r.StepRung)
	}
	return out
}

// addCandidate lists candidate cd, which delivers r, as a rung of its own or, when an
// earlier candidate delivers the same stream, as another request for that rung. The
// first candidate of a stream gives the rung its wire form: its own editionId and its
// constraints as the ceiling clamped them (cl; the same when it binds nothing), so a
// rung is never listed above the ceiling; the request with neither names the Edition it
// plays untouched, if it plays one.
func addCandidate(rungs []ladderRung, cd ladderCandidate, cl Constraints, r ladderRung) []ladderRung {
	for i := range rungs {
		if rungs[i].id == r.id {
			rungs[i].requests = append(rungs[i].requests, cd.requestKey)
			rungs[i].unconstrained = rungs[i].unconstrained || cd.bare
			return rungs
		}
	}
	r.requests = []requestKey{cd.requestKey}
	r.unconstrained = cd.bare
	if cd.bare {
		r.EditionID = r.edition
	} else {
		r.EditionID = cd.editionID
		if cd.resolution != "" || cd.bitrate != 0 {
			r.MaxResolution, r.MaxBitrate = cl.MaxResolution, cl.MaxBitrate
			r.requests = append(r.requests, requestKey{r.EditionID, r.MaxResolution, r.MaxBitrate})
		}
	}
	return append(rungs, r)
}

// markCurrent marks the rung the request asked (its editionId and constraints, as the
// client sent them) current, else the rung whose stream the decision actually delivers,
// else inserts a rung for what is delivered. actual describes the delivery.
func markCurrent(rungs []ladderRung, asked requestKey, actual func() (ladderRung, bool)) []ladderRung {
	for i := range rungs {
		if rungs[i].requested(asked) {
			rungs[i].Current = true
			return rungs
		}
	}
	a, ok := actual()
	if !ok {
		return rungs
	}
	for i := range rungs {
		if rungs[i].id == a.id {
			rungs[i].Current = true
			return rungs
		}
	}
	// The inserted rung is requested as the client asked, so asking again reproduces the
	// delivery and the request is never another rung's.
	a.Current = true
	a.EditionID, a.MaxResolution, a.MaxBitrate = asked.editionID, asked.resolution, asked.bitrate
	a.requests = []requestKey{{a.EditionID, a.MaxResolution, a.MaxBitrate}}
	return append(rungs, a)
}

// deliveryOf is the stream dec delivers under constraints c (already clamped to the
// ceiling), read from the same plan the ffmpeg args are built from, with its picture
// height and honest bitrate: the -b:v (an encode), the -b:a (an audio encode), else the
// File's own. 0 is unknown.
func deliveryOf(profile DeviceProfile, c Constraints, dec Decision) (id delivery, height int, bitrate int64) {
	// An audio-only delivery has no picture: the File's height is its cover art's.
	if !dec.AudioOnly {
		height = dec.VideoStream.Height
		if height == 0 {
			height = dec.File.Height
		}
		if dec.Tier == TierDirectPlay {
			height, _ = defaultVideo(dec.File)
		}
	}
	id = delivery{tier: dec.Tier, source: dec.File.ID}
	bitrate = dec.File.Bitrate
	if dec.Tier != TierTranscode {
		// Direct play opens the File and plays its default tracks whatever the Decision
		// records, so it is identified by the File alone; a remux names its tracks.
		if dec.Tier != TierDirectPlay {
			id.videoStream, id.audioStream, id.height = dec.VideoStream.ID, dec.AudioStream.ID, height
		}
		return id, height, bitrate
	}
	job := transcodeJobPlan(profile, c, dec)
	id.audioCopy, id.audioBitrate = job.Audio.Copy, job.Audio.MaxBitrate
	if job.Audio.Copy {
		id.audioStream = dec.AudioStream.ID
	}
	if dec.AudioOnly {
		if !job.Audio.Copy && job.Audio.MaxBitrate > 0 {
			bitrate = job.Audio.MaxBitrate
		}
		return id, 0, bitrate
	}
	if job.Video.Copy {
		id.videoCopy, id.videoStream, id.height = true, dec.VideoStream.ID, height
		return id, height, bitrate
	}
	if m := job.Video.MaxHeight; m > 0 && (height == 0 || height > m) {
		height = m
	}
	// An encode with no -b:v has no bitrate we know; the File's is not it.
	id.height, id.videoBitrate = height, job.Video.MaxBitrate
	return id, height, job.Video.MaxBitrate
}

// defaultVideo is the height and codec of the File's default video track, the one direct
// play opens: the File's own fields when it lists no usable video stream.
func defaultVideo(f store.File) (height int, codec string) {
	if v, ok := pickVideoStream(f); ok {
		height = v.Height
		if height == 0 {
			height = f.Height
		}
		return height, firstNonEmpty(v.Codec, f.VideoCodec)
	}
	return f.Height, f.VideoCodec
}

// describeRung is the rung dec stands for under c: the Edition it plays untouched, or the
// Transcode named for what it delivers.
func describeRung(profile DeviceProfile, c Constraints, dec Decision, n rungNaming) ladderRung {
	id, height, bitrate := deliveryOf(profile, c, dec)
	r := ladderRung{
		StepRung: StepRung{Kind: StepKindTranscode, Bitrate: bitrate},
		height:   height,
		codec:    n.codec(dec.File, deliveredCodec(profile, c, dec)),
		id:       id,
	}
	switch {
	case dec.Tier != TierTranscode:
		// Named for the video stream delivered: a picked lower cut is not the File's top height.
		f := dec.File
		if height > 0 {
			f.Height = height
		}
		r.Kind, r.Label, r.edition = StepKindEdition, n.label(dec.Edition, f), dec.Edition.ID
	case dec.AudioOnly:
		r.Label = aacLabel(bitrate)
	default:
		r.Label = deliveredLabel(height, bitrate)
	}
	return r
}

// deliveredCodec is the codec dec delivers: the encoder's output (from the job plan) for an
// encode, the selected stream's for a copy or remux, the File's default track for direct play.
// For music it is the audio codec: "aac" for an encode.
func deliveredCodec(profile DeviceProfile, c Constraints, dec Decision) string {
	switch {
	case dec.AudioOnly && dec.Tier == TierTranscode:
		if transcodeJobPlan(profile, c, dec).Audio.Copy {
			return firstNonEmpty(dec.AudioStream.Codec, dec.File.AudioCodec)
		}
		return "aac"
	case dec.AudioOnly:
		return firstNonEmpty(dec.AudioStream.Codec, dec.File.AudioCodec)
	case dec.Tier == TierTranscode:
		return firstNonEmpty(transcodeJobPlan(profile, c, dec).Video.Codec, firstNonEmpty(dec.VideoStream.Codec, dec.File.VideoCodec))
	case dec.Tier == TierDirectStream:
		return firstNonEmpty(dec.VideoStream.Codec, dec.File.VideoCodec)
	}
	_, codec := defaultVideo(dec.File)
	return codec
}

// rungNaming is how a ladder names its Edition rungs: the label to show and the codec that
// tells two of the same label apart (given the codec the rung delivers).
type rungNaming struct {
	label func(store.Edition, store.File) string
	codec func(f store.File, video string) string
}

var (
	videoNaming = rungNaming{editionLabel, func(_ store.File, video string) string { return video }}
	audioNaming = rungNaming{audioEditionLabel, func(_ store.File, audio string) string { return audio }}
)

// stepDownFor computes the ladder for a local Decision: every rung this client may
// play, best first, with the one the Decision delivers marked Current. The rungs are
// bounded by the Capability profile and the User's Playback ceiling only — never by the
// request's own maxBitrate, maxResolution or editionId, which pick the current rung,
// not what is on offer. req.Scope is the ceiling; asked is the request's own
// Constraints as sent (req.Constraints is already clamped to the ceiling and is what
// the Decision was made under).
func (s *Service) stepDownFor(req Request, asked Constraints, detail store.TitleDetail, dec Decision, mem *rememberedPicks) []StepRung {
	if dec.AudioOnly {
		return plainRungs(s.deriveRungs(req, asked, detail, dec, mem, audioNaming, musicCandidates(detail), true))
	}
	return plainRungs(s.deriveRungs(req, asked, detail, dec, mem, videoNaming, videoCandidates(detail), false))
}

// videoCandidates are the requests a video ladder tries: none, each Edition alone, each
// fixed Transcode rung's constraints (lowest first: of several fixed rungs delivering
// one stream the lowest is kept).
func videoCandidates(detail store.TitleDetail) []ladderCandidate {
	cands := []ladderCandidate{{bare: true}}
	for _, ed := range detail.Editions {
		cands = append(cands, ladderCandidate{requestKey: requestKey{editionID: ed.ID}})
	}
	for i := len(transcodeLadder) - 1; i >= 0; i-- {
		r := transcodeLadder[i]
		cands = append(cands, ladderCandidate{requestKey: requestKey{resolution: r.resolution, bitrate: r.bitrate}})
	}
	return cands
}

// musicCandidates are the requests a music ladder tries: none, each Edition alone, the
// fixed AAC rungs' bitrate caps (lowest first).
func musicCandidates(detail store.TitleDetail) []ladderCandidate {
	cands := []ladderCandidate{{bare: true}}
	for _, ed := range detail.Editions {
		cands = append(cands, ladderCandidate{requestKey: requestKey{editionID: ed.ID}})
	}
	for i := len(musicLadder) - 1; i >= 0; i-- {
		cands = append(cands, ladderCandidate{requestKey: requestKey{bitrate: musicLadder[i].bitrate}})
	}
	return cands
}

// deriveRungs runs every candidate through the planner and returns the ladder, sorted, with
// the current rung marked. A music ladder (music) never lists a fixed AAC rung at or above
// the bitrate of the unconstrained encode: the unconstrained one is the best of them. A lossy Edition rung at or above it (both bitrates known) is not listed either.
func (s *Service) deriveRungs(req Request, asked Constraints, detail store.TitleDetail, dec Decision, mem *rememberedPicks, n rungNaming, cands []ladderCandidate, music bool) []ladderRung {
	plan := func(cd ladderCandidate) (Decision, Constraints, bool) {
		c := cd.constraints()
		c.PreferredAudioLang = req.Constraints.PreferredAudioLang
		cl, _ := clampToCeiling(c, req.Scope)
		d, unsup := SelectEdition(req.Profile, cl, detail.Editions, cd.editionID)
		if unsup != nil {
			return Decision{}, cl, false
		}
		d.Subtitles = buildSubtitleTracks(d.File, detail.Subtitles)
		// The request's own stream picks apply to the candidate as they would to a
		// request for it (an audio pick pins its File whatever the editionId says).
		cr := req
		cr.Constraints, cr.EditionID = cl, cd.editionID
		d, err := s.applyPicks(cr, detail, d, mem)
		if err != nil || (d.Tier == TierTranscode && !s.canTranscode) {
			return Decision{}, cl, false
		}
		// An Edition alone is a rung only while it does not re-encode the video: Direct
		// play, Direct stream or a video copy (ADR-0071 decision 1).
		if cd.editionID != "" && d.Tier == TierTranscode && !d.VideoCopy {
			return Decision{}, cl, false
		}
		// A fixed rung's constraints say what it delivers: no taller than the device
		// decodes the codec it plans (an encode is h264).
		if cd.resolution != "" && d.Tier == TierTranscode && !d.AudioOnly {
			codec := transcode.VideoCodecH264
			if d.VideoCopy {
				codec = firstNonEmpty(d.VideoStream.Codec, d.File.VideoCodec)
			}
			if sup, ok := req.Profile.videoCodec(codec); ok {
				if dev := resolutionHeight(sup.MaxResolution); dev > 0 && (resolutionHeight(cl.MaxResolution) == 0 || dev < resolutionHeight(cl.MaxResolution)) {
					cl.MaxResolution = sup.MaxResolution
				}
			}
		}
		return d, cl, true
	}
	var rungs []ladderRung
	var top *ladderRung
	for _, cd := range cands {
		d, cl, ok := plan(cd)
		if !ok {
			continue
		}
		r := describeRung(req.Profile, cl, d, n)
		if cd.bare {
			top = &r
		}
		if music && !cd.bare && cd.editionID == "" && top != nil && top.audioEncode() && r.audioEncode() &&
			r.id != top.id && r.id.audioBitrate >= top.id.audioBitrate {
			continue
		}
		// The same floor for a rung that is not an encode (a lossy Edition): with both
		// bitrates known, a step down never raises the bitrate.
		if music && !cd.bare && top != nil && !r.audioEncode() && r.id != top.id &&
			top.Bitrate > 0 && r.Bitrate > 0 && r.Bitrate >= top.Bitrate {
			continue
		}
		rungs = addCandidate(rungs, cd, cl, r)
		if music && !cd.bare && cd.editionID == "" {
			capMusicEdition(rungs, cd, r.id)
		}
	}
	if music {
		capMusicEdition(rungs, ladderCandidate{}, delivery{})
	}
	rungs = markCurrent(rungs, requestKey{req.EditionID, asked.MaxResolution, asked.MaxBitrate}, func() (ladderRung, bool) {
		return describeRung(req.Profile, req.Constraints, dec, n), true
	})
	sortRungs(rungs)
	return rungs
}

// capMusicEdition gives an Edition rung of a music ladder a maxBitrate, so that it means the
// same on any Track (the editionId names it only within this one): the cap of the fixed AAC
// request cd that delivers it (the lowest, as they come lowest first), else, with cd the
// zero candidate, its own delivered bitrate. The unconstrained rung carries none. The
// request with the cap is another way of asking for the rung.
func capMusicEdition(rungs []ladderRung, cd ladderCandidate, id delivery) {
	for i := range rungs {
		g := &rungs[i]
		if (id != delivery{} && g.id != id) || g.unconstrained || g.EditionID == "" || g.MaxBitrate != 0 {
			continue
		}
		c := cd.bitrate
		if cd == (ladderCandidate{}) {
			c = g.Bitrate
		}
		if c <= 0 {
			continue
		}
		g.MaxBitrate = c
		g.requests = append(g.requests, requestKey{g.EditionID, "", c})
	}
}

// uniqueLabels makes every label in the ladder different: two rungs that share one are
// told apart by codec, then by bitrate, and a pair still alike (the same Edition twice
// over) by a count. A client keys its rungs on kind and label, so a repeat would hide
// one of them.
func uniqueLabels(rungs []ladderRung) {
	base := make([]string, len(rungs))
	parts := make([][]string, len(rungs))
	label := func(i int) string {
		if len(parts[i]) == 0 {
			return base[i]
		}
		return base[i] + " (" + strings.Join(parts[i], ", ") + ")"
	}
	for i, r := range rungs {
		base[i] = r.Label
	}
	for _, by := range []func(ladderRung) string{
		func(r ladderRung) string { return r.codec },
		func(r ladderRung) string {
			if r.Bitrate <= 0 {
				return ""
			}
			return bitrateText(r.Bitrate)
		},
	} {
		groups := map[string][]int{}
		for i := range rungs {
			groups[label(i)] = append(groups[label(i)], i)
		}
		for _, idx := range groups {
			vals := map[string]bool{}
			for _, i := range idx {
				vals[by(rungs[i])] = true
			}
			if len(idx) < 2 || len(vals) < 2 {
				continue
			}
			for _, i := range idx {
				if v := by(rungs[i]); v != "" {
					parts[i] = append(parts[i], v)
				}
			}
		}
	}
	seen := map[string]bool{}
	for i := range rungs {
		l := label(i)
		for n := 2; seen[l]; n++ {
			l = label(i) + " (" + strconv.Itoa(n) + ")"
		}
		seen[l] = true
		rungs[i].Label = l
	}
}

// bitrateText is a bitrate as a client shows it: "8 Mbps", "320 kbps".
func bitrateText(bitrate int64) string {
	if bitrate >= 1_000_000 {
		return strconv.FormatFloat(float64(bitrate)/1e6, 'f', -1, 64) + " Mbps"
	}
	return strconv.FormatFloat(float64(bitrate)/1e3, 'f', -1, 64) + " kbps"
}

// deliveredLabel names a rung by what it delivers: its picture height and bitrate,
// whichever are known.
func deliveredLabel(height int, bitrate int64) string {
	label := ""
	if height > 0 {
		label = strconv.Itoa(height) + "p"
	}
	if bitrate > 0 {
		if label != "" {
			label += " "
		}
		label += strconv.FormatFloat(float64(bitrate)/1e6, 'f', -1, 64) + " Mbps"
	}
	if label == "" {
		return "Transcode"
	}
	return label
}

// resolutionName matches an Edition Name that only says a resolution, the forms the scanner
// names Editions by (internal/scanner/filename.go editionName).
var resolutionName = regexp.MustCompile(`(?i)^(\d{3,4}p|4k|uhd|sd)$`)

// editionLabel is the delivered height, behind the Edition's name unless that name is just a
// resolution (which the height replaces: the name is the Edition's top, not what plays).
func editionLabel(ed store.Edition, f store.File) string {
	name := strings.TrimSpace(ed.Name)
	height := ""
	if f.Height > 0 {
		height = strconv.Itoa(f.Height) + "p"
	}
	switch {
	case name == "" && height == "":
		return "Original"
	case name == "" || (height != "" && resolutionName.MatchString(name)):
		return height
	case height == "":
		return name
	}
	return name + " · " + height
}

// audioEditionLabel is the Edition's trimmed name, else "Original": the untouched audio.
func audioEditionLabel(ed store.Edition, _ store.File) string {
	if name := strings.TrimSpace(ed.Name); name != "" {
		return name
	}
	return "Original"
}

// aacLabel names an AAC rung by its bitrate, "AAC" alone when that is unknown.
func aacLabel(bitrate int64) string {
	if bitrate <= 0 {
		return "AAC"
	}
	return "AAC " + strconv.FormatInt(bitrate/1000, 10) + " kbps"
}

// relayStepDown makes the sharer's ladder (the []any its decision JSON decoded to)
// this Server's own: each Edition rung's editionId is rewritten to the local id the
// same way the decision's Edition is (a rung the mirror has no Edition for cannot be
// requested here and is dropped, except the current one, which stays with its editionId
// removed), then every rung above the home User's Playback ceiling is dropped (again
// except the one the sharer is playing: what is playing is always listed). A rung with
// neither editionId nor constraints is judged by its bitrate (a bitrate over the ceiling,
// or unknown under a bitrate ceiling, is over it) and by the height its label names.
//
// The home Server then names the rung a request with no editionId and no constraints
// gets here: what the sharer plays under the ceiling, which is the Edition it is
// playing when that was the request, else the best Edition the ceiling leaves. When the
// ceiling leaves none, the sharer's top rung stands in for it (its editionId, no
// constraints), described as what it delivers under the ceiling (a height and a bitrate
// never above it); the sharer's own name for that delivery, a transcode rung whose
// constraints the ceiling clamps to its own, is absorbed into it. Rungs are one rung when
// the requests this Server would send for them, clamped to the ceiling, are equal. The
// current rung is the one the request (editionID and asked, as the client sent them,
// before the ceiling clamped them) names, by the same equality the sharer applies; the
// sharer's own current flag is only the fallback. Always a non-nil list; a malformed
// entry is dropped.
func (s *Service) relayStepDown(raw any, detail store.TitleDetail, scope access.Scope, editionID string, asked Constraints) []any {
	type kept struct {
		m       map[string]any
		current bool // the sharer's own flag
		over    bool // above the ceiling, kept only for being current
	}
	var rungs []kept
	entries, _ := raw.([]any)
	ceilH, ceilBR := resolutionHeight(scope.MaxResolution), scope.MaxBitrate
	ceilKey, _ := clampToCeiling(Constraints{}, scope)
	isEdition := func(m map[string]any) bool { k, _ := m["kind"].(string); return k == StepKindEdition }
	constraintsOf := func(m map[string]any) (Constraints, bool) {
		cc, ok := m["constraints"].(map[string]any)
		if !ok {
			return Constraints{}, false
		}
		res, _ := cc["maxResolution"].(string)
		return Constraints{MaxResolution: res, MaxBitrate: jsonInt(cc["maxBitrate"])}, true
	}
	isBare := func(m map[string]any) bool {
		id, _ := m["editionId"].(string)
		_, has := m["constraints"]
		return id == "" && !has
	}
	// twinOf says whether m is a Transcode rung with no editionId whose constraints this
	// ceiling clamps to the ceiling's own, so that a request for it and a request for
	// nothing are one request here; exact says they are already equal.
	twinOf := func(m map[string]any) (exact, ok bool) {
		if id, _ := m["editionId"].(string); id != "" || isEdition(m) {
			return false, false
		}
		c, has := constraintsOf(m)
		if !has {
			return false, false
		}
		// A rung that states no bitrate has no -b:v, whatever bitrate cap it carries.
		cl, _ := clampToCeiling(c, scope)
		if cl.MaxResolution != ceilKey.MaxResolution || (cl.MaxBitrate != ceilKey.MaxBitrate && jsonInt(m["bitrate"]) != 0) {
			return false, false
		}
		return c.MaxResolution == ceilKey.MaxResolution && c.MaxBitrate == ceilKey.MaxBitrate, true
	}
	var top map[string]any // the sharer's top rung, when the ceiling dropped it
	topHeight := 0
	var twins []map[string]any // twins the ceiling dropped
	for n, e := range entries {
		src, ok := e.(map[string]any)
		if !ok {
			continue
		}
		rung := make(map[string]any, len(src))
		for k, v := range src {
			rung[k] = v
		}
		var height int
		var bitrate int64
		bare := isBare(rung)
		current, _ := rung["current"].(bool)
		if id, _ := rung["editionId"].(string); id != "" {
			local := s.relayLocalEdition(id)
			ed, ok := localEditionFor(detail, local)
			switch {
			case ok:
				rung["editionId"] = local
				if f, ok := firstPresentFile(ed); ok {
					height, bitrate = f.Height, f.Bitrate
					// A Track's cover art is not a picture: its rung has no height.
					if len(SelectableVideoStreams(f)) == 0 && len(f.Streams) > 0 {
						height = 0
					}
				}
				// The sharer's rung may play a track other than the File's primary one.
				if h := int(jsonInt(rung["height"])); h > 0 {
					height = h
				}
			case current:
				delete(rung, "editionId")
				height = int(jsonInt(rung["height"]))
			default:
				continue
			}
		} else if c, ok := constraintsOf(rung); ok {
			height = resolutionHeight(c.MaxResolution)
			bitrate = c.MaxBitrate
		} else {
			// Neither editionId nor constraints: only the rung's own bitrate says what it
			// delivers, and its label the height.
			label, _ := rung["label"].(string)
			height, bitrate = labelHeight(label), jsonInt(rung["bitrate"])
			if h := int(jsonInt(rung["height"])); h > 0 {
				height = h
			}
		}
		over := (ceilH > 0 && height > ceilH) || (ceilBR > 0 && bitrate > ceilBR) || (bare && ceilBR > 0 && bitrate == 0)
		if over && !current {
			if _, ok := twinOf(rung); ok {
				twins = append(twins, rung)
			}
			if n == 0 {
				top, topHeight = rung, height
			}
			continue
		}
		rungs = append(rungs, kept{m: rung, current: current, over: over})
	}

	// The unconstrained rung.
	unasked := editionID == "" && asked.MaxResolution == "" && asked.MaxBitrate == 0
	unconstrained := func(m map[string]any) bool { return isEdition(m) || isBare(m) }
	u := -1
	if unasked {
		for i, r := range rungs {
			if r.current && unconstrained(r.m) {
				u = i
			}
		}
	}
	for i := 0; u < 0 && i < len(rungs); i++ {
		if !rungs[i].over && unconstrained(rungs[i].m) {
			u = i
		}
	}
	absorbed := false
	if u < 0 && top != nil {
		delete(top, "constraints")
		delete(top, "current")
		height, bitrate := topHeight, jsonInt(top["bitrate"])
		clamped := ceilH > 0 && height > ceilH
		if clamped {
			height = ceilH
		}
		switch {
		case ceilBR > 0 && (bitrate == 0 || bitrate > ceilBR):
			bitrate = ceilBR
		case clamped && ceilBR == 0:
			bitrate = 0
		}
		topLabel := top["label"]
		if height > 0 {
			top["label"] = deliveredLabel(height, bitrate)
		}
		// The sharer's height stands unless the ceiling clamps it; unknown stays unknown.
		if h := jsonInt(top["height"]); h > 0 {
			if ceilH > 0 && h > int64(ceilH) {
				h = int64(ceilH)
			}
			top["height"] = float64(h)
		} else {
			delete(top, "height")
		}
		// The sharer's own name for what it delivers under this ceiling, if it listed
		// one: the twin whose constraints are the ceiling's own, else one that states no
		// bitrate (the ceiling's bitrate changes nothing for it).
		var named map[string]any
		namedExact := false
		consider := func(m map[string]any) {
			exact, ok := twinOf(m)
			if ok && (exact || jsonInt(m["bitrate"]) == 0) && (named == nil || exact && !namedExact) {
				named, namedExact = m, exact
			}
		}
		for _, r := range rungs {
			consider(r.m)
		}
		for _, m := range twins {
			consider(m)
		}
		if named != nil {
			top["label"], bitrate = named["label"], jsonInt(named["bitrate"])
			// It delivers what the sharer says that does, picture included.
			if h := jsonInt(named["height"]); h > 0 {
				top["height"] = float64(h)
			}
		}
		// It is the same Edition rung, so its name stays in front of the height.
		if l, ok := top["label"].(string); ok && isEdition(top) {
			top["label"] = withEditionName(topLabel, l)
		}
		if bitrate > 0 {
			top["bitrate"] = float64(bitrate)
		} else {
			delete(top, "bitrate")
		}
		rungs = append([]kept{{m: top}}, rungs...)
		u = 0
	}

	// Twins of the unconstrained rung are that rung.
	for i := 0; u >= 0 && i < len(rungs); {
		if _, ok := twinOf(rungs[i].m); ok && i != u {
			absorbed = absorbed || rungs[i].current
			rungs = append(rungs[:i], rungs[i+1:]...)
			if i < u {
				u--
			}
			continue
		}
		i++
	}
	// Rungs whose requests are equal once the ceiling has clamped them are one rung.
	seen := map[string]int{}
	for i := 0; i < len(rungs); {
		id, _ := rungs[i].m["editionId"].(string)
		c, _ := constraintsOf(rungs[i].m)
		cl, _ := clampToCeiling(c, scope)
		k := id + "|" + cl.MaxResolution + "|" + strconv.FormatInt(cl.MaxBitrate, 10)
		j, dup := seen[k]
		if !dup {
			seen[k] = i
			i++
			continue
		}
		if i == u {
			rungs[j].m, u = rungs[i].m, j
		}
		rungs[j].current = rungs[j].current || rungs[i].current
		rungs = append(rungs[:i], rungs[i+1:]...)
		if u > i {
			u--
		}
	}

	// The current rung.
	sharerCur, mark := -1, -1
	for i, r := range rungs {
		if r.current {
			sharerCur = i
		}
		delete(r.m, "current")
	}
	for i, r := range rungs {
		id, _ := r.m["editionId"].(string)
		c, _ := constraintsOf(r.m)
		if (ladderRung{StepRung: StepRung{EditionID: id, MaxResolution: c.MaxResolution, MaxBitrate: c.MaxBitrate}, unconstrained: i == u}).asks(editionID, asked) {
			mark = i
			break
		}
	}
	if mark < 0 {
		mark = sharerCur
	}
	if mark < 0 && absorbed {
		mark = u
	}
	out := make([]any, 0, len(rungs))
	for i, r := range rungs {
		if i == mark {
			r.m["current"] = true
		}
		out = append(out, r.m)
	}
	// Labels are unique within a ladder, as for a local one.
	tmp := make([]ladderRung, len(out))
	for i, m := range out {
		tmp[i].Label, _ = m.(map[string]any)["label"].(string)
		tmp[i].Bitrate = jsonInt(m.(map[string]any)["bitrate"])
	}
	uniqueLabels(tmp)
	for i, m := range out {
		m.(map[string]any)["label"] = tmp[i].Label
	}
	return out
}

// editionNamePrefix is the Edition name an Edition rung's label carries in front of its
// height ("Director's Cut" of "Director's Cut · 1080p").
var editionNamePrefix = regexp.MustCompile(`^(.+?) · \d{3,4}p(?:\s|\(|$)`)

// withEditionName puts the Edition name that old (a rung's label) carries in front of label.
func withEditionName(old any, label string) string {
	if l, _ := old.(string); l != "" {
		if m := editionNamePrefix.FindStringSubmatch(l); m != nil {
			return m[1] + " · " + label
		}
	}
	return label
}

// labelHeight is the picture height a rung's label starts with ("1080p 5 Mbps"), 0 when
// it does not start with one.
func labelHeight(label string) int {
	n := 0
	for n < len(label) && label[n] >= '0' && label[n] <= '9' {
		n++
	}
	if n == 0 || n >= len(label) || label[n] != 'p' {
		return 0
	}
	h, _ := strconv.Atoi(label[:n])
	return h
}

// jsonInt reads a number out of decoded JSON, 0 when it is not one.
func jsonInt(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}

// OnlineStepDown is the ladder for an Online item (ADR-0068, ADR-0071): every rung this
// client may play, best first, with the one the play delivers marked current. It is
// derived like a Title's: the candidates are the request with no constraints, whose rung
// is "Original" (kind edition, no editionId, no constraints: the item's best variant,
// held only to the ceiling), and each fixed Transcode rung's constraints; each is clamped
// to the ceiling as the play is and run through PlanOnline, and a candidate that does not
// plan (or needs ffmpeg, absent: canTranscode) is no rung. Candidates that plan the same
// delivery — one variant relayed, or encoded to one height at one -b:v — are one rung. A
// rung's bitrate is its encode's -b:v, absent for a relay (a variant states none). A
// variant that states no height cannot be undercut, so it gets no Transcode rungs.
// ceiling is the User's Playback ceiling alone; asked is what the client sent,
// constraints what this play was made under (already clamped to the ceiling) and plan
// the result. The rung asked is current, else the one whose delivery matches the plan;
// when none does, one with the request's constraints is inserted. Always non-nil.
func OnlineStepDown(profile DeviceProfile, ceiling, asked, constraints Constraints, variants []pluginapi.OnlineVariant, plan OnlinePlan, canTranscode bool) []StepRung {
	scope := access.Scope{MaxResolution: ceiling.MaxResolution, MaxBitrate: ceiling.MaxBitrate}
	rungOf := func(cd ladderCandidate, p OnlinePlan) ladderRung {
		id := delivery{tier: TierDirectPlay, source: p.Variant.URL + "\x00" + p.Variant.VideoURL}
		r := ladderRung{StepRung: StepRung{Kind: StepKindEdition, Label: "Original"}, height: onlineHeight(p)}
		if p.Transcode {
			id.tier, id.height, id.videoBitrate = TierTranscode, r.height, p.MaxBitrate
			if r.height == 0 {
				// A variant that states no height is encoded under the cap, which tells encodes apart.
				id.height = p.MaxHeight
			}
			r.Bitrate = p.MaxBitrate
		}
		if !cd.bare {
			r.Kind, r.Label = StepKindTranscode, deliveredLabel(r.height, r.Bitrate)
		} else if r.height > 0 {
			r.Label = "Original · " + strconv.Itoa(r.height) + "p"
		}
		r.id = id
		return r
	}
	planOf := func(c Constraints) (OnlinePlan, Constraints, bool) {
		c, _ = ClampToCeiling(c, scope)
		p, unsup := PlanOnline(profile, c, variants, canTranscode)
		return p, c, unsup == nil
	}
	var rungs []ladderRung
	cands := []ladderCandidate{{bare: true}}
	if p, _, ok := planOf(Constraints{}); ok && onlineHeight(p) > 0 {
		for i := len(transcodeLadder) - 1; i >= 0; i-- {
			r := transcodeLadder[i]
			cands = append(cands, ladderCandidate{requestKey: requestKey{resolution: r.resolution, bitrate: r.bitrate}})
		}
	}
	for _, cd := range cands {
		if p, cl, ok := planOf(cd.constraints()); ok {
			rungs = addCandidate(rungs, cd, cl, rungOf(cd, p))
		}
	}
	key := requestKey{resolution: asked.MaxResolution, bitrate: asked.MaxBitrate}
	rungs = markCurrent(rungs, key, func() (ladderRung, bool) {
		return rungOf(ladderCandidate{requestKey: key}, plan), true
	})
	sortRungs(rungs)
	return plainRungs(rungs)
}

// onlineHeight is the picture height a plan delivers: the variant's, scaled down to the
// encode's cap. 0 is unknown.
func onlineHeight(plan OnlinePlan) int {
	h := resolutionHeight(plan.Variant.Resolution)
	if plan.Transcode && plan.MaxHeight > 0 && h > plan.MaxHeight {
		h = plan.MaxHeight
	}
	return h
}
