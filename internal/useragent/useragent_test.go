package useragent

import (
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/server"
)

// The outbound identity's SHAPE. These two tests moved here from internal/enrich
// with the string itself (.scratch/bundled-plugins issue 02): the identity is no
// longer enrichment's alone, because the plugin host stamps the same one onto
// every fetch a guest makes.

// TestDefaultShape: the UA carries a version and a reachable contact, and is none
// of the anonymous forms MusicBrainz calls out.
func TestDefaultShape(t *testing.T) {
	ua := Default
	if !strings.HasPrefix(ua, "obelo/") {
		t.Errorf("UA must lead with the application name: %q", ua)
	}
	openIdx, closeIdx := strings.Index(ua, "("), strings.Index(ua, ")")
	if openIdx < 0 || closeIdx < openIdx {
		t.Fatalf("UA must carry a parenthesised contact: %q", ua)
	}
	contact := ua[openIdx+1 : closeIdx]
	if !strings.Contains(contact, "https://www.obelo.tv") || !strings.Contains(contact, "metadata@obelo.tv") {
		t.Errorf("UA contact must reach the project: %q", contact)
	}
	for _, bad := range []string{"Java", "Python-urllib", "Go-http-client", "self-hosted"} {
		if strings.Contains(ua, bad) {
			t.Errorf("UA contains the anonymous/generic marker %q: %s", bad, ua)
		}
	}
}

// TestDefaultTracksBuildVersion: the version is READ from server.Version, never
// hand-copied. The string this replaced said "obelo/1.0" against a 0.1.0 build; a
// host trying to pin a misbehaving release got a version that never existed.
// Asserting on the constant (not a literal) is what keeps it honest — and the
// plugin host's own second copy of "obelo/1.0" is exactly what ADR-0059 decision
// 7 retired.
func TestDefaultTracksBuildVersion(t *testing.T) {
	if !strings.Contains(Default, "obelo/"+server.Version+" ") {
		t.Errorf("UA %q does not carry the build version %q", Default, server.Version)
	}
}

// TestForPluginAppendsTheProductComment: a Plugin's fetch is still Obelo, with
// the Plugin named after the contact so a source can tell which part called.
func TestForPluginAppendsTheProductComment(t *testing.T) {
	got := ForPlugin("tmdb", "1.0.0")
	want := Default + " plugin/tmdb/1.0.0"
	if got != want {
		t.Errorf("ForPlugin = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, "obelo/"+server.Version+" ") {
		t.Errorf("a plugin's agent must still lead with the server's own identity: %q", got)
	}
}

// TestForPluginWithoutAVersion: a manifest need not carry one, and a bare slash
// would be a version nobody can read.
func TestForPluginWithoutAVersion(t *testing.T) {
	if got, want := ForPlugin("tmdb", ""), Default+" plugin/tmdb"; got != want {
		t.Errorf("ForPlugin with no version = %q, want %q", got, want)
	}
	if got := ForPlugin("", "1.0.0"); got != Default {
		t.Errorf("ForPlugin with no id = %q, want the plain default", got)
	}
}

// TestForPluginRefusesToLetAManifestWriteHeaders: the version is opaque author
// prose the host never parses, so it is the one part of this string somebody else
// controls. A carriage return in it would be a Plugin writing its own request
// headers; a kilobyte of it would be a Plugin filling somebody's access log.
func TestForPluginRefusesToLetAManifestWriteHeaders(t *testing.T) {
	got := ForPlugin("evil", "1.0\r\nX-Injected: yes")
	for _, bad := range []string{"\r", "\n"} {
		if strings.Contains(got, bad) {
			t.Fatalf("a manifest version put a control character into the agent: %q", got)
		}
	}
	if !strings.HasPrefix(got, Default+" plugin/evil/") {
		t.Errorf("the sanitised agent lost its shape: %q", got)
	}

	long := ForPlugin(strings.Repeat("a", 500), strings.Repeat("b", 500))
	if len(long) > len(Default)+len(" plugin//")+2*maxToken {
		t.Errorf("a 1000-character manifest produced a %d-character agent: %q", len(long), long)
	}
}
