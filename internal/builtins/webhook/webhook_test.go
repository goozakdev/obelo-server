package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The Webhook Built-in stands in for every future sink an author writes: one
// document, one signature, bounded retrying, and nothing about the server in its
// surface. These tests play the operator's RECEIVER — the script at the other end
// — because that is the only party whose experience of this Plugin matters.

func testEvent() pluginapi.SinkEvent {
	return pluginapi.SinkEvent{
		ID:      "1b4e28ba-2fa1-11d2-883f-0016d3cca427",
		Type:    pluginapi.EventScanCompleted,
		At:      "2026-09-16T12:00:00Z",
		Library: pluginapi.EventEntity{ID: "lib-1", Name: "Movies", Kind: "movie"},
		Scan:    &pluginapi.EventScan{TitlesFound: 3, FilesFound: 4},
	}
}

// TestDeliverPostsOneSignedDocument: the whole promise of this sink, from the
// receiver's side. One POST, the event as JSON, and a signature the receiver can
// recompute from the raw body under the shared secret.
func TestDeliverPostsOneSignedDocument(t *testing.T) {
	type received struct {
		body      []byte
		signature string
		evType    string
		evID      string
	}
	var posts []received
	var calls atomic.Int32

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content type = %q, want application/json", ct)
		}
		body, _ := io.ReadAll(r.Body)
		posts = append(posts, received{
			body:      body,
			signature: r.Header.Get(HeaderSignature),
			evType:    r.Header.Get(HeaderEvent),
			evID:      r.Header.Get(HeaderEventID),
		})
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()

	sink, err := New(pluginapi.Settings{URL: target.URL, Secret: "topsecret"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ev := testEvent()
	if err := sink.Deliver(context.Background(), ev); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("posts = %d, want exactly 1", got)
	}
	got := posts[0]

	// The receiver's own check: recompute the MAC over the raw body.
	want := SignaturePrefix + Sign("topsecret", got.body)
	if got.signature != want {
		t.Fatalf("signature = %q, want %q", got.signature, want)
	}
	// And a forged body under the same secret must not verify.
	if got.signature == SignaturePrefix+Sign("topsecret", append(got.body, ' ')) {
		t.Fatal("the signature does not depend on the body")
	}
	// Nor must the right body under the wrong secret.
	if got.signature == SignaturePrefix+Sign("guessed", got.body) {
		t.Fatal("the signature does not depend on the secret")
	}

	if got.evType != ev.Type || got.evID != ev.ID {
		t.Fatalf("headers = %q/%q, want %q/%q", got.evType, got.evID, ev.Type, ev.ID)
	}

	var back pluginapi.SinkEvent
	if err := json.Unmarshal(got.body, &back); err != nil {
		t.Fatalf("the body is not the event: %v\nbody: %s", err, got.body)
	}
	if back.ID != ev.ID || back.Type != ev.Type || back.Library.Name != "Movies" {
		t.Fatalf("body = %+v, want the event", back)
	}
	if back.Scan == nil || back.Scan.TitlesFound != 3 {
		t.Fatalf("body scan = %+v, want the terminal counts", back.Scan)
	}
}

// TestDeliverRetriesABlipAndKeepsTheEventID: a target that fails once and then
// answers must not cost the operator the event — and the retry must be recognizable
// as the SAME event, which is the whole of what idempotency asks of a receiver.
func TestDeliverRetriesABlipAndKeepsTheEventID(t *testing.T) {
	var calls atomic.Int32
	var ids []string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ids = append(ids, r.Header.Get(HeaderEventID))
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	sink, err := New(pluginapi.Settings{URL: target.URL, Secret: "topsecret"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := sink.Deliver(context.Background(), testEvent()); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2 (one blip, then success)", got)
	}
	if ids[0] != ids[1] {
		t.Fatalf("event ids across attempts = %q and %q, want the same id", ids[0], ids[1])
	}
}

// TestDeliverGivesUpAfterABoundedBurst: a dead target costs a bounded number of
// attempts and then an error the host counts. It must not pile up work.
func TestDeliverGivesUpAfterABoundedBurst(t *testing.T) {
	var calls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer target.Close()

	sink, err := New(pluginapi.Settings{URL: target.URL, Secret: "topsecret"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = sink.Deliver(context.Background(), testEvent())
	if err == nil {
		t.Fatal("Deliver returned nil for a target that answered 500 every time")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("error = %v, want it to name the status the target answered", err)
	}
	if got := calls.Load(); got != maxAttempts {
		t.Fatalf("attempts = %d, want %d", got, maxAttempts)
	}
}

// TestDeliverHonorsTheHostDeadline: the host sets the deadline, and a target that
// never answers must not hold the call past it. This is what makes a hanging
// webhook cost one worker rather than the server's responsiveness.
func TestDeliverHonorsTheHostDeadline(t *testing.T) {
	hang := make(chan struct{})
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hang
	}))
	// Release the handler BEFORE shutting the server down: httptest.Close waits for
	// outstanding handlers, so closing in the other order deadlocks the test.
	defer target.Close()
	defer close(hang)

	sink, err := New(pluginapi.Settings{URL: target.URL, Secret: "topsecret"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := sink.Deliver(ctx, testEvent()); err == nil {
		t.Fatal("Deliver returned nil against a target that never answered")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Deliver took %v, want it bounded by the 150ms deadline", elapsed)
	}
}

// TestNewRefusesAnUnsignedOrUndirectedSink: a sink with no secret would post
// documents a receiver cannot trust, and one with no URL has nowhere to post. The
// honest answer to either is no sink at all (PRD story 53).
func TestNewRefusesAnUnsignedOrUndirectedSink(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   pluginapi.Settings
	}{
		{"no url", pluginapi.Settings{Secret: "s"}},
		{"no secret", pluginapi.Settings{URL: "https://sink.test/hook"}},
		{"neither", pluginapi.Settings{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink, err := New(tc.in)
			if !errors.Is(err, ErrNoTarget) {
				t.Fatalf("New(%+v) error = %v, want ErrNoTarget", tc.in, err)
			}
			if sink != nil {
				t.Fatal("New returned a sink it had already refused to build")
			}
		})
	}
}

// TestSinkImplementsTheContract is the compile-time claim made explicit: the
// Webhook's whole surface is pluginapi.EventSink, which is what lets the host wire
// it without knowing anything about HTTP.
func TestSinkImplementsTheContract(t *testing.T) {
	var _ pluginapi.EventSink = (*Sink)(nil)
	var _ pluginapi.EventSinkFactory = New
}
