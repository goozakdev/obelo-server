package enrich

import (
	"context"
	"testing"
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
