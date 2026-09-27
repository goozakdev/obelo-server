package playback

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/access"
	"github.com/goozakdev/obelo-server/internal/store"
)

// The Credits Watched ceiling (ADR-0065 §5): a File whose Credits Marker starts at
// or past half its duration is watched once playback crosses the Credits start,
// instead of at the flat ~90% WatchedCeiling. One starting earlier is ignored.

// markerStore is ceilingStore plus a recorded watch state and a Marker list.
type markerStore struct {
	ceilingStore
	markers map[string][]store.Marker // file id → markers
	state   store.WatchState
}

func (s *markerStore) WatchStateFor(string, string) (store.WatchState, error) { return s.state, nil }
func (s *markerStore) SaveWatchState(_, _ string, resumeMs int64, watched, _ bool) error {
	s.state = store.WatchState{ResumePositionMs: resumeMs, Watched: watched}
	return nil
}
func (s *markerStore) MarkersForFile(fileID string) ([]store.Marker, error) {
	return s.markers[fileID], nil
}

const markerFileMs = 1_000_000

// creditsSession negotiates a session over a single-File Title whose File carries
// ms, and returns the Service and session id.
func creditsSession(t *testing.T, ms ...store.Marker) (*Service, *markerStore, string) {
	t.Helper()
	f := mp4File(1080, 6_000_000)
	f.DurationMs = markerFileMs
	st := &markerStore{
		ceilingStore: ceilingStore{detail: titleWith(store.Edition{ID: "e1", Files: []store.File{f}})},
		markers:      map[string][]store.Marker{f.ID: ms},
	}
	svc := NewService(st, nil, "", Governance{})
	sess := mustNegotiate(t, svc, Request{
		UserID: "u1", TitleID: "t1", Profile: uhdProfile(),
		Constraints: Constraints{MaxResolution: "2160p", MaxBitrate: 100_000_000},
		Scope:       access.Scope{AllLibraries: true},
	})
	return svc, st, sess.ID
}

func credits(startMs int64) store.Marker {
	return store.Marker{Kind: "credits", Source: "local", StartMs: startMs, EndMs: markerFileMs}
}

// TestCreditsPastHalfwayIsTheWatchedCeiling: Credits at 55% — crossing the Credits
// start marks the File watched and clears its resume, long before ~90%.
func TestCreditsPastHalfwayIsTheWatchedCeiling(t *testing.T) {
	svc, st, sid := creditsSession(t, credits(550_000))

	out, err := svc.ReportProgress("u1", sid, 549_000, "", "")
	if err != nil {
		t.Fatalf("progress before credits: %v", err)
	}
	if out.Watched || out.ResumePositionMs != 549_000 {
		t.Fatalf("just before the Credits start: %+v, want unwatched with resume 549000", out)
	}

	out, err = svc.ReportProgress("u1", sid, 550_000, "", "")
	if err != nil {
		t.Fatalf("progress at credits: %v", err)
	}
	if !out.Watched || out.ResumePositionMs != 0 {
		t.Fatalf("at the Credits start (55%%): %+v, want watched with resume cleared", out)
	}
	if !st.state.Watched || st.state.ResumePositionMs != 0 {
		t.Errorf("stored state = %+v, want watched with resume cleared", st.state)
	}
}

// TestCreditsBeforeHalfwayIsIgnored: Credits at 40% would clear a viewer's resume
// almost as soon as they started, so the flat ~90% ceiling still applies.
func TestCreditsBeforeHalfwayIsIgnored(t *testing.T) {
	svc, _, sid := creditsSession(t, credits(400_000))

	for _, pos := range []int64{400_000, 600_000, 899_000} {
		out, err := svc.ReportProgress("u1", sid, pos, "", "")
		if err != nil {
			t.Fatalf("progress %d: %v", pos, err)
		}
		if out.Watched || out.ResumePositionMs != pos {
			t.Fatalf("at %d with Credits at 40%%: %+v, want unwatched with resume %d", pos, out, pos)
		}
	}
	out, err := svc.ReportProgress("u1", sid, 900_000, "", "")
	if err != nil {
		t.Fatalf("progress at 90%%: %v", err)
	}
	if !out.Watched {
		t.Errorf("at 90%% with an ignored Credits marker: %+v, want watched by the flat ceiling", out)
	}
}

// TestCreditsAtExactlyHalfwayCounts: the floor is inclusive — "at or past".
func TestCreditsAtExactlyHalfwayCounts(t *testing.T) {
	svc, _, sid := creditsSession(t, credits(500_000))
	out, err := svc.ReportProgress("u1", sid, 500_000, "", "")
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if !out.Watched {
		t.Errorf("at Credits exactly at 50%%: %+v, want watched", out)
	}
}

// TestCreditsCeilingReplacesTheFlatOne: Credits at 95% is the ceiling INSTEAD of
// 90%, so 92% is still in progress.
func TestCreditsCeilingReplacesTheFlatOne(t *testing.T) {
	svc, _, sid := creditsSession(t, credits(950_000))
	out, err := svc.ReportProgress("u1", sid, 920_000, "", "")
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if out.Watched {
		t.Errorf("at 92%% with Credits at 95%%: %+v, want still in progress", out)
	}
}

// TestNonCreditsMarkersLeaveTheCeilingAlone: an Intro (or any other kind) past
// halfway is not a Watched ceiling.
func TestNonCreditsMarkersLeaveTheCeilingAlone(t *testing.T) {
	svc, _, sid := creditsSession(t, store.Marker{Kind: "preview", Source: "local", StartMs: 600_000, EndMs: 700_000})
	out, err := svc.ReportProgress("u1", sid, 650_000, "", "")
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if out.Watched {
		t.Errorf("inside a Preview at 65%%: %+v, want in progress", out)
	}
}

// TestCreditsOnOnePartOfAMultiPartEditionIsIgnored: the session measures the whole
// work, so one part's Credits are not the work's; the flat ceiling applies.
func TestCreditsOnOnePartOfAMultiPartEditionIsIgnored(t *testing.T) {
	ed := twoPartEdition()
	for i := range ed.Files {
		ed.Files[i].Streams = mp4File(1080, 6_000_000).Streams
	}
	// Part 1's Credits at 1.4M sit at 52% of the 2.7M whole work.
	st := &markerStore{
		ceilingStore: ceilingStore{detail: titleWith(ed)},
		markers: map[string][]store.Marker{"f1": {
			{Kind: "credits", Source: "local", StartMs: 1_400_000, EndMs: 1_500_000},
		}},
	}
	svc := NewService(st, nil, "", Governance{})
	sess := mustNegotiate(t, svc, Request{
		UserID: "u1", TitleID: "t1", Profile: uhdProfile(),
		Constraints: Constraints{MaxResolution: "2160p", MaxBitrate: 100_000_000},
		Scope:       access.Scope{AllLibraries: true},
	})
	out, err := svc.ReportProgress("u1", sess.ID, 1_450_000, "", "")
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if out.Watched {
		t.Errorf("inside part 1's Credits: %+v, want the whole work still in progress", out)
	}
}

// multiPartCreditsSession negotiates a session over twoPartEdition (1.5M + 1.2M)
// whose Files carry the given Markers, and returns the Service and session id.
func multiPartCreditsSession(t *testing.T, ms map[string][]store.Marker) (*Service, string) {
	t.Helper()
	ed := twoPartEdition()
	for i := range ed.Files {
		ed.Files[i].Streams = mp4File(1080, 6_000_000).Streams
	}
	st := &markerStore{ceilingStore: ceilingStore{detail: titleWith(ed)}, markers: ms}
	svc := NewService(st, nil, "", Governance{})
	sess := mustNegotiate(t, svc, Request{
		UserID: "u1", TitleID: "t1", Profile: uhdProfile(),
		Constraints: Constraints{MaxResolution: "2160p", MaxBitrate: 100_000_000},
		Scope:       access.Scope{AllLibraries: true},
	})
	return svc, sess.ID
}

// TestCreditsOnTheLastPartOfAMultiPartEditionIsTheWatchedPoint: the last part's
// Credits end the work, so crossing them — at their start on the whole-work
// timeline, after every earlier part — marks the Title watched.
func TestCreditsOnTheLastPartOfAMultiPartEditionIsTheWatchedPoint(t *testing.T) {
	// Part 2's Credits at 700k sit at 58% of its 1.2M, so at 2.2M of the 2.7M work.
	svc, sid := multiPartCreditsSession(t, map[string][]store.Marker{"f2": {
		{Kind: "credits", Source: "local", StartMs: 700_000, EndMs: 1_200_000},
	}})
	out, err := svc.ReportProgress("u1", sid, 2_199_000, "", "")
	if err != nil {
		t.Fatalf("progress before credits: %v", err)
	}
	if out.Watched || out.ResumePositionMs != 2_199_000 {
		t.Fatalf("just before the last part's Credits: %+v, want unwatched with resume 2199000", out)
	}
	out, err = svc.ReportProgress("u1", sid, 2_200_000, "", "")
	if err != nil {
		t.Fatalf("progress at credits: %v", err)
	}
	if !out.Watched || out.ResumePositionMs != 0 {
		t.Errorf("at the last part's Credits (81%% of the work): %+v, want watched with resume cleared", out)
	}
	if got := svc.SessionWatchedPointMs(sid); got != 2_200_000 {
		t.Errorf("SessionWatchedPointMs = %d, want 2200000: the last part's Credits, on the session timeline", got)
	}
}

// TestCreditsBeforeHalfwayOfTheLastPartIsIgnored: the floor is measured on the
// last part's own duration, as a single File's is on its own.
func TestCreditsBeforeHalfwayOfTheLastPartIsIgnored(t *testing.T) {
	// Part 2's Credits at 500k sit at 42% of its 1.2M (74% of the whole work).
	svc, sid := multiPartCreditsSession(t, map[string][]store.Marker{"f2": {
		{Kind: "credits", Source: "local", StartMs: 500_000, EndMs: 1_200_000},
	}})
	out, err := svc.ReportProgress("u1", sid, 2_100_000, "", "")
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if out.Watched {
		t.Errorf("past Credits at 42%% of the last part: %+v, want in progress under the flat ceiling", out)
	}
}

// TestSessionMarkersServeEveryPartOnTheSessionTimeline: a multi-part session plays
// one concatenated timeline, so every part's Markers are served, each shifted by
// its part's start on it — Skip works in part 2 as in part 1.
func TestSessionMarkersServeEveryPartOnTheSessionTimeline(t *testing.T) {
	svc, sid := multiPartCreditsSession(t, map[string][]store.Marker{
		"f1": {{Kind: "intro", Source: "local", StartMs: 10_000, EndMs: 60_000}},
		"f2": {
			{Kind: "intro", Source: "local", StartMs: 20_000, EndMs: 50_000},
			{Kind: "credits", Source: "local", StartMs: 700_000, EndMs: 1_200_000},
		},
	})
	got, err := svc.SessionMarkers("u1", sid)
	if err != nil {
		t.Fatalf("SessionMarkers: %v", err)
	}
	want := []store.Marker{
		{Kind: "intro", Source: "local", StartMs: 10_000, EndMs: 60_000},
		{Kind: "intro", Source: "local", StartMs: 1_520_000, EndMs: 1_550_000},
		{Kind: "credits", Source: "local", StartMs: 2_200_000, EndMs: 2_700_000},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SessionMarkers = %+v, want %+v", got, want)
	}
}

// threePartEdition is a 1.5M + 1.2M + 1.0M Edition (3.7M in all), whose flat
// ceiling sits at 3.33M.
func threePartEdition() store.Edition {
	ed := twoPartEdition()
	ed.Files = append(ed.Files, partFile("f3", "/media/Movie/Movie - part3.mkv", 1_000_000))
	for i := range ed.Files {
		ed.Files[i].Streams = mp4File(1080, 6_000_000).Streams
	}
	return ed
}

// threePartCreditsSession negotiates a session over threePartEdition whose last
// part carries Credits at 600k (60% of it; 3.3M of the work), and returns the
// store so a test can change the Title under the running session.
func threePartCreditsSession(t *testing.T) (*Service, *markerStore, string) {
	t.Helper()
	st := &markerStore{
		ceilingStore: ceilingStore{detail: titleWith(threePartEdition())},
		markers: map[string][]store.Marker{"f3": {
			{Kind: "credits", Source: "local", StartMs: 600_000, EndMs: 1_000_000},
		}},
	}
	svc := NewService(st, nil, "", Governance{})
	sess := mustNegotiate(t, svc, Request{
		UserID: "u1", TitleID: "t1", Profile: uhdProfile(),
		Constraints: Constraints{MaxResolution: "2160p", MaxBitrate: 100_000_000},
		Scope:       access.Scope{AllLibraries: true},
	})
	return svc, st, sess.ID
}

// TestCreditsOnTheLastOfThreePartsIsTheWatchedPoint: the last part starts after
// EVERY earlier part, not just the first.
func TestCreditsOnTheLastOfThreePartsIsTheWatchedPoint(t *testing.T) {
	svc, _, sid := threePartCreditsSession(t)
	out, err := svc.ReportProgress("u1", sid, 3_299_000, "", "")
	if err != nil {
		t.Fatalf("progress before credits: %v", err)
	}
	if out.Watched {
		t.Fatalf("just before part 3's Credits (3.3M): %+v, want unwatched", out)
	}
	out, err = svc.ReportProgress("u1", sid, 3_300_000, "", "")
	if err != nil {
		t.Fatalf("progress at credits: %v", err)
	}
	if !out.Watched {
		t.Errorf("at part 3's Credits (3.3M, before the flat 3.33M): %+v, want watched", out)
	}
}

// TestAPartMissingMidSessionFallsBackToTheFlatCeiling: once a part the session
// started with has left the Title, the timeline it was measured on is gone, so
// the flat ceiling applies.
func TestAPartMissingMidSessionFallsBackToTheFlatCeiling(t *testing.T) {
	svc, st, sid := threePartCreditsSession(t)
	ed := st.detail.Editions[0]
	ed.Files = []store.File{ed.Files[0], ed.Files[2]}
	st.detail = titleWith(ed)

	out, err := svc.ReportProgress("u1", sid, 3_300_000, "", "")
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if out.Watched {
		t.Errorf("at part 3's Credits with part 2 gone: %+v, want in progress under the flat ceiling", out)
	}
	out, err = svc.ReportProgress("u1", sid, 3_330_000, "", "")
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if !out.Watched {
		t.Errorf("at the flat 90%%: %+v, want watched", out)
	}
}

// TestTheWatchedPointUsesThePartOrderTheSessionStartedWith: the session plays the
// parts in the order it negotiated; a reorder in the store mid-session does not
// move its Watched point.
func TestTheWatchedPointUsesThePartOrderTheSessionStartedWith(t *testing.T) {
	svc, st, sid := threePartCreditsSession(t)
	ed := st.detail.Editions[0]
	ed.Files = []store.File{ed.Files[0], ed.Files[2], ed.Files[1]}
	st.detail = titleWith(ed)

	out, err := svc.ReportProgress("u1", sid, 3_300_000, "", "")
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if !out.Watched {
		t.Errorf("at part 3's Credits after a reorder in the store: %+v, want watched", out)
	}
}

// relayMarkerStore is markerStore on a mirror: the sharer's Edition id maps onto
// the local one.
type relayMarkerStore struct{ *markerStore }

func (relayMarkerStore) LocalIDForRemote(_, remoteID string) (string, error) {
	if remoteID == "re1" {
		return "e1", nil
	}
	return "", store.ErrNotFound
}

// markerRelay is a sharer that answers every play and serves ms (or err) as the
// Markers of the session it opened.
type markerRelay struct {
	ms  []store.Marker
	err error
	// hang, when set, makes every ask wait for its context to end and send why.
	hang chan error

	mu    sync.Mutex
	asked []string // "linkID/remoteSessionID" per ask
}

func (*markerRelay) RelaysLibrary(string) bool { return true }
func (*markerRelay) RelayNegotiate(context.Context, RelayRequest) (RelayAnswer, error) {
	return RelayAnswer{
		LinkID: "l1", RemoteSessionID: "rs1", RemoteTitleID: "rt1", RemoteEditionID: "re1",
		Tier: TierDirectPlay, Decision: map[string]any{},
	}, nil
}
func (r *markerRelay) RelayMarkers(ctx context.Context, linkID, remoteSessionID string) ([]store.Marker, error) {
	r.mu.Lock()
	r.asked = append(r.asked, linkID+"/"+remoteSessionID)
	r.mu.Unlock()
	if r.hang != nil {
		<-ctx.Done()
		r.hang <- ctx.Err()
		return nil, ctx.Err()
	}
	return r.ms, r.err
}

// relayedCreditsSession negotiates a relayed session over a single-File mirrored
// Title and waits for the sharer's Markers to be asked for.
func relayedCreditsSession(t *testing.T, r *markerRelay) (*Service, string) {
	t.Helper()
	f := mp4File(1080, 6_000_000)
	f.DurationMs = markerFileMs
	f.Path = ""
	svc, sess := relayedSession(t, r, store.Edition{ID: "e1", Files: []store.File{f}})
	done := sess.RelayMarkersFetched()
	if done == nil {
		t.Fatal("a relayed session has no ask for the sharer's Markers")
	}
	<-done
	return svc, sess.ID
}

// relayedSession negotiates a relayed session over a mirrored Title with the
// one Edition ed, without waiting for the sharer's Markers.
func relayedSession(t *testing.T, r *markerRelay, ed store.Edition) (*Service, Session) {
	t.Helper()
	st := relayMarkerStore{&markerStore{ceilingStore: ceilingStore{detail: titleWith(ed)}}}
	svc := NewService(st, nil, "", Governance{})
	svc.SetRelay(r)
	sess := mustNegotiate(t, svc, Request{
		UserID: "u1", TitleID: "t1", Profile: uhdProfile(),
		Constraints: Constraints{MaxResolution: "2160p", MaxBitrate: 100_000_000},
		Scope:       access.Scope{AllLibraries: true},
	})
	return svc, sess
}

// TestARelayedSessionServesTheSharersMarkers: the sharer's Markers for the
// session it opened are asked for once, at playback start, and are what the
// session serves and measures its Watched point against — under this Server's
// own CreditsFloor rule.
func TestARelayedSessionServesTheSharersMarkers(t *testing.T) {
	r := &markerRelay{ms: []store.Marker{
		{Kind: "intro", Source: "local", StartMs: 10_000, EndMs: 60_000},
		{Kind: "credits", Source: "detected", StartMs: 550_000, EndMs: markerFileMs},
	}}
	svc, sid := relayedCreditsSession(t, r)

	got, err := svc.SessionMarkers("u1", sid)
	if err != nil {
		t.Fatalf("SessionMarkers: %v", err)
	}
	if !reflect.DeepEqual(got, r.ms) {
		t.Errorf("SessionMarkers = %+v, want the sharer's %+v", got, r.ms)
	}
	if got := svc.SessionWatchedPointMs(sid); got != 550_000 {
		t.Errorf("SessionWatchedPointMs = %d, want the sharer's Credits at 550000", got)
	}
	out, err := svc.ReportProgress("u1", sid, 549_000, "", "")
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if out.Watched {
		t.Fatalf("before the sharer's Credits: %+v, want unwatched", out)
	}
	out, err = svc.ReportProgress("u1", sid, 550_000, "", "")
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if !out.Watched {
		t.Errorf("at the sharer's Credits (55%%): %+v, want watched", out)
	}
	if _, err := svc.SessionMarkers("u1", sid); err != nil {
		t.Fatalf("SessionMarkers again: %v", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if want := []string{"l1/rs1"}; !reflect.DeepEqual(r.asked, want) {
		t.Errorf("asked the sharer %v, want once for its session %v", r.asked, want)
	}
}

// TestARelayedSessionAppliesItsOwnCreditsFloor: the sharer's Credits at 40% are
// served, but this Server's floor ignores them as the Watched point.
func TestARelayedSessionAppliesItsOwnCreditsFloor(t *testing.T) {
	svc, sid := relayedCreditsSession(t, &markerRelay{ms: []store.Marker{credits(400_000)}})
	if got := svc.SessionWatchedPointMs(sid); got != -1 {
		t.Errorf("SessionWatchedPointMs = %d, want -1 for Credits at 40%%", got)
	}
	out, err := svc.ReportProgress("u1", sid, 600_000, "", "")
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if out.Watched {
		t.Errorf("past Credits at 40%%: %+v, want in progress under the flat ceiling", out)
	}
}

// TestARelayedSessionWhoseMarkersFailServesNone: a sharer that cannot answer
// leaves the session with no Markers and the flat ceiling.
func TestARelayedSessionWhoseMarkersFailServesNone(t *testing.T) {
	svc, sid := relayedCreditsSession(t, &markerRelay{ms: []store.Marker{credits(550_000)}, err: errors.New("unreachable")})
	got, err := svc.SessionMarkers("u1", sid)
	if err != nil || len(got) != 0 {
		t.Errorf("SessionMarkers = %+v, %v; want none", got, err)
	}
	out, err := svc.ReportProgress("u1", sid, 550_000, "", "")
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if out.Watched {
		t.Errorf("at 55%% with no Markers from the sharer: %+v, want in progress", out)
	}
}

// TestARelayedSessionIgnoresSpansOutsideTheFile: the sharer's answer is not
// trusted. A span past the File's end is neither served nor the Watched point,
// so the File is still watched at its end.
func TestARelayedSessionIgnoresSpansOutsideTheFile(t *testing.T) {
	intro := store.Marker{Kind: "intro", Source: "local", StartMs: 10_000, EndMs: 60_000}
	for _, bad := range []store.Marker{
		{Kind: "credits", Source: "local", StartMs: 5_000_000, EndMs: 5_000_001},
		{Kind: "credits", Source: "local", StartMs: math.MaxInt64 - 1, EndMs: math.MaxInt64},
		{Kind: "credits", Source: "local", StartMs: 900_000, EndMs: markerFileMs + 1},
	} {
		svc, sid := relayedCreditsSession(t, &markerRelay{ms: []store.Marker{intro, bad}})
		got, err := svc.SessionMarkers("u1", sid)
		if err != nil {
			t.Fatalf("SessionMarkers: %v", err)
		}
		if want := []store.Marker{intro}; !reflect.DeepEqual(got, want) {
			t.Errorf("with %+v: SessionMarkers = %+v, want only the span inside the File %+v", bad, got, want)
		}
		if got := svc.SessionWatchedPointMs(sid); got != -1 {
			t.Errorf("with %+v: SessionWatchedPointMs = %d, want -1", bad, got)
		}
		out, err := svc.ReportProgress("u1", sid, markerFileMs, "", "")
		if err != nil {
			t.Fatalf("progress: %v", err)
		}
		if !out.Watched {
			t.Errorf("with %+v: at the File's end: %+v, want watched", bad, out)
		}
	}
}

// TestARelayedMultiPartSessionsWatchedPointIsOnTheLastPart: the sharer serves
// the session's timeline, so the last part's Credits are measured from where
// that part starts on it.
func TestARelayedMultiPartSessionsWatchedPointIsOnTheLastPart(t *testing.T) {
	ed := twoPartEdition()
	for i := range ed.Files {
		ed.Files[i].Streams = mp4File(1080, 6_000_000).Streams
		ed.Files[i].Path = ""
	}
	r := &markerRelay{ms: []store.Marker{
		{Kind: "credits", Source: "local", StartMs: 1_400_000, EndMs: 1_500_000},
		{Kind: "credits", Source: "local", StartMs: 2_200_000, EndMs: 2_700_000},
	}}
	svc, sess := relayedSession(t, r, ed)
	<-sess.RelayMarkersFetched()
	if got := svc.SessionWatchedPointMs(sess.ID); got != 2_200_000 {
		t.Errorf("SessionWatchedPointMs = %d, want part 2's Credits at 2200000", got)
	}
	out, err := svc.ReportProgress("u1", sess.ID, 2_199_000, "", "")
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if out.Watched {
		t.Fatalf("just before part 2's Credits: %+v, want unwatched", out)
	}
	out, err = svc.ReportProgress("u1", sess.ID, 2_200_000, "", "")
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if !out.Watched {
		t.Errorf("at part 2's Credits (2.2M, before the flat 2.43M): %+v, want watched", out)
	}
}

// TestTheWatchedPointFallsBackWhenTheFileChangesLengthMidSession: a File that
// changed length under the session is no longer the timeline its Credits were
// measured on, so the flat ceiling applies.
func TestTheWatchedPointFallsBackWhenTheFileChangesLengthMidSession(t *testing.T) {
	svc, st, sid := creditsSession(t, credits(550_000))
	ed := st.detail.Editions[0]
	ed.Files[0].DurationMs = 2 * markerFileMs
	st.detail = titleWith(ed)

	out, err := svc.ReportProgress("u1", sid, 550_000, "", "")
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if out.Watched {
		t.Errorf("at the Credits of a File that changed length: %+v, want in progress under the flat ceiling", out)
	}
	out, err = svc.ReportProgress("u1", sid, 900_000, "", "")
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if !out.Watched {
		t.Errorf("at the flat 90%%: %+v, want watched", out)
	}
}

// TestTheRelayMarkersAskEndsWithTheSession: a session that ends, cleanly or by
// the reaper, ends its ask for the sharer's Markers with it.
func TestTheRelayMarkersAskEndsWithTheSession(t *testing.T) {
	f := mp4File(1080, 6_000_000)
	f.DurationMs = markerFileMs
	f.Path = ""
	ed := store.Edition{ID: "e1", Files: []store.File{f}}
	for _, end := range []struct {
		name string
		end  func(svc *Service, id string)
	}{
		{"ended", func(svc *Service, id string) { svc.Sessions().End(id) }},
		{"reaped", func(svc *Service, _ string) {
			later := time.Now().Add(time.Hour)
			svc.Sessions().SetNow(func() time.Time { return later })
			svc.Sessions().Reap(time.Minute)
		}},
	} {
		r := &markerRelay{hang: make(chan error, 1)}
		svc, sess := relayedSession(t, r, ed)
		end.end(svc, sess.ID)
		select {
		case err := <-r.hang:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("%s: the ask ended with %v, want context.Canceled", end.name, err)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("%s: the ask for the sharer's Markers outlived its session", end.name)
		}
		<-sess.RelayMarkersFetched()
	}
}
