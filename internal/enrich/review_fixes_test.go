package enrich

import (
	"context"
	"errors"
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
