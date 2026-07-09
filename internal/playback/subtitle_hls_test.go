package playback

import (
	"errors"
	"testing"

	"github.com/marioquake/juicebox/internal/store"
)

// Unit test for the owner-checked SessionSubtitleContext seam (subtitles/03):
// given an HLS session it returns the played File's Subtitle tracks + Title
// detail + duration the api layer segments into an in-band rendition, and
// enforces the ownership / not-direct-play gates. It drives the Service directly
// over a fake store (no ffmpeg, no DB).

type fakeSubStore struct {
	detail store.TitleDetail
}

func (f fakeSubStore) TitleByID(id string) (store.TitleDetail, error) {
	if id != f.detail.ID {
		return store.TitleDetail{}, store.ErrNotFound
	}
	return f.detail, nil
}
func (f fakeSubStore) WatchStateFor(userID, titleID string) (store.WatchState, error) {
	return store.WatchState{}, nil
}
func (f fakeSubStore) SaveWatchState(userID, titleID string, resumeMs int64, watched bool) error {
	return nil
}

// subFixtureDetail is a Title with one File carrying an embedded English text
// subtitle Stream plus a Spanish forced text Sidecar and a German image Sidecar.
func subFixtureDetail() store.TitleDetail {
	d := store.TitleDetail{}
	d.Title.ID = "t1"
	d.Editions = []store.Edition{{
		ID: "e1",
		Files: []store.File{{
			ID:         "f1",
			EditionID:  "e1",
			DurationMs: 10_000,
			Present:    true,
			Streams: []store.Stream{
				{ID: "vs", Kind: "video", Codec: "h264"},
				{ID: "as", Kind: "audio", Codec: "aac"},
				{ID: "s-en", Kind: "subtitle", Codec: "subrip", Language: "eng"},
			},
		}},
	}}
	d.Subtitles = []store.Subtitle{
		{ID: "sc-es", TitleID: "t1", Source: "sidecar", Kind: "text", Codec: "srt", Language: "es", Forced: true},
		{ID: "sc-de", TitleID: "t1", Source: "sidecar", Kind: "image", Codec: "sup", Language: "de"},
	}
	return d
}

func newSubService(t *testing.T) *Service {
	t.Helper()
	return NewService(fakeSubStore{detail: subFixtureDetail()}, nil, "", Governance{})
}

func TestSessionSubtitleContextListsTracks(t *testing.T) {
	svc := newSubService(t)
	// A remux (HLS) session for File f1 of Title t1.
	sess := svc.Sessions().Create(CreateInput{UserID: "u1", TitleID: "t1"}, Decision{
		Tier:    TierDirectStream,
		Edition: store.Edition{ID: "e1"},
		File:    store.File{ID: "f1", DurationMs: 10_000},
	})

	ctx, err := svc.SessionSubtitleContext("u1", sess.ID)
	if err != nil {
		t.Fatalf("SessionSubtitleContext: %v", err)
	}
	if ctx.DurationMs != 10_000 {
		t.Errorf("DurationMs = %d, want 10000", ctx.DurationMs)
	}
	if ctx.Detail.ID != "t1" {
		t.Errorf("Detail.ID = %q, want t1", ctx.Detail.ID)
	}
	byID := map[string]SubtitleTrack{}
	for _, tr := range ctx.Tracks {
		byID[tr.ID] = tr
	}
	if en := byID["s-en"]; en.Source != "embedded" || en.Kind != "text" || !en.Convertible || en.Language != "en" {
		t.Errorf("embedded English track wrong: %+v", en)
	}
	if es := byID["sc-es"]; es.Source != "sidecar" || es.Kind != "text" || !es.Convertible || !es.Forced {
		t.Errorf("sidecar Spanish forced track wrong: %+v", es)
	}
	if de := byID["sc-de"]; de.Kind != "image" || de.Convertible {
		t.Errorf("sidecar German image track should be non-convertible image: %+v", de)
	}
}

func TestSessionSubtitleContextOwnershipAndTier(t *testing.T) {
	svc := newSubService(t)
	remux := svc.Sessions().Create(CreateInput{UserID: "u1", TitleID: "t1"}, Decision{
		Tier: TierDirectStream, Edition: store.Edition{ID: "e1"}, File: store.File{ID: "f1", DurationMs: 10_000},
	})
	// A different User is hidden as not-found.
	if _, err := svc.SessionSubtitleContext("other", remux.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("foreign user err = %v, want ErrSessionNotFound", err)
	}
	// An unknown session is not-found.
	if _, err := svc.SessionSubtitleContext("u1", "nope"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("unknown session err = %v, want ErrSessionNotFound", err)
	}
	// A direct-play session has no master playlist.
	dp := svc.Sessions().Create(CreateInput{UserID: "u1", TitleID: "t1"}, Decision{
		Tier: TierDirectPlay, Edition: store.Edition{ID: "e1"}, File: store.File{ID: "f1", DurationMs: 10_000},
	})
	if _, err := svc.SessionSubtitleContext("u1", dp.ID); !errors.Is(err, ErrNotHLS) {
		t.Errorf("direct-play err = %v, want ErrNotHLS", err)
	}
}
