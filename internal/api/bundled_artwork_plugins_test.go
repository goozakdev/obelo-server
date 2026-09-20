package api_test

import (
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/goozakdev/obelo-server/internal/enrich"
	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/rotation"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// The two ARTWORK sources, end to end as Bundled plugins (ADR-0059,
// .scratch/bundled-plugins issue 07): fanart.tv and TheAudioDB.
//
// The behaviour they carry is already covered by this package's existing suites —
// artist photos, movie/show fanart, and the music chain's image and bio fill —
// and those inject a fake provider, so they pass unmodified and prove the
// COMPOSITION is unchanged. What is new, and what this file covers, is the two
// facts that moved out of internal/enrich when the registrations left it:
//
//  1. fanart.tv must stay AHEAD of TheAudioDB. Catalog.musicSupplements hands
//     the music chain its Supplements in registration order (the fill order), and what holds
//     that order now is internal/bundled's ordered id list rather than two adjacent
//     literals in MetadataPlugins().
//  2. the ADR-0032 rotator's default fanart.tv key must still land in the
//     `fanarttv` row. It is written BY ID, against a row that no longer comes from
//     a compiled-in registration.

// TestAFreshServerInstallsTheShippedArtworkPlugins: both artwork sources arrive on
// first boot, as bundled, enabled, on disk, and on the providers screen — with
// fanart.tv before TheAudioDB.
func TestAFreshServerInstallsTheShippedArtworkPlugins(t *testing.T) {
	srv := testharness.New(t)
	token := adminToken(t, srv)

	installed := readPlugins(t, srv, token)
	for _, id := range []string{"fanarttv", "theaudiodb"} {
		got := pluginNamed(t, installed, id)
		if got.Origin != plugins.OriginBundled {
			t.Errorf("%s origin = %q, want %q", id, got.Origin, plugins.OriginBundled)
		}
		if !got.Enabled || got.DisabledByFailure || got.LastError != "" {
			t.Fatalf("the shipped %s plugin did not come up: %+v", id, got)
		}
		if got.Version == "" || got.APIVersion != 1 {
			t.Errorf("%s version/apiVersion = %q/%d, want the manifest's", id, got.Version, got.APIVersion)
		}
		// On disk, exactly where an Admin's upload would be.
		dir := filepath.Join(srv.DataDir, plugins.DirName, id)
		for _, name := range []string{plugins.ManifestFile, plugins.DefaultModuleFile} {
			if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
				t.Errorf("%s is missing from %s: %v", name, dir, err)
			}
		}
	}

	// ORDER: the providers screen lists the catalog in registration order, and the
	// music chain fills its two image slots from that same order. fanart.tv ahead of
	// TheAudioDB is what makes fanart.tv the preferred artist photo and TheAudioDB
	// the image-plus-biography fallback; swapping them is a silent behaviour change
	// no other test in this package would notice.
	providers := readMetadataProviders(t, srv, token)
	fanart, audiodb := -1, -1
	for i, p := range providers {
		switch p.Slug {
		case "fanarttv":
			fanart = i
		case "theaudiodb":
			audiodb = i
		}
	}
	if fanart < 0 || audiodb < 0 {
		t.Fatalf("the providers screen lists %+v, want both fanarttv and theaudiodb", providers)
	}
	if fanart > audiodb {
		t.Errorf("the providers screen lists theaudiodb (%d) before fanarttv (%d); registration order "+
			"decides which is the music chain's PREFERRED artist image source", audiodb, fanart)
	}
}

// TestAnUnkeyedArtworkPluginIsListedButNotKeyed: both sources require a key, so a
// server with none shows them present and unkeyed rather than absent. That is the
// state an operator sees before they paste a key, and it is what `requiresSecret`
// in the manifest buys.
func TestAnUnkeyedArtworkPluginIsListedButNotKeyed(t *testing.T) {
	srv := testharness.New(t)
	token := adminToken(t, srv)

	for _, p := range readMetadataProviders(t, srv, token) {
		if p.Slug != "fanarttv" && p.Slug != "theaudiodb" {
			continue
		}
		if p.HasAPIKey {
			t.Errorf("%s reports a key on a server that was given none: %+v", p.Slug, p)
		}
		if p.Name == "" {
			t.Errorf("%s has no name on the settings screen; the manifest's is what an operator reads", p.Slug)
		}
	}
}

// fanartRecordingBuilder captures the fanart.tv key each rebuild was composed
// with, so a test can read the resolved default credential by VALUE rather than
// only see that some key exists. It is this file's own, beside the TMDB one the
// rotation suite keeps.
type fanartRecordingBuilder struct {
	mu   sync.Mutex
	last string
}

func (rb *fanartRecordingBuilder) build() enrich.BuildFunc {
	return func(cfg enrich.ProviderConfig) (enrich.MetadataProvider, enrich.Enablement) {
		rb.mu.Lock()
		rb.last = cfg.ProviderKeys[enrich.SlugFanartTV]
		rb.mu.Unlock()
		return enrich.CompositeProvider{}, enrich.DeriveEnablement(cfg)
	}
}

func (rb *fanartRecordingBuilder) key() string {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	return rb.last
}

// TestTheRotatorsDefaultFanartKeyLandsInTheFanartTVRow is acceptance criterion 3:
// the ADR-0032 rotator writes its bundled default fanart.tv key into the
// `fanarttv` row BY ID, and a rotated key supersedes it — against a row that no
// longer comes from a compiled-in registration.
//
// It is the half the existing rotation suite does not assert. Those tests read the
// TMDB key back off the rebuilt provider and the fanart key off the on-disk cache;
// neither looks at the fanarttv ROW or at the fanart key reaching a composition,
// which is the thing this port could have broken.
func TestTheRotatorsDefaultFanartKeyLandsInTheFanartTVRow(t *testing.T) {
	encKey := newRotationEncKey(t)
	stub := &rotationStub{encKey: encKey, keys: rotation.Keys{TMDB: "rot-tmdb", Fanart: "fan-A"}}
	endpoint := stub.serve(t)

	rb := &fanartRecordingBuilder{}
	srv := testharness.New(t,
		testharness.WithProviderBuilder(rb.build()),
		// interval 0 → no periodic timer; the test drives polls deterministically.
		testharness.WithKeyRotation(endpoint.URL, encKey, 0),
	)
	token := adminToken(t, srv)

	srv.RefreshRotationKeys()

	if got := rb.key(); got != "fan-A" {
		t.Errorf("the composition was given the fanart.tv key %q, want fan-A — the rotator writes "+
			"by id, and the id did not change when fanart.tv became a plugin", got)
	}
	row := providerRow(t, readMetadataProviders(t, srv, token), "fanarttv")
	if !row.HasAPIKey {
		t.Error("the rotator's default fanart.tv key did not reach the fanarttv row")
	}
	// A newly-arrived default turns the source on, which is what makes artwork work
	// out of the box on an official-keys build.
	if !row.Enabled {
		t.Errorf("fanarttv row = %+v, want enabled once a default key arrived", row)
	}

	// A ROTATED key supersedes it without a restart — the whole point of the channel,
	// asserted for the id this issue moved.
	stub.setKeys(rotation.Keys{TMDB: "rot-tmdb", Fanart: "fan-B"})
	srv.RefreshRotationKeys()
	if got := rb.key(); got != "fan-B" {
		t.Errorf("the composition was given %q after a rotation, want fan-B", got)
	}

	// A server given no rotation endpoint has no key at all, so the assertions above
	// are about the ROTATOR rather than about some default the harness plants.
	plain := testharness.New(t)
	plainToken := adminToken(t, plain)
	if got := providerRow(t, readMetadataProviders(t, plain, plainToken), "fanarttv"); got.HasAPIKey {
		t.Errorf("a server with no rotation endpoint reports a fanart.tv key: %+v", got)
	}
}

// TestAnArtworkOnlyPluginCannotLeadALibrary: fanart.tv and TheAudioDB declare
// `class: artwork`, so the Authoritative-provider pointer must refuse them
// (ADR-0027). The class now comes from a manifest, so the refusal is worth
// asserting against the real shipped ones.
func TestAnArtworkOnlyPluginCannotLeadALibrary(t *testing.T) {
	srv := testharness.New(t, testharness.WithEnrichmentKey("test-key"))
	token := adminToken(t, srv)
	libID := createMusicLibrary(t, srv, token, t.TempDir())

	for _, slug := range []string{"fanarttv", "theaudiodb"} {
		putPolicy(t, srv, token, libID, map[string]any{"authoritativeProvider": slug}, http.StatusUnprocessableEntity)
	}
}

// providerRow finds one row on the providers screen.
func providerRow(t *testing.T, rows []metadataProviderRow, slug string) metadataProviderRow {
	t.Helper()
	for _, r := range rows {
		if r.Slug == slug {
			return r
		}
	}
	t.Fatalf("the providers screen has no %q row: %+v", slug, rows)
	return metadataProviderRow{}
}
