package enrich

import (
	"context"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// TestConnection recognizes and probes every registered provider, and rejects an
// unknown slug without any call.
//
// It used to prove that against OMDb and TheTVDB, which were the two Built-ins
// added to the registry after the settings surface. Both are Bundled plugins now
// (.scratch/bundled-plugins: issue 05), and the stand-in these white-box suites
// compose a catalog from answers no-match without a request — so a probe against
// either would pass whatever the provider did, which is a test that cannot fail.
// What the probe actually exercises for a plugin is the sandbox, and that is
// proved where it lives: internal/api drives the real bundled modules through
// wazero, and internal/plugins drives the fetch policy those probes go through.
// What is left here is the one rule this function owns alone.

func TestTestConnectionUnknownSlug(t *testing.T) {
	ok, detail := TestConnection(context.Background(), shippedCatalog(), "nope", "key", "http://unused", "", "en-US")
	if ok || detail == "" {
		t.Errorf("unknown slug = ok:%v detail:%q, want ok:false with a detail", ok, detail)
	}
}

// TestTestConnectionSaysWhichURLsAnAdminEntered: an empty host in the request is
// the Descriptor's default, not a URL the operator typed, and the Settings the
// probe builds say so for each of the two.
func TestTestConnectionSaysWhichURLsAnAdminEntered(t *testing.T) {
	var got pluginapi.Settings
	registration := bundledStandIn(SlugTMDB)
	registration.New = func(s pluginapi.Settings) (pluginapi.MetadataProvider, error) {
		got = s
		return silentPlugin{}, nil
	}
	reg := pluginapi.NewRegistry()
	reg.RegisterMetadataProvider(registration)

	TestConnection(context.Background(), NewCatalog(reg), SlugTMDB, "key", "", "http://img.mirror", "en-US")
	if got.URL != registration.Descriptor.DefaultURL || got.URLEntered {
		t.Errorf("url = %q entered=%v, want the default, not entered", got.URL, got.URLEntered)
	}
	if got.URL2 != "http://img.mirror" || !got.URL2Entered {
		t.Errorf("url2 = %q entered=%v, want the typed host, entered", got.URL2, got.URL2Entered)
	}
}
