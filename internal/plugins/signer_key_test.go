package plugins_test

import (
	"context"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/plugins/signing"
	"github.com/goozakdev/obelo-server/internal/store"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// Trust on first install (ADR-0069, Q2b): every signed install records the signer's
// public key, on every server. The record is SEPARATE from the pinned-verification
// publisher/key_id columns, which these tests also hold to their old meaning.

func rowOf(t *testing.T, f *managerFixture, id string) store.PluginRow {
	t.Helper()
	rows, err := f.store.Plugins()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no row for %s", id)
	return store.PluginRow{}
}

// signedArchive packs a sink plugin signed by priv, letting edit change the document
// before it is encoded.
func signedArchive(t *testing.T, id string, priv []byte, edit func(*pluginapi.Signature)) []byte {
	t.Helper()
	manifest := plugintest.ManifestJSON(t, plugintest.SinkManifest(id))
	module := plugintest.Guest(t)
	sig, err := signing.Sign(priv, "Example Publisher", manifest, module)
	if err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(&sig)
	}
	doc, err := signing.Encode(sig)
	if err != nil {
		t.Fatal(err)
	}
	return plugintest.PackageZip(t, manifest, module, doc)
}

func TestASignedInstallWithNothingPinnedRecordsTheSignersKey(t *testing.T) {
	t.Parallel()
	f := newManagerFixture(t)
	pub, priv, err := signing.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.manager.InstallPackage(context.Background(), signedArchive(t, "tofu-sink", priv, nil), plugins.SourceUpload)
	if err != nil {
		t.Fatal(err)
	}
	row := rowOf(t, f, "tofu-sink")
	if row.SignerKey != signing.EncodeKey(pub) || row.SignerName != "Example Publisher" || row.SignerKeyID != signing.KeyID(pub) {
		t.Fatalf("recorded %q / %q / %q, want Example Publisher, the signing key and its id", row.SignerName, row.SignerKey, row.SignerKeyID)
	}
	// The pinned-verification columns keep their meaning: nothing was pinned, so nothing is verified.
	if row.Publisher != "" || row.KeyID != "" || got.Publisher != "" {
		t.Fatalf("pinned columns = %q / %q (view %q), want empty with nothing pinned", row.Publisher, row.KeyID, got.Publisher)
	}
}

func TestAPinnedInstallRecordsThePinnedKey(t *testing.T) {
	t.Parallel()
	f := newManagerFixture(t)
	pub, priv, err := signing.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.manager.PinPublisher("example publisher", signing.EncodeKey(pub)); err != nil {
		t.Fatal(err)
	}
	// A document with no embedded key still installs under a pinned key.
	archive := signedArchive(t, "pinned-sink", priv, func(s *pluginapi.Signature) { s.PublicKey = "" })
	if _, err := f.manager.InstallPackage(context.Background(), archive, plugins.SourceUpload); err != nil {
		t.Fatal(err)
	}
	row := rowOf(t, f, "pinned-sink")
	if row.SignerKey != signing.EncodeKey(pub) || row.SignerName != "example publisher" || row.SignerKeyID != signing.KeyID(pub) {
		t.Fatalf("recorded %q / %q / %q, want the pinned key", row.SignerName, row.SignerKey, row.SignerKeyID)
	}
	if row.Publisher != "example publisher" {
		t.Fatalf("pinned publisher column = %q, want it kept as before", row.Publisher)
	}
}

func TestADocumentWithoutAKeyInstallsAndRecordsNothingOnAnUnpinnedServer(t *testing.T) {
	t.Parallel()
	f := newManagerFixture(t)
	_, priv, _ := signing.GenerateKey()
	archive := signedArchive(t, "nokey-sink", priv, func(s *pluginapi.Signature) { s.PublicKey = "" })
	if _, err := f.manager.InstallPackage(context.Background(), archive, plugins.SourceUpload); err != nil {
		t.Fatal(err)
	}
	if row := rowOf(t, f, "nokey-sink"); row.SignerKey != "" || row.SignerName != "" || row.SignerKeyID != "" {
		t.Fatalf("recorded %+v from a document with no key", row)
	}
}

func TestAnEmbeddedKeyThatDoesNotVerifyRecordsNothing(t *testing.T) {
	t.Parallel()
	f := newManagerFixture(t)
	_, priv, _ := signing.GenerateKey()
	otherPub, _, _ := signing.GenerateKey()
	archive := signedArchive(t, "badkey-sink", priv, func(s *pluginapi.Signature) { s.PublicKey = signing.EncodeKey(otherPub) })
	if _, err := f.manager.InstallPackage(context.Background(), archive, plugins.SourceUpload); err != nil {
		t.Fatalf("an unverifiable signature must not refuse on an unpinned server: %v", err)
	}
	if row := rowOf(t, f, "badkey-sink"); row.SignerKey != "" || row.SignerName != "" || row.SignerKeyID != "" {
		t.Fatalf("recorded %+v for a key that does not verify", row)
	}
}

func TestAnUnsignedInstallRecordsNoKey(t *testing.T) {
	t.Parallel()
	f := newManagerFixture(t)
	archive := plugintest.PackageZip(t, plugintest.ManifestJSON(t, plugintest.SinkManifest("plain-sink")), plugintest.Guest(t), nil)
	if _, err := f.manager.InstallPackage(context.Background(), archive, plugins.SourceUpload); err != nil {
		t.Fatal(err)
	}
	if row := rowOf(t, f, "plain-sink"); row.SignerKey != "" || row.SignerName != "" || row.SignerKeyID != "" {
		t.Fatalf("recorded %+v for an unsigned install", row)
	}
}

func TestAPinnedInstallIgnoresADifferentKeyEmbeddedInTheDocument(t *testing.T) {
	t.Parallel()
	f := newManagerFixture(t)
	pub, priv, _ := signing.GenerateKey()
	otherPub, _, _ := signing.GenerateKey()
	if err := f.manager.PinPublisher("Example Publisher", signing.EncodeKey(pub)); err != nil {
		t.Fatal(err)
	}
	archive := signedArchive(t, "pinned-other", priv, func(s *pluginapi.Signature) { s.PublicKey = signing.EncodeKey(otherPub) })
	if _, err := f.manager.InstallPackage(context.Background(), archive, plugins.SourceUpload); err != nil {
		t.Fatal(err)
	}
	if row := rowOf(t, f, "pinned-other"); row.SignerKey != signing.EncodeKey(pub) || row.SignerKeyID != signing.KeyID(pub) {
		t.Fatalf("recorded key %q, want the pinned key %q, not the document's", row.SignerKey, signing.EncodeKey(pub))
	}
}

func TestAnIconCoveredByTheSignatureIsRecordedAndASwappedIconIsNot(t *testing.T) {
	t.Parallel()
	f := newManagerFixture(t)
	pub, priv, _ := signing.GenerateKey()
	manifest, module := packageParts(t)
	icon := iconPNG(t, 32, 32)
	sig, err := signing.SignWithIcon(priv, "Example Publisher", manifest, module, icon)
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := signing.Encode(sig)
	ctx := context.Background()

	got, err := f.manager.InstallPackage(ctx, iconPackage(t, manifest, module, icon, doc), plugins.SourceUpload)
	if err != nil {
		t.Fatal(err)
	}
	if row := rowOf(t, f, got.ID); row.SignerKey != signing.EncodeKey(pub) {
		t.Fatalf("a correctly signed icon package recorded %q, want the signing key", row.SignerKey)
	}
	if err := f.manager.Uninstall(ctx, got.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := f.manager.InstallPackage(ctx, iconPackage(t, manifest, module, iconPNG(t, 16, 16), doc), plugins.SourceUpload); err != nil {
		t.Fatalf("an unpinned server must still install a swapped-icon package: %v", err)
	}
	if row := rowOf(t, f, got.ID); row.SignerKey != "" || row.SignerName != "" {
		t.Fatalf("a swapped icon recorded %+v, want nothing", row)
	}
}

func TestATamperedModuleRecordsNothingAndStillInstallsUnpinned(t *testing.T) {
	t.Parallel()
	f := newManagerFixture(t)
	_, priv, _ := signing.GenerateKey()
	manifest := plugintest.ManifestJSON(t, plugintest.SinkManifest("tamper-sink"))
	module := plugintest.Guest(t)
	sig, err := signing.Sign(priv, "Example Publisher", manifest, module)
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := signing.Encode(sig)
	// A custom section right after the header is valid WebAssembly that changes the module's bytes.
	tampered := append(append(append([]byte{}, module[:8]...), 0x00, 0x03, 0x02, 'x', 'y'), module[8:]...)
	if _, err := f.manager.InstallPackage(context.Background(), plugintest.PackageZip(t, manifest, tampered, doc), plugins.SourceUpload); err != nil {
		t.Fatalf("an unpinned server must still install it: %v", err)
	}
	if row := rowOf(t, f, "tamper-sink"); row.SignerKey != "" || row.SignerName != "" {
		t.Fatalf("a tampered module recorded %+v, want nothing", row)
	}
}
