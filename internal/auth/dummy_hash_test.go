package auth

import (
	"errors"
	"fmt"
	"testing"
)

// TestDummyHashFallbackVerifiesNormally pins dummyHashFallback (the constant
// dummyHash's initializer falls back to if HashPassword itself ever fails at
// package init): it must parse and verify through VerifyPassword's ordinary
// path — no format error — and return the ordinary mismatch error for both
// the plaintext dummyHash normally hashes and for "". A format error here
// would mean the fallback silently escaped the timing equalization it exists
// for; a match would mean it is secretly the hash of a known plaintext.
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
