package v1

import "testing"

// MediaRef.ID is how a guest reads an id: ExternalIDs is the only carrier, keyed
// by namespace, trimmed on read.

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

// A blank map entry is no id.
func TestIDSkipsABlankEntry(t *testing.T) {
	ref := MediaRef{ExternalIDs: map[string]string{NamespaceIMDB: "  "}}
	if got := ref.ID(NamespaceIMDB); got != "" {
		t.Errorf("ID(imdb) = %q, want empty for a blank entry", got)
	}
}
