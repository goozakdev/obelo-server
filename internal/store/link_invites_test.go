package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/store"
)

// The link_invites table (ADR-0055 §1). What is asserted here is the SQL,
// because that is where the rules live: single use and expiry are both WHERE
// clauses on one UPDATE, and a test that went through the service would be
// asserting the service's ordering rather than the statement's.

// seedRemote inserts a `remote` User — no password, which is the one state the
// schema's CHECK admits for the role.
func seedRemote(t *testing.T, db *store.DB, id string) {
	t.Helper()
	mustExec(t, db,
		`INSERT INTO users (id, username, role, password_hash) VALUES (?, ?, 'remote', '')`,
		id, id)
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func TestLinkInviteRedeemsExactlyOnce(t *testing.T) {
	db := openTemp(t)
	seedRemote(t, db, "peer")

	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	if err := db.InsertLinkInvite(store.LinkInvite{
		CodeHash:  "hash-1",
		UserID:    "peer",
		CreatedAt: rfc3339(now),
		ExpiresAt: rfc3339(now.Add(24 * time.Hour)),
	}); err != nil {
		t.Fatalf("InsertLinkInvite: %v", err)
	}

	got, err := db.RedeemLinkInvite("hash-1", rfc3339(now.Add(time.Minute)))
	if err != nil {
		t.Fatalf("RedeemLinkInvite: %v", err)
	}
	if got.UserID != "peer" {
		t.Errorf("redeemed invite belongs to %q, want peer", got.UserID)
	}
	if got.RedeemedAt == "" {
		t.Error("redeemed_at was not stamped")
	}

	// The compare-and-swap is what makes it one-shot: the second UPDATE matches
	// nothing, so no second session can ever be minted from this code.
	if _, err := db.RedeemLinkInvite("hash-1", rfc3339(now.Add(2*time.Minute))); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second redeem: err = %v, want ErrNotFound", err)
	}

	// The spent row is KEPT until the sweeper reaps it — it is the record that a
	// Link was established.
	if _, err := db.LinkInviteByCodeHash("hash-1"); err != nil {
		t.Errorf("the spent row was deleted on collection: %v", err)
	}
}

func TestLinkInviteExpiryIsAWhereClause(t *testing.T) {
	db := openTemp(t)
	seedRemote(t, db, "peer")

	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	expires := now.Add(24 * time.Hour)
	if err := db.InsertLinkInvite(store.LinkInvite{
		CodeHash:  "hash-1",
		UserID:    "peer",
		CreatedAt: rfc3339(now),
		ExpiresAt: rfc3339(expires),
	}); err != nil {
		t.Fatalf("InsertLinkInvite: %v", err)
	}

	// One second past the expiry. Note both operands are RFC3339-UTC: the
	// comparison is a plain string compare in SQL, which breaks if the two formats
	// are ever mixed.
	if _, err := db.RedeemLinkInvite("hash-1", rfc3339(expires.Add(time.Second))); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("redeem past expiry: err = %v, want ErrNotFound", err)
	}
	// The row is still there — nothing swept it — so expiry is enforced by the
	// statement rather than by the reaper having run.
	if _, err := db.LinkInviteByCodeHash("hash-1"); err != nil {
		t.Fatalf("the unswept expired row is gone: %v", err)
	}
	// And it is still good one second before.
	if _, err := db.RedeemLinkInvite("hash-1", rfc3339(expires.Add(-time.Second))); err != nil {
		t.Errorf("redeem one second before expiry: %v", err)
	}
}

func TestDeleteUnredeemedLinkInvitesSparesSpentOnes(t *testing.T) {
	db := openTemp(t)
	seedRemote(t, db, "peer")
	seedRemote(t, db, "other")

	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	insert := func(hash, user string) {
		t.Helper()
		if err := db.InsertLinkInvite(store.LinkInvite{
			CodeHash:  hash,
			UserID:    user,
			CreatedAt: rfc3339(now),
			ExpiresAt: rfc3339(now.Add(24 * time.Hour)),
		}); err != nil {
			t.Fatalf("InsertLinkInvite(%s): %v", hash, err)
		}
	}
	insert("spent", "peer")
	insert("live", "peer")
	insert("elsewhere", "other")
	if _, err := db.RedeemLinkInvite("spent", rfc3339(now.Add(time.Minute))); err != nil {
		t.Fatalf("redeem: %v", err)
	}

	if err := db.DeleteUnredeemedLinkInvites("peer"); err != nil {
		t.Fatalf("DeleteUnredeemedLinkInvites: %v", err)
	}
	if _, err := db.LinkInviteByCodeHash("live"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the outstanding invite survived a re-mint: err = %v", err)
	}
	if _, err := db.LinkInviteByCodeHash("spent"); err != nil {
		t.Errorf("a spent invite was swept by a re-mint: %v", err)
	}
	if _, err := db.LinkInviteByCodeHash("elsewhere"); err != nil {
		t.Errorf("another User's invite was swept: %v", err)
	}
}

func TestDeleteExpiredLinkInvitesReapsBothStates(t *testing.T) {
	db := openTemp(t)
	seedRemote(t, db, "peer")

	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	old := store.LinkInvite{
		CodeHash: "old", UserID: "peer",
		CreatedAt: rfc3339(now.Add(-48 * time.Hour)),
		ExpiresAt: rfc3339(now.Add(-24 * time.Hour)),
	}
	fresh := store.LinkInvite{
		CodeHash: "fresh", UserID: "peer",
		CreatedAt: rfc3339(now),
		ExpiresAt: rfc3339(now.Add(24 * time.Hour)),
	}
	for _, inv := range []store.LinkInvite{old, fresh} {
		if err := db.InsertLinkInvite(inv); err != nil {
			t.Fatalf("InsertLinkInvite: %v", err)
		}
	}

	if err := db.DeleteExpiredLinkInvites(rfc3339(now)); err != nil {
		t.Fatalf("DeleteExpiredLinkInvites: %v", err)
	}
	if _, err := db.LinkInviteByCodeHash("old"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the expired invite survived the sweep: err = %v", err)
	}
	if _, err := db.LinkInviteByCodeHash("fresh"); err != nil {
		t.Errorf("the live invite was swept: %v", err)
	}
}

func TestDeletingTheUserCascadesToItsInvites(t *testing.T) {
	db := openTemp(t)
	seedRemote(t, db, "peer")

	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	if err := db.InsertLinkInvite(store.LinkInvite{
		CodeHash: "hash-1", UserID: "peer",
		CreatedAt: rfc3339(now), ExpiresAt: rfc3339(now.Add(24 * time.Hour)),
	}); err != nil {
		t.Fatalf("InsertLinkInvite: %v", err)
	}
	if err := db.DeleteUser("peer"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	// The sharer's kill switch must take unspent codes with it — otherwise a
	// string in a chat log outlives the User it was minted for.
	if _, err := db.LinkInviteByCodeHash("hash-1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the invite outlived its User: err = %v", err)
	}
}
