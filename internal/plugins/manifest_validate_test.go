package plugins

import (
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

func validManifestForTest() pluginapi.Manifest {
	return pluginapi.Manifest{
		APIVersion: pluginapi.APIVersion,
		ID:         "demo",
		Name:       "Demo",
		Provides:   []pluginapi.ManifestProvides{{Kind: pluginapi.ExtensionLyricProvider}},
	}
}

func TestValidateManifestRefusesNonBareNetworkHosts(t *testing.T) {
	for _, h := range []string{
		"api.example.com:443", "https://api.example.com", "api.example.com/path",
		"user@api.example.com", "api example.com", "api.example.com?x=1", "api.example.com#f", "[::1]:80",
		`api.example.com\path`, "api%2eexample.com", "api.example.com%2fx",
	} {
		m := validManifestForTest()
		m.Network.Hosts = []string{h}
		if err := validateManifest(m); err == nil {
			t.Errorf("host %q was accepted", h)
		}
	}
	for _, h := range []string{"api.example.com", "::1", "127.0.0.1"} {
		m := validManifestForTest()
		m.Network.Hosts = []string{h}
		if err := validateManifest(m); err != nil {
			t.Errorf("host %q refused: %v", h, err)
		}
	}
}

func TestValidateManifestRefusesDuplicateKind(t *testing.T) {
	m := validManifestForTest()
	m.Provides = append(m.Provides, pluginapi.ManifestProvides{Kind: pluginapi.ExtensionLyricProvider})
	if err := validateManifest(m); err == nil {
		t.Fatal("a manifest with two lyric-provider entries was accepted")
	}
}
