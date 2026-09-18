package discordtest_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins/discordtest"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The two properties that keep the vendored copy honest, and the manifest
// assertions that are cheap enough to make here rather than through the HTTP API
// (.scratch/plugin-system issue 14).

// TestTheVendoredCopyMatchesTheSibling is the anti-rot guard.
//
// The Discord plugin's home is a sibling repository that a clean clone of this
// one does not have, so a copy of its source lives under testdata and everything
// in this suite can run without it. A copy is a thing that drifts — that is what
// internal/webui/dist/index.html taught this project — so on any machine that
// HAS the sibling checked out, every vendored file is compared with the original
// and a difference is a failure naming the file.
//
// It SKIPS when the sibling is absent, which is a clean clone and CI. That is not
// a hole: the only person who can make the two disagree is the person editing the
// sibling, and they are the person this test runs for.
func TestTheVendoredCopyMatchesTheSibling(t *testing.T) {
	sibling, ok := discordtest.SiblingDir()
	if !ok {
		t.Skipf("no sibling checkout at %s; the vendored copy is what this suite builds", sibling)
	}
	vendored, err := discordtest.VendoredPath()
	if err != nil {
		t.Fatalf("locating the vendored copy: %v", err)
	}

	for _, name := range discordtest.VendoredFiles {
		want, err := os.ReadFile(filepath.Join(sibling, name))
		if err != nil {
			t.Errorf("%s: reading it from the sibling repository: %v", name, err)
			continue
		}
		got, err := os.ReadFile(filepath.Join(vendored, name))
		if err != nil {
			t.Errorf("%s: it is in the sibling repository but not vendored here: %v", name, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s has drifted from %s.\n"+
				"Re-vendor it (copy the sibling's file over %s) and update %s with the sibling's commit.",
				name, filepath.Join(sibling, name), filepath.Join(vendored, name), discordtest.SourceFile)
		}
	}

	// A file added to the sibling and never vendored is the other half of the same
	// failure: this suite would build a module the sibling no longer describes.
	entries, err := os.ReadDir(sibling)
	if err != nil {
		t.Fatalf("reading the sibling repository: %v", err)
	}
	known := map[string]bool{".git": true}
	for _, name := range discordtest.VendoredFiles {
		known[name] = true
	}
	for _, e := range entries {
		name := e.Name()
		if known[name] || name == "plugin.wasm" {
			continue
		}
		t.Errorf("the sibling repository has %q, which is not in discordtest.VendoredFiles; "+
			"vendor it or say why it is excluded", name)
	}
}

// TestTheVendoredCopyRecordsWhereItCameFrom: SOURCE is the provenance note, and a
// copy with no provenance is a copy nobody can check.
func TestTheVendoredCopyRecordsWhereItCameFrom(t *testing.T) {
	vendored, err := discordtest.VendoredPath()
	if err != nil {
		t.Fatalf("locating the vendored copy: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(vendored, discordtest.SourceFile))
	if err != nil {
		t.Fatalf("reading %s: %v", discordtest.SourceFile, err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		t.Fatalf("%s is empty; it must name the sibling repository and the commit this copy was taken from",
			discordtest.SourceFile)
	}
	if !bytes.Contains(raw, []byte(discordtest.SiblingDirName)) {
		t.Errorf("%s does not name %s:\n%s", discordtest.SourceFile, discordtest.SiblingDirName, raw)
	}
}

// TestTheReferencePluginsManifestAllowlistsDiscordAndNothingElse is the
// acceptance criterion read off the file an operator installs. The allowlist is
// the most load-bearing claim in a manifest and the one the host trusts least, so
// the reference plugin had better make the smallest one that works.
func TestTheReferencePluginsManifestAllowlistsDiscordAndNothingElse(t *testing.T) {
	m := discordtest.Manifest(t)

	if got := m.Network.Hosts; len(got) != 1 || got[0] != "discord.com" {
		t.Fatalf("network.hosts = %v, want exactly [discord.com]", got)
	}
	if m.ID != "discord" {
		t.Errorf("id = %q, want discord", m.ID)
	}
	if m.APIVersion != pluginapi.APIVersion {
		t.Errorf("apiVersion = %d, want %d — the reference plugin must be built for the contract this server speaks",
			m.APIVersion, pluginapi.APIVersion)
	}
	if len(m.Provides) != 1 || m.Provides[0].Kind != pluginapi.ExtensionEventSink {
		t.Fatalf("provides = %+v, want one event-sink entry", m.Provides)
	}
	// A sink that signs nothing must not claim to need a signing secret: issue 13
	// made `requiresSecret: false` an honest declaration rather than a way to build
	// a Plugin nobody can switch on, and this is the Plugin that proves it.
	if m.Provides[0].RequiresSecret || m.Settings.RequiresSecret {
		t.Errorf("the Discord sink declares requiresSecret, but its only credential is the webhook URL, "+
			"which is a declared secret FIELD: %+v", m.Settings)
	}

	// The webhook URL is a credential. It must be a `secret` field — which the API
	// never returns — and it must be required, because a sink with no target is a
	// sink that can only fail.
	var webhook *pluginapi.SettingsField
	for i := range m.Settings.Fields {
		if m.Settings.Fields[i].Key == "webhook_url" {
			webhook = &m.Settings.Fields[i]
		}
	}
	if webhook == nil {
		t.Fatalf("the manifest declares no webhook_url field: %+v", m.Settings.Fields)
	}
	if webhook.Type != pluginapi.FieldSecret {
		t.Errorf("webhook_url is declared %q; a Discord webhook URL is a bearer credential and must be %q",
			webhook.Type, pluginapi.FieldSecret)
	}
	if !webhook.Required {
		t.Error("webhook_url is not required, so the plugin can be turned on with nowhere to post")
	}

	// One template per curated event type, so adding an event type to the contract
	// is a visible omission here rather than a silent one.
	declared := map[string]bool{}
	for _, f := range m.Settings.Fields {
		declared[f.Key] = true
	}
	for _, eventType := range pluginapi.AllEventTypes() {
		key := "template_" + replaceDot(eventType)
		if !declared[key] {
			t.Errorf("the manifest declares no %q, so %s would post nothing and say nothing about why",
				key, eventType)
		}
	}
	for _, key := range []string{"mention_role", "role_id"} {
		if !declared[key] {
			t.Errorf("the manifest declares no %q", key)
		}
	}
}

func replaceDot(s string) string {
	out := []rune(s)
	for i, r := range out {
		if r == '.' {
			out[i] = '_'
		}
	}
	return string(out)
}
