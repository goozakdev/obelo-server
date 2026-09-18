package signing_test

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins/signing"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// Unit tests for the signing primitive (.scratch/plugin-system issue 15).
//
// The point of this file is the SPECIFICATION, not the happy path. An author in
// another language reimplements Message() and nothing else, so what is worth
// pinning is what exactly goes into it and what exactly comes back out — and the
// four ways a verification can fail, because the server turns each of them into a
// different sentence for an operator and a package that collapsed them would make
// those sentences lies.

var (
	manifest = []byte(`{"id":"discord","name":"Discord","apiVersion":1}`)
	module   = []byte("\x00asm\x01\x00\x00\x00 and then some bytes")
)

func mustKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := signing.GenerateKey()
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	return pub, priv
}

// TestTheSignedMessageIsExactlyTheSpecification. This is the whole contract with
// an implementation in another language: a domain separator, then two raw
// SHA-256 digests, concatenated, and nothing else — no length prefixes, no
// separators, no canonicalisation of the manifest, because the manifest bytes ARE
// the object.
func TestTheSignedMessageIsExactlyTheSpecification(t *testing.T) {
	manifestSum := sha256.Sum256(manifest)
	moduleSum := sha256.Sum256(module)

	want := append([]byte(pluginapi.SignatureDomain), append(manifestSum[:], moduleSum[:]...)...)
	got := signing.Message(manifest, module)

	if string(got) != string(want) {
		t.Fatalf("the signed message is not domain||sha256(manifest)||sha256(module)")
	}
	if len(got) != len(pluginapi.SignatureDomain)+64 {
		t.Fatalf("the signed message is %d bytes, want %d", len(got), len(pluginapi.SignatureDomain)+64)
	}
	// The separator ends in a newline so it can never run into the digest that
	// follows it.
	if !strings.HasSuffix(pluginapi.SignatureDomain, "\n") {
		t.Fatal("the domain separator must end in a newline, or it runs into the first digest")
	}
}

// TestADomainSeparatedMessageIsNotABareDigestPair: the separator is part of the
// message, so a signature over the bare digests does not verify. That is the
// property the separator exists for — these 64 bytes can never be mistaken for
// any other thing a publisher's key is asked to sign.
func TestADomainSeparatedMessageIsNotABareDigestPair(t *testing.T) {
	pub, priv := mustKey(t)
	manifestSum := sha256.Sum256(manifest)
	moduleSum := sha256.Sum256(module)
	bare := append(manifestSum[:], moduleSum[:]...)

	if ed25519.Verify(pub, signing.Message(manifest, module), ed25519.Sign(priv, bare)) {
		t.Fatal("a signature over the bare digest pair verified as a plugin signature")
	}
}

// TestSignAndVerifyRoundTrip, through the encoded document — which is what a
// server actually holds: bytes fetched from beside a manifest.
func TestSignAndVerifyRoundTrip(t *testing.T) {
	pub, priv := mustKey(t)

	sig, err := signing.Sign(priv, "Example Publisher", manifest, module)
	if err != nil {
		t.Fatalf("signing: %v", err)
	}
	doc, err := signing.Encode(sig)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	parsed, err := signing.Parse(doc)
	if err != nil {
		t.Fatalf("parsing the document this package just wrote: %v", err)
	}
	if err := signing.Verify(parsed, pub, manifest, module); err != nil {
		t.Fatalf("a freshly signed plugin did not verify: %v", err)
	}

	if parsed.Algorithm != pluginapi.SignatureAlgorithmEd25519 {
		t.Fatalf("algorithm = %q, want %q", parsed.Algorithm, pluginapi.SignatureAlgorithmEd25519)
	}
	if parsed.KeyID != signing.KeyID(pub) {
		t.Fatalf("keyId = %q, want the signing key's fingerprint %q", parsed.KeyID, signing.KeyID(pub))
	}
	// The digests in the document are the ones a reader can check by hand.
	wantManifest := sha256.Sum256(manifest)
	if parsed.ManifestSHA256 != hex.EncodeToString(wantManifest[:]) {
		t.Fatalf("manifestSha256 = %q, want the manifest's own digest", parsed.ManifestSHA256)
	}
}

// TestADigestMismatchIsItsOwnFailure. "These files do not go together" and "this
// was not signed by who it says" are different things to tell an operator, and
// the server writes a different sentence for each — so the package has to tell
// them apart rather than answering "invalid signature" twice.
func TestADigestMismatchIsItsOwnFailure(t *testing.T) {
	pub, priv := mustKey(t)
	sig, err := signing.Sign(priv, "Example Publisher", manifest, module)
	if err != nil {
		t.Fatalf("signing: %v", err)
	}

	for _, tc := range []struct {
		name             string
		manifest, module []byte
	}{
		{"the manifest changed after signing", append(manifest, ' '), module},
		{"a different module was shipped", manifest, append(module, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := signing.Verify(sig, pub, tc.manifest, tc.module)
			if !errors.Is(err, signing.ErrDigestMismatch) {
				t.Fatalf("err = %v, want a digest mismatch", err)
			}
		})
	}
}

// TestAnotherKeyDoesNotVerify — the property the whole feature rests on.
func TestAnotherKeyDoesNotVerify(t *testing.T) {
	_, priv := mustKey(t)
	otherPub, _ := mustKey(t)

	sig, err := signing.Sign(priv, "Example Publisher", manifest, module)
	if err != nil {
		t.Fatalf("signing: %v", err)
	}
	if err := signing.Verify(sig, otherPub, manifest, module); !errors.Is(err, signing.ErrBadSignature) {
		t.Fatalf("err = %v, want a bad signature", err)
	}
}

// TestAMalformedDocumentIsRefusedWithASentence. Each of these reaches an operator
// verbatim through the install refusal, so each one has to say something.
func TestAMalformedDocumentIsRefusedWithASentence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		raw    string
		wantIn string
	}{
		{"empty", "", "empty"},
		{"not JSON", "{nope", "not valid JSON"},
		{"no publisher", `{"algorithm":"ed25519","signature":"AA=="}`, "names no publisher"},
		{
			"an algorithm this server does not verify",
			`{"publisher":"X","algorithm":"rsa-pss","signature":"AA=="}`,
			"rsa-pss",
		},
		{"no signature", `{"publisher":"X","algorithm":"ed25519"}`, "carries no signature"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := signing.Parse([]byte(tc.raw))
			if err == nil {
				t.Fatal("a malformed signature document was accepted")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Fatalf("err = %q, want it to mention %q", err, tc.wantIn)
			}
		})
	}
}

// TestASignatureThatIsNotASignatureIsRefusedBeforeEd25519SeesIt: base64 that is
// not base64, and 64 bytes that are not 64 bytes.
func TestASignatureThatIsNotASignatureIsRefused(t *testing.T) {
	pub, priv := mustKey(t)
	good, err := signing.Sign(priv, "Example Publisher", manifest, module)
	if err != nil {
		t.Fatalf("signing: %v", err)
	}

	for _, tc := range []struct {
		name  string
		value string
	}{
		{"not base64", "!!!not base64!!!"},
		{"the wrong length", base64.StdEncoding.EncodeToString([]byte("too short"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := good
			bad.Signature = tc.value
			if err := signing.Verify(bad, pub, manifest, module); !errors.Is(err, signing.ErrBadSignature) {
				t.Fatalf("err = %v, want a bad signature", err)
			}
		})
	}
}

// TestBothPrivateKeySpellingsAreAccepted. Another tool's key file is usually the
// 32-byte seed; refusing it would send an operator looking for a converter for no
// reason, and the two spellings are the same key.
func TestBothPrivateKeySpellingsAreAccepted(t *testing.T) {
	pub, priv := mustKey(t)

	expanded, err := signing.ParsePrivateKey(signing.EncodeKey(priv))
	if err != nil {
		t.Fatalf("reading the expanded key: %v", err)
	}
	seeded, err := signing.ParsePrivateKey(signing.EncodeKey(priv.Seed()))
	if err != nil {
		t.Fatalf("reading the seed: %v", err)
	}
	for name, key := range map[string]ed25519.PrivateKey{"expanded": expanded, "seed": seeded} {
		sig, err := signing.Sign(key, "Example Publisher", manifest, module)
		if err != nil {
			t.Fatalf("%s: signing: %v", name, err)
		}
		if err := signing.Verify(sig, pub, manifest, module); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// TestAPublicKeyIsCheckedByLengthNotByLabel, because an Admin pastes this string
// into a form and every way it can be wrong wants its own sentence.
func TestAPublicKeyIsCheckedByLength(t *testing.T) {
	for _, tc := range []struct {
		name, value, wantIn string
	}{
		{"empty", "", "not valid base64"},
		{"not base64", "!!!", "not valid base64"},
		{"too short", base64.StdEncoding.EncodeToString([]byte("nope")), "32 bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := signing.ParsePublicKey(tc.value)
			if err == nil {
				t.Fatal("a value that is not an ed25519 public key was accepted")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Fatalf("err = %q, want it to mention %q", err, tc.wantIn)
			}
		})
	}
	// Whitespace a key picks up from a terminal or a clipboard is not an error.
	pub, _ := mustKey(t)
	if _, err := signing.ParsePublicKey("  " + signing.EncodeKey(pub) + "\n"); err != nil {
		t.Fatalf("a key with surrounding whitespace was refused: %v", err)
	}
}

// TestSigningNeedsAPublisherAndBothArtifacts: a signature with no publisher names
// nothing a server could look a key up by, and one over half a plugin covers half
// a plugin.
func TestSigningNeedsAPublisherAndBothArtifacts(t *testing.T) {
	_, priv := mustKey(t)

	if _, err := signing.Sign(priv, "  ", manifest, module); err == nil {
		t.Fatal("a signature with no publisher was produced")
	}
	if _, err := signing.Sign(priv, "Example Publisher", manifest, nil); err == nil {
		t.Fatal("a signature over a missing module was produced")
	}
}
