// Package signing is the one implementation of what a plugin signature means
// (.scratch/plugin-system issue 15): how the signed message is built, how a
// document is produced over it, and how one is checked.
//
// # One package, two callers, no second opinion
//
// The server verifies with it and `cmd/pluginsign` signs with it. That is not
// tidiness — it is the only thing that makes "the tool's output verifies in the
// server" a property rather than a coincidence. A signing tool with its own idea
// of the message would produce documents that fail on somebody else's server, and
// the failure would look like a forgery.
//
// The server never imports `cmd/`, so the shared code lives here and the command
// is a thin argument parser over it.
//
// # The signed message
//
//	pluginapi.SignatureDomain || sha256(manifest bytes) || sha256(module bytes)
//
// 16 bytes of domain separator and two raw 32-byte digests, concatenated, signed
// with ed25519. There is no canonicalisation step and there is nothing to agree
// on beyond this sentence, because the manifest bytes ARE the signed object: an
// install writes them to disk byte for byte and never re-encodes them, so what
// was signed and what is stored are the same bytes forever.
//
// A package that carries an optional icon.png (ADR-0068) appends a third raw
// digest, sha256(icon bytes). A package with no icon signs exactly the message
// above, so every signature made before icons existed still verifies.
//
// This is also why the signature is DETACHED. A signature field inside the
// manifest would change the manifest, which would change what the signature
// covers. See pluginapi/v1/signature.go.
//
// # What this package does NOT do
//
// It does not decide whether to trust a key. Verify is handed a public key by its
// caller and answers one question — do these bytes carry this key's signature —
// and the policy about WHICH key that may be is the host's (internal/plugins),
// because the answer is "one an Admin pinned by hand, and nothing else"
// (ADR-0001). There is no root, no chain, no revocation and no registry here, and
// adding one would be a decision for an ADR rather than a function.
//
// It is also not `cmd/keytool`, which is AES-GCM sealing for the maintainer's key
// rotation (ADR-0032) — a different primitive answering a different question with
// a different key format. Reusing it would have made one piece of key material
// mean two things.
package signing

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// MaxSignatureBytes caps a signature document. It is a few hundred bytes of JSON
// with two hex digests and one base64 signature in it; anything near this is not
// one, and the cap is here so the signature member of a package can be
// bounded like the other two.
const MaxSignatureBytes = 8 << 10

// Message is the exact bytes an ed25519 signature covers. It is exported because
// it is the whole specification: an implementation in another language reproduces
// this function and nothing else.
func Message(manifest, module []byte) []byte {
	return MessageWithIcon(manifest, module, nil)
}

// MessageWithIcon is Message for a package that may carry an icon.png (ADR-0068).
// With no icon it is exactly Message, byte for byte, so every signature made before
// icons existed still verifies; with one, the icon's raw 32-byte digest is appended,
// which makes it a different (112-byte) message, so an icon cannot be added, swapped
// or removed from a signed package without breaking the signature.
func MessageWithIcon(manifest, module, icon []byte) []byte {
	return messageOf(sha256.Sum256(manifest), sha256.Sum256(module), icon)
}

// messageOf is MessageWithIcon over digests already computed, so Verify hashes a
// module of up to 64 MiB once rather than twice.
func messageOf(manifestSum, moduleSum [sha256.Size]byte, icon []byte) []byte {
	msg := make([]byte, 0, len(pluginapi.SignatureDomain)+len(manifestSum)+len(moduleSum)+sha256.Size)
	msg = append(msg, pluginapi.SignatureDomain...)
	msg = append(msg, manifestSum[:]...)
	msg = append(msg, moduleSum[:]...)
	if len(icon) > 0 {
		iconSum := sha256.Sum256(icon)
		msg = append(msg, iconSum[:]...)
	}
	return msg
}

// Digest is the lower-case hex SHA-256 of one artifact, the spelling the document
// carries so a reader can see which bytes a signature claims to cover.
func Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// KeyID is the short fingerprint of a public key that a document carries and a
// screen shows: the first eight bytes of the key's SHA-256, in lower-case hex.
//
// IT DECIDES NOTHING. Verification uses the pinned key's own bytes, so a document
// whose keyId is wrong, stale or absent still verifies or fails on the signature
// alone. It exists so that a human comparing what their server has pinned against
// what a publisher advertises has something short enough to compare, which a
// 44-character base64 key is not.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// EncodeKey is the on-the-wire spelling of a key: standard base64 with padding.
// Both halves use it — a pinned public key and a private key file — so an
// operator never has to know which encoding a given file is in.
func EncodeKey(key []byte) string { return base64.StdEncoding.EncodeToString(key) }

// GenerateKey makes a publisher's key pair. The private key is the caller's to
// protect: nothing in this repository stores one, and the server never sees one.
func GenerateKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("signing: generating a key: %w", err)
	}
	return pub, priv, nil
}

// ParsePublicKey reads a base64 public key, refusing anything that is not an
// ed25519 key by LENGTH rather than by trusting the label on it. An Admin pastes
// this string into a form, so every way it can be wrong gets its own sentence.
func ParsePublicKey(encoded string) (ed25519.PublicKey, error) {
	raw, err := decodeKeyMaterial(encoded)
	if err != nil {
		return nil, fmt.Errorf("the public key is not valid base64: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("an ed25519 public key is %d bytes and this one is %d",
			ed25519.PublicKeySize, len(raw))
	}
	return ed25519.PublicKey(raw), nil
}

// ParsePrivateKey reads a base64 private key, accepting both spellings ed25519
// has: the 64-byte expanded form the standard library produces, and the 32-byte
// seed. A key file written by another tool is usually the seed, and refusing it
// would send an operator to look for a converter.
func ParsePrivateKey(encoded string) (ed25519.PrivateKey, error) {
	raw, err := decodeKeyMaterial(encoded)
	if err != nil {
		return nil, fmt.Errorf("the private key is not valid base64: %w", err)
	}
	switch len(raw) {
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(raw), nil
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(raw), nil
	default:
		return nil, fmt.Errorf("an ed25519 private key is %d or %d bytes and this one is %d",
			ed25519.SeedSize, ed25519.PrivateKeySize, len(raw))
	}
}

// decodeKeyMaterial tolerates the whitespace a key that has been through a
// terminal, a text file or a clipboard picks up, and both base64 alphabets'
// padding conventions.
func decodeKeyMaterial(encoded string) ([]byte, error) {
	s := strings.Join(strings.Fields(encoded), "")
	if s == "" {
		return nil, errors.New("it is empty")
	}
	if raw, err := base64.StdEncoding.DecodeString(s); err == nil {
		return raw, nil
	}
	return base64.RawStdEncoding.DecodeString(s)
}

// Sign produces the detached document for one manifest and one module.
//
// The publisher name is the caller's to choose and is signed only in the sense
// that changing it changes the document, not the message — which is exactly right
// and worth stating: a server does not verify that the name is true, it verifies
// that the bytes were signed by the key IT HAS PINNED AGAINST THAT NAME. The name
// is the lookup, the key is the proof.
func Sign(priv ed25519.PrivateKey, publisher string, manifest, module []byte) (pluginapi.Signature, error) {
	return SignWithIcon(priv, publisher, manifest, module, nil)
}

// SignWithIcon is Sign over a package that may carry an icon.png: a non-empty icon
// is covered by the signature, a nil one gives exactly the signature Sign does.
func SignWithIcon(priv ed25519.PrivateKey, publisher string, manifest, module, icon []byte) (pluginapi.Signature, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return pluginapi.Signature{}, fmt.Errorf("signing: the private key is %d bytes, not %d",
			len(priv), ed25519.PrivateKeySize)
	}
	if strings.TrimSpace(publisher) == "" {
		return pluginapi.Signature{}, errors.New("signing: a signature needs a publisher name, because that is what a server pins a key against")
	}
	if len(manifest) == 0 || len(module) == 0 {
		return pluginapi.Signature{}, errors.New("signing: a signature covers a manifest AND a module, and one of them is empty")
	}
	sig := ed25519.Sign(priv, MessageWithIcon(manifest, module, icon))
	pub := priv.Public().(ed25519.PublicKey)
	return pluginapi.Signature{
		Publisher:      strings.TrimSpace(publisher),
		KeyID:          KeyID(pub),
		PublicKey:      EncodeKey(pub),
		Algorithm:      pluginapi.SignatureAlgorithmEd25519,
		ManifestSHA256: Digest(manifest),
		ModuleSHA256:   Digest(module),
		Signature:      base64.StdEncoding.EncodeToString(sig),
	}, nil
}

// Encode writes a signature document as the file that travels in a package beside the manifest:
// indented, with a trailing newline, because a human reads it and a text editor
// should not be the first thing to touch it.
func Encode(sig pluginapi.Signature) ([]byte, error) {
	raw, err := json.MarshalIndent(sig, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("signing: encoding the signature: %w", err)
	}
	return append(raw, '\n'), nil
}

// Parse reads a signature document, refusing a malformed one with a sentence
// rather than a zero value. It checks only that the document IS one — the
// algorithm it names, and that the fields a verifier needs are present — and
// judges nothing about whether it is to be believed.
func Parse(raw []byte) (pluginapi.Signature, error) {
	if len(raw) == 0 {
		return pluginapi.Signature{}, errors.New("the signature document is empty")
	}
	var sig pluginapi.Signature
	if err := json.Unmarshal(raw, &sig); err != nil {
		return pluginapi.Signature{}, fmt.Errorf("%s is not valid JSON: %w", pluginapi.SignatureFile, err)
	}
	if strings.TrimSpace(sig.Publisher) == "" {
		return pluginapi.Signature{}, errors.New("the signature names no publisher, so there is no pinned key it could be checked against")
	}
	if sig.Algorithm != pluginapi.SignatureAlgorithmEd25519 {
		return pluginapi.Signature{}, fmt.Errorf("the signature names the algorithm %q, and this server verifies only %s",
			sig.Algorithm, pluginapi.SignatureAlgorithmEd25519)
	}
	if sig.Signature == "" {
		return pluginapi.Signature{}, errors.New("the signature document carries no signature")
	}
	return sig, nil
}

// ErrDigestMismatch is a signature that is well formed and covers OTHER BYTES
// than the ones in hand — the wrong module, or a manifest edited after signing.
// It is its own error because it is a different thing to tell an operator from a
// signature that does not verify: one says "these files do not go together", the
// other says "this was not signed by who it says".
var ErrDigestMismatch = errors.New("signing: the signature covers different bytes")

// ErrBadSignature is a signature that does not verify under the key given.
var ErrBadSignature = errors.New("signing: the signature does not verify under that key")

// Verify checks a document against the bytes it claims to cover and the public
// key it is to be checked under.
//
// The order is deliberate and is part of what an operator is told. The digests
// are compared FIRST, against the real bytes, so a mismatch reports itself as a
// mismatch; only then is the signature checked, so a failure there means what it
// says. Verifying the signature first would collapse both cases into "invalid
// signature" and lose the one an operator can actually act on.
func Verify(sig pluginapi.Signature, pub ed25519.PublicKey, manifest, module []byte) error {
	return VerifyWithIcon(sig, pub, manifest, module, nil)
}

// VerifyWithIcon is Verify over a package that may carry an icon.png. The icon is
// part of the signed message when there is one, so a swapped, added or removed icon
// is ErrBadSignature: the document carries no digest of it to compare first.
func VerifyWithIcon(sig pluginapi.Signature, pub ed25519.PublicKey, manifest, module, icon []byte) error {
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("signing: the public key is %d bytes, not %d", len(pub), ed25519.PublicKeySize)
	}
	if sig.Algorithm != pluginapi.SignatureAlgorithmEd25519 {
		return fmt.Errorf("signing: the signature names the algorithm %q, and only %s is verified here",
			sig.Algorithm, pluginapi.SignatureAlgorithmEd25519)
	}
	manifestSum, moduleSum := sha256.Sum256(manifest), sha256.Sum256(module)
	if want := hex.EncodeToString(manifestSum[:]); !strings.EqualFold(sig.ManifestSHA256, want) {
		return fmt.Errorf("%w: the manifest hashes to %s and the signature names %s",
			ErrDigestMismatch, want, sig.ManifestSHA256)
	}
	if want := hex.EncodeToString(moduleSum[:]); !strings.EqualFold(sig.ModuleSHA256, want) {
		return fmt.Errorf("%w: the module hashes to %s and the signature names %s",
			ErrDigestMismatch, want, sig.ModuleSHA256)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(sig.Signature), ""))
	if err != nil {
		return fmt.Errorf("%w: it is not valid base64", ErrBadSignature)
	}
	if len(raw) != ed25519.SignatureSize {
		return fmt.Errorf("%w: an ed25519 signature is %d bytes and this one is %d",
			ErrBadSignature, ed25519.SignatureSize, len(raw))
	}
	if !ed25519.Verify(pub, messageOf(manifestSum, moduleSum, icon), raw) {
		return ErrBadSignature
	}
	return nil
}
