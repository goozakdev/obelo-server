package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Black-box tests for the four sink events issue 06 adds, for the attribution rule
// that governs the two playback ones, and for the delivery counters
// (.scratch/plugin-system issue 06, ADR-0057 decision 6).
//
// Same posture as event_sinks_test.go: nothing here reaches inside the server. A
// document arrives at an httptest server standing in for the operator's receiver,
// and the counters are read off the settings response an Admin's screen reads.
// That is the whole point of surfacing them — the assertions a sink's behavior
// needs are now the numbers the Admin sees, not a package internal.

// --- the document a receiver parses -----------------------------------------

// sinkDoc is the wire shape of a curated event as a receiving script would model
// it. Actor and Device are pointers so a test can tell "the block was absent" from
// "the block was there and empty", which is the whole assertion for a relayed
// play.
type sinkDoc struct {
	ID      string       `json:"id"`
	Type    string       `json:"type"`
	At      string       `json:"at"`
	Library *sinkDocEnt  `json:"library"`
	Title   *sinkDocEnt  `json:"title"`
	Actor   *sinkDocActr `json:"actor"`
	Device  *sinkDocEnt  `json:"device"`
	Scan    *struct {
		TitlesFound int `json:"titlesFound"`
		FilesFound  int `json:"filesFound"`
	} `json:"scan"`
	Enrich *struct {
		Total     int `json:"total"`
		Done      int `json:"done"`
		Matched   int `json:"matched"`
		Unmatched int `json:"unmatched"`
		Disabled  int `json:"disabled"`
	} `json:"enrich"`
}

type sinkDocEnt struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type sinkDocActr struct {
	UserID string `json:"userId"`
	LinkID string `json:"linkId"`
	Name   string `json:"name"`
}

// parseSinkDocs decodes every recorded POST body as a sink event.
func parseSinkDocs(t *testing.T, posts []sinkPost) []sinkDoc {
	t.Helper()
	out := make([]sinkDoc, 0, len(posts))
	for _, p := range posts {
		var doc sinkDoc
		if err := json.Unmarshal(p.Body, &doc); err != nil {
			t.Fatalf("a delivered body is not a sink event: %v\nbody: %s", err, p.Body)
		}
		if doc.ID == "" || doc.ID != p.EventID {
			t.Fatalf("event id %q disagrees with the header %q — a receiver cannot deduplicate",
				doc.ID, p.EventID)
		}
		if doc.Type != p.EventType {
			t.Fatalf("event type %q disagrees with the header %q", doc.Type, p.EventType)
		}
		if _, err := time.Parse(time.RFC3339, doc.At); err != nil {
			t.Fatalf("at = %q, not RFC 3339: %v", doc.At, err)
		}
		out = append(out, doc)
	}
	return out
}

// docsOfType filters the delivered documents to one event type.
func docsOfType(docs []sinkDoc, eventType string) []sinkDoc {
	var out []sinkDoc
	for _, d := range docs {
		if d.Type == eventType {
			out = append(out, d)
		}
	}
	return out
}

// settleSinkDeliveries waits long enough that a delivery which was going to happen
// has happened. Used only for the "and nothing else arrived" half of an assertion,
// never to wait for something expected — waitForPosts does that.
func settleSinkDeliveries() { time.Sleep(750 * time.Millisecond) }

// subscribeSink turns the Webhook on, points it at a receiver and subscribes it to
// exactly the given events.
func subscribeSink(t *testing.T, srv *testharness.Server, token, url string, events ...string) {
	t.Helper()
	configureSink(t, srv, token, map[string]any{
		"slug":    "webhook",
		"enabled": true,
		"secret":  "topsecret",
		"url":     url,
		"events":  events,
	})
}

// sinkCounters reads one sink's delivery tally off the settings response — the
// same read the admin screen does.
func sinkCounters(t *testing.T, srv *testharness.Server, token, slug string) eventSinkCountersResp {
	t.Helper()
	var view eventSinksWithCountersResp
	if status, body := srv.AuthGET("/api/v1/settings/event-sinks", token, &view); status != http.StatusOK {
		t.Fatalf("GET event-sinks = %d; body: %s", status, body)
	}
	for _, s := range view.Sinks {
		if s.Slug == slug {
			return s.Counters
		}
	}
	t.Fatalf("no sink %q on the settings response", slug)
	return eventSinkCountersResp{}
}

// awaitCounters polls the settings response until want is satisfied, so a test
// never races the worker goroutine that moves the numbers.
func awaitCounters(t *testing.T, srv *testharness.Server, token, slug string,
	want func(eventSinkCountersResp) bool) eventSinkCountersResp {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var got eventSinkCountersResp
	for {
		got = sinkCounters(t, srv, token, slug)
		if want(got) {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("counters never reached the expected state; last read %+v", got)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// The counters half of the settings response, read separately from
// event_sinks_test.go's shape so that file's wire model stays the settings one.
type eventSinkCountersResp struct {
	Delivered int64 `json:"delivered"`
	Dropped   int64 `json:"dropped"`
	Failed    int64 `json:"failed"`
}

type eventSinksWithCountersResp struct {
	Sinks []struct {
		Slug     string                `json:"slug"`
		Counters eventSinkCountersResp `json:"counters"`
	} `json:"sinks"`
}

// --- the remaining four events ----------------------------------------------

// TestEnrichCompletedIsOneEventPerPass: an Enrichment pass finishing produces
// exactly one enrich.completed naming the Library and carrying that pass's terminal
// counts — not one per Title, and not one per progress tick.
func TestEnrichCompletedIsOneEventPerPass(t *testing.T) {
	requireFixtures(t)
	receiver := newSinkReceiver(t)

	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")
	// A scan enqueues an auto-pass; let it finish BEFORE the sink exists, so the
	// only pass this test can be told about is the one it asks for.
	srv.AwaitEnrichPass(libID)

	subscribeSink(t, srv, token, receiver.srv.URL, "enrich.completed")
	postEnrich(t, srv, token, "/api/v1/libraries/"+libID+"/enrich", nil)

	posts := receiver.waitForPosts(t, 1)
	settleSinkDeliveries()
	docs := parseSinkDocs(t, receiver.received())
	if len(docs) != 1 {
		t.Fatalf("received %d documents for one pass, want exactly 1: %+v", len(docs), docs)
	}
	verifySignature(t, "topsecret", posts[0])

	doc := docs[0]
	if doc.Type != "enrich.completed" {
		t.Fatalf("type = %q, want enrich.completed", doc.Type)
	}
	if doc.Library == nil || doc.Library.ID != libID || doc.Library.Name != "Movies" || doc.Library.Kind != "movie" {
		t.Fatalf("library = %+v, want the enriched Library named and kinded", doc.Library)
	}
	if doc.Enrich == nil {
		t.Fatalf("no enrich block on an enrich.completed event; body: %s", posts[0].Body)
	}
	// A pass over a Library with no provider configured settles every Title as
	// disabled, so the counts are a real answer rather than an empty one.
	if doc.Enrich.Total == 0 {
		t.Fatalf("enrich = %+v, want the pass's terminal counts", doc.Enrich)
	}
	if doc.Enrich.Done != doc.Enrich.Total {
		t.Fatalf("enrich = %+v, want a TERMINAL snapshot (done == total)", doc.Enrich)
	}
	// A pass is not a play and not a walk: no other block belongs on it.
	if doc.Scan != nil || doc.Actor != nil || doc.Title != nil {
		t.Fatalf("enrich.completed carried blocks that are not its own: %+v", doc)
	}
}

// TestLibraryChangedIsDeliveredAndTheSubsetIsHonoured: a sink subscribed to
// library.changed and nothing else is told a Library's contents changed — and is
// NOT told the scan that changed them completed, even though both fire from the
// same scan. The subset a sink asked for is the subset it gets.
func TestLibraryChangedIsDeliveredAndTheSubsetIsHonoured(t *testing.T) {
	requireFixtures(t)
	receiver := newSinkReceiver(t)

	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))

	subscribeSink(t, srv, token, receiver.srv.URL, "library.changed")
	scanLib(t, srv, token, libID, "")

	posts := receiver.waitForPosts(t, 1)
	verifySignature(t, "topsecret", posts[0])
	docs := parseSinkDocs(t, posts)
	if got := docsOfType(docs, "library.changed"); len(got) == 0 {
		t.Fatalf("no library.changed delivered for a scan that changed a Library: %+v", docs)
	} else if got[0].Library == nil || got[0].Library.ID != libID || got[0].Library.Name != "Movies" {
		t.Fatalf("library = %+v, want the changed Library named", got[0].Library)
	}

	// The scan DID complete, and the translator DID derive scan.completed for
	// anyone who asked. This sink did not ask.
	settleSinkDeliveries()
	for _, d := range parseSinkDocs(t, receiver.received()) {
		if d.Type != "library.changed" {
			t.Fatalf("an unsubscribed event reached the sink: %+v", d)
		}
	}
}

// TestPlaybackEventsNameTheUserAndTheDevice: a LOCAL play produces exactly one
// playback.started and, when the client stops, exactly one playback.stopped. Both
// name the User who is watching, the Device they are watching on and the Title —
// and neither carries a Link id, because nothing about this session came over one.
func TestPlaybackEventsNameTheUserAndTheDevice(t *testing.T) {
	requireFixtures(t)
	receiver := newSinkReceiver(t)

	srv := testharness.New(t)
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	titleID := findTitle(t, list, "Dune")
	adminID := userIDByName(t, srv, token, "brandon")

	subscribeSink(t, srv, token, receiver.srv.URL, "playback.started", "playback.stopped")

	var dec decisionResp
	if status, raw := srv.JSON(http.MethodPost, "/api/v1/titles/"+titleID+"/playback",
		token, mp4Profile(), &dec); status != http.StatusOK {
		t.Fatalf("negotiate = %d; body: %s", status, raw)
	}

	started := docsOfType(parseSinkDocs(t, receiver.waitForPosts(t, 1)), "playback.started")
	if len(started) != 1 {
		t.Fatalf("received %d playback.started for one play, want 1", len(started))
	}
	start := started[0]
	if start.Title == nil || start.Title.ID != titleID || start.Title.Name != "Dune" || start.Title.Kind != "movie" {
		t.Fatalf("title = %+v, want Dune named and kinded", start.Title)
	}
	if start.Actor == nil || start.Actor.UserID != adminID || start.Actor.Name != "brandon" {
		t.Fatalf("actor = %+v, want the local User named", start.Actor)
	}
	if start.Actor.LinkID != "" {
		t.Fatalf("a local play named a Link: %+v", start.Actor)
	}
	// The Device is the one the token was minted for (ADR-0015).
	if start.Device == nil || start.Device.Name != "Laptop" || start.Device.Kind != "macos" {
		t.Fatalf("device = %+v, want the signed-in Device named", start.Device)
	}
	// A play is not a scan and not a pass.
	if start.Scan != nil || start.Enrich != nil || start.Library != nil {
		t.Fatalf("playback.started carried blocks that are not its own: %+v", start)
	}

	// The clean stop.
	if status, raw := srv.JSON(http.MethodDelete, "/api/v1/sessions/"+dec.SessionID, token, nil, nil); status != http.StatusNoContent {
		t.Fatalf("DELETE session = %d; body: %s", status, raw)
	}

	receiver.waitForPosts(t, 2)
	settleSinkDeliveries()
	docs := parseSinkDocs(t, receiver.received())
	stopped := docsOfType(docs, "playback.stopped")
	if len(stopped) != 1 {
		t.Fatalf("received %d playback.stopped for one stop, want 1: %+v", len(stopped), docs)
	}
	stop := stopped[0]
	if stop.ID == start.ID {
		t.Fatal("the start and the stop share an event id — a receiver would discard the stop as a duplicate")
	}
	if stop.Actor == nil || stop.Actor.UserID != adminID || stop.Actor.LinkID != "" {
		t.Fatalf("stop actor = %+v, want the same local User", stop.Actor)
	}
	if stop.Title == nil || stop.Title.ID != titleID {
		t.Fatalf("stop title = %+v, want Dune", stop.Title)
	}
	if len(docs) != 2 {
		t.Fatalf("received %d documents for one play and one stop, want 2: %+v", len(docs), docs)
	}
}

// --- attribution over a Link -------------------------------------------------

// TestRelayedPlaybackNamesTheLinkAndNotAPerson is the ADR-0054 §3 rule as an
// assertion, on the side that has to honour it: the SHARER's own webhook.
//
// Somebody in another household presses play on a Title of ours. Our Server mints
// a session under the `remote` User, so our sink is told a play started — that is
// load, and content, and our Playback ceiling being spent, all of which is ours to
// know. What it is NOT told is who: no user id, and no name that came from over
// there. The linked Server is named by OUR label for it, the one our Admin typed.
func TestRelayedPlaybackNamesTheLinkAndNotAPerson(t *testing.T) {
	receiver := newSinkReceiver(t)
	f := linkForRelay(t)

	// The far household's own viewer, whose name must never reach our sink.
	f.home.CreateUser(f.homeAdmin, "faraway-viewer", "viewerpass123", "member")
	grantLibraries(t, f.home, f.homeAdmin, userIDByName(t, f.home, f.homeAdmin, "faraway-viewer"), f.mirrorLib)
	viewerTok := f.home.LoginAs("faraway-viewer", "viewerpass123")

	// The sink is the SHARER's: this is the sharing Admin's automation.
	subscribeSink(t, f.sharer, f.sharerAdmin, receiver.srv.URL, "playback.started", "playback.stopped")

	titleID := f.mirroredTitle(t, "Dune")
	status, dec, raw := f.play(t, viewerTok, titleID, mp4Profile())
	if status != http.StatusOK {
		t.Fatalf("relayed play = %d; body: %s", status, raw)
	}

	docs := parseSinkDocs(t, receiver.waitForPosts(t, 1))
	started := docsOfType(docs, "playback.started")
	if len(started) != 1 {
		t.Fatalf("received %d playback.started, want 1: %+v", len(started), docs)
	}
	start := started[0]
	if start.Actor == nil || start.Actor.LinkID != f.remoteUser {
		t.Fatalf("actor = %+v, want linkId %q — the sharer's own record of the linked Server",
			start.Actor, f.remoteUser)
	}
	if start.Actor.UserID != "" {
		t.Fatalf("a relayed play named a User: %+v — across a Link the person is never named", start.Actor)
	}
	if start.Actor.Name != "The other household" {
		t.Fatalf("actor name = %q, want the label THIS Admin typed for the Link", start.Actor.Name)
	}
	// A relayed session's Device is the linked Server presenting itself as one
	// (ADR-0055 §4), so its name is a name from over there. It is omitted whole.
	if start.Device != nil {
		t.Fatalf("device = %+v, want no device block at all on a relayed play", start.Device)
	}
	// The Title is OURS, so naming it is fine — it is our shelf they are watching.
	if start.Title == nil || start.Title.Name != "Dune" {
		t.Fatalf("title = %+v, want our own Title named", start.Title)
	}

	// Ending it names the Link too, and still nothing about a person.
	if status, raw := f.home.JSON(http.MethodDelete, "/api/v1/sessions/"+dec.SessionID, viewerTok, nil, nil); status != http.StatusNoContent {
		t.Fatalf("DELETE relayed session = %d; body: %s", status, raw)
	}
	receiver.waitForPosts(t, 2)
	settleSinkDeliveries()

	all := parseSinkDocs(t, receiver.received())
	if stopped := docsOfType(all, "playback.stopped"); len(stopped) == 0 {
		t.Fatalf("no playback.stopped for an ended relayed session: %+v", all)
	} else if stopped[0].Actor == nil || stopped[0].Actor.LinkID == "" || stopped[0].Actor.UserID != "" {
		t.Fatalf("stop actor = %+v, want the Link and no User", stopped[0].Actor)
	}
	// The whole-body assertion: nothing the far household calls anything by
	// appears anywhere in any document we sent out.
	for _, post := range receiver.received() {
		body := string(post.Body)
		for _, secret := range []string{"faraway-viewer", "faraway-viewer-device", "faraway-viewer-client"} {
			if strings.Contains(body, secret) {
				t.Fatalf("a name from the other household reached the sink (%q): %s", secret, body)
			}
		}
	}
}

// --- the counters ------------------------------------------------------------

// TestEventSinkCountersReportDeliveredAndFailed: the settings API answers with
// delivered, dropped and failed for each sink, which is what lets an Admin tell a
// misconfigured URL from a quiet server. A sink has no Test button — its secret is
// this server's own signing key, so there is nobody to ask — and these three
// numbers are what stands in its place.
func TestEventSinkCountersReportDeliveredAndFailed(t *testing.T) {
	requireFixtures(t)
	receiver := newSinkReceiver(t)

	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))

	// A never-configured sink reports three zeroes — the truth, not an absence.
	if got := sinkCounters(t, srv, token, "webhook"); got != (eventSinkCountersResp{}) {
		t.Fatalf("counters before anything happened = %+v, want all zero", got)
	}

	// A receiver that answers: delivered climbs.
	subscribeSink(t, srv, token, receiver.srv.URL, "scan.completed")
	scanLib(t, srv, token, libID, "")
	receiver.waitForPosts(t, 1)
	delivered := awaitCounters(t, srv, token, "webhook", func(c eventSinkCountersResp) bool {
		return c.Delivered >= 1
	})
	if delivered.Failed != 0 || delivered.Dropped != 0 {
		t.Fatalf("counters = %+v, want a clean delivery", delivered)
	}

	// Now point it at nothing: failed climbs, and the delivered count the Admin
	// was already looking at survives the save they made in response to it.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	configureSink(t, srv, token, map[string]any{"slug": "webhook", "url": deadURL})

	scanLib(t, srv, token, libID, "")
	failed := awaitCounters(t, srv, token, "webhook", func(c eventSinkCountersResp) bool {
		return c.Failed >= 1
	})
	if failed.Delivered < delivered.Delivered {
		t.Fatalf("counters = %+v, want the earlier delivered count (%d) to survive a settings save",
			failed, delivered.Delivered)
	}
}

// userIDByName finds a User's id on the Admin Users list. It is how a test names
// the actor it expects in a sink document without reaching into the database.
func userIDByName(t *testing.T, srv *testharness.Server, adminTok, username string) string {
	t.Helper()
	var list usersListResp
	if status, body := srv.AuthGET("/api/v1/users", adminTok, &list); status != http.StatusOK {
		t.Fatalf("GET /users = %d; body: %s", status, body)
	}
	for _, u := range list.Users {
		if u.Username == username {
			return u.ID
		}
	}
	t.Fatalf("no User %q on the roster", username)
	return ""
}
