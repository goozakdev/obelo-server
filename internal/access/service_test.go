package access

import (
	"errors"
	"sort"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// fakeStore is a minimal access.Store for resolver/grant tests.
type fakeStore struct {
	users    map[string]store.User
	grants   map[string][]string // userID -> granted library ids
	ceilings map[string]string   // userID -> ceiling label
	libs     map[string]bool     // existing library ids (grant validation)
	// linked is the subset of libs that arrived over a Link (ADR-0056 §1) — the
	// Libraries a `remote` User may never be granted and never resolves.
	linked map[string]bool
	// play is the per-User Playback ceiling (ADR-0054 §2); an absent entry is the
	// zero value, i.e. uncapped in all three dimensions.
	play map[string]store.PlaybackCeiling
}

func (f *fakeStore) UserByID(id string) (store.User, error) {
	u, ok := f.users[id]
	if !ok {
		return store.User{}, store.ErrNotFound
	}
	return u, nil
}

func (f *fakeStore) LibraryAccessForUser(userID string) ([]string, error) {
	return f.grants[userID], nil
}

func (f *fakeStore) ReplaceLibraryAccess(userID string, libraryIDs []string) error {
	for _, l := range libraryIDs {
		if !f.libs[l] {
			return store.ErrNotFound
		}
	}
	f.grants[userID] = append([]string{}, libraryIDs...)
	return nil
}

func (f *fakeStore) RatingCeilingForUser(userID string) (string, error) {
	return f.ceilings[userID], nil
}

func (f *fakeStore) SetRatingCeiling(userID, label string) error {
	if _, ok := f.users[userID]; !ok {
		return store.ErrNotFound
	}
	if label == "" {
		delete(f.ceilings, userID)
	} else {
		f.ceilings[userID] = label
	}
	return nil
}

func (f *fakeStore) PlaybackCeilingForUser(userID string) (store.PlaybackCeiling, error) {
	if _, ok := f.users[userID]; !ok {
		return store.PlaybackCeiling{}, store.ErrNotFound
	}
	return f.play[userID], nil
}

func (f *fakeStore) SetPlaybackCeiling(userID string, c store.PlaybackCeiling) error {
	if _, ok := f.users[userID]; !ok {
		return store.ErrNotFound
	}
	f.play[userID] = c
	return nil
}

func (f *fakeStore) LinkedLibraryIDs() ([]string, error) {
	out := make([]string, 0, len(f.linked))
	for id := range f.linked {
		if f.linked[id] {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out, nil
}

// newFake: two local Libraries and one ("mirror") that arrived over a Link,
// plus one User of each role.
func newFake() *fakeStore {
	return &fakeStore{
		users: map[string]store.User{
			"a": {ID: "a", Role: "admin"},
			"m": {ID: "m", Role: "member"},
			"r": {ID: "r", Role: "remote"},
		},
		grants:   map[string][]string{},
		ceilings: map[string]string{},
		libs:     map[string]bool{"l1": true, "l2": true, "mirror": true},
		linked:   map[string]bool{"mirror": true},
		play:     map[string]store.PlaybackCeiling{},
	}
}

// TestResolveAdminIsAllAccess: an Admin resolves to all Libraries by role, with
// no grant rows consulted.
func TestResolveAdminIsAllAccess(t *testing.T) {
	sc, err := NewService(newFake()).Resolve("a")
	if err != nil {
		t.Fatalf("Resolve(admin): %v", err)
	}
	if !sc.IsAdmin || !sc.AllLibraries {
		t.Errorf("admin scope = %+v, want IsAdmin + AllLibraries", sc)
	}
	if !sc.AllowsLibrary("any") {
		t.Error("admin must see any Library")
	}
}

// TestResolveMemberIsGrantedSet: a Member resolves to exactly their granted set
// — visible for granted Libraries, hidden for the rest; no grants → sees nothing.
func TestResolveMemberIsGrantedSet(t *testing.T) {
	fs := newFake()
	fs.grants["m"] = []string{"l1"}
	sc, err := NewService(fs).Resolve("m")
	if err != nil {
		t.Fatalf("Resolve(member): %v", err)
	}
	if sc.IsAdmin || sc.AllLibraries {
		t.Errorf("member scope = %+v, want neither IsAdmin nor AllLibraries", sc)
	}
	if !sc.AllowsLibrary("l1") {
		t.Error("member must see a granted Library")
	}
	if sc.AllowsLibrary("l2") {
		t.Error("member must NOT see an ungranted Library")
	}

	// No grants → empty scope, sees nothing (not an error).
	sc, err = NewService(newFake()).Resolve("m")
	if err != nil {
		t.Fatalf("Resolve(member, no grants): %v", err)
	}
	if sc.AllowsLibrary("l1") {
		t.Error("a Member with no grants must see no Library")
	}
}

// TestResolveUnknownUserErrors: an unknown id surfaces the store error (mapped to
// a 500), not a silent all-access Scope.
func TestResolveUnknownUserErrors(t *testing.T) {
	if _, err := NewService(newFake()).Resolve("nope"); err == nil {
		t.Fatal("Resolve(unknown) returned nil error, want the store error")
	}
}

// TestSetLibraryAccess covers the grant-management rules: a Member's set is
// replaced; an Admin target, unknown User, and unknown library are each rejected.
func TestSetLibraryAccess(t *testing.T) {
	fs := newFake()
	svc := NewService(fs)

	if err := svc.SetLibraryAccess("m", []string{"l1", "l2"}); err != nil {
		t.Fatalf("grant to member: %v", err)
	}
	if got := fs.grants["m"]; len(got) != 2 {
		t.Errorf("member grants = %v, want [l1 l2]", got)
	}

	if err := svc.SetLibraryAccess("a", []string{"l1"}); !errors.Is(err, ErrAdminGrant) {
		t.Errorf("grant to admin err = %v, want ErrAdminGrant", err)
	}
	if err := svc.SetLibraryAccess("ghost", []string{"l1"}); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("grant to unknown user err = %v, want ErrUserNotFound", err)
	}
	if err := svc.SetLibraryAccess("m", []string{"l1", "nope"}); !errors.Is(err, ErrUnknownLibrary) {
		t.Errorf("grant with unknown library err = %v, want ErrUnknownLibrary", err)
	}
}

// TestSetLibraryAccessRefusesALinkedLibraryForARemoteUser: a Library that
// arrived over a Link is never re-shared (ADR-0054 §4). The whole set is
// refused, the prior set stands, and the same Library granted to a MEMBER is
// ordinary business.
func TestSetLibraryAccessRefusesALinkedLibraryForARemoteUser(t *testing.T) {
	fs := newFake()
	svc := NewService(fs)

	if err := svc.SetLibraryAccess("r", []string{"l1"}); err != nil {
		t.Fatalf("granting a local library to a remote user: %v", err)
	}
	if err := svc.SetLibraryAccess("r", []string{"l2", "mirror"}); !errors.Is(err, ErrLinkedGrant) {
		t.Errorf("granting a mirror to a remote user = %v, want ErrLinkedGrant", err)
	}
	// Rejected whole: the l2 in the same set was not applied either.
	if got := fs.grants["r"]; len(got) != 1 || got[0] != "l1" {
		t.Errorf("after the refusal, remote grants = %v, want [l1] (unchanged)", got)
	}
	// A Member is unaffected — the second hop is what is refused, not the first.
	if err := svc.SetLibraryAccess("m", []string{"mirror"}); err != nil {
		t.Errorf("granting a mirror to a member: %v, want nil", err)
	}
}

// TestResolveDropsALinkedLibraryFromARemoteScope: the invariant holds where the
// Scope is COMPUTED, not only where a grant is written — a row inserted behind
// the API (a restore, a hand-edit, a Library that became a mirror after it was
// granted) still resolves to nothing for a `remote` User, while the same row
// resolves normally for a Member.
func TestResolveDropsALinkedLibraryFromARemoteScope(t *testing.T) {
	fs := newFake()
	fs.grants["r"] = []string{"l1", "mirror"} // written behind SetLibraryAccess
	fs.grants["m"] = []string{"l1", "mirror"}

	sc, err := NewService(fs).Resolve("r")
	if err != nil {
		t.Fatalf("Resolve(remote): %v", err)
	}
	if sc.AllowsLibrary("mirror") {
		t.Error("a remote User's Scope allows a linked Library; sharing does not travel")
	}
	if !sc.AllowsLibrary("l1") {
		t.Errorf("a remote User's Scope lost its local grant: %v", sc.LibraryIDs)
	}
	if len(sc.LibraryIDs) != 1 {
		t.Errorf("remote LibraryIDs = %v, want just [l1]", sc.LibraryIDs)
	}

	member, err := NewService(fs).Resolve("m")
	if err != nil {
		t.Fatalf("Resolve(member): %v", err)
	}
	if !member.AllowsLibrary("mirror") {
		t.Error("a Member lost their linked Library; only the remote role is capped")
	}
}

// TestResolveMemberCeiling: a Member's stored ceiling label resolves to its
// rank on the Scope; an Admin is always uncapped regardless of any stored value.
func TestResolveMemberCeiling(t *testing.T) {
	fs := newFake()
	fs.grants["m"] = []string{"l1"}
	fs.ceilings["m"] = "PG-13"
	sc, err := NewService(fs).Resolve("m")
	if err != nil {
		t.Fatalf("Resolve(member): %v", err)
	}
	if sc.RatingCeiling != 3 {
		t.Errorf("member ceiling rank = %d, want 3 (PG-13)", sc.RatingCeiling)
	}

	fs.ceilings["a"] = "G" // ignored for an Admin
	sc, _ = NewService(fs).Resolve("a")
	if sc.RatingCeiling != 0 {
		t.Errorf("admin ceiling rank = %d, want 0 (uncapped)", sc.RatingCeiling)
	}
}

// TestSetRatingCeiling covers the rules: set/clear a Member, reject an Admin
// target, an unknown label, and an unknown User.
func TestSetRatingCeiling(t *testing.T) {
	fs := newFake()
	svc := NewService(fs)

	if err := svc.SetRatingCeiling("m", "PG-13"); err != nil {
		t.Fatalf("set ceiling: %v", err)
	}
	if fs.ceilings["m"] != "PG-13" {
		t.Errorf("stored ceiling = %q, want PG-13", fs.ceilings["m"])
	}
	if err := svc.SetRatingCeiling("m", ""); err != nil { // clear
		t.Fatalf("clear ceiling: %v", err)
	}
	if _, ok := fs.ceilings["m"]; ok {
		t.Error("ceiling not cleared")
	}
	if err := svc.SetRatingCeiling("a", "G"); !errors.Is(err, ErrAdminCeiling) {
		t.Errorf("ceiling on admin err = %v, want ErrAdminCeiling", err)
	}
	if err := svc.SetRatingCeiling("m", "BOGUS"); !errors.Is(err, ErrUnknownRating) {
		t.Errorf("unknown label err = %v, want ErrUnknownRating", err)
	}
	if err := svc.SetRatingCeiling("ghost", "G"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("unknown user err = %v, want ErrUserNotFound", err)
	}
}

// TestAllowsLibrary covers the predicate directly.
func TestAllowsLibrary(t *testing.T) {
	if !(Scope{AllLibraries: true}).AllowsLibrary("anything") {
		t.Error("AllLibraries scope must allow any Library")
	}
	restricted := Scope{LibraryIDs: []string{"l1", "l2"}}
	if !restricted.AllowsLibrary("l1") || restricted.AllowsLibrary("l3") {
		t.Error("restricted scope must allow only its set")
	}
	if (Scope{}).AllowsLibrary("l1") {
		t.Error("zero-value scope must allow nothing (fail-closed)")
	}
}

// TestStoreFilterProjection: the Scope maps onto the persistence-side filter —
// libraries verbatim, and the ceiling rank expanded to its blocked-label set.
func TestStoreFilterProjection(t *testing.T) {
	s := Scope{AllLibraries: false, LibraryIDs: []string{"l1"}, RatingCeiling: 3}
	f := s.StoreFilter()
	if f.AllLibraries != s.AllLibraries || len(f.LibraryIDs) != 1 || f.LibraryIDs[0] != "l1" {
		t.Errorf("StoreFilter() libraries = %+v, want mirror of the Scope", f)
	}
	// RatingCeiling 3 (PG-13) → blocked {R, TV-MA, NC-17}.
	if len(f.BlockedRatings) != 3 {
		t.Errorf("StoreFilter() BlockedRatings = %v, want 3 above-ceiling labels", f.BlockedRatings)
	}
}
