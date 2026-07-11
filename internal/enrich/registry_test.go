package enrich

import (
	"testing"

	"github.com/marioquake/juicebox/internal/store"
)

// TestProviderClass asserts the Full vs. Artwork-only capability the Authoritative-
// provider pointer constrains against (ADR-0027): the Full providers are eligible
// to lead; the Artwork-only providers can only ever be Supplements.
func TestProviderClass(t *testing.T) {
	wantFull := map[string]bool{SlugTMDB: true, SlugOMDb: true, SlugTheTVDB: true, SlugMusicBrainz: true}
	wantArtwork := map[string]bool{SlugFanartTV: true, SlugCoverArt: true, SlugTheAudioDB: true}
	for _, e := range Registry() {
		switch {
		case wantFull[e.Slug]:
			if e.Class != ClassFull {
				t.Errorf("%s class = %q, want full", e.Slug, e.Class)
			}
		case wantArtwork[e.Slug]:
			if e.Class != ClassArtworkOnly {
				t.Errorf("%s class = %q, want artwork", e.Slug, e.Class)
			}
		}
	}
}

// TestFullProvidersForKind asserts the candidate helper returns only Full providers
// of the requested kind, in registry order, and never an Artwork-only source.
func TestFullProvidersForKind(t *testing.T) {
	video := FullProvidersForKind(KindVideo)
	var slugs []string
	for _, e := range video {
		slugs = append(slugs, e.Slug)
		if e.Class != ClassFull {
			t.Errorf("video candidate %s is not Full", e.Slug)
		}
	}
	// TMDB, OMDb, TheTVDB are the Full video providers today (AniDB arrives in issue
	// 04); registry order puts TMDB first.
	if len(slugs) < 3 || slugs[0] != SlugTMDB {
		t.Errorf("video Full providers = %v, want TMDB-first list", slugs)
	}
	for _, e := range video {
		if e.Slug == SlugFanartTV {
			t.Errorf("fanart.tv (artwork-only) leaked into the video Full candidates")
		}
	}

	music := FullProvidersForKind(KindMusic)
	if len(music) != 1 || music[0].Slug != SlugMusicBrainz {
		t.Errorf("music Full providers = %v, want just MusicBrainz", music)
	}
}

// TestDefaultAuthoritativeForKind asserts the kind defaults the pointer inherits.
func TestDefaultAuthoritativeForKind(t *testing.T) {
	if got := DefaultAuthoritativeForKind(KindVideo); got != SlugTMDB {
		t.Errorf("video default authoritative = %q, want tmdb", got)
	}
	if got := DefaultAuthoritativeForKind(KindMusic); got != SlugMusicBrainz {
		t.Errorf("music default authoritative = %q, want musicbrainz", got)
	}
}

// TestProviderStatesFromRows asserts the per-slug reachability the resolver reads:
// enabled reflects the row; keyed is true for a keyless provider always and for a
// key-requiring one only with a key; a provider with no row is disabled + (for a
// key-requiring one) unkeyed.
func TestProviderStatesFromRows(t *testing.T) {
	rows := []store.MetadataProviderRow{
		{Slug: SlugTMDB, Enabled: true, APIKey: "tk"},
		{Slug: SlugOMDb, Enabled: false, APIKey: "ok"}, // disabled but keyed
		{Slug: SlugTheTVDB, Enabled: true, APIKey: ""}, // enabled but unkeyed
	}
	states := ProviderStatesFromRows(rows)

	if s := states[SlugTMDB]; !s.Enabled || !s.Keyed || s.APIKey != "tk" {
		t.Errorf("tmdb state = %+v, want enabled+keyed", s)
	}
	if s := states[SlugOMDb]; s.Enabled || !s.Keyed {
		t.Errorf("omdb state = %+v, want disabled+keyed (selectable authoritative)", s)
	}
	if s := states[SlugTheTVDB]; !s.Enabled || s.Keyed {
		t.Errorf("thetvdb state = %+v, want enabled+unkeyed (not selectable)", s)
	}
	// MusicBrainz needs no key: keyed even with no row.
	if s := states[SlugMusicBrainz]; !s.Keyed {
		t.Errorf("musicbrainz state = %+v, want keyed (requires no key)", s)
	}
	// A key-requiring provider with no row is unkeyed.
	if s := states[SlugFanartTV]; s.Enabled || s.Keyed {
		t.Errorf("fanarttv (no row) state = %+v, want disabled+unkeyed", s)
	}
}
