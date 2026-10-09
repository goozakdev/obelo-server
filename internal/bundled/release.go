package bundled

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/plugins/signing"
	"github.com/goozakdev/obelo-server/internal/store"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The Obelo release key (ADR-0069 Q8, issue 02).
//
// Official builds sign every Bundled plugin with a private key that exists only in
// the release pipeline (OBELO_RELEASE_SIGNING_KEY, a CI secret; see the Makefile and
// docker/Dockerfile) and compile the matching PUBLIC key in below, the way
// kAppEncKey is injected: empty in source, set with -ldflags -X. The private key is
// never in the tree, the binary or an image layer.
//
// EMPTY MEANS A DEV BUILD. Nothing is verified, the Bundled plugins install unsigned
// exactly as they always did, and they carry no recorded key, the same as any
// unsigned plugin. NON-EMPTY MEANS A RELEASE BUILD: a Bundled module whose signature
// is missing or does not verify under this key is refused at boot, not installed
// unsigned, and a verified one records this key on its row like any signed install.
var releasePublicKey string

// ReleaseSigner is the publisher name the release pipeline signs Bundled plugins as.
const ReleaseSigner = "Obelo"

// signatureSuffix is the embedded detached signature beside a module and manifest.
const signatureSuffix = ".sig.json"

// errReleaseKeyUnreadable marks a release build whose compiled-in key cannot be
// parsed: every Bundled install is then refused rather than waved through, because a
// garbled key is a broken release, not a dev build.
var errReleaseKeyUnreadable = errors.New("the Obelo release public key compiled into this build cannot be read")

// releaseKeyFromBuild is the compiled-in key, nil on a dev build.
func releaseKeyFromBuild() (ed25519.PublicKey, error) {
	if releasePublicKey == "" {
		return nil, nil
	}
	pub, err := signing.ParsePublicKey(releasePublicKey)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errReleaseKeyUnreadable, err)
	}
	return pub, nil
}

// Signature is one bundled plugin's embedded detached signature document, nil when
// the build carries none.
func Signature(id string) []byte {
	if v, ok := supplied.Load(id); ok {
		return v.(pair).signature
	}
	raw, err := modules.ReadFile("modules/" + id + signatureSuffix)
	if err != nil {
		return nil
	}
	return raw
}

// VerifySigned reports why a shipped plugin's embedded signature does not verify
// under pub over its manifest and (decompressed) module, nil when it does. It is the
// question the release guard (cmd/checksigned) asks of every embedded module.
func VerifySigned(id string, pub ed25519.PublicKey) error {
	manifest, err := ManifestBytes(id)
	if err != nil {
		return err
	}
	module, err := Module(id)
	if err != nil {
		return err
	}
	_, err = verifyDocument(Signature(id), pub, manifest, module)
	return err
}

// verifyDocument checks a detached signature document over exactly these bytes
// under pub, returning the parsed document. The document's own publicKey field is
// ignored: the compiled-in key is the only one that counts.
func verifyDocument(doc []byte, pub ed25519.PublicKey, manifest, module []byte) (pluginapi.Signature, error) {
	if len(doc) == 0 {
		return pluginapi.Signature{}, errors.New("it carries no signature")
	}
	sig, err := signing.Parse(doc)
	if err != nil {
		return pluginapi.Signature{}, fmt.Errorf("its signature could not be read: %w", err)
	}
	if err := signing.Verify(sig, pub, manifest, module); err != nil {
		return pluginapi.Signature{}, fmt.Errorf("its signature does not verify under the Obelo release key: %w", err)
	}
	return sig, nil
}

// ReleaseKey is the Obelo release key this build carries, for the plugin Manager's
// upgrade rule: an upload over a bundled-origin row must verify under it. release is
// false on a dev build; it is true with an empty key when a key was compiled in but
// cannot be read, so that nothing can be verified against it.
func (s *Source) ReleaseKey() (name, publicKey string, release bool) {
	if s.keyErr != nil {
		return ReleaseSigner, "", true
	}
	if s.releaseKey == nil {
		return "", "", false
	}
	return ReleaseSigner, signing.EncodeKey(s.releaseKey), true
}

// recordKey writes the Obelo key on a bundled row, as a signed install does.
func (s *Source) recordKey(id string) error {
	return s.store.SetPluginSignerKey(id, ReleaseSigner, signing.EncodeKey(s.releaseKey), signing.KeyID(s.releaseKey))
}

// recordExisting records the Obelo key on a bundled-origin row whose on-disk module
// and manifest verify under the compiled-in key, so a server upgraded from a release
// that recorded nothing ends up with the key and an unchanged shipped version. A row
// that does not verify is left exactly as it is. Failures are logged and never stop
// a boot.
func (s *Source) recordExisting(id string, row store.PluginRow) {
	if s.releaseKey == nil || s.store == nil || row.ID == "" {
		return
	}
	if row.SignerKey == signing.EncodeKey(s.releaseKey) {
		return
	}
	dir := filepath.Join(s.dir, id)
	manifest, err := os.ReadFile(filepath.Join(dir, plugins.ManifestFile))
	if err != nil {
		return
	}
	module, err := os.ReadFile(filepath.Join(dir, moduleFileOf(manifest)))
	if err != nil {
		return
	}
	// The signature that arrived with the installed copy first, then the one this
	// build embeds (a pre-feature copy has no file of its own).
	onDisk, _ := os.ReadFile(filepath.Join(dir, pluginapi.SignatureFile))
	for _, doc := range [][]byte{onDisk, Signature(id)} {
		if len(doc) == 0 {
			continue
		}
		if _, err := verifyDocument(doc, s.releaseKey, manifest, module); err != nil {
			continue
		}
		if err := s.recordKey(id); err != nil {
			s.logf("obelo: the Obelo signing key of the plugin %s could not be recorded: %v", id, err)
		}
		return
	}
}

// moduleFileOf is the module file name a manifest names, the default when it names
// none; the same rule plugins.InstallFiles writes by.
func moduleFileOf(manifestRaw []byte) string {
	var m struct {
		Module string `json:"module"`
	}
	if err := json.Unmarshal(manifestRaw, &m); err != nil || m.Module == "" {
		return plugins.DefaultModuleFile
	}
	return filepath.Base(m.Module)
}
