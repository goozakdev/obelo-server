//go:build tailscale

package api_test

import (
	"net/http"
	"testing"

	"github.com/goozakdev/obelo-server/internal/tailnet"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// The tagged half of the feature-flag pair (ADR-0043). See its twin,
// tailnet_feature_notag_test.go, for why these are two files rather than one test
// branching on the build constant.
//
// This is the variant the Docker image and the release binaries are, and it is the
// one an operator actually runs — which is exactly why the suite has to be run
// both ways: if only one variant is exercised, it will be the one users do not
// have.

// TestTailscaleFeatureFollowsTheBuild: features.tailscale is TRUE here, because
// `tailscale.com` is linked in and this binary can genuinely join a Tailnet.
//
// The flag says nothing about whether the feature is switched ON — that is the
// settings endpoint's business, and this test deliberately enables nothing. A flag
// that tracked the setting would tell every client to hide the panel on the
// perfectly good server the operator has not configured yet.
func TestTailscaleFeatureFollowsTheBuild(t *testing.T) {
	srv := testharness.New(t)

	var got serverInfo
	if status, body := srv.GET("/api/v1/server", &got); status != http.StatusOK {
		t.Fatalf("handshake status = %d, want 200; body: %s", status, body)
	}
	advertised, present := got.Features["tailscale"]
	if !present {
		t.Fatal("the features map has no \"tailscale\" key; a client cannot tell the two builds apart")
	}
	if !advertised {
		t.Error("features[\"tailscale\"] = false in a build WITH -tags tailscale — " +
			"every client would hide remote access on the binary that supports it")
	}

	// Serving the routes is not the flag's claim, but it is the other half of the
	// contract and costs nothing to pin here too.
	resp := srv.Do(http.MethodGet, "/api/v1/settings/tailscale", nil)
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		t.Error("GET /settings/tailscale 404s in the tagged build")
	}
}

// TestBootSurvivesAnUnreachableCoordinationServer drives the REAL `tsnet` adapter
// through a REAL boot: the feature enabled, the node told to connect at boot, and
// the coordination server pointed at an address where nothing is listening.
//
// This is the criterion ADR-0043 cares most about — "a coordination server that is
// unreachable is a warning, never a boot failure, and never affects the LAN
// listener" — and it is the one the Fake cannot fully answer, because the Fake
// fails on command whereas the real thing fails by never finishing. Nothing here
// touches the network: 127.0.0.1:1 refuses instantly.
func TestBootSurvivesAnUnreachableCoordinationServer(t *testing.T) {
	srv := testharness.New(t,
		testharness.WithTailnet(tailnet.NewNode()),
		testharness.WithTailnetEnabled(true),
		testharness.WithTailnetControlURL("http://127.0.0.1:1"),
		testharness.WithTailnetHostname("obelo"),
	)

	// The server booted at all — the assertion that matters, and the one that fails
	// if a join ever becomes something a boot waits on or dies of.
	if status, body := srv.GET("/api/v1/server", nil); status != http.StatusOK {
		t.Fatalf("handshake = %d, want 200; body: %s", status, body)
	}

	// And it is honest about the node: anything but "running" is a true answer to
	// "we cannot reach control", but claiming to be up would send an operator
	// looking for a MagicDNS name that will never resolve.
	token := adminToken(t, srv)
	v, body := getTailnet(t, srv, token)
	if v.Status.State == "running" {
		t.Errorf("the node reports itself running with no reachable coordination server; body: %s", body)
	}
	if !v.Enabled {
		t.Error("the enabled setting did not survive the boot; an enabled node must come back up")
	}
}
