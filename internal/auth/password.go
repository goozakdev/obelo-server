package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Password hashing uses argon2id, the modern memory-hard password KDF and the
// current OWASP/PHC recommendation for password storage.
//
// Hashes are stored as a self-describing PHC-style string, so the algorithm and
// its cost parameters can evolve without a schema change: VerifyPassword
// dispatches on the recorded prefix. argon2id is produced for new hashes; the
// legacy pbkdf2-sha256 format is still accepted on verify so older stored hashes
// keep working through an algorithm migration.
//
// All comparisons of derived keys are constant-time.

const (
	argon2idPrefix = "argon2id"
	argon2Version  = 19 // argon2.Version

	// Cost parameters (OWASP 2023 argon2id guidance): 64 MiB memory, 3 passes,
	// 2 lanes. Tune upward as hardware improves; stored hashes carry their own
	// params so raising these does not break existing logins.
	argon2Memory  = 64 * 1024 // KiB
	argon2Time    = 3
	argon2Threads = 2
	argon2KeyLen  = 32 // bytes
	argon2SaltLen = 16 // bytes

	// Legacy PBKDF2 parameters, retained for verifying pre-migration hashes.
	pbkdf2Prefix = "pbkdf2-sha256"
)

// ErrPasswordMismatch is returned by VerifyPassword when the password does not
// match the stored hash. It is deliberately indistinguishable from other
// verification failures so callers can return a single generic auth error.
var ErrPasswordMismatch = errors.New("auth: password mismatch")

// HashPassword derives a self-describing PHC-style argon2id hash of the form
//
//	argon2id$v=19$m=65536,t=3,p=2$<base64Salt>$<base64Key>
//
// using a fresh random salt. The full string is what gets persisted; it carries
// every parameter needed to verify later, so the algorithm/cost can evolve
// without a schema change.
//
// It waits for a KDF slot (see the concurrency section at the bottom of this
// file) and cannot be cancelled while it waits. Anything serving a request should
// call HashPasswordContext instead; this form is for startup and for tests, where
// there is no request to hang up.
func HashPassword(password string) (string, error) {
	return HashPasswordContext(context.Background(), password)
}

// HashPasswordContext is HashPassword with a cancellable wait for a KDF slot. If
// ctx is done before a slot frees up it returns ctx.Err() and derives nothing —
// the caller's client has gone, and holding a place in the queue for it would let
// a disconnected caller crowd out a present one.
func HashPasswordContext(ctx context.Context, password string) (string, error) {
	if password == "" {
		return "", errors.New("auth: password must not be empty")
	}
	salt := make([]byte, argon2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: generating salt: %w", err)
	}

	release, err := acquireKDF(ctx)
	if err != nil {
		return "", err
	}
	defer release()

	key := argon2.IDKey([]byte(password), salt, argon2Time, argon2Memory, argon2Threads, argon2KeyLen)
	return fmt.Sprintf("%s$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2idPrefix,
		argon2Version,
		argon2Memory, argon2Time, argon2Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword checks password against a stored hash string. It returns nil on
// a match, ErrPasswordMismatch on a mismatch, and a descriptive error if the
// stored hash is malformed/unsupported. The derived-key comparison is
// constant-time. It dispatches on the stored algorithm prefix so both argon2id
// (current) and legacy pbkdf2-sha256 hashes verify.
//
// Like HashPassword it waits uncancellably for a KDF slot; request paths want
// VerifyPasswordContext.
func VerifyPassword(stored, password string) error {
	return VerifyPasswordContext(context.Background(), stored, password)
}

// VerifyPasswordContext is VerifyPassword with a cancellable wait for a KDF slot,
// returning ctx.Err() if the caller goes away before one frees up.
//
// The slot is taken before the algorithm dispatch rather than around the argon2
// call alone. Parsing a PHC string is microseconds, so the bound is unaffected,
// and it means the legacy pbkdf2 path — which is not memory-hard but is exactly
// as unauthenticated and exactly as expensive to spam — queues in the same line
// instead of quietly escaping the meter.
func VerifyPasswordContext(ctx context.Context, stored, password string) error {
	release, err := acquireKDF(ctx)
	if err != nil {
		return err
	}
	defer release()

	switch {
	case strings.HasPrefix(stored, argon2idPrefix+"$"):
		return verifyArgon2id(stored, password)
	case strings.HasPrefix(stored, pbkdf2Prefix+"$"):
		return verifyPBKDF2(stored, password)
	default:
		return fmt.Errorf("auth: unsupported or malformed password hash")
	}
}

// verifyArgon2id parses and checks an argon2id PHC string:
//
//	argon2id$v=19$m=65536,t=3,p=2$<b64salt>$<b64key>
func verifyArgon2id(stored, password string) error {
	parts := strings.Split(stored, "$")
	if len(parts) != 5 {
		return fmt.Errorf("auth: malformed argon2id hash")
	}
	var version int
	if _, err := fmt.Sscanf(parts[1], "v=%d", &version); err != nil || version != argon2Version {
		return fmt.Errorf("auth: unsupported argon2 version in password hash")
	}
	var mem uint32
	var time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[2], "m=%d,t=%d,p=%d", &mem, &time, &threads); err != nil {
		return fmt.Errorf("auth: invalid argon2 parameters in password hash")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return fmt.Errorf("auth: invalid salt in password hash")
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return fmt.Errorf("auth: invalid key in password hash")
	}
	got := argon2.IDKey([]byte(password), salt, time, mem, threads, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrPasswordMismatch
	}
	return nil
}

// verifyPBKDF2 checks a legacy pbkdf2-sha256 PHC string:
//
//	pbkdf2-sha256$<iterations>$<b64salt>$<b64key>
func verifyPBKDF2(stored, password string) error {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 {
		return fmt.Errorf("auth: malformed pbkdf2 hash")
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 {
		return fmt.Errorf("auth: invalid iteration count in password hash")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return fmt.Errorf("auth: invalid salt in password hash")
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return fmt.Errorf("auth: invalid key in password hash")
	}
	got := pbkdf2(sha256.New, []byte(password), salt, iter, len(want))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrPasswordMismatch
	}
	return nil
}

// --- KDF concurrency -------------------------------------------------------

// argon2id is memory-hard on purpose: every derivation allocates argon2Memory
// (64 MiB) and holds it for the whole run. That is what makes an offline cracker
// expensive — and, unmetered, it is also a loaded gun pointed at this process.
// POST /auth/login runs a derivation per request and needs no credentials to do
// it, so with nothing bounding concurrency ~100 simultaneous login POSTs is
// ~6.4 GB of resident memory conjured by an anonymous caller. The box OOMs long
// before the password guessing gets anywhere; the denial of service IS the
// attack, and it is cheaper to mount than the brute force it looks like.
//
// So the KDF is metered. A slot is held ONLY across the derivation, which makes
// peak KDF memory a constant no request volume can move:
//
//	kdfMaxConcurrency × argon2Memory  =  4 × 64 MiB  =  256 MiB
//
// Everything else a login does — the user lookup, the Device upsert, the token
// insert — happens outside the slot. Do not "tidy" this by acquiring once around
// a whole operation: holding a KDF slot across a database write couples the
// memory bound to disk latency, and a bound that depends on how slow the disk is
// today is not a bound.
//
// The meter lives inside HashPassword/VerifyPassword rather than at the call
// sites, deliberately. There are five ways into argon2 from a request — login's
// real verify, login's dummy-hash timing verify, Setup, CreateUser, SetPassword —
// and a sixth added later would silently escape a call-site gate. Here it cannot.
const (
	// kdfMaxConcurrency is the ceiling on the cap below, and the number in the
	// memory arithmetic above. Raising it raises peak RSS by 64 MiB a step; this
	// server is expected to share a box with a media library and possibly a
	// transcode, so 256 MiB of password hashing is already generous.
	kdfMaxConcurrency = 4

	// kdfMinConcurrency keeps a single-CPU host able to log in at all. Zero slots
	// is not a smaller memory bound, it is an outage.
	kdfMinConcurrency = 1
)

// kdfConcurrency is the live cap: one derivation per CPU, clamped to the range
// above. Sizing from GOMAXPROCS rather than pinning a number means a one-core NAS
// does not queue four memory-hungry derivations it cannot run in parallel anyway.
var kdfConcurrency = min(max(runtime.GOMAXPROCS(0), kdfMinConcurrency), kdfMaxConcurrency)

// kdfSem is the counting semaphore, a plain buffered channel rather than
// golang.org/x/sync/semaphore: every acquisition here weighs 1, so the weighted
// semaphore's whole feature is unused, and promoting it from an indirect to a
// direct dependency would be a go.mod change bought for a select statement.
//
// It must exist before dummyHash (service.go), which calls HashPassword during
// package initialization. Go's initialization ordering guarantees that — dummyHash
// depends on HashPassword, which references this variable — but it is the kind of
// guarantee worth writing down, because the failure mode if it were ever violated
// is a send on a nil channel, i.e. a hang at startup with no output.
var kdfSem = make(chan struct{}, kdfConcurrency)

// acquireKDF takes a derivation slot and returns the function that gives it back.
// It blocks while every slot is busy.
//
// Waiting is fine — the login failure limiter (login_limit.go) bounds how much
// work can pile up behind this — but it must be interruptible: a client that has
// already hung up must not keep a place in line, or a flood of abandoned requests
// starves the one caller still listening.
func acquireKDF(ctx context.Context) (release func(), err error) {
	select {
	case kdfSem <- struct{}{}:
		// Uncontended: don't consult ctx at all. With a free slot AND a done ctx,
		// select picks at random, and a request that could have been served
		// immediately should not lose a coin flip to a context that expired a
		// microsecond ago.
		return func() { <-kdfSem }, nil
	default:
	}
	select {
	case kdfSem <- struct{}{}:
		return func() { <-kdfSem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// pbkdf2 implements PBKDF2 (RFC 8018) over the given PRF hash, retained only to
// verify legacy pre-migration hashes. New hashes use argon2id.
func pbkdf2(h func() hash.Hash, password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(h, password)
	hashLen := prf.Size()
	numBlocks := (keyLen + hashLen - 1) / hashLen

	var out []byte
	buf := make([]byte, 4)
	block := make([]byte, hashLen)
	for i := 1; i <= numBlocks; i++ {
		prf.Reset()
		prf.Write(salt)
		buf[0] = byte(i >> 24)
		buf[1] = byte(i >> 16)
		buf[2] = byte(i >> 8)
		buf[3] = byte(i)
		prf.Write(buf)
		u := prf.Sum(nil)
		copy(block, u)
		for n := 2; n <= iter; n++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for x := range block {
				block[x] ^= u[x]
			}
		}
		out = append(out, block...)
	}
	return out[:keyLen]
}
