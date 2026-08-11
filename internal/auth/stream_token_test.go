package auth_test

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marioquake/obelo-server/internal/auth"
	"github.com/marioquake/obelo-server/internal/store"
)

// Service-level tests for session stream tokens (.scratch/session-stream-tokens,
// issue 01).
//
// They live here for the same reason the device-auth tests do: every interesting
// rule is a rule about TIME or about SQL, and the black-box harness boots a real
// app whose clock cannot be moved. auth.WithClock plus a real store is the pairing
// that can actually assert "the expiry predicate is in the lookup" — a fake store
// would assert the fake, and a real clock would need a four-hour test.

// The session ids these tests use. A stream token's session_id is not a foreign
// key (Playback sessions live in memory, never in SQLite), so any opaque string
// stands in for one here exactly as a real session id would.
const (
	sessionA = "11111111-1111-4111-8111-111111111111"
	sessionB = "22222222-2222-4222-8222-222222222222"
)

// newStreamFixture builds a real store on a temp DB plus an auth service on a
// fake clock, seeded with one Admin — and hands back the DB, because several of
// these tests must look at the ROW to prove something about it (that the secret
// is not in it, that an expired row is refused while still present).
func newStreamFixture(t *testing.T) (*auth.Service, *fakeClock, store.User, *store.DB) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	clock := &fakeClock{t: time.Date(2026, 8, 3, 20, 0, 0, 0, time.UTC)}
	svc, err := auth.NewService(db, auth.WithClock(clock.now))
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	user, err := svc.Setup(context.Background(), svc.ClaimToken(), "admin", "correct-horse-battery")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	return svc, clock, user, db
}

func mustMint(t *testing.T, svc *auth.Service, sessionID, userID string) auth.StreamTokenGrant {
	t.Helper()
	grant, err := svc.MintStreamToken(sessionID, userID)
	if err != nil {
		t.Fatalf("mint stream token: %v", err)
	}
	if grant.Token == "" {
		t.Fatal("mint returned an empty token")
	}
	return grant
}

// countStreamTokens reports how many rows exist for a session, regardless of
// expiry — the "is the row still there?" question, as distinct from "does the
// lookup find it?".
func countStreamTokens(t *testing.T, db *store.DB, sessionID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM stream_tokens WHERE session_id = ?`, sessionID,
	).Scan(&n); err != nil {
		t.Fatalf("counting stream tokens: %v", err)
	}
	return n
}

// TestStreamTokenAuthorizesItsOwnSession is the happy path: a minted token
// verifies for the session it was minted for, and reports the owning User.
func TestStreamTokenAuthorizesItsOwnSession(t *testing.T) {
	svc, _, admin, _ := newStreamFixture(t)
	grant := mustMint(t, svc, sessionA, admin.ID)

	userID, ok := svc.VerifyStreamToken(grant.Token, sessionA)
	if !ok {
		t.Fatal("VerifyStreamToken refused a freshly minted token for its own session")
	}
	if userID != admin.ID {
		t.Errorf("userID = %q, want %q", userID, admin.ID)
	}
}

// TestStreamTokenRefusesAnotherSession is the security claim of the whole
// feature: a token that authenticates perfectly well is still no for a session it
// does not name. Both halves of the check live in one lookup, so this cannot pass
// by accident.
func TestStreamTokenRefusesAnotherSession(t *testing.T) {
	svc, _, admin, _ := newStreamFixture(t)
	grant := mustMint(t, svc, sessionA, admin.ID)

	if _, ok := svc.VerifyStreamToken(grant.Token, sessionB); ok {
		t.Fatal("a token minted for session A verified for session B")
	}
	// And it still works for its own session — the refusal above is about the
	// session, not about the token having been consumed.
	if _, ok := svc.VerifyStreamToken(grant.Token, sessionA); !ok {
		t.Fatal("the token stopped working for its own session after a wrong-session check")
	}
}

// TestStreamTokenExpiryIsEnforcedInTheLookup advances past the TTL and asserts
// the token is refused WHILE ITS ROW IS STILL PRESENT. That is the distinction
// worth testing: a Go-side comparison after a fetch would pass a "does it expire"
// test just as well, and would be the shape that grows a TOCTOU later.
func TestStreamTokenExpiryIsEnforcedInTheLookup(t *testing.T) {
	svc, clock, admin, db := newStreamFixture(t)
	grant := mustMint(t, svc, sessionA, admin.ID)

	if _, ok := svc.VerifyStreamToken(grant.Token, sessionA); !ok {
		t.Fatal("token refused before expiry")
	}
	clock.advance(grant.ExpiresIn + time.Second)

	if n := countStreamTokens(t, db, sessionA); n != 1 {
		t.Fatalf("stream token rows = %d, want the expired row still present (1)", n)
	}
	if _, ok := svc.VerifyStreamToken(grant.Token, sessionA); ok {
		t.Fatal("an expired token verified; expiry is not being applied in the lookup")
	}
	if _, _, ok := svc.ResolveStreamToken(grant.Token); ok {
		t.Fatal("an expired token resolved")
	}
}

// TestStreamTokenExpiryComparesAcrossTheDatetimeFormatBoundary is the same guard
// device auth carries: expires_at is compared IN SQL, so it must be written in
// the same format as the "now" it is compared against. An RFC3339 value is
// lexicographically greater than any same-day SQLite datetime('now') string ('T'
// sorts after ' '), so a mixed pair would read every row as unexpired.
func TestStreamTokenExpiryComparesAcrossTheDatetimeFormatBoundary(t *testing.T) {
	svc, clock, admin, _ := newStreamFixture(t)
	grant := mustMint(t, svc, sessionA, admin.ID)

	// One second past expiry, on the SAME calendar day the row was written — the
	// case a format mismatch would silently pass.
	clock.advance(grant.ExpiresIn + time.Second)
	if _, ok := svc.VerifyStreamToken(grant.Token, sessionA); ok {
		t.Fatal("a same-day expired token verified: the timestamp formats do not compare")
	}
}

// TestRevokeStreamTokensKillsEverySessionToken covers the DELETE
// /sessions/{id} promise at the service level: revocation is per SESSION, not per
// token, so a client that re-minted twice does not keep a live credential.
func TestRevokeStreamTokensKillsEverySessionToken(t *testing.T) {
	svc, _, admin, db := newStreamFixture(t)
	first := mustMint(t, svc, sessionA, admin.ID)
	second := mustMint(t, svc, sessionA, admin.ID)
	other := mustMint(t, svc, sessionB, admin.ID)

	if first.Token == second.Token {
		t.Fatal("two mints produced the same secret")
	}
	if err := svc.RevokeStreamTokens(sessionA); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	for name, tok := range map[string]string{"first": first.Token, "second": second.Token} {
		if _, ok := svc.VerifyStreamToken(tok, sessionA); ok {
			t.Errorf("%s token still verifies after its session was revoked", name)
		}
	}
	if n := countStreamTokens(t, db, sessionA); n != 0 {
		t.Errorf("rows left for the revoked session = %d, want 0", n)
	}
	// Another session's token is untouched — revocation is scoped, not a purge.
	if _, ok := svc.VerifyStreamToken(other.Token, sessionB); !ok {
		t.Error("revoking session A killed session B's token")
	}
	// Revoking again is a no-op, so the session-ended hook can run unconditionally.
	if err := svc.RevokeStreamTokens(sessionA); err != nil {
		t.Errorf("second revoke: %v", err)
	}
}

// TestStreamTokenIsStoredHashedAndUnreplayable: the raw secret is nowhere in the
// database, and the value that IS stored cannot be presented as a credential — a
// leaked table dump yields nothing playable.
func TestStreamTokenIsStoredHashedAndUnreplayable(t *testing.T) {
	svc, _, admin, db := newStreamFixture(t)
	grant := mustMint(t, svc, sessionA, admin.ID)

	var storedHash string
	if err := db.QueryRow(
		`SELECT token_hash FROM stream_tokens WHERE session_id = ?`, sessionA,
	).Scan(&storedHash); err != nil {
		t.Fatalf("reading token_hash: %v", err)
	}
	if storedHash == grant.Token {
		t.Fatal("the raw secret is stored in token_hash")
	}
	if strings.Contains(storedHash, grant.Token) {
		t.Fatal("the stored hash contains the raw secret")
	}
	// Replay the row: present the stored hash as if it were the secret.
	if _, ok := svc.VerifyStreamToken(storedHash, sessionA); ok {
		t.Fatal("the stored hash works as a stream token; a DB dump is replayable")
	}
}

// TestStreamTokenIsNotABearerToken and its mirror below are the two halves of
// "the namespaces do not overlap". Neither credential may cross into the other's
// lookup, in either direction.
func TestStreamTokenIsNotABearerToken(t *testing.T) {
	svc, _, admin, _ := newStreamFixture(t)
	grant := mustMint(t, svc, sessionA, admin.ID)

	if _, err := svc.Authenticate(grant.Token); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("Authenticate(streamToken) = %v, want ErrInvalidToken", err)
	}
}

func TestBearerTokenIsNotAStreamToken(t *testing.T) {
	svc, _, _, _ := newStreamFixture(t)
	login, err := svc.Login(context.Background(), "admin", "correct-horse-battery", auth.DeviceInput{
		Name: "Living Room TV", Platform: "tvos", ClientID: "tv-client-1",
	}, "192.0.2.10")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, ok := svc.VerifyStreamToken(login.Token, sessionA); ok {
		t.Fatal("an auth_tokens bearer verified as a stream token")
	}
	if _, _, ok := svc.ResolveStreamToken(login.Token); ok {
		t.Fatal("an auth_tokens bearer resolved as a stream token")
	}
}

// TestStreamTokenSurvivesURLPathEncoding: the secret rides in a URL PATH segment,
// so it must contain nothing a path escape would rewrite. base64url's alphabet
// ([A-Za-z0-9_-]) is entirely unreserved; standard base64's '+' and '/' would not
// be, and the '/' would split the segment and break the route outright.
func TestStreamTokenSurvivesURLPathEncoding(t *testing.T) {
	svc, _, admin, _ := newStreamFixture(t)

	for i := 0; i < 32; i++ {
		grant := mustMint(t, svc, sessionA, admin.ID)
		if escaped := url.PathEscape(grant.Token); escaped != grant.Token {
			t.Fatalf("token %q escapes to %q — it cannot ride a URL path unaltered", grant.Token, escaped)
		}
		if strings.ContainsAny(grant.Token, "/+=?#%&") {
			t.Fatalf("token %q contains a character that is not path-safe", grant.Token)
		}
		// 256 bits in base64 is 43 characters; anything shorter is not the secret
		// this is supposed to be.
		if len(grant.Token) != 43 {
			t.Fatalf("token %q is %d chars, want 43 (256 bits of base64url)", grant.Token, len(grant.Token))
		}
	}
}

// TestStreamTokensAreDistinct: 256 bits of CSPRNG, so two mints never collide.
// A weak or counter-derived secret would show up here immediately.
func TestStreamTokensAreDistinct(t *testing.T) {
	svc, _, admin, _ := newStreamFixture(t)
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		grant := mustMint(t, svc, sessionA, admin.ID)
		if seen[grant.Token] {
			t.Fatalf("duplicate stream token minted: %q", grant.Token)
		}
		seen[grant.Token] = true
	}
}

// TestMintSweepsExpiredStreamTokens: the reaper runs on the same trigger device
// auth's does — before each mint — so an abandoned server that never ended its
// sessions cleanly does not accumulate rows forever.
func TestMintSweepsExpiredStreamTokens(t *testing.T) {
	svc, clock, admin, db := newStreamFixture(t)
	stale := mustMint(t, svc, sessionA, admin.ID)

	clock.advance(stale.ExpiresIn + time.Minute)
	if n := countStreamTokens(t, db, sessionA); n != 1 {
		t.Fatalf("rows before the sweep = %d, want 1", n)
	}

	// Minting for an unrelated session still sweeps the whole table.
	fresh := mustMint(t, svc, sessionB, admin.ID)
	if n := countStreamTokens(t, db, sessionA); n != 0 {
		t.Errorf("expired rows left after a mint = %d, want 0", n)
	}
	if _, ok := svc.VerifyStreamToken(fresh.Token, sessionB); !ok {
		t.Error("the sweep took the token it had just minted")
	}
}

// TestSweepStreamTokensLeavesLiveRows: the sweeper deletes by expiry only, so a
// live session's credential survives a sweep it happened to be present for.
func TestSweepStreamTokensLeavesLiveRows(t *testing.T) {
	svc, clock, admin, db := newStreamFixture(t)
	stale := mustMint(t, svc, sessionA, admin.ID)
	clock.advance(stale.ExpiresIn + time.Minute)
	live := mustMint(t, svc, sessionB, admin.ID)

	if err := svc.SweepStreamTokens(); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n := countStreamTokens(t, db, sessionA); n != 0 {
		t.Errorf("expired rows after sweep = %d, want 0", n)
	}
	if n := countStreamTokens(t, db, sessionB); n != 1 {
		t.Errorf("live rows after sweep = %d, want 1", n)
	}
	if _, ok := svc.VerifyStreamToken(live.Token, sessionB); !ok {
		t.Error("the sweep killed a live token")
	}
}

// TestResolveStreamTokenNamesItsSession: the path routes get the session FROM the
// token (the URL carries no session id), so resolution must report both halves —
// and must refuse an unknown secret rather than resolving to something.
func TestResolveStreamTokenNamesItsSession(t *testing.T) {
	svc, _, admin, _ := newStreamFixture(t)
	grant := mustMint(t, svc, sessionA, admin.ID)

	sessionID, userID, ok := svc.ResolveStreamToken(grant.Token)
	if !ok {
		t.Fatal("ResolveStreamToken refused a live token")
	}
	if sessionID != sessionA || userID != admin.ID {
		t.Errorf("resolved (%q, %q), want (%q, %q)", sessionID, userID, sessionA, admin.ID)
	}
	for _, bad := range []string{"", "not-a-token", strings.Repeat("A", 43)} {
		if _, _, ok := svc.ResolveStreamToken(bad); ok {
			t.Errorf("ResolveStreamToken(%q) resolved; want refused", bad)
		}
	}
}

// TestVerifyStreamTokenRefusesGarbage: an empty or unknown secret, and an empty
// session id, are all plain refusals — never a panic, never a store error handed
// up for a transport to render.
func TestVerifyStreamTokenRefusesGarbage(t *testing.T) {
	svc, _, admin, _ := newStreamFixture(t)
	grant := mustMint(t, svc, sessionA, admin.ID)

	cases := []struct{ raw, session string }{
		{"", sessionA},
		{grant.Token, ""},
		{"not-a-token", sessionA},
		{strings.ToUpper(grant.Token), sessionA}, // a case-mangled copy is a different secret
	}
	for _, c := range cases {
		if _, ok := svc.VerifyStreamToken(c.raw, c.session); ok {
			t.Errorf("VerifyStreamToken(%q, %q) accepted", c.raw, c.session)
		}
	}
}

// TestMintStreamTokenRequiresSessionAndUser: minting without a session or a User
// is a programming error, refused rather than recorded — an orphan row would be a
// credential nobody can revoke.
func TestMintStreamTokenRequiresSessionAndUser(t *testing.T) {
	svc, _, admin, db := newStreamFixture(t)
	if _, err := svc.MintStreamToken("", admin.ID); err == nil {
		t.Error("minting with no session id succeeded")
	}
	if _, err := svc.MintStreamToken(sessionA, ""); err == nil {
		t.Error("minting with no user id succeeded")
	}
	if n := countStreamTokens(t, db, sessionA); n != 0 {
		t.Errorf("rows written by a refused mint = %d, want 0", n)
	}
}

// TestDeletingAUserRevokesTheirStreamTokens: user_id cascades, so removing an
// account takes its outstanding media credentials with it rather than leaving a
// live URL pointing at a deleted User's session.
func TestDeletingAUserRevokesTheirStreamTokens(t *testing.T) {
	svc, _, admin, db := newStreamFixture(t)
	member, err := svc.CreateUser(context.Background(), "member", "member-password-long", auth.RoleMember)
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	memberGrant := mustMint(t, svc, sessionB, member.ID)
	adminGrant := mustMint(t, svc, sessionA, admin.ID)

	if err := svc.DeleteUser(member.ID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if n := countStreamTokens(t, db, sessionB); n != 0 {
		t.Errorf("rows left for the deleted User = %d, want 0", n)
	}
	if _, ok := svc.VerifyStreamToken(memberGrant.Token, sessionB); ok {
		t.Error("a deleted User's stream token still verifies")
	}
	if _, ok := svc.VerifyStreamToken(adminGrant.Token, sessionA); !ok {
		t.Error("deleting one User revoked another's stream token")
	}
}

// TestStreamTokenNeverReachesALogLine is the hard constraint, checked rather than
// assumed. The secret's whole risk model is "it will be written down somewhere we
// do not control" — this server must not be one of those places. It exercises
// mint, a good verify, a wrong-session verify, an expired verify, and a resolve,
// with the package logger captured throughout.
func TestStreamTokenNeverReachesALogLine(t *testing.T) {
	svc, clock, admin, _ := newStreamFixture(t)

	var logged bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&logged)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})

	grant := mustMint(t, svc, sessionA, admin.ID)
	svc.VerifyStreamToken(grant.Token, sessionA)
	svc.VerifyStreamToken(grant.Token, sessionB)
	svc.ResolveStreamToken(grant.Token)
	svc.VerifyStreamToken("not-a-token", sessionA)
	clock.advance(grant.ExpiresIn + time.Second)
	svc.VerifyStreamToken(grant.Token, sessionA)
	if err := svc.RevokeStreamTokens(sessionA); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	if strings.Contains(logged.String(), grant.Token) {
		t.Fatalf("the raw stream token appeared in a log line:\n%s", logged.String())
	}
}
