//go:build !tailscale

// The tag-less half of the seam (ADR-0043). A plain `go build` has no
// `tailscale.com` linked in — that is the point of the build tag, and it is what
// keeps day-to-day development on a fast loop: 274 packages against 610, a 21 MiB
// binary against 40, and a cold build of 5.8 s against 14.9 s (ADR-0043's table,
// measured). The module GRAPH grows either way — go.mod is one file — so what the
// tag buys is compiled code, not a shorter list to audit.
//
// What this file exists to prevent is the tag-less build failing to COMPILE. The
// constructor has to exist in both builds, or every caller of it grows a build tag
// too, and the "one file behind the tag" property that makes the CI matrix cheap
// is gone by the second commit.

package tailnet

import (
	"context"
	"net"
)

// Supported reports that this build cannot join a Tailnet. It is what GET
// /server's features.tailscale advertises — a capability of THIS BINARY, not the
// existence of a route (the Tailnet routes are served in both builds, so a client
// gets an explanation rather than a 404 that reads like a typo'd path).
const Supported = false

// NewNode returns a node that cannot join anything and says so.
//
// It is a working Node rather than nil so that the failure arrives from ONE place
// with ONE message: every verb funnels into the state machine, which records
// ErrNoNode as the node's last error, and the settings screen then explains "this
// build has no Tailnet support" instead of showing a button that silently does
// nothing.
func NewNode() Node { return unsupportedNode{} }

// unsupportedNode answers every operation with ErrNoNode, which names the build
// and the two places to get one that works.
type unsupportedNode struct{}

func (unsupportedNode) Start(context.Context, Config) error { return ErrNoNode }

// Status is stopped, not error: nothing has gone wrong until somebody actually
// asks this node to do something, and a settings panel that opens red on a server
// where the feature was never switched on would be a lie about the resting state.
func (unsupportedNode) Status() Status { return Status{State: StateStopped} }

// Changes is a channel that never carries anything, because nothing here ever
// transitions. It is non-nil so a consumer's select blocks on it harmlessly
// instead of spinning on a nil channel, and it is never closed — the contract the
// seam states for every Node.
func (unsupportedNode) Changes() <-chan Status { return make(chan Status) }

func (unsupportedNode) Listen(string, string) (net.Listener, error)    { return nil, ErrNoNode }
func (unsupportedNode) ListenTLS(string, string) (net.Listener, error) { return nil, ErrNoNode }

// Close succeeds: there is nothing running, and shutdown paths must not have to
// care which build they are in.
func (unsupportedNode) Close() error { return nil }
