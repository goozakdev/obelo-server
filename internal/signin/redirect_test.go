package signin

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"
	"time"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The redirect flow's bookkeeping, with a clock the test holds: how long a
// started sign-in lives, and which one gives way when too many are in flight,
// in all and from one client.

// plainProvider is a plain OAuth2 redirect provider with no network: it sends
// the browser to a fixed authorize URL and accepts every code as one identity.
type plainProvider struct{}

func (plainProvider) CheckPassword(context.Context, pluginapi.SignInPasswordRequest) (pluginapi.SignInPasswordResponse, error) {
	return pluginapi.SignInPasswordResponse{}, nil
}

func (plainProvider) AuthorizeURL(_ context.Context, req pluginapi.SignInAuthorizeRequest) (pluginapi.SignInAuthorizeResponse, error) {
	return pluginapi.SignInAuthorizeResponse{URL: "https://idp.example.test/authorize?state=" + url.QueryEscape(req.State)}, nil
}

func (plainProvider) Exchange(context.Context, pluginapi.SignInExchangeRequest) (pluginapi.SignInExchangeResponse, error) {
	return pluginapi.SignInExchangeResponse{
		Accepted: true,
		Identity: &pluginapi.SignInIdentity{Subject: "subject-ada", Username: "ada"},
	}, nil
}

// testRedirects is the redirect flow over one plainProvider, "plain", whose clock
// is *now.
func testRedirects(t *testing.T, now *time.Time) *Redirects {
	t.Helper()
	reg := pluginapi.NewRegistry()
	reg.RegisterSignInProvider(pluginapi.SignInProviderRegistration{
		Descriptor: pluginapi.Descriptor{
			Slug:         "plain",
			Name:         "Plain",
			Capabilities: []pluginapi.Capability{pluginapi.CapabilityRedirectSignIn},
		},
		New: func(pluginapi.Settings) (pluginapi.SignInProvider, error) { return plainProvider{}, nil },
	})
	r := NewRedirects(reg)
	r.now = func() time.Time { return *now }
	return r
}

// stateOf is the state the host put on a started sign-in's authorize URL.
func stateOf(t *testing.T, s Started) string {
	t.Helper()
	u, err := url.Parse(s.URL)
	if err != nil {
		t.Fatalf("the authorize URL %q does not parse: %v", s.URL, err)
	}
	return u.Query().Get("state")
}

func TestAStartedSignInExpires(t *testing.T) {
	now := time.Now()
	r := testRedirects(t, &now)
	ctx := context.Background()

	started, err := r.Start(ctx, "plain", "https://obelo.example/sign-in/callback", "client-a")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	now = now.Add(redirectTTL)
	if _, _, err := r.Complete(ctx, stateOf(t, started), "code", started.Binding); !errors.Is(err, ErrRedirectRefused) {
		t.Fatalf("a callback %v after the start = %v, want refused", redirectTTL, err)
	}

	started, err = r.Start(ctx, "plain", "https://obelo.example/sign-in/callback", "client-a")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	now = now.Add(redirectTTL - time.Second)
	if _, _, err := r.Complete(ctx, stateOf(t, started), "code", started.Binding); err != nil {
		t.Fatalf("a callback inside %v = %v, want accepted", redirectTTL, err)
	}
}

// TestAFullTableEvictsTheOldestStartInAll: at the cap in all, a start from a
// client never seen still works, and it is the oldest sign-in in flight, not
// the newest, that is given up.
func TestAFullTableEvictsTheOldestStartInAll(t *testing.T) {
	now := time.Now()
	r := testRedirects(t, &now)
	ctx := context.Background()

	var started []Started
	for i := 0; i < maxPendingRedirects; i++ {
		s, err := r.Start(ctx, "plain", "https://obelo.example/sign-in/callback", fmt.Sprintf("client-%d", i))
		if err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		started = append(started, s)
	}
	newest, err := r.Start(ctx, "plain", "https://obelo.example/sign-in/callback", "one-client-more")
	if err != nil {
		t.Fatalf("start past %d in flight = %v, want it to work", maxPendingRedirects, err)
	}
	if _, _, err := r.Complete(ctx, stateOf(t, newest), "code", newest.Binding); err != nil {
		t.Fatalf("the newest start's callback = %v, want accepted", err)
	}
	if _, _, err := r.Complete(ctx, stateOf(t, started[0]), "code", started[0].Binding); !errors.Is(err, ErrRedirectRefused) {
		t.Fatalf("the oldest start's callback = %v, want refused: it was the one evicted", err)
	}
	if _, _, err := r.Complete(ctx, stateOf(t, started[1]), "code", started[1].Binding); err != nil {
		t.Fatalf("the second-oldest start's callback = %v, want accepted: only one was evicted", err)
	}
}

// TestOneClientsStartsEvictItsOwnOldest: at the per-client cap, the client's
// next start works and gives up that client's oldest sign-in, and nobody
// else's.
func TestOneClientsStartsEvictItsOwnOldest(t *testing.T) {
	now := time.Now()
	r := testRedirects(t, &now)
	ctx := context.Background()

	other, err := r.Start(ctx, "plain", "https://obelo.example/sign-in/callback", "somebody-else")
	if err != nil {
		t.Fatalf("another client's start: %v", err)
	}
	var started []Started
	for i := 0; i < maxPendingRedirectsPerClient; i++ {
		s, err := r.Start(ctx, "plain", "https://obelo.example/sign-in/callback", "greedy")
		if err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		started = append(started, s)
	}
	newest, err := r.Start(ctx, "plain", "https://obelo.example/sign-in/callback", "greedy")
	if err != nil {
		t.Fatalf("start past %d from one client = %v, want it to work", maxPendingRedirectsPerClient, err)
	}
	if _, _, err := r.Complete(ctx, stateOf(t, newest), "code", newest.Binding); err != nil {
		t.Fatalf("the newest start's callback = %v, want accepted", err)
	}
	if _, _, err := r.Complete(ctx, stateOf(t, started[0]), "code", started[0].Binding); !errors.Is(err, ErrRedirectRefused) {
		t.Fatalf("the client's oldest start's callback = %v, want refused: it was the one evicted", err)
	}
	if _, _, err := r.Complete(ctx, stateOf(t, started[1]), "code", started[1].Binding); err != nil {
		t.Fatalf("the client's second-oldest start's callback = %v, want accepted", err)
	}
	if _, _, err := r.Complete(ctx, stateOf(t, other), "code", other.Binding); err != nil {
		t.Fatalf("another client's older start's callback = %v, want accepted: only greedy's own are evicted", err)
	}
}

// TestAnIPv6ClientWithManyAddressesIsOneClient: one IPv6 client may hold a
// whole /64, and a start from every address in it must not fill the table for
// everybody else. Its starts count as one client's — they give up their own
// oldest — so another client's sign-in in flight survives them all.
func TestAnIPv6ClientWithManyAddressesIsOneClient(t *testing.T) {
	now := time.Now()
	r := testRedirects(t, &now)
	ctx := context.Background()

	other, err := r.Start(ctx, "plain", "https://obelo.example/sign-in/callback", "203.0.113.9")
	if err != nil {
		t.Fatalf("another client's start: %v", err)
	}
	var last Started
	for i := 0; i < maxPendingRedirects; i++ {
		last, err = r.Start(ctx, "plain", "https://obelo.example/sign-in/callback", fmt.Sprintf("2001:db8:0:1::%x", i+1))
		if err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
	}
	if _, _, err := r.Complete(ctx, stateOf(t, other), "code", other.Binding); err != nil {
		t.Fatalf("another client's start's callback = %v, want accepted: one /64 must not fill the table", err)
	}
	if _, _, err := r.Complete(ctx, stateOf(t, last), "code", last.Binding); err != nil {
		t.Fatalf("the /64's newest start's callback = %v, want accepted", err)
	}

	// A different /64 is a different client.
	neighbour, err := r.Start(ctx, "plain", "https://obelo.example/sign-in/callback", "2001:db8:0:2::1")
	if err != nil {
		t.Fatalf("a neighbouring /64's start: %v", err)
	}
	for i := 0; i < maxPendingRedirectsPerClient; i++ {
		if _, err := r.Start(ctx, "plain", "https://obelo.example/sign-in/callback", fmt.Sprintf("2001:db8:0:1::%x", i+1)); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
	}
	if _, _, err := r.Complete(ctx, stateOf(t, neighbour), "code", neighbour.Binding); err != nil {
		t.Fatalf("a neighbouring /64's start's callback = %v, want accepted: it is another client", err)
	}
}

// TestAnIPv4MappedAddressIsItsIPv4Client: "::ffff:a.b.c.d" is the IPv4 address
// a.b.c.d as a dual-stack listener reports it, so starts in either form are one
// client's — sixteen in the mapped form give up the oldest start made in the
// plain one.
func TestAnIPv4MappedAddressIsItsIPv4Client(t *testing.T) {
	now := time.Now()
	r := testRedirects(t, &now)
	ctx := context.Background()

	first, err := r.Start(ctx, "plain", "https://obelo.example/sign-in/callback", "203.0.113.9")
	if err != nil {
		t.Fatalf("the plain-form start: %v", err)
	}
	for i := 0; i < maxPendingRedirectsPerClient; i++ {
		if _, err := r.Start(ctx, "plain", "https://obelo.example/sign-in/callback", "::ffff:203.0.113.9"); err != nil {
			t.Fatalf("mapped start %d: %v", i, err)
		}
	}
	if _, _, err := r.Complete(ctx, stateOf(t, first), "code", first.Binding); err == nil {
		t.Fatal("the plain-form start survived sixteen more from the same address in its mapped form; want it given up as the client's oldest")
	}
}
