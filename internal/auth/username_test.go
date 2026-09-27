package auth_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/auth"
	"github.com/goozakdev/obelo-server/internal/store"
)

// The username rule, one for every User whoever names it — an Admin creating
// one, the first Admin at setup, or a Sign-in provider at a first sign-in: 1 to
// 64 characters, no control or zero-width character, stored in Unicode NFC, and
// unique regardless of case.

// usernamesBreakingTheRule are refused wherever a username is chosen.
var usernamesBreakingTheRule = map[string]string{
	"empty":                   "",
	"65 characters":           strings.Repeat("é", 65),
	"a NUL":                   "ad\x00a",
	"a tab":                   "ad\ta",
	"a newline":               "ad\na",
	"a DEL":                   "ad\x7fa",
	"a C1 control":            "ad\u0085a",
	"a zero-width space":      "ad\u200ba",
	"a zero-width joiner":     "ad\u200da",
	"a zero-width non-joiner": "ad\u200ca",
	"a word joiner":           "ad\u2060a",
	"a byte-order mark":       "\ufeffada",

	// Every other invisible format or default-ignorable character, and the
	// fillers that print as a blank.
	"a left-to-right mark":        "ad\u200ea",
	"a right-to-left mark":        "ad\u200fa",
	"a right-to-left override":    "ad\u202ea",
	"a left-to-right isolate":     "ad\u2066a",
	"a soft hyphen":               "ad\u00ada",
	"a function application":      "ad\u2061a",
	"a Mongolian vowel separator": "ad\u180ea",
	"a combining grapheme joiner": "ad\u034fa",
	"a variation selector":        "ad\ufe0fa",
	"a tag character":             "ad\U000e0041a",
	"a Hangul filler":             "ad\u3164a",
	"a Hangul choseong filler":    "ad\u115fa",
	"a Hangul jungseong filler":   "ad\u1160a",
	"a halfwidth Hangul filler":   "ad\uffa0a",
	"a blank Braille pattern":     "ad\u2800a",
	"a private-use character":     "ad\ue000a",
	"an unassigned code point":    "ad\U0003fffea",
	"only surrounding whitespace": "  \t ",
}

// TestAUsernameBreakingTheRuleIsRefused: an Admin creating a User with a
// username outside the rule is refused, and nothing is created; 64 characters —
// counted as characters, not bytes — is allowed.
func TestAUsernameBreakingTheRuleIsRefused(t *testing.T) {
	for name, username := range usernamesBreakingTheRule {
		t.Run(name, func(t *testing.T) {
			svc, db := newRemoteFixture(t)
			if _, err := svc.CreateUser(context.Background(), username, "pw", auth.RoleMember); !errors.Is(err, auth.ErrInvalidUser) {
				t.Fatalf("CreateUser(%q) = %v, want ErrInvalidUser", username, err)
			}
			if users, err := db.ListUsers(); err != nil || len(users) != 1 {
				t.Fatalf("users = %+v (err %v), want only the admin", users, err)
			}
		})
	}
	svc, _ := newRemoteFixture(t)
	long := strings.Repeat("é", 64)
	if u, err := svc.CreateUser(context.Background(), long, "pw", auth.RoleMember); err != nil || u.Username != long {
		t.Fatalf("CreateUser(64 characters) = %q, %v; want it created", u.Username, err)
	}
}

// TestSetupHoldsTheFirstAdminToTheUsernameRule: the first Admin's username
// is held to the same rule, and nothing is created when it breaks it.
func TestSetupHoldsTheFirstAdminToTheUsernameRule(t *testing.T) {
	for _, username := range []string{strings.Repeat("a", 65), "ad\u200ba", "ad\x00a"} {
		svc, db := newUnsetFixture(t)
		if _, err := svc.Setup(context.Background(), svc.ClaimToken(), username, "correct-horse-battery"); !errors.Is(err, auth.ErrInvalidUser) {
			t.Fatalf("Setup(%q) = %v, want ErrInvalidUser", username, err)
		}
		if n, err := db.CountUsers(); err != nil || n != 0 {
			t.Fatalf("users after a refused setup = %d (err %v), want 0", n, err)
		}
	}
}

// TestAUsernameIsStoredInNFC: "é" typed as e plus a combining accent is stored
// as the one precomposed character, by setup and by an Admin alike, and signs in
// typed either way.
func TestAUsernameIsStoredInNFC(t *testing.T) {
	const decomposed, composed = "Rene\u0301", "René"

	svc, db := newUnsetFixture(t)
	admin, err := svc.Setup(context.Background(), svc.ClaimToken(), decomposed, "correct-horse-battery")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if admin.Username != composed {
		t.Fatalf("setup stored %q, want the NFC %q", admin.Username, composed)
	}
	for _, typed := range []string{decomposed, composed} {
		if _, err := svc.Login(context.Background(), typed, "correct-horse-battery", laptop, ""); err != nil {
			t.Fatalf("login typed %q: %v", typed, err)
		}
	}

	u, err := svc.CreateUser(context.Background(), "Zoe\u0308", "pw", auth.RoleMember)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if u.Username != "Zoë" {
		t.Fatalf("CreateUser stored %q, want the NFC %q", u.Username, "Zoë")
	}
	if _, err := db.UserByUsername("Zoë"); err != nil {
		t.Fatalf("the NFC username is not what is stored: %v", err)
	}
}

// TestLocalUsernamesAreUniqueRegardlessOfCase: a username differing from one
// already held only in case — or in its Unicode form — is taken, under full
// Unicode case folding: "STRASSE" and "straße" are one name, which a simple
// one-rune-for-one-rune fold does not see.
func TestLocalUsernamesAreUniqueRegardlessOfCase(t *testing.T) {
	for _, tc := range []struct{ held, second string }{
		{"admin", "Admin"},
		{"admin", "ADMIN"},
		{"straße", "STRASSE"},
		{"René", "RENE\u0301"},
		{"ǅemal", "ǆemal"},
		{"victor", "ｖｉｃｔｏｒ"},
		{"ｖｉｃｔｏｒ", "VICTOR"},
		{"ﬁle", "FILE"},
	} {
		t.Run(tc.held+"/"+tc.second, func(t *testing.T) {
			svc, db := newRemoteFixture(t)
			if tc.held != "admin" {
				if _, err := svc.CreateUser(context.Background(), tc.held, "pw", auth.RoleMember); err != nil {
					t.Fatalf("CreateUser(%q): %v", tc.held, err)
				}
			}
			before, _ := db.ListUsers()
			if _, err := svc.CreateUser(context.Background(), tc.second, "pw", auth.RoleMember); !errors.Is(err, auth.ErrUsernameTaken) {
				t.Fatalf("CreateUser(%q) with %q held = %v, want ErrUsernameTaken", tc.second, tc.held, err)
			}
			if after, _ := db.ListUsers(); len(after) != len(before) {
				t.Fatalf("users = %d after a taken username, want %d", len(after), len(before))
			}
		})
	}
}

// TestAProviderUsernameIsHeldToTheRuleAtFirstSignIn: a first sign-in whose
// provider names a username outside the rule mints nobody and links nothing —
// it is refused as an answer the Server cannot act on.
func TestAProviderUsernameIsHeldToTheRuleAtFirstSignIn(t *testing.T) {
	for _, username := range []string{strings.Repeat("a", 65), "ad\u200ba", "ad\x1ba"} {
		svc, db := newRemoteFixture(t)
		svc.UseSignInProviders(providerList{directory("dir", "someone", "dir-pw",
			auth.ExternalAnswer{Subject: "s-1", Username: username})})
		if _, err := svc.Login(context.Background(), "someone", "dir-pw", laptop, ""); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("login with the provider naming %q = %v, want ErrInvalidCredentials", username, err)
		}
		if users, err := db.ListUsers(); err != nil || len(users) != 1 {
			t.Fatalf("users = %+v (err %v), want only the admin", users, err)
		}
		if _, err := db.ExternalIdentityUser("dir", "s-1"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("the identity resolves (err %v); it must be linked to nobody", err)
		}
	}
	svc, _ := newRemoteFixture(t)
	if _, err := svc.SignInExternal("dir", auth.ExternalAnswer{Subject: "s-1", Username: "ad\u200ba"}, laptop); !errors.Is(err, auth.ErrExternalUsernameInvalid) {
		t.Fatalf("redirect first sign-in naming a zero-width username = %v, want ErrExternalUsernameInvalid", err)
	}
}

// TestAProviderUsernameIsStoredInNFCAndCollidesUnderFullFolding: the Member
// a first sign-in mints holds the NFC form of what the provider named, and a
// provider's "STRASSE" collides with a local "straße".
func TestAProviderUsernameIsStoredInNFCAndCollidesUnderFullFolding(t *testing.T) {
	svc, _ := newRemoteFixture(t)
	svc.UseSignInProviders(providerList{directory("dir", "zoe", "pw",
		auth.ExternalAnswer{Subject: "s-zoe", Username: "Zoe\u0308"})})
	res, err := svc.Login(context.Background(), "zoe", "pw", laptop, "")
	if err != nil {
		t.Fatalf("first sign-in: %v", err)
	}
	if res.User.Username != "Zoë" {
		t.Fatalf("the new Member is %q, want the NFC %q", res.User.Username, "Zoë")
	}

	svc, _ = newRemoteFixture(t)
	if _, err := svc.CreateUser(context.Background(), "straße", "pw", auth.RoleMember); err != nil {
		t.Fatal(err)
	}
	svc.UseSignInProviders(providerList{directory("dir", "someone", "dir-pw",
		auth.ExternalAnswer{Subject: "s-1", Username: "STRASSE"})})
	if _, err := svc.Login(context.Background(), "someone", "dir-pw", laptop, ""); !errors.Is(err, auth.ErrUsernameCollision) {
		t.Fatalf("provider STRASSE with local straße = %v, want ErrUsernameCollision", err)
	}
}

// newUnsetFixture is a real store and a Service with nobody set up yet.
func newUnsetFixture(t *testing.T) (*auth.Service, *store.DB) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	svc, err := auth.NewService(db)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc, db
}

// TestAUsernameStoredBeforeTheRuleStillSignsIn: usernames already stored are not
// re-judged, so one stored in another form than NFC still signs in typed as it
// was stored.
func TestAUsernameStoredBeforeTheRuleStillSignsIn(t *testing.T) {
	_, db := newRemoteFixture(t)
	hash, err := auth.HashPassword("pw")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateUser("legacy", "René", auth.RoleMember, hash); err != nil {
		t.Fatal(err)
	}
	svc, err := auth.NewService(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Login(context.Background(), "René", "pw", laptop, ""); err != nil {
		t.Fatalf("login as the username stored before the rule: %v", err)
	}
}

// TestTheLoginLimitCountsAUsernameInItsNFCForm: failures typed in one form of a
// name count against the name, so typing it another way buys no more guesses.
func TestTheLoginLimitCountsAUsernameInItsNFCForm(t *testing.T) {
	svc, _, _ := newFixture(t)
	if _, err := svc.CreateUser(context.Background(), "René", loginPassword, auth.RoleMember); err != nil {
		t.Fatal(err)
	}
	burnUntilThrottled(t, svc, func(i int) (string, string) {
		return "René", fmt.Sprintf("198.51.100.%d", i+1)
	})
	if _, err := svc.Login(context.Background(), "René", loginPassword, testDevice(), victimIP); !errors.Is(err, auth.ErrTooManyLoginAttempts) {
		t.Fatalf("the name typed precomposed after its decomposed form tripped = %v, want ErrTooManyLoginAttempts", err)
	}
}

// TestALocalUsernameIsTrimmedOfSurroundingWhitespace: setup and an Admin store
// a local username without the whitespace around it, so " kid" is "kid" —
// taken when "kid" is held — and it signs in typed either way.
func TestALocalUsernameIsTrimmedOfSurroundingWhitespace(t *testing.T) {
	svc, db := newUnsetFixture(t)
	admin, err := svc.Setup(context.Background(), svc.ClaimToken(), " boss\t", "correct-horse-battery")
	if err != nil || admin.Username != "boss" {
		t.Fatalf("setup stored %q (err %v), want %q", admin.Username, err, "boss")
	}
	u, err := svc.CreateUser(context.Background(), "\u3000kid ", "pw", auth.RoleMember)
	if err != nil || u.Username != "kid" {
		t.Fatalf("CreateUser stored %q (err %v), want %q", u.Username, err, "kid")
	}
	if _, err := svc.CreateUser(context.Background(), " kid", "pw", auth.RoleMember); !errors.Is(err, auth.ErrUsernameTaken) {
		t.Fatalf("CreateUser(\" kid\") with kid held = %v, want ErrUsernameTaken", err)
	}
	if n, _ := db.CountUsers(); n != 2 {
		t.Fatalf("users = %d, want 2", n)
	}
	if _, err := svc.Login(context.Background(), "  kid ", "pw", laptop, ""); err != nil {
		t.Fatalf("login typed with surrounding whitespace: %v", err)
	}
}

// TestSignInFindsTheUserRegardlessOfCase: sign-in matches the typed username by
// the key uniqueness uses, so "ADA", "ada" and the fullwidth "ＡＤＡ" all sign
// in the User "Ada" — and the wrong password is still wrong.
func TestSignInFindsTheUserRegardlessOfCase(t *testing.T) {
	svc, _ := newRemoteFixture(t)
	if _, err := svc.CreateUser(context.Background(), "Ada", "pw", auth.RoleMember); err != nil {
		t.Fatal(err)
	}
	for _, typed := range []string{"Ada", "ada", "ADA", "ＡＤＡ"} {
		res, err := svc.Login(context.Background(), typed, "pw", laptop, "")
		if err != nil || res.User.Username != "Ada" {
			t.Fatalf("login typed %q = %q, %v; want Ada signed in", typed, res.User.Username, err)
		}
	}
	if _, err := svc.Login(context.Background(), "ada", "wrong", laptop, ""); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("login with the wrong password = %v, want ErrInvalidCredentials", err)
	}
}

// TestAnAmbiguousUsernameSignsInOnlyItsExactForm: two Users stored before the
// rule may share one key; the one typed exactly signs in, and a form matching
// neither exactly signs in nobody.
func TestAnAmbiguousUsernameSignsInOnlyItsExactForm(t *testing.T) {
	_, db := newRemoteFixture(t)
	for _, name := range []string{"Ada", "ada"} {
		hash, err := auth.HashPassword("pw-" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO users (id, username, role, password_hash) VALUES (?, ?, 'member', ?)`,
			"legacy-"+name, name, hash); err != nil {
			t.Fatal(err)
		}
	}
	svc, err := auth.NewService(db)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Ada", "ada"} {
		res, err := svc.Login(context.Background(), name, "pw-"+name, laptop, "")
		if err != nil || res.User.Username != name {
			t.Fatalf("login typed %q = %q, %v; want %q signed in", name, res.User.Username, err, name)
		}
	}
	for _, pw := range []string{"pw-Ada", "pw-ada"} {
		if _, err := svc.Login(context.Background(), "ADA", pw, laptop, ""); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("login typed ADA with %s = %v, want ErrInvalidCredentials", pw, err)
		}
	}
}

// TestTheLoginLimitCountsAUsernameRegardlessOfCase: failures typed in one case
// count against the name, so typing it in another buys no more guesses.
func TestTheLoginLimitCountsAUsernameRegardlessOfCase(t *testing.T) {
	svc, _, _ := newFixture(t)
	if _, err := svc.CreateUser(context.Background(), "victor", loginPassword, auth.RoleMember); err != nil {
		t.Fatal(err)
	}
	burnUntilThrottled(t, svc, func(i int) (string, string) {
		return []string{"victor", "VICTOR", "Victor", "ｖｉｃｔｏｒ"}[i%4], fmt.Sprintf("198.51.100.%d", i+1)
	})
	if _, err := svc.Login(context.Background(), " VicTor ", loginPassword, testDevice(), victimIP); !errors.Is(err, auth.ErrTooManyLoginAttempts) {
		t.Fatalf("the name in another case after its other cases tripped = %v, want ErrTooManyLoginAttempts", err)
	}
}

// TestAProviderIsAskedAboutTheUsernameAsTyped: the lookup folds and trims, but a
// Sign-in provider is handed exactly what was typed.
func TestAProviderIsAskedAboutTheUsernameAsTyped(t *testing.T) {
	svc, _ := newRemoteFixture(t)
	svc.UseSignInProviders(providerList{directory("dir", " Someone ", "dir-pw",
		auth.ExternalAnswer{Subject: "s-1", Username: "someone"})})
	if _, err := svc.Login(context.Background(), " Someone ", "dir-pw", laptop, ""); err != nil {
		t.Fatalf("login with the provider expecting the typed form: %v", err)
	}
}

// setupRacedStore reports no Users, as a store does to a setup that checked
// before a racing one inserted.
type setupRacedStore struct{ *store.DB }

func (setupRacedStore) CountUsers() (int, error) { return 0, nil }

// TestASetupLosingARaceForTheUsernameIsTaken: a setup whose username a racing
// insert took first is ErrUsernameTaken, not a store failure.
func TestASetupLosingARaceForTheUsernameIsTaken(t *testing.T) {
	_, db := newUnsetFixture(t)
	if _, err := db.CreateAdmin("first", "admin", "x"); err != nil {
		t.Fatal(err)
	}
	svc, err := auth.NewService(setupRacedStore{db})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Setup(context.Background(), svc.ClaimToken(), "Admin", "correct-horse-battery"); !errors.Is(err, auth.ErrUsernameTaken) {
		t.Fatalf("setup losing the race = %v, want ErrUsernameTaken", err)
	}
}
