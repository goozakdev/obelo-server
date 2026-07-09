package auth

import "testing"

// TestNewTokenUniqueAndHashDeterministic: tokens are unique per call, and
// hashing a token is deterministic (so it can serve as a DB lookup key) yet
// differs between distinct tokens.
func TestNewTokenUniqueAndHashDeterministic(t *testing.T) {
	a, err := newToken()
	if err != nil {
		t.Fatalf("newToken: %v", err)
	}
	b, err := newToken()
	if err != nil {
		t.Fatalf("newToken: %v", err)
	}
	if a == b {
		t.Errorf("two tokens collided: %q", a)
	}
	if a == "" {
		t.Errorf("token is empty")
	}

	if hashToken(a) != hashToken(a) {
		t.Errorf("hashToken is not deterministic")
	}
	if hashToken(a) == hashToken(b) {
		t.Errorf("distinct tokens hashed to the same value")
	}
	if hashToken(a) == a {
		t.Errorf("hash equals raw token; raw value would be stored")
	}
}
