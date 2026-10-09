package bundled

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/plugins/signing"
)

// The release-key tests (ADR-0069, issue 02). Every key here is generated for the
// test and thrown away; no real Obelo key exists in this tree.

const fakeID = "tmdb"

func fakeManifest(version string) []byte {
	return []byte(`{"id":"` + fakeID + `","name":"TMDB","version":"` + version +
		`","apiVersion":1,"provides":[{"kind":"metadata-provider"}]}`)
}

// supplyFake stands a fake shipped tmdb in for the embedded one, signed by priv (or
// unsigned when priv is nil), and restores whatever was there afterwards.
func supplyFake(t *testing.T, version string, priv ed25519.PrivateKey) (manifest, module []byte) {
	t.Helper()
	prev, had := supplied.Load(fakeID)
	t.Cleanup(func() {
		if had {
			supplied.Store(fakeID, prev)
		} else {
			supplied.Delete(fakeID)
		}
	})
	manifest, module = fakeManifest(version), []byte("\x00asm fake module "+version)
	if err := SupplyForTests(fakeID, manifest, module); err != nil {
		t.Fatal(err)
	}
	if priv != nil {
		sig, err := signing.Sign(priv, "Obelo", manifest, module)
		if err != nil {
			t.Fatal(err)
		}
		doc, err := signing.Encode(sig)
		if err != nil {
			t.Fatal(err)
		}
		if err := SupplySignatureForTests(fakeID, doc); err != nil {
			t.Fatal(err)
		}
	}
	return manifest, module
}

func throwawayKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := signing.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func releaseSource(t *testing.T, st *assertStore, pub ed25519.PublicKey) (*Source, string) {
	t.Helper()
	dir := t.TempDir()
	src := NewSource(dir, st, quiet(t))
	src.releaseKey = pub
	return src, filepath.Join(dir, plugins.DirName, fakeID)
}

func TestReleaseBuildRecordsTheObeloKeyOnFirstInstall(t *testing.T) {
	pub, priv := throwawayKey(t)
	supplyFake(t, "2.0.0", priv)
	st := newAssertStore()
	src, _ := releaseSource(t, st, pub)

	mustAssert(t, src, fakeID)

	row, ok := st.row(fakeID)
	if !ok {
		t.Fatal("no row was written")
	}
	if row.SignerName != "Obelo" || row.SignerKey != signing.EncodeKey(pub) || row.SignerKeyID != signing.KeyID(pub) {
		t.Errorf("the Obelo key was not recorded on the row: %+v", row)
	}
}

func TestDevBuildInstallsUnsignedWithNoRecordedKey(t *testing.T) {
	supplyFake(t, "2.0.0", nil)
	st := newAssertStore()
	src, dir := releaseSource(t, st, nil)

	if err := src.Assert(t.Context(), fakeID); err != nil {
		t.Fatalf("a dev build must install with no error: %v", err)
	}
	row, ok := st.row(fakeID)
	if !ok || row.Version != "2.0.0" {
		t.Fatalf("a dev build did not install: %+v", row)
	}
	if row.SignerKey != "" || row.SignerName != "" || row.SignerKeyID != "" {
		t.Errorf("a dev build recorded a key: %+v", row)
	}
	if _, err := os.Stat(filepath.Join(dir, plugins.DefaultModuleFile)); err != nil {
		t.Errorf("the module was not written: %v", err)
	}
}

func TestReleaseBuildRefusesAModuleWithoutAValidSignature(t *testing.T) {
	pub, _ := throwawayKey(t)
	_, other := throwawayKey(t)
	for name, signer := range map[string]ed25519.PrivateKey{
		"unsigned":          nil,
		"signed by another": other,
	} {
		t.Run(name, func(t *testing.T) {
			// A first boot installs nothing at all.
			supplyFake(t, "2.0.0", signer)
			st := newAssertStore()
			src, dir := releaseSource(t, st, pub)
			err := src.Assert(t.Context(), fakeID)
			if err == nil || !strings.Contains(err.Error(), fakeID) {
				t.Fatalf("want a refusal naming %q, got %v", fakeID, err)
			}
			if _, ok := st.row(fakeID); ok {
				t.Error("a row was written for a refused module")
			}
			if _, err := os.Stat(dir); err == nil {
				t.Error("files were written for a refused module")
			}

			// An existing older copy is left exactly as it was.
			st = newAssertStore()
			st.put(fakeID, plugins.OriginBundled, "1.0.0")
			src, dir = releaseSource(t, st, pub)
			mustMkdir(t, dir)
			writeFile(t, filepath.Join(dir, plugins.ManifestFile), string(fakeManifest("1.0.0")))
			writeFile(t, filepath.Join(dir, plugins.DefaultModuleFile), "old module")
			if err := src.Assert(t.Context(), fakeID); err == nil {
				t.Fatal("the replace was not refused")
			}
			if got := readFile(t, filepath.Join(dir, plugins.DefaultModuleFile)); got != "old module" {
				t.Errorf("the existing module was touched: %q", got)
			}
			if got := versionOnDisk(t, dir); got != "1.0.0" {
				t.Errorf("the existing manifest was touched: %q", got)
			}
			if row, _ := st.row(fakeID); row.Version != "1.0.0" || st.updates != 0 {
				t.Errorf("the existing row was touched: %+v", row)
			}
		})
	}
}

func TestReleaseBootRecordsTheKeyOnAPreFeatureBundledRow(t *testing.T) {
	pub, priv := throwawayKey(t)
	manifest, module := supplyFake(t, "2.0.0", priv)
	st := newAssertStore()
	st.put(fakeID, plugins.OriginBundled, "2.0.0") // installed by a release that recorded no key
	src, dir := releaseSource(t, st, pub)
	mustMkdir(t, dir)
	writeFile(t, filepath.Join(dir, plugins.ManifestFile), string(manifest))
	writeFile(t, filepath.Join(dir, plugins.DefaultModuleFile), string(module))

	mustAssert(t, src, fakeID)

	row, _ := st.row(fakeID)
	if row.SignerKey != signing.EncodeKey(pub) || row.SignerName != "Obelo" {
		t.Errorf("the key was not recorded for the existing row: %+v", row)
	}
	if row.Version != "2.0.0" || st.updates != 0 || st.inserts != 0 {
		t.Errorf("the shipped version must be unchanged (version %q, %d updates, %d inserts)", row.Version, st.updates, st.inserts)
	}
}

func TestReleaseBootLeavesARowWhoseBytesDoNotVerify(t *testing.T) {
	pub, priv := throwawayKey(t)
	supplyFake(t, "2.0.0", priv)
	st := newAssertStore()
	st.put(fakeID, plugins.OriginBundled, "2.0.0")
	src, dir := releaseSource(t, st, pub)
	mustMkdir(t, dir)
	writeFile(t, filepath.Join(dir, plugins.ManifestFile), string(fakeManifest("2.0.0")))
	writeFile(t, filepath.Join(dir, plugins.DefaultModuleFile), "tampered bytes")

	mustAssert(t, src, fakeID)

	if row, _ := st.row(fakeID); row.SignerKey != "" {
		t.Errorf("a key was recorded for bytes that do not verify: %+v", row)
	}
}

func TestReplacingAnOlderBundledCopyRecordsTheKey(t *testing.T) {
	pub, priv := throwawayKey(t)
	supplyFake(t, "2.0.0", priv)
	st := newAssertStore()
	st.put(fakeID, plugins.OriginBundled, "1.0.0")
	src, dir := releaseSource(t, st, pub)
	mustMkdir(t, dir)
	writeFile(t, filepath.Join(dir, plugins.ManifestFile), string(fakeManifest("1.0.0")))
	writeFile(t, filepath.Join(dir, plugins.DefaultModuleFile), "old module")

	mustAssert(t, src, fakeID)

	row, _ := st.row(fakeID)
	if row.Version != "2.0.0" || row.SignerKey != signing.EncodeKey(pub) || row.SignerName != "Obelo" {
		t.Errorf("the replace did not record the key: %+v", row)
	}
	if st.insertsOf[fakeID] != 0 {
		t.Error("a replace inserted a row")
	}
}

func TestVerifySignedIsTheReleaseGuardsQuestion(t *testing.T) {
	pub, priv := throwawayKey(t)
	otherPub, _ := throwawayKey(t)
	supplyFake(t, "2.0.0", priv)
	if err := VerifySigned(fakeID, pub); err != nil {
		t.Errorf("a signed module must verify: %v", err)
	}
	if err := VerifySigned(fakeID, otherPub); err == nil {
		t.Error("a module signed by another key must not verify")
	}
	supplyFake(t, "2.0.0", nil)
	if err := VerifySigned(fakeID, pub); err == nil {
		t.Error("an unsigned module must not verify")
	}
}

func TestReplacingUnsignedOverAKeyedRowClearsTheRecordedKey(t *testing.T) {
	pub, _ := throwawayKey(t)
	supplyFake(t, "2.0.0", nil)
	st := newAssertStore()
	st.put(fakeID, plugins.OriginBundled, "1.0.0")
	row := st.rows[fakeID]
	row.SignerName, row.SignerKey, row.SignerKeyID = "Obelo", signing.EncodeKey(pub), signing.KeyID(pub)
	st.rows[fakeID] = row
	src, dir := releaseSource(t, st, nil) // dev build
	mustMkdir(t, dir)
	writeFile(t, filepath.Join(dir, plugins.ManifestFile), string(fakeManifest("1.0.0")))
	writeFile(t, filepath.Join(dir, plugins.DefaultModuleFile), "old module")

	mustAssert(t, src, fakeID)

	got, _ := st.row(fakeID)
	if got.Version != "2.0.0" || got.SignerName != "" || got.SignerKey != "" || got.SignerKeyID != "" {
		t.Errorf("an unsigned replace left a signer on the row: %+v", got)
	}
}

func TestReleaseKeyFromBuildReadsTheInjectedValue(t *testing.T) {
	pub, _ := throwawayKey(t)
	prev := releasePublicKey
	t.Cleanup(func() { releasePublicKey = prev })

	releasePublicKey = ""
	if k, err := releaseKeyFromBuild(); k != nil || err != nil {
		t.Errorf("a dev build must have no key and no error, got %v, %v", k, err)
	}
	releasePublicKey = signing.EncodeKey(pub)
	if k, err := releaseKeyFromBuild(); err != nil || !k.Equal(pub) {
		t.Errorf("the injected key was not read: %v, %v", k, err)
	}
	if src := NewSource(t.TempDir(), newAssertStore(), quiet(t)); !src.releaseKey.Equal(pub) {
		t.Error("NewSource did not pick up the compiled-in key")
	}
}

func TestAGarbledCompiledInKeyRefusesEveryInstall(t *testing.T) {
	_, priv := throwawayKey(t)
	supplyFake(t, "2.0.0", priv)
	prev := releasePublicKey
	t.Cleanup(func() { releasePublicKey = prev })
	releasePublicKey = "not a key"

	st := newAssertStore()
	dir := t.TempDir()
	src := NewSource(dir, st, quiet(t))
	if src.keyErr == nil {
		t.Fatal("a garbled key was accepted")
	}
	if err := src.Assert(t.Context(), fakeID); err == nil {
		t.Fatal("an install went ahead under a garbled release key")
	}
	if _, ok := st.row(fakeID); ok {
		t.Error("a row was written")
	}
	if _, err := os.Stat(filepath.Join(dir, plugins.DirName, fakeID)); err == nil {
		t.Error("files were written")
	}
}
