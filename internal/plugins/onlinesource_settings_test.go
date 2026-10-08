package plugins

import (
	"reflect"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// An Online source has no fixed-settings table, so the URL an Admin enters for it
// is the value of the manifest's declared `url` field (the one an author names
// "instance" for a PeerTube source). It must reach the Plugin as Settings.URL with
// URLEntered, be reachable by the Plugin's own fetches (ADR-0058, an
// operator-typed host), and be an exact media host for the first-URL check.

func onlineSettingsPlugin(values map[string]any) *Plugin {
	m := pluginapi.Manifest{
		Network: pluginapi.ManifestNetwork{Hosts: []string{"api.example.test"}},
		Settings: pluginapi.ManifestSettings{Fields: []pluginapi.SettingsField{
			{Key: "region", Type: pluginapi.FieldString, Label: "Region"},
			{Key: "instance", Type: pluginapi.FieldURL, Label: "Instance"},
		}},
	}
	p := &Plugin{id: "tube", manifest: m, hosts: map[string]struct{}{"api.example.test": {}}}
	p.SetSettingValues(values)
	return p
}

// TestAnAdminEnteredURLIsAnExactHttpsMediaHost: the entered URL's host is a media
// host, https only; another host, a sibling or an http:// URL is not.
func TestAnAdminEnteredURLIsAnExactHttpsMediaHost(t *testing.T) {
	parallel(t)
	s := &Set{plugins: []*Plugin{onlineSettingsPlugin(map[string]any{"instance": "https://media.example/"})}}
	if !s.MediaHostAllowed("tube", "media.example") || !s.MediaHostAllowed("tube", "MEDIA.example.") {
		t.Fatal("the host of the Admin's URL was not a media host")
	}
	if !s.MediaHostAllowed("tube", "api.example.test") {
		t.Fatal("the manifest's own host stopped being a media host")
	}
	for _, host := range []string{"other.example", "a.media.example", "example"} {
		if s.MediaHostAllowed("tube", host) {
			t.Errorf("%q was licensed by an Admin URL for media.example", host)
		}
	}

	plain := &Set{plugins: []*Plugin{onlineSettingsPlugin(map[string]any{"instance": "http://media.example/"})}}
	if plain.MediaHostAllowed("tube", "media.example") {
		t.Fatal("an http:// Admin URL was treated as a media host")
	}
	none := &Set{plugins: []*Plugin{onlineSettingsPlugin(nil)}}
	if none.MediaHostAllowed("tube", "media.example") {
		t.Fatal("a host nobody entered was licensed")
	}
}

// TestAnAdminEnteredURLReachesTheOnlineSourceAndItsFetchPolicy: the Settings a call
// carries hold the Admin's URL as entered, and its host and port are the operator
// address the Plugin's fetches may reach. With none entered the default stands and
// reaches nothing beyond the manifest.
func TestAnAdminEnteredURLReachesTheOnlineSourceAndItsFetchPolicy(t *testing.T) {
	parallel(t)
	def := pluginapi.Settings{Enabled: true, URL: "https://api.example.test/"}

	g := &guestOnlineSourceProvider{p: onlineSettingsPlugin(map[string]any{"instance": "https://media.example/"}), settings: def}
	got := g.current()
	if got.URL != "https://media.example/" || !got.URLEntered {
		t.Fatalf("settings = %+v, want the Admin's URL, entered", got)
	}
	if want := []string{dialKey("media.example", "443")}; !reflect.DeepEqual(addrsOf(got), want) {
		t.Fatalf("operator addresses = %v, want %v", addrsOf(got), want)
	}

	g = &guestOnlineSourceProvider{p: onlineSettingsPlugin(map[string]any{"region": "eu"}), settings: def}
	got = g.current()
	if got.URL != def.URL || got.URLEntered || len(addrsOf(got)) != 0 {
		t.Fatalf("with no Admin URL settings = %+v, want the default and nothing entered", got)
	}
}

// TestAManifestDefaultIsNotAnAdminURL: SettingValues fills a field's default in, so
// a url field with a `default` and nothing saved must not read as entered — else an
// author could name a private address and skip the allowlist and address checks.
func TestAManifestDefaultIsNotAnAdminURL(t *testing.T) {
	parallel(t)
	p := onlineSettingsPlugin(map[string]any{"instance": "https://192.168.1.1/"})
	p.manifest.Settings.Fields[1].Default = []byte(`"https://192.168.1.1/"`)
	g := &guestOnlineSourceProvider{p: p, settings: pluginapi.Settings{Enabled: true, URL: "https://api.example.test/"}}
	got := g.current()
	if got.URLEntered || got.URL != "https://api.example.test/" || len(addrsOf(got)) != 0 {
		t.Fatalf("a default was read as entered: %+v", got)
	}
	if (&Set{plugins: []*Plugin{p}}).MediaHostAllowed("tube", "192.168.1.1") {
		t.Fatal("a manifest default became a media host")
	}
}

// TestOnlyTheFirstDeclaredURLFieldIsTheAdminURL: a later url field (a webhook, say)
// never stands in for an unset first one.
func TestOnlyTheFirstDeclaredURLFieldIsTheAdminURL(t *testing.T) {
	parallel(t)
	p := onlineSettingsPlugin(map[string]any{"webhook": "https://hooks.example/"})
	p.manifest.Settings.Fields = append(p.manifest.Settings.Fields,
		pluginapi.SettingsField{Key: "webhook", Type: pluginapi.FieldURL, Label: "Webhook"})
	if u := p.enteredURL(); u != "" {
		t.Fatalf("entered URL = %q, want none", u)
	}
	if (&Set{plugins: []*Plugin{p}}).MediaHostAllowed("tube", "hooks.example") {
		t.Fatal("the second url field became a media host")
	}
}
