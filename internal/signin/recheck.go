package signin

import (
	"context"
	"errors"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/goozakdev/obelo-server/internal/store"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The periodic re-check of External identities (ADR-0063 decisions 4 and 9),
// host side. Between sign-ins the host asks each provider again about the
// identities it vouched for — through lookup(subject), or by redeeming the
// refresh token its exchange handed back — so an Admin's Group mapping follows
// the directory, and a person removed there loses their sessions here. The
// Server never stores a password to do it.
//
// What an answer is worth is decided here, and the two failures are never one:
//
//   - The provider says the identity is gone or disabled: every session the
//     User holds is revoked.
//   - The provider cannot be asked — it errs, times out, answers something this
//     host does not understand, or the host has disabled it — and the User's
//     last known state is kept and the identity is asked again later. A failing
//     Plugin is not evidence a person should be locked out. A refresh the
//     source refuses (an expired or revoked refresh token) is one of these: it
//     says the token is dead, not that the person is.
//   - A refresh whose ID token was checked and fails verification is treated as
//     unreachable for up to MaxUnverifiedRechecks consecutive re-checks; the one
//     that reaches it revokes like gone. Any answer in between resets the count.
//     A token that could not be checked, because the issuer's keys could not be
//     fetched, is unreachable and is not counted.
//
// A provider is flagged from its first failure until every identity whose
// re-check failed has answered, and every failure is an audit line naming the
// provider, the User and the kind of failure — never what the Plugin said.
//
// A failed lookup, or a provider the host disabled, is the provider failing:
// every User whose only way in runs through it has no working sign-in path. A
// failed refresh, or a token that fails verification, may be one person's
// refresh token alone, so only that identity's User is flagged.

// DefaultRecheckInterval is how long after a provider last vouched for an
// identity it is asked again, unless the Admin set another interval for it.
const DefaultRecheckInterval = 24 * time.Hour

// recheckRetryDelay is how soon a re-check that could not reach the provider is
// tried again, when that is sooner than the interval.
const recheckRetryDelay = time.Hour

// MaxUnverifiedRechecks is how many consecutive re-checks whose ID token failed
// verification are treated as unreachable; the last of them revokes.
const MaxUnverifiedRechecks = 3

// RecheckStore is the persistence the re-check needs. *store.DB satisfies it.
type RecheckStore interface {
	ExternalIdentityChecks() ([]store.ExternalIdentityCheck, error)
	ExternalIdentitiesByUser(userID string) ([]store.ExternalIdentity, error)
	RecheckInterval(pluginID string) (time.Duration, error)
	SetRecheckInterval(pluginID string, d time.Duration) error
	RecordExternalCheck(pluginID, subject, username string, groups []string, refreshToken string, at time.Time) error
	NoteExternalCheckFailure(pluginID, subject string, retryAt time.Time, unverified bool) (int, error)
}

// Governor is what a re-check changes about a User. *auth.Service satisfies it:
// the Group mapping is applied, and sessions revoked, by the same code a
// sign-in uses, and a sign-in says when a provider answered for an identity.
type Governor interface {
	SyncGroupMappingChanged(userID string) (bool, error)
	RevokeSessions(userID string) error
	OnExternalSignIn(fn func(pluginID, subject string))
}

// ErrUnknownSignInProvider is a re-sync naming no registered Sign-in provider.
var ErrUnknownSignInProvider = errors.New("signin: no such sign-in provider")

// ProviderFailure is a provider flagged on the Admin page: since when its
// re-checks have been failing, and the kind of the latest failure.
type ProviderFailure struct {
	Since  time.Time `json:"since"`
	Reason string    `json:"reason"`
}

// The kinds of failure an audit line and a flag name.
const (
	failUnreachable = "unreachable"
	failDisabled    = "provider-disabled"
	failUnverified  = "id-token-unverified"
)

// Rechecker is the host half of the re-check.
type Rechecker struct {
	reg      *pluginapi.Registry
	store    RecheckStore
	gov      Governor
	verifier *idTokenVerifier
	now      func() time.Time
	logf     func(string, ...any)

	// pass serialises re-check passes, so the schedule and an Admin's "re-sync
	// now" never ask about one identity twice at once.
	pass sync.Mutex

	mu      sync.Mutex
	failing map[string]ProviderFailure
	// failed is, by provider and then subject, each identity whose latest
	// re-check failed. A provider is flagged while it holds any.
	failed map[string]map[string]identityFailure
}

// identityFailure is one identity's failed re-check: its kind, and whether it
// was the provider failing (wide) rather than that identity alone.
type identityFailure struct {
	reason string
	wide   bool
}

// NewRechecker returns the re-check over reg's Sign-in providers.
// A sign-in through a provider is it answering for that identity, so it clears
// the identity's failure exactly as a re-check that answered does.
func NewRechecker(reg *pluginapi.Registry, st RecheckStore, gov Governor) *Rechecker {
	r := &Rechecker{
		reg:      reg,
		store:    st,
		gov:      gov,
		verifier: newIDTokenVerifier(),
		now:      time.Now,
		logf:     log.Printf,
		failing:  map[string]ProviderFailure{},
		failed:   map[string]map[string]identityFailure{},
	}
	gov.OnExternalSignIn(r.answered)
	return r
}

// Interval is how often pluginID's identities are re-checked.
func (r *Rechecker) Interval(pluginID string) (time.Duration, error) {
	d, err := r.store.RecheckInterval(pluginID)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return DefaultRecheckInterval, nil
	}
	return d, nil
}

// SetInterval stores the Admin's interval for pluginID; 0 restores the default.
func (r *Rechecker) SetInterval(pluginID string, d time.Duration) error {
	if _, ok := r.reg.SignInProvider(pluginID); !ok {
		return ErrUnknownSignInProvider
	}
	return r.store.SetRecheckInterval(pluginID, d)
}

// Known is whether pluginID is a registered Sign-in provider.
func (r *Rechecker) Known(pluginID string) bool {
	_, ok := r.reg.SignInProvider(pluginID)
	return ok
}

// CanRecheck is whether pluginID can be asked between sign-ins at all: it
// declared lookup(subject), or a refresh. One that declared neither is synced at
// sign-in only, and by an Admin's "re-sync now".
func (r *Rechecker) CanRecheck(pluginID string) bool {
	reg, ok := r.reg.SignInProvider(pluginID)
	return ok && declaresRecheck(reg.Descriptor)
}

func declaresRecheck(d pluginapi.Descriptor) bool {
	return d.HasCapability(pluginapi.CapabilitySignInLookup) || d.HasCapability(pluginapi.CapabilitySignInRefresh)
}

// Failures lists the providers flagged now, by id.
func (r *Rechecker) Failures() map[string]ProviderFailure {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]ProviderFailure, len(r.failing))
	for k, v := range r.failing {
		out[k] = v
	}
	return out
}

// Run re-checks every due identity each time every elapses, until ctx ends.
func (r *Rechecker) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.RunDue(ctx); err != nil {
				r.logf("obelo: the sign-in re-check did not run: %v", err)
			}
		}
	}
}

// RunDue re-checks every identity that is due: its provider can be asked, and
// the provider's interval has passed since it last vouched for the identity, or
// a retry after a failure has come round.
func (r *Rechecker) RunDue(ctx context.Context) error {
	r.pass.Lock()
	defer r.pass.Unlock()
	checks, err := r.store.ExternalIdentityChecks()
	if err != nil {
		return err
	}
	r.forgetMissing(checks)
	now := r.now()
	intervals := map[string]time.Duration{}
	for _, c := range checks {
		if !r.CanRecheck(c.PluginID) {
			continue
		}
		interval, ok := intervals[c.PluginID]
		if !ok {
			if interval, err = r.Interval(c.PluginID); err != nil {
				return err
			}
			intervals[c.PluginID] = interval
		}
		due := c.LastSeenAt.Add(interval)
		if !c.RetryAt.IsZero() {
			due = c.RetryAt
		}
		if now.Before(due) {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.recheck(ctx, c, interval)
	}
	return nil
}

// ResyncResult is what an Admin's "re-sync now" did: how many identities were
// asked, how many were only re-mapped from the groups last recorded (the
// provider cannot be asked without the person), how many failed and how many
// Users lost their sessions.
type ResyncResult struct {
	Checked  int `json:"checked"`
	Remapped int `json:"remapped"`
	Failed   int `json:"failed"`
	Revoked  int `json:"revoked"`
}

// ResyncNow is an Admin's "re-sync now" for pluginID: every identity it vouched
// for is re-checked at once, whatever the schedule says. For a provider that
// declared no lookup and no refresh it is the check a sign-in makes with the
// groups the provider last reported — the Admin's current mapping, applied
// again — because nothing else can be asked without the person present.
func (r *Rechecker) ResyncNow(ctx context.Context, pluginID string) (ResyncResult, error) {
	reg, ok := r.reg.SignInProvider(pluginID)
	if !ok {
		return ResyncResult{}, ErrUnknownSignInProvider
	}
	r.pass.Lock()
	defer r.pass.Unlock()
	checks, err := r.store.ExternalIdentityChecks()
	if err != nil {
		return ResyncResult{}, err
	}
	r.forgetMissing(checks)
	interval, err := r.Interval(pluginID)
	if err != nil {
		return ResyncResult{}, err
	}
	var res ResyncResult
	for _, c := range checks {
		if c.PluginID != pluginID {
			continue
		}
		if !declaresRecheck(reg.Descriptor) {
			changed, err := r.gov.SyncGroupMappingChanged(c.UserID)
			if err != nil {
				return res, err
			}
			if changed {
				res.Remapped++
			}
			continue
		}
		o := r.recheck(ctx, c, interval)
		if o != outcomeSkipped {
			res.Checked++
		}
		switch o {
		case outcomeFailed:
			res.Failed++
		case outcomeRevoked:
			res.Revoked++
		case outcomeFailedAndRevoked:
			res.Failed++
			res.Revoked++
		}
	}
	return res, nil
}

type outcome int

const (
	outcomeSynced outcome = iota
	outcomeFailed
	outcomeRevoked
	outcomeFailedAndRevoked
	outcomeSkipped
)

// recheck asks c's provider about it once and acts on the answer.
func (r *Rechecker) recheck(ctx context.Context, c store.ExternalIdentityCheck, interval time.Duration) outcome {
	reg, ok := r.reg.SignInProvider(c.PluginID)
	if !ok {
		return outcomeSkipped
	}
	built, err := reg.New(pluginapi.Settings{Enabled: true})
	if err != nil {
		// Disabled by the host after repeated failure, or refused at load: the
		// same as unreachable (ADR-0063 decision 9), never a revocation.
		return r.unreachable(c, interval, failDisabled, true)
	}

	d := reg.Descriptor
	if lp, ok := built.(pluginapi.SignInLookupProvider); ok && d.HasCapability(pluginapi.CapabilitySignInLookup) {
		resp, err := lp.Lookup(ctx, pluginapi.SignInLookupRequest{Subject: c.Subject})
		if err != nil {
			return r.unreachable(c, interval, failUnreachable, true)
		}
		switch resp.Status {
		case pluginapi.SignInGone, pluginapi.SignInDisabled:
			return r.revoke(c, interval, string(resp.Status))
		case pluginapi.SignInActive:
			if resp.Identity == nil || resp.Identity.Subject != c.Subject {
				return r.unreachable(c, interval, failUnreachable, true)
			}
			return r.synced(c, resp.Identity.Username, tidyGroups(resp.Identity.Groups), "")
		}
		return r.unreachable(c, interval, failUnreachable, true)
	}

	rp, ok := built.(pluginapi.SignInRefreshProvider)
	if !ok || !d.HasCapability(pluginapi.CapabilitySignInRefresh) || c.RefreshToken == "" {
		return outcomeSkipped
	}
	resp, err := rp.Refresh(ctx, pluginapi.SignInRefreshRequest{RefreshToken: c.RefreshToken})
	if err != nil {
		return r.unreachable(c, interval, failUnreachable, false)
	}
	switch resp.Status {
	case pluginapi.SignInGone, pluginapi.SignInDisabled:
		return r.revoke(c, interval, string(resp.Status))
	case pluginapi.SignInActive:
	default:
		return r.unreachable(c, interval, failUnreachable, false)
	}

	var issuer, clientID string
	declared := false
	if a, ok := built.(IDTokenAudience); ok {
		issuer, clientID, declared = a.IDTokenAudience()
	}
	if !declared {
		// A plain OAuth2 provider: its identity is taken as given, as at sign-in,
		// and a token it answers anyway is one nobody could verify.
		if resp.IDToken != "" {
			return r.unverified(c, interval)
		}
		if resp.Identity == nil || resp.Identity.Subject != c.Subject {
			return r.unreachable(c, interval, failUnreachable, false)
		}
		return r.synced(c, resp.Identity.Username, tidyGroups(resp.Identity.Groups), resp.RefreshToken)
	}
	// An OpenID Connect provider: the refreshed token is verified exactly as one
	// at sign-in is, and the subject and groups are the TOKEN's. A token naming
	// somebody else is no answer about this identity.
	if resp.IDToken == "" || issuer == "" || clientID == "" {
		return r.unverified(c, interval)
	}
	claims, err := r.verifier.verifyRefreshed(ctx, resp.IDToken, issuer, clientID)
	var unfetched keysUnreachableError
	if errors.As(err, &unfetched) {
		// The token could not be checked at all: the issuer is unreachable,
		// which is never a count toward revoking anybody.
		return r.unreachable(c, interval, failUnreachable, false)
	}
	if err != nil || claims.Subject != c.Subject {
		return r.unverified(c, interval)
	}
	return r.synced(c, claims.PreferredUsername, tidyGroups(claims.Groups), resp.RefreshToken)
}

// synced records an answer and applies the mapping to it.
func (r *Rechecker) synced(c store.ExternalIdentityCheck, username string, groups []string, refreshToken string) outcome {
	if err := r.store.RecordExternalCheck(c.PluginID, c.Subject, username, groups, refreshToken, r.now()); err != nil {
		r.logf("obelo: the sign-in re-check could not record an answer: plugin=%s user=%s: %v", c.PluginID, c.UserID, err)
		return outcomeFailed
	}
	if _, err := r.gov.SyncGroupMappingChanged(c.UserID); err != nil {
		r.logf("obelo: the sign-in re-check could not apply the group mapping: plugin=%s user=%s: %v", c.PluginID, c.UserID, err)
		return outcomeFailed
	}
	r.answered(c.PluginID, c.Subject)
	return outcomeSynced
}

// retryAt is when a failed re-check of an identity is tried again.
func (r *Rechecker) retryAt(interval time.Duration) time.Time {
	return r.now().Add(min(interval, recheckRetryDelay))
}

// flag marks c's identity failing, and its provider from its first failure.
func (r *Rechecker) flag(c store.ExternalIdentityCheck, reason string, wide bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.failing[c.PluginID]
	if !ok {
		f.Since = r.now()
	}
	f.Reason = reason
	r.failing[c.PluginID] = f
	if r.failed[c.PluginID] == nil {
		r.failed[c.PluginID] = map[string]identityFailure{}
	}
	r.failed[c.PluginID][c.Subject] = identityFailure{reason: reason, wide: wide}
}

// answered clears the identity's failure, and its provider's flag once no
// identity of it is failing.
func (r *Rechecker) answered(pluginID, subject string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.failed[pluginID], subject)
	if len(r.failed[pluginID]) == 0 {
		delete(r.failed, pluginID)
		delete(r.failing, pluginID)
	}
}

// forgetMissing drops the failures of identities that no longer exist, so a
// User deleted mid-failure does not keep a provider flagged.
func (r *Rechecker) forgetMissing(checks []store.ExternalIdentityCheck) {
	present := map[string]map[string]bool{}
	for _, c := range checks {
		if present[c.PluginID] == nil {
			present[c.PluginID] = map[string]bool{}
		}
		present[c.PluginID][c.Subject] = true
	}
	var gone [][2]string
	r.mu.Lock()
	for pluginID, subjects := range r.failed {
		for subject := range subjects {
			if !present[pluginID][subject] {
				gone = append(gone, [2]string{pluginID, subject})
			}
		}
	}
	r.mu.Unlock()
	for _, g := range gone {
		r.answered(g[0], g[1])
	}
}

// unreachable keeps the User's last known state and asks again later. wide is
// whether it was the provider failing, not only this identity.
func (r *Rechecker) unreachable(c store.ExternalIdentityCheck, interval time.Duration, reason string, wide bool) outcome {
	r.flag(c, reason, wide)
	r.logf("obelo: sign-in audit: a re-check failed and the last known state is kept: plugin=%s user=%s reason=%s",
		c.PluginID, c.UserID, reason)
	if _, err := r.store.NoteExternalCheckFailure(c.PluginID, c.Subject, r.retryAt(interval), false); err != nil {
		r.logf("obelo: the sign-in re-check could not schedule a retry: plugin=%s user=%s: %v", c.PluginID, c.UserID, err)
	}
	return outcomeFailed
}

// unverified is a re-check whose ID token failed verification: unreachable,
// until it is the MaxUnverifiedRechecks-th in a row.
func (r *Rechecker) unverified(c store.ExternalIdentityCheck, interval time.Duration) outcome {
	r.flag(c, failUnverified, false)
	n, err := r.store.NoteExternalCheckFailure(c.PluginID, c.Subject, r.retryAt(interval), true)
	if err != nil {
		r.logf("obelo: the sign-in re-check could not record a failure: plugin=%s user=%s: %v", c.PluginID, c.UserID, err)
		return outcomeFailed
	}
	r.logf("obelo: sign-in audit: a re-check's ID token failed verification: plugin=%s user=%s consecutive=%d",
		c.PluginID, c.UserID, n)
	if n < MaxUnverifiedRechecks {
		return outcomeFailed
	}
	if err := r.gov.RevokeSessions(c.UserID); err != nil {
		r.logf("obelo: the sign-in re-check could not revoke sessions: plugin=%s user=%s: %v", c.PluginID, c.UserID, err)
		return outcomeFailed
	}
	r.logf("obelo: sign-in audit: every session was revoked: plugin=%s user=%s reason=%s",
		c.PluginID, c.UserID, failUnverified)
	return outcomeFailedAndRevoked
}

// revoke ends every session of a User whose identity the provider says is gone
// or disabled. It is asked again at the next interval, not sooner.
func (r *Rechecker) revoke(c store.ExternalIdentityCheck, interval time.Duration, status string) outcome {
	if err := r.gov.RevokeSessions(c.UserID); err != nil {
		r.logf("obelo: the sign-in re-check could not revoke sessions: plugin=%s user=%s: %v", c.PluginID, c.UserID, err)
		return outcomeFailed
	}
	r.logf("obelo: sign-in audit: every session was revoked: plugin=%s user=%s reason=%s", c.PluginID, c.UserID, status)
	r.answered(c.PluginID, c.Subject)
	if _, err := r.store.NoteExternalCheckFailure(c.PluginID, c.Subject, r.now().Add(interval), false); err != nil {
		r.logf("obelo: the sign-in re-check could not schedule the next check: plugin=%s user=%s: %v", c.PluginID, c.UserID, err)
	}
	return outcomeRevoked
}

// NoWorkingSignInPath answers, of users, the ids of those left with no way to
// sign in right now: no Local password, and every External identity they hold
// runs through a provider that is not registered, that the host has disabled,
// or that is failing its re-checks — or is itself one whose re-check failed.
// Their sessions are kept (ADR-0063 decision 9); the Admin Users page flags
// them until a provider they hold recovers.
func (r *Rechecker) NoWorkingSignInPath(users []store.User) ([]string, error) {
	// A provider the host disabled is judged live, by whether it builds now:
	// one re-enabled works again at once, before any re-check.
	counts := func(f identityFailure) bool { return f.reason != failDisabled }
	working := map[string]bool{}
	providerWorks := func(pluginID string) bool {
		if w, ok := working[pluginID]; ok {
			return w
		}
		w := false
		if reg, ok := r.reg.SignInProvider(pluginID); ok {
			if _, err := reg.New(pluginapi.Settings{Enabled: true}); err == nil {
				w = true
				r.mu.Lock()
				for _, f := range r.failed[pluginID] {
					if f.wide && counts(f) {
						w = false
					}
				}
				r.mu.Unlock()
			}
		}
		working[pluginID] = w
		return w
	}
	works := func(x store.ExternalIdentity) bool {
		if !providerWorks(x.PluginID) {
			return false
		}
		r.mu.Lock()
		f, failed := r.failed[x.PluginID][x.Subject]
		r.mu.Unlock()
		return !failed || !counts(f)
	}
	var out []string
	for _, u := range users {
		if u.PasswordHash != "" || u.Role == "remote" {
			continue
		}
		ids, err := r.store.ExternalIdentitiesByUser(u.ID)
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			continue
		}
		stranded := true
		for _, x := range ids {
			if works(x) {
				stranded = false
				break
			}
		}
		if stranded {
			out = append(out, u.ID)
		}
	}
	sort.Strings(out)
	return out, nil
}
