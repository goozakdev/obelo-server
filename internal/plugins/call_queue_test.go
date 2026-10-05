package plugins

import (
	"context"
	"errors"
	"testing"
	"time"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// A call that does not queue in its budget (every seam but sign-in) must still
// give up waiting for callMu when its own ctx ends (R03-01).
func TestQueuedCallHonoursCallerCtx(t *testing.T) {
	parallel(t)
	p := newPlugin("q", t.TempDir(), Options{CallTimeout: time.Minute})
	p.callMu <- struct{}{} // another call holds the instance

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() {
		done <- p.callGuestUnder(ctx, callPolicy{}, "x", nil, func(context.Context) any { return nil }, nil)
	}()
	select {
	case err := <-done:
		if !errors.Is(err, errQueuedPastDeadline) {
			t.Fatalf("err = %v, want errQueuedPastDeadline", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled caller is still blocked on callMu")
	}
}

// A metadata call's Settings (secret included) must not be visible to settings_get
// while the call is still queued behind another call on the Plugin (R03-06).
func TestQueuedMetadataCallDoesNotPublishSettings(t *testing.T) {
	parallel(t)
	p := newPlugin("q", t.TempDir(), Options{CallTimeout: time.Minute})
	p.callMu <- struct{}{}
	g := &guestProvider{p: p, settings: pluginapi.Settings{Enabled: true, Secret: "meta-secret"}}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.call(ctx, exportMetadataLookup, nil, nil) }()
	time.Sleep(100 * time.Millisecond)

	if got := p.currentSettings(context.Background()); got.Secret != "" || got.Enabled {
		t.Fatalf("queued call's settings visible: %+v", got)
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("queued call succeeded without ever running")
	}
}
