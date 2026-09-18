package plugins

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/goozakdev/obelo-server/internal/plugins/signing"
	"github.com/goozakdev/obelo-server/internal/store"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The pinned-publisher policy (.scratch/plugin-system issue 15): whether this
// server will run code, and on whose word.
//
// # The whole policy, in two states
//
// NO PUBLISHER PINNED — the shipped state, and the state of every server that
// has not opted in. Nothing is verified. A signature that arrives is stored beside
// the manifest as provenance and is otherwise ignored, and no install is refused
// for any reason this file contains. This is ADR-0001 being honest: the project
// runs no registry, so there is nobody for a default policy to trust.
//
// ONE OR MORE PINNED — every install must carry a signature naming a publisher
// in the table and verifying under THAT publisher's key, over the exact bytes
// being installed. Anything else is refused, by name, with a sentence that says
// which of the four things went wrong. There is deliberately no "warn only"
// middle setting: an operator who pinned a key did it to stop something, and a
// warning does not stop anything.
//
// # Where it runs, and where it deliberately does not
//
// It runs inside Manager.install, between decodeManifest and checkDuplicate —
// the one place where the raw manifest bytes and the module bytes are both in
// hand and NOTHING has been written to disk or to the database yet. A refused
// install therefore leaves exactly as little behind as a bad manifest does.
//
// It does NOT run at boot, and it does not run on enable, disable or re-enable.
// The files under <dataDir>/plugins/<id>/ are the ones this server already
// accepted; re-checking them would mean that unpinning a key, or a publisher
// rotating one, silently disabled plugins an operator had installed and was
// running — a change of mind about future installs turning into an outage of
// present ones. An operator who wants a plugin gone uninstalls it. This is worth
// stating plainly because it is the first question anyone asks about this file.

// The publisher name a signature names is matched case-insensitively, because an
// operator pinning "Example Publisher" and a document saying "example publisher"
// mean the same publisher and the alternative is a refusal with no visible cause.
// The store column is COLLATE NOCASE for the same reason; this function is what
// makes the in-memory half agree with it.
func samePublisher(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// signer is the outcome of the policy: who signed, verified, or nothing at all.
// An empty Publisher means "no verification happened" and is never "unsigned" —
// the two are different and only the first is something this server knows.
type signer struct {
	Publisher string
	KeyID     string
}

// checkSignature applies the policy to one install. It returns what should be
// recorded about the signer, or a *Refusal written for the Admin.
//
// signatureRaw is the detached document that travelled with the plugin, or nil
// when none did. It is NOT parsed unless it matters: a server with nothing pinned
// does not refuse an install because a signature file happened to be malformed,
// because it was not going to read it anyway.
func (m *Manager) checkSignature(man pluginapi.Manifest, manifestRaw, module, signatureRaw []byte) (signer, error) {
	pinned, err := m.pinnedPublishers()
	if err != nil {
		return signer{}, err
	}
	if len(pinned) == 0 {
		// The default policy. Nothing is checked, nothing is recorded, and the
		// signature file (if any) is still stored beside the manifest so that an
		// operator who pins a key later can check it by hand.
		return signer{}, nil
	}

	if len(signatureRaw) == 0 {
		return signer{}, refuse(ReasonSignature,
			"%s carries no signature, and this server only installs plugins signed by a pinned publisher (%s)",
			man.ID, publisherList(pinned))
	}
	sig, err := signing.Parse(signatureRaw)
	if err != nil {
		return signer{}, refuse(ReasonSignature,
			"the signature that came with %s could not be read: %v", man.ID, err)
	}

	var key *store.PluginPublisher
	for i := range pinned {
		if samePublisher(pinned[i].Publisher, sig.Publisher) {
			key = &pinned[i]
			break
		}
	}
	if key == nil {
		// NAME THE PUBLISHER IT CLAIMED. That is the one fact the operator needs:
		// either they meant to pin this publisher and have not, or the plugin is not
		// what they thought it was, and they cannot tell which from "unsigned".
		return signer{}, refuse(ReasonSignature,
			"%s claims to be published by %q, which is not a publisher pinned on this server (%s)",
			man.ID, sig.Publisher, publisherList(pinned))
	}
	pub, err := signing.ParsePublicKey(key.PublicKey)
	if err != nil {
		// The pinned key itself is unusable. That is the operator's own row and not
		// the plugin's fault, so the sentence says so rather than accusing the
		// publisher.
		return signer{}, refuse(ReasonSignature,
			"the key pinned for %q on this server cannot be read (%v), so nothing can be verified against it",
			key.Publisher, err)
	}
	switch err := signing.Verify(sig, pub, manifestRaw, module); {
	case err == nil:
	case errors.Is(err, signing.ErrDigestMismatch):
		return signer{}, refuse(ReasonSignature,
			"%s claims to be published by %q, but its signature covers different files than the ones that arrived — "+
				"the manifest or the module has changed since it was signed",
			man.ID, sig.Publisher)
	default:
		return signer{}, refuse(ReasonSignature,
			"%s claims to be published by %q, but its signature does not verify against that publisher's pinned key",
			man.ID, sig.Publisher)
	}
	return signer{Publisher: key.Publisher, KeyID: key.KeyID}, nil
}

// PinPublisher adds or replaces a publisher's key.
//
// Replacing rather than refusing a duplicate is what a key ROTATION needs, and
// rotation is the only honest reason to pin one publisher twice. The old key stops
// being accepted the moment this returns; an Admin who wants both live pins the
// second under a second name.
//
// Pinning the FIRST key changes this server's policy for every later install, and
// the log line says so, because it is the kind of change somebody should be able
// to find afterwards.
func (m *Manager) PinPublisher(publisher, publicKey string) error {
	name := strings.TrimSpace(publisher)
	if name == "" {
		return refuse(ReasonSignature,
			"a pinned key needs a publisher name, because the name is what a signature is looked up by")
	}
	pub, err := signing.ParsePublicKey(publicKey)
	if err != nil {
		return refuse(ReasonSignature, "that is not an ed25519 public key: %v", err)
	}
	if m.store == nil {
		return nil
	}
	before, err := m.pinnedPublishers()
	if err != nil {
		return err
	}
	if err := m.store.UpsertPluginPublisher(store.PluginPublisher{
		Publisher: name,
		PublicKey: signing.EncodeKey(pub),
		KeyID:     signing.KeyID(pub),
	}); err != nil {
		return err
	}
	if len(before) == 0 {
		m.logf("obelo: %s is the first pinned plugin publisher; this server will now install only signed plugins", name)
	} else {
		m.logf("obelo: the plugin publisher %s was pinned", name)
	}
	return nil
}

// UnpinPublisher removes a pinned key, refusing a name nobody pinned so an Admin
// who mistyped is told rather than reassured.
//
// Removing the LAST key returns this server to its default policy — nothing is
// verified — which is the consequential half of this call and is why it gets its
// own log line.
func (m *Manager) UnpinPublisher(publisher string) error {
	name := strings.TrimSpace(publisher)
	if name == "" || m.store == nil {
		return refuse(ReasonUnknown, "no publisher was named")
	}
	removed, err := m.store.DeletePluginPublisher(name)
	if err != nil {
		return err
	}
	if !removed {
		return refuse(ReasonUnknown, "no key is pinned for the publisher %q", name)
	}
	after, err := m.pinnedPublishers()
	if err != nil {
		return err
	}
	if len(after) == 0 {
		m.logf("obelo: the last pinned plugin publisher was removed; this server no longer checks plugin signatures")
	} else {
		m.logf("obelo: the plugin publisher %s was unpinned", name)
	}
	return nil
}

// pinnedPublishers reads the pinned keys. A Manager with no store has none, which
// is the narrow test's state and the default policy either way.
func (m *Manager) pinnedPublishers() ([]store.PluginPublisher, error) {
	if m.store == nil {
		return nil, nil
	}
	return m.store.PluginPublishers()
}

// publisherList names who this server WOULD accept, so a refusal is actionable
// rather than merely correct.
func publisherList(pinned []store.PluginPublisher) string {
	names := make([]string, 0, len(pinned))
	for _, p := range pinned {
		names = append(names, p.Publisher)
	}
	return strings.Join(names, ", ")
}

// writeSignature stores the detached document beside the manifest it covers.
//
// It is written whenever one arrived, verified or not, and that is deliberate:
// the file is PROVENANCE. An operator who installs today with nothing pinned and
// pins a key next month can check what they already have without re-downloading
// it, and `plugin.sig.json` sitting in the directory is not a claim that anything
// was verified — the plugins row's publisher column is the only thing that says
// that, and it is written only after a successful verification.
func writeSignature(dir string, signatureRaw []byte) error {
	if len(signatureRaw) == 0 {
		return nil
	}
	path := filepath.Join(dir, pluginapi.SignatureFile)
	if err := os.WriteFile(path, signatureRaw, 0o644); err != nil {
		return fmt.Errorf("plugins: writing the signature to %s: %w", path, err)
	}
	return nil
}
