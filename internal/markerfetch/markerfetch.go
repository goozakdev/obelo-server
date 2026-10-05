// Package markerfetch is the host half of the Marker provider Extension point:
// it decides when a File's Marker providers are asked, judges what they answer,
// and remembers the outcome, hit or miss.
//
// A provider is asked lazily — the first time a File is played — and again only
// once the question changes. What survives is stored as the File's Fetched
// Markers, which the read (store.MarkersForFile) serves only where neither a
// Local nor a Detected Marker already covers that kind (ADR-0065 §1).
//
// The judgment is the HOST's, and a Plugin is never asked whether its own answer
// is acceptable:
//   - a candidate timed for a recording more than LengthToleranceMs longer or
//     shorter than the File — or one that does not say — is dropped outright,
//     never stored (ADR-0065 §2): a Marker has no lesser shape to fall back to;
//   - a candidate naming no known kind, or a span that is empty or lies outside
//     the File, is dropped;
//   - providers are asked in registration order, and the first to answer a kind
//     acceptably supplies that kind.
//
// Fetched Markers are served only while at least one Marker provider is
// installed and enabled (Serving); the rows are kept, so a provider enabled again
// serves them without being asked again.
package markerfetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/goozakdev/obelo-server/internal/markers"
	"github.com/goozakdev/obelo-server/internal/store"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// LengthToleranceMs is how far a candidate's stated recording length may be
// from the File's own before its timing is not trusted: a trimmed silence or a
// different encoder's padding, not a different cut. It is the tolerance a Lyric
// provider's Synced answer is held to.
const LengthToleranceMs = 3000

// ProviderTimeout bounds one provider's answer about one File, the wait for the
// Plugin's call slot included. Each provider has its own: providers are asked one
// after another, and one that hangs must not spend the time of those after it.
const ProviderTimeout = 30 * time.Second

// Store is the persistence the Service reads and writes. *store.DB satisfies it.
type Store interface {
	MarkerFetchQuestion(path string) (string, bool, error)
	SaveFetchedMarkers(path, question string, ms []store.Marker) error
}

// Catalog is what FetchFile resolves a played File from. *store.DB satisfies it.
type Catalog interface {
	TitleByID(id string) (store.TitleDetail, error)
	EpisodeContextForTitle(titleID string) (store.EpisodeContext, error)
	ShowByID(id string) (store.Show, error)
}

// Item is what the API layer resolves from a session's File for the Service:
// where the File is and how long it is, and what a provider looks it up by.
type Item struct {
	Path       string
	DurationMs int64

	Kind          string
	Title         string
	Year          int
	IDs           map[string]string
	ShowTitle     string
	ShowIDs       map[string]string
	SeasonNumber  int
	EpisodeNumber int
}

// Service fetches a File's Markers.
type Service struct {
	store   Store
	catalog Catalog
	reg     *pluginapi.Registry

	// inflight holds one channel per File whose providers are being asked right
	// now, closed when the asking is done, so two sessions starting the same File
	// at once ask each provider once between them.
	mu       sync.Mutex
	inflight map[string]chan struct{}

	providerTimeout time.Duration

	// ctx is cancelled by Close, which then waits on running for every asking
	// Start began. closed refuses a Start after Close; it is guarded by mu.
	ctx     context.Context
	cancel  context.CancelFunc
	running sync.WaitGroup
	closed  bool
}

// New returns a Service over st, asking the Marker providers registered in reg.
// A st that is also a Catalog (as *store.DB is) serves FetchFile.
func New(st Store, reg *pluginapi.Registry) *Service {
	cat, _ := st.(Catalog)
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{
		store: st, catalog: cat, reg: reg, inflight: map[string]chan struct{}{},
		providerTimeout: ProviderTimeout, ctx: ctx, cancel: cancel,
	}
}

// Start asks the providers about the File fileID of the Title titleID, as
// FetchFile does, in the background: the asking is not the viewer's, so a viewer
// giving up on the read that started it ends nothing. It returns a channel closed
// when the asking is done. Close ends it; after Close nothing is asked.
func (s *Service) Start(titleID, fileID string) <-chan struct{} {
	done := make(chan struct{})
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		close(done)
		return done
	}
	s.running.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.running.Done()
		defer close(done)
		if err := s.FetchFile(s.ctx, titleID, fileID); err != nil && s.ctx.Err() == nil {
			log.Printf("obelo: fetching markers of file %s: %v", fileID, err)
		}
	}()
	return done
}

// Close cancels every asking Start began and waits for it to end. An asking cut
// short saves nothing and logs nothing: the Server is shutting down, and the
// question is asked again on the next play.
func (s *Service) Close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.cancel()
	s.running.Wait()
}

// Serving reports whether any Marker provider would be asked now: one installed,
// enabled, and serving video. Fetched Markers are served only while one is.
func (s *Service) Serving() bool {
	return len(s.build()) > 0
}

// FetchFile is Fetch for the File fileID of the Title titleID — a Movie or an
// Episode — as a session plays it. Any other Title, and a File the Title does
// not hold, asks nothing.
func (s *Service) FetchFile(ctx context.Context, titleID, fileID string) error {
	if s.catalog == nil || len(s.reg.MarkerProviders()) == 0 {
		return nil
	}
	d, err := s.catalog.TitleByID(titleID)
	if err != nil {
		return err
	}
	if d.Kind != "movie" && d.Kind != "episode" {
		return nil
	}
	it := Item{Kind: d.Kind, Title: d.Title.Title, IDs: heldIDs(d.IdentityIDs, d.RecordIDs)}
	if d.EnrichedTitle != "" {
		it.Title = d.EnrichedTitle
	}
	for _, e := range d.Editions {
		for _, f := range e.Files {
			if f.ID == fileID {
				it.Path, it.DurationMs = f.Path, f.DurationMs
			}
		}
	}
	if it.Path == "" {
		return nil
	}
	if d.Kind == "movie" {
		it.Year = d.Year
		return s.Fetch(ctx, it)
	}
	c, err := s.catalog.EpisodeContextForTitle(titleID)
	if err != nil {
		return err
	}
	it.ShowTitle, it.SeasonNumber, it.EpisodeNumber = c.ShowTitle, c.SeasonNumber, c.EpisodeNumber
	if sh, err := s.catalog.ShowByID(c.ShowID); err == nil {
		it.ShowIDs = heldIDs(map[string]string{"tmdb": sh.TMDBID, "imdb": sh.IMDBID})
	}
	return s.Fetch(ctx, it)
}

// heldIDs merges id maps by namespace, a later map outranking an earlier one —
// a Title's record ids over the ids its folder asserts, as every other read
// does (ADR-0045, ADR-0060). Empty ids are left out; nil when none is held.
func heldIDs(maps ...map[string]string) map[string]string {
	var out map[string]string
	for _, m := range maps {
		for ns, id := range m {
			if id == "" {
				continue
			}
			if out == nil {
				out = map[string]string{}
			}
			out[ns] = id
		}
	}
	return out
}

// provider is one Marker provider built and ready to ask.
type provider struct {
	slug string
	p    pluginapi.MarkerProvider
}

// Fetch asks the Marker providers about the File of it unless the question was
// already answered, and stores what survives the host's judgment. A File with no
// path on this disk or no known length is never asked about: there is nothing
// to hold a candidate's timing against.
func (s *Service) Fetch(ctx context.Context, it Item) error {
	if it.Path == "" || it.DurationMs <= 0 {
		return nil
	}
	providers := s.build()
	if len(providers) == 0 {
		return nil
	}
	req := request(it)
	question := questionFor(req, providers)
	for {
		q, asked, err := s.store.MarkerFetchQuestion(it.Path)
		if err != nil {
			return err
		}
		if asked && q == question {
			return nil
		}
		done, lead := s.claim(it.Path)
		if !lead {
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return nil
			}
		}
		// An asking that finished between the read above and the claim has already
		// answered; asking again would put the question twice.
		if q, asked, err := s.store.MarkerFetchQuestion(it.Path); err != nil || (asked && q == question) {
			s.release(it.Path, done)
			return err
		}
		ms, settled := s.ask(ctx, it, req, providers)
		if ctx.Err() != nil {
			// Abandoned (the Server is shutting down): nothing was answered.
			s.release(it.Path, done)
			return nil
		}
		if !settled {
			question = ""
		}
		err = s.store.SaveFetchedMarkers(it.Path, question, ms)
		s.release(it.Path, done)
		return err
	}
}

// build makes every Marker provider serving video, in registration order. One
// whose factory refuses — a Plugin this server stopped calling — is not asked,
// and is not part of the question either.
func (s *Service) build() []provider {
	var out []provider
	for _, r := range s.reg.MarkerProviders() {
		if len(r.Descriptor.Kinds) > 0 && !r.Descriptor.Serves(pluginapi.KindVideo) {
			continue
		}
		p, err := r.New(pluginapi.Settings{Enabled: true, URL: r.Descriptor.DefaultURL})
		if err != nil {
			continue
		}
		out = append(out, provider{slug: r.Descriptor.Slug, p: p})
	}
	return out
}

// request is what every provider is asked about it. ask hands each provider its
// own copy of the id maps, so one scribbling on them cannot change what the next
// is asked.
func request(it Item) pluginapi.MarkersRequest {
	return pluginapi.MarkersRequest{
		Kind: it.Kind, Title: it.Title, Year: it.Year, IDs: it.IDs,
		ShowTitle: it.ShowTitle, ShowIDs: it.ShowIDs,
		SeasonNumber: it.SeasonNumber, EpisodeNumber: it.EpisodeNumber,
		DurationMs: it.DurationMs,
	}
}

// ask puts the question to each provider in order and returns the Markers the
// File keeps: per kind, the first acceptable candidate. settled is false when a
// provider failed — a failure is not an answer, so the question must be asked
// again. Each provider has providerTimeout to answer. Once ctx ends the asking
// is abandoned, and a provider cut short by it is not logged as failing.
func (s *Service) ask(ctx context.Context, it Item, req pluginapi.MarkersRequest, providers []provider) ([]store.Marker, bool) {
	var out []store.Marker
	have := map[string]bool{}
	settled := true
	for _, p := range providers {
		r := req
		r.IDs, r.ShowIDs = copyIDs(req.IDs), copyIDs(req.ShowIDs)
		pctx, cancel := context.WithTimeout(ctx, s.providerTimeout)
		resp, err := p.p.Markers(pctx, r)
		cancel()
		if ctx.Err() != nil {
			return nil, false
		}
		if err != nil {
			log.Printf("obelo: markers from %s: %v", p.slug, err)
			settled = false
			continue
		}
		for _, c := range resp.Markers {
			m, ok := judge(it.DurationMs, c)
			if !ok || have[m.Kind] {
				continue
			}
			have[m.Kind] = true
			out = append(out, m)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartMs < out[j].StartMs })
	return out, settled
}

// judge is the host's whole judgment on one candidate for a File of fileMs: the
// Marker to keep, and false when there is nothing to keep.
func judge(fileMs int64, c pluginapi.MarkerCandidate) (store.Marker, bool) {
	if !timedForThisFile(fileMs, c.DurationMs) {
		return store.Marker{}, false
	}
	switch c.Kind {
	case markers.KindIntro, markers.KindRecap, markers.KindCredits, markers.KindPreview:
	default:
		return store.Marker{}, false
	}
	end := min(c.EndMs, fileMs)
	if c.StartMs < 0 || end <= c.StartMs {
		return store.Marker{}, false
	}
	return store.Marker{Kind: c.Kind, Source: markers.SourceFetched, StartMs: c.StartMs, EndMs: end}, true
}

// timedForThisFile reports whether a span measured on a recording of answered
// ms can be trusted on a File of file ms. Either length unknown is a no.
func timedForThisFile(file, answered int64) bool {
	if file <= 0 || answered <= 0 {
		return false
	}
	diff := file - answered
	if diff < 0 {
		diff = -diff
	}
	return diff <= LengthToleranceMs
}

// questionFor is what the providers are asked, as a digest a remembered answer
// is compared against: the providers that would be asked, and everything the
// request carries.
func questionFor(req pluginapi.MarkersRequest, providers []provider) string {
	slugs := make([]string, len(providers))
	for i, p := range providers {
		slugs[i] = p.slug
	}
	b, _ := json.Marshal(struct {
		Providers []string                 `json:"providers"`
		Request   pluginapi.MarkersRequest `json:"request"`
	}{slugs, req})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func copyIDs(ids map[string]string) map[string]string {
	if ids == nil {
		return nil
	}
	out := make(map[string]string, len(ids))
	for k, v := range ids {
		out[k] = v
	}
	return out
}

// claim returns the channel that closes when the asking for path is done, and
// true when the caller is the one to do it.
func (s *Service) claim(path string) (chan struct{}, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.inflight[path]; ok {
		return ch, false
	}
	ch := make(chan struct{})
	s.inflight[path] = ch
	return ch, true
}

// release ends the asking for path.
func (s *Service) release(path string, ch chan struct{}) {
	s.mu.Lock()
	delete(s.inflight, path)
	s.mu.Unlock()
	close(ch)
}
