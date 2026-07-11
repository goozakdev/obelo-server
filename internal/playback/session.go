package playback

import (
	"errors"
	"path/filepath"
	"sync"
	"time"

	"github.com/marioquake/juicebox/internal/transcode"
	"github.com/google/uuid"
)

// Session is one active Playback session: a single stream from the server to one
// client (CONTEXT.md). It records the negotiated tier and enough about the chosen
// File to serve the progressive stream and to enforce ownership — only the User
// (and Device) that created a session may stream it.
//
// Sessions are ephemeral and live only in memory (the Manager); they are NOT
// persisted to SQLite — a session does not survive a server restart, and that is
// correct: a client whose session vanished simply re-negotiates. This matches the
// "in-memory session manager" decision in the issue.
//
// Issue 08 hook: watch-state/progress/keepalive/reaping attach here. LastSeen is
// already stamped on create and refreshed by Touch so a future reaper can sweep
// sessions idle for more than N seconds without reshaping this record; issue 08
// adds a positionMs/state field and the reaper goroutine.
type Session struct {
	ID         string
	UserID     string
	DeviceID   string
	TitleID    string
	EditionID  string
	FileID     string
	FilePath   string
	DurationMs int64 // chosen File's duration; the Watched threshold measures positionMs against it (issue 08)
	Tier       Tier
	// VideoCopy marks a TierTranscode session that copies the video and transcodes
	// only the audio (ADR-0024). It was NOT counted against the transcode cap (no
	// video encode), so End/Reap must not decrement the counter for it — the release
	// is gated on `Tier == TierTranscode && !VideoCopy`, mirroring the create-time
	// metering.
	VideoCopy bool
	// FMP4 is true when the session delivers fragmented-MP4 (a copied HEVC video,
	// ADR-0024). The demuxed audio-rendition runtimes read it to name their .m4s
	// segments + init segment so they share the video variant's container.
	FMP4 bool
	// AudioStreamID is the id of the resolved audio Stream the Decision reported (the
	// negotiated default: preferred language → default disposition → first). For a
	// DEMUXED multi-audio session it marks which in-band rendition the master playlist
	// flags DEFAULT=YES — the track a native HLS player turns on unless the viewer
	// picks another (audio-streams/03). "" for a single-audio / silent File.
	AudioStreamID string
	StartPosition int64 // ms; the requested resume offset (informational this slice)
	CreatedAt     time.Time
	// LastSeen is refreshed on create and by Touch; the issue-08 reaper uses it as
	// the idle-since signal — a session silent past the idle timeout is reaped.
	LastSeen time.Time
	// ScratchDir is the session's HLS scratch directory under the data dir
	// (ADR-0007), set for directStream/transcode sessions and empty for direct
	// play (which streams the File's bytes and needs no scratch). The Manager
	// creates it on Create and removes it (with any ffmpeg output) on End/Reap.
	ScratchDir string
}

// SessionEventKind is the closed set of Playback session lifecycle transitions an
// observer is notified of. It is a small enum — NOT an events-package import — so
// playback stays transport-agnostic: app.New translates these into the realtime
// Broker's Admin-only session events (started → sessionStarted, nowPlaying →
// nowPlaying, ended → sessionEnded). Keeping the seam a plain callback is the same
// decoupling enrich uses for onProgress.
type SessionEventKind int

const (
	// SessionStarted fires once when a session is created (CreateGoverned, the
	// common path Create delegates to). A rejected create (ErrTranscodeCapFull,
	// no session minted) does NOT fire it.
	SessionStarted SessionEventKind = iota
	// SessionNowPlaying fires on a progress report (TouchProgress) and carries the
	// reported PositionMs. Plain Touch (manifest/segment keepalives) does NOT fire
	// it — a keepalive has no fresh position.
	SessionNowPlaying
	// SessionEnded fires when a session leaves: a clean End that actually removed
	// one, and every session swept by Reap (the idle-reap path).
	SessionEnded
)

// SessionEvent is one session-lifecycle notification handed to the Manager's
// observer. It carries enough identity to correlate started → nowPlaying → ended
// (SessionID) and render a live row (UserID, TitleID); PositionMs is set only for
// SessionNowPlaying.
type SessionEvent struct {
	Kind       SessionEventKind
	SessionID  string
	UserID     string
	TitleID    string
	PositionMs int64
}

// Manager is the in-memory store of active Playback sessions: a map guarded by a
// mutex. It is the lifecycle owner — Create mints a session from a Decision, Get
// looks one up (for the stream endpoint), and End frees it (the clean stop,
// DELETE /sessions/{id}). It deliberately holds no DB handle: sessions are
// process-local and ephemeral.
type Manager struct {
	mu       sync.Mutex
	sessions map[string]Session
	runtimes map[string]*hlsRuntime // per-session VIDEO HLS runtime; only for directStream/transcode
	// audioRuntimes holds the LAZY per-(session, audio Stream) rendition runtimes of a
	// DEMUXED multi-audio session (audio-streams/03): sessionID → streamID → runtime,
	// each created on the first request for that rendition's playlist/segment and
	// writing its namespaced files into the session's shared scratch dir. audioBuilders
	// holds the per-session ffmpeg-args builder that mints them (closed over the
	// client profile so it can decide copy-vs-AAC per Stream); a session that is not a
	// demuxed multi-audio HLS session has no entry, so it exposes no audio renditions.
	audioRuntimes map[string]map[string]*hlsRuntime
	audioBuilders map[string]func(streamID, outputDir string, seek transcode.SeekOffset) []string
	now           func() time.Time // injectable clock; defaults to time.Now
	// observer, when set, is notified of session lifecycle transitions (started/
	// nowPlaying/ended). It is the seam app.New wires to the realtime Broker so
	// playback imports no events package; nil (the default, and every playback unit
	// test) is a no-op. Set once at boot via SetObserver and invoked OUTSIDE m.mu so
	// a publish never blocks a session operation.
	observer func(SessionEvent)

	// runner is the seam used to lazily start the ffmpeg job (remux OR transcode)
	// for a directStream/transcode session on its first manifest/segment request;
	// scratchRoot is the base dir under which each session's scratch subdir is
	// created. Both are nil/empty for a direct-play-only Manager (the issue-07 unit
	// tests build one with NewManager).
	runner      transcode.Runner
	scratchRoot string

	// transcodeCap is the global concurrent-transcode cap (ADR-0009 governance).
	// activeTranscodes counts live TierTranscode sessions ONLY — directPlay and
	// directStream (remux) never increment it, so they are never blocked. A cap of
	// 0 (or negative) means unlimited: the metering still counts for observability
	// but Create never rejects. The counter is incremented atomically with map
	// insertion in Create and decremented when a transcode session leaves via
	// End/Reap, so a freed slot reliably lets a previously-rejected transcode in.
	transcodeCap     int
	activeTranscodes int
}

// NewManager builds an empty session Manager with no HLS runtime — direct play
// only (issue 07). directStream/transcode sessions need NewRemuxManager.
func NewManager() *Manager {
	return &Manager{
		sessions:      make(map[string]Session),
		runtimes:      make(map[string]*hlsRuntime),
		audioRuntimes: make(map[string]map[string]*hlsRuntime),
		audioBuilders: make(map[string]func(streamID, outputDir string, seek transcode.SeekOffset) []string),
		now:           time.Now,
	}
}

// NewRemuxManager builds a Manager that can back directStream (remux) AND
// transcode sessions: runner starts the ffmpeg HLS job lazily, and each session's
// scratch lives in a fresh subdir under scratchRoot (ADR-0007). Direct-play
// sessions still need no scratch and run unchanged. (Named for the slice that
// introduced it; it now backs both HLS tiers since their delivery is identical.)
func NewRemuxManager(runner transcode.Runner, scratchRoot string) *Manager {
	m := NewManager()
	m.runner = runner
	m.scratchRoot = scratchRoot
	return m
}

// SetTranscodeCap sets the global concurrent-transcode cap (ADR-0009). A value
// <= 0 means unlimited (no rejection). The Service wires the config knob through
// here at construction; tests set a small cap (e.g. 1) to exercise the busy
// path. It is set once at boot before any session exists, so it needs no lock.
func (m *Manager) SetTranscodeCap(n int) { m.transcodeCap = n }

// ErrTranscodeCapFull is returned by Create when a TierTranscode session would
// exceed the configured concurrent-transcode cap (ADR-0009: reject-don't-queue).
// The Service translates it into a ServerBusy negotiation outcome, which the api
// layer renders as 503 SERVER_BUSY. Direct play and remux never trigger it.
var ErrTranscodeCapFull = errors.New("playback: transcode concurrency cap reached")

// CreateInput is what Create needs beyond the negotiated Decision: who owns the
// session and where they want to start.
type CreateInput struct {
	UserID        string
	DeviceID      string
	TitleID       string
	StartPosition int64
	// BuildHLSArgs builds the ffmpeg argument vector for a directStream/transcode
	// session, given the session scratch dir as the ffmpeg output dir AND a seek
	// offset — RemuxArgs for remux, TranscodeArgs for transcode. The seek offset is
	// the realignment hook (ADR-0004): the zero value is the from-the-top launch, a
	// non-zero offset restarts ffmpeg near a sought timestamp. The Service supplies
	// it (it holds the profile/constraints needed to plan a transcode); the runtime
	// calls it on the first launch and again on each realignment. Nil for a
	// direct-play session (no HLS runtime). Taking the output dir as a parameter
	// resolves the chicken-and-egg of the scratch path depending on the
	// not-yet-minted session id.
	BuildHLSArgs func(outputDir string, seek transcode.SeekOffset) []string
	// BuildHLSArgsCPU, when non-nil, is the CPU (libx264) variant of BuildHLSArgs
	// used for the single per-session hardware→CPU fallback (ADR-0009): if the
	// hardware ffmpeg job fails to launch, the runtime restarts ONCE with these
	// args in the SAME session, scratch dir, and transcode cap slot. The Service
	// sets it ONLY for a transcode whose configured backend is hardware
	// (transcode.IsHardware); remux and an already-CPU transcode leave it nil — they
	// are not fallback-eligible, so a launch failure there surfaces as an honest
	// error rather than a pointless restart on the identical path. It is the exact
	// same builder as BuildHLSArgs with the encode backend forced to AccelCPU, so
	// the fallback reuses every other plan decision (scale/bitrate/audio/seek).
	BuildHLSArgsCPU func(outputDir string, seek transcode.SeekOffset) []string
	// BuildAudioRenditionArgs, when non-nil, builds the ffmpeg args for ONE demuxed
	// audio rendition (audio-streams/03): given an audio Stream id, the shared scratch
	// dir, and a seek offset, it returns the AudioRenditionArgs vector (copy-or-AAC
	// decided inside, closed over the client profile). The Manager stores it and calls
	// it lazily on the first request for a rendition to mint that rendition's runtime.
	// It is set ONLY for a DEMUXED multi-audio HLS session; a single-audio or
	// direct-play session leaves it nil and exposes no audio renditions (the video
	// variant stays muxed). The scratch dir is passed at call time (same reason as
	// BuildHLSArgs) so it can be bound after the session id — hence the scratch path —
	// is known.
	BuildAudioRenditionArgs func(streamID, outputDir string, seek transcode.SeekOffset) []string
	// SegmentBoundaries, when non-empty, is the EXACT segment boundary times (seconds)
	// of a video-COPY session (a remux, or a video-copy transcode), computed from the
	// source's keyframes (transcode.SegmentBoundaries). The video runtime synthesizes
	// its media playlist from these — appearing immediately and matching ffmpeg's
	// irregular copy segments — instead of serving ffmpeg's own VOD playlist, which a
	// copy writes only when the whole (feature-length) input is finished. Nil for a
	// re-encode transcode (uniform segments, synthesized from the duration) and left
	// nil when the keyframe probe failed (the runtime then falls back to ffmpeg's
	// playlist — correct for short files).
	SegmentBoundaries []float64
}

// Create records a new Session for a Decision and returns it (the unmetered
// helper used by the direct-play/remux unit tests). It is governance-free: it
// panics nothing and never rejects, because direct play and remux are never
// metered. For metered creation that can return ErrTranscodeCapFull, the Service
// calls CreateGoverned. Create is a thin wrapper that drops the (always-nil for
// these tiers, and nil whenever no cap is set) error.
func (m *Manager) Create(in CreateInput, d Decision) Session {
	s, _ := m.CreateGoverned(in, d)
	return s
}

// CreateGoverned records a new Session for a Decision, ENFORCING the transcode
// cap (ADR-0009). The session id is a fresh UUID; the caller turns it into the
// session-scoped streamUrl. For a directStream/transcode Decision it allocates
// (but does not yet populate) the session's HLS scratch directory under
// scratchRoot and registers a runtime so the first manifest/segment request can
// lazily start ffmpeg; direct play gets no scratch.
//
// Metering (ADR-0009): only a TierTranscode session counts against the cap. When
// the tier is transcode and the cap is already full, it returns
// (zero, ErrTranscodeCapFull) WITHOUT creating a session — reject-don't-queue.
// Direct play and remux are never metered and never rejected. The check and the
// counter increment happen under the same lock as the map insertion, so two
// concurrent transcode requests cannot both slip past a cap of 1.
func (m *Manager) CreateGoverned(in CreateInput, d Decision) (Session, error) {
	now := m.now()
	id := uuid.NewString()
	s := Session{
		ID:            id,
		UserID:        in.UserID,
		DeviceID:      in.DeviceID,
		TitleID:       in.TitleID,
		EditionID:     d.Edition.ID,
		FileID:        d.File.ID,
		FilePath:      d.File.Path,
		DurationMs:    d.File.DurationMs,
		Tier:          d.Tier,
		VideoCopy:     d.VideoCopy,
		FMP4:          d.UsesFMP4(),
		AudioStreamID: d.AudioStream.ID,
		StartPosition: in.StartPosition,
		CreatedAt:     now,
		LastSeen:      now,
	}
	var rt *hlsRuntime
	if d.Tier != TierDirectPlay && m.runner != nil && m.scratchRoot != "" {
		s.ScratchDir = filepath.Join(m.scratchRoot, id)
		// Bind the scratch path into the per-seek args builder now that it is known.
		// The runtime calls buildArgs on the first launch (zero seek) and again on
		// each realignment (a non-zero seek), so it can reposition ffmpeg without the
		// Manager knowing ffmpeg specifics. A nil BuildHLSArgs (defensive) yields an
		// empty arg vector.
		build := in.BuildHLSArgs
		buildArgs := func(seek transcode.SeekOffset) []string {
			if build == nil {
				return nil
			}
			return build(s.ScratchDir, seek)
		}
		// The CPU-fallback builder (issue 03) is bound to the same scratch path. It
		// stays nil unless the Service marked this session hardware-encoded, in which
		// case the runtime may restart ONCE on it after a HW launch failure — in the
		// same scratch dir and the same cap slot this session already holds.
		var buildCPUArgs func(transcode.SeekOffset) []string
		if cpu := in.BuildHLSArgsCPU; cpu != nil {
			buildCPUArgs = func(seek transcode.SeekOffset) []string {
				return cpu(s.ScratchDir, seek)
			}
		}
		rt = &hlsRuntime{
			runner:         m.runner,
			buildArgs:      buildArgs,
			buildCPUArgs:   buildCPUArgs,
			scratchDir:     s.ScratchDir,
			segmentSeconds: transcode.SegmentSeconds,
		}
		// fMP4 sessions (a copied HEVC video) name segments .m4s + an init segment; TS
		// sessions use .ts. Derived from the Decision so the synthesized playlist matches
		// what ffmpeg writes.
		segFmt := transcode.SegmentPattern
		initSeg := ""
		if d.UsesFMP4() {
			segFmt = transcode.SegmentPatternFMP4
			initSeg = transcode.InitSegmentName
		}
		// Who owns (synthesizes) the media playlist, and how:
		//   - A video RE-ENCODE forces a keyframe every SegmentSeconds, so its segments
		//     are exactly uniform: own a UNIFORM playlist and REALIGN ffmpeg on a forward
		//     seek (re-encoding is expensive — never re-encode everything before the seek).
		//   - A video COPY / REMUX cannot force keyframes, so its segments fall on the
		//     source's irregular keyframes. Own an EXACT playlist synthesized from the
		//     probed boundaries (transcode.SegmentBoundaries); the TS copy job CUTS AT
		//     those same times (segment muxer -segment_times), so segments match the
		//     playlist BY CONSTRUCTION — including across a seek REALIGNMENT, which
		//     restarts ffmpeg at the target segment's exact boundary (a deep seek must
		//     not wait minutes for a linear copy to catch up). The segment muxer writes
		//     in place, so serving is sequential-gated. An fMP4 (HEVC) copy keeps the
		//     hls muxer (the segment muxer cannot emit CMAF init+m4s) and REALIGNS the
		//     same way — without it a resume/seek beyond the production frontier waits
		//     on a linear ~realtime copy of a huge file and the player times out and
		//     never starts. Its temp_file renames are atomic, so serving needs no
		//     successor gate; and because its post-seek cuts follow the muxer's own
		//     keyframe grid rather than dictated times, EXTINF drift vs the synthesized
		//     playlist is possible but bounded (<~1 segment) and non-compounding, while
		//     timestamps stay exact via -output_ts_offset — which is what MSE/Safari
		//     actually key on. Without probed boundaries (probe failed) the runtime
		//     serves ffmpeg's own playlist — the short-file path, unchanged.
		reencodeVideo := d.Tier == TierTranscode && !d.AudioOnly && !d.VideoCopy
		copyVideo := (d.VideoCopy || d.Tier == TierDirectStream) && !d.AudioOnly
		switch {
		case reencodeVideo:
			rt.ownsPlaylist = true
			rt.realignable = true
			rt.segmentCount = segmentCountFor(d.File.DurationMs, transcode.SegmentSeconds)
		case copyVideo && len(in.SegmentBoundaries) > 1:
			rt.ownsPlaylist = true
			rt.boundaries = in.SegmentBoundaries
			rt.segmentCount = len(in.SegmentBoundaries) - 1
			rt.segNameFmt = segFmt
			rt.initSegment = initSeg
			rt.realignable = true
			// TS copy only: the segment muxer writes in place → sequential-gated
			// serving. The fMP4 hls muxer renames atomically (temp_file) — no gate.
			rt.gateSequential = !d.UsesFMP4()
		default:
			// Direct-play never reaches here (no runtime); a copy/remux with no probed
			// boundaries serves ffmpeg's own playlist (the short-file path, unchanged).
			rt.ownsPlaylist = false
		}
	}
	m.mu.Lock()
	// Cap check + reservation under the lock, so the count cannot change between
	// "is there room?" and "take the slot". Only a VIDEO-ENCODING transcode is
	// metered: a video-copy transcode (ADR-0024) re-encodes only the audio (a few
	// percent of a core, like the in-session audio renditions ADR-0022 exempts), so
	// it is unmetered like remux — the cap exists to bound video encodes saturating
	// the host, which a copy is not.
	if d.Tier == TierTranscode && !d.VideoCopy {
		if m.transcodeCap > 0 && m.activeTranscodes >= m.transcodeCap {
			m.mu.Unlock()
			return Session{}, ErrTranscodeCapFull
		}
		m.activeTranscodes++
	}
	m.sessions[s.ID] = s
	if rt != nil {
		m.runtimes[s.ID] = rt
	}
	// A demuxed multi-audio HLS session carries an audio-rendition builder; register
	// it (plus an empty runtime map) so the first request for any rendition can mint
	// its lazy runtime. Left absent for single-audio / direct-play sessions, which
	// expose no renditions.
	if rt != nil && in.BuildAudioRenditionArgs != nil {
		m.audioBuilders[s.ID] = in.BuildAudioRenditionArgs
		m.audioRuntimes[s.ID] = make(map[string]*hlsRuntime)
	}
	m.mu.Unlock()
	// started fires HERE — the single common create path Create delegates to — so a
	// session is announced exactly once. The cap-rejection path above returned
	// without minting a session, so it correctly does not reach this.
	m.notify(SessionEvent{Kind: SessionStarted, SessionID: s.ID, UserID: s.UserID, TitleID: s.TitleID})
	return s, nil
}

// segmentCountFor computes how many HLS segments cover a File of durationMs at
// segmentSeconds each — ceil(duration / segLen). It is the length of the
// server-owned (transcode) playlist: a stable, complete VOD segment list the
// runtime can hand the player up front, so a seek to any listed segment is well-
// defined and a realignment never changes what the player sees. An unknown
// duration (<= 0) yields 0 segments; the runtime then falls back to serving
// whatever ffmpeg produces without bounding the index.
func segmentCountFor(durationMs int64, segmentSeconds int) int {
	if durationMs <= 0 || segmentSeconds <= 0 {
		return 0
	}
	segMs := int64(segmentSeconds) * 1000
	return int((durationMs + segMs - 1) / segMs)
}

// remuxRuntimeFor returns the HLS runtime for a session, or (nil, false) when the
// id is unknown or the session is direct-play (no runtime). The api layer uses it
// to lazily start the remux and locate the scratch dir for serving segments.
func (m *Manager) remuxRuntimeFor(id string) (*hlsRuntime, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rt, ok := m.runtimes[id]
	return rt, ok
}

// ErrNoAudioRendition means the session exposes no demuxed audio rendition for the
// requested Stream — the session is not a demuxed multi-audio HLS session, or the
// id names no audio Stream it carries. The Service maps it to a 404 on the rendition
// routes (hide existence), the same posture as an unknown segment.
var ErrNoAudioRendition = errors.New("playback: no audio rendition for stream")

// EnsureAudioRuntime returns the LAZY per-(session, audio Stream) rendition runtime
// for a demuxed multi-audio session (audio-streams/03), creating and registering it
// on first request. The runtime writes its namespaced audio_<streamID>.m3u8 +
// audio_<streamID>_NNN.ts into the SESSION's shared scratch dir (sharedScratch, so
// its teardown never removes the dir), serving ffmpeg's OWN playlist (ownsPlaylist
// false, no realignment — an audio encode is faster than realtime, so the whole
// rendition is produced up front and a seek reads an already-written segment). It
// returns ErrNoAudioRendition when the session is not a demuxed multi-audio HLS
// session (no builder registered) — the Service has already validated that streamID
// names a real audio Stream of the played File before calling.
func (m *Manager) EnsureAudioRuntime(sessionID, streamID string) (*hlsRuntime, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	build, ok := m.audioBuilders[sessionID]
	if !ok || build == nil {
		return nil, ErrNoAudioRendition
	}
	if rt, ok := m.audioRuntimes[sessionID][streamID]; ok {
		return rt, nil
	}
	sess, ok := m.sessions[sessionID]
	if !ok || sess.ScratchDir == "" {
		return nil, ErrNoAudioRendition
	}
	scratch := sess.ScratchDir
	// The rendition SYNTHESIZES a uniform playlist (ownsPlaylist) rather than serving
	// ffmpeg's own: an audio transcode's segments are ~uniform SegmentSeconds (AAC
	// frames are tiny, so the muxer cuts within a frame of the target), and — like the
	// video copy — ffmpeg writes its VOD rendition playlist only when it FINISHES
	// transcoding the whole (feature-length) track, minutes away, so serving it would
	// 404 the long file. A synthesized playlist appears immediately; the demuxed player
	// syncs the audio to the video variant by PTS, so a ~frame of EXTINF imprecision is
	// immaterial. It never realigns (the audio track is produced fast, linearly).
	segFmt := transcode.AudioRenditionSegmentPattern(streamID)
	initSeg := ""
	if sess.FMP4 {
		segFmt = transcode.AudioRenditionSegmentPatternFMP4(streamID)
		initSeg = transcode.AudioRenditionInit(streamID)
	}
	rt := &hlsRuntime{
		runner: m.runner,
		buildArgs: func(seek transcode.SeekOffset) []string {
			return build(streamID, scratch, seek)
		},
		scratchDir:    scratch,
		ownsPlaylist:  true,
		sharedScratch: true,
		// REALIGNABLE: a deep seek must not wait for the linear audio job to demux
		// its way there (reading a feature-length source over a slow mount takes
		// minutes). The realigned job seeks to the uniform grid position (4·N), and
		// because the hls muxer anchors its cut grid at the FIRST packet — a grid
		// multiple here — the produced segments keep matching the uniform playlist.
		realignable:    true,
		playlistName:   transcode.AudioRenditionPlaylist(streamID),
		segNameFmt:     segFmt,
		initSegment:    initSeg,
		segmentSeconds: transcode.SegmentSeconds,
		segmentCount:   segmentCountFor(sess.DurationMs, transcode.SegmentSeconds),
	}
	if m.audioRuntimes[sessionID] == nil {
		m.audioRuntimes[sessionID] = make(map[string]*hlsRuntime)
	}
	m.audioRuntimes[sessionID][streamID] = rt
	return rt, nil
}

// Get returns the Session by id and whether it exists. A session that has been
// ended (or never existed) returns false — the stream endpoint then answers 404,
// hiding existence the same way the browse surface does.
func (m *Manager) Get(id string) (Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	return s, ok
}

// Touch refreshes a session's LastSeen timestamp, the keepalive signal. Issue 08
// calls this from progress reporting; exposed now so the seam exists. It is a
// no-op for an unknown id.
func (m *Manager) Touch(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[id]; ok {
		s.LastSeen = m.now()
		m.sessions[id] = s
	}
}

// TouchProgress refreshes LastSeen exactly like Touch AND notifies the observer
// of a nowPlaying transition carrying positionMs. It is the progress-report seam
// (Service.ReportProgress drives it from POST /sessions/{id}/progress), kept
// distinct from plain Touch on purpose: the manifest/segment keepalives call
// Touch and carry no fresh position, so firing nowPlaying there would spam stale
// positions. A no-op (and no event) for an unknown id.
func (m *Manager) TouchProgress(id string, positionMs int64) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if ok {
		s.LastSeen = m.now()
		m.sessions[id] = s
	}
	m.mu.Unlock()
	if ok {
		m.notify(SessionEvent{
			Kind:       SessionNowPlaying,
			SessionID:  s.ID,
			UserID:     s.UserID,
			TitleID:    s.TitleID,
			PositionMs: positionMs,
		})
	}
}

// End removes the session, freeing it (the clean stop). For a directStream/
// transcode session it also kills the running ffmpeg job and deletes the scratch
// directory (kill + delete is the load-bearing cleanup — an abandoned remux must
// not hold a process or disk). It reports whether a session was actually removed
// so the api layer can answer 404 for an unknown or already-ended id rather than
// a misleading success.
func (m *Manager) End(id string) bool {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return false
	}
	delete(m.sessions, id)
	rt := m.runtimes[id]
	delete(m.runtimes, id)
	// Collect the session's demuxed audio rendition runtimes so they are torn down
	// with the session (audio-streams/03): an abandoned rendition encode must die with
	// the stream, exactly like the video job. They share the scratch dir, so their
	// teardown only kills ffmpeg; the video runtime removes the directory.
	audioRts := m.audioRuntimes[id]
	delete(m.audioRuntimes, id)
	delete(m.audioBuilders, id)
	// Free the transcode slot under the same lock that removed the session, so a
	// previously-rejected transcode can take it immediately (ADR-0009). Only a
	// transcode session ever incremented the counter, so only it decrements.
	if s.Tier == TierTranscode && !s.VideoCopy {
		m.releaseTranscodeSlot()
	}
	m.mu.Unlock()
	// Tear down outside the lock: killing ffmpeg and removing scratch must not
	// block other session operations. Kill the rendition jobs BEFORE the video
	// runtime removes the shared scratch dir.
	for _, art := range audioRts {
		art.teardown()
	}
	if rt != nil {
		rt.teardown()
	}
	// ended fires only when a session was actually removed (the !ok early-return
	// above already left), so a DELETE on an unknown/ended id emits nothing.
	m.notify(SessionEvent{Kind: SessionEnded, SessionID: s.ID, UserID: s.UserID, TitleID: s.TitleID})
	return true
}

// releaseTranscodeSlot decrements the active-transcode counter, never below
// zero (a defensive floor — paired increment/decrement keep it exact, but a
// guard means a double-free can never wrap the unsigned-style accounting into a
// permanently-full state). The caller holds m.mu.
func (m *Manager) releaseTranscodeSlot() {
	if m.activeTranscodes > 0 {
		m.activeTranscodes--
	}
}

// Count returns the number of active sessions (test/observability helper).
func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// ActiveTranscodes returns how many transcode sessions currently hold a cap slot
// (ADR-0009 observability). Direct play and remux are not counted. The Service
// surfaces it so the existing session surface can report transcode load without
// a new admin endpoint.
func (m *Manager) ActiveTranscodes() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activeTranscodes
}

// TranscodeLoad is a snapshot of the governance counters (ADR-0009): the live
// full-transcode count and the configured concurrency cap (0 = unlimited). It is
// the read accessor the admin /transcoding surface (ADR-0029) projects, so the api
// layer never reaches into the Manager's private fields.
type TranscodeLoad struct {
	Active int
	Cap    int
}

// TranscodeLoad returns the current transcode load: the number of full transcodes
// holding a cap slot and the cap itself (0 = unlimited). Direct play and remux
// carry no load and are never counted. Read under the lock so the pair is
// consistent with the reservation path.
func (m *Manager) TranscodeLoad() TranscodeLoad {
	m.mu.Lock()
	defer m.mu.Unlock()
	return TranscodeLoad{Active: m.activeTranscodes, Cap: m.transcodeCap}
}

// SetNow overrides the Manager's clock (tests inject a fake to drive reaping
// deterministically without real sleeps). Not used in production.
func (m *Manager) SetNow(now func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = now
}

// SetObserver installs the session lifecycle observer (started/nowPlaying/ended),
// mirroring SetNow's post-construction setter style. app.New wires it to the
// realtime Broker after both the Service and Broker exist; a nil observer (the
// default) is a no-op everywhere. Set once at boot before any session exists.
func (m *Manager) SetObserver(obs func(SessionEvent)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.observer = obs
}

// notify hands a lifecycle event to the observer if one is set. Callers invoke it
// OUTSIDE m.mu so a publish never blocks a concurrent session operation; a nil
// observer is a no-op.
func (m *Manager) notify(e SessionEvent) {
	if m.observer != nil {
		m.observer(e)
	}
}

// Reap removes every session whose LastSeen is older than idle ago, returning
// how many it swept. A session's progress reports double as keepalive (they
// Touch it); one that has gone silent for longer than the configured idle
// timeout is ended exactly as a clean DELETE would. For a directStream/transcode
// session that means the same teardown End does — kill ffmpeg + delete scratch —
// so an abandoned remux never leaks a process or disk. A reaped session's
// stream/hls/progress/delete then answer 404, consistent with the clean-stop
// behavior (issue 07).
func (m *Manager) Reap(idle time.Duration) int {
	m.mu.Lock()
	cutoff := m.now().Add(-idle)
	var dead []*hlsRuntime
	var ended []SessionEvent
	n := 0
	for id, s := range m.sessions {
		if s.LastSeen.Before(cutoff) {
			delete(m.sessions, id)
			// Reap the session's demuxed audio rendition runtimes first (shared-scratch,
			// so they only kill ffmpeg) so an abandoned rendition encode never outlives
			// the session (audio-streams/03) — then the video runtime removes the scratch.
			for _, art := range m.audioRuntimes[id] {
				dead = append(dead, art)
			}
			delete(m.audioRuntimes, id)
			delete(m.audioBuilders, id)
			if rt := m.runtimes[id]; rt != nil {
				dead = append(dead, rt)
				delete(m.runtimes, id)
			}
			// Reaping frees the transcode slot exactly as a clean DELETE does, so an
			// abandoned transcode never permanently holds a cap slot (ADR-0009). A
			// video-copy transcode never took a slot, so it never releases one (ADR-0024).
			if s.Tier == TierTranscode && !s.VideoCopy {
				m.releaseTranscodeSlot()
			}
			// A reaped session ends exactly as a clean DELETE does (the headline
			// acceptance criterion): announce it so the Admin's live view drops
			// abandoned streams, not only cleanly-stopped ones.
			ended = append(ended, SessionEvent{Kind: SessionEnded, SessionID: s.ID, UserID: s.UserID, TitleID: s.TitleID})
			n++
		}
	}
	m.mu.Unlock()
	// Tear down reaped remux jobs outside the lock.
	for _, rt := range dead {
		rt.teardown()
	}
	// Notify outside the lock, after teardown, so a publish never blocks a session op.
	for _, e := range ended {
		m.notify(e)
	}
	return n
}
