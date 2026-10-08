package onlinesource

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/goozakdev/obelo-server/internal/playback"
	"github.com/goozakdev/obelo-server/internal/transcode"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The one re-resolve (ADR-0068 decision 10, Q11). resolve() runs once per playback
// session; when the media host answers 403 or 410 the Server resolves again, once,
// and carries on from the position: a relay asks the new URL for the Range the client
// asked, ffmpeg restarts where it left off, as ADR-0025's switch restarts a stream.
// The new first URL passes the same checks as the first. Nothing runs on a timer.
//
// If the re-resolve fails, or a second refusal follows the recovery, the session ends
// and keeps a short-lived note the player can read (Gone): "This video is no longer
// available from {source}".

// reState is a session's single re-resolve. mu serializes the recovery, so several
// requests that hit the same expired URL at once cost one resolve().
type reState struct {
	mu   sync.Mutex
	used bool
	// gaveBack: an aborted re-resolve was already handed back once.
	gaveBack bool
}

const (
	// goneNoteTTL and maxGoneNotes bound how long, and how many, ended sessions are
	// remembered for their player to read.
	goneNoteTTL  = 10 * time.Minute
	maxGoneNotes = 256
)

type goneNote struct {
	userID  string
	message string
	at      time.Time
}

// Gone reports the message a session left when it ended because its media was no
// longer available, to the User who held it and no one else. It is what the player
// shows in place of the video.
func (s *Service) Gone(sessionID, userID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.gone[sessionID]
	if !ok || n.userID != userID || s.now().Sub(n.at) > goneNoteTTL {
		return "", false
	}
	return n.message, true
}

// HasGone reports whether a session left a note, whoever it was for. The api layer
// uses it only to route the request to the handler that checks the caller.
func (s *Service) HasGone(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.gone[sessionID]
	return ok && s.now().Sub(n.at) <= goneNoteTTL
}

// endGone records the note, then ends the session. The note comes first so a player
// that sees the stream stop always finds it. A session already ended by other means
// (the User stopped it, access was revoked) while the re-resolve ran leaves no note.
func (s *Service) endGone(sess Session) error {
	if s.leaveNote(sess, "This video is no longer available from "+sess.SourceName) {
		s.End(sess.ID)
	}
	return ErrGone
}

// leaveNote records message as what sess's player reads once it has ended, and says
// whether sess was still live to leave one.
func (s *Service) leaveNote(sess Session, message string) bool {
	s.mu.Lock()
	if _, live := s.sessions[sess.ID]; !live {
		s.mu.Unlock()
		return false
	}
	if s.gone == nil {
		s.gone = map[string]goneNote{}
	}
	now := s.now()
	for id, n := range s.gone {
		if now.Sub(n.at) > goneNoteTTL {
			delete(s.gone, id)
		}
	}
	for len(s.gone) >= maxGoneNotes {
		oldest, first := "", true
		var at time.Time
		for id, n := range s.gone {
			if first || n.at.Before(at) {
				oldest, at, first = id, n.at, false
			}
		}
		delete(s.gone, oldest)
	}
	s.gone[sess.ID] = goneNote{userID: sess.UserID, message: message, at: now}
	s.mu.Unlock()
	return true
}

// resolveVariants calls resolve() for an item and returns the variants that pass the
// host's judgment on their first URL (check.go). ErrNoItem: the Plugin offered none;
// ErrUnavailable: it failed, or none of what it offered passed.
func (s *Service) resolveVariants(ctx context.Context, p pluginapi.OnlineSourceProvider, sourceID, itemID string, c playback.Constraints) ([]pluginapi.OnlineVariant, error) {
	rctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	resp, err := p.Resolve(rctx, pluginapi.OnlineResolveRequest{
		ItemID: itemID,
		Hints:  pluginapi.OnlineHints{MaxHeight: playback.ResolutionHeight(c.MaxResolution)},
	})
	if err != nil {
		log.Printf("obelo: online source %s: resolve %s: %v", sourceID, itemID, err)
		return nil, ErrUnavailable
	}
	if len(resp.Variants) == 0 {
		return nil, ErrNoItem
	}
	// The host's judgment on the first URL of every variant, for both paths: one the
	// Plugin could not have meant for the Server to fetch is not a variant.
	var variants []pluginapi.OnlineVariant
	for _, v := range resp.Variants {
		if err := s.checkVariant(ctx, sourceID, v); err != nil {
			log.Printf("obelo: online source %s: item %s: variant dropped: %v", sourceID, itemID, err)
			continue
		}
		variants = append(variants, v)
	}
	if len(variants) == 0 {
		return nil, ErrUnavailable
	}
	return variants, nil
}

// replan resolves sess's item again and chooses a variant the way Play did. The new
// choice must be played the same way as the old (relayed or encoded): a session does
// not change path in flight.
func (s *Service) replan(ctx context.Context, sess Session) (playback.OnlinePlan, error) {
	p, _, err := s.provider(sess.SourceID)
	if err != nil {
		return playback.OnlinePlan{}, err
	}
	variants, err := s.resolveVariants(ctx, p, sess.SourceID, sess.ItemID, sess.constraints)
	if err != nil {
		return playback.OnlinePlan{}, err
	}
	plan, unsup := playback.PlanOnline(sess.profile, sess.constraints, variants, s.transcoder != nil)
	if unsup != nil {
		return playback.OnlinePlan{}, fmt.Errorf("%w: re-resolved variant is not playable as before", ErrUnavailable)
	}
	if plan.Transcode != sess.Transcoded {
		return playback.OnlinePlan{}, fmt.Errorf("%w: re-resolved variant needs a different path", ErrUnavailable)
	}
	return plan, nil
}

// reresolveRelay answers a relayed request that the media host refused (failed was the
// session as the request saw it): it returns the session with a new variant to
// retry on, or ErrGone after ending the session, or ErrUnavailable when it is already
// over.
func (s *Service) reresolveRelay(ctx context.Context, failed Session) (Session, error) {
	s.mu.Lock()
	live, ok := s.sessions[failed.ID]
	var re *reState
	if ok {
		if live.re == nil {
			live.re = &reState{}
		}
		re = live.re
	}
	s.mu.Unlock()
	if !ok {
		return Session{}, ErrUnavailable
	}
	re.mu.Lock()
	defer re.mu.Unlock()

	s.mu.Lock()
	cur, ok := s.sessions[failed.ID]
	var now Session
	if ok {
		now = *cur
	}
	s.mu.Unlock()
	if !ok {
		return Session{}, ErrUnavailable
	}
	if now.gen != failed.gen {
		// Another request of this session recovered it while this one waited: retry
		// on what it found, even if that is the same URL.
		return now, nil
	}
	if re.used {
		return Session{}, s.endGone(now)
	}
	re.used = true
	plan, err := s.replan(ctx, now)
	if err != nil {
		if ctx.Err() != nil {
			// This client went away mid-way; that says nothing of the media, so the
			// session keeps its re-resolve for the next request. Given back once only:
			// a client that aborts every time must not turn one re-resolve into many.
			if !re.gaveBack {
				re.gaveBack = true
				re.used = false
			}
			return Session{}, ErrUnavailable
		}
		log.Printf("obelo: online source %s: re-resolve %s: %v", now.SourceID, now.ItemID, err)
		return Session{}, s.endGone(now)
	}
	s.mu.Lock()
	cur, ok = s.sessions[failed.ID]
	if ok {
		cur.Variant = plan.Variant
		cur.gen++
		now = *cur
	}
	s.mu.Unlock()
	if !ok {
		return Session{}, ErrUnavailable
	}
	return now, nil
}

// restartEncode is watch's answer to ffmpeg dying on a refused URL: re-resolve once
// and start ffmpeg again, on the new URL, in the same directory, from the end of
// what was already cut. It returns the new run, or false once the session has ended
// (by it or otherwise).
func (s *Service) restartEncode(sessionID string, old *run) (*run, bool) {
	s.mu.Lock()
	live, ok := s.sessions[sessionID]
	var cur Session
	var re *reState
	if ok {
		if live.re == nil {
			live.re = &reState{}
		}
		cur, re = *live, live.re
	}
	s.mu.Unlock()
	if !ok || cur.run != old {
		return nil, false
	}
	re.mu.Lock()
	defer re.mu.Unlock()
	if re.used {
		_ = s.endGone(cur)
		return nil, false
	}
	re.used = true
	plan, err := s.replan(cur.Context(), cur)
	if err != nil {
		log.Printf("obelo: online source %s: re-resolve %s: %v", cur.SourceID, cur.ItemID, err)
		_ = s.endGone(cur)
		return nil, false
	}
	// Continue where the playlist the player is reading ends: its listed durations are
	// the position (the last segment is usually short), and ffmpeg numbers the next
	// segment from the same playlist.
	cutSeconds, cutSegments := playlistCut(old.dir)
	job := transcode.OnlineJob{
		Inputs:       encodeInputs(plan.Variant),
		Headers:      allowedHeaders(plan.Variant.Headers),
		OutputDir:    old.dir,
		MaxHeight:    plan.MaxHeight,
		MaxBitrate:   plan.MaxBitrate,
		Accel:        s.transcoder.Accel,
		StartSeconds: cutSeconds,
		Append:       cutSegments > 0,
	}
	ctx, cancel := context.WithCancel(context.Background())
	j, err := s.transcoder.Runner.Start(ctx, transcode.OnlineArgs(job))
	if err != nil {
		cancel()
		log.Printf("obelo: online source: restarting ffmpeg: %v", redactURLsIn(err))
		s.End(sessionID)
		return nil, false
	}
	// The new run takes over the scratch directory and the cap slot; the old one is
	// disarmed so a late stop of it neither removes the directory nor gives the slot
	// back twice.
	next := &run{dir: old.dir, job: j, cancel: cancel, release: old.release}
	old.once.Do(old.cancel)
	s.mu.Lock()
	live, ok = s.sessions[sessionID]
	if ok {
		live.run = next
		live.Variant = plan.Variant
	}
	s.mu.Unlock()
	if !ok {
		next.stop()
		return nil, false
	}
	return next, true
}

// playlistCut reads the playlist ffmpeg has written in dir and returns the seconds of
// media it lists (the sum of the EXTINF durations) and how many segments that is.
func playlistCut(dir string) (seconds float64, segments int) {
	b, err := os.ReadFile(filepath.Join(dir, transcode.PlaylistName))
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if d, ok := strings.CutPrefix(line, "#EXTINF:"); ok {
			d, _, _ = strings.Cut(d, ",")
			if v, err := strconv.ParseFloat(d, 64); err == nil && v > 0 {
				seconds += v
				segments++
			}
		}
	}
	return math.Round(seconds*1000) / 1000, segments
}

// finishPlaylist marks r's playlist complete once its encode has ended cleanly,
// unless the session has gone or moved to another run meanwhile. ffmpeg is told not
// to write the ENDLIST itself (transcode.OnlineArgs).
func (s *Service) finishPlaylist(sessionID string, r *run) {
	s.mu.Lock()
	live, ok := s.sessions[sessionID]
	current := ok && live.run == r
	s.mu.Unlock()
	if !current {
		return
	}
	path := filepath.Join(r.dir, transcode.PlaylistName)
	b, err := os.ReadFile(path)
	if err != nil || bytes.Contains(b, []byte("#EXT-X-ENDLIST")) {
		return
	}
	tmp := path + ".end"
	if err := os.WriteFile(tmp, append(b, []byte("#EXT-X-ENDLIST\n")...), 0o644); err != nil {
		return
	}
	// A rename, so a player reading the playlist sees the old one or the new, never half.
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
	}
}

// mediaRefused reports whether an ffmpeg failure or its stderr says the media host
// refused a URL (403 or 410, transcode.MediaRefusalPattern).
func mediaRefused(err error, job transcode.Job) bool {
	if err != nil && transcode.MediaRefusalPattern.MatchString(err.Error()) {
		return true
	}
	// ffmpeg skips the HLS segments a host refuses and exits 0, naming them only on
	// stderr; the job remembers the line even when later output scrolls it away.
	sj, ok := job.(transcode.StderrJob)
	return ok && sj.Refused()
}
