//go:build !tailscale

package tailnet_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/tailnet"
)

// The tag-less half of the CI matrix (ADR-0043). This file only compiles in the
// default build, and its whole job is to prove that build fails LEGIBLY: a plain
// `go build` has no `tailscale.com` linked in, and an operator who tries to use
// remote access there must be told that, in those words, rather than getting a
// silent no-op, a 404 that reads like a typo'd path, or a compile error in
// somebody else's package.

// TestUnsupportedNodeNamesTheBuild: every operation answers with the error that
// says which build this is and where to get one that works.
func TestUnsupportedNodeNamesTheBuild(t *testing.T) {
	node := tailnet.NewNode()
	if node == nil {
		t.Fatal("NewNode returned nil; the tag-less build needs a Node that explains itself, not one that cannot be called")
	}

	if err := node.Start(context.Background(), tailnet.Config{Hostname: "obelo"}); !errors.Is(err, tailnet.ErrNoNode) {
		t.Errorf("Start = %v, want ErrNoNode", err)
	}
	if _, err := node.Listen("tcp", ":80"); !errors.Is(err, tailnet.ErrNoNode) {
		t.Errorf("Listen = %v, want ErrNoNode", err)
	}
	if _, err := node.ListenTLS("tcp", ":443"); !errors.Is(err, tailnet.ErrNoNode) {
		t.Errorf("ListenTLS = %v, want ErrNoNode", err)
	}
	// Close succeeds: nothing is running, and a shutdown path must not have to know
	// which build it is in.
	if err := node.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
	// Stopped, not error. Nothing has gone wrong until somebody asks for something,
	// and a settings panel that opens red on a server where the feature was never
	// switched on is a lie about the resting state.
	if got := node.Status().State; got != tailnet.StateStopped {
		t.Errorf("Status = %q, want %q", got, tailnet.StateStopped)
	}

	// The message has a job: it must name the build and point at the two artifacts
	// that carry the tag. "not supported" alone leaves an operator unable to tell
	// "not in your binary" from "broken".
	msg := tailnet.ErrNoNode.Error()
	for _, want := range []string{"-tags tailscale", "Docker image", "release binary"} {
		if !strings.Contains(msg, want) {
			t.Errorf("ErrNoNode does not mention %q: %s", want, msg)
		}
	}
}

// TestSupportedIsFalseWithoutTheTag pins the build constant that GET /server's
// features.tailscale is computed from. It is the whole reason a client can tell
// the two binaries apart, since the routes exist in both.
func TestSupportedIsFalseWithoutTheTag(t *testing.T) {
	if tailnet.Supported {
		t.Error("tailnet.Supported is true in a build without -tags tailscale")
	}
}
