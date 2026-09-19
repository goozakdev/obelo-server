package enrich

import (
	"reflect"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The host fills BOTH id carriers on the wire (ADR-0060 decision 7): the map a new
// guest reads through MediaRef.ID, and the five named mirrors a v1 guest reads. These
// pin the TitleRef → MediaRef mapping from each side a caller can set.

// Every caller today sets only the named fields; a new guest must still find them
// in the map.
func TestWireRefFromNamedFieldsFillsTheMap(t *testing.T) {
	got := wireRefFromTitleRef(TitleRef{
		Kind: "episode", TMDBID: "1396", IMDBID: "tt0903747", TheTVDBID: "81189", AniDBID: " 1 ",
	})
	want := map[string]string{"tmdb": "1396", "imdb": "tt0903747", "thetvdb": "81189", "anidb": "1"}
	if !reflect.DeepEqual(got.ExternalIDs, want) {
		t.Errorf("ExternalIDs = %v, want %v", got.ExternalIDs, want)
	}
	if got.TMDBID != "1396" || got.IMDBID != "tt0903747" || got.TheTVDBID != "81189" ||
		got.AniDBID != "1" || got.MusicbrainzID != "" {
		t.Errorf("named mirrors = %+v, want the same ids back", got)
	}
}

// Issue 15's callers will set only the map; a v1 guest must still find the five
// shipped namespaces in the named fields, and a third party's id rides in the map.
func TestWireRefFromTheMapFillsTheNamedMirrors(t *testing.T) {
	got := wireRefFromTitleRef(TitleRef{Kind: "show", ExternalIDs: map[string]string{
		"tmdb": "1396", "musicbrainz": "mb", "thetvdb": "81189", "anidb": "69", "anilist": "21",
	}})
	if got.TMDBID != "1396" || got.MusicbrainzID != "mb" || got.TheTVDBID != "81189" || got.AniDBID != "69" {
		t.Errorf("named mirrors = %+v, want each filled from the map", got)
	}
	if got.IMDBID != "" {
		t.Errorf("IMDBID = %q, want empty: the map held no imdb id", got.IMDBID)
	}
	if got.ExternalIDs["anilist"] != "21" || got.ID("anilist") != "21" {
		t.Errorf("a third-party namespace did not reach the wire: %v", got.ExternalIDs)
	}
}

// Where the two disagree the map is the carrier and wins, and the named mirror is
// set to it, so a guest sees one id whichever carrier it reads. A blank on either
// side is no id.
func TestWireRefMapWinsAndBlanksAreDropped(t *testing.T) {
	in := TitleRef{
		ExternalIDs: map[string]string{"tmdb": "2", "imdb": "  ", "": "x"},
		TMDBID:      "1",
		IMDBID:      "tt1",
		AniDBID:     "  ",
	}
	got := wireRefFromTitleRef(in)
	want := map[string]string{"tmdb": "2", "imdb": "tt1"}
	if !reflect.DeepEqual(got.ExternalIDs, want) {
		t.Errorf("ExternalIDs = %v, want %v", got.ExternalIDs, want)
	}
	if got.TMDBID != "2" || got.IMDBID != "tt1" || got.AniDBID != "" {
		t.Errorf("named mirrors = %+v, want tmdb=2 (the map's), imdb=tt1, no anidb", got)
	}
	if in.ExternalIDs["tmdb"] != "2" || len(in.ExternalIDs) != 3 {
		t.Errorf("the caller's map was written to: %v", in.ExternalIDs)
	}
}

func TestWireRefWithNoIDsSendsNoMap(t *testing.T) {
	if got := wireRefFromTitleRef(TitleRef{Kind: "movie", Title: "Dune"}); got.ExternalIDs != nil {
		t.Errorf("ExternalIDs = %v, want nil so the key is omitted on the wire", got.ExternalIDs)
	}
}

// A declared probe reference may use either carrier; both read into the TitleRef.
func TestTitleRefFromWireReadsBothCarriers(t *testing.T) {
	got := titleRefFromWire(pluginapi.MediaRef{
		Kind:          "album",
		ExternalIDs:   map[string]string{"anilist": "21", "tmdb": "5"},
		MusicbrainzID: "mb",
	})
	if got.MusicbrainzID != "mb" || got.TMDBID != "5" {
		t.Errorf("named = %+v, want musicbrainz from the named field and tmdb from the map", got)
	}
	if got.ExternalIDs["anilist"] != "21" {
		t.Errorf("ExternalIDs = %v, want the third-party id kept", got.ExternalIDs)
	}
	back := wireRefFromTitleRef(got)
	if back.ID("musicbrainz") != "mb" || back.ID("tmdb") != "5" || back.ID("anilist") != "21" {
		t.Errorf("round trip lost an id: %v", back.ExternalIDs)
	}
}

// The connection probe's image call names the resolved id under its record's
// namespace — and under the Plugin's id when the record names none — and no longer
// scatters it across every named field.
func TestProbeRefWithIDKeysTheIDByNamespace(t *testing.T) {
	probe := TitleRef{Kind: "album", ExternalIDs: map[string]string{"x": "y"}}

	got := wireRefFromTitleRef(probeRefWithID(probe, "musicbrainz",
		TitleMetadata{ExternalID: "rg-1", Source: "musicbrainz"}))
	if got.ID("musicbrainz") != "rg-1" || got.MusicbrainzID != "rg-1" {
		t.Errorf("musicbrainz id = %q / mirror %q, want rg-1", got.ID("musicbrainz"), got.MusicbrainzID)
	}
	if got.TMDBID != "" || got.IMDBID != "" || got.TheTVDBID != "" || got.AniDBID != "" {
		t.Errorf("the id leaked into another namespace: %+v", got)
	}
	if probe.ExternalIDs["musicbrainz"] != "" {
		t.Error("the declared probe reference was written to")
	}

	got = wireRefFromTitleRef(probeRefWithID(probe, "a-guest", TitleMetadata{ExternalID: "7"}))
	if got.ID("a-guest") != "7" || got.ID("x") != "y" {
		t.Errorf("ExternalIDs = %v, want the plugin id's namespace and the declared ids", got.ExternalIDs)
	}

	if got := probeRefWithID(probe, "a-guest", TitleMetadata{}); !reflect.DeepEqual(got, probe) {
		t.Errorf("an empty id changed the reference: %+v", got)
	}
}
