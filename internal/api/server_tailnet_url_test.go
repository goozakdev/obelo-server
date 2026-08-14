package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/marioquake/obelo-server/internal/tailnet"
	"github.com/marioquake/obelo-server/internal/testharness"
)

// The Tailnet address on the handshake (ADR-0043). The field is gated on
// authentication; the ROUTE is not, and the difference between those two
// sentences is the whole point of this file.

type handshakeWithTailnet struct {
	TailnetURL *string `json:"tailnetURL"`
}

// running is a Fake settled into a running node with an FQDN, optionally with
// Tailnet HTTPS bound.
func running(fqdn string, httpsBound bool) *tailnet.Fake {
	st := tailnet.Status{State: tailnet.StateRunning, FQDN: fqdn, HTTPSBound: httpsBound}
	return &tailnet.Fake{Fresh: st, Returning: st}
}

// TestServerInfoNeverAnswers401 is named for the reason it exists rather than
// for the mechanism it checks.
//
// The obvious way to publish an authenticated-only field is to wrap the route in
// requireAuth. That would answer a missing or invalid bearer with 401 — and the
// Apple client drives token-drop from exactly that status, so a handshake that
// began 401-ing would present to every client in the house as a revoked token
// and sign people out. The handshake must answer 200 to anyone who can reach the
// port. Only the FIELD is gated.
//
// If this test ever fails, do not "fix" it by relaxing the assertion.
func TestServerInfoNeverAnswers401(t *testing.T) {
	srv := testharness.New(t,
		testharness.WithTailnet(running("obelo.tail1a2b.ts.net", true)),
		testharness.WithTailnetEnabled(true))
	adminToken(t, srv) // a real token exists, so "invalid" below means invalid, not "no users yet"

	for _, tc := range []struct {
		name, token string
	}{
		{"no bearer at all", ""},
		{"malformed bearer", "not-a-token"},
		{"well-formed but unknown", "obelo_00000000000000000000000000000000"},
		{"revoked-looking garbage", "Bearer Bearer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got handshakeWithTailnet
			var status int
			var body []byte
			if tc.token == "" {
				status, body = srv.GET("/api/v1/server", &got)
			} else {
				status, body = srv.AuthGET("/api/v1/server", tc.token, &got)
			}
			if status != http.StatusOK {
				t.Fatalf("GET /server with %s = %d, want 200. A 401 here reads as a REVOKED TOKEN to "+
					"the Apple client and signs the user out; the handshake is unauthenticated and the "+
					"field is what gets gated (ADR-0043). body: %s", tc.name, status, body)
			}
			if got.TailnetURL != nil {
				t.Errorf("tailnetURL = %q for %s, want null — an unauthenticated caller must not learn "+
					"the Tailnet name; a scanner on a port-forward would learn the tailnet exists and "+
					"what it is called", *got.TailnetURL, tc.name)
			}
		})
	}
}

// TestTailnetURLPresentAndNullForEveryCaller pins that the key is always in the
// body, never omitted. `null` is this server SAYING it has no Tailnet address,
// which is a different statement from a field that is simply absent — and the
// client contract keys on the field being explicitly nullable (the same reasoning
// keyExpiry carries on the settings payload).
func TestTailnetURLPresentAndNullForEveryCaller(t *testing.T) {
	srv := testharness.New(t) // no Tailnet node at all
	_, body := srv.GET("/api/v1/server", nil)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("handshake is not an object: %v", err)
	}
	v, ok := raw["tailnetURL"]
	if !ok {
		t.Fatal("tailnetURL is ABSENT from the handshake. It must be present and null: absent and null " +
			"are the same to a decoder but not to a reader, and the contract says never omitted")
	}
	if string(v) != "null" {
		t.Errorf("tailnetURL = %s with no Tailnet node, want null", v)
	}
}

// TestTailnetURLIsTheOriginAchievedNotRequested is the pair of rules the client
// team asked for: the name only, and the scheme that actually bound.
func TestTailnetURLIsTheOriginAchievedNotRequested(t *testing.T) {
	for _, tc := range []struct {
		name       string
		fqdn       string
		httpsBound bool
		want       string
	}{
		{"https once :443 is bound", "obelo.tail1a2b.ts.net", true, "https://obelo.tail1a2b.ts.net"},
		{"http while only :80 is bound", "obelo.tail1a2b.ts.net", false, "http://obelo.tail1a2b.ts.net"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := testharness.New(t,
				testharness.WithTailnet(running(tc.fqdn, tc.httpsBound)),
				testharness.WithTailnetEnabled(true))
			// Play the supervisor's part. httpsBound is sourced from an OBSERVED bind
			// reported by cmd/obelo's listener supervisor, never from the node's own
			// opinion — that separation is the whole of issue 06, and it is why the
			// handler cannot simply read the Fake here. Reporting it explicitly is
			// also the honest shape: nothing below cmd/obelo knows whether :443 came up.
			srv.Tailnet().ReportHTTPS(tc.httpsBound, "")
			token := adminToken(t, srv)

			var got handshakeWithTailnet
			status, body := srv.AuthGET("/api/v1/server", token, &got)
			if status != http.StatusOK {
				t.Fatalf("status = %d, body: %s", status, body)
			}
			if got.TailnetURL == nil {
				t.Fatalf("tailnetURL is null for a running node with an FQDN; body: %s", body)
			}
			if *got.TailnetURL != tc.want {
				t.Errorf("tailnetURL = %q, want %q. The scheme must follow the OBSERVED bind, never the "+
					"operator's request — sending https:// for a listener that never came up is how the "+
					"admin UI once handed people an address that refused connections", *got.TailnetURL, tc.want)
			}
		})
	}
}

// TestTailnetURLCarriesNoAddresses guards the name-only rule, which has three
// independent justifications: the addresses are what the name exists to replace
// and they move; no certificate can match an IP literal, so the Apple media
// channel (which verifies against a bundled CA and fails CLOSED) would break
// while JSON kept working; and App Transport Security refuses cleartext to a
// 100.64.0.0/10 literal anyway, so there is not even an escape hatch.
func TestTailnetURLCarriesNoAddresses(t *testing.T) {
	srv := testharness.New(t,
		testharness.WithTailnet(running("obelo.tail1a2b.ts.net", false)),
		testharness.WithTailnetEnabled(true))
	token := adminToken(t, srv)

	var got handshakeWithTailnet
	_, body := srv.AuthGET("/api/v1/server", token, &got)
	if got.TailnetURL == nil {
		t.Fatalf("tailnetURL is null; body: %s", body)
	}
	if u := *got.TailnetURL; u != "http://obelo.tail1a2b.ts.net" {
		t.Errorf("tailnetURL = %q — it must be the MagicDNS name alone, with no port and no address", u)
	}
}

// TestTailnetURLNullWhileTheNodeHasNoName covers the states an operator actually
// sits in: the feature off, the node down, or up but not yet carrying an FQDN.
// Each must publish null, because each obliges a client to CLEAR a stored
// address rather than keep probing one that no longer answers.
func TestTailnetURLNullWhileTheNodeHasNoName(t *testing.T) {
	for _, tc := range []struct {
		name    string
		node    *tailnet.Fake
		enabled bool
	}{
		// The feature off entirely — the default, and the state every fresh install
		// is in. The node is never started, so there is nothing to publish.
		{"feature off, node never started", &tailnet.Fake{Fresh: tailnet.Status{State: tailnet.StateStopped}}, false},
		{"running but no FQDN yet", running("", false), true},
		{"waiting on an interactive login", &tailnet.Fake{
			Fresh: tailnet.Status{State: tailnet.StateNeedsLogin, LoginURL: "https://login.tailscale.com/a/x"},
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := []testharness.Option{testharness.WithTailnet(tc.node)}
			if tc.enabled {
				opts = append(opts, testharness.WithTailnetEnabled(true))
			}
			srv := testharness.New(t, opts...)
			token := adminToken(t, srv)

			var got handshakeWithTailnet
			_, body := srv.AuthGET("/api/v1/server", token, &got)
			if got.TailnetURL != nil {
				t.Errorf("tailnetURL = %q for %s, want null; body: %s", *got.TailnetURL, tc.name, body)
			}
		})
	}
}
