package enrich

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/store"
)

// The pass half of ADR-0048, driven against a REAL store over a fake provider:
// a Title lost to a provider outage must come back on its own, and one lost to a
// rejected credential must not.
//
// This is the test that would have failed before the change. The old pass wrote
// 'failed' for every provider error and the only-new selection matched only
// 'pending', so pass two never looked at the Title again — no matter how healthy
// the provider had become.

// retryFakeProvider answers Lookup from a script: one entry per call, reused for
// every call past the end.
type retryFakeProvider struct {
	answers []error // nil = match
	calls   int
}

func (p *retryFakeProvider) Lookup(context.Context, TitleRef) (TitleMetadata, error) {
	i := p.calls
	p.calls++
	if i >= len(p.answers) {
		i = len(p.answers) - 1
	}
	if err := p.answers[i]; err != nil {
		return TitleMetadata{}, err
	}
	return TitleMetadata{
		Matched: true, Name: "Inception", Overview: "A thief who steals corporate secrets.",
		ExternalID: "27205", Source: "tmdb",
	}, nil
}

func (p *retryFakeProvider) Search(context.Context, string, string, SearchOptions) ([]Candidate, error) {
	return nil, ErrSearchUnavailable
}

func (p *retryFakeProvider) ArtworkCandidates(context.Context, TitleRef, string) ([]ArtworkCandidate, error) {
	return nil, ErrSearchUnavailable
}

// noArtwork is an ArtworkFetcher that is never expected to be called (the fake
// provider returns no artwork refs).
type noArtwork struct{}

func (noArtwork) Fetch(context.Context, string) ([]byte, string, error) {
	return nil, "", ErrArtworkNotFound
}

// newRetryFixture builds a Service over a real migrated DB holding one Movie, with
// a clock the test drives.
func newRetryFixture(t *testing.T, prov MetadataProvider) (*Service, *store.DB, *time.Time) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO libraries (id, name, kind) VALUES ('lib', 'Movies', 'movie')`); err != nil {
		t.Fatalf("seed library: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO titles (id, library_id, kind, title, year, identity_key, sort_title)
		 VALUES ('m1', 'lib', 'movie', 'Inception', 2010, 'inception|2010', 'inception')`); err != nil {
		t.Fatalf("seed title: %v", err)
	}

	clock := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	svc := NewService(db, prov, noArtwork{}, Enablement{Video: true, Music: true}, t.TempDir(), 0)
	svc.SetClock(func() time.Time { return clock })
	return svc, db, &clock
}

func TestTransientFailureIsRetriedByALaterPass(t *testing.T) {
	prov := &retryFakeProvider{answers: []error{
		statusError("tmdb", "/search/movie", http.StatusServiceUnavailable), // the outage
		nil, // the provider is back
	}}
	svc, db, clock := newRetryFixture(t, prov)
	ctx := context.Background()

	// Pass one: the provider is down.
	res, err := svc.EnrichLibrary(ctx, "lib", ModeNew)
	if err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if res.Retrying != 1 || res.Failed != 0 {
		t.Fatalf("pass 1 reported retrying=%d failed=%d, want retrying=1 failed=0 — an outage is "+
			"being reported to the operator as work they have to do", res.Retrying, res.Failed)
	}
	got, err := db.TitleForEnrichmentByID("m1")
	if err != nil {
		t.Fatal(err)
	}
	if got.EnrichmentRetryAt == "" {
		t.Fatal("no retry scheduled after a 503: the Title is parked, which is the bug")
	}

	// A pass before the backoff expires must not re-ask — the provider is already
	// struggling, and every scan would otherwise re-hammer it.
	*clock = clock.Add(time.Minute)
	if _, err := svc.EnrichLibrary(ctx, "lib", ModeNew); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if prov.calls != 1 {
		t.Fatalf("provider called %d times, want 1 — the backoff is not being honored", prov.calls)
	}

	// Once the backoff has expired the next ordinary only-new pass — the one a scan
	// fires, with nobody asking for a full refresh — picks the Title up and settles it.
	*clock = clock.Add(retryDelay(1))
	res, err = svc.EnrichLibrary(ctx, "lib", ModeNew)
	if err != nil {
		t.Fatalf("pass 3: %v", err)
	}
	if res.Matched != 1 {
		t.Fatalf("pass 3 matched %d, want 1 — the due retry never happened, so the Title stays "+
			"un-enriched until somebody runs a full pass by hand", res.Matched)
	}
	got, err = db.TitleForEnrichmentByID("m1")
	if err != nil {
		t.Fatal(err)
	}
	if got.EnrichmentStatus != "matched" {
		t.Fatalf("status = %q, want matched", got.EnrichmentStatus)
	}
	if got.Overview == "" {
		t.Error("the Title matched but carries no overview — the retry resolved nothing")
	}
	if got.EnrichmentAttempts != 0 || got.EnrichmentRetryAt != "" {
		t.Errorf("a recovered Title still carries retry bookkeeping (attempts=%d, retryAt=%q)",
			got.EnrichmentAttempts, got.EnrichmentRetryAt)
	}
}

// The backoff must actually climb across passes, not restart at the first step
// each time — otherwise a provider that is down for a day is re-asked every scan.
func TestConsecutiveFailuresLengthenTheBackoff(t *testing.T) {
	prov := &retryFakeProvider{answers: []error{
		statusError("tmdb", "/search/movie", http.StatusBadGateway),
	}}
	svc, db, clock := newRetryFixture(t, prov)
	ctx := context.Background()

	for attempt := 1; attempt <= 3; attempt++ {
		if _, err := svc.EnrichLibrary(ctx, "lib", ModeNew); err != nil {
			t.Fatalf("pass %d: %v", attempt, err)
		}
		got, err := db.TitleForEnrichmentByID("m1")
		if err != nil {
			t.Fatal(err)
		}
		if got.EnrichmentAttempts != attempt {
			t.Fatalf("after %d failures attempts = %d — the streak is not accumulating, so the "+
				"backoff never lengthens and never escalates", attempt, got.EnrichmentAttempts)
		}
		want := clock.Add(retryDelay(attempt)).Format(time.RFC3339)
		if got.EnrichmentRetryAt != want {
			t.Fatalf("after %d failures retryAt = %q, want %q", attempt, got.EnrichmentRetryAt, want)
		}
		*clock = clock.Add(retryDelay(attempt))
	}
}

// A permanent failure keeps the old behavior exactly: parked, on the attention
// list, no retry. Retrying a rejected key forever would be a different silent
// failure, not a fix for this one.
func TestPermanentFailureStillParks(t *testing.T) {
	prov := &retryFakeProvider{answers: []error{
		statusError("tmdb", "/search/movie", http.StatusUnauthorized),
	}}
	svc, db, _ := newRetryFixture(t, prov)

	res, err := svc.EnrichLibrary(context.Background(), "lib", ModeNew)
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if res.Failed != 1 || res.Retrying != 0 {
		t.Fatalf("reported failed=%d retrying=%d, want failed=1 retrying=0 — a rejected API key "+
			"is being treated as a blip the server can wait out", res.Failed, res.Retrying)
	}
	got, err := db.TitleForEnrichmentByID("m1")
	if err != nil {
		t.Fatal(err)
	}
	if got.EnrichmentStatus != "failed" || got.EnrichmentRetryAt != "" {
		t.Fatalf("status=%q retryAt=%q, want failed with no retry scheduled",
			got.EnrichmentStatus, got.EnrichmentRetryAt)
	}
	needing, err := db.TitlesNeedingMatch("lib")
	if err != nil {
		t.Fatal(err)
	}
	if len(needing) != 1 {
		t.Fatalf("attention list has %d rows, want the parked Title", len(needing))
	}
}

// A browse PARENT lost to an outage must come back too, and this is where the old
// behavior did the most damage: enrichParent returns early for any parent that is
// not 'pending', so one 503 on a Show parked it — and with it every Season and
// Episode underneath, none of which appeared on any list as a problem.
func TestTransientParentFailureIsRetried(t *testing.T) {
	prov := &retryFakeProvider{answers: []error{
		statusError("tmdb", "/search/tv", http.StatusServiceUnavailable),
		nil,
	}}
	svc, db, clock := newRetryFixture(t, prov)
	if _, err := db.Exec(
		`INSERT INTO libraries (id, name, kind) VALUES ('tv', 'TV', 'tv')`); err != nil {
		t.Fatalf("seed library: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO shows (id, library_id, title, identity_key, sort_title)
		 VALUES ('sh1', 'tv', 'The Bear', 'the bear', 'bear')`); err != nil {
		t.Fatalf("seed show: %v", err)
	}
	ctx := context.Background()

	if _, err := svc.EnrichLibrary(ctx, "tv", ModeNew); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	got, err := db.EntityEnrichmentByID(store.EntityShow, "sh1")
	if err != nil {
		t.Fatal(err)
	}
	if got.RetryAt == "" {
		t.Fatal("a Show lost to a 503 has no retry scheduled — every Season and Episode under " +
			"it stays un-enriched, with nothing anywhere saying why")
	}

	*clock = clock.Add(retryDelay(1))
	if _, err := svc.EnrichLibrary(ctx, "tv", ModeNew); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	got, err = db.EntityEnrichmentByID(store.EntityShow, "sh1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "matched" {
		t.Fatalf("Show status = %q after the retry came due, want matched — the parent "+
			"short-circuit is still skipping it", got.Status)
	}
	if got.Attempts != 0 || got.RetryAt != "" {
		t.Errorf("a recovered Show still carries retry bookkeeping (attempts=%d, retryAt=%q)",
			got.Attempts, got.RetryAt)
	}
}

// The payoff of ADR-0049: a tagged library resolves by LOOKUP, never touching the
// search endpoint that is the thing actually falling over. This asserts the URL
// path, because "it still matched" would pass just as well with a search.
func TestTaggedTracksResolveByLookupNotSearch(t *testing.T) {
	const recording = "b9ad642e-b012-41c7-b72a-42cf4911a0f1"
	var paths []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		fmt.Fprint(w, `{"id":"`+recording+`","title":"Roygbiv"}`)
	}))
	defer srv.Close()

	p := NewMusicBrainzProvider(srv.URL, srv.URL, "en")
	p.MinInterval = 0

	// A Track whose file carried a recording id in its tags.
	_, err := p.Lookup(context.Background(), TitleRef{
		Kind: "track", Track: "Roygbiv", Artist: "Boards of Canada",
		MusicbrainzID: trackRecordID(store.Title{MusicbrainzRecordingID: recording}),
	})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	mu.Lock()
	got := paths
	mu.Unlock()
	if len(got) != 1 || got[0] != "/recording/"+recording {
		t.Fatalf("requested %v, want a single /recording/<mbid> lookup — a tagged library is "+
			"still going through /recording?query=, the search cluster that sheds load", got)
	}
}

// The precedence between the two ids a Track can carry. Getting this backwards
// would let a rescan's tag id quietly overrule the Admin's Fix-info correction.
func TestTrackRecordIDPrefersTheAdminsRecordOverTheTag(t *testing.T) {
	const record = "11111111-1111-4111-8111-111111111111"
	const tagged = "22222222-2222-4222-8222-222222222222"

	if got := trackRecordID(store.Title{MusicbrainzID: record, MusicbrainzRecordingID: tagged}); got != record {
		t.Errorf("got %q, want the enrichment record %q — the file's tag is overruling a "+
			"human's correction, which every scan would then re-apply", got, record)
	}
	if got := trackRecordID(store.Title{MusicbrainzRecordingID: tagged}); got != tagged {
		t.Errorf("got %q, want the tag id %q", got, tagged)
	}
	if got := trackRecordID(store.Title{}); got != "" {
		t.Errorf("got %q, want empty so the provider falls back to a search", got)
	}
}
