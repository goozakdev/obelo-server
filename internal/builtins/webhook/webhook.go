// Package webhook is the Webhook Built-in: the Plugin that provides the Event
// sink Extension point by POSTing one signed JSON document per event to a URL an
// Admin typed (ADR-0057 decision 6).
//
// It is the simplest possible sink on purpose. It knows nothing about the server
// — not the Broker, not the translator, not the settings rows — and its whole
// surface is pluginapi.EventSink: one call, a whole event in, an error or nil out.
// A sink has no domain judgment to report, so there is no Outcome here; everything
// that can go wrong is transport, which the host counts and then forgets.
//
// The signature is what makes the document trustworthy at the other end. A
// receiver that knows the secret recomputes HMAC-SHA256 over the RAW BODY and
// compares it to the signature header; anything that does not match was not sent
// by this server. That is also why enabling the sink requires a secret: posting
// unsigned would hand the operator a receiver they cannot defend.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/goozakdev/obelo-server/internal/safefetch"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// Slug is the stable Plugin identity: the key persisted in the event-sink settings
// row and used in the settings API routes.
const Slug = "webhook"

// The headers each POST carries. The event type and id are duplicated out of the
// body so a receiver can route and deduplicate from headers alone — before it
// parses, and before it has decided whether to trust the body at all.
const (
	// HeaderEvent is the event type ("scan.completed").
	HeaderEvent = "X-Obelo-Event"
	// HeaderEventID is the stable event id the sink call is idempotent on.
	HeaderEventID = "X-Obelo-Event-Id"
	// HeaderSignature is "sha256=" + lowercase hex HMAC-SHA256 of the raw body
	// under the configured secret. The prefix names the algorithm so a future one
	// can be added without a receiver guessing.
	HeaderSignature = "X-Obelo-Signature"
	// SignaturePrefix is the algorithm tag on HeaderSignature.
	SignaturePrefix = "sha256="
)

// maxAttempts bounds the retry burst inside ONE call's deadline (PRD §The
// Webhook): a brief blip at the target should not lose the event, and a dead
// target must not pile up work. Three attempts with a short backoff fits
// comfortably inside the host's per-call deadline and stops there — durability is
// explicitly not this slice's job (delivery is best-effort, ADR-0057 decision 6).
const maxAttempts = 3

// retryBackoff is the pause before each retry. Deliberately small: the whole burst
// has to finish inside the deadline, and the queue behind it drops its oldest
// event while this call is still running.
const retryBackoff = 100 * time.Millisecond

// maxResponseBytes caps how much of a target's response body is read for the error
// detail. A hostile or misconfigured receiver answering an enormous body must not
// cost this server memory; nothing in the response is used for anything else.
const maxResponseBytes = 4 << 10

// ErrNoTarget is returned by the factory when a sink is built with no URL or no
// secret. The settings endpoint refuses that combination first, so reaching this
// means something built a sink the API would not have — and the honest answer is
// to have no sink at all rather than one that posts nowhere, or unsigned.
var ErrNoTarget = errors.New("webhook: a target URL and a signing secret are both required")

// Sink is the Webhook Plugin: a target URL, a signing secret, and an HTTP client
// carrying the server's one redirect policy.
type Sink struct {
	URL        string
	Secret     string
	HTTPClient *http.Client
}

// Sink implements the Event sink Extension point.
var _ pluginapi.EventSink = (*Sink)(nil)

// New builds the Plugin from the Settings an Admin saved: URL is the target,
// Secret the signing key. It is the pluginapi.EventSinkFactory this Built-in
// registers with.
func New(s pluginapi.Settings) (pluginapi.EventSink, error) {
	if s.URL == "" || s.Secret == "" {
		return nil, ErrNoTarget
	}
	return &Sink{
		URL:    s.URL,
		Secret: s.Secret,
		// The same guarded client every outbound fetch of a URL this server did not
		// choose runs under (safefetch): the redirect chain is bounded and a hop into
		// loopback/RFC1918/link-local space is refused. The FIRST hop is deliberately
		// not checked — a self-hosted operator pointing a webhook at a box on their
		// own LAN is the whole point of this product (ADR-0001) — but where that box
		// then bounces us is not their decision to have made.
		HTTPClient: safefetch.Client(0),
	}, nil
}

// Deliver posts one event and returns nil once the target has accepted it. A
// non-2xx or a transport failure is retried a bounded number of times inside the
// context's deadline; exhaustion returns the last error, which the host counts as
// a delivery failure and does not retry again.
//
// The event id travels unchanged through every attempt, so a receiver that saw
// attempt 1 and answered slowly can recognize attempt 2 as the same event. That is
// the whole of what "idempotent on the event id" asks of a sink author.
func (s *Sink) Deliver(ctx context.Context, ev pluginapi.SinkEvent) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("webhook: encoding event %s: %w", ev.ID, err)
	}
	signature := SignaturePrefix + Sign(s.Secret, body)

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("webhook: delivering event %s: %w", ev.ID, ctx.Err())
			case <-time.After(retryBackoff):
			}
		}
		if err := s.post(ctx, body, signature, ev); err != nil {
			lastErr = err
			// A cancelled or expired deadline is the host saying stop, not a blip:
			// retrying it would only burn the budget the next event needs.
			if ctx.Err() != nil {
				break
			}
			continue
		}
		return nil
	}
	return fmt.Errorf("webhook: delivering event %s: %w", ev.ID, lastErr)
}

// post is one attempt: build the request, send it, and read just enough of the
// answer to say what went wrong.
func (s *Sink) post(ctx context.Context, body []byte, signature string, ev pluginapi.SinkEvent) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "obelo/1.0 (self-hosted)")
	req.Header.Set(HeaderEvent, ev.Type)
	req.Header.Set(HeaderEventID, ev.ID)
	req.Header.Set(HeaderSignature, signature)

	client := s.HTTPClient
	if client == nil {
		client = safefetch.Client(0)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook: target answered %d: %s", resp.StatusCode, bytes.TrimSpace(detail))
	}
	return nil
}

// Sign is the signature a receiver recomputes: lowercase hex HMAC-SHA256 over the
// exact bytes of the request body, under the configured secret. Exported because
// it is the one thing a receiving script has to reimplement, and a test that
// verifies a delivered document is standing in for that script.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
