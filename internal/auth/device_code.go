package auth

import (
	"crypto/rand"
	"fmt"
	"math/big"
)

// The two codes of the Device authorization grant (ADR-0036). They look similar
// and are not: one is a secret, the other is furniture.

// userCodeAlphabet is what a human reads off a TV and types into a phone, so it
// is chosen for the eye and the thumb, not for entropy:
//
//   - No 0/O and no 1/I/L. At ten feet, in a condensed TV face, those pairs are
//     a coin flip, and a code you cannot read twice the same way is a support
//     call.
//   - No U. It reads as V in several system faces at a distance.
//   - Uppercase only, so the phone's input can upcase without ambiguity and the
//     user never wonders whether case matters.
//   - Letters and digits only. A code is read aloud across a room as often as it
//     is read off the screen, and punctuation does not survive that trip.
//
// That leaves 30 symbols: 8 digits + 22 letters. See userCodeLength for why 30^4
// is enough.
const userCodeAlphabet = "23456789ABCDEFGHJKMNPQRSTVWXYZ"

// userCodeLength is 4 — short enough to read across a room and type one-handed.
//
// That is only 30^4 = 810,000 combinations, which would be indefensible for a
// credential. It is defensible here because the user code is NOT the credential:
// the TV polls with deviceCode, a 256-bit secret it generated and never showed
// anyone. Guessing a user code therefore yields no token and no access. The most
// a guesser achieves is approving a stranger's TV onto their OWN account — they
// give access away rather than take it.
//
// What the short code does cost is real, and is paid for elsewhere:
//   - Someone could farm live codes to link their TV to a victim's account IF the
//     victim could be induced to approve — that is phishing, which no code length
//     fixes and which the approve endpoint's rate limit and short TTL blunt.
//   - The space is small enough to crowd, so generation caps live requests
//     (store.CountLiveDeviceAuthRequests) rather than retrying forever.
const userCodeLength = 4

// deviceCodeEntropyBytes matches tokenEntropyBytes: the device code is a bearer
// secret in every sense that matters — whoever holds it collects the session —
// so it gets a session token's entropy, not a user code's.
const deviceCodeEntropyBytes = 32

// newUserCode returns a fresh human-readable code. Uniqueness is the caller's
// problem (it owns the database): this only promises uniform randomness over the
// alphabet, drawn from crypto/rand so the sequence is unguessable even to
// someone who has watched a thousand of them.
func newUserCode() (string, error) {
	out := make([]byte, userCodeLength)
	n := big.NewInt(int64(len(userCodeAlphabet)))
	for i := range out {
		// crypto/rand.Int over the alphabet length is rejection-sampled by the
		// stdlib, so there is no modulo bias toward the low end of the alphabet.
		k, err := rand.Int(rand.Reader, n)
		if err != nil {
			return "", fmt.Errorf("auth: generating user code: %w", err)
		}
		out[i] = userCodeAlphabet[k.Int64()]
	}
	return string(out), nil
}

// newDeviceCode returns the TV's poll secret: high-entropy, URL-safe, and shown
// to no human. Like a session token, the raw value is returned exactly once and
// only its hash is stored (ADR-0015).
func newDeviceCode() (string, error) {
	return newToken()
}
