package enrich

import (
	"context"
	"testing"
)

// What is left of this file is ROUTING, and routing needs sources that ANSWER
// rather than sources that talk to anything.
//
// The TMDB half went with the TMDB provider (.scratch/bundled-plugins issue 04) and
// the MusicBrainz half with MusicBrainz (issue 06): both are plugins now, and their
// request/parse layers are asserted natively in plugins/<id>/<id>. A CompositeProvider
// dispatching by kind is a fact about this package, so it stays, with five-line
// stubs standing where two real clients used to.

// videoSourceStub is a video provider that matches every video kind and stamps a
// source name, so a test about ROUTING can assert which sub-provider answered
// without standing up a real one.
type videoSourceStub struct{ source string }

func (v videoSourceStub) Lookup(_ context.Context, ref TitleRef) (TitleMetadata, error) {
	switch ref.Kind {
	case "movie", "show", "season", "episode":
		return TitleMetadata{Matched: true, Name: ref.Title, Source: v.source}, nil
	default:
		return TitleMetadata{}, ErrNoMatch
	}
}

func (v videoSourceStub) Search(context.Context, string, string, SearchOptions) ([]Candidate, error) {
	return nil, ErrSearchUnavailable
}

func (v videoSourceStub) ArtworkCandidates(context.Context, TitleRef, string) ([]ArtworkCandidate, error) {
	return nil, ErrSearchUnavailable
}

// musicSourceStub is its music twin.
type musicSourceStub struct{ source string }

func (m musicSourceStub) Lookup(_ context.Context, ref TitleRef) (TitleMetadata, error) {
	switch ref.Kind {
	case "artist", "album", "track":
		return TitleMetadata{Matched: true, Name: ref.Title, Source: m.source}, nil
	default:
		return TitleMetadata{}, ErrNoMatch
	}
}

func (m musicSourceStub) Search(context.Context, string, string, SearchOptions) ([]Candidate, error) {
	return nil, ErrSearchUnavailable
}

func (m musicSourceStub) ArtworkCandidates(context.Context, TitleRef, string) ([]ArtworkCandidate, error) {
	return nil, ErrSearchUnavailable
}

// CompositeProvider routes by kind: video → the video source, music → the music one.
func TestCompositeRoutesByKind(t *testing.T) {
	c := CompositeProvider{
		Video: videoSourceStub{source: "tmdb"},
		Music: musicSourceStub{source: "musicbrainz"},
	}

	if m, err := c.Lookup(context.Background(), TitleRef{Kind: "show", Title: "GoT"}); err != nil || m.Source != "tmdb" {
		t.Errorf("show routed wrong: %+v err=%v", m, err)
	}
	if m, err := c.Lookup(context.Background(), TitleRef{Kind: "artist", Title: "Radiohead"}); err != nil || m.Source != "musicbrainz" {
		t.Errorf("artist routed wrong: %+v err=%v", m, err)
	}
	// A nil sub-provider degrades to ErrNoMatch for its kinds.
	videoOnly := CompositeProvider{Video: videoSourceStub{source: "tmdb"}}
	if _, err := videoOnly.Lookup(context.Background(), TitleRef{Kind: "artist", Title: "x"}); err != ErrNoMatch {
		t.Errorf("nil music provider err = %v, want ErrNoMatch", err)
	}
}
