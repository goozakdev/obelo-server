package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
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
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", errors.New("auth: password must not be empty")
	}
	salt := make([]byte, argon2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: generating salt: %w", err)
	}
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
func VerifyPassword(stored, password string) error {
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
