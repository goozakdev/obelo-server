package auth

import (
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
	for _, bad := range []string{"", "garbage", "scrypt$x$y", "bcrypt$1$a$b"} {
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

// TestProductionKDFParamsAreOWASP pins productionKDFParams — what a production
// binary actually derives new hashes with — to the OWASP 2023 argon2id
// guidance. Weaken the constants and this fails.
//
// It also proves those parameters still verify a real password, not just a
// mismatch: a hash produced with exactly them (a fixed stored string and a
// known password, generated once with argon2.IDKey directly rather than
// derived by this binary) checks out through the ordinary VerifyPassword path
// for the right password and fails it for a wrong one. A verifyArgon2id that
// ignored the parameters parsed out of the stored string (using some fixed
// cheap params instead) would pass a mismatch check by accident but fail the
// match here.
func TestProductionKDFParamsAreOWASP(t *testing.T) {
	memory, time, threads := productionKDFParams()
	if memory != 64*1024 || time != 3 || threads != 2 {
		t.Fatalf("production argon2id params = m=%d,t=%d,p=%d, want m=%d,t=3,p=2 (OWASP 2023)",
			memory, time, threads, 64*1024)
	}
	const stored = "argon2id$v=19$m=65536,t=3,p=2$MDEyMzQ1Njc4OWFiY2RlZg$NCLW3mJRjIVMQ0H52Ndg/7bsjremA7fceoJFw7FWyVg"
	const password = "swordfish-and-a-narwhal"
	if err := VerifyPassword(stored, password); err != nil {
		t.Errorf("verify of the right password against a production-params hash = %v, want nil", err)
	}
	if err := VerifyPassword(stored, "definitely-not-the-password"); !errors.Is(err, ErrPasswordMismatch) {
		t.Errorf("verify against a production-params hash = %v, want ErrPasswordMismatch", err)
	}
}

// TestKDFParamsFor pins kdfParamsFor's two branches directly: false is the
// OWASP production params, true is the cheap test stand-in. This catches an
// edit that makes kdfParamsFor ignore its argument and return the cheap
// params unconditionally (e.g. `if true || isTest`) — kdfParamsFor(false)
// would then wrongly equal the cheap params. It does NOT catch the same swap
// made where kdfHashParams calls kdfParamsFor(testing.Testing()), or in
// HashPasswordContext itself: every test binary already has
// testing.Testing() == true, so no test running in this binary can
// distinguish either from the real wiring.
func TestKDFParamsFor(t *testing.T) {
	if memory, time, threads := kdfParamsFor(false); memory != 64*1024 || time != 3 || threads != 2 {
		t.Errorf("kdfParamsFor(false) = m=%d,t=%d,p=%d, want the OWASP production params m=%d,t=3,p=2",
			memory, time, threads, 64*1024)
	}
	if memory, time, threads := kdfParamsFor(true); memory != testArgon2Memory || time != testArgon2Time || threads != testArgon2Threads {
		t.Errorf("kdfParamsFor(true) = m=%d,t=%d,p=%d, want the cheap test stand-in m=%d,t=%d,p=%d",
			memory, time, threads, testArgon2Memory, testArgon2Time, testArgon2Threads)
	}
}
