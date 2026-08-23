//go:build !tailscale

package api_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/tailnet"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// The tag-less half of the feature-flag pair (ADR-0043). Its twin is
// tailnet_feature_tag_test.go, which asserts the opposite in the other build, and
// the two exist as separate files rather than one test reading the build constant
// because a test that compares the flag against the same constant the flag is
// computed from asserts nothing at all.
//
// This is the variant an operator gets from a plain `go build`, and CLAUDE.md
// records what happens in this repository when two guards want opposite things and
// only one side is automated: the committed index.html was wrong for a month while
// its guard reported success throughout. A build tag is that shape again.

// TestTailscaleFeatureFollowsTheBuild: features.tailscale is FALSE here, and the
// Tailnet routes are served anyway.
//
// Both halves matter. The flag is how a client knows not to offer a Connect button
// that could only fail; serving the routes regardless is how the operator who goes
// looking gets told WHY — an error naming the build, not a 404 that reads exactly
// like a mistyped path.
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
	if advertised {
		t.Error("features[\"tailscale\"] = true in a build without -tags tailscale — " +
			"every client would offer remote access this binary cannot provide")
	}

	// The routes exist in both builds, deliberately (ADR-0043). Unauthenticated, so
	// a live route answers 401 — which is all that is being measured here.
	resp := srv.Do(http.MethodGet, "/api/v1/settings/tailscale", nil)
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		t.Error("GET /settings/tailscale 404s in the tag-less build; it must answer with an " +
			"error that names the build, because a 404 is indistinguishable from a typo")
	}
}

// TestTailnetVerbsNameTheBuildWhenItCannotJoin: the operator who presses Connect
// on a binary with no Tailnet support gets told exactly that, in words, and the
// server keeps serving.
//
// It is answered 200 with an error STATE rather than an HTTP error on purpose,
// which is ADR-0043's posture: the request succeeded, the node is simply not
// something this build has. A 500 would claim a fault the server does not have.
func TestTailnetVerbsNameTheBuildWhenItCannotJoin(t *testing.T) {
	// tailnet.NewNode() is exactly what cmd/obelo wires in this build — the stub that
	// answers everything with ErrNoNode — rather than the nil the harness defaults to,
	// so this walks the same code the shipped tag-less binary walks.
	srv := testharness.New(t, testharness.WithTailnet(tailnet.NewNode()))
	token := adminToken(t, srv)

	v := postTailnet(t, srv, token, "connect")
	if v.Status.State != "error" {
		t.Errorf("state after connect = %q, want %q", v.Status.State, "error")
	}
	for _, want := range []string{"-tags tailscale", "Docker image", "release binary"} {
		if !strings.Contains(v.Status.LastError, want) {
			t.Errorf("lastError does not mention %q: %s", want, v.Status.LastError)
		}
	}

	// And the server is still serving, which is the part that must never be in
	// doubt: remote access being unavailable costs remote access and nothing else.
	if status, body := srv.GET("/api/v1/server", nil); status != http.StatusOK {
		t.Fatalf("handshake after a refused connect = %d, want 200; body: %s", status, body)
	}
}

// TestUnsupportedBuildLeavesNoStateDirectory: a build that cannot join a Tailnet
// must not leave the feature's furniture in the data directory. The operator here
// has actually pressed Connect — so this is not the "off by default is inert"
// criterion, it is the one after it: a failure that wrote nothing must clean up
// after itself, because the alternative is a directory that looks like state and
// holds none.
func TestUnsupportedBuildLeavesNoStateDirectory(t *testing.T) {
	srv := testharness.New(t, testharness.WithTailnet(tailnet.NewNode()))
	token := adminToken(t, srv)

	postTailnet(t, srv, token, "connect")

	if _, err := os.Stat(filepath.Join(srv.DataDir, "tailscale")); !os.IsNotExist(err) {
		t.Errorf("a build with no Tailnet support created a state directory anyway (stat err = %v)", err)
	}
}
