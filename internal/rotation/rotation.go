// Package rotation is the CLIENT half of the optional maintainer-hosted metadata
// key-rotation channel (ADR-0032): the bounded, disable-able mechanism that lets a
// running server pick up REPLACEMENT default TMDB/fanart.tv credentials without a
// release, as the precedence layer between operator BYOK and the build-injected
// bootstrap key.
//
// It is deliberately a small, pure library: it fetches a versioned JSON envelope
// from a configurable URL, decrypts its ciphertext payload with the build-injected
// kAppEncKey (base64 AES-256-GCM), and hands back the plaintext keys. It performs
// NO scheduling, NO consent checking, and NO persistence policy — the app package
// owns those (it gates the fetch on the first-run consent decision, runs the
// periodic loop, and applies the result to the running provider). The real
// Cloudflare Worker + KV that serves the envelope is issue 04; this slice is fully
// exercised against an httptest stub.
//
// Everything here is FAIL-SAFE by construction: every parse/decrypt/version error
// is returned as a plain error so the caller can log once and fall through to the
// bootstrap key (ADR-0001: a maintainer outage never degrades the server beyond
// metadata freshness). Nothing here ever panics on hostile input.
package rotation

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// SupportedVersion is the one envelope schema version this client understands. The
// versioned envelope (ADR-0032) future-proofs the format: a payload announcing a
// different v is REJECTED (an explicit "unsupported version" error that falls
// through to the bootstrap key) rather than being mis-parsed against the wrong
// shape. Issue 04's Worker serves v=1; a future breaking change bumps this in a
// release alongside the new payload shape.
const SupportedVersion = 1

// maxEnvelopeBytes caps how much of the response body we read, so a hostile or
// misconfigured endpoint answering an enormous body cannot exhaust memory. The
// real envelope is a few hundred bytes; 64 KiB is generous headroom.
const maxEnvelopeBytes = 64 << 10

// Envelope is the versioned JSON document the rotation endpoint serves (ADR-0032):
//
//	{ "v": 1, "minAppVersion": "0.x",
//	  "payload": "<base64( nonce ‖ AES-256-GCM(kAppEncKey, {"tmdb":…,"fanart":…}) )>" }
//
// The keys live ONLY inside the encrypted payload — the envelope itself carries no
// secret, which is why the public URL is safe to expose (a scraper that fetches it
// gets ciphertext useless without the build-injected kAppEncKey). MinAppVersion
// lets the maintainer publish a payload only newer builds should adopt.
type Envelope struct {
	V             int    `json:"v"`
	MinAppVersion string `json:"minAppVersion"`
	Payload       string `json:"payload"`
}

// Keys is the decrypted default-credential set — the plaintext inside a payload.
// A field is empty when that provider has no bundled default (the maintainer may
// rotate one source without the other). It is the value the app layers in as the
// cached-rotation precedence layer (config.RotationKeys mirrors it).
type Keys struct {
	TMDB   string `json:"tmdb"`
	Fanart string `json:"fanart"`
}

// Client fetches and decrypts the rotation envelope. Its zero value is unusable
// (URL and EncKeyB64 are required); the app constructs it from config. It holds no
// state — Fetch is safe to call repeatedly on a shared instance.
type Client struct {
	// URL is the fully-qualified rotation endpoint (e.g. https://host/v1/keys).
	URL string
	// EncKeyB64 is the base64 AES-256-GCM key (the build-injected kAppEncKey) that
	// decrypts the payload. Empty ⇒ this build has no rotation key, so Fetch errors
	// out immediately without a network call (a build-from-source binary).
	EncKeyB64 string
	// AppVersion is this server's version, compared against the envelope's
	// minAppVersion so a payload meant only for newer builds is skipped, not adopted.
	AppVersion string
	// UserAgent labels the request so the Worker can shed dumb bots on a header check
	// (trivially spoofed — a politeness signal, not a security control, per ADR-0032).
	UserAgent string
	// HTTP is the client used for the fetch; nil falls back to http.DefaultClient.
	// The caller is expected to supply one with a sane timeout (or pass a ctx with a
	// deadline) so a hung endpoint never blocks the periodic loop.
	HTTP *http.Client
}

// Fetch retrieves the envelope, validates its version, and returns the decrypted
// Keys. Every failure mode — no enc key, transport error, non-200, oversized or
// malformed body, unsupported version, too-new minAppVersion, undecryptable
// payload — comes back as a plain error so the caller falls through to the
// bootstrap key and logs once (ADR-0032 fail-safe). It never blocks beyond the
// ctx/HTTP deadline and never panics.
func (c Client) Fetch(ctx context.Context) (Keys, error) {
	if c.EncKeyB64 == "" {
		return Keys{}, fmt.Errorf("rotation: no app encryption key in this build (build-from-source); rotation is unavailable")
	}
	if c.URL == "" {
		return Keys{}, fmt.Errorf("rotation: no endpoint URL configured")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return Keys{}, fmt.Errorf("rotation: building request: %w", err)
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	req.Header.Set("Accept", "application/json")

	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return Keys{}, fmt.Errorf("rotation: fetching %s: %w", c.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Keys{}, fmt.Errorf("rotation: endpoint returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxEnvelopeBytes))
	if err != nil {
		return Keys{}, fmt.Errorf("rotation: reading response: %w", err)
	}

	var env Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return Keys{}, fmt.Errorf("rotation: decoding envelope: %w", err)
	}
	if env.V != SupportedVersion {
		return Keys{}, fmt.Errorf("rotation: unsupported envelope version %d (this build understands v%d)", env.V, SupportedVersion)
	}
	if !versionAtLeast(c.AppVersion, env.MinAppVersion) {
		return Keys{}, fmt.Errorf("rotation: payload requires app version >= %q, this build is %q — skipping", env.MinAppVersion, c.AppVersion)
	}

	keys, err := Decrypt(c.EncKeyB64, env.Payload)
	if err != nil {
		return Keys{}, err
	}
	return keys, nil
}

// Decrypt opens a base64 `nonce ‖ AES-256-GCM(key, plaintext)` payload with the
// base64 32-byte key and unmarshals the plaintext JSON into Keys. It is the exact
// inverse of Encrypt and is exported so the app's fetch path and the offline
// keytool (issue 04) share one implementation. A wrong key, tampered ciphertext,
// short input, or non-JSON plaintext all yield an error (never a partial/garbage
// result) so the caller falls through to the bootstrap key.
func Decrypt(encKeyB64, payloadB64 string) (Keys, error) {
	gcm, err := newGCM(encKeyB64)
	if err != nil {
		return Keys{}, err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(payloadB64))
	if err != nil {
		return Keys{}, fmt.Errorf("rotation: payload is not valid base64: %w", err)
	}
	ns := gcm.NonceSize()
	if len(raw) < ns {
		return Keys{}, fmt.Errorf("rotation: payload too short (%d bytes) to contain a %d-byte nonce", len(raw), ns)
	}
	nonce, ciphertext := raw[:ns], raw[ns:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return Keys{}, fmt.Errorf("rotation: decrypting payload (wrong key or tampered ciphertext): %w", err)
	}
	var keys Keys
	if err := json.Unmarshal(plaintext, &keys); err != nil {
		return Keys{}, fmt.Errorf("rotation: decrypted payload is not valid JSON: %w", err)
	}
	return keys, nil
}

// Encrypt seals keys as a base64 `nonce ‖ AES-256-GCM(key, json)` payload under the
// base64 32-byte key, with a fresh random nonce each call. It is the maintainer's
// side of the channel — the offline keytool (issue 04) calls it to produce a new
// payload for the Worker — and lives here so encrypt/decrypt can never drift out of
// sync (the round-trip is unit-tested). It is also how tests build stub payloads.
func Encrypt(encKeyB64 string, keys Keys) (string, error) {
	gcm, err := newGCM(encKeyB64)
	if err != nil {
		return "", err
	}
	plaintext, err := json.Marshal(keys)
	if err != nil {
		return "", fmt.Errorf("rotation: encoding keys: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("rotation: generating nonce: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// newGCM builds an AES-256-GCM AEAD from the base64 key, enforcing the 32-byte
// (AES-256) length so a truncated/mis-encoded kAppEncKey fails loudly here rather
// than silently using a weaker cipher.
func newGCM(encKeyB64 string) (cipher.AEAD, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encKeyB64))
	if err != nil {
		return nil, fmt.Errorf("rotation: app encryption key is not valid base64: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("rotation: app encryption key must be 32 bytes (AES-256), got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("rotation: initializing AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("rotation: initializing GCM: %w", err)
	}
	return gcm, nil
}

// versionAtLeast reports whether the dotted-numeric version have is >= min. It is
// deliberately lenient (this is a soft "don't adopt a too-new payload" gate, not a
// strict semver engine): an empty min is always satisfied, missing trailing
// segments count as 0, and a non-numeric segment (e.g. the "x" in "0.x") counts as
// 0 — so "0.x" means "any 0.* or newer". Anything it can't make sense of errs
// toward adopting the payload (returns true), because the envelope's own v gate is
// the hard compatibility check.
func versionAtLeast(have, min string) bool {
	if strings.TrimSpace(min) == "" {
		return true
	}
	h := parseVersion(have)
	m := parseVersion(min)
	n := len(h)
	if len(m) > n {
		n = len(m)
	}
	for i := 0; i < n; i++ {
		var hv, mv int
		if i < len(h) {
			hv = h[i]
		}
		if i < len(m) {
			mv = m[i]
		}
		if hv != mv {
			return hv > mv
		}
	}
	return true // equal
}

// parseVersion splits a dotted version into numeric segments, treating any
// non-numeric segment as 0 (so "0.x" → [0, 0]). A leading "v" is tolerated.
func parseVersion(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ".")
	out := make([]int, len(parts))
	for i, p := range parts {
		// Trim any pre-release/build suffix (e.g. "1-rc1") to its leading digits.
		digits := p
		if idx := strings.IndexFunc(p, func(r rune) bool { return r < '0' || r > '9' }); idx >= 0 {
			digits = p[:idx]
		}
		n, err := strconv.Atoi(digits)
		if err != nil {
			n = 0
		}
		out[i] = n
	}
	return out
}
