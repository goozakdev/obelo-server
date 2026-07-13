package config

import "encoding/base64"

// Build-time default metadata credentials (ADR-0032). These three package vars
// are EMPTY string literals in source and are populated ONLY by official/Docker
// builds via `-ldflags -X`, sourced from CI secrets — the plaintext keys never
// enter the open-source repo. A build-from-source binary therefore ships no
// default keys: credential resolution falls through to operator BYOK, and absent
// that to sparse metadata (ADR-0001's blessed degrade path).
//
// The injected bootstrap*Key values are lightly OBFUSCATED (base64) so `strings`
// on the binary yields no bare, regex-matchable API key — a speed bump against
// automated scrapers, explicitly NOT a secrecy claim (ADR-0032: shipping a secret
// is unavoidably extractable; the endpoint and binary add rotation, not
// confidentiality). The credential-free-source invariant is machine-checked by
// TestBootstrapVarsEmptyInSource and the `make check-credentials-free` gate.
//
// kAppEncKey is declared here so the injection plumbing lives in one place, but it
// is CONSUMED by the rotation client in issue 03: it is the base64 AES-256-GCM key
// that decrypts the rotation endpoint's payload. It is NOT base64-obfuscated a
// second time — it is already base64 — so AppEncKey returns it verbatim.
var (
	bootstrapTMDBKey   string
	bootstrapFanartKey string
	kAppEncKey         string
)

// DefaultKeyRotationURL is the maintainer-hosted rotation endpoint official builds
// poll for a replacement default-key payload (ADR-0032, layer 2). Unlike the
// credential vars above it is NOT a secret — it is present in every official binary
// and its response is ciphertext-only, so a scraper that finds it gets nothing
// useful. It is nonetheless EMPTY in source and injected via `-ldflags -X` alongside
// the credentials, so the maintainer host is not committed to the open-source repo
// (no GitHub code-search links the endpoint to maintainer infra). A build-from-source
// binary therefore has no rotation URL and never polls — already true, since it also
// has no kAppEncKey to decrypt a payload with. Injected verbatim (not obfuscated: it
// is public in the binary by design). Overridable at runtime via
// JUICEBOX_KEY_ROTATION_URL so tests point it at a stub.
var DefaultKeyRotationURL string

// deobfuscate decodes a build-injected bootstrap credential. Injected values are
// base64 (the obfuscation applied at build time); an empty value (a from-source
// build) decodes to empty. A value that fails to base64-decode is treated as
// ABSENT — a malformed injection degrades to "no bundled key" rather than
// shipping a broken credential that would fail every provider call.
func deobfuscate(v string) string {
	if v == "" {
		return ""
	}
	b, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// BootstrapTMDBKey returns the de-obfuscated build-injected default TMDB
// credential, or "" when none was injected (a build-from-source binary).
func BootstrapTMDBKey() string { return deobfuscate(bootstrapTMDBKey) }

// BootstrapFanartKey returns the de-obfuscated build-injected default fanart.tv
// credential, or "" when none was injected (a build-from-source binary).
func BootstrapFanartKey() string { return deobfuscate(bootstrapFanartKey) }

// AppEncKey returns the build-injected base64 AES-256-GCM key used to decrypt the
// rotation endpoint's payload (ADR-0032), or "" when none was injected. The
// rotation client (issue 03) base64-decodes it to the raw 32-byte key; it is
// declared and injected here so the whole `-ldflags -X` surface exists in one
// place from the start.
func AppEncKey() string { return kAppEncKey }

// RotationKeys is the decrypted default-credential set from the rotation endpoint's
// cache (data/metadata-keys.json), the precedence layer BETWEEN operator BYOK and
// the build-injected bootstrap key (ADR-0032, issue 03). config owns this plain
// value type — not the rotation package — so the resolver can consult it without
// config importing rotation (the app bridges rotation.Cache → RotationKeys). A
// field is empty when that provider has no cached rotation key (a source the
// maintainer hasn't rotated, an install that hasn't fetched, or rotation disabled),
// in which case the resolver falls through to the bootstrap layer for that field.
type RotationKeys struct {
	TMDB   string
	Fanart string
}

// CredentialSource names which precedence layer supplied a default metadata
// credential (ADR-0032). It is reported by the resolver purely so boot logging
// can explain where the effective key came from — or that none is bundled. Its
// zero value is CredentialNone (no key at any layer). The rotation layer slots in
// BETWEEN operator and bootstrap.
type CredentialSource int

const (
	// CredentialNone means no layer supplied a key → the kind degrades to sparse
	// metadata (ADR-0001).
	CredentialNone CredentialSource = iota
	// CredentialOperator means the operator's own BYOK key (env / admin UI) won —
	// zero maintainer contact, the ADR-0001-pure path.
	CredentialOperator
	// CredentialRotation means the cached rotation-endpoint key was used (an
	// official build that has fetched a replacement default, with no operator key
	// set). It wins over the bootstrap key so a rotated credential supersedes the
	// one baked into the binary without a release.
	CredentialRotation
	// CredentialBootstrap means the build-injected default key was used (an
	// official build with no operator key and no cached rotation key).
	CredentialBootstrap
)

// String renders the source for the boot log line.
func (s CredentialSource) String() string {
	switch s {
	case CredentialOperator:
		return "operator"
	case CredentialRotation:
		return "rotation"
	case CredentialBootstrap:
		return "bootstrap"
	default:
		return "none"
	}
}

// ResolveTMDBKey applies the default-credential precedence chain for the TMDB key
// (ADR-0032): the operator's BYOK key wins, else the cached rotation key (rot.TMDB),
// else the build-injected bootstrap key, else none. It returns the effective key
// and its source (for boot logging and the rotation propagation guard). rot carries
// the current rotation-cache contents; pass the zero RotationKeys when rotation is
// disabled or nothing has been fetched.
func (c Config) ResolveTMDBKey(rot RotationKeys) (string, CredentialSource) {
	if c.TMDBAPIKey != "" {
		return c.TMDBAPIKey, CredentialOperator
	}
	if rot.TMDB != "" {
		return rot.TMDB, CredentialRotation
	}
	if k := BootstrapTMDBKey(); k != "" {
		return k, CredentialBootstrap
	}
	return "", CredentialNone
}

// ResolveFanartTVKey applies the same precedence chain (ADR-0032) for the
// fanart.tv key: operator BYOK wins, else the cached rotation key (rot.Fanart),
// else the build-injected bootstrap key, else none.
func (c Config) ResolveFanartTVKey(rot RotationKeys) (string, CredentialSource) {
	if c.FanartTVAPIKey != "" {
		return c.FanartTVAPIKey, CredentialOperator
	}
	if rot.Fanart != "" {
		return rot.Fanart, CredentialRotation
	}
	if k := BootstrapFanartKey(); k != "" {
		return k, CredentialBootstrap
	}
	return "", CredentialNone
}
