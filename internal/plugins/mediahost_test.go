package plugins

import (
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// TestMediaHostMatching: the allowlist entry for a media host is exact or a
// domain suffix (".example.com"). A suffix covers every host UNDER the domain and
// nothing that merely ends in the same letters.
func TestMediaHostMatching(t *testing.T) {
	parallel(t)
	for _, tc := range []struct {
		entries []string
		host    string
		want    bool
	}{
		{[]string{"cdn.example.com"}, "cdn.example.com", true},
		{[]string{"cdn.example.com"}, "CDN.Example.COM", true},
		{[]string{"cdn.example.com"}, "a.cdn.example.com", false},
		{[]string{".example.com"}, "a.example.com", true},
		{[]string{".example.com"}, "a.b.example.com", true},
		{[]string{".example.com"}, "A.B.EXAMPLE.COM.", true},
		{[]string{".example.com"}, "example.com", false},
		{[]string{".example.com"}, "example.com.evil.net", false},
		{[]string{".example.com"}, "notexample.com", false},
		{[]string{".example.com"}, "a.notexample.com", false},
		{[]string{".example.com"}, "", false},
		{nil, "a.example.com", false},
		{[]string{"other.test", ".example.com"}, "x.example.com", true},
	} {
		if got := hostMatchesAllowlist(tc.entries, tc.host); got != tc.want {
			t.Errorf("hostMatchesAllowlist(%v, %q) = %v, want %v", tc.entries, tc.host, got, tc.want)
		}
	}
}

// TestManifestAcceptsTheSuffixFormAndRefusesMalformedOnes.
func TestManifestAcceptsTheSuffixFormAndRefusesMalformedOnes(t *testing.T) {
	parallel(t)
	for _, h := range []string{".googlevideo.com", ".a.b.example.com", ".xn--bcher-kva.example"} {
		m := validManifestForTest()
		m.Network.Hosts = []string{h}
		if err := validateManifest(m); err != nil {
			t.Errorf("suffix %q refused: %v", h, err)
		}
	}
	for _, h := range []string{
		".", "..", ".com", "..example.com", ".example..com", ".example.com.", ".-example.com", ".example-.com",
		".ex ample.com", ".*.example.com", "*.example.com", ".example.com:443", ".exa_mple.com", ".1.2", ".127.0.0.1",
	} {
		m := validManifestForTest()
		m.Network.Hosts = []string{h}
		if err := validateManifest(m); err == nil {
			t.Errorf("malformed suffix %q was accepted", h)
		}
	}
}

// TestASuffixEntryWidensMediaHostsOnly: the entry licenses the host of a media
// URL; it does not widen what the Plugin's own fetches may reach.
func TestASuffixEntryWidensMediaHostsOnly(t *testing.T) {
	parallel(t)
	m := pluginapi.Manifest{Network: pluginapi.ManifestNetwork{Hosts: []string{"api.example.test", ".media.example.test"}}}
	p := &Plugin{id: "tube", manifest: m, hosts: map[string]struct{}{"api.example.test": {}}}
	s := &Set{plugins: []*Plugin{p}}

	if !s.MediaHostAllowed("tube", "api.example.test") || !s.MediaHostAllowed("tube", "a.media.example.test") {
		t.Fatal("an exact entry and a suffix entry should both license a media host")
	}
	if s.MediaHostAllowed("tube", "media.example.test") || s.MediaHostAllowed("other", "api.example.test") {
		t.Fatal("the apex of a suffix entry, or another Plugin's host, was licensed")
	}
	if !p.allows("api.example.test") || p.allows("a.media.example.test") {
		t.Fatal("the Plugin's own fetches must stay exact: a suffix entry is for media hosts only")
	}
	var nilSet *Set
	if nilSet.MediaHostAllowed("tube", "api.example.test") {
		t.Fatal("a nil Set licensed a host")
	}
}
