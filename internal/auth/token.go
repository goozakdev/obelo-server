package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// tokenEntropyBytes is the size of the random opaque token. 32 bytes = 256 bits
// of entropy, far beyond brute-force reach, matching ADR-0015's "high-entropy
// random" requirement.
const tokenEntropyBytes = 32

// newToken returns a fresh opaque bearer token as a URL-safe base64 string. The
// raw value is shown to the client exactly once (at login) and never persisted;
// only its hash is stored (see hashToken).
func newToken() (string, error) {
	b := make([]byte, tokenEntropyBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: generating token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashToken maps a raw bearer token to the value stored and looked up in the
// database. A plain SHA-256 is correct here (unlike for passwords): the token
// is already a 256-bit uniformly random secret, so it is not subject to
// dictionary/brute-force attack and needs no salt or work factor. Storing only
// the hash means a database leak does not expose usable tokens. The hex digest
// is deterministic, so it doubles as the primary-key lookup.
func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
