package enrich

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/store"
)

// ADR-0050's tracklist tier, driven through a REAL enrichment pass over a real
// store: a matched Album names the recording behind each of its own Tracks, and
// the pass looks each one up instead of searching for it.
//
// The assertions here are mostly about the CALL LOG, on purpose. "The album's
// tracklist resolves its tracks" and "each track searches for itself and happens
// to succeed" produce identical rows in the database; the only place they differ
// is in what was asked of MusicBrainz, and the cost on the endpoint that sheds
// load (ADR-0049) is the entire reason this tier exists.

// --- the fake provider --------------------------------------------------------

// albumTierProvider answers a Music pass from canned data and records every call
// it is asked to make, tagged by WHICH endpoint it would have been:
//
//	"rg:<id>"        a release-group lookup by id        (album parent, by id)
//	"rg?search"      a release-group SEARCH              (album parent, by name)
//	"tracklist:<rg>|<rel>|<n>"  one AlbumTracklist read
//	"recording:<id>" a /recording/<mbid> LOOKUP          (the cheap endpoint)
//	"search:<title>" a /recording?query= SEARCH          (the fragile one)
type albumTierProvider struct {
	mu    sync.Mutex
	calls []string

	albumRG      string            // what a release-group SEARCH resolves to ("" = ErrNoMatch)
	tracklist    []TrackCandidate  // what AlbumTracklist answers
	tracklistErr error             // ... unless this is set
	recordings   map[string]string // recording MBID → title, for the by-id lookup
	searchHits   map[string]string // local title → recording MBID a search would find
	searchErr    error             // when set, every track SEARCH fails with it
}

func (p *albumTierProvider) note(call string) {
	p.mu.Lock()
	p.calls = append(p.calls, call)
	p.mu.Unlock()
}

// count returns how many recorded calls start with prefix.
func (p *albumTierProvider) count(prefix string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// history returns a copy of every call the pass made, in order.
func (p *albumTierProvider) history() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

func (p *albumTierProvider) Lookup(_ context.Context, ref TitleRef) (TitleMetadata, error) {
	switch ref.Kind {
	case "artist":
		p.note("artist")
		return TitleMetadata{Matched: true, Name: ref.Title, ExternalID: "artist-1", Source: "musicbrainz"}, nil
	case "album":
		if id := strings.TrimSpace(ref.MusicbrainzID); id != "" {
			p.note("rg:" + id)
			return TitleMetadata{Matched: true, Name: ref.Album, ExternalID: id, Source: "musicbrainz"}, nil
		}
		p.note("rg?search")
		if p.albumRG == "" {
			return TitleMetadata{}, ErrNoMatch
		}
		return TitleMetadata{Matched: true, Name: ref.Album, ExternalID: p.albumRG, Source: "musicbrainz"}, nil
	case "track":
		if id := strings.TrimSpace(ref.MusicbrainzID); id != "" {
			p.note("recording:" + id)
			title, ok := p.recordings[id]
			if !ok {
				return TitleMetadata{}, ErrNoMatch // the real 404-on-a-stale-id shape
			}
			return TitleMetadata{Matched: true, Name: title, ExternalID: id, Source: "musicbrainz"}, nil
		}
		p.note("search:" + ref.Track)
		if p.searchErr != nil {
			return TitleMetadata{}, p.searchErr
		}
		id, ok := p.searchHits[ref.Track]
		if !ok {
			return TitleMetadata{}, ErrNoMatch
		}
		return TitleMetadata{Matched: true, Name: ref.Track, ExternalID: id, Source: "musicbrainz"}, nil
	}
	return TitleMetadata{}, ErrNoMatch
}

func (p *albumTierProvider) Search(context.Context, string, string, SearchOptions) ([]Candidate, error) {
	return nil, ErrSearchUnavailable
}

func (p *albumTierProvider) ArtworkCandidates(context.Context, TitleRef, string) ([]ArtworkCandidate, error) {
	return nil, ErrSearchUnavailable
}

func (p *albumTierProvider) AlbumTracklist(_ context.Context, req TracklistRequest) ([]TrackCandidate, error) {
	p.note(fmt.Sprintf("tracklist:%s|%s|%d", req.ReleaseGroupID, req.ReleaseID, req.LocalTrackCount))
	if p.tracklistErr != nil {
		return nil, p.tracklistErr
	}
	return p.tracklist, nil
}

// entry builds one tracklist position.
func entry(pos int, title, rec string) TrackCandidate {
	return TrackCandidate{Disc: 1, Position: pos, Title: title, ExternalID: rec}
}

// --- the fixture --------------------------------------------------------------

// seedTrack is one local Track as it sits in the database before the pass.
type seedTrack struct {
	id, title    string
	num          int
	recordingTag string // musicbrainz_recording_id — what the FILE asserts
	record       string // musicbrainz_id — the enrichment RECORD
	origin       string // enrichment_id_origin ('' derived, 'chosen', 'cascaded')
	status       string // enrichment_status ('' → the 'pending' default)
	retryAt      string // enrichment_retry_at ('' → parked; an instant → in-flight, ADR-0048)
	reason       string // enrichment_reason — the ADR-0050 diagnosis a previous pass wrote
}

// seedAlbum is one local Album and its Tracks.
type seedAlbum struct {
	rgTag        string // albums.musicbrainz_id — the release-group the FILES assert
	releaseTag   string // albums.musicbrainz_release_id — the edition the FILES assert
	entityRecord string // entity_enrichment.external_id, i.e. an already-matched Album
	// entityStatus is the Album's entity_enrichment.enrichment_status. Empty means
	// "matched when entityRecord is set, no row at all when it is not" — the two
	// shapes every test predating ADR-0051 wanted. A recheck test sets it explicitly
	// to seed the settled non-answers ('unmatched', or 'failed' with no retry).
	entityStatus string
	tracks       []seedTrack
}

// newAlbumFixture builds a Service over a real migrated DB holding one music
// Library → one Artist → one Album → the given Tracks.
func newAlbumFixture(t *testing.T, prov MetadataProvider, al seedAlbum) (*Service, *store.DB) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("seed (%s): %v", q, err)
		}
	}
	exec(`INSERT INTO libraries (id, name, kind) VALUES ('lib', 'Music', 'music')`)
	exec(`INSERT INTO artists (id, library_id, name, identity_key, sort_name)
	      VALUES ('ar1', 'lib', 'Harry Connick Jr.', 'artist:harry connick jr', 'harry connick jr')`)
	exec(`INSERT INTO albums (id, artist_id, title, identity_key, sort_title, musicbrainz_id, musicbrainz_release_id)
	      VALUES ('al1', 'ar1', 'She', 'artist:harry connick jr|album:she', 'she', ?, ?)`,
		al.rgTag, al.releaseTag)
	if al.entityRecord != "" || al.entityStatus != "" {
		status := al.entityStatus
		if status == "" {
			status = "matched"
		}
		exec(`INSERT INTO entity_enrichment (entity_type, entity_id, external_id, enrichment_status)
		      VALUES ('album', 'al1', ?, ?)`, al.entityRecord, status)
	}
	// The Artist is already settled so an only-new pass spends no call on it; these
	// tests are about the Album.
	exec(`INSERT INTO entity_enrichment (entity_type, entity_id, external_id, enrichment_status)
	      VALUES ('artist', 'ar1', 'artist-1', 'matched')`)
	for _, tr := range al.tracks {
		status := tr.status
		if status == "" {
			status = "pending"
		}
		exec(`INSERT INTO titles
		        (id, library_id, kind, title, identity_key, sort_title, album_id, disc_number, track_number,
		         musicbrainz_id, musicbrainz_recording_id, enrichment_id_origin, enrichment_status,
		         enrichment_retry_at, enrichment_reason)
		      VALUES (?, 'lib', 'track', ?, ?, ?, 'al1', 1, ?, ?, ?, ?, ?, ?, ?)`,
			tr.id, tr.title,
			"artist:harry connick jr|album:she|d01t"+fmt.Sprintf("%02d", tr.num)+":"+strings.ToLower(tr.title),
			strings.ToLower(tr.title), tr.num,
			tr.record, tr.recordingTag, tr.origin, status, tr.retryAt, tr.reason)
	}
	svc := NewService(db, prov, noArtwork{}, Enablement{Video: true, Music: true}, t.TempDir(), 0)
	svc.SetClock(func() time.Time { return time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC) })
	return svc, db
}

// trackRow reads back one Track's enrichment columns.
func trackRow(t *testing.T, db *store.DB, id string) store.Title {
	t.Helper()
	got, err := db.TitleForEnrichmentByID(id)
	if err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return got
}

// --- the tests ----------------------------------------------------------------

// The headline: a matched Album resolves every one of its Tracks for ONE call, and
// the search cluster is never touched. Before ADR-0050 this album cost three
// searches — and, on the day the operator hit it, three 503s.
func TestMatchedAlbumResolvesItsTracksInOneTracklistCall(t *testing.T) {
	prov := &albumTierProvider{
		tracklist: []TrackCandidate{
			entry(1, "Whisper Your Name", "rec-1"),
			entry(2, "She", "rec-2"),
			entry(3, "Follow the Music", "rec-3"),
		},
		recordings: map[string]string{"rec-1": "Whisper Your Name", "rec-2": "She", "rec-3": "Follow the Music"},
	}
	svc, db := newAlbumFixture(t, prov, seedAlbum{
		entityRecord: "rg-she",
		tracks: []seedTrack{
			{id: "t1", title: "Whisper Your Name", num: 1},
			{id: "t2", title: "She", num: 2},
			{id: "t3", title: "Follow the Music", num: 3},
		},
	})

	res, err := svc.EnrichLibrary(context.Background(), "lib", ModeNew)
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if res.Matched != 3 {
		t.Fatalf("matched %d of 3 (calls: %v)", res.Matched, prov.history())
	}
	if n := prov.count("tracklist:"); n != 1 {
		t.Fatalf("%d tracklist calls, want exactly 1 — the tier is album-grained, so three "+
			"tracks must cost one read (calls: %v)", n, prov.history())
	}
	if n := prov.count("search:"); n != 0 {
		t.Fatalf("%d recording SEARCHES, want 0 — the album named every one of these "+
			"recordings, and the search cluster is the dependency ADR-0049 watched shed "+
			"load (calls: %v)", n, prov.history())
	}
	if n := prov.count("recording:"); n != 3 {
		t.Fatalf("%d recording lookups, want 3 — the tracklist supplies an ID and the leaf "+
			"still resolves through the ordinary lookup (calls: %v)", n, prov.history())
	}
	// The request carried the anchor and the local track count, not just an id.
	if got, want := prov.history()[0], "tracklist:rg-she||3"; got != want {
		t.Fatalf("tracklist request %q, want %q — the album's record id is the anchor and "+
			"the local track count is what picks the right edition", got, want)
	}
	for id, want := range map[string]string{"t1": "rec-1", "t2": "rec-2", "t3": "rec-3"} {
		got := trackRow(t, db, id)
		if got.MusicbrainzID != want {
			t.Errorf("%s recorded %q, want %q", id, got.MusicbrainzID, want)
		}
		if got.EnrichmentStatus != "matched" {
			t.Errorf("%s status %q, want matched", id, got.EnrichmentStatus)
		}
	}
}

// The Album's tag release-group id is the anchor when the Album has no record of
// its own, and the tag RELEASE id is passed as the edition the files name.
func TestTracklistAnchorFallsBackToTheTagIDsAndPassesTheRelease(t *testing.T) {
	prov := &albumTierProvider{
		tracklist:  []TrackCandidate{entry(1, "She", "rec-2")},
		recordings: map[string]string{"rec-2": "She"},
	}
	svc, _ := newAlbumFixture(t, prov, seedAlbum{
		rgTag:      "rg-tag",
		releaseTag: "rel-tag",
		tracks:     []seedTrack{{id: "t1", title: "She", num: 1}},
	})

	if _, err := svc.EnrichLibrary(context.Background(), "lib", ModeNew); err != nil {
		t.Fatalf("pass: %v", err)
	}
	// The album parent resolved BY its tag release-group id, and that id is what the
	// tracklist request is anchored on; the tag release rides along as ReleaseID.
	want := "tracklist:rg-tag|rel-tag|1"
	found := false
	for _, c := range prov.history() {
		if c == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("calls %v, want one %q — the tag release-group is the fallback anchor and "+
			"the tag release is the edition the files were ripped against", prov.history(), want)
	}
}

// A Track the mapping declines is not settled by this tier: it falls through to
// the search path exactly as it did before, untouched.
func TestADeclinedTrackStillReachesTheSearch(t *testing.T) {
	prov := &albumTierProvider{
		// Two spare positions, so the leftover rule cannot rescue the odd track out.
		tracklist: []TrackCandidate{
			entry(1, "Whisper Your Name", "rec-1"),
			entry(2, "Bonus One", "rec-x"),
			entry(3, "Bonus Two", "rec-y"),
		},
		recordings: map[string]string{"rec-1": "Whisper Your Name", "rec-3": "Recovered By Search"},
		searchHits: map[string]string{"Nowhere On The Release": "rec-3"},
	}
	svc, db := newAlbumFixture(t, prov, seedAlbum{
		entityRecord: "rg-she",
		tracks: []seedTrack{
			{id: "t1", title: "Whisper Your Name", num: 1},
			{id: "t2", title: "Nowhere On The Release", num: 2},
		},
	})

	if _, err := svc.EnrichLibrary(context.Background(), "lib", ModeNew); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if n := prov.count("search:Nowhere On The Release"); n != 1 {
		t.Fatalf("the declined track searched %d times, want 1 — a decline must fall through "+
			"to the old path, not settle the track (calls: %v)", n, prov.history())
	}
	if got := trackRow(t, db, "t2").MusicbrainzID; got != "rec-3" {
		t.Fatalf("declined track recorded %q, want the search's rec-3", got)
	}
	if got := trackRow(t, db, "t1").MusicbrainzID; got != "rec-1" {
		t.Fatalf("mapped track recorded %q, want rec-1", got)
	}
}

// No anchor, no call. An Album that is neither matched nor tagged knows nothing
// about its contents and must not be asked — asking would spend a request per
// album on every untagged library to learn that.
func TestAlbumWithNoAnchorMakesNoTracklistCall(t *testing.T) {
	prov := &albumTierProvider{ // albumRG "" → the album search finds nothing
		searchHits: map[string]string{"She": "rec-2"},
	}
	svc, _ := newAlbumFixture(t, prov, seedAlbum{
		tracks: []seedTrack{{id: "t1", title: "She", num: 1}},
	})

	if _, err := svc.EnrichLibrary(context.Background(), "lib", ModeNew); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if n := prov.count("tracklist:"); n != 0 {
		t.Fatalf("%d tracklist calls, want 0 — an unmatched, untagged album has no anchor "+
			"to ask about (calls: %v)", n, prov.history())
	}
	if n := prov.count("search:She"); n != 1 {
		t.Fatalf("the track searched %d times, want 1 — the old path is unchanged for an "+
			"album that cannot help (calls: %v)", n, prov.history())
	}
}

// Nothing to resolve, no call. A fully tagged album — the population ADR-0049
// already fixed — must not start paying one call per album to learn that.
func TestAlbumWhoseTracksAllCarryIDsMakesNoTracklistCall(t *testing.T) {
	prov := &albumTierProvider{
		recordings: map[string]string{"tag-1": "Whisper Your Name", "rec-2": "She"},
	}
	svc, _ := newAlbumFixture(t, prov, seedAlbum{
		entityRecord: "rg-she",
		tracks: []seedTrack{
			{id: "t1", title: "Whisper Your Name", num: 1, recordingTag: "tag-1"},
			{id: "t2", title: "She", num: 2, record: "rec-2"},
		},
	})

	if _, err := svc.EnrichLibrary(context.Background(), "lib", ModeNew); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if n := prov.count("tracklist:"); n != 0 {
		t.Fatalf("%d tracklist calls, want 0 — every track already carries an anchor, so "+
			"there is nothing a tracklist could resolve (calls: %v)", n, prov.history())
	}
	if n := prov.count("recording:"); n != 2 {
		t.Fatalf("%d recording lookups, want 2 (calls: %v)", n, prov.history())
	}
}

// ModeNew still means only-new. An album whose Tracks are all settled is out of
// scope, and the tier does not drag it back in by asking about its tracklist.
func TestModeNewSkipsSettledLeavesAndAsksForNoTracklist(t *testing.T) {
	prov := &albumTierProvider{
		// A Full pass re-resolves the Album parent too, so the album search has to
		// answer for the second half of this test.
		albumRG:    "rg-she",
		tracklist:  []TrackCandidate{entry(1, "She", "rec-2")},
		recordings: map[string]string{"rec-2": "She"},
	}
	svc, _ := newAlbumFixture(t, prov, seedAlbum{
		entityRecord: "rg-she",
		// Settled with no id at all: nothing here is anchored, so only SCOPE can be
		// what keeps the tier quiet.
		tracks: []seedTrack{{id: "t1", title: "She", num: 1, status: "matched"}},
	})

	res, err := svc.EnrichLibrary(context.Background(), "lib", ModeNew)
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if res.Total != 0 {
		t.Fatalf("pass touched %d leaves, want 0 in ModeNew", res.Total)
	}
	if n := prov.count("tracklist:"); n != 0 {
		t.Fatalf("%d tracklist calls, want 0 — no track in this album is in scope for the "+
			"pass (calls: %v)", n, prov.history())
	}

	// ... and a Full pass, which IS in scope, asks.
	if _, err := svc.EnrichLibrary(context.Background(), "lib", ModeFull); err != nil {
		t.Fatalf("full pass: %v", err)
	}
	if n := prov.count("tracklist:"); n != 1 {
		t.Fatalf("%d tracklist calls in the full pass, want 1 (calls: %v)", n, prov.history())
	}
}

// The stale-record rule. SetTitleEnrichmentStatus writes the status and leaves
// musicbrainz_id alone, so a Track that matched once and later failed carries a
// record AND reads 'unmatched' — five such rows exist in the developer's library,
// all on one album. A tier that read "has an id" as "is anchored" would decide
// this album has nothing left to resolve and never ask.
func TestAStaleRecordStillCountsAsNeedingATracklist(t *testing.T) {
	prov := &albumTierProvider{
		albumRG: "rg-she", // a Full pass re-resolves the Album parent as well
		tracklist: []TrackCandidate{
			entry(1, "Whisper Your Name", "rec-1"),
			entry(2, "She", "rec-2"),
		},
		// "stale-rec" resolves to nothing: the id is in the row, the record is gone.
		recordings: map[string]string{"rec-2": "She"},
	}
	svc, db := newAlbumFixture(t, prov, seedAlbum{
		entityRecord: "rg-she",
		tracks: []seedTrack{
			{id: "t1", title: "Whisper Your Name", num: 1, record: "stale-rec", status: "unmatched"},
			{id: "t2", title: "She", num: 2, record: "rec-2", status: "matched"},
		},
	})

	if _, err := svc.EnrichLibrary(context.Background(), "lib", ModeFull); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if n := prov.count("tracklist:"); n != 1 {
		t.Fatalf("%d tracklist calls, want 1 — a Track reading 'unmatched' still needs "+
			"resolution however many ids its row happens to carry (calls: %v)", n, prov.history())
	}
	// ... and the stale record is STILL looked up first: the tracklist supplies a
	// lower tier, it does not overrule the record column.
	if n := prov.count("recording:stale-rec"); n != 1 {
		t.Fatalf("the stale record was looked up %d times, want 1 — the precedence puts the "+
			"record first, and that lookup is what either clears the row or diagnoses it "+
			"(calls: %v)", n, prov.history())
	}
	if n := prov.count("recording:rec-1"); n != 0 {
		t.Fatalf("the tracklist's id overruled the record column (calls: %v)", prov.history())
	}
	if got := trackRow(t, db, "t1").EnrichmentStatus; got != "unmatched" {
		t.Fatalf("stale-record track ended %q, want unmatched", got)
	}
}

// All four tiers of ADR-0050's precedence, end to end, in one pass:
//
//	t1  the Admin's Fix-info record  → looked up by the record, tracklist ignored
//	t2  the file's own tag id        → looked up by the tag, tracklist ignored
//	t3  neither, but on the tracklist → looked up by the tracklist's id
//	t4  neither, and declined         → the search, last resort
func TestTrackAnchorPrecedenceAcrossAllFourTiers(t *testing.T) {
	prov := &albumTierProvider{
		albumRG: "rg-she", // a Full pass re-resolves the Album parent as well
		tracklist: []TrackCandidate{
			entry(1, "Chosen Track", "tracklist-1"),
			entry(2, "Tagged Track", "tracklist-2"),
			entry(3, "Derived Track", "tracklist-3"),
			entry(4, "Spare One", "tracklist-spare-a"),
			entry(5, "Spare Two", "tracklist-spare-b"),
		},
		recordings: map[string]string{
			"chosen-rec": "Chosen Track", "tag-rec": "Tagged Track",
			"tracklist-3": "Derived Track", "searched-rec": "Searched Track",
		},
		searchHits: map[string]string{"Searched Track": "searched-rec"},
	}
	svc, db := newAlbumFixture(t, prov, seedAlbum{
		entityRecord: "rg-she",
		tracks: []seedTrack{
			{id: "t1", title: "Chosen Track", num: 1, record: "chosen-rec", origin: "chosen", recordingTag: "tag-ignored"},
			{id: "t2", title: "Tagged Track", num: 2, recordingTag: "tag-rec"},
			{id: "t3", title: "Derived Track", num: 3},
			// Not on the release at all, and two spare positions mean the leftover rule
			// cannot pair it with one.
			{id: "t4", title: "Searched Track", num: 6},
		},
	})

	if _, err := svc.EnrichLibrary(context.Background(), "lib", ModeFull); err != nil {
		t.Fatalf("pass: %v", err)
	}
	for _, want := range []string{
		"recording:chosen-rec", // the human's correction outranks everything
		"recording:tag-rec",    // the file outranks the album
		"recording:tracklist-3",
		"search:Searched Track",
	} {
		if n := prov.count(want); n != 1 {
			t.Fatalf("%q happened %d times, want 1 — the four-tier precedence is record → "+
				"tag → album tracklist → search (calls: %v)", want, n, prov.history())
		}
	}
	if n := prov.count("recording:tracklist-1"); n != 0 {
		t.Fatalf("the tracklist overruled an Admin's Fix-info choice (calls: %v)", prov.history())
	}
	if n := prov.count("recording:tracklist-2"); n != 0 {
		t.Fatalf("the tracklist overruled the file's own tag id (calls: %v)", prov.history())
	}
	if n := prov.count("search:"); n != 1 {
		t.Fatalf("%d searches, want exactly 1 — only the declined track may reach the search "+
			"cluster (calls: %v)", n, prov.history())
	}
	if got := trackRow(t, db, "t1").MusicbrainzID; got != "chosen-rec" {
		t.Errorf("the chosen record became %q", got)
	}
	if got := trackRow(t, db, "t3").MusicbrainzID; got != "tracklist-3" {
		t.Errorf("the tracklist-derived record became %q, want tracklist-3", got)
	}
}

// A tracklist-derived record is nobody's choice (ADR-0046): OriginDerived, the
// zero value, so the NEXT pass re-derives it rather than skipping it. Writing
// OriginCascaded here would both lie about who decided and make every auto-match
// immune to every later pass.
func TestTracklistDerivedRecordIsDerivedAndReDerivedByAFullPass(t *testing.T) {
	prov := &albumTierProvider{
		tracklist:  []TrackCandidate{entry(1, "She", "rec-2")},
		recordings: map[string]string{"rec-2": "She"},
	}
	svc, db := newAlbumFixture(t, prov, seedAlbum{
		entityRecord: "rg-she",
		tracks:       []seedTrack{{id: "t1", title: "She", num: 1}},
	})
	ctx := context.Background()

	if _, err := svc.EnrichLibrary(ctx, "lib", ModeNew); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	got := trackRow(t, db, "t1")
	if got.EnrichmentIDOrigin != store.OriginDerived {
		t.Fatalf("origin %q, want %q (OriginDerived) — a pass's own result is derived, and "+
			"anything durable here would make an auto-match immune to the next pass",
			got.EnrichmentIDOrigin, store.OriginDerived)
	}
	if got.EnrichmentIDOrigin.Locked() {
		t.Fatal("the tracklist-derived record reports itself as locked")
	}

	// A Full pass revisits it — the record is now its anchor, so it resolves by
	// lookup and never needs the tracklist again.
	res, err := svc.EnrichLibrary(ctx, "lib", ModeFull)
	if err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if res.Matched != 1 {
		t.Fatalf("full pass matched %d, want 1 — the derived record was skipped rather than "+
			"re-derived (calls: %v)", res.Matched, prov.history())
	}
	if n := prov.count("recording:rec-2"); n != 2 {
		t.Fatalf("the record was looked up %d times over two passes, want 2 (calls: %v)",
			n, prov.history())
	}
}

// Issue 03 keeps an id-less tracklist entry IN POSITION, so a neighbour cannot
// claim it — but "matched with no id" is not an anchor. Writing it would pin the
// empty string and send the leaf to a lookup of nothing.
func TestATracklistEntryWithNoRecordingIDIsNotAnAnchor(t *testing.T) {
	prov := &albumTierProvider{
		tracklist:  []TrackCandidate{entry(1, "She", "")}, // MusicBrainz has the track, no recording
		searchHits: map[string]string{"She": "searched-rec"},
	}
	svc, db := newAlbumFixture(t, prov, seedAlbum{
		entityRecord: "rg-she",
		tracks:       []seedTrack{{id: "t1", title: "She", num: 1}},
	})

	if _, err := svc.EnrichLibrary(context.Background(), "lib", ModeNew); err != nil {
		t.Fatalf("pass: %v", err)
	}
	for _, c := range prov.history() {
		if c == "recording:" {
			t.Fatalf("an empty id was pinned and looked up (calls: %v)", prov.history())
		}
	}
	if n := prov.count("search:She"); n != 1 {
		t.Fatalf("the id-less match searched %d times, want 1 (calls: %v)", n, prov.history())
	}
	if got := trackRow(t, db, "t1").MusicbrainzID; got != "searched-rec" {
		t.Fatalf("recorded %q, want the search's id", got)
	}
}

// A TRANSIENT tracklist failure must not settle the album's tracks. The album
// simply has no mapping this pass; its tracks fall through to search exactly as
// they do today, and if that search also fails transiently the ordinary ADR-0048
// classification schedules the retry. The tracklist gets no retry machinery of
// its own — the next pass re-reads it for free.
func TestATransientTracklistFailureLeavesTheTracksRetryable(t *testing.T) {
	shed := statusError("musicbrainz", "/release", http.StatusServiceUnavailable)
	prov := &albumTierProvider{
		tracklistErr: shed,
		searchErr:    statusError("musicbrainz", "/recording", http.StatusServiceUnavailable),
	}
	svc, db := newAlbumFixture(t, prov, seedAlbum{
		entityRecord: "rg-she",
		tracks:       []seedTrack{{id: "t1", title: "She", num: 1}},
	})
	if !IsTransient(shed) {
		t.Fatal("the fixture's error is not transient; the test proves nothing")
	}

	res, err := svc.EnrichLibrary(context.Background(), "lib", ModeNew)
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if n := prov.count("search:She"); n != 1 {
		t.Fatalf("the track searched %d times, want 1 — a failed tracklist read is not a leaf "+
			"failure, so the track falls through to the path it used before (calls: %v)",
			n, prov.history())
	}
	if res.Retrying != 1 || res.Unmatched != 0 || res.Failed != 0 {
		t.Fatalf("retrying=%d unmatched=%d failed=%d, want retrying=1 — a 503 on the "+
			"tracklist must not settle a track as 'no match'",
			res.Retrying, res.Unmatched, res.Failed)
	}
	got := trackRow(t, db, "t1")
	if got.EnrichmentStatus != "failed" || got.EnrichmentRetryAt == "" {
		t.Fatalf("status %q retry_at %q, want a scheduled retry — the track is parked on an "+
			"outage", got.EnrichmentStatus, got.EnrichmentRetryAt)
	}

	// The outage clears: the next only-new pass reads the tracklist and settles the
	// track by lookup, with no operator gesture in between.
	prov.tracklistErr, prov.searchErr = nil, nil
	prov.tracklist = []TrackCandidate{entry(1, "She", "rec-2")}
	prov.recordings = map[string]string{"rec-2": "She"}
	svc.SetClock(func() time.Time { return time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC).Add(retryDelay(1)) })

	res, err = svc.EnrichLibrary(context.Background(), "lib", ModeNew)
	if err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if res.Matched != 1 {
		t.Fatalf("pass 2 matched %d, want 1 (calls: %v)", res.Matched, prov.history())
	}
	if got := trackRow(t, db, "t1").MusicbrainzID; got != "rec-2" {
		t.Fatalf("recorded %q, want rec-2 from the recovered tracklist", got)
	}
}

// The settled half of the same split: ErrNoTracklist says "this album HAS no
// tracklist", which is a fact and not an outage. The track still falls through to
// search — the difference is that nothing here is retried on the album's behalf.
func TestNoTracklistIsSettledAndFallsThroughToSearch(t *testing.T) {
	prov := &albumTierProvider{
		tracklistErr: ErrNoTracklist,
		searchHits:   map[string]string{"She": "searched-rec"},
	}
	svc, db := newAlbumFixture(t, prov, seedAlbum{
		entityRecord: "rg-she",
		tracks:       []seedTrack{{id: "t1", title: "She", num: 1}},
	})

	res, err := svc.EnrichLibrary(context.Background(), "lib", ModeNew)
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if res.Matched != 1 {
		t.Fatalf("matched %d, want 1 (calls: %v)", res.Matched, prov.history())
	}
	if got := trackRow(t, db, "t1").MusicbrainzID; got != "searched-rec" {
		t.Fatalf("recorded %q, want the search's id", got)
	}
}

// Music enrichment off: the gate inside Service.albumTracklist is what answers,
// and no provider call is made at all.
func TestTracklistIsNotFetchedWhenMusicEnrichmentIsOff(t *testing.T) {
	prov := &albumTierProvider{tracklist: []TrackCandidate{entry(1, "She", "rec-2")}}
	svc, _ := newAlbumFixture(t, prov, seedAlbum{
		entityRecord: "rg-she",
		tracks:       []seedTrack{{id: "t1", title: "She", num: 1}},
	})
	svc.SetProvider(prov, Enablement{Video: true, Music: false})

	if _, err := svc.EnrichLibrary(context.Background(), "lib", ModeNew); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if n := len(prov.history()); n != 0 {
		t.Fatalf("%d provider calls with music enrichment off, want 0: %v", n, prov.history())
	}
}
