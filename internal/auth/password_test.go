package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// TestHashVerifyRoundTrip: a hashed password verifies, a wrong one does not,
// and two hashes of the same password differ (random salt).
func TestHashVerifyRoundTrip(t *testing.T) {
	h, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(h, argon2idPrefix+"$") {
		t.Errorf("hash not self-describing: %q", h)
	}
	if strings.Contains(h, "correct horse") {
		t.Errorf("hash leaks plaintext: %q", h)
	}
	if err := VerifyPassword(h, "correct horse battery staple"); err != nil {
		t.Errorf("verify of correct password failed: %v", err)
	}
	if err := VerifyPassword(h, "wrong"); !errors.Is(err, ErrPasswordMismatch) {
		t.Errorf("verify of wrong password = %v, want ErrPasswordMismatch", err)
	}

	h2, _ := HashPassword("correct horse battery staple")
	if h == h2 {
		t.Errorf("two hashes of same password are identical; salt not random")
	}
}

// TestVerifyMalformedHash: a malformed stored hash is a non-mismatch error, not
// a silent pass.
func TestVerifyMalformedHash(t *testing.T) {
	for _, bad := range []string{"", "garbage", "pbkdf2-sha256$x$y", "bcrypt$1$a$b"} {
		if err := VerifyPassword(bad, "anything"); err == nil {
			t.Errorf("VerifyPassword(%q) = nil, want error", bad)
		}
	}
}

// TestEmptyPasswordRejected: hashing an empty password is an error.
func TestEmptyPasswordRejected(t *testing.T) {
	if _, err := HashPassword(""); err == nil {
		t.Errorf("HashPassword(\"\") = nil error, want error")
	}
}

// TestVerifyLegacyPBKDF2: a stored pbkdf2-sha256 hash (pre-argon2id migration)
// still verifies, so an algorithm swap doesn't lock existing users out.
func TestVerifyLegacyPBKDF2(t *testing.T) {
	const pw = "legacy password"
	salt := []byte("0123456789abcdef")
	key := pbkdf2(sha256.New, []byte(pw), salt, 210000, 32)
	stored := pbkdf2Prefix + "$210000$" +
		base64.RawStdEncoding.EncodeToString(salt) + "$" +
		base64.RawStdEncoding.EncodeToString(key)

	if err := VerifyPassword(stored, pw); err != nil {
		t.Errorf("legacy pbkdf2 verify of correct password failed: %v", err)
	}
	if err := VerifyPassword(stored, "wrong"); !errors.Is(err, ErrPasswordMismatch) {
		t.Errorf("legacy pbkdf2 verify of wrong password = %v, want ErrPasswordMismatch", err)
	}
}
