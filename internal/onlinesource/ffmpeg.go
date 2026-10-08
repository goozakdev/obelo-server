package onlinesource

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/goozakdev/obelo-server/internal/playback"
	"github.com/goozakdev/obelo-server/internal/transcode"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The ffmpeg path (C) of an Online play (ADR-0068 decision 4): a variant the client
// cannot take as is — split, a manifest, a format it cannot decode, or above the
// Playback ceiling — is read and encoded by ffmpeg into HLS in a scratch directory
// of the transcode cache. The bytes in flight are the only thing ever written, and
// they go when the session does (Q2).

// ErrBusy: the concurrent-transcode cap (ADR-0009) is full. A Title is refused the
// same way, and the api layer renders both as SERVER_BUSY.
var ErrBusy = errors.New("onlinesource: transcode capacity full")

// Transcoder is what the ffmpeg path runs on. Without one the Service relays what
// it can and refuses the rest as "a transcode would be required".
type Transcoder struct {
	Runner transcode.Runner
	// ScratchRoot holds one directory per session, removed when it ends.
	ScratchRoot string
	Accel       transcode.Accel
	// Reserve takes a slot of the transcode cap shared with Title sessions
	// (playback.Manager.ReserveTranscode), returning ErrTranscodeCapFull when none
	// is free, and a func that gives the slot back.
	Reserve func() (release func(), err error)
}

// SetTranscoder enables the ffmpeg path.
func (s *Service) SetTranscoder(t Transcoder) { s.transcoder = &t }

// encodedFileName is the only names a session's encoded files are served by:
// ffmpeg's playlist and its numbered segments (transcode.PlaylistName and
// SegmentPattern), never a path.
var encodedFileName = regexp.MustCompile(`^(index\.m3u8|segment[0-9]{3,}\.ts)$`)

const (
	// encodedWait is how long a request for a file ffmpeg has not written yet waits
	// for it: the first playlist appears once the first segment is cut.
	encodedWait = 30 * time.Second
	encodedPoll = 50 * time.Millisecond
)

// run is one session's ffmpeg encode.
type run struct {
	dir     string
	job     transcode.Job
	cancel  context.CancelFunc
	release func()
	once    sync.Once
}

// stop ends the encode and takes everything it made with it: the process, the
// scratch directory with its playlist and segments, and the cap slot.
func (r *run) stop() {
	r.once.Do(func() {
		r.cancel()
		_ = r.job.Kill()
		if err := os.RemoveAll(r.dir); err != nil {
			log.Printf("obelo: online source: removing transcode scratch %s: %v", r.dir, err)
		}
		r.release()
	})
}

// startEncode reserves a cap slot and starts ffmpeg for the session. On any failure
// nothing is left behind: no slot, no directory, no process.
func (s *Service) startEncode(sessionID string, v pluginapi.OnlineVariant, plan playback.OnlinePlan) (*run, error) {
	t := s.transcoder
	release, err := t.Reserve()
	if err != nil {
		if errors.Is(err, playback.ErrTranscodeCapFull) {
			return nil, ErrBusy
		}
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	dir := filepath.Join(t.ScratchRoot, sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		release()
		log.Printf("obelo: online source: creating transcode scratch: %v", err)
		return nil, ErrUnavailable
	}
	job := transcode.OnlineJob{
		Inputs:     encodeInputs(v),
		Headers:    allowedHeaders(v.Headers),
		OutputDir:  dir,
		MaxHeight:  plan.MaxHeight,
		MaxBitrate: plan.MaxBitrate,
		Accel:      t.Accel,
	}
	ctx, cancel := context.WithCancel(context.Background())
	j, err := t.Runner.Start(ctx, transcode.OnlineArgs(job))
	if err != nil {
		cancel()
		_ = os.RemoveAll(dir)
		release()
		log.Printf("obelo: online source: starting ffmpeg: %v", redactURLsIn(err))
		return nil, ErrUnavailable
	}
	return &run{dir: dir, job: j, cancel: cancel, release: sync.OnceFunc(release)}, nil
}

// watch ends the session if its ffmpeg fails. A clean exit is the encode finishing:
// the session lives on so the player can read what was made. A failure ends the
// session, and with it the files, unless the media host refused the URL (403/410):
// then the session gets its one re-resolve and ffmpeg restarts where it left off
// (restartEncode). It starts once the session is registered, so a run that dies at
// once still finds the session to end.
func (s *Service) watch(sessionID string, r *run) {
	for {
		err := r.job.Wait()
		refused := mediaRefused(err, r.job)
		if err != nil {
			log.Printf("obelo: online source: ffmpeg for session %s: %v", sessionID, redactURLsIn(err))
		} else if refused {
			log.Printf("obelo: online source: ffmpeg for session %s: the media host refused a URL", sessionID)
		}
		if !refused {
			if err != nil {
				s.End(sessionID)
			} else {
				s.finishPlaylist(sessionID, r)
			}
			return
		}
		next, ok := s.restartEncode(sessionID, r)
		if !ok {
			return
		}
		r = next
	}
}

// encodeInputs are the URLs ffmpeg reads for a variant: the picture then the sound
// of a split variant, else the one URL.
func encodeInputs(v pluginapi.OnlineVariant) []string {
	if v.Kind == pluginapi.OnlineVariantSplit {
		return []string{v.VideoURL, v.AudioURL}
	}
	return []string{v.URL}
}

// allowedHeaders keeps the request headers a Plugin may have sent upstream
// (relayRequestHeaders), for the ffmpeg path as for the relay.
func allowedHeaders(h map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range h {
		if relayRequestHeaders[strings.ToLower(k)] {
			out[k] = v
		}
	}
	return out
}

// redactURLsIn drops everything from an https URL on in an error message, so the
// upstream address (which may hold a Plugin's token) is not written to the log.
func redactURLsIn(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, "https://"); i >= 0 {
		msg = msg[:i] + "<url>"
	}
	return msg
}

// OpenEncoded opens one file of a live ffmpeg session's output by name, waiting for
// ffmpeg to write it. A session that is not live, is not an ffmpeg one, or a name
// that is not the playlist or a segment is ErrNoSession.
func (s *Service) OpenEncoded(ctx context.Context, sessionID, name string) (*os.File, error) {
	if !encodedFileName.MatchString(name) {
		return nil, ErrNoSession
	}
	deadline := time.NewTimer(encodedWait)
	defer deadline.Stop()
	tick := time.NewTicker(encodedPoll)
	defer tick.Stop()
	for {
		s.mu.Lock()
		sess, ok := s.sessions[sessionID]
		var cur *run
		if ok {
			cur = sess.run // a restart replaces it, under s.mu
		}
		s.mu.Unlock()
		if cur == nil {
			return nil, ErrNoSession
		}
		f, err := os.Open(filepath.Join(cur.dir, name))
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, ErrUnavailable
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, ErrUnavailable
		case <-tick.C:
		}
	}
}
