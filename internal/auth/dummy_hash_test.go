package auth

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestDummyHashFallbackVerifiesNormally pins dummyHashFallback (the constant
// dummyHash's initializer falls back to if its crypto/rand read or its
// HashPassword call ever fails at package init): it must parse and verify
// through VerifyPassword's ordinary path — no format error — and return the
// ordinary mismatch error for a representative password and for "". A format
// error here would mean the fallback silently escaped the timing equalization
// it exists for; a match would mean it is secretly the hash of a known
// plaintext.
func TestDummyHashFallbackVerifiesNormally(t *testing.T) {
	for _, pw := range []string{"dummy-password-for-timing-equalization", ""} {
		err := VerifyPassword(dummyHashFallback, pw)
		if !errors.Is(err, ErrPasswordMismatch) {
			t.Errorf("VerifyPassword(dummyHashFallback, %q) = %v, want ErrPasswordMismatch", pw, err)
		}
	}
}

// TestDummyHashFallbackParamsMatchCurrent pins dummyHashFallback's encoded
// argon2id parameters to the package's current ones, so a future cost bump
// (argon2Memory/argon2Time/argon2Threads) fails loudly here instead of
// silently skewing the login timing it is meant to equalize.
func TestDummyHashFallbackParamsMatchCurrent(t *testing.T) {
	want := fmt.Sprintf("argon2id$v=%d$m=%d,t=%d,p=%d$", argon2Version, argon2Memory, argon2Time, argon2Threads)
	if len(dummyHashFallback) < len(want) || dummyHashFallback[:len(want)] != want {
		t.Errorf("dummyHashFallback params = %q, want prefix %q (current package parameters)", dummyHashFallback, want)
	}
}

// TestDummyHashDoesNotVerifyKnownLiteral guards dummyHash against being the
// hash of a plaintext anyone can read out of the source: it must reject both
// the literal below and "", the same as any other password nobody happens to
// know.
func TestDummyHashDoesNotVerifyKnownLiteral(t *testing.T) {
	for _, pw := range []string{"dummy-password-for-timing-equalization", ""} {
		err := VerifyPassword(dummyHash, pw)
		if !errors.Is(err, ErrPasswordMismatch) {
			t.Errorf("VerifyPassword(dummyHash, %q) = %v, want ErrPasswordMismatch", pw, err)
		}
	}
}

// TestDummyHashesHaveCurrentKeyAndSaltLengths pins the salt and key segments
// of both dummyHashFallback and dummyHash to argon2SaltLen/argon2KeyLen bytes
// once decoded, so a future change to either constant fails here instead of
// silently shipping hashes whose embedded lengths disagree with the constants
// used to verify them.
func TestDummyHashesHaveCurrentKeyAndSaltLengths(t *testing.T) {
	for name, hash := range map[string]string{
		"dummyHashFallback": dummyHashFallback,
		"dummyHash":         dummyHash,
	} {
		parts := strings.Split(hash, "$")
		if len(parts) != 5 {
			t.Errorf("%s: malformed hash %q", name, hash)
			continue
		}
		salt, err := base64.RawStdEncoding.DecodeString(parts[3])
		if err != nil {
			t.Errorf("%s: invalid salt: %v", name, err)
			continue
		}
		if len(salt) != argon2SaltLen {
			t.Errorf("%s: salt length = %d, want argon2SaltLen (%d)", name, len(salt), argon2SaltLen)
		}
		key, err := base64.RawStdEncoding.DecodeString(parts[4])
		if err != nil {
			t.Errorf("%s: invalid key: %v", name, err)
			continue
		}
		if len(key) != argon2KeyLen {
			t.Errorf("%s: key length = %d, want argon2KeyLen (%d)", name, len(key), argon2KeyLen)
		}
	}
}
