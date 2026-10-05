package signin

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/auth"
	"github.com/goozakdev/obelo-server/internal/store"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The periodic re-check (ADR-0063 decisions 4 and 9), against a real store and
// a real auth.Service: the Group mapping it applies and the sessions it revokes
// are theirs, and asserting a fake of either would assert the fake.

// directory is one Sign-in provider as a re-check sees it. Its lookup answers
// come from answers, by subject; its refresh answers from refresh.
type directory struct {
	mu       sync.Mutex
	answers  map[string]pluginapi.SignInLookupResponse
	fail     bool
	failFor  map[string]bool
	refresh  func(token string) (pluginapi.SignInRefreshResponse, error)
	issuer   string
	clientID string
	declared bool
	disabled bool
	asked    int
}

func (d *directory) CheckPassword(context.Context, pluginapi.SignInPasswordRequest) (pluginapi.SignInPasswordResponse, error) {
	return pluginapi.SignInPasswordResponse{}, nil
}

func (d *directory) Lookup(_ context.Context, req pluginapi.SignInLookupRequest) (pluginapi.SignInLookupResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.asked++
	if d.fail || d.failFor[req.Subject] {
		return pluginapi.SignInLookupResponse{}, errors.New("the directory timed out")
	}
	if a, ok := d.answers[req.Subject]; ok {
		return a, nil
	}
	return pluginapi.SignInLookupResponse{Status: pluginapi.SignInGone}, nil
}

func (d *directory) Refresh(_ context.Context, req pluginapi.SignInRefreshRequest) (pluginapi.SignInRefreshResponse, error) {
	d.mu.Lock()
	d.asked++
	f := d.refresh
	d.mu.Unlock()
	return f(req.RefreshToken)
}

func (d *directory) IDTokenAudience() (string, string, bool) { return d.issuer, d.clientID, d.declared }

func (d *directory) set(subject string, status pluginapi.SignInStatus, groups ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.answers == nil {
		d.answers = map[string]pluginapi.SignInLookupResponse{}
	}
	d.answers[subject] = pluginapi.SignInLookupResponse{Status: status,
		Identity: &pluginapi.SignInIdentity{Subject: subject, Groups: groups}}
}

func (d *directory) timesAsked() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.asked
}

type recheckFixture struct {
	t    *testing.T
	db   *store.DB
	auth *auth.Service
	reg  *pluginapi.Registry
	r    *Rechecker
	at   time.Time
	logs *strings.Builder
}

func newRecheckFixture(t *testing.T) *recheckFixture {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	svc, err := auth.NewService(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Setup(context.Background(), svc.ClaimToken(), "admin", "correct-horse-battery"); err != nil {
		t.Fatal(err)
	}
	for _, lib := range []string{"films", "cartoons"} {
		if _, err := db.CreateLibrary(lib, lib, "movie", []store.LibraryRootInput{{ID: lib + "-root", Path: "/media/" + lib}}); err != nil {
			t.Fatal(err)
		}
	}
	f := &recheckFixture{t: t, db: db, auth: svc, reg: pluginapi.NewRegistry(), at: time.Now(), logs: &strings.Builder{}}
	f.r = NewRechecker(f.reg, db, svc)
	var mu sync.Mutex
	f.r.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return f.at }
	f.r.logf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(f.logs, format+"\n", args...)
	}
	return f
}

// provide registers d as the Sign-in provider slug, declaring caps.
func (f *recheckFixture) provide(slug string, d *directory, caps ...pluginapi.Capability) {
	f.reg.RegisterSignInProvider(pluginapi.SignInProviderRegistration{
		Descriptor: pluginapi.Descriptor{Slug: slug, Name: slug, Capabilities: caps},
		New: func(pluginapi.Settings) (pluginapi.SignInProvider, error) {
			d.mu.Lock()
			defer d.mu.Unlock()
			if d.disabled {
				return nil, errors.New("plugin " + slug + " is disabled after 3 consecutive failures")
			}
			return d, nil
		},
	})
}

func (f *recheckFixture) mapping(slug string, rules ...store.GroupMappingRule) {
	f.t.Helper()
	if err := f.db.SetGroupMapping(slug, rules); err != nil {
		f.t.Fatal(err)
	}
}

var kidsAndAdults = []store.GroupMappingRule{
	{Group: "kids", Role: auth.RoleMember, LibraryIDs: []string{"cartoons"}},
	{Group: "adults", Role: auth.RoleMember, LibraryIDs: []string{"films"}},
}

// signIn signs subject in through slug, on a device of its own, and answers
// the User and the session token.
func (f *recheckFixture) signIn(slug, subject, device string, answer auth.ExternalAnswer) (store.User, string) {
	f.t.Helper()
	answer.Subject = subject
	if answer.Username == "" {
		answer.Username = subject
	}
	res, err := f.auth.SignInExternal(slug, answer, auth.DeviceInput{Name: device, Platform: "test", ClientID: device})
	if err != nil {
		f.t.Fatalf("signing %s in through %s: %v", subject, slug, err)
	}
	return res.User, res.Token
}

func (f *recheckFixture) runAfter(d time.Duration) {
	f.t.Helper()
	f.at = time.Now().Add(d)
	if err := f.r.RunDue(context.Background()); err != nil {
		f.t.Fatalf("RunDue: %v", err)
	}
}

func (f *recheckFixture) grants(userID string) []string {
	f.t.Helper()
	got, err := f.db.LibraryAccessForUser(userID)
	if err != nil {
		f.t.Fatal(err)
	}
	if got == nil {
		got = []string{}
	}
	return got
}

func (f *recheckFixture) live(token string) bool {
	_, err := f.auth.Authenticate(token)
	return err == nil
}

// TestAPeriodicRecheckUpdatesASignedOutUserAfterTheInterval: cy signed in as a
// kid and signed out. The directory moves her to adults. Before 24 hours
// nothing is asked; after, her grants follow the directory with nobody signing
// in.
func TestAPeriodicRecheckUpdatesASignedOutUserAfterTheInterval(t *testing.T) {
	f := newRecheckFixture(t)
	dir := &directory{}
	f.provide("dir", dir, pluginapi.CapabilityPasswordSignIn, pluginapi.CapabilitySignInLookup)
	f.mapping("dir", kidsAndAdults...)
	cy, token := f.signIn("dir", "s-cy", "laptop", auth.ExternalAnswer{Groups: []string{"kids"}})
	if err := f.auth.Logout(token); err != nil {
		t.Fatal(err)
	}
	dir.set("s-cy", pluginapi.SignInActive, "adults")

	f.runAfter(23 * time.Hour)
	if dir.timesAsked() != 0 || !reflect.DeepEqual(f.grants(cy.ID), []string{"cartoons"}) {
		t.Fatalf("at 23 h: asked %d times, grants %v; want not asked and still cartoons", dir.timesAsked(), f.grants(cy.ID))
	}
	f.runAfter(DefaultRecheckInterval + time.Minute)
	if dir.timesAsked() != 1 {
		t.Fatalf("at 24 h the directory was asked %d times, want 1", dir.timesAsked())
	}
	if got := f.grants(cy.ID); !reflect.DeepEqual(got, []string{"films"}) {
		t.Fatalf("after the re-check cy's grants = %v, want films", got)
	}
}

// TestAPerPluginIntervalIsHonored: "dir" is re-checked every 2 hours by the
// Admin's say-so, "other" at the default. At 3 hours only dir has been asked.
func TestAPerPluginIntervalIsHonored(t *testing.T) {
	f := newRecheckFixture(t)
	dir, other := &directory{}, &directory{}
	f.provide("dir", dir, pluginapi.CapabilitySignInLookup)
	f.provide("other", other, pluginapi.CapabilitySignInLookup)
	f.mapping("dir", kidsAndAdults...)
	if err := f.db.SetRecheckInterval("dir", 2*time.Hour); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.r.Interval("other"); got != DefaultRecheckInterval {
		t.Fatalf("other's interval = %v, want the 24 h default", got)
	}
	cy, _ := f.signIn("dir", "s-cy", "laptop", auth.ExternalAnswer{Groups: []string{"kids"}})
	f.signIn("other", "s-dan", "phone", auth.ExternalAnswer{})
	dir.set("s-cy", pluginapi.SignInActive, "adults")
	other.set("s-dan", pluginapi.SignInActive)

	f.runAfter(time.Hour)
	if dir.timesAsked() != 0 {
		t.Fatalf("at 1 h dir was asked %d times, want 0", dir.timesAsked())
	}
	f.runAfter(3 * time.Hour)
	if dir.timesAsked() != 1 || other.timesAsked() != 0 {
		t.Fatalf("at 3 h dir asked %d, other asked %d; want 1 and 0", dir.timesAsked(), other.timesAsked())
	}
	if got := f.grants(cy.ID); !reflect.DeepEqual(got, []string{"films"}) {
		t.Fatalf("cy's grants = %v, want films", got)
	}
}

// TestAGoneOrDisabledIdentityLosesEverySession: the directory says the
// identity is gone (or disabled). Every session the User holds, on every
// Device, is revoked.
func TestAGoneOrDisabledIdentityLosesEverySession(t *testing.T) {
	for _, status := range []pluginapi.SignInStatus{pluginapi.SignInGone, pluginapi.SignInDisabled} {
		t.Run(string(status), func(t *testing.T) {
			f := newRecheckFixture(t)
			dir := &directory{}
			f.provide("dir", dir, pluginapi.CapabilitySignInLookup)
			_, laptop := f.signIn("dir", "s-cy", "laptop", auth.ExternalAnswer{})
			_, phone := f.signIn("dir", "s-cy", "phone", auth.ExternalAnswer{})
			_, other := f.signIn("dir", "s-dan", "tv", auth.ExternalAnswer{})
			dir.set("s-cy", status)
			dir.set("s-dan", pluginapi.SignInActive)

			f.runAfter(DefaultRecheckInterval + time.Minute)
			if f.live(laptop) || f.live(phone) {
				t.Fatalf("after %s: laptop live %v, phone live %v; want every session revoked", status, f.live(laptop), f.live(phone))
			}
			if !f.live(other) {
				t.Fatal("another User's session was revoked")
			}
			if !strings.Contains(f.logs.String(), "every session was revoked: plugin=dir") {
				t.Fatalf("no audit line for the revocation; logs:\n%s", f.logs)
			}
		})
	}
}

// TestAnUnreachableProviderKeepsTheLastKnownStateAndRetries: the directory
// times out. cy's grants and session stay exactly as they were, the provider
// is flagged, an audit line is written, and the re-check is tried again an hour
// later rather than a day later. When it answers, the flag clears.
func TestAnUnreachableProviderKeepsTheLastKnownStateAndRetries(t *testing.T) {
	f := newRecheckFixture(t)
	dir := &directory{fail: true}
	f.provide("dir", dir, pluginapi.CapabilitySignInLookup)
	f.mapping("dir", kidsAndAdults...)
	cy, token := f.signIn("dir", "s-cy", "laptop", auth.ExternalAnswer{Groups: []string{"kids"}})

	f.runAfter(DefaultRecheckInterval + time.Minute)
	if dir.timesAsked() != 1 {
		t.Fatalf("asked %d times, want 1", dir.timesAsked())
	}
	if !f.live(token) || !reflect.DeepEqual(f.grants(cy.ID), []string{"cartoons"}) {
		t.Fatalf("after a timeout: session live %v, grants %v; want both kept", f.live(token), f.grants(cy.ID))
	}
	if _, flagged := f.r.Failures()["dir"]; !flagged {
		t.Fatal("the provider is not flagged after a failed re-check")
	}
	if !strings.Contains(f.logs.String(), "sign-in audit: a re-check failed and the last known state is kept: plugin=dir user="+cy.ID+" reason=unreachable") {
		t.Fatalf("no audit line for the failure; logs:\n%s", f.logs)
	}

	f.runAfter(DefaultRecheckInterval + 30*time.Minute)
	if dir.timesAsked() != 1 {
		t.Fatalf("a retry ran after 29 minutes (asked %d times)", dir.timesAsked())
	}
	dir.mu.Lock()
	dir.fail = false
	dir.mu.Unlock()
	dir.set("s-cy", pluginapi.SignInActive, "adults")
	f.runAfter(DefaultRecheckInterval + time.Hour + 2*time.Minute)
	if dir.timesAsked() != 2 {
		t.Fatalf("the retry an hour later asked %d times in all, want 2", dir.timesAsked())
	}
	if got := f.grants(cy.ID); !reflect.DeepEqual(got, []string{"films"}) {
		t.Fatalf("after the retry answered, grants = %v, want films", got)
	}
	if _, flagged := f.r.Failures()["dir"]; flagged {
		t.Fatal("the provider is still flagged after it answered")
	}
}

// oidcDirectory is a refresh-capable OpenID Connect provider whose issuer is a
// test server holding key, and whose refresh answers what the test sets.
func oidcDirectory(t *testing.T) (*directory, func(claims map[string]any) string, string) {
	srv, key := ecIssuer(t)
	d := &directory{issuer: srv.URL + "/", clientID: "client", declared: true}
	sign := func(claims map[string]any) string { return signES256(t, key, claims) }
	return d, sign, srv.URL
}

func goodClaims(iss, sub string, groups ...string) map[string]any {
	return map[string]any{"iss": iss, "aud": "client", "sub": sub,
		"exp": time.Now().Add(time.Hour).Unix(), "groups": groups}
}

func (d *directory) answerRefresh(idToken string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.refresh = func(string) (pluginapi.SignInRefreshResponse, error) {
		return pluginapi.SignInRefreshResponse{Status: pluginapi.SignInActive, IDToken: idToken, RefreshToken: "rt-next"}, nil
	}
}

// TestARefreshReverifiesTheIDTokenBeforeTrustingIt: a refresh-token re-check's
// new ID token is verified exactly as one at sign-in — signature, iss, aud, exp
// — before its groups are applied. One that verifies moves cy to adults; each
// that fails one check is a failure, applies nothing, and keeps her session.
func TestARefreshReverifiesTheIDTokenBeforeTrustingIt(t *testing.T) {
	otherKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		token func(iss string, sign func(map[string]any) string) string
		ok    bool
	}{
		{"a token that verifies", func(iss string, sign func(map[string]any) string) string {
			return sign(goodClaims(iss, "s-cy", "adults"))
		}, true},
		{"another key's signature", func(iss string, _ func(map[string]any) string) string {
			return signES256(t, otherKey, goodClaims(iss, "s-cy", "adults"))
		}, false},
		{"another issuer", func(_ string, sign func(map[string]any) string) string {
			return sign(goodClaims("https://elsewhere.example", "s-cy", "adults"))
		}, false},
		{"another audience", func(iss string, sign func(map[string]any) string) string {
			c := goodClaims(iss, "s-cy", "adults")
			c["aud"] = "someone-else"
			return sign(c)
		}, false},
		{"expired", func(iss string, sign func(map[string]any) string) string {
			c := goodClaims(iss, "s-cy", "adults")
			c["exp"] = time.Now().Add(-time.Hour).Unix()
			return sign(c)
		}, false},
		{"another subject", func(iss string, sign func(map[string]any) string) string {
			return sign(goodClaims(iss, "s-somebody-else", "adults"))
		}, false},
		{"no token", func(string, func(map[string]any) string) string { return "" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecheckFixture(t)
			dir, sign, iss := oidcDirectory(t)
			f.provide("oidc", dir, pluginapi.CapabilityRedirectSignIn, pluginapi.CapabilitySignInRefresh)
			f.mapping("oidc", kidsAndAdults...)
			cy, token := f.signIn("oidc", "s-cy", "laptop", auth.ExternalAnswer{Groups: []string{"kids"}, RefreshToken: "rt-1"})
			dir.answerRefresh(tc.token(iss, sign))

			f.runAfter(DefaultRecheckInterval + time.Minute)
			want := []string{"cartoons"}
			if tc.ok {
				want = []string{"films"}
			}
			if got := f.grants(cy.ID); !reflect.DeepEqual(got, want) {
				t.Fatalf("grants after the refresh = %v, want %v", got, want)
			}
			if !f.live(token) {
				t.Fatal("one refresh revoked the session")
			}
			_, flagged := f.r.Failures()["oidc"]
			if flagged == tc.ok {
				t.Fatalf("provider flagged = %v, want %v", flagged, !tc.ok)
			}
		})
	}
}

// TestTheThirdConsecutiveUnverifiedRecheckRevokes: two re-checks whose ID token
// fails verification keep cy's session; the third in a row revokes it, exactly
// as gone would. With a good re-check between, the count starts again, so a
// fourth failure in all — the second since the reset — revokes nothing.
func TestTheThirdConsecutiveUnverifiedRecheckRevokes(t *testing.T) {
	f := newRecheckFixture(t)
	dir, sign, _ := oidcDirectory(t)
	f.provide("oidc", dir, pluginapi.CapabilityRedirectSignIn, pluginapi.CapabilitySignInRefresh)
	_, token := f.signIn("oidc", "s-cy", "laptop", auth.ExternalAnswer{RefreshToken: "rt-1"})
	dir.answerRefresh(sign(goodClaims("https://elsewhere.example", "s-cy")))

	// Each retry comes round an hour after the failure before it.
	step := DefaultRecheckInterval + time.Minute
	next := func() { f.runAfter(step); step += time.Hour + time.Minute }

	next()
	next()
	if !f.live(token) {
		t.Fatal("two consecutive unverified re-checks revoked the session")
	}
	next()
	if f.live(token) {
		t.Fatal("the third consecutive unverified re-check left the session live")
	}
	if !strings.Contains(f.logs.String(), "every session was revoked: plugin=oidc") {
		t.Fatalf("no audit line for the revocation; logs:\n%s", f.logs)
	}

	g := newRecheckFixture(t)
	dir2, sign2, iss2 := oidcDirectory(t)
	g.provide("oidc", dir2, pluginapi.CapabilityRedirectSignIn, pluginapi.CapabilitySignInRefresh)
	_, token2 := g.signIn("oidc", "s-cy", "laptop", auth.ExternalAnswer{RefreshToken: "rt-1"})
	at := DefaultRecheckInterval + time.Minute
	run := func(tok string, wait time.Duration) {
		dir2.answerRefresh(tok)
		g.runAfter(at)
		at += wait
	}
	badTok := sign2(goodClaims("https://elsewhere.example", "s-cy"))
	goodTok := sign2(goodClaims(iss2, "s-cy"))
	run(badTok, time.Hour+time.Minute)
	run(badTok, time.Hour+time.Minute)
	run(goodTok, DefaultRecheckInterval+time.Minute)
	run(badTok, time.Hour+time.Minute)
	run(badTok, time.Hour+time.Minute)
	if dir2.timesAsked() != 5 {
		t.Fatalf("the directory was asked %d times, want 5", dir2.timesAsked())
	}
	if !g.live(token2) {
		t.Fatal("failures separated by a good re-check revoked the session")
	}
}

// TestAnAutoDisabledProviderKeepsSessionsAndFlagsItsUsers: the host disabled
// "dir" after repeated failure. Its re-checks are unreachable, never
// revocations: every session stays. dee, whose only way in ran through dir, is
// flagged as having no working sign-in path; ed, who has a Local password, and
// fay, who also holds a working provider's identity, are not. Re-enabled, dee
// is no longer flagged.
func TestAnAutoDisabledProviderKeepsSessionsAndFlagsItsUsers(t *testing.T) {
	f := newRecheckFixture(t)
	dir, other := &directory{}, &directory{}
	f.provide("dir", dir, pluginapi.CapabilityPasswordSignIn, pluginapi.CapabilitySignInLookup)
	f.provide("other", other, pluginapi.CapabilityPasswordSignIn)
	dee, deeToken := f.signIn("dir", "s-dee", "laptop", auth.ExternalAnswer{})
	fay, fayToken := f.signIn("dir", "s-fay", "phone", auth.ExternalAnswer{})
	if err := f.auth.AttachExternal(fay.ID, "other", auth.ExternalAnswer{Subject: "o-fay", Username: "fay"}); err != nil {
		t.Fatal(err)
	}
	ed, err := f.auth.CreateUser(context.Background(), "ed", "ed-local-pw", auth.RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.auth.AttachExternal(ed.ID, "dir", auth.ExternalAnswer{Subject: "s-ed", Username: "ed"}); err != nil {
		t.Fatal(err)
	}

	dir.mu.Lock()
	dir.disabled = true
	dir.mu.Unlock()
	f.runAfter(DefaultRecheckInterval + time.Minute)

	if !f.live(deeToken) || !f.live(fayToken) {
		t.Fatalf("with the provider disabled: dee live %v, fay live %v; want every session kept", f.live(deeToken), f.live(fayToken))
	}
	users, err := f.db.ListUsers()
	if err != nil {
		t.Fatal(err)
	}
	stranded, err := f.r.NoWorkingSignInPath(users)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stranded, []string{dee.ID}) {
		t.Fatalf("flagged %v, want only dee %q", stranded, dee.ID)
	}
	if !strings.Contains(f.logs.String(), "reason=provider-disabled") {
		t.Fatalf("no audit line for the disabled provider; logs:\n%s", f.logs)
	}

	dir.mu.Lock()
	dir.disabled = false
	dir.mu.Unlock()
	if stranded, _ := f.r.NoWorkingSignInPath(users); len(stranded) != 0 {
		t.Fatalf("after the provider is re-enabled %v are still flagged", stranded)
	}
}

// TestAProviderWithoutLookupSyncsAtSignInAndOnResyncNowOnly: "plain" declares
// neither lookup nor refresh. The Admin changes its mapping; the schedule never
// asks it and changes nothing, however long passes. "Re-sync now" applies the
// mapping at once, from the groups plain last reported, as a sign-in would.
func TestAProviderWithoutLookupSyncsAtSignInAndOnResyncNowOnly(t *testing.T) {
	f := newRecheckFixture(t)
	plain := &directory{}
	f.provide("plain", plain, pluginapi.CapabilityPasswordSignIn)
	f.mapping("plain", kidsAndAdults...)
	cy, _ := f.signIn("plain", "s-cy", "laptop", auth.ExternalAnswer{Groups: []string{"kids"}})
	if f.r.CanRecheck("plain") {
		t.Fatal("a provider with no lookup and no refresh can be re-checked")
	}
	f.mapping("plain", store.GroupMappingRule{Group: "kids", Role: auth.RoleMember, LibraryIDs: []string{"films"}})

	f.runAfter(10 * DefaultRecheckInterval)
	if got := f.grants(cy.ID); !reflect.DeepEqual(got, []string{"cartoons"}) || plain.timesAsked() != 0 {
		t.Fatalf("after the schedule: grants %v, asked %d; want cartoons and never asked", got, plain.timesAsked())
	}
	res, err := f.r.ResyncNow(context.Background(), "plain")
	if err != nil {
		t.Fatal(err)
	}
	if res.Remapped != 1 || res.Checked != 0 {
		t.Fatalf("re-sync now = %+v, want one identity re-mapped and none asked", res)
	}
	if got := f.grants(cy.ID); !reflect.DeepEqual(got, []string{"films"}) {
		t.Fatalf("after re-sync now: grants %v, want films", got)
	}

	// A provider that CAN be asked is asked at once, whatever the schedule says.
	dir := &directory{}
	f.provide("dir", dir, pluginapi.CapabilitySignInLookup)
	f.signIn("dir", "s-dan", "tv", auth.ExternalAnswer{})
	dir.set("s-dan", pluginapi.SignInActive)
	if res, err := f.r.ResyncNow(context.Background(), "dir"); err != nil || res.Checked != 1 || dir.timesAsked() != 1 {
		t.Fatalf("re-sync now of dir = %+v, %v, asked %d; want one identity asked", res, err, dir.timesAsked())
	}
	if _, err := f.r.ResyncNow(context.Background(), "nobody"); !errors.Is(err, ErrUnknownSignInProvider) {
		t.Fatalf("re-sync now of an unknown provider: %v, want ErrUnknownSignInProvider", err)
	}
}

// TestARecheckNeverGovernsAUserWithALocalPassword: ed has a Local password and
// the films an Admin granted him, and holds a directory identity in a group
// mapped to cartoons. The re-check answers; his grants do not move.
func TestARecheckNeverGovernsAUserWithALocalPassword(t *testing.T) {
	f := newRecheckFixture(t)
	dir := &directory{}
	f.provide("dir", dir, pluginapi.CapabilitySignInLookup)
	f.mapping("dir", kidsAndAdults...)
	ed, err := f.auth.CreateUser(context.Background(), "ed", "ed-local-pw", auth.RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.ReplaceLibraryAccess(ed.ID, []string{"films"}); err != nil {
		t.Fatal(err)
	}
	if err := f.auth.AttachExternal(ed.ID, "dir", auth.ExternalAnswer{Subject: "s-ed", Username: "ed"}); err != nil {
		t.Fatal(err)
	}
	dir.set("s-ed", pluginapi.SignInActive, "kids")

	f.runAfter(DefaultRecheckInterval + time.Minute)
	if dir.timesAsked() != 1 {
		t.Fatalf("asked %d times, want 1", dir.timesAsked())
	}
	if got := f.grants(ed.ID); !reflect.DeepEqual(got, []string{"films"}) {
		t.Fatalf("ed's grants after the re-check = %v, want the films an Admin gave him", got)
	}
}

// TestNoRecheckPersistsAPassword: a password sign-in, a lookup, a refresh and a
// re-sync later, the password is nowhere in the database. The refresh token is
// stored — it is what a re-check redeems — and nothing else of the person's.
func TestNoRecheckPersistsAPassword(t *testing.T) {
	f := newRecheckFixture(t)
	const secret = "the-directory-password-9d2e"
	dir := &directory{}
	f.provide("dir", dir, pluginapi.CapabilityPasswordSignIn, pluginapi.CapabilitySignInLookup)
	f.auth.UseSignInProviders(passwordOnly{id: "dir", password: secret})
	if _, err := f.auth.Login(context.Background(), "cy", secret, auth.DeviceInput{Name: "l", Platform: "t", ClientID: "l"}, ""); err != nil {
		t.Fatal(err)
	}
	dir.set("s-cy", pluginapi.SignInActive, "kids")
	f.runAfter(DefaultRecheckInterval + time.Minute)
	if _, err := f.r.ResyncNow(context.Background(), "dir"); err != nil {
		t.Fatal(err)
	}
	if dir.timesAsked() != 2 {
		t.Fatalf("asked %d times, want 2", dir.timesAsked())
	}
	if where := findInDatabase(t, f.db, secret); where != "" {
		t.Fatalf("the password is stored in %s", where)
	}
}

type passwordOnly struct{ id, password string }

func (p passwordOnly) PasswordProviders() []auth.PasswordProvider { return []auth.PasswordProvider{p} }
func (p passwordOnly) ID() string                                 { return p.id }
func (p passwordOnly) CheckPassword(_ context.Context, username, password string) (auth.ExternalAnswer, bool) {
	if password != p.password {
		return auth.ExternalAnswer{}, false
	}
	return auth.ExternalAnswer{Subject: "s-" + username, Username: username}, true
}

// findInDatabase answers the first table.column holding needle anywhere in a
// value, or "".
func findInDatabase(t *testing.T, db *store.DB, needle string) string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, n)
	}
	rows.Close()
	for _, table := range tables {
		r, err := db.Query(`SELECT * FROM "` + table + `"`)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := r.Columns()
		for r.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := r.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for i, v := range vals {
				if strings.Contains(fmt.Sprintf("%s", v), needle) {
					r.Close()
					return table + "." + cols[i]
				}
			}
		}
		r.Close()
	}
	return ""
}

// checkFailures answers the consecutive unverified re-checks recorded for
// subject.
func (f *recheckFixture) checkFailures(subject string) int {
	f.t.Helper()
	checks, err := f.db.ExternalIdentityChecks()
	if err != nil {
		f.t.Fatal(err)
	}
	for _, c := range checks {
		if c.Subject == subject {
			return c.CheckFailures
		}
	}
	f.t.Fatalf("no external identity %q", subject)
	return 0
}

func (f *recheckFixture) stranded() []string {
	f.t.Helper()
	users, err := f.db.ListUsers()
	if err != nil {
		f.t.Fatal(err)
	}
	out, err := f.r.NoWorkingSignInPath(users)
	if err != nil {
		f.t.Fatal(err)
	}
	if out == nil {
		out = []string{}
	}
	return out
}

// refreshBy makes d's refresh answer by the refresh token it is handed.
func (d *directory) refreshBy(answers map[string]func() (pluginapi.SignInRefreshResponse, error)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.refresh = func(token string) (pluginapi.SignInRefreshResponse, error) {
		if a, ok := answers[token]; ok {
			return a()
		}
		return pluginapi.SignInRefreshResponse{}, errors.New("no answer for this refresh token")
	}
}

// TestARefusedRefreshKeepsSessionsAndFlagsOnlyThatUser: the issuer refuses
// cy's refresh token — it expired — which the provider reports as an error,
// not as the identity gone. Her session and grants are kept, she alone is
// flagged as having no working sign-in path, and she is asked again within the
// hour. dan, whose refresh answered, is neither flagged nor asked again.
func TestARefusedRefreshKeepsSessionsAndFlagsOnlyThatUser(t *testing.T) {
	f := newRecheckFixture(t)
	dir, sign, iss := oidcDirectory(t)
	f.provide("oidc", dir, pluginapi.CapabilityRedirectSignIn, pluginapi.CapabilitySignInRefresh)
	f.mapping("oidc", kidsAndAdults...)
	f.signIn("oidc", "a-dan", "tv", auth.ExternalAnswer{Groups: []string{"kids"}, RefreshToken: "rt-dan"})
	cy, token := f.signIn("oidc", "b-cy", "laptop", auth.ExternalAnswer{Groups: []string{"kids"}, RefreshToken: "rt-cy"})
	good := sign(goodClaims(iss, "a-dan", "kids"))
	dir.refreshBy(map[string]func() (pluginapi.SignInRefreshResponse, error){
		"rt-dan": func() (pluginapi.SignInRefreshResponse, error) {
			return pluginapi.SignInRefreshResponse{Status: pluginapi.SignInActive, IDToken: good}, nil
		},
		"rt-cy": func() (pluginapi.SignInRefreshResponse, error) {
			return pluginapi.SignInRefreshResponse{}, errors.New("refresh: the issuer refused the refresh token (invalid_grant)")
		},
	})

	f.runAfter(DefaultRecheckInterval + time.Minute)
	if !f.live(token) || !reflect.DeepEqual(f.grants(cy.ID), []string{"cartoons"}) {
		t.Fatalf("after a refused refresh: session live %v, grants %v; want both kept", f.live(token), f.grants(cy.ID))
	}
	if got := f.stranded(); !reflect.DeepEqual(got, []string{cy.ID}) {
		t.Fatalf("flagged %v, want only cy %q", got, cy.ID)
	}
	asked := dir.timesAsked()
	f.runAfter(DefaultRecheckInterval + time.Hour + 2*time.Minute)
	if dir.timesAsked() != asked+1 {
		t.Fatalf("an hour later the provider was asked %d more times, want cy's one retry", dir.timesAsked()-asked)
	}
	if !f.live(token) {
		t.Fatal("a second refused refresh revoked the session")
	}
}

// TestAnUnreachableIssuerNeverCountsTowardRevocation: the issuer's discovery
// document cannot be fetched, so the refreshed token cannot be checked at all.
// That is the provider unreachable, not a token that failed verification:
// however many times it happens, nothing is counted and nothing is revoked. A
// token that WAS checked against the issuer's keys and failed does count.
func TestAnUnreachableIssuerNeverCountsTowardRevocation(t *testing.T) {
	f := newRecheckFixture(t)
	down := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(down.Close)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := &directory{issuer: down.URL + "/", clientID: "client", declared: true}
	f.provide("oidc", dir, pluginapi.CapabilityRedirectSignIn, pluginapi.CapabilitySignInRefresh)
	_, token := f.signIn("oidc", "s-cy", "laptop", auth.ExternalAnswer{RefreshToken: "rt-1"})
	dir.answerRefresh(signES256(t, key, goodClaims(down.URL, "s-cy")))

	step := DefaultRecheckInterval + time.Minute
	for i := 0; i < MaxUnverifiedRechecks+1; i++ {
		f.runAfter(step)
		step += time.Hour + time.Minute
	}
	if dir.timesAsked() != MaxUnverifiedRechecks+1 {
		t.Fatalf("asked %d times, want %d", dir.timesAsked(), MaxUnverifiedRechecks+1)
	}
	if !f.live(token) {
		t.Fatal("an issuer whose keys could not be fetched revoked the session")
	}
	if n := f.checkFailures("s-cy"); n != 0 {
		t.Fatalf("unverified count = %d after the issuer was unreachable, want 0", n)
	}
	if !strings.Contains(f.logs.String(), "reason=unreachable") {
		t.Fatalf("no unreachable audit line; logs:\n%s", f.logs)
	}

	g := newRecheckFixture(t)
	dir2, sign2, iss2 := oidcDirectory(t)
	g.provide("oidc", dir2, pluginapi.CapabilityRedirectSignIn, pluginapi.CapabilitySignInRefresh)
	g.signIn("oidc", "s-cy", "laptop", auth.ExternalAnswer{RefreshToken: "rt-1"})
	bad := goodClaims(iss2, "s-cy")
	bad["aud"] = "someone-else"
	dir2.answerRefresh(sign2(bad))
	g.runAfter(DefaultRecheckInterval + time.Minute)
	if n := g.checkFailures("s-cy"); n != 1 {
		t.Fatalf("unverified count = %d after a fetched token failed verification, want 1", n)
	}
}

// TestOneIdentitysAnswerDoesNotClearAnotherIdentitysFailure: the directory
// fails for dan and answers for cy. The provider stays flagged while dan's
// re-check is failing, and clears once dan's answers too.
func TestOneIdentitysAnswerDoesNotClearAnotherIdentitysFailure(t *testing.T) {
	f := newRecheckFixture(t)
	dir := &directory{failFor: map[string]bool{"a-dan": true}}
	f.provide("dir", dir, pluginapi.CapabilitySignInLookup)
	f.signIn("dir", "a-dan", "tv", auth.ExternalAnswer{})
	f.signIn("dir", "b-cy", "laptop", auth.ExternalAnswer{})
	dir.set("b-cy", pluginapi.SignInActive)

	f.runAfter(DefaultRecheckInterval + time.Minute)
	if dir.timesAsked() != 2 {
		t.Fatalf("asked %d times, want 2", dir.timesAsked())
	}
	if _, flagged := f.r.Failures()["dir"]; !flagged {
		t.Fatal("cy's answer cleared the flag while dan's re-check is failing")
	}

	dir.mu.Lock()
	dir.failFor = nil
	dir.mu.Unlock()
	dir.set("a-dan", pluginapi.SignInActive)
	f.runAfter(DefaultRecheckInterval + time.Hour + 2*time.Minute)
	if _, flagged := f.r.Failures()["dir"]; flagged {
		t.Fatal("the provider is still flagged after every identity answered")
	}
}

// TestAnUnverifiedIdentityFlagsOnlyItsOwnUser: cy's refreshed token names
// another audience; dan's verifies. Only cy is flagged as having no working
// sign-in path — dan's way in works.
func TestAnUnverifiedIdentityFlagsOnlyItsOwnUser(t *testing.T) {
	f := newRecheckFixture(t)
	dir, sign, iss := oidcDirectory(t)
	f.provide("oidc", dir, pluginapi.CapabilityRedirectSignIn, pluginapi.CapabilitySignInRefresh)
	f.signIn("oidc", "a-dan", "tv", auth.ExternalAnswer{RefreshToken: "rt-dan"})
	cy, _ := f.signIn("oidc", "b-cy", "laptop", auth.ExternalAnswer{RefreshToken: "rt-cy"})
	good := sign(goodClaims(iss, "a-dan"))
	badClaims := goodClaims(iss, "b-cy")
	badClaims["aud"] = "someone-else"
	bad := sign(badClaims)
	dir.refreshBy(map[string]func() (pluginapi.SignInRefreshResponse, error){
		"rt-dan": func() (pluginapi.SignInRefreshResponse, error) {
			return pluginapi.SignInRefreshResponse{Status: pluginapi.SignInActive, IDToken: good}, nil
		},
		"rt-cy": func() (pluginapi.SignInRefreshResponse, error) {
			return pluginapi.SignInRefreshResponse{Status: pluginapi.SignInActive, IDToken: bad}, nil
		},
	})

	f.runAfter(DefaultRecheckInterval + time.Minute)
	if got := f.stranded(); !reflect.DeepEqual(got, []string{cy.ID}) {
		t.Fatalf("flagged %v, want only cy %q", got, cy.ID)
	}
	if _, flagged := f.r.Failures()["oidc"]; !flagged {
		t.Fatal("the provider is not flagged while cy's re-check is failing")
	}
}

// TestReSyncNowCountsWhatItDid: "re-sync now" counts as re-mapped only the
// Users whose access it changed — not ed, whom a mapping never governs, nor
// dan, whose access already matched — and as asked only the identities it
// asked, not one with no refresh token to redeem.
func TestReSyncNowCountsWhatItDid(t *testing.T) {
	f := newRecheckFixture(t)
	plain := &directory{}
	f.provide("plain", plain, pluginapi.CapabilityPasswordSignIn)
	f.mapping("plain", kidsAndAdults...)
	cy, _ := f.signIn("plain", "s-cy", "laptop", auth.ExternalAnswer{Groups: []string{"kids"}})
	f.signIn("plain", "s-dan", "tv", auth.ExternalAnswer{Groups: []string{"adults"}})
	ed, err := f.auth.CreateUser(context.Background(), "ed", "ed-local-pw", auth.RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.auth.AttachExternal(ed.ID, "plain", auth.ExternalAnswer{Subject: "s-ed", Username: "ed", Groups: []string{"kids"}}); err != nil {
		t.Fatal(err)
	}
	f.mapping("plain",
		store.GroupMappingRule{Group: "kids", Role: auth.RoleMember, LibraryIDs: []string{"films"}},
		store.GroupMappingRule{Group: "adults", Role: auth.RoleMember, LibraryIDs: []string{"films"}})

	res, err := f.r.ResyncNow(context.Background(), "plain")
	if err != nil {
		t.Fatal(err)
	}
	if res.Remapped != 1 || res.Checked != 0 {
		t.Fatalf("re-sync now = %+v, want only cy re-mapped", res)
	}
	if got := f.grants(cy.ID); !reflect.DeepEqual(got, []string{"films"}) {
		t.Fatalf("cy's grants = %v, want films", got)
	}

	dir, sign, iss := oidcDirectory(t)
	f.provide("oidc", dir, pluginapi.CapabilityRedirectSignIn, pluginapi.CapabilitySignInRefresh)
	f.signIn("oidc", "o-fay", "phone", auth.ExternalAnswer{RefreshToken: "rt-fay"})
	f.signIn("oidc", "o-gus", "desk", auth.ExternalAnswer{})
	dir.answerRefresh(sign(goodClaims(iss, "o-fay")))
	res, err = f.r.ResyncNow(context.Background(), "oidc")
	if err != nil {
		t.Fatal(err)
	}
	if res.Checked != 1 || dir.timesAsked() != 1 {
		t.Fatalf("re-sync now = %+v, asked %d; want the one identity with a refresh token asked", res, dir.timesAsked())
	}
}

// TestAGoneIdentityLosesItsStreamTokens: a stream token outlives no revocation.
// Once the directory says cy is gone, the token minted for her Playback session
// no longer resolves; dan's still does.
func TestAGoneIdentityLosesItsStreamTokens(t *testing.T) {
	f := newRecheckFixture(t)
	dir := &directory{}
	f.provide("dir", dir, pluginapi.CapabilitySignInLookup)
	cy, _ := f.signIn("dir", "s-cy", "laptop", auth.ExternalAnswer{})
	dan, _ := f.signIn("dir", "s-dan", "tv", auth.ExternalAnswer{})
	cyStream, err := f.auth.MintStreamToken("session-cy", cy.ID)
	if err != nil {
		t.Fatal(err)
	}
	danStream, err := f.auth.MintStreamToken("session-dan", dan.ID)
	if err != nil {
		t.Fatal(err)
	}
	dir.set("s-cy", pluginapi.SignInGone)
	dir.set("s-dan", pluginapi.SignInActive)

	f.runAfter(DefaultRecheckInterval + time.Minute)
	if _, _, ok := f.auth.ResolveStreamToken(cyStream.Token); ok {
		t.Fatal("cy's stream token still resolves after she is gone")
	}
	if _, _, ok := f.auth.ResolveStreamToken(danStream.Token); !ok {
		t.Fatal("dan's stream token was revoked")
	}
}

// TestEveryUnverifiedRecheckWritesAnAuditLine: each re-check whose ID token
// fails verification is its own audit line, naming the provider, the User and
// how many in a row — not only the one that revokes.
func TestEveryUnverifiedRecheckWritesAnAuditLine(t *testing.T) {
	f := newRecheckFixture(t)
	dir, sign, _ := oidcDirectory(t)
	f.provide("oidc", dir, pluginapi.CapabilityRedirectSignIn, pluginapi.CapabilitySignInRefresh)
	cy, _ := f.signIn("oidc", "s-cy", "laptop", auth.ExternalAnswer{RefreshToken: "rt-1"})
	dir.answerRefresh(sign(goodClaims("https://elsewhere.example", "s-cy")))

	f.runAfter(DefaultRecheckInterval + time.Minute)
	f.runAfter(DefaultRecheckInterval + time.Hour + 2*time.Minute)
	for n := 1; n <= 2; n++ {
		want := fmt.Sprintf("sign-in audit: a re-check's ID token failed verification: plugin=oidc user=%s consecutive=%d", cy.ID, n)
		if !strings.Contains(f.logs.String(), want) {
			t.Fatalf("no audit line %q; logs:\n%s", want, f.logs)
		}
	}
}

// brokenIssuer serves a discovery document and a JWKS as the handlers say:
// reachable, answering 200, and saying something no key can be read from.
func brokenIssuer(t *testing.T, discovery func(url string) string, jwks string) string {
	t.Helper()
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(discovery(srv.URL)))
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(jwks))
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestAnIssuerAnsweringInvalidKeysCountsAsUnverified: the issuer answers, but
// what it answers is no key set — an HTML page, a JWKS of the wrong shape, a
// discovery document naming another issuer or no jwks_uri. That is not the
// issuer unreachable: each counts toward revoking, and the third in a row
// revokes.
func TestAnIssuerAnsweringInvalidKeysCountsAsUnverified(t *testing.T) {
	good := func(url string) string { return `{"issuer":"` + url + `","jwks_uri":"` + url + `/jwks"}` }
	for _, tc := range []struct {
		name      string
		discovery func(url string) string
		jwks      string
	}{
		{"a JWKS that is HTML", good, "<html><body>Sign in</body></html>"},
		{"a JWKS of the wrong shape", good, `{"keys":"x"}`},
		{"a discovery document naming another issuer", func(string) string {
			return `{"issuer":"https://elsewhere.example","jwks_uri":"https://elsewhere.example/jwks"}`
		}, `{"keys":[]}`},
		{"a discovery document naming no jwks_uri", func(url string) string { return `{"issuer":"` + url + `"}` }, `{"keys":[]}`},
		{"a discovery document that is not JSON", func(string) string { return "<html></html>" }, `{"keys":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecheckFixture(t)
			iss := brokenIssuer(t, tc.discovery, tc.jwks)
			key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			dir := &directory{issuer: iss + "/", clientID: "client", declared: true}
			f.provide("oidc", dir, pluginapi.CapabilityRedirectSignIn, pluginapi.CapabilitySignInRefresh)
			_, token := f.signIn("oidc", "s-cy", "laptop", auth.ExternalAnswer{RefreshToken: "rt-1"})
			dir.answerRefresh(signES256(t, key, goodClaims(iss, "s-cy")))

			step := DefaultRecheckInterval + time.Minute
			for i := 1; i < MaxUnverifiedRechecks; i++ {
				f.runAfter(step)
				step += time.Hour + time.Minute
			}
			if n := f.checkFailures("s-cy"); n != MaxUnverifiedRechecks-1 {
				t.Fatalf("unverified count = %d, want %d", n, MaxUnverifiedRechecks-1)
			}
			if !f.live(token) {
				t.Fatal("fewer than three unverified re-checks revoked the session")
			}
			f.runAfter(step)
			if f.live(token) {
				t.Fatal("the third re-check against invalid keys left the session live")
			}
		})
	}
}

// TestASignInClearsTheRechecksFailure: the issuer refused cy's refresh token,
// so she is flagged as having no working sign-in path and the provider as
// failing. She signs in again: the provider has answered for her, so neither
// flag stands — while dan's refresh is still refused, the provider stays
// flagged for him alone, until he too signs in.
func TestASignInClearsTheRechecksFailure(t *testing.T) {
	f := newRecheckFixture(t)
	dir, _, _ := oidcDirectory(t)
	f.provide("oidc", dir, pluginapi.CapabilityRedirectSignIn, pluginapi.CapabilitySignInRefresh)
	dan, _ := f.signIn("oidc", "a-dan", "tv", auth.ExternalAnswer{RefreshToken: "rt-dan"})
	cy, _ := f.signIn("oidc", "b-cy", "laptop", auth.ExternalAnswer{RefreshToken: "rt-cy"})
	dir.refreshBy(map[string]func() (pluginapi.SignInRefreshResponse, error){})

	f.runAfter(DefaultRecheckInterval + time.Minute)
	if got := f.stranded(); !reflect.DeepEqual(got, sortedIDs(cy.ID, dan.ID)) {
		t.Fatalf("after refused refreshes flagged %v, want cy and dan", got)
	}

	f.signIn("oidc", "b-cy", "laptop", auth.ExternalAnswer{RefreshToken: "rt-cy-2"})
	if got := f.stranded(); !reflect.DeepEqual(got, []string{dan.ID}) {
		t.Fatalf("after cy signed in again flagged %v, want only dan %q", got, dan.ID)
	}
	if _, flagged := f.r.Failures()["oidc"]; !flagged {
		t.Fatal("cy's sign-in cleared the provider's flag while dan's re-check is failing")
	}

	f.signIn("oidc", "a-dan", "tv", auth.ExternalAnswer{RefreshToken: "rt-dan-2"})
	if got := f.stranded(); len(got) != 0 {
		t.Fatalf("after both signed in again flagged %v, want nobody", got)
	}
	if _, flagged := f.r.Failures()["oidc"]; flagged {
		t.Fatal("the provider is still flagged after every failing identity signed in")
	}
}

func sortedIDs(ids ...string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}

// TestALookupFailureFlagsEveryUserWhoseOnlyPathIsThatProvider: the directory's
// lookup fails for dee, the one identity due. fay's is not due for another 12
// hours and is never asked, but her only way in runs through the same failing
// lookup, so she is flagged with dee.
func TestALookupFailureFlagsEveryUserWhoseOnlyPathIsThatProvider(t *testing.T) {
	f := newRecheckFixture(t)
	dir := &directory{fail: true}
	f.provide("dir", dir, pluginapi.CapabilityPasswordSignIn, pluginapi.CapabilitySignInLookup)
	dee, _ := f.signIn("dir", "s-dee", "laptop", auth.ExternalAnswer{})
	fay, _ := f.signIn("dir", "s-fay", "phone", auth.ExternalAnswer{})
	if err := f.db.RecordExternalCheck("dir", "s-fay", "", nil, "", time.Now().Add(12*time.Hour)); err != nil {
		t.Fatal(err)
	}

	f.runAfter(DefaultRecheckInterval + time.Minute)
	if dir.timesAsked() != 1 {
		t.Fatalf("asked %d times, want only dee's identity asked", dir.timesAsked())
	}
	if got := f.stranded(); !reflect.DeepEqual(got, sortedIDs(dee.ID, fay.ID)) {
		t.Fatalf("flagged %v, want dee %q and fay %q", got, dee.ID, fay.ID)
	}
}
