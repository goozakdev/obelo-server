package builtins_test

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/builtins"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// What this binary still has compiled into it, stated as a list
// (.scratch/bundled-plugins: issue 08).
//
// It is worth a test because two other things read the answer and neither says it
// out loud. internal/plugins' checkDuplicate refuses an Admin's upload whose id
// this registry already claims — "the id %q is already claimed by a plugin this
// server ships" — so every id here is an id no third party may ever use, and
// adding one silently takes a name away from the world. And the enrichment
// catalog's ORDER is "the Bundled plugins, then the Built-ins, then everything
// else alphabetically" (plugins.Set.RegisterEnabledAround), so a Built-in Metadata
// provider reappearing here would land between the shipped plugins and an Admin's
// and could quietly become a kind's default lead.
//
// Until ADR-0059 this registry also held eight Metadata providers — TMDB, OMDb,
// TheTVDB, AniDB, MusicBrainz, the Cover Art Archive, fanart.tv and TheAudioDB.
// They are Bundled plugins now: WebAssembly modules under plugins/<id>/, embedded
// by internal/bundled and installed into the data directory on first boot, which
// means they reach the same Registry by the same route an Admin's upload does.
// Bundling the two below is follow-up issue 09; when it lands, this test's lists
// go empty rather than gaining a third name.
func TestTheBuiltInsAreOpenSubtitlesAndTheWebhookAndNothingElse(t *testing.T) {
	reg := pluginapi.NewRegistry()
	builtins.Register(reg)

	var metadata []string
	for _, r := range reg.MetadataProviders() {
		metadata = append(metadata, r.Descriptor.Slug)
	}
	if len(metadata) != 0 {
		t.Errorf("Built-in Metadata providers = %v, want none — every shipped source is a "+
			"Bundled plugin (ADR-0059), and a compiled-in one would register ahead of an "+
			"Admin's and could take over a kind's lead", metadata)
	}

	assertOnly(t, "Subtitle provider", subtitleSlugs(reg), "opensubtitles")
	assertOnly(t, "Event sink", sinkSlugs(reg), "webhook")
}

func subtitleSlugs(reg *pluginapi.Registry) []string {
	var out []string
	for _, r := range reg.SubtitleProviders() {
		out = append(out, r.Descriptor.Slug)
	}
	return out
}

func sinkSlugs(reg *pluginapi.Registry) []string {
	var out []string
	for _, r := range reg.EventSinks() {
		out = append(out, r.Descriptor.Slug)
	}
	return out
}

func assertOnly(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("Built-in %ss = %v, want exactly %v", what, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Built-in %s %d = %q, want %q", what, i, got[i], want[i])
		}
	}
}
