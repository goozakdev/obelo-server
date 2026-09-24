// Package lyricfetch is the host half of the Lyric provider Extension point: it
// decides when a track's Lyric providers are asked, judges what they answer, and
// remembers the outcome, hit or miss.
//
// A provider is asked only when the track has no Local lyrics or only Plain ones
// (ADR-0063's note on this seam), and only lazily — the first time someone opens
// the lyrics view for that track. What comes back is kept in the lyrics table and
// nowhere else: never beside the track in its library folder, where a scan would
// take it for a Local lyric.
//
// The judgment is the HOST's, and a Plugin is never asked whether its own answer
// is acceptable:
//   - a Synced answer timed for a recording more than three seconds longer or
//     shorter than the track — or one that does not say — is kept only as Plain,
//     because its words are still right even where its timing is not;
//   - an answer naming a different MusicBrainz recording than the one the host
//     sent is dropped whole, not even kept as Plain;
//   - providers are asked in the Admin's order, the first acceptable Synced
//     answer wins, and the first Plain one seen — the track's own Local one
//     first — is the fallback;
//   - an answer anybody marked wrong for a track (Reject) is never kept for that
//     track again, whichever provider gives it.
package lyricfetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"sort"
	"strings"
	"sync"

	"github.com/goozakdev/obelo-server/internal/lyrics"
	"github.com/goozakdev/obelo-server/internal/store"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// Store is the persistence the Service reads and writes. *store.DB satisfies it.
type Store interface {
	LocalLyrics(titleID string) (lyrics.Lyrics, bool, error)
	FetchedLyrics(titleID string) (store.FetchedLyrics, bool, error)
	WriteFetchedLyrics(titleID string, f store.FetchedLyrics) error
	LyricProviderOrder() ([]string, error)
	RejectedLyrics(titleID string) ([]string, error)
	RejectFetchedLyrics(titleID, answer string) error
}

// ErrNothingToReject is Reject's answer when what the track shows is not a Lyric
// provider's — its own Local lyrics, or none.
var ErrNothingToReject = errors.New("lyricfetch: no fetched lyrics to reject")

// ErrNotShown is Reject's answer when the answer named is not the one the track
// shows any more — someone else's rejection replaced it — so nothing is rejected.
var ErrNotShown = errors.New("lyricfetch: that answer is no longer shown")

// Track is what the API layer resolves from a Track's detail for the Service:
// the words a provider searches by, the track's own length, and the MusicBrainz
// recording id this server holds for it ("" when none).
type Track struct {
	ID          string
	Artist      string
	Title       string
	Album       string
	DurationMs  int64
	RecordingID string
}

// Sources of an Answer.
const (
	SourceLocal   = "local"
	SourceFetched = "fetched"
)

// Answer is the lyrics a Track shows and where they came from. ID names a
// fetched answer — what "wrong lyrics" sends back to say which answer it was
// pressed on — and is "" for Local lyrics.
type Answer struct {
	Lyrics lyrics.Lyrics
	Source string
	ID     string
}

// durationToleranceMs is how far a Synced answer's stated duration may be from
// the track's own before its timing is not trusted: a trimmed silence or a
// different encoder's padding, not a different edit.
const durationToleranceMs = 3000

// Bounds on what one answer may carry. A whole song is a few kilobytes; past
// these it is not lyrics.
const (
	maxAnswerLines = 5000
	maxAnswerBytes = 1 << 20
	maxLineMs      = 24 * 60 * 60 * 1000
)

// Service answers a Track's lyrics.
type Service struct {
	store Store
	reg   *pluginapi.Registry

	// inflight holds one channel per Track whose providers are being asked right
	// now, closed when the asking is done, so two viewers opening the same track
	// at once ask each provider once between them.
	mu       sync.Mutex
	inflight map[string]chan struct{}
}

// New returns a Service over st, asking the Lyric providers registered in reg.
func New(st Store, reg *pluginapi.Registry) *Service {
	return &Service{store: st, reg: reg, inflight: map[string]chan struct{}{}}
}

// provider is one Lyric provider built and ready to ask.
type provider struct {
	slug string
	p    pluginapi.LyricProvider
}

// Providers returns the registered Lyric providers that serve music, in the
// Admin's order: those the Admin placed first, in that order, then the rest in
// registration order.
func (s *Service) Providers() ([]pluginapi.LyricProviderRegistration, error) {
	order, err := s.store.LyricProviderOrder()
	if err != nil {
		return nil, err
	}
	rank := make(map[string]int, len(order))
	for i, slug := range order {
		rank[slug] = i
	}
	var out []pluginapi.LyricProviderRegistration
	for _, r := range s.reg.LyricProviders() {
		if len(r.Descriptor.Kinds) == 0 || r.Descriptor.Serves(pluginapi.KindMusic) {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, iok := rank[out[i].Descriptor.Slug]
		rj, jok := rank[out[j].Descriptor.Slug]
		switch {
		case iok && jok:
			return ri < rj
		case iok != jok:
			return iok
		}
		return false
	})
	return out, nil
}

// Lyrics returns the lyrics t shows, and false when it has none.
//
// Local Synced lyrics are the answer and nothing is asked. Otherwise the
// remembered provider answer is used when there is one — a hit always, a miss
// only while the question it answered is still the question — and the providers
// are asked when there is not.
func (s *Service) Lyrics(ctx context.Context, t Track) (Answer, bool, error) {
	local, hasLocal, err := s.store.LocalLyrics(t.ID)
	if err != nil {
		return Answer{}, false, err
	}
	if hasLocal && local.Kind == lyrics.Synced {
		return Answer{Lyrics: local, Source: SourceLocal}, true, nil
	}
	providers, err := s.build()
	if err != nil {
		return Answer{}, false, err
	}
	question := questionFor(t, providers)
	for {
		f, have, err := s.store.FetchedLyrics(t.ID)
		if err != nil {
			return Answer{}, false, err
		}
		if (have && (f.Lyrics != nil || f.Question == question)) || len(providers) == 0 {
			return pick(local, hasLocal, f.Lyrics)
		}
		done, lead := s.claim(t.ID)
		if !lead {
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return pick(local, hasLocal, nil)
			}
		}
		f, settled, err := s.ask(ctx, t, providers)
		f.Question = question
		if err == nil && settled {
			err = s.store.WriteFetchedLyrics(t.ID, f)
		}
		s.release(t.ID, done)
		if err != nil {
			return Answer{}, false, err
		}
		return pick(local, hasLocal, f.Lyrics)
	}
}

// build makes every Lyric provider in Admin order. One whose factory refuses —
// a Plugin this server stopped calling — is not asked, and is not part of the
// question either.
func (s *Service) build() ([]provider, error) {
	regs, err := s.Providers()
	if err != nil {
		return nil, err
	}
	var out []provider
	for _, r := range regs {
		p, err := r.New(pluginapi.Settings{Enabled: true, URL: r.Descriptor.DefaultURL})
		if err != nil {
			continue
		}
		out = append(out, provider{slug: r.Descriptor.Slug, p: p})
	}
	return out, nil
}

// Reject is the "wrong lyrics" action: it forgets the provider answer t shows,
// never to keep that answer for t again, and asks every provider afresh — the
// same judgment, in the same order, passing over every answer rejected for t so
// far. It returns what t shows afterwards. The rejection is t's, not the
// viewer's: everyone opening t from now on sees the outcome.
//
// id is the Answer.ID the viewer was shown. Only that answer is rejected: when t
// shows another one now, or none, nothing changes and the error is ErrNotShown
// (or ErrNothingToReject), so a stale view or a second press of the same button
// cannot reject an answer nobody pressed on.
//
// It waits out any asking already under way for t, so an answer found by an
// open that started before the rejection cannot be written back after it.
func (s *Service) Reject(ctx context.Context, t Track, id string) (Answer, bool, error) {
	var done chan struct{}
	for {
		ch, lead := s.claim(t.ID)
		if lead {
			done = ch
			break
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return Answer{}, false, ctx.Err()
		}
	}
	err := s.rejectShown(t.ID, id)
	s.release(t.ID, done)
	if err != nil {
		return Answer{}, false, err
	}
	return s.Lyrics(ctx, t)
}

// rejectShown records the fetched answer titleID shows as rejected and forgets
// it, provided it is the answer named id. It returns ErrNothingToReject when what
// titleID shows is not fetched, and ErrNotShown when it is another answer.
func (s *Service) rejectShown(titleID, id string) error {
	local, hasLocal, err := s.store.LocalLyrics(titleID)
	if err != nil {
		return err
	}
	f, have, err := s.store.FetchedLyrics(titleID)
	if err != nil {
		return err
	}
	if !have || f.Lyrics == nil {
		return ErrNothingToReject
	}
	if hasLocal && local.Kind == lyrics.Synced {
		return ErrNothingToReject
	}
	if a, _, _ := pick(local, hasLocal, f.Lyrics); a.Source != SourceFetched {
		return ErrNothingToReject
	}
	if answerID(*f.Lyrics) != id {
		return ErrNotShown
	}
	return s.store.RejectFetchedLyrics(titleID, id)
}

// ask puts the question to each provider in order and returns what the track
// should remember: the first acceptable Synced answer, else the first Plain one,
// else a miss. An answer rejected for the track is not acceptable. settled is
// false when nothing was found and a provider failed — a failure is not an
// answer, so it must not be remembered as a miss.
func (s *Service) ask(ctx context.Context, t Track, providers []provider) (store.FetchedLyrics, bool, error) {
	ids, err := s.store.RejectedLyrics(t.ID)
	if err != nil {
		return store.FetchedLyrics{}, false, err
	}
	rejected := make(map[string]bool, len(ids))
	for _, id := range ids {
		rejected[id] = true
	}
	req := pluginapi.LyricsRequest{
		Artist: t.Artist, Title: t.Title, Album: t.Album, DurationMs: t.DurationMs, RecordingID: t.RecordingID,
	}
	var fallback store.FetchedLyrics
	failed := false
	for _, p := range providers {
		resp, err := p.p.Lyrics(ctx, req)
		if err != nil {
			log.Printf("obelo: lyrics from %s: %v", p.slug, err)
			failed = true
			continue
		}
		l, ok := judge(t, resp)
		if !ok || rejected[answerID(l)] {
			continue
		}
		if l.Kind == lyrics.Synced {
			return store.FetchedLyrics{Lyrics: &l, Provider: p.slug}, true, nil
		}
		if fallback.Lyrics == nil {
			fallback = store.FetchedLyrics{Lyrics: &l, Provider: p.slug}
		}
	}
	if fallback.Lyrics != nil {
		return fallback, !failed, nil
	}
	return store.FetchedLyrics{}, !failed, nil
}

// judge is the host's whole judgment on one answer: the lyrics to keep, and
// false when there is nothing to keep.
func judge(t Track, resp pluginapi.LyricsResponse) (lyrics.Lyrics, bool) {
	if t.RecordingID != "" && resp.RecordingID != "" &&
		!strings.EqualFold(strings.TrimSpace(resp.RecordingID), strings.TrimSpace(t.RecordingID)) {
		return lyrics.Lyrics{}, false
	}
	switch resp.Kind {
	case pluginapi.LyricsSynced:
		if len(resp.Lines) > maxAnswerLines {
			return lyrics.Lyrics{}, false
		}
		var lines []lyrics.Line
		size, words := 0, false
		for _, ln := range resp.Lines {
			if ln.StartMs < 0 || ln.StartMs > maxLineMs {
				continue
			}
			size += len(ln.Text)
			words = words || strings.TrimSpace(ln.Text) != ""
			lines = append(lines, lyrics.Line{StartMs: ln.StartMs, Text: ln.Text})
		}
		if !words || size > maxAnswerBytes {
			return lyrics.Lyrics{}, false
		}
		sort.SliceStable(lines, func(i, j int) bool { return lines[i].StartMs < lines[j].StartMs })
		if !timedForThisTrack(t.DurationMs, resp.DurationMs) {
			return asPlain(lines), true
		}
		return lyrics.Lyrics{Kind: lyrics.Synced, Lines: lines}, true
	case pluginapi.LyricsPlain:
		text := strings.TrimSpace(resp.Text)
		if text == "" || len(text) > maxAnswerBytes {
			return lyrics.Lyrics{}, false
		}
		return lyrics.Lyrics{Kind: lyrics.Plain, Text: text}, true
	}
	return lyrics.Lyrics{}, false
}

// timedForThisTrack reports whether lines timed against a recording of answered
// ms can be followed on a track of track ms. Either length unknown is a no.
func timedForThisTrack(track, answered int64) bool {
	if track <= 0 || answered <= 0 {
		return false
	}
	diff := track - answered
	if diff < 0 {
		diff = -diff
	}
	return diff <= durationToleranceMs
}

// asPlain keeps a mistimed Synced answer's words, one line each.
func asPlain(lines []lyrics.Line) lyrics.Lyrics {
	texts := make([]string, len(lines))
	for i, ln := range lines {
		texts[i] = ln.Text
	}
	return lyrics.Lyrics{Kind: lyrics.Plain, Text: strings.TrimSpace(strings.Join(texts, "\n"))}
}

// pick chooses what the track shows: a fetched Synced answer, else the Local
// Plain one, else a fetched Plain one. (Local Synced never reaches here.)
func pick(local lyrics.Lyrics, hasLocal bool, fetched *lyrics.Lyrics) (Answer, bool, error) {
	switch {
	case fetched != nil && fetched.Kind == lyrics.Synced:
		return Answer{Lyrics: *fetched, Source: SourceFetched, ID: answerID(*fetched)}, true, nil
	case hasLocal:
		return Answer{Lyrics: local, Source: SourceLocal}, true, nil
	case fetched != nil:
		return Answer{Lyrics: *fetched, Source: SourceFetched, ID: answerID(*fetched)}, true, nil
	}
	return Answer{}, false, nil
}

// questionFor is what the providers are asked, as a digest a remembered miss is
// compared against: the providers that would be asked, and everything the
// request carries. Order is left out — reordering the same providers cannot turn
// a miss into a hit.
func questionFor(t Track, providers []provider) string {
	slugs := make([]string, len(providers))
	for i, p := range providers {
		slugs[i] = p.slug
	}
	sort.Strings(slugs)
	b, _ := json.Marshal(struct {
		Providers   []string `json:"providers"`
		Artist      string   `json:"artist"`
		Title       string   `json:"title"`
		Album       string   `json:"album"`
		DurationMs  int64    `json:"durationMs"`
		RecordingID string   `json:"recordingId"`
	}{slugs, t.Artist, t.Title, t.Album, t.DurationMs, strings.ToLower(t.RecordingID)})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// answerID names an answer as the host kept it — its shape, its timing and its
// words, not who gave it — so an answer once rejected is recognised from any
// provider. The words are compared as a reader sees them: each line trimmed,
// runs of spaces as one, case folded, and a Plain answer's blank lines dropped,
// so a trivially respaced or recased copy of a rejected answer is the same one.
func answerID(l lyrics.Lyrics) string {
	n := lyrics.Lyrics{Kind: l.Kind}
	if l.Kind == lyrics.Synced {
		for _, ln := range l.Lines {
			n.Lines = append(n.Lines, lyrics.Line{StartMs: ln.StartMs, Text: normalWords(ln.Text)})
		}
	} else {
		var lines []string
		for _, ln := range strings.Split(l.Text, "\n") {
			if w := normalWords(ln); w != "" {
				lines = append(lines, w)
			}
		}
		n.Text = strings.Join(lines, "\n")
	}
	b, _ := json.Marshal(n)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// normalWords is one line's words as answerID compares them.
func normalWords(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// claim returns the channel that closes when the asking for titleID is done, and
// true when the caller is the one to do it.
func (s *Service) claim(titleID string) (chan struct{}, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.inflight[titleID]; ok {
		return ch, false
	}
	ch := make(chan struct{})
	s.inflight[titleID] = ch
	return ch, true
}

// release ends the asking for titleID.
func (s *Service) release(titleID string, ch chan struct{}) {
	s.mu.Lock()
	delete(s.inflight, titleID)
	s.mu.Unlock()
	close(ch)
}
