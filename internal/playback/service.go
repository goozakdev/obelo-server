package playback

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/marioquake/juicebox/internal/access"
	"github.com/marioquake/juicebox/internal/audio"
	"github.com/marioquake/juicebox/internal/store"
	"github.com/marioquake/juicebox/internal/transcode"
)

// ErrTitleNotFound means the requested Title does not exist (or, per the
// hide-existence posture, is not visible). The api layer maps it to 404.
var ErrTitleNotFound = errors.New("playback: title not found")

// ErrSessionNotFound means the session id is unknown, ended, reaped, or owned by
// a different User. The api layer maps it to 404 (hide existence), the same as a
// stream/delete on a vanished session (issue 07).
var ErrSessionNotFound = errors.New("playback: session not found")

// ErrBurnSubtitleNotFound means a request carried a burnSubtitleId that does not
// resolve to a burnable IMAGE Subtitle track of the Title — an unknown id, a text
// track (text is delivered selectably, never burned), or a track over an audio-
// only File. The api layer maps it to 404 (hide existence): there is no image
// subtitle to burn, and the caller should not have asked to burn one.
var ErrBurnSubtitleNotFound = errors.New("playback: burn subtitle track not found")

// ErrAudioStreamNotFound means a request carried an audioStreamId that does not
// resolve to an audio Stream of the Title — an unknown id, an id belonging to a
// video/subtitle Stream, or one from another File/Title. The api layer maps it to
// 404 (hide existence): selection must name a real audio Stream of this Title, and
// a stale/invalid id fails structurally rather than silently falling back to the
// default (audio-streams/02).
var ErrAudioStreamNotFound = errors.New("playback: audio stream not found")

// ErrVideoStreamNotFound means a request carried a videoStreamId that does not
// resolve to a selectable (non-cover-art) video Stream of the Title — an unknown id,
// an id belonging to an audio/subtitle Stream, an embedded cover-art still, or one
// from another File. The api layer maps it to 404 (hide existence): selection must
// name a real video Stream of the played File, and a stale/invalid id fails
// structurally rather than silently falling back to the default (selectable-video/02).
var ErrVideoStreamNotFound = errors.New("playback: video stream not found")

// The Watched threshold — SERVER constants, never per-User and never sent by the
// client (CONTEXT.md "Watched threshold"). The server applies these against the
// session File's DurationMs when a progress report arrives:
//
//   - At or above WatchedCeiling (~90%) the Title is marked watched and its
//     resume is cleared, so it leaves Continue Watching.
//   - Below StartedFloor (~2%) the position counts as "not started": no resume is
//     recorded, so a quick open-then-close never clutters Continue Watching.
//   - In between, the raw position is stored as the resume offset.
//
// They are fractions of the File duration, applied in ReportProgress — the single
// place the threshold lives. Clients report only raw positionMs and state; they
// cannot invent "watched" semantics.
const (
	WatchedCeiling = 0.90
	StartedFloor   = 0.02
)

// TitleStore is the slice of the catalog the playback Service reads: the nested
// Title tree it negotiates against. *store.DB satisfies it via TitleByID, and the
// existing catalog.Service does the same lookup — a narrow interface keeps the
// negotiation testable without a live database.
type TitleStore interface {
	TitleByID(id string) (store.TitleDetail, error)
}

// WatchStateStore is the persistence the progress/watch-state side needs:
// read-current and write-resume+watched for a (User, Title). *store.DB satisfies
// it. Kept narrow so progress handling is testable without a live database. The
// played flag distinguishes the two write paths (ADR-0028): progress reports pass
// played=true so the write stamps played_at (the Up Next anchor's recency),
// SetWatchState's manual toggle passes played=false so the anchor never moves.
type WatchStateStore interface {
	WatchStateFor(userID, titleID string) (store.WatchState, error)
	SaveWatchState(userID, titleID string, resumeMs int64, watched, played bool) error
}

// AudioMemoryStore is the Remembered-audio persistence (audio-streams/05, ADR-0023):
// read/write the per-(User, Title) pick, the per-(User, Show) bubble-up, and the
// Episode->Show linkage the bubble-up keys on. *store.DB satisfies it. It is an
// OPTIONAL dependency of the Service — NewService type-asserts it off the passed
// store, so a fake store that does not implement it simply runs with memory disabled
// (the pre-05 behavior), which keeps every existing playback unit test compiling
// unchanged. Production always passes *store.DB, so memory is always live there.
type AudioMemoryStore interface {
	RememberedAudioForTitle(userID, titleID string) (store.RememberedAudio, bool, error)
	SaveRememberedAudioForTitle(userID, titleID string, m store.RememberedAudio) error
	RememberedAudioForShow(userID, showID string) (store.RememberedAudio, bool, error)
	SaveRememberedAudioForShow(userID, showID string, m store.RememberedAudio) error
	ShowIDForTitle(titleID string) (string, bool, error)
}

// VideoMemoryStore is the Remembered-video persistence (selectable-video/04, ADR-0025,
// ADR-0023 mirrored): read/write the per-(User, Title) video pick, the per-(User, Show)
// bubble-up, and the Episode->Show linkage the bubble-up keys on. It is the direct video
// mirror of AudioMemoryStore. *store.DB satisfies it. Like AudioMemoryStore it is an
// OPTIONAL dependency — NewService type-asserts it off the passed store, so a fake store
// that does not implement it runs with video memory disabled (video resolution falls
// back to the capability-then-quality default), keeping existing playback unit tests
// compiling unchanged.
type VideoMemoryStore interface {
	RememberedVideoForTitle(userID, titleID string) (store.RememberedVideo, bool, error)
	SaveRememberedVideoForTitle(userID, titleID string, m store.RememberedVideo) error
	RememberedVideoForShow(userID, showID string) (store.RememberedVideo, bool, error)
	SaveRememberedVideoForShow(userID, showID string, m store.RememberedVideo) error
	ShowIDForTitle(titleID string) (string, bool, error)
}

// Service is the direct-play negotiation domain: given a Title, a Capability
// profile, and constraints, it picks the best playable Edition, makes the
// directPlay-or-TRANSCODE_REQUIRED decision, and creates the Playback session
// that backs the stream. It owns the in-memory session Manager and applies the
// Watched threshold to incoming progress reports.
type Service struct {
	store TitleStore
	watch WatchStateStore
	// audioMem is the Remembered-audio store (audio-streams/05, ADR-0023), or nil
	// when the passed store does not implement AudioMemoryStore (a fake in some unit
	// tests) — in which case memory read/write is skipped and audio resolution falls
	// back to preferredAudioLang -> default -> first, the pre-05 behavior.
	audioMem AudioMemoryStore
	// videoMem is the Remembered-video store (selectable-video/04, ADR-0025), or nil
	// when the passed store does not implement VideoMemoryStore (a fake in some unit
	// tests) — in which case video memory read/write is skipped and video resolution
	// stays on the capability-then-quality default, the pre-04 behavior.
	videoMem VideoMemoryStore
	sessions *Manager
	// accel is the resolved video encode backend threaded into every transcode
	// job's args (ADR-0009 HW-accel knob). It is transcode.AccelCPU (the zero
	// value, software libx264) unless the server's HardwareAccel config selects a
	// backend; VideoToolbox is honored by the args builder and every other value
	// falls back to the guaranteed CPU path.
	accel transcode.Accel
}

// Governance holds the ADR-0009 governance knobs the Service applies: the global
// concurrent-transcode cap (0 = unlimited) and the resolved video-encode backend
// (HW-accel knob). Accel is the already-resolved transcode.Accel the app derives
// from config.HardwareAccel — the zero value (AccelCPU) is the always-available
// software path, so a zero Governance (the unit-test default) is unlimited + CPU,
// matching the pre-governance behavior. The args builder (transcode.videoBackend)
// falls any not-yet-wired backend back to CPU, so this stays a plain passthrough.
type Governance struct {
	MaxConcurrentTranscodes int
	Accel                   transcode.Accel
}

// NewService builds the playback Service over the catalog store and a session
// Manager that can back the remux/HLS tier: runner starts ffmpeg lazily for a
// directStream session, and each session's HLS scratch lives under scratchRoot
// (ADR-0007). s must satisfy both TitleStore (negotiation) and WatchStateStore
// (progress); *store.DB does. Passing a nil runner / empty scratchRoot yields a
// direct-play-only Service (the playback unit tests do this). gov carries the
// ADR-0009 governance knobs (transcode cap + the resolved encode backend); the
// zero Governance is unlimited + CPU.
func NewService(s interface {
	TitleStore
	WatchStateStore
}, runner transcode.Runner, scratchRoot string, gov Governance) *Service {
	var mgr *Manager
	if runner != nil && scratchRoot != "" {
		mgr = NewRemuxManager(runner, scratchRoot)
	} else {
		mgr = NewManager()
	}
	mgr.SetTranscodeCap(gov.MaxConcurrentTranscodes)
	// gov.Accel is the resolved backend (CPU by default). It is threaded straight
	// into every transcode job's args; the args builder honors VideoToolbox and
	// falls every other value back to the guaranteed CPU path (ADR-0009).
	svc := &Service{store: s, watch: s, sessions: mgr, accel: gov.Accel}
	// Remembered audio (audio-streams/05) is an optional capability: wire it only
	// when the store implements it (*store.DB does in production). A fake store that
	// doesn't runs with memory disabled, so pre-05 unit tests keep passing untouched.
	if am, ok := s.(AudioMemoryStore); ok {
		svc.audioMem = am
	}
	// Remembered video (selectable-video/04) is wired the same way: only when the store
	// implements it. A fake store that doesn't runs with video memory disabled.
	if vm, ok := s.(VideoMemoryStore); ok {
		svc.videoMem = vm
	}
	return svc
}

// Sessions exposes the session Manager so the api layer can serve the stream and
// the DELETE without a second lookup path.
func (s *Service) Sessions() *Manager { return s.sessions }

// TranscodeLoad returns the live transcode-governance snapshot (active count +
// cap) for the admin /transcoding surface (ADR-0029), delegating to the Manager
// so the api layer stays out of the counter internals.
func (s *Service) TranscodeLoad() TranscodeLoad { return s.sessions.TranscodeLoad() }

// Request is a validated playback-negotiation request: the inline Capability
// profile (device profile + constraints), the resume offset, and an optional
// explicit Edition. clientId-referenced profiles are deferred (see package doc),
// so the profile is always carried inline here.
type Request struct {
	UserID        string
	DeviceID      string
	TitleID       string
	Profile       DeviceProfile
	Constraints   Constraints
	StartPosition int64
	EditionID     string // "" → server auto-selects the best playable Edition
	// BurnSubtitleID, when set, selects an IMAGE Subtitle track to burn into the
	// video frames (ADR-0020, subtitles/04). It ESCALATES negotiation to the
	// transcode tier (which consumes a governance cap slot and can return
	// ServerBusy) and plays the File that carries the track (for an embedded sub) or
	// the best playable File (for a Title-scoped sidecar). An id that resolves to no
	// image track of the Title — an unknown id or a TEXT track (text is client-side,
	// never burned) — is ErrBurnSubtitleNotFound. Empty leaves negotiation on its
	// cheapest tier, unchanged.
	BurnSubtitleID string
	// AudioStreamID, when set, selects the audio Stream to deliver (audio-streams/02,
	// ADR-0022) — exactly parallel to BurnSubtitleID: selection is a fresh
	// negotiation, never a session-mutate. The Decision resolves and reports that
	// Stream and the delivered bytes carry it. A NON-DEFAULT selection on an
	// otherwise direct-playable File escalates to remux (direct play carries only the
	// default audio); a Stream whose codec the client can't decode escalates to a
	// governed transcode (busy-rejectable at start). An id that names no audio Stream
	// of the Title is ErrAudioStreamNotFound. Empty leaves audio on the default
	// resolution order (preferred language → default disposition → first), unchanged.
	AudioStreamID string
	// VideoStreamID, when set, selects the video Stream to deliver (selectable-video/02,
	// ADR-0025) — the video parallel of AudioStreamID, but following the image-subtitle
	// RESTART model, not the in-band audio one: there is no user-selectable video
	// rendition in HLS, so a non-default pick is a full re-negotiation. Direct play
	// carries only the DEFAULT video Stream, so a non-default selection escalates to HLS
	// remux where the server maps the chosen Stream with -map 0:v:N; a Stream the client
	// can't decode escalates to a GOVERNED transcode (busy-rejectable with 503 at the cap
	// — NOT cap-exempt, contrast the in-band audio switch). The switch preserves the
	// supplied AudioStreamID, BurnSubtitleID, and StartPosition. A videoStreamId equal to
	// the resolved default does not force an escalation off direct play. An id that names
	// no selectable video Stream of the Title is ErrVideoStreamNotFound. Empty leaves the
	// video on the capability-then-quality default, unchanged.
	VideoStreamID string
	// Scope is the caller's resolved access scope. A Title in a Library the caller
	// may not access is negotiated as not-found (404, hide existence) before any
	// session is created. No-op under an all-access scope (the current default).
	Scope access.Scope
}

// ServerBusy is the transcode-governance rejection (ADR-0009): the negotiated
// tier is transcode but the concurrent-transcode cap is full, so the server
// refuses to start another re-encode rather than queuing the client behind a
// spinner. The api layer renders it as 503 SERVER_BUSY with
// details: { retryable: true, suggestedMaxBitrate }. It is NOT an error in the
// Go sense (no fault occurred) — it travels as its own negotiation outcome,
// like *Unsupported. Direct play and remux never produce it.
type ServerBusy struct {
	// SuggestedMaxBitrate is a lower bitrate (bits/sec) the client could retry at
	// — see suggestBusyBitrate for the heuristic. A retry at this cap may land in
	// direct play / remux (no slot needed) or simply a cheaper transcode.
	SuggestedMaxBitrate int64
}

// busyBitrateFraction is the fraction of the would-be transcode's bitrate the
// server suggests a busy client retry at (ADR-0009 "suggested lower bitrate").
// Half is a deliberate, documented heuristic: a meaningful step down (likely to
// drop a tier or materially cut encode cost) without collapsing quality to
// unwatchable. It is the single knob behind suggestBusyBitrate.
const busyBitrateFraction = 0.5

// busyBitrateFloor is the lowest suggestedMaxBitrate we will ever return
// (bits/sec). It guards the heuristic against suggesting a uselessly tiny
// bitrate when the estimate is unknown or already very low — 600 kbps is a
// watchable low-quality SD floor.
const busyBitrateFloor = 600_000

// suggestBusyBitrate computes the suggestedMaxBitrate for a SERVER_BUSY response
// from the rejected transcode's estimated bitrate (and, as a fallback, the
// request's own maxBitrate constraint): half the estimate, never below
// busyBitrateFloor. When neither is known it returns the floor — a safe, always-
// retryable suggestion. The heuristic is pure so it is unit-testable in isolation.
func suggestBusyBitrate(estimated, requestedMax int64) int64 {
	base := estimated
	if base <= 0 {
		base = requestedMax
	}
	if base <= 0 {
		return busyBitrateFloor
	}
	suggested := int64(float64(base) * busyBitrateFraction)
	if suggested < busyBitrateFloor {
		suggested = busyBitrateFloor
	}
	// Never suggest something at or above the base — a retry must be a real step
	// down, else the client loops on the same busy outcome.
	if suggested >= base {
		suggested = base / 2
		if suggested < 1 {
			suggested = 1
		}
	}
	return suggested
}

// Negotiate resolves the Title, selects the best playable Edition for the
// client, and on success creates a Playback session and returns the Decision
// alongside it. It returns:
//   - ErrTitleNotFound (mapped to 404) for an unknown Title or one whose Files are
//     all Missing (nothing to play);
//   - a non-nil *Unsupported (mapped to TRANSCODE_REQUIRED) when no Edition can be
//     direct-played and the failure is structural;
//   - a non-nil *ServerBusy (mapped to 503 SERVER_BUSY) when the chosen tier is
//     transcode and the concurrent-transcode cap is full (ADR-0009);
//   - otherwise the Decision and the created Session.
//
// The error return is reserved for genuine faults (store failures); negotiation
// outcomes travel as ErrTitleNotFound, *Unsupported, or *ServerBusy. The cap is
// only consulted for a transcode Decision — direct play and remux are unmetered
// and never busy.
func (s *Service) Negotiate(req Request) (Decision, Session, *Unsupported, *ServerBusy, error) {
	detail, err := s.store.TitleByID(req.TitleID)
	if errors.Is(err, store.ErrNotFound) {
		return Decision{}, Session{}, nil, nil, ErrTitleNotFound
	}
	if err != nil {
		return Decision{}, Session{}, nil, nil, err
	}
	// A Title in a Library the caller may not access, or above their Rating
	// ceiling, is hidden as 404 — no stream is negotiated and no session is
	// created. No-op under an all-access scope.
	if !req.Scope.AllowsLibrary(detail.LibraryID) || !req.Scope.AllowsRating(detail.ContentRating) {
		return Decision{}, Session{}, nil, nil, ErrTitleNotFound
	}

	dec, unsup := SelectEdition(req.Profile, req.Constraints, detail.Editions, req.EditionID)
	if unsup != nil {
		// No present File at all is "not found" (hide existence), not a transcode
		// hint — there is genuinely nothing to stream.
		if unsup.Reason == ReasonNoFile {
			return Decision{}, Session{}, nil, nil, ErrTitleNotFound
		}
		return Decision{}, Session{}, unsup, nil, nil
	}

	// Attach the full Subtitle-track list for the chosen File so the decision can
	// offer every selectable track (embedded Streams + the Title's Sidecar/Fetched
	// rows), delivered per its kind (ADR-0020). SelectEdition leaves this nil; the
	// pure negotiation stays subtitle-agnostic and the Service (which holds the
	// Title detail) fills it here.
	dec.Subtitles = buildSubtitleTracks(dec.File, detail.Subtitles)

	// Image-subtitle burn-in escalation (ADR-0020, subtitles/04): a burnSubtitleId
	// that resolves to an image track FORCES the transcode tier with the subtitle
	// burned into the frames — even if the File would otherwise direct-play or remux.
	// A text/unknown id has no image sub to burn, so it is ErrBurnSubtitleNotFound.
	if req.BurnSubtitleID != "" {
		burnDec, berr := escalateForBurn(req, detail, dec)
		if berr != nil {
			return Decision{}, Session{}, nil, nil, berr
		}
		dec = burnDec
	}

	// Audio selection resolution order (audio-streams/05, ADR-0023):
	//   Title memory → Show memory → preferredAudioLang → default disposition → first.
	// An EXPLICIT audioStreamId (a viewer's pick, or a direct-play escalation) wins
	// outright and is remembered afterward; otherwise Remembered audio re-resolves by
	// MEANING against the chosen File's Streams and, when it matches, pins the audio
	// exactly like an explicit pick — WITHOUT writing memory (passive resolution never
	// writes). The tail of the order (preferredAudioLang → default → first) already
	// lives in SelectEdition's pickAudioStream, so a no-memory / no-match session keeps
	// the pre-05 Decision untouched.
	explicitAudio := req.AudioStreamID != ""
	audioID := req.AudioStreamID
	if !explicitAudio {
		if rememberedID, ok := s.resolveRememberedAudio(req.UserID, detail, dec.File); ok {
			audioID = rememberedID
		}
	}
	if audioID != "" {
		audioDec, aerr := escalateForAudio(req, detail, dec, audioID)
		if aerr != nil {
			// An explicit id that resolves to nothing is a hard 404 (hide existence). A
			// memory-derived id always comes from the chosen File's own Streams, so it
			// resolves — but if a race removed it, degrade to the default rather than error.
			if explicitAudio {
				return Decision{}, Session{}, nil, nil, aerr
			}
		} else {
			dec = audioDec
		}
	}

	// Video selection resolution order (selectable-video/04, ADR-0025, ADR-0023 mirrored):
	//   Title memory → Show memory → capability-then-quality default (already resolved) →
	//   is_default disposition → first Stream.
	// An EXPLICIT videoStreamId (a viewer's pick) wins outright and is remembered
	// afterward; otherwise Remembered video re-resolves by MEANING against the chosen
	// File's video Streams and, when it matches, re-pins the video exactly like an
	// explicit pick — WITHOUT writing memory (passive resolution never writes). No match
	// (or no memory) leaves the Decision on its capability-then-quality default, unchanged
	// from selectable-video/01. It runs AFTER the burn+audio escalations so the preserved
	// audioStreamId, burnSubtitleId, and startPosition ride through unchanged — the switch
	// is a full re-negotiation that only swaps the video and re-tiers. Direct play carries
	// only the default video, so a non-default pick escalates (remux for a decodable
	// Stream, a governed transcode for one the client can't decode); a pick equal to the
	// resolved default is a no-op.
	explicitVideo := req.VideoStreamID != ""
	videoID := req.VideoStreamID
	if !explicitVideo {
		if rememberedID, ok := s.resolveRememberedVideo(req.UserID, detail, dec.File); ok {
			videoID = rememberedID
		}
	}
	if videoID != "" {
		videoDec, verr := escalateForVideo(req, detail, dec, videoID)
		if verr != nil {
			// An explicit id that resolves to nothing is a hard 404 (hide existence). A
			// memory-derived id always comes from the chosen File's own Streams, so it
			// resolves — but if a race removed it, degrade to the default rather than error.
			if explicitVideo {
				return Decision{}, Session{}, nil, nil, verr
			}
		} else {
			dec = videoDec
		}
	}

	// A video-copy transcode (ADR-0024) copies the source video and transcodes only
	// the audio — mark it so the session runs UNMETERED with an ffmpeg-owned playlist
	// (like remux: a copied stream has no forced-keyframe grid), rather than the
	// synthesized-playlist video-transcode path. Computed from the same plan the args
	// builder uses, so the two agree. Only a video-bearing, non-burn transcode qualifies.
	if dec.Tier == TierTranscode && !dec.AudioOnly && dec.Burn == nil {
		dec.VideoCopy = planVideoFor(req.Profile, req.Constraints, dec).Copy
	}
	// Stamp the client's HLS segment-container capability onto the Decision so every
	// downstream UsesFMP4() consumer (session runtime, args builders, master CODECS)
	// routes a copied HEVC to the container this client actually plays: MPEG-TS for
	// an hls.js client, fMP4 for Apple's native player.
	dec.HevcInMpegTS = req.Profile.HevcInMpegTS

	// A video COPY / REMUX serves a SERVER-SYNTHESIZED media playlist computed from the
	// source's keyframes, because ffmpeg writes its own copy playlist only when the
	// whole (feature-length) input finishes — so serving ffmpeg's would 404 the long
	// file. Probe the keyframes now so the playlist is ready when playback starts; the
	// probe reads only the file's keyframe index (fast even on a large/remote file) and
	// is best-effort — a failure leaves boundaries nil and the runtime falls back to
	// ffmpeg's playlist (correct for short files). Skipped for a re-encode (uniform
	// segments) and audio-only.
	var boundaries []float64
	if !dec.AudioOnly && (dec.VideoCopy || dec.Tier == TierDirectStream) {
		boundaries = s.probeSegmentBoundaries(dec.File)
	}

	// CreateGoverned enforces the transcode cap atomically (ADR-0009): only a
	// transcode Decision can be rejected, and only when the cap is full — direct
	// play and remux always create their session. A rejection becomes a ServerBusy
	// outcome carrying a suggested lower bitrate the client can retry at.
	hlsArgs, cpuFallback, audioRendition := s.hlsArgsBuilders(req.Profile, req.Constraints, dec, boundaries)
	sess, err := s.sessions.CreateGoverned(CreateInput{
		UserID:                  req.UserID,
		DeviceID:                req.DeviceID,
		TitleID:                 req.TitleID,
		StartPosition:           req.StartPosition,
		BuildHLSArgs:            hlsArgs,
		BuildHLSArgsCPU:         cpuFallback,
		BuildAudioRenditionArgs: audioRendition,
		SegmentBoundaries:       boundaries,
	}, dec)
	if errors.Is(err, ErrTranscodeCapFull) {
		return Decision{}, Session{}, nil, &ServerBusy{
			SuggestedMaxBitrate: suggestBusyBitrate(dec.EstimatedBitrate, req.Constraints.MaxBitrate),
		}, nil
	}
	if err != nil {
		return Decision{}, Session{}, nil, nil, err
	}

	// Only an EXPLICIT pick writes Remembered audio (ADR-0023): the resolved Stream
	// becomes the Title's memory and — for a non-commentary Episode pick — the Show's
	// bubble-up default. Passive default/memory resolution never writes. A memory
	// write is best-effort: the session already exists, so a store hiccup must not
	// fail the playback the client is about to start.
	if explicitAudio {
		s.rememberAudioPick(req.UserID, req.TitleID, dec.AudioStream)
	}
	// Only an EXPLICIT pick writes Remembered video (ADR-0025): the resolved video Stream
	// becomes the Title's memory and — for an Episode — the Show's bubble-up. Passive
	// default/memory resolution never writes. Best-effort, like the audio write.
	if explicitVideo {
		s.rememberVideoPick(req.UserID, req.TitleID, dec.VideoStream)
	}
	return dec, sess, nil, nil, nil
}

// resolveRememberedAudio applies the Remembered-audio half of the resolution order
// (ADR-0023): the Title's pick first, then the Show's bubble-up, each re-resolved by
// MEANING against the chosen File's current audio Streams (exact-trait → language).
// It returns the resolved Stream's id and ok=true on the first level that matches;
// ok=false when memory is disabled, absent, or matches nothing — the caller then
// leaves the Decision on its preferredAudioLang → default → first resolution. Store
// reads are best-effort: an error degrades to "no memory", never blocks playback.
func (s *Service) resolveRememberedAudio(userID string, detail store.TitleDetail, f store.File) (string, bool) {
	if s.audioMem == nil {
		return "", false
	}
	// Title memory.
	if mem, found, err := s.audioMem.RememberedAudioForTitle(userID, detail.ID); err == nil && found {
		if stream, ok := resolveRememberedAudioStream(f.Streams, mem); ok {
			return stream.ID, true
		}
	}
	// Show memory (the bubble-up default for an Episode without its own pick). A Movie
	// has no Show, so ShowIDForTitle reports found=false and this level is skipped.
	if showID, ok, err := s.audioMem.ShowIDForTitle(detail.ID); err == nil && ok {
		if mem, found, err := s.audioMem.RememberedAudioForShow(userID, showID); err == nil && found {
			if stream, ok := resolveRememberedAudioStream(f.Streams, mem); ok {
				return stream.ID, true
			}
		}
	}
	return "", false
}

// rememberAudioPick stores an explicit pick as Remembered audio (ADR-0023): always
// the Title's memory, and — for an Episode — the Show's bubble-up UNLESS the pick is
// a commentary, which stays quarantined on its Title so a one-off commentary choice
// never becomes the whole Show's default (the rest of the Show keeps the language
// pick). Best-effort: errors are swallowed so a memory write never fails the play.
func (s *Service) rememberAudioPick(userID, titleID string, chosen store.Stream) {
	if s.audioMem == nil || chosen.ID == "" {
		return
	}
	mem := audioMemoryOf(chosen)
	_ = s.audioMem.SaveRememberedAudioForTitle(userID, titleID, mem)

	if mem.Commentary {
		// Quarantined: a commentary pick does not bubble up to the Show.
		return
	}
	if showID, ok, err := s.audioMem.ShowIDForTitle(titleID); err == nil && ok {
		_ = s.audioMem.SaveRememberedAudioForShow(userID, showID, mem)
	}
}

// resolveRememberedVideo applies the Remembered-video half of the resolution order
// (ADR-0025, ADR-0023 mirrored): the Title's pick first, then the Show's bubble-up, each
// re-resolved by MEANING against the chosen File's current video Streams (exact-trait →
// label). It returns the resolved Stream's id and ok=true on the first level that
// matches; ok=false when memory is disabled, absent, or matches nothing — the caller
// then leaves the Decision on its capability-then-quality default. Store reads are
// best-effort: an error degrades to "no memory", never blocks playback. The direct video
// mirror of resolveRememberedAudio.
func (s *Service) resolveRememberedVideo(userID string, detail store.TitleDetail, f store.File) (string, bool) {
	if s.videoMem == nil {
		return "", false
	}
	// Title memory.
	if mem, found, err := s.videoMem.RememberedVideoForTitle(userID, detail.ID); err == nil && found {
		if stream, ok := resolveRememberedVideoStream(f.Streams, mem); ok {
			return stream.ID, true
		}
	}
	// Show memory (the bubble-up default for an Episode without its own pick). A Movie
	// has no Show, so ShowIDForTitle reports found=false and this level is skipped.
	if showID, ok, err := s.videoMem.ShowIDForTitle(detail.ID); err == nil && ok {
		if mem, found, err := s.videoMem.RememberedVideoForShow(userID, showID); err == nil && found {
			if stream, ok := resolveRememberedVideoStream(f.Streams, mem); ok {
				return stream.ID, true
			}
		}
	}
	return "", false
}

// rememberVideoPick stores an explicit pick as Remembered video (ADR-0025): always the
// Title's memory, and — for an Episode — the Show's bubble-up. Unlike audio there is no
// commentary quarantine: a video Stream has no commentary analogue, so every Episode pick
// bubbles up to the Show. Best-effort: errors are swallowed so a memory write never fails
// the play. The direct video mirror of rememberAudioPick.
func (s *Service) rememberVideoPick(userID, titleID string, chosen store.Stream) {
	if s.videoMem == nil || chosen.ID == "" {
		return
	}
	mem := videoMemoryOf(chosen)
	_ = s.videoMem.SaveRememberedVideoForTitle(userID, titleID, mem)
	if showID, ok, err := s.videoMem.ShowIDForTitle(titleID); err == nil && ok {
		_ = s.videoMem.SaveRememberedVideoForShow(userID, showID, mem)
	}
}

// escalateForBurn rebuilds a Decision as a TRANSCODE with the requested image
// Subtitle track burned in (subtitles/04). It resolves the burn target from the
// Title detail (embedded Stream or Sidecar/Fetched row); an id that is not a
// burnable image track is ErrBurnSubtitleNotFound. It then transcodes the RIGHT
// File — the File that carries an embedded track, or the already-chosen best File
// for a Title-scoped sidecar — re-attaches the Subtitle list, and stamps
// Decision.Burn so the args builder emits the -vf subtitles= overlay. A File that
// cannot transcode (no video Stream — an audio-only File a sub cannot overlay) is
// also ErrBurnSubtitleNotFound.
func escalateForBurn(req Request, detail store.TitleDetail, base Decision) (Decision, error) {
	target, ok := resolveBurnTarget(detail, req.BurnSubtitleID)
	if !ok {
		return Decision{}, ErrBurnSubtitleNotFound
	}
	// Pick the Edition+File to transcode: the embedded track's own File (so the
	// burned sub belongs to the played container), else the best File SelectEdition
	// already chose for a Title-scoped sidecar.
	ed, f := base.Edition, base.File
	if target.FileID != "" {
		e, file, found := editionFileByID(detail, target.EditionID, target.FileID)
		if !found {
			return Decision{}, ErrBurnSubtitleNotFound
		}
		ed, f = e, file
	}
	tdec, unsup := negotiateTranscode(req.Profile, req.Constraints, ed, f)
	if unsup != nil || tdec.AudioOnly {
		// No video to overlay (audio-only File) — there is nothing to burn onto.
		return Decision{}, ErrBurnSubtitleNotFound
	}
	tdec.Subtitles = buildSubtitleTracks(tdec.File, detail.Subtitles)
	tdec.Burn = &BurnSubtitle{Path: target.Path, StreamIndex: target.StreamIndex}
	return tdec, nil
}

// escalateForAudio re-resolves a Decision to deliver an explicitly-chosen audio
// Stream (audio-streams/02, ADR-0022), the audio parallel of escalateForBurn. It
// locates the Stream anywhere in the Title (an unknown id is ErrAudioStreamNotFound)
// and plays the File that carries it, choosing the cheapest tier that can actually
// deliver that Stream (audioSelectionTier): a non-default pick escalates to remux,
// an undecodable/oversized/non-remuxable codec to a transcode.
//
// It composes with a burn: when the base Decision already forced a transcode to
// burn an image sub, the audio pick is re-pinned WITHIN that same File (the common
// single-File case) and the burn is preserved — an audioStreamId naming a Stream in
// a different File than the burn is treated as not found rather than splitting the
// session across Files. An audioStreamId on an audio-only File (a Music Track) is
// out of scope for this Movie/TV slice and is likewise ErrAudioStreamNotFound.
func escalateForAudio(req Request, detail store.TitleDetail, base Decision, audioID string) (Decision, error) {
	ed, f, stream, ok := resolveAudioStream(detail, audioID)
	if !ok {
		return Decision{}, ErrAudioStreamNotFound
	}

	// Compose with an active burn only within its File: keep the burn's forced
	// transcode + Burn target and just re-pin which audio it maps.
	if base.Burn != nil {
		if f.ID != base.File.ID {
			return Decision{}, ErrAudioStreamNotFound
		}
		base.AudioStream = stream
		return base, nil
	}

	video, hasVideo := defaultVideoStream(req.Profile, req.Constraints, f)
	if !hasVideo {
		// Audio-only File (a Music Track): multi-audio music selection is out of scope
		// (PRD) — don't invent a tier, treat as not found.
		return Decision{}, ErrAudioStreamNotFound
	}

	tier := audioSelectionTier(req.Profile, req.Constraints, ed, f, stream)
	dec := Decision{
		Tier:             tier,
		Edition:          ed,
		File:             f,
		VideoStream:      video,
		AudioStream:      stream,
		EstimatedBitrate: estimatedBitrateFor(f, req.Constraints, tier),
	}
	dec.Subtitles = buildSubtitleTracks(f, detail.Subtitles)
	return dec, nil
}

// escalateForVideo re-resolves a Decision to deliver an explicitly-chosen video
// Stream (selectable-video/02, ADR-0025), the video parallel of escalateForAudio —
// but following the image-subtitle RESTART model (there is no in-band video
// rendition). It locates the Stream in the Title (an unknown/cover-art/foreign id is
// ErrVideoStreamNotFound) and, since the selectable video Streams are co-packaged in
// one File with the shared audio (same-container scope), requires the pick to live in
// the already-resolved File — an id from another File/Edition would split
// audio/subtitle selection across containers and is treated as not found.
//
// It PRESERVES the base Decision's audio/subtitle/burn picks and resume position by
// copying base and swapping only the video Stream and the tier: the delivered tier is
// the more expensive of the base tier (already escalated for a burn or a non-default/
// undecodable audio pick) and the video pick's own floor (videoSelectionFloor) — so
// both selections are honored. A videoStreamId equal to the resolved default leaves
// the tier untouched (no needless escalation off direct play).
func escalateForVideo(req Request, detail store.TitleDetail, base Decision, videoID string) (Decision, error) {
	ed, f, stream, ok := resolveVideoStream(detail, videoID)
	if !ok {
		return Decision{}, ErrVideoStreamNotFound
	}
	// Same-container scope (ADR-0025): a valid pick names a Stream of the File the rest
	// of the Decision already resolved (which also carries the shared audio). A Stream
	// in a different File is out of scope — treat as not found rather than splitting the
	// session across containers.
	if f.ID != base.File.ID {
		return Decision{}, ErrVideoStreamNotFound
	}

	dec := base
	dec.VideoStream = stream
	// base.VideoStream is always the capability-then-quality default (SelectEdition and
	// both escalations resolve it via defaultVideoStream), so it is the reference the
	// floor compares against to decide whether this pick escalates.
	floor := videoSelectionFloor(req.Profile, req.Constraints, ed, f, stream, base.VideoStream)
	dec.Tier = maxTier(base.Tier, floor)
	dec.EstimatedBitrate = estimatedBitrateFor(f, req.Constraints, dec.Tier)
	return dec, nil
}

// hlsArgsBuilders returns the ffmpeg-args builder for a directStream/transcode
// Decision AND, for a hardware-encoded transcode, its CPU-fallback sibling (both
// nil for direct play, which has no HLS job). The Manager calls a builder with the
// session's scratch dir AND a seek offset; the seek offset is the realignment hook
// (ADR-0004) — the zero value is the from-the-top launch, a non-zero offset
// restarts ffmpeg near a sought timestamp. Keeping the per-tier arg construction
// here — where the profile/constraints live — lets the transcode planner pick the
// right copy-vs-re-encode + scale/downmix flags, while the session Manager/runtime
// stay oblivious to ffmpeg specifics.
//
//   - directStream → RemuxArgs (`-c copy`), unchanged from slice 1 plus the seek;
//     no CPU-fallback builder (remux re-encodes nothing).
//   - transcode    → TranscodeArgs, planning each track: copy an already-HLS-
//     friendly stream, else re-encode video to the configured backend (scaled
//     within the binding resolution cap, capped to the bitrate constraint) and
//     audio to AAC (downmixed past the client's channel cap), with the seek for
//     realignment. When that backend is HARDWARE (transcode.IsHardware), the second
//     return is the identical builder forced to AccelCPU — the per-session fallback
//     the runtime restarts on if the hardware job fails to launch (issue 03).
// boundaries, when non-nil, are the probed keyframe segment boundaries of a video
// COPY (Negotiate probes them for remux/video-copy sessions): a TS copy job then
// DICTATES its cut times to the segment muxer so segments match the synthesized
// playlist by construction, per seek offset (segmentTimesFor).
func (s *Service) hlsArgsBuilders(profile DeviceProfile, constraints Constraints, dec Decision, boundaries []float64) (primary, cpuFallback func(string, transcode.SeekOffset) []string, audioRendition func(streamID, outputDir string, seek transcode.SeekOffset) []string) {
	// A multi-audio HLS session is DEMUXED (audio-streams/03, ADR-0022): the video
	// variant carries NO audio (each audio Stream rides as its own in-band rendition),
	// and the audio-rendition builder mints those renditions lazily. A single-audio
	// (or direct-play / audio-only) session stays muxed by construction — the video
	// job carries its one audio track and no rendition builder is set.
	demuxed := IsDemuxed(dec)
	// fMP4 delivery when a copied non-h264 video (HEVC) rides the session — MPEG-TS
	// cannot carry it for Safari (ADR-0024). It applies to a remux of a HEVC File and
	// to a video-copy transcode; every rendition (video variant + audio) shares it.
	fmp4 := dec.UsesFMP4()
	// Cut-time dictation applies only to a TS copy (fMP4 keeps the hls muxer).
	cutTimes := boundaries
	if fmp4 {
		cutTimes = nil
	}
	switch dec.Tier {
	case TierDirectStream:
		src := dec.File.Path
		// Pin the negotiated video Stream on a multi-video File so the copied output
		// carries the resolved cut, not ffmpeg's implicit first video (nil for single-
		// video → byte-for-byte unchanged, selectable-video/01).
		videoIdx := videoMapIndex(dec.File, dec.VideoStream)
		if demuxed {
			// Video-only variant; the audio is delivered as renditions.
			build := func(outputDir string, seek transcode.SeekOffset) []string {
				return transcode.RemuxArgs(transcode.RemuxJob{SourcePath: src, OutputDir: outputDir, Seek: seek, VideoOnly: true, VideoStreamIndex: videoIdx, FMP4: fmp4, SegmentTimes: segmentTimesFor(cutTimes, seek)})
			}
			return build, nil, s.audioRenditionBuilder(profile, constraints, dec.File, fmp4)
		}
		// Pin the negotiated audio Stream into the copied output on a multi-audio File
		// (nil for single-audio → byte-for-byte the original copy-everything remux),
		// closing the reported-vs-audible divergence (audio-streams/02).
		audioIdx := audioMapIndex(dec.File, dec.AudioStream)
		return func(outputDir string, seek transcode.SeekOffset) []string {
			return transcode.RemuxArgs(transcode.RemuxJob{SourcePath: src, OutputDir: outputDir, Seek: seek, VideoStreamIndex: videoIdx, AudioStreamIndex: audioIdx, FMP4: fmp4, SegmentTimes: segmentTimesFor(cutTimes, seek)})
		}, nil, nil
	case TierTranscode:
		plan := transcodeJobPlan(profile, constraints, dec)
		// A demuxed video transcode drops audio (-an); the audio comes from renditions.
		if demuxed {
			plan.VideoOnly = true
			plan.AudioStreamIndex = nil
		}
		// A re-encode picks its own uniform keyframe grid — dictation is only for a
		// copied video, whose cuts must land on the source's keyframes.
		planCuts := cutTimes
		if !plan.Video.Copy {
			planCuts = nil
		}
		// transcodeArgsBuilder closes over a COPY of the plan with a chosen backend, so
		// the hardware and CPU-fallback builders are independent — flipping one's Accel
		// never mutates the other.
		primary = transcodeArgsBuilder(plan, s.accel, planCuts)
		// Arm the per-session hardware→CPU fallback ONLY when the configured backend is
		// genuinely hardware AND we are actually re-encoding the video; a video-COPY
		// transcode (ADR-0024) runs no video encoder, so it has nothing to fall back
		// from and stays ineligible.
		if transcode.IsHardware(s.accel) && !plan.Video.Copy {
			cpuFallback = transcodeArgsBuilder(plan, transcode.AccelCPU, nil)
		}
		if demuxed {
			audioRendition = s.audioRenditionBuilder(profile, constraints, dec.File, fmp4)
		}
		return primary, cpuFallback, audioRendition
	default:
		return nil, nil, nil
	}
}

// segmentTimesFor converts the absolute keyframe boundaries into the CUT times for
// a job starting at seek.StartNumber, relative to that job's output timeline (a
// realigned job restarts near 0). The final boundary is the file duration — EOF,
// not a cut — so it is excluded. nil boundaries (no probe / fMP4 / re-encode) or a
// start at/past the last segment yield nil, keeping the plain hls-muxer args.
func segmentTimesFor(boundaries []float64, seek transcode.SeekOffset) []float64 {
	if len(boundaries) < 3 || seek.StartNumber >= len(boundaries)-1 {
		return nil
	}
	base := boundaries[seek.StartNumber]
	out := make([]float64, 0, len(boundaries)-2-seek.StartNumber)
	for k := seek.StartNumber + 1; k <= len(boundaries)-2; k++ {
		out = append(out, boundaries[k]-base)
	}
	return out
}

// isDemuxed reports whether an HLS session for this Decision uses the demuxed
// multi-audio layout (audio-streams/03): a video-only variant plus one in-band
// audio rendition per Stream. It is on for an HLS tier (remux or transcode) File
// that has a real video track and 2+ audio Streams; a single-audio File, an
// audio-only Music Track, and direct play all stay muxed/unchanged by construction.
func IsDemuxed(dec Decision) bool {
	if dec.Tier == TierDirectPlay || dec.AudioOnly {
		return false
	}
	return audioStreamCount(dec.File) >= 2
}

// audioStreamCount counts the File's audio Streams — the multi-audio test that
// gates demuxing and the audio-rendition group.
func audioStreamCount(f store.File) int {
	n := 0
	for _, s := range f.Streams {
		if s.Kind == "audio" {
			n++
		}
	}
	return n
}

// audioRenditionBuilder returns the per-session ffmpeg-args builder for a demuxed
// File's audio renditions (audio-streams/03), closed over the client profile +
// constraints so it can decide copy-vs-AAC per Stream. Given an audio Stream id, the
// shared scratch dir, and a seek offset, it locates the Stream in the File, resolves
// its audio-relative -map index and the copy/AAC/downmix plan, and returns the
// AudioRenditionArgs vector under the rendition's namespaced filenames. An unknown
// id (defensive — the api validates first) yields nil args.
// fmp4 delivers the renditions as fragmented-MP4 (.m4s + a per-rendition init
// segment) so they share the container of a copied-HEVC video variant (ADR-0024);
// false keeps them MPEG-TS. Only AAC is ever copied (planAudioRendition), and AAC
// rides both containers, so the copy decision is unaffected — the flag only reshapes
// the output filenames.
func (s *Service) audioRenditionBuilder(profile DeviceProfile, constraints Constraints, f store.File, fmp4 bool) func(streamID, outputDir string, seek transcode.SeekOffset) []string {
	src := f.Path
	return func(streamID, outputDir string, seek transcode.SeekOffset) []string {
		idx, _, found := audioRelIndex(f, streamID)
		if !found {
			return nil
		}
		stream, ok := audioStreamByID(f, streamID)
		if !ok {
			return nil
		}
		copyStream, maxChannels := planAudioRendition(profile, stream, f)
		job := transcode.AudioRenditionJob{
			SourcePath:       src,
			OutputDir:        outputDir,
			AudioStreamIndex: idx,
			Copy:             copyStream,
			MaxChannels:      maxChannels,
			PlaylistName:     transcode.AudioRenditionPlaylist(streamID),
			SegmentPattern:   transcode.AudioRenditionSegmentPattern(streamID),
			Seek:             seek,
		}
		if fmp4 {
			job.SegmentPattern = transcode.AudioRenditionSegmentPatternFMP4(streamID)
			job.InitName = transcode.AudioRenditionInit(streamID)
		}
		return transcode.AudioRenditionArgs(job)
	}
}

// planAudioRendition decides how ONE demuxed audio rendition is produced
// (audio-streams/03): stream-copy the source Stream when the client's capability
// profile accepts its codec AND MPEG-TS can carry it AND it is within the channel
// cap; otherwise re-encode to AAC, downmixing to the channel cap when the source
// exceeds it. This is the audio parallel of PlanVideo/PlanAudio, but copy is allowed
// for ANY client-decodable, TS-carryable codec (ac3/eac3/mp3, not just aac) — the
// point of in-band delivery is to avoid a needless re-encode of a compatible track.
func planAudioRendition(profile DeviceProfile, stream store.Stream, f store.File) (copyStream bool, maxChannels int) {
	codec := firstNonEmpty(stream.Codec, f.AudioCodec)
	overChannelCap := profile.MaxAudioChannels > 0 && stream.Channels > profile.MaxAudioChannels
	if codec != "" && profile.supportsAudio(codec) && hlsRemuxableAudio(codec) && !overChannelCap {
		return true, 0
	}
	if overChannelCap {
		return false, profile.MaxAudioChannels
	}
	return false, 0
}

// audioStreamByID returns the File's audio Stream with id streamID, or false.
func audioStreamByID(f store.File, streamID string) (store.Stream, bool) {
	for _, s := range f.Streams {
		if s.Kind == "audio" && s.ID == streamID {
			return s, true
		}
	}
	return store.Stream{}, false
}

// keyframeProbeTimeout bounds the keyframe-index probe so a negotiation never hangs
// on an unreadable/very-slow source; on timeout the boundaries are nil and the runtime
// falls back to ffmpeg's own playlist.
const keyframeProbeTimeout = 15 * time.Second

// probeSegmentBoundaries returns the exact copy-mode HLS segment boundaries for a File
// (transcode.KeyframeBoundaries), or nil on any failure. Best-effort by design — a nil
// result degrades to serving ffmpeg's own playlist, which is correct for short files —
// but the failure is LOGGED: on a long file the fallback playlist appears only when
// the whole copy finishes, so a silent probe failure looks like an inexplicable HLS
// 404 to the operator.
func (s *Service) probeSegmentBoundaries(f store.File) []float64 {
	if f.Path == "" || f.DurationMs <= 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), keyframeProbeTimeout)
	defer cancel()
	b, err := transcode.KeyframeBoundaries(ctx, "", f.Path, float64(transcode.SegmentSeconds), float64(f.DurationMs)/1000)
	if err != nil {
		log.Printf("juicebox: playback: keyframe probe failed for %s: %v — serving ffmpeg's own playlist (a LONG file may 404 until the copy completes)", f.Path, err)
		return nil
	}
	return b
}

// planVideoFor computes the video-track plan for a video-bearing transcode Decision
// (ADR-0024): COPY the source video when the client can decode its codec at its
// resolution — a 4K HEVC on a HEVC-capable client is copied untouched, delivered as
// fMP4 — else re-encode to h264 scaled to the h264 ceiling. The copy ceiling is the
// SOURCE codec's device ceiling ∩ the session cap; the re-encode ceiling is h264's ∩
// the session cap (the re-encode always targets h264). A BURN forces a re-encode (you
// cannot overlay onto a copied stream), so a burn Decision never copies. Shared by
// transcodeJobPlan (the ffmpeg args) and Negotiate (setting Decision.VideoCopy for
// governance + playlist ownership), so both agree on whether the video is copied.
func planVideoFor(profile DeviceProfile, constraints Constraints, dec Decision) transcode.VideoPlan {
	f := dec.File
	// Judge the CHOSEN video Stream's own codec/height (falling back to the File-level
	// primary when a Stream value is missing), so the copy-vs-re-encode decision matches
	// the Stream negotiation actually delivers. On a multi-video File the resolved Stream
	// may not be the File's primary — an explicit videoStreamId can select an undecodable
	// co-packaged cut that must be re-encoded even though the primary is copyable, or a
	// decodable one that is copied even though the primary is not (selectable-video/02).
	// For a single-video File the Stream and File attributes agree, so this is unchanged.
	srcVideoCodec := firstNonEmpty(dec.VideoStream.Codec, f.VideoCodec)
	srcHeight := dec.VideoStream.Height
	if srcHeight == 0 {
		srcHeight = f.Height
	}
	srcSupport, clientSupportsSource := profile.videoCodec(srcVideoCodec)
	// A burn overlays onto the frames, so the video MUST be re-encoded, never copied.
	if dec.Burn != nil {
		clientSupportsSource = false
	}
	h264, _ := profile.videoCodec(transcode.VideoCodecH264)
	// The copy cap is the source codec's ceiling (a HEVC-capable client may take 4K
	// HEVC even though it only decodes h264 to 1080p); the re-encode cap is the h264
	// ceiling (we always output h264 when re-encoding). minPositive treats 0 as "no cap".
	copyMaxHeight := minPositive(resolutionHeight(constraints.MaxResolution), resolutionHeight(srcSupport.MaxResolution))
	reencodeMaxHeight := minPositive(resolutionHeight(constraints.MaxResolution), resolutionHeight(h264.MaxResolution))
	return transcode.PlanVideo(srcVideoCodec, srcHeight, f.Bitrate, clientSupportsSource, copyMaxHeight, reencodeMaxHeight, constraints.MaxBitrate)
}

// transcodeArgsBuilder binds a transcode plan to a specific encode backend and
// returns the per-(scratch dir, seek) ffmpeg-args builder. Taking the plan by value
// means each call captures its own copy, so the hardware builder and its CPU-
// fallback sibling never share mutable state.
// cutBoundaries, when non-nil, dictates the segment cut times per seek for a
// video-COPY transcode (see segmentTimesFor); nil for every re-encode.
func transcodeArgsBuilder(job transcode.TranscodeJob, accel transcode.Accel, cutBoundaries []float64) func(string, transcode.SeekOffset) []string {
	job.Accel = accel
	return func(outputDir string, seek transcode.SeekOffset) []string {
		job.OutputDir = outputDir
		job.Seek = seek
		job.SegmentTimes = segmentTimesFor(cutBoundaries, seek)
		return transcode.TranscodeArgs(job)
	}
}

// transcodeJobPlan resolves the per-track encode plan for a transcode Decision
// from the client's capabilities + constraints (everything but OutputDir, which
// the Manager fills once the scratch path is known). The binding resolution cap
// is the SESSION constraint (the per-request network/quality limit); the device's
// per-codec ceiling only matters when the device supports the target codec, which
// for our single h264 rendition we treat via the constraint. The video copy
// decision asks whether the client can take the source codec (h264) directly; the
// audio copy decision likewise for aac within the channel cap.
func transcodeJobPlan(profile DeviceProfile, constraints Constraints, dec Decision) transcode.TranscodeJob {
	f := dec.File

	// Audio-only Track (a Music Track): no video to plan — emit an audio-only
	// encode (FLAC/ALAC → AAC, downmixed past the channel cap). AudioOnly tells
	// TranscodeArgs to drop the video stream (-vn) entirely.
	if dec.AudioOnly {
		srcAudioCodec := firstNonEmpty(dec.AudioStream.Codec, f.AudioCodec)
		return transcode.TranscodeJob{
			SourcePath:       f.Path,
			AudioOnly:        true,
			HasAudio:         true,
			AudioStreamIndex: audioMapIndex(f, dec.AudioStream),
			Audio: transcode.PlanAudio(
				srcAudioCodec,
				dec.AudioStream.Channels,
				profile.supportsAudio(transcode.AudioCodecAAC),
				profile.MaxAudioChannels,
			),
		}
	}

	video := planVideoFor(profile, constraints, dec)

	hasAudio := dec.AudioStream.Kind == "audio" || dec.AudioStream.Codec != "" || f.AudioCodec != ""
	var audio transcode.AudioPlan
	if hasAudio {
		srcAudioCodec := firstNonEmpty(dec.AudioStream.Codec, f.AudioCodec)
		audio = transcode.PlanAudio(
			srcAudioCodec,
			dec.AudioStream.Channels,
			profile.supportsAudio(transcode.AudioCodecAAC),
			profile.MaxAudioChannels,
		)
	}

	job := transcode.TranscodeJob{
		SourcePath: f.Path,
		Video:      video,
		Audio:      audio,
		HasAudio:   hasAudio,
		// Pin the negotiated video Stream so the re-encoded/copied output carries the
		// resolved cut on a multi-video File — the reported video is the one FFmpeg maps
		// (nil for single-video → unchanged args, selectable-video/01). Composes with a
		// burn: the overlay graph reads the chosen video's `[0:v:N]` input.
		VideoStreamIndex: videoMapIndex(f, dec.VideoStream),
		// Pin the negotiated audio Stream so the re-encoded output carries exactly the
		// Stream the Decision reports on a multi-audio File (nil for single-audio →
		// unchanged args). Composes with a burn: the args builder maps the chosen audio
		// alongside the overlay output (audio-streams/02).
		AudioStreamIndex: audioMapIndex(f, dec.AudioStream),
		// Deliver fMP4 when the Decision says so (ADR-0024): a stream-copied non-h264
		// codec (HEVC) for a client that needs it in fMP4 (Apple's native player). An
		// hls.js client (HevcInMpegTS) takes copied HEVC over MPEG-TS instead; a
		// re-encode (to h264) or an h264 copy stays MPEG-TS regardless. UsesFMP4 is
		// the single authority so the job, the session runtime, and the synthesized
		// playlists always agree on the segment container.
		FMP4: dec.UsesFMP4(),
	}
	// Burn-in (subtitles/04): a selected image sub is overlaid onto the frames. The
	// args builder forces a re-encode on the CPU backend for it, so the plan's copy
	// decision is superseded — we only carry WHICH track to burn here. An EMBEDDED
	// track (StreamIndex >= 0) is overlaid from the source container itself, so it
	// needs no path; a SIDECAR track (StreamIndex -1) is added as a second input by
	// its path.
	if b := dec.Burn; b != nil {
		tb := &transcode.BurnSubtitle{StreamIndex: b.StreamIndex}
		if b.StreamIndex < 0 {
			tb.SidecarPath = b.Path
		}
		job.Burn = tb
	}
	return job
}

// minPositive returns the smaller of two caps where 0 means "no cap": if either
// is 0 the other binds; otherwise the smaller. Used to fold the session
// resolution constraint and the device's per-codec ceiling into one output cap.
func minPositive(a, b int) int {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

// ProgressOutcome reports what ReportProgress did with a position, so the api
// layer (and tests) can see the server-side threshold decision without re-deriving it.
type ProgressOutcome struct {
	TitleID          string
	ResumePositionMs int64 // resume now stored for the (User, Title); 0 when cleared/not recorded
	Watched          bool  // whether the Title is now marked watched
}

// ReportProgress applies a raw progress report against a session. It is the
// keepalive (Touches the session) AND the single place the Watched threshold is
// applied — SERVER-side, against the session File's DurationMs:
//
//   - position ≥ WatchedCeiling*duration → mark the Title watched, clear resume;
//   - position < StartedFloor*duration   → record no resume (leave it as-is for a
//     fresh Title; a below-floor stop never enters Continue Watching);
//   - otherwise                          → store the raw position as the resume.
//
// Ownership: the session must exist and belong to userID, else ErrSessionNotFound
// (404, hide existence) — a reaped/ended/foreign session cannot report progress.
// Concurrency is last-write-wins (the store upsert overwrites), so two Devices
// reporting against the same Title simply race to the latest position, no error.
//
// state ("playing|paused|buffering") is accepted for keepalive/observability but
// does not change the threshold math — position alone decides watched/resume.
// audioStreamID, when non-empty, records an in-band audio pick the web player made
// mid-session (audio-streams/05, ADR-0023): an in-band HLS audio switch is entirely
// client-side and never re-negotiates, so the player reports it here — alongside
// progress, on the same watch-state surface — to have it Remembered. It is resolved
// against the session's File and written best-effort; it never affects the resume /
// watched threshold math (that stays position-only) and an unknown id is ignored.
func (s *Service) ReportProgress(userID, sessionID string, positionMs int64, audioStreamID string) (ProgressOutcome, error) {
	sess, ok := s.sessions.Get(sessionID)
	if !ok || sess.UserID != userID {
		return ProgressOutcome{}, ErrSessionNotFound
	}
	if audioStreamID != "" {
		s.rememberSessionAudioPick(userID, sess, audioStreamID)
	}
	if positionMs < 0 {
		positionMs = 0
	}
	// Keepalive: a fresh progress report resets the idle clock so the reaper leaves
	// an actively-playing session alone. TouchProgress (not plain Touch) also emits
	// the observer's nowPlaying transition carrying this position — the manifest/
	// segment keepalives use plain Touch and carry no fresh position.
	s.sessions.TouchProgress(sessionID, positionMs)

	// Read current state so a below-floor report leaves an already-watched or
	// already-resuming Title untouched rather than wiping it.
	cur, err := s.watch.WatchStateFor(userID, sess.TitleID)
	if err != nil {
		return ProgressOutcome{}, err
	}

	out := ProgressOutcome{TitleID: sess.TitleID, ResumePositionMs: cur.ResumePositionMs, Watched: cur.Watched}

	// Without a known duration we cannot compute a percentage; fall back to
	// storing the raw position as the resume (best effort) and leave watched as-is.
	if sess.DurationMs <= 0 {
		out.ResumePositionMs = positionMs
		// played=true: every write on the progress path stamps played_at, the Up Next
		// anchor's recency (ADR-0028) — only the manual toggle leaves it untouched.
		if err := s.watch.SaveWatchState(userID, sess.TitleID, positionMs, cur.Watched, true); err != nil {
			return ProgressOutcome{}, err
		}
		return out, nil
	}

	frac := float64(positionMs) / float64(sess.DurationMs)
	switch {
	case frac >= WatchedCeiling:
		// Crossed the ceiling: watched, resume cleared so it leaves Continue Watching.
		out.Watched = true
		out.ResumePositionMs = 0
		if err := s.watch.SaveWatchState(userID, sess.TitleID, 0, true, true); err != nil {
			return ProgressOutcome{}, err
		}
	case frac < StartedFloor:
		// Below the floor: not started — record no resume. Leave any prior state
		// (a real resume or a watched flag) intact rather than clobbering it.
		// No write needed.
	default:
		// In-band: store the raw position as the resume, clearing any stale watched
		// flag so re-watching from the middle puts it back in Continue Watching.
		out.ResumePositionMs = positionMs
		out.Watched = false
		if err := s.watch.SaveWatchState(userID, sess.TitleID, positionMs, false, true); err != nil {
			return ProgressOutcome{}, err
		}
	}
	return out, nil
}

// rememberSessionAudioPick writes Remembered audio for an in-band pick reported
// through the progress surface: it resolves the picked audio Stream from the
// session's own File (so the stored meaning is real traits, not a bare id) and
// delegates to rememberAudioPick (Title memory + the Show bubble-up, commentary
// quarantined). Entirely best-effort — a missing Title/File/Stream is silently
// ignored so a stale or malformed id never fails a progress report.
func (s *Service) rememberSessionAudioPick(userID string, sess Session, audioStreamID string) {
	if s.audioMem == nil {
		return
	}
	detail, err := s.store.TitleByID(sess.TitleID)
	if err != nil {
		return
	}
	file, ok := fileByID(detail, sess.FileID)
	if !ok {
		return
	}
	stream, ok := audioStreamByID(file, audioStreamID)
	if !ok {
		return
	}
	s.rememberAudioPick(userID, sess.TitleID, stream)
}

// SetWatchState manually toggles watched/unwatched for a (User, Title),
// BYPASSING the Watched threshold (PUT /titles/{id}/watchState). Marking watched
// clears the resume (it leaves Continue Watching); marking unwatched clears both
// the watched flag and the resume (a clean "start over"). Last-write-wins.
func (s *Service) SetWatchState(userID, titleID string, watched bool) error {
	// Both directions zero the resume: watched means finished, unwatched means
	// start-over — neither carries a meaningful mid-file offset. played=false: a
	// manual mark is bookkeeping, not playback, so it never stamps played_at and
	// never moves the Up Next anchor (ADR-0028).
	return s.watch.SaveWatchState(userID, titleID, 0, watched, false)
}

// ReapIdle removes sessions idle past the timeout (the reaper's unit of work),
// returning how many were swept. The app's reaper goroutine calls it on a tick.
func (s *Service) ReapIdle(idle time.Duration) int {
	return s.sessions.Reap(idle)
}

// ErrNotHLS means the session exists but is direct-play — it has no HLS
// playlist/segments (the client should use the progressive stream URL). The api
// layer maps it to 404 on the HLS routes (a direct-play session has no /hls
// resource).
var ErrNotHLS = errors.New("playback: session is not an HLS (remux/transcode) session")

// HLSPlaylist returns the bytes of the session's HLS media playlist
// (index.m3u8), lazily starting the remux on the first call and briefly waiting
// for ffmpeg to write the initial playlist. Ownership is enforced (the session
// must exist and belong to userID, else ErrSessionNotFound → 404, hide
// existence); a direct-play session yields ErrNotHLS. A successful read also
// Touches the session (the manifest fetch is keepalive for an actively-playing
// HLS stream).
func (s *Service) HLSPlaylist(userID, sessionID string) ([]byte, error) {
	rt, err := s.hlsRuntimeFor(userID, sessionID)
	if err != nil {
		return nil, err
	}
	if err := rt.EnsureStarted(); err != nil {
		return nil, err
	}
	s.sessions.Touch(sessionID)
	// The playlist is server-owned for transcode (synthesized from the File
	// duration, stable across seek realignment) and ffmpeg's own for remux (which
	// appears once ffmpeg has written its first segment; the runtime waits briefly
	// rather than 404 a manifest the client just asked for).
	return rt.playlist()
}

// HLSSegment returns the bytes of a named HLS segment from the session scratch,
// lazily starting the remux if needed and briefly waiting for the segment ffmpeg
// is still writing (a segment the playlist lists but has not flushed yet must not
// hard-404). name is a single path element (no separators) — the api layer has
// already rejected traversal. Ownership/HLS checks mirror HLSPlaylist.
func (s *Service) HLSSegment(userID, sessionID, name string) ([]byte, error) {
	rt, err := s.hlsRuntimeFor(userID, sessionID)
	if err != nil {
		return nil, err
	}
	if err := rt.EnsureStarted(); err != nil {
		return nil, err
	}
	s.sessions.Touch(sessionID)
	// The runtime serves the segment from scratch, realigning the ffmpeg job when the
	// requested segment is ahead of what it has produced (a transcode forward seek,
	// ADR-0004) so the segment is generated near its timestamp.
	return rt.segment(name)
}

// SessionSubtitleContext is what the api layer needs to serve an HLS session's
// in-band subtitle artifacts (ADR-0020, slice 03): the master playlist's
// renditions, each rendition's segmented WebVTT, and the subtitle media playlist.
// Tracks is the played File's full Subtitle-track list (the same set the decision
// offered — embedded Streams + the Title's Sidecar/Fetched rows); the api layer
// filters it to the deliverable text tracks for the master's SUBTITLES group.
// Detail lets the api layer locate + convert one track to WebVTT (reusing the
// out-of-band conversion path). DurationMs sizes the segmented subtitle playlist
// to the video cadence.
type SessionSubtitleContext struct {
	Detail     store.TitleDetail
	Tracks     []SubtitleTrack
	DurationMs int64
	// ScratchDir is the session's HLS scratch directory (ADR-0007), where the api
	// layer caches each track's whole-file WebVTT so segmenting it does not re-run
	// the (embedded) ffmpeg extraction on every segment request. Removed with the
	// session, so the cache never outlives the stream.
	ScratchDir string
}

// SessionSubtitleContext resolves the owner-checked subtitle context for an HLS
// session: the played File's Subtitle tracks, the Title detail (for on-demand
// WebVTT conversion), and the File duration (for segmenting). It enforces the
// same ownership + HLS gates as the media-playlist path — ErrSessionNotFound for
// an unknown/foreign/ended session (404, hide existence), ErrNotHLS for a direct-
// play session (which has no master playlist). A missing Title/File is a genuine
// error the api layer renders as 404 (the session referenced media that is gone).
func (s *Service) SessionSubtitleContext(userID, sessionID string) (SessionSubtitleContext, error) {
	sess, ok := s.sessions.Get(sessionID)
	if !ok || sess.UserID != userID {
		return SessionSubtitleContext{}, ErrSessionNotFound
	}
	if sess.Tier == TierDirectPlay {
		// Direct play delivers subtitles out-of-band (a <track>), not via a master
		// playlist — the in-band artifacts don't exist for it.
		return SessionSubtitleContext{}, ErrNotHLS
	}
	detail, err := s.store.TitleByID(sess.TitleID)
	if err != nil {
		return SessionSubtitleContext{}, err
	}
	file, ok := fileByID(detail, sess.FileID)
	if !ok {
		return SessionSubtitleContext{}, store.ErrNotFound
	}
	return SessionSubtitleContext{
		Detail:     detail,
		Tracks:     buildSubtitleTracks(file, detail.Subtitles),
		DurationMs: sess.DurationMs,
		ScratchDir: sess.ScratchDir,
	}, nil
}

// AudioRenditionInfo is one demuxed in-band audio Stream the api layer advertises
// in the master playlist's AUDIO group (audio-streams/03): the id selects the
// rendition's media playlist (audio_<id>.m3u8), Label is the menu string, Language
// is the ISO-639-1 code, and Default marks the resolved default audio (the master's
// DEFAULT=YES rendition — the track a native HLS player turns on).
type AudioRenditionInfo struct {
	StreamID string
	Label    string
	Language string
	Default  bool
}

// SessionAudioContext is what the api layer needs to serve an HLS session's demuxed
// in-band audio artifacts (audio-streams/03): whether the session is demuxed at all
// (multi-audio → a master playlist with an AUDIO group), and the per-Stream rendition
// list for that group. A single-audio (or direct-play/audio-only) session reports
// Demuxed=false with no renditions — its audio stays muxed in the video variant.
type SessionAudioContext struct {
	Demuxed    bool
	Renditions []AudioRenditionInfo
	// FMP4 is true when the session delivers fragmented-MP4 (a copied-HEVC video —
	// remux or video-copy transcode, ADR-0024). The api layer uses it to advertise a
	// CODECS attribute on the master's video variant so Safari accepts the HEVC.
	FMP4 bool
	// VideoCodec is the played File's video codec (e.g. "hevc"), so the api can build
	// the CODECS string. "" for a silent/odd File.
	VideoCodec string
	// Bandwidth/Width/Height describe the video variant for the master's
	// #EXT-X-STREAM-INF (the File's own bitrate and dimensions — a copied video
	// delivers the source stream, so they are the honest values).
	Bandwidth     int64
	Width, Height int
	// VideoRange ("PQ"/"HLG", "" for SDR) and FrameRate are probed from the source
	// per session (transcode.ProbeVideoTraits): Safari REQUIRES an honest
	// VIDEO-RANGE (an HDR stream under an implicitly-SDR variant is killed with a
	// bare decode error) and will not load a PQ variant without RESOLUTION +
	// FRAME-RATE. Zero values omit the attributes (correct for SDR).
	VideoRange string
	FrameRate  float64
}

// SessionAudioContext resolves the owner-checked demuxed-audio context for an HLS
// session, mirroring SessionSubtitleContext's ownership + HLS gates
// (ErrSessionNotFound for unknown/foreign/ended, ErrNotHLS for direct play). It
// loads the played File and, when it carries 2+ audio Streams, builds the rendition
// list (labels + the resolved default). A missing Title/File is a store.ErrNotFound
// the api renders as 404.
func (s *Service) SessionAudioContext(userID, sessionID string) (SessionAudioContext, error) {
	sess, ok := s.sessions.Get(sessionID)
	if !ok || sess.UserID != userID {
		return SessionAudioContext{}, ErrSessionNotFound
	}
	if sess.Tier == TierDirectPlay {
		return SessionAudioContext{}, ErrNotHLS
	}
	detail, err := s.store.TitleByID(sess.TitleID)
	if err != nil {
		return SessionAudioContext{}, err
	}
	file, ok := fileByID(detail, sess.FileID)
	if !ok {
		return SessionAudioContext{}, store.ErrNotFound
	}
	if audioStreamCount(file) < 2 {
		// Single-audio (or silent) File: muxed, no AUDIO group.
		return SessionAudioContext{Demuxed: false}, nil
	}
	var rends []AudioRenditionInfo
	for _, st := range file.Streams {
		if st.Kind != "audio" {
			continue
		}
		lang := audio.NormalizeLang(st.Language)
		rends = append(rends, AudioRenditionInfo{
			StreamID: st.ID,
			Label:    audio.Label(lang, st.Channels, st.Title, st.Commentary),
			Language: lang,
			// The resolved default audio Stream (what negotiation picked) is the master's
			// DEFAULT=YES rendition; fall back to the File's default disposition if the
			// session recorded no resolved id (defensive — a demuxed session always has one).
			Default: (sess.AudioStreamID != "" && st.ID == sess.AudioStreamID) ||
				(sess.AudioStreamID == "" && st.IsDefault),
		})
	}
	// fMP4 delivery (ADR-0024): the Session records the Decision's authoritative
	// container choice (which also weighs the CLIENT — an hls.js client takes copied
	// HEVC over MPEG-TS instead, HevcInMpegTS). The api reads this to advertise the
	// CODECS attribute the fMP4 HEVC variant needs.
	videoCodec := file.VideoCodec
	width, height := file.Width, file.Height
	if v, ok := pickVideoStream(file); ok {
		videoCodec = firstNonEmpty(file.VideoCodec, v.Codec)
		if width == 0 || height == 0 {
			width, height = v.Width, v.Height
		}
	}
	ctx := SessionAudioContext{
		Demuxed:    true,
		Renditions: rends,
		FMP4:       sess.FMP4,
		VideoCodec: videoCodec,
		Bandwidth:  file.Bitrate,
		Width:      width,
		Height:     height,
	}
	// Probe the video-range/frame-rate traits Safari's master validation needs (a
	// header-only ffprobe; the scan-time store predates these fields). Best-effort:
	// a failure degrades to omitting the attributes — but for an HDR stream that
	// means Safari will refuse the variant, so the failure is logged.
	if videoCodec != "" && file.Path != "" {
		tctx, cancel := context.WithTimeout(context.Background(), keyframeProbeTimeout)
		traits, terr := transcode.ProbeVideoTraits(tctx, "", file.Path)
		cancel()
		if terr != nil {
			log.Printf("juicebox: playback: video-traits probe failed for %s: %v — master omits VIDEO-RANGE/FRAME-RATE (Safari may refuse an HDR variant)", file.Path, terr)
		} else {
			ctx.VideoRange = traits.VideoRange
			ctx.FrameRate = traits.FrameRate
		}
	}
	return ctx, nil
}

// HLSAudioRendition serves a demuxed audio rendition's media playlist
// (audio-streams/03), lazily starting that rendition's ffmpeg job on first request.
// It enforces ownership + the HLS gate (via SessionAudioContext) and validates that
// streamID names a real audio Stream of the played File — an id that does not, or a
// non-demuxed session, yields ErrNoAudioRendition (404, hide existence). A successful
// read Touches the session (keepalive), like the video playlist.
func (s *Service) HLSAudioRendition(userID, sessionID, streamID string) ([]byte, error) {
	rt, err := s.audioRuntimeFor(userID, sessionID, streamID)
	if err != nil {
		return nil, err
	}
	if err := rt.EnsureStarted(); err != nil {
		return nil, err
	}
	s.sessions.Touch(sessionID)
	return rt.playlist()
}

// HLSAudioSegment serves one segment of a demuxed audio rendition, lazily starting
// the rendition's job if needed and briefly waiting for a segment ffmpeg is still
// flushing. Ownership/validation mirror HLSAudioRendition.
func (s *Service) HLSAudioSegment(userID, sessionID, streamID, name string) ([]byte, error) {
	rt, err := s.audioRuntimeFor(userID, sessionID, streamID)
	if err != nil {
		return nil, err
	}
	if err := rt.EnsureStarted(); err != nil {
		return nil, err
	}
	s.sessions.Touch(sessionID)
	return rt.segment(name)
}

// audioRuntimeFor resolves the owner-checked, validated lazy rendition runtime for
// (session, audio Stream): it confirms the session is a demuxed multi-audio HLS
// session that carries streamID (else ErrNoAudioRendition → 404), then ensures the
// runtime. Validating against the File's audio Streams keeps an arbitrary id from
// spinning up an ffmpeg job.
func (s *Service) audioRuntimeFor(userID, sessionID, streamID string) (*hlsRuntime, error) {
	sctx, err := s.SessionAudioContext(userID, sessionID)
	if err != nil {
		return nil, err
	}
	if !sctx.Demuxed || !audioContextHasStream(sctx, streamID) {
		return nil, ErrNoAudioRendition
	}
	return s.sessions.EnsureAudioRuntime(sessionID, streamID)
}

// audioContextHasStream reports whether streamID is one of the context's renditions.
func audioContextHasStream(sctx SessionAudioContext, streamID string) bool {
	for _, r := range sctx.Renditions {
		if r.StreamID == streamID {
			return true
		}
	}
	return false
}

// fileByID finds the played File within a Title detail by its id (the session
// records which File it negotiated). Returns false when no File matches — a File
// that vanished between negotiation and this request.
func fileByID(detail store.TitleDetail, fileID string) (store.File, bool) {
	for _, ed := range detail.Editions {
		for _, f := range ed.Files {
			if f.ID == fileID {
				return f, true
			}
		}
	}
	return store.File{}, false
}

// hlsRuntimeFor resolves the owner-checked HLS runtime for a session, or the
// appropriate sentinel error (ErrSessionNotFound for unknown/foreign, ErrNotHLS
// for a direct-play session).
func (s *Service) hlsRuntimeFor(userID, sessionID string) (*hlsRuntime, error) {
	sess, ok := s.sessions.Get(sessionID)
	if !ok || sess.UserID != userID {
		return nil, ErrSessionNotFound
	}
	rt, ok := s.sessions.remuxRuntimeFor(sessionID)
	if !ok {
		return nil, ErrNotHLS
	}
	return rt, nil
}
