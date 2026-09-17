package plugins

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// ManifestFile is the document an author ships beside their module, in the
// Plugin's own directory under <dataDir>/plugins/<id>/.
const ManifestFile = "manifest.json"

// DefaultModuleFile is the module an author ships when their manifest names none.
const DefaultModuleFile = "plugin.wasm"

// readManifest reads and validates one Plugin's manifest.json.
//
// Everything it refuses, it refuses with a sentence an operator can act on,
// because these messages are the whole of what they will see: there is no
// installer yet, they placed these files by hand, and a Plugin that does not
// appear needs to say why. Nothing here ever fails a boot — the caller records
// the message and moves on to the next directory (ADR-0001, ADR-0043: a Plugin
// may never stop a boot).
func readManifest(dir string) (pluginapi.Manifest, error) {
	path := filepath.Join(dir, ManifestFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return pluginapi.Manifest{}, fmt.Errorf("no %s in %s", ManifestFile, filepath.Base(dir))
		}
		return pluginapi.Manifest{}, fmt.Errorf("reading %s: %w", ManifestFile, err)
	}
	var m pluginapi.Manifest
	// Unknown keys are IGNORED, not refused: the contract leaves
	// additionalProperties open on every type precisely so a Plugin written
	// against a later v1.x manifest still loads here (pluginapi/v1 doc.go,
	// "frozen, additive only"). Refusing an unknown key would turn every additive
	// change into a breaking one.
	if err := json.Unmarshal(raw, &m); err != nil {
		return pluginapi.Manifest{}, fmt.Errorf("%s is not valid JSON: %w", ManifestFile, err)
	}
	if err := validateManifest(m); err != nil {
		return pluginapi.Manifest{}, err
	}
	return m, nil
}

// validateManifest checks the claims a manifest is allowed to make about itself.
// It does NOT check the allowlist against anything — that is enforced at every
// fetch, from the file, and never from what a guest says.
func validateManifest(m pluginapi.Manifest) error {
	if err := checkAPIVersion(m.APIVersion); err != nil {
		return err
	}
	if strings.TrimSpace(m.ID) == "" {
		return fmt.Errorf("the manifest has no id, and the id is how settings are stored")
	}
	if m.ID != slugOf(m.ID) {
		return fmt.Errorf("the id %q must be a slug: lowercase letters, digits and dashes", m.ID)
	}
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("the manifest has no name, and the name is what an operator reads")
	}
	if len(m.Provides) == 0 {
		return fmt.Errorf("the manifest provides nothing; a Plugin that fills no Extension point cannot be registered")
	}
	for _, p := range m.Provides {
		switch p.Kind {
		case pluginapi.ExtensionEventSink, pluginapi.ExtensionMetadataProvider, pluginapi.ExtensionSubtitleProvider:
		case "":
			return fmt.Errorf("a provides entry names no kind")
		default:
			return fmt.Errorf("unknown extension point %q; this server knows %s, %s and %s",
				p.Kind, pluginapi.ExtensionMetadataProvider,
				pluginapi.ExtensionSubtitleProvider, pluginapi.ExtensionEventSink)
		}
	}
	// The module is a FILE NAME beside the manifest, never a path. A manifest that
	// could name "../../obelo.db" would be a Plugin reading the database by asking
	// the loader to compile it.
	if mf := moduleFile(m); mf != filepath.Base(mf) || mf == "." || mf == ".." {
		return fmt.Errorf("module %q must be a file name beside the manifest, not a path", m.Module)
	}
	for _, h := range m.Network.Hosts {
		if h != normalizeHost(h) || h == "" {
			return fmt.Errorf("network host %q must be a bare lowercase host name with no scheme, port or path", h)
		}
		if strings.Contains(h, "*") {
			return fmt.Errorf("network host %q uses a wildcard; the allowlist is exact, so an operator can read it and know what this code may reach", h)
		}
	}
	return nil
}

// checkAPIVersion is ADR-0058 decision 8 and the ADR-0055 posture in one function:
// a mismatch is refused BEFORE the module is compiled, and the message names which
// side has to move. "Incompatible" is useless to the person reading it; "upgrade
// the server" is an instruction.
func checkAPIVersion(declared int) error {
	switch {
	case declared == pluginapi.APIVersion:
		return nil
	case declared <= 0:
		return fmt.Errorf("the manifest declares no apiVersion; this server speaks plugin API v%d, so the plugin must declare apiVersion %d — upgrade the plugin",
			pluginapi.APIVersion, pluginapi.APIVersion)
	case declared > pluginapi.APIVersion:
		return fmt.Errorf("this server speaks plugin API v%d; the plugin needs v%d — upgrade the server",
			pluginapi.APIVersion, declared)
	default:
		return fmt.Errorf("this server speaks plugin API v%d; the plugin was built for v%d — upgrade the plugin",
			pluginapi.APIVersion, declared)
	}
}

// slugOf is the shape an id must already have. It does not transform an id into a
// slug — a Plugin whose id is not one is refused rather than quietly renamed,
// because the id is the directory on disk AND the key its settings row is written
// under, and a host that renamed it would orphan the row.
func slugOf(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// normalizeHost is how both sides of the allowlist comparison are spelled: lower
// case, no port, no trailing dot, no brackets around an IPv6 literal.
func normalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.TrimSuffix(h, ".")
	h = strings.TrimPrefix(h, "[")
	h = strings.TrimSuffix(h, "]")
	return h
}

// descriptorFor turns one provides entry into the Descriptor this Plugin
// registers with. An Installed plugin reaches the Registry as exactly the kind of
// value a Built-in does, which is what makes the settings API, the screen, the
// sink Manager and the translator work on it with no change at all.
func descriptorFor(m pluginapi.Manifest, p pluginapi.ManifestProvides) pluginapi.Descriptor {
	return pluginapi.Descriptor{
		Slug:         m.ID,
		Name:         m.Name,
		Kinds:        p.Kinds,
		Role:         p.Role,
		Class:        p.Class,
		RequiresKey:  p.RequiresSecret || m.Settings.RequiresSecret,
		Capabilities: p.Capabilities,
		DefaultURL:   m.Settings.DefaultURL,
		DefaultURL2:  m.Settings.DefaultURL2,
		Description:  m.Description,
		DocsURL:      m.DocsURL,
	}
	// ExtensionPoint is set by the Registry's Register call, exactly as it is for
	// a Built-in, so it can never disagree with the seam it was registered into.
}

// moduleFile is the module beside a manifest: what the manifest named, or the
// convention.
func moduleFile(m pluginapi.Manifest) string {
	if strings.TrimSpace(m.Module) == "" {
		return DefaultModuleFile
	}
	return m.Module
}
