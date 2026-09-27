package playback

import (
	"testing"

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
	if got := svc.SessionWatchedPointMs(sid); got != -1 {
		t.Errorf("SessionWatchedPointMs = %d, want -1: the session serves part 1's Markers, and none of them is the Watched point", got)
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
