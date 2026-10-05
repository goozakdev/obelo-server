package enrich

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// libraryOff makes the service resolve every Library to a snapshot with music
// enrichment switched off, while the GLOBAL snapshot stays enabled — the shape of
// a Library whose Enrichment policy turns enrichment off.
func libraryOff(svc *Service) {
	global := svc.snapshot()
	svc.resolveLibrary = func(context.Context, string) (providerSnapshot, error) {
		off := global
		off.enablement = Enablement{}
		return off, nil
	}
}

// R04-02: an Album Cascade on a pinned release-group must read that group's
// tracklist BY ID when the title search does not surface it (a pasted id, a tag
// title that differs from MusicBrainz, a common title), not write every Track
// 'album-unmatched'.
func TestCascadeAlbumReadsPinnedGroupWhenSearchMissesIt(t *testing.T) {
	prov := &editionProvider{
		albumTierProvider: &albumTierProvider{
			recordings: map[string]string{"rec-fit-1": "Nessun Dorma"},
		},
		searchAlbums: []Candidate{{ExternalID: "rg-other", Title: "Other", Kind: "album"}},
		fit:          []TrackCandidate{entry(1, "Nessun Dorma", "rec-fit-1")},
	}
	svc, db := newAlbumFixture(t, prov, seedAlbum{
		entityRecord: "rg-she",
		tracks:       []seedTrack{{id: "t1", title: "Nessun Dorma", num: 1}},
	})

	sum, err := svc.CascadeEntity(context.Background(), store.EntityAlbum, "al1", "rg-she")
	if err != nil {
		t.Fatalf("cascade: %v", err)
	}
	if sum.Updated != 1 || sum.Attention != 0 {
		t.Fatalf("summary = %+v, want Updated 1 Attention 0 (calls: %v)", sum, prov.history())
	}
	if rec := trackRow(t, db, "t1").MusicbrainzID; rec != "rec-fit-1" {
		t.Errorf("track pinned %q, want rec-fit-1", rec)
	}
}

// parseCountingProvider answers a paste and counts how often it is asked to parse.
type parseCountingProvider struct {
	parses int
}

func (p *parseCountingProvider) Lookup(context.Context, TitleRef) (TitleMetadata, error) {
	return TitleMetadata{Matched: true, Name: "Heat", Year: 1995, ExternalID: "949"}, nil
}

func (p *parseCountingProvider) Search(context.Context, string, string, SearchOptions) ([]Candidate, error) {
	return nil, ErrSearchUnavailable
}

func (p *parseCountingProvider) ArtworkCandidates(context.Context, TitleRef, string) ([]ArtworkCandidate, error) {
	return nil, nil
}

func (p *parseCountingProvider) ParseExternalRef(_ context.Context, _, pasted string) (ExternalRef, error) {
	p.parses++
	return ExternalRef{ExternalID: pasted, Namespace: "tmdb"}, nil
}

// R04-14: a pasted id is parsed once (a plugin round-trip), not once by findIn
// and again by previewExternal.
func TestFindParsesAPastedRefOnce(t *testing.T) {
	prov := &parseCountingProvider{}
	svc := NewService(nil, prov, noArtwork{}, Enablement{Video: true, Music: true}, t.TempDir(), 0)

	got, err := svc.findIn(context.Background(), svc.snapshot(), "movie", "949", SearchOptions{})
	if err != nil {
		t.Fatalf("findIn: %v", err)
	}
	if !got.ResolvedRef || len(got.Candidates) != 1 {
		t.Fatalf("result = %+v, want one resolved candidate", got)
	}
	if prov.parses != 1 {
		t.Errorf("ParseExternalRef called %d times, want 1", prov.parses)
	}
}

type formatFetcher struct{ contentType string }

func (f *formatFetcher) Fetch(context.Context, string) ([]byte, string, error) {
	return []byte("image-bytes"), f.contentType, nil
}

// R04-13: a role whose image switches format leaves exactly one file, and the
// write leaves no temp file behind.
func TestCacheArtworkReplacesAFormatChange(t *testing.T) {
	dir := t.TempDir()
	f := &formatFetcher{contentType: "image/jpeg"}
	svc := NewService(nil, CompositeProvider{}, f, Enablement{}, dir, 0)
	ar := ArtworkRef{Role: "poster", URL: "http://x/p"}

	first, ok := svc.cacheArtwork(context.Background(), "k1", ar)
	if !ok || first != "k1-poster.jpg" {
		t.Fatalf("first = %q, %v", first, ok)
	}
	f.contentType = "image/png"
	second, ok := svc.cacheArtwork(context.Background(), "k1", ar)
	if !ok || second != "k1-poster.png" {
		t.Fatalf("second = %q, %v", second, ok)
	}
	// The old-format file survives until the caller has persisted the new name.
	if _, err := os.Stat(filepath.Join(dir, first)); err != nil {
		t.Fatalf("old-format file removed before the new path was persisted: %v", err)
	}
	svc.pruneArtworkFormats(second)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "k1-poster.png" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("cache dir = %v, want only k1-poster.png", names)
	}
}

// R04-07: item-scoped reads honour the item's Library policy, not the global
// snapshot.
func TestCascadeAlbumUsesTheLibrarySnapshot(t *testing.T) {
	prov := &editionProvider{
		albumTierProvider: &albumTierProvider{
			recordings: map[string]string{"rec-preview-1": "Nessun Dorma"},
		},
		searchAlbums: []Candidate{{
			ExternalID: "rg-she", Title: "She", Kind: "album",
			Tracklist: []TrackCandidate{entry(1, "Nessun Dorma", "rec-preview-1")},
		}},
	}
	svc, _ := newAlbumFixture(t, prov, seedAlbum{
		entityRecord: "rg-she",
		tracks:       []seedTrack{{id: "t1", title: "Nessun Dorma", num: 1}},
	})
	libraryOff(svc)

	sum, err := svc.CascadeEntity(context.Background(), store.EntityAlbum, "al1", "rg-she")
	if err != nil {
		t.Fatalf("cascade: %v", err)
	}
	if sum.Updated != 0 {
		t.Errorf("cascade updated %d tracks in a Library with enrichment off (calls: %v)", sum.Updated, prov.history())
	}
	if n := prov.count("album?search"); n != 0 {
		t.Errorf("searched %d times for a Library with enrichment off", n)
	}
}

func TestAlbumEditionsUsesTheLibrarySnapshot(t *testing.T) {
	svc, _, _ := viaggioFixture(t, seedAlbum{})
	libraryOff(svc)

	if _, err := svc.AlbumEditions(context.Background(), "al1"); !errors.Is(err, ErrSearchUnavailable) {
		t.Errorf("AlbumEditions err = %v, want ErrSearchUnavailable for a Library with enrichment off", err)
	}
}

func TestEntityArtworkCandidatesUseTheLibrarySnapshot(t *testing.T) {
	svc, _, _ := viaggioFixture(t, seedAlbum{})
	libraryOff(svc)

	if _, err := svc.ListEntityArtworkCandidates(context.Background(), store.EntityAlbum, "al1", "cover"); !errors.Is(err, ErrSearchUnavailable) {
		t.Errorf("ListEntityArtworkCandidates err = %v, want ErrSearchUnavailable for a Library with enrichment off", err)
	}
}

// R04-07: ResolveIdentity reads through the given Library's snapshot, not the
// global one — a Library with enrichment off makes no provider call.
func TestResolveIdentityUsesTheLibrarySnapshot(t *testing.T) {
	global := &stubProvider{meta: TitleMetadata{Matched: true, Name: "Global"}}
	lib := &stubProvider{meta: TitleMetadata{Matched: true, Name: "Library"}}
	svc := NewService(nil, global, nil, Enablement{Video: true}, "", 0)
	svc.resolveLibrary = func(_ context.Context, id string) (providerSnapshot, error) {
		if id == "off" {
			return providerSnapshot{provider: lib}, nil
		}
		return providerSnapshot{provider: lib, enablement: Enablement{Video: true}}, nil
	}
	ref := TitleRef{Kind: "movie", TMDBID: "1"}

	name, _, matched, err := svc.ResolveIdentity(context.Background(), "on", ref)
	if err != nil || !matched || name != "Library" {
		t.Fatalf("on: got (%q, %v, %v), want Library", name, matched, err)
	}
	_, _, matched, err = svc.ResolveIdentity(context.Background(), "off", ref)
	if err != nil || matched {
		t.Fatalf("off: matched=%v err=%v, want not matched", matched, err)
	}
	if global.calls != 0 || lib.calls != 1 {
		t.Errorf("calls = global:%d library:%d, want 0 and 1", global.calls, lib.calls)
	}
}
