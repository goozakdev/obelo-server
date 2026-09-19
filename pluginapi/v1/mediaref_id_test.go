package v1

import "testing"

// MediaRef.ID is how a guest reads an id, and it has to answer two hosts: one that
// fills only ExternalIDs (a host built after ADR-0060 decision 7 whose caller set
// only the map) and one that fills only the five named fields (a host built before
// the map existed). Both directions, and the rule between them, are pinned here.

func TestIDReadsTheMap(t *testing.T) {
	ref := MediaRef{ExternalIDs: map[string]string{
		NamespaceTMDB: "438631", NamespaceAniDB: "69", "anilist": "  21  ",
	}}
	for ns, want := range map[string]string{
		NamespaceTMDB: "438631", NamespaceAniDB: "69", "anilist": "21",
		NamespaceIMDB: "", "kitsu": "",
	} {
		if got := ref.ID(ns); got != want {
			t.Errorf("ID(%q) = %q, want %q", ns, got, want)
		}
	}
}

func TestIDFallsBackToTheNamedFieldsOfAnOlderHost(t *testing.T) {
	ref := MediaRef{
		TMDBID: "1396", IMDBID: "tt0903747", MusicbrainzID: " b10bbbfc ",
		TheTVDBID: "81189", AniDBID: "1",
	}
	for ns, want := range map[string]string{
		NamespaceTMDB: "1396", NamespaceIMDB: "tt0903747", NamespaceMusicBrainz: "b10bbbfc",
		NamespaceTheTVDB: "81189", NamespaceAniDB: "1",
		// A third-party namespace has no named field to fall back to.
		"anilist": "",
	} {
		if got := ref.ID(ns); got != want {
			t.Errorf("ID(%q) = %q, want %q", ns, got, want)
		}
	}
}

// The host never sends the two disagreeing, but a guest must still have one rule:
// the map is the carrier, so it wins; a blank map entry is no id and does not hide
// the named field.
func TestIDPrefersTheMapAndSkipsABlankEntry(t *testing.T) {
	ref := MediaRef{
		ExternalIDs: map[string]string{NamespaceTMDB: "2", NamespaceIMDB: "  "},
		TMDBID:      "1",
		IMDBID:      "tt1",
	}
	if got := ref.ID(NamespaceTMDB); got != "2" {
		t.Errorf("ID(tmdb) = %q, want the map's %q", got, "2")
	}
	if got := ref.ID(NamespaceIMDB); got != "tt1" {
		t.Errorf("ID(imdb) = %q, want the named field's %q", got, "tt1")
	}
}

func TestNamedNamespacesAreTheFiveMirrors(t *testing.T) {
	ref := MediaRef{TMDBID: "a", IMDBID: "b", MusicbrainzID: "c", TheTVDBID: "d", AniDBID: "e"}
	var got string
	for _, ns := range NamedNamespaces() {
		got += ref.NamedID(ns)
	}
	if got != "abcde" {
		t.Errorf("the named namespaces read %q through NamedID, want every mirror once in order", got)
	}
	if ref.NamedID("anilist") != "" {
		t.Error("a namespace without a mirror field read one")
	}
}
