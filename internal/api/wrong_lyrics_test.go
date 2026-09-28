package api_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"sync"
	"testing"

	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Black-box tests for the "wrong lyrics" action: POST /titles/{id}/lyrics/wrong,
// pressed from the lyrics view by any User, against the same Installed Lyric
// providers and fixture library as lyric_provider_test.go.

// shownBody is one lyrics body with the id a fetched answer carries — the id the
// view sends back when "wrong lyrics" is pressed on it.
type shownBody struct {
	lyricsBodyResp
	ID string `json:"id"`
}

type shownLyrics struct {
	Lyrics *shownBody `json:"lyrics"`
}

// lyricsConflict is the 409 envelope, which carries what the track shows now.
type lyricsConflict struct {
	Error struct {
		Code    string `json:"code"`
		Details struct {
			Lyrics *shownBody `json:"lyrics"`
		} `json:"details"`
	} `json:"error"`
}

// readShown opens the lyrics view on titleID as token.
func readShown(t *testing.T, srv *testharness.Server, token, titleID string) shownLyrics {
	t.Helper()
	var resp shownLyrics
	if status, body := srv.AuthGET("/api/v1/titles/"+titleID+"/lyrics", token, &resp); status != http.StatusOK {
		t.Fatalf("GET lyrics = %d %s, want 200", status, body)
	}
	return resp
}

// pressWrong presses "wrong lyrics" on titleID as token, naming the answer shown
// as answerID.
func pressWrong(srv *testharness.Server, token, titleID, answerID string) (int, []byte) {
	return srv.JSON(http.MethodPost, "/api/v1/titles/"+titleID+"/lyrics/wrong", token,
		map[string]any{"id": answerID}, nil)
}

// markLyricsWrong presses "wrong lyrics" on the answer shown as answerID and
// returns what the track shows afterwards.
func markLyricsWrong(t *testing.T, srv *testharness.Server, token, titleID, answerID string) shownLyrics {
	t.Helper()
	status, body := pressWrong(srv, token, titleID, answerID)
	if status != http.StatusOK {
		t.Fatalf("POST lyrics/wrong = %d %s, want 200", status, body)
	}
	var resp shownLyrics
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}
	return resp
}

// conflictOf decodes a 409 from "wrong lyrics", failing on any other status.
func conflictOf(t *testing.T, status int, body []byte) lyricsConflict {
	t.Helper()
	if status != http.StatusConflict {
		t.Fatalf("POST lyrics/wrong = %d %s, want 409", status, body)
	}
	var c lyricsConflict
	if err := json.Unmarshal(body, &c); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}
	return c
}

// memberOf signs in a new Member granted libID.
func memberOf(t *testing.T, srv *testharness.Server, admin, libID string) string {
	t.Helper()
	id := srv.CreateUser(admin, "listener", "memberpass123", "member")
	grantLibraries(t, srv, admin, id, libID)
	return srv.LoginAs("listener", "memberpass123")
}

// threeProviderServer installs alpha (wrong), beta (right) and gamma (a third
// candidate) in that order, so a second rejection would show gamma's answer.
func threeProviderServer(t *testing.T) (srv *testharness.Server, admin, libID string, gamma *lyricSource, tracks map[string]string) {
	t.Helper()
	a := newLyricSource(t, syncedAnswer(0, "Wrong song"))
	b := newLyricSource(t, syncedAnswer(0, "Right song"))
	gamma = newLyricSource(t, syncedAnswer(0, "Third song"))
	srv, admin, libID, _, tracks = lyricProviderServer(t,
		map[string]*lyricSource{"alpha-lyrics": a, "beta-lyrics": b, "gamma-lyrics": gamma})
	if status, body := srv.JSON(http.MethodPut, "/api/v1/settings/lyric-providers", admin,
		map[string]any{"order": []string{"alpha-lyrics", "beta-lyrics", "gamma-lyrics"}}, nil); status != http.StatusOK {
		t.Fatalf("PUT lyric-providers = %d %s", status, body)
	}
	return srv, admin, libID, gamma, tracks
}

// TestAMemberMarksLyricsWrongAndEveryoneSeesTheNextAnswer: the Admin's first
// provider answers wrongly, and that is what the track shows. A Member marks it
// wrong. The first provider is asked again and gives the same answer, which is
// passed over; the second provider's answer is shown to the Member, and to the
// Admin opening the track afterwards.
func TestAMemberMarksLyricsWrongAndEveryoneSeesTheNextAnswer(t *testing.T) {
	t.Parallel()
	a := newLyricSource(t, syncedAnswer(0, "Wrong song"))
	b := newLyricSource(t, syncedAnswer(0, "Right song"))
	srv, admin, libID, _, tracks := lyricProviderServer(t, map[string]*lyricSource{"alpha-lyrics": a, "beta-lyrics": b})
	if status, body := srv.JSON(http.MethodPut, "/api/v1/settings/lyric-providers", admin,
		map[string]any{"order": []string{"alpha-lyrics", "beta-lyrics"}}, nil); status != http.StatusOK {
		t.Fatalf("PUT lyric-providers = %d %s", status, body)
	}
	id := tracks["Nothing Local"]
	member := memberOf(t, srv, admin, libID)

	first := readShown(t, srv, member, id)
	if first.Lyrics == nil || first.Lyrics.Source != "fetched" || first.Lyrics.Lines[0].Text != "Wrong song one" || first.Lyrics.ID == "" {
		t.Fatalf("first answer = %+v, want alpha's with an id", first.Lyrics)
	}

	after := markLyricsWrong(t, srv, member, id, first.Lyrics.ID)
	if after.Lyrics == nil || after.Lyrics.Kind != "synced" || after.Lyrics.Lines[0].Text != "Right song one" {
		t.Fatalf("after wrong lyrics = %+v, want beta's", after.Lyrics)
	}
	if after.Lyrics.ID == "" || after.Lyrics.ID == first.Lyrics.ID {
		t.Fatalf("beta's answer has id %q, want its own (alpha's was %q)", after.Lyrics.ID, first.Lyrics.ID)
	}
	if n := a.askedAbout("Nothing Local"); n != 2 {
		t.Fatalf("alpha was asked %d times, want 2: re-asked after the rejection", n)
	}

	if seen := readShown(t, srv, admin, id); !reflect.DeepEqual(seen, after) {
		t.Fatalf("the Admin opening the track sees %+v, want the post-rejection %+v", seen.Lyrics, after.Lyrics)
	}
}

// TestWrongLyricsWithNoOtherCandidateLeavesTheTrackWithout: the only provider
// keeps giving the answer marked wrong. Afterwards the track has no lyrics — for
// the User who marked it and for the next one to open it — and the provider's
// identical answer was never stored again.
func TestWrongLyricsWithNoOtherCandidateLeavesTheTrackWithout(t *testing.T) {
	t.Parallel()
	src := newLyricSource(t, syncedAnswer(0, "Wrong song"))
	srv, admin, libID, _, tracks := lyricProviderServer(t, map[string]*lyricSource{"example-lyrics": src})
	id := tracks["Nothing Local"]
	member := memberOf(t, srv, admin, libID)

	shown := readShown(t, srv, admin, id)
	if shown.Lyrics == nil || shown.Lyrics.Source != "fetched" {
		t.Fatalf("first answer = %+v, want the fetched one", shown.Lyrics)
	}
	if after := markLyricsWrong(t, srv, member, id, shown.Lyrics.ID); after.Lyrics != nil {
		t.Fatalf("after wrong lyrics = %+v, want null", *after.Lyrics)
	}
	for _, who := range []string{admin, member} {
		if got := readShown(t, srv, who, id); got.Lyrics != nil {
			t.Fatalf("a later open = %+v, want null", *got.Lyrics)
		}
	}
	if n := src.askedAbout("Nothing Local"); n != 2 {
		t.Fatalf("the source was asked %d times, want 2: the first open and the re-ask", n)
	}
}

// TestWrongLyricsOnlyRejectsAProvidersAnswer: a track showing its own Local
// lyrics, or none, has no provider answer to reject — 409 carrying what it shows,
// and nothing changes.
func TestWrongLyricsOnlyRejectsAProvidersAnswer(t *testing.T) {
	t.Parallel()
	src := newLyricSource(t, nil)
	srv, admin, _, _, tracks := lyricProviderServer(t, map[string]*lyricSource{"example-lyrics": src})

	for _, title := range []string{"Synced Local", "Nothing Local"} {
		shown := readShown(t, srv, admin, tracks[title])
		status, body := pressWrong(srv, admin, tracks[title], "some-answer")
		c := conflictOf(t, status, body)
		if c.Error.Code != "NO_FETCHED_LYRICS" || !reflect.DeepEqual(c.Error.Details.Lyrics, shown.Lyrics) {
			t.Fatalf("%s: 409 = %s, want NO_FETCHED_LYRICS carrying %+v", title, body, shown.Lyrics)
		}
	}
	if got := readShown(t, srv, admin, tracks["Synced Local"]); got.Lyrics == nil || got.Lyrics.Source != "local" || got.Lyrics.ID != "" {
		t.Fatalf("Synced Local lyrics after a refused rejection = %+v, want the Local ones, with no id", got.Lyrics)
	}
	if status, body := srv.JSON(http.MethodGet, "/api/v1/titles/"+tracks["Synced Local"]+"/lyrics/wrong", admin, nil, nil); status != http.StatusMethodNotAllowed {
		t.Fatalf("GET lyrics/wrong = %d %s, want 405", status, body)
	}
}

// TestWrongLyricsLeavesAPlainLocalTrackAlone: a track's own Plain lyrics outrank
// a provider's Plain answer, so that answer is stored but not shown. Pressing
// "wrong lyrics" with its id — learnt from a track that does show it — is a 409
// carrying the Local lyrics, and the Local lyrics stay.
func TestWrongLyricsLeavesAPlainLocalTrackAlone(t *testing.T) {
	t.Parallel()
	src := newLyricSource(t, func(lyricQuestion) map[string]any {
		return map[string]any{"kind": "plain", "text": "Fetched plain words"}
	})
	srv, admin, _, _, tracks := lyricProviderServer(t, map[string]*lyricSource{"example-lyrics": src})

	other := readShown(t, srv, admin, tracks["Nothing Local"])
	if other.Lyrics == nil || other.Lyrics.Source != "fetched" || other.Lyrics.ID == "" {
		t.Fatalf("Nothing Local = %+v, want the fetched Plain answer with an id", other.Lyrics)
	}
	shown := readShown(t, srv, admin, tracks["Plain Local"])
	if shown.Lyrics == nil || shown.Lyrics.Source != "local" || shown.Lyrics.Text != "Local plain words" {
		t.Fatalf("Plain Local = %+v, want its Local lyrics", shown.Lyrics)
	}
	if n := src.askedAbout("Plain Local"); n != 1 {
		t.Fatalf("the source was asked about Plain Local %d times, want 1", n)
	}

	status, body := pressWrong(srv, admin, tracks["Plain Local"], other.Lyrics.ID)
	if c := conflictOf(t, status, body); c.Error.Code != "NO_FETCHED_LYRICS" || !reflect.DeepEqual(c.Error.Details.Lyrics, shown.Lyrics) {
		t.Fatalf("409 = %s, want NO_FETCHED_LYRICS carrying the Local lyrics", body)
	}
	if got := readShown(t, srv, admin, tracks["Plain Local"]); !reflect.DeepEqual(got, shown) {
		t.Fatalf("Plain Local afterwards = %+v, want the Local lyrics unchanged", got.Lyrics)
	}
	if n := src.askedAbout("Plain Local"); n != 1 {
		t.Fatalf("the source was asked about Plain Local %d times, want still 1: nothing was rejected", n)
	}
}

// TestWrongLyricsFromAStaleViewRejectsNothing: two Users open the track and see
// alpha's answer. The Member marks it wrong and beta's is shown. The Admin, still
// looking at alpha's, presses too: that answer is no longer the one stored, so
// nothing is rejected — beta's stays, gamma is never asked — and the 409 carries
// beta's answer for the Admin's view to show.
func TestWrongLyricsFromAStaleViewRejectsNothing(t *testing.T) {
	t.Parallel()
	srv, admin, libID, gamma, tracks := threeProviderServer(t)
	id := tracks["Nothing Local"]
	member := memberOf(t, srv, admin, libID)

	stale := readShown(t, srv, admin, id)
	if seen := readShown(t, srv, member, id); !reflect.DeepEqual(seen, stale) || stale.Lyrics == nil {
		t.Fatalf("the two Users see %+v and %+v, want alpha's answer for both", stale.Lyrics, seen.Lyrics)
	}
	after := markLyricsWrong(t, srv, member, id, stale.Lyrics.ID)
	if after.Lyrics == nil || after.Lyrics.Lines[0].Text != "Right song one" {
		t.Fatalf("after the Member's press = %+v, want beta's", after.Lyrics)
	}

	status, body := pressWrong(srv, admin, id, stale.Lyrics.ID)
	c := conflictOf(t, status, body)
	if c.Error.Code != "LYRICS_CHANGED" || !reflect.DeepEqual(c.Error.Details.Lyrics, after.Lyrics) {
		t.Fatalf("the stale press = %s, want LYRICS_CHANGED carrying beta's %+v", body, after.Lyrics)
	}
	if got := readShown(t, srv, admin, id); !reflect.DeepEqual(got, after) {
		t.Fatalf("the track shows %+v after the stale press, want beta's still", got.Lyrics)
	}
	if n := gamma.askedAbout("Nothing Local"); n != 0 {
		t.Fatalf("gamma was asked %d times, want 0: beta's answer was never rejected", n)
	}
}

// TestASimultaneousDoublePressRejectsOnce: the same answer's "wrong lyrics" is
// sent twice at once. Exactly one press rejects it; the other finds it gone and
// is a 409. The track shows beta's answer, not gamma's.
func TestASimultaneousDoublePressRejectsOnce(t *testing.T) {
	t.Parallel()
	srv, admin, _, gamma, tracks := threeProviderServer(t)
	id := tracks["Nothing Local"]
	shown := readShown(t, srv, admin, id)
	if shown.Lyrics == nil {
		t.Fatal("the track shows no lyrics, want alpha's")
	}

	statuses := make([]int, 2)
	var wg sync.WaitGroup
	for i := range statuses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i], _ = pressWrong(srv, admin, id, shown.Lyrics.ID)
		}(i)
	}
	wg.Wait()
	ok, conflict := 0, 0
	for _, s := range statuses {
		switch s {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			conflict++
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("statuses = %v, want one 200 and one 409", statuses)
	}
	if got := readShown(t, srv, admin, id); got.Lyrics == nil || got.Lyrics.Lines[0].Text != "Right song one" {
		t.Fatalf("the track shows %+v, want beta's", got.Lyrics)
	}
	if n := gamma.askedAbout("Nothing Local"); n != 0 {
		t.Fatalf("gamma was asked %d times, want 0: only one answer was rejected", n)
	}
}

// TestWrongLyricsNeedsTheShownAnswersID: a press that does not name the answer it
// was made on is a 400, and rejects nothing.
func TestWrongLyricsNeedsTheShownAnswersID(t *testing.T) {
	t.Parallel()
	src := newLyricSource(t, syncedAnswer(0, "Wrong song"))
	srv, admin, _, _, tracks := lyricProviderServer(t, map[string]*lyricSource{"example-lyrics": src})
	id := tracks["Nothing Local"]
	shown := readShown(t, srv, admin, id)

	for _, in := range []any{nil, map[string]any{}, map[string]any{"id": ""}} {
		status, body := srv.JSON(http.MethodPost, "/api/v1/titles/"+id+"/lyrics/wrong", admin, in, nil)
		if status != http.StatusBadRequest {
			t.Fatalf("POST lyrics/wrong with body %v = %d %s, want 400", in, status, body)
		}
	}
	if got := readShown(t, srv, admin, id); !reflect.DeepEqual(got, shown) {
		t.Fatalf("the track shows %+v, want alpha's unchanged", got.Lyrics)
	}
}

// TestALinkedServerCannotMarkLyricsWrong: the remote role is a linked Server,
// not a person reading along, so its press is a 403 and rejects nothing.
func TestALinkedServerCannotMarkLyricsWrong(t *testing.T) {
	t.Parallel()
	src := newLyricSource(t, syncedAnswer(0, "Wrong song"))
	srv, admin, libID, _, tracks := lyricProviderServer(t, map[string]*lyricSource{"example-lyrics": src})
	id := tracks["Nothing Local"]
	remoteID := createRemoteUser(t, srv, admin, "peer-server")
	grantLibraries(t, srv, admin, remoteID, libID)
	remote := srv.IssueTokenForUser(remoteID, "peer-server-id")
	shown := readShown(t, srv, admin, id)

	if status, body := pressWrong(srv, remote, id, shown.Lyrics.ID); status != http.StatusForbidden {
		t.Fatalf("the linked Server's press = %d %s, want 403", status, body)
	}
	if got := readShown(t, srv, admin, id); !reflect.DeepEqual(got, shown) {
		t.Fatalf("the track shows %+v, want alpha's unchanged", got.Lyrics)
	}
}
