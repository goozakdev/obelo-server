package v1

// The detached signature document (.scratch/plugin-system issue 15) — how an
// author asserts that a manifest and a module are theirs, and the only way a
// publisher name becomes something a server can check.
//
// # It is DETACHED, and it could not have been anything else
//
// A manifest is written to disk byte for byte as its author shipped it and is
// never re-encoded (ADR-0058; the install path says so in its own comment). That
// is what makes a signature over it stable — the bytes on disk are the bytes that
// were signed, with no canonicalisation step to agree on and nothing to get wrong
// in a second implementation. The cost is an ordering constraint with no way
// around it: THE SIGNATURE CANNOT BE A FIELD OF THE DOCUMENT IT COVERS, because
// adding it would change the bytes it covers. So it travels beside the manifest,
// as its own small JSON document, under the name SignatureFile.
//
// # What is signed, exactly
//
// Not the manifest's "canonical form" — the manifest has no canonical form and
// needs none, because the bytes ARE the object. The signed message is:
//
//	SignatureDomain || sha256(manifest bytes) || sha256(module bytes)
//
// — the domain separator string, then the two 32-byte digests, concatenated with
// no separator and no length prefix, signed with ed25519 over that 80-byte
// message. The digests also appear in the document as lower-case hex, so a reader
// can see which artifacts a signature claims to cover without holding them, and a
// verifier checks them against the real bytes before it checks the signature: a
// document whose digests do not match what arrived is refused as a mismatch
// rather than as a bad signature, which are different things to tell an operator.
//
// The domain separator exists so that these 64 bytes can never be mistaken for
// any other thing this project ever asks the same key to sign. It is part of the
// message, not a prefix of the file.
//
// # What it is NOT
//
// It is not `cmd/keytool`. That tool is AES-GCM sealing for the maintainer's own
// key rotation (ADR-0032) — a different primitive answering a different question,
// and reusing it would have meant one key material format meaning two things. The
// signing tool for this is `cmd/pluginsign`, new and separate.
//
// It is also not a chain of trust. There is no CA, no root, no revocation list and
// no registry of publishers — ADR-0001 again. A signature is checked against a key
// an ADMIN PINNED on their own server, by hand, for a publisher they named; with
// nothing pinned it is checked against nothing and the server behaves exactly as
// it did before signatures existed.

// SignatureFile is the conventional name of the detached signature document,
// published beside `manifest.json` and written beside it on disk when a plugin is
// installed with one.
const SignatureFile = "plugin.sig.json"

// SignatureAlgorithmEd25519 is the one algorithm value this contract defines.
// It is a field rather than an assumption so that a second algorithm can be added
// additively; a document naming anything else is refused by name, never guessed at.
const SignatureAlgorithmEd25519 = "ed25519"

// SignatureDomain is the domain separator the signed message begins with. It ends
// in a newline deliberately, so the separator can never run into the digest bytes
// that follow it, and it names the contract major so a v2 signature is a different
// message even over identical artifacts.
const SignatureDomain = "obelo-plugin-v1\n"

// Signature is the detached signature document: `plugin.sig.json`, published
// beside the manifest it covers.
//
// Publisher is the only field with any consequence for trust. A server with keys
// pinned looks Publisher up among them, verifies under THAT key, and refuses
// everything else — so the name is not a label, it is the lookup. KeyID never
// decides anything: it is shown to an operator so they can tell which of a
// publisher's keys signed this, and a document whose KeyID does not match the
// pinned key still verifies or fails on the signature alone.
type Signature struct {
	// Publisher is who claims to have published this plugin — the name an Admin
	// pins a public key against. Matching is exact and case-insensitive.
	Publisher string `json:"publisher"`
	// KeyID is a short fingerprint of the public key that signed, for a human
	// comparing what a server has pinned against what a publisher advertises. It is
	// DIAGNOSTIC: verification uses the pinned key's bytes and never this string.
	KeyID string `json:"keyId,omitempty"`
	// Algorithm is the signature algorithm. SignatureAlgorithmEd25519 is the only
	// value this version defines.
	Algorithm string `json:"algorithm"`
	// ManifestSHA256 and ModuleSHA256 are lower-case hex digests of the two signed
	// artifacts, so a reader can see what is claimed without holding the bytes. A
	// verifier recomputes both from the real bytes and refuses a mismatch BEFORE it
	// looks at the signature — "this signature is for a different module" and "this
	// signature is forged" are different sentences an operator needs told apart.
	ManifestSHA256 string `json:"manifestSha256"`
	ModuleSHA256   string `json:"moduleSha256"`
	// Signature is the ed25519 signature over
	// SignatureDomain || sha256(manifest) || sha256(module), standard base64 with
	// padding.
	Signature string `json:"signature"`
}
