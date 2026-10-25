package link

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/playback"
	"github.com/goozakdev/obelo-server/internal/store"
)

// A sharer that predates step-down rejects unknown JSON fields with a 400, so
// replacesSessionId goes to it only when its handshake advertises features.stepDown
// (ADR-0071 §5). The local replacement happens either way.

type mirrorFake struct {
	RelayStore
}

func (mirrorFake) LibraryByID(string) (store.Library, error) {
	return store.Library{Source: store.LibrarySourceLinked, LinkID: "l1"}, nil
}

func (mirrorFake) RemoteIDOf(_, id string) (string, error) { return "r-" + id, nil }

func relayReplaces(t *testing.T, features map[string]bool) (sent map[string]any, present bool) {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case apiPrefix + "/server":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "sharer", "name": "Sharer", "features": features, "linkProtocolVersion": 1})
		default:
			raw, _ := io.ReadAll(r.Body)
			mu.Lock()
			sent = map[string]any{}
			_ = json.Unmarshal(raw, &sent)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"sessionId":"rs2","tier":"directPlay","edition":{"id":"re1"}}`))
		}
	}))
	t.Cleanup(srv.Close)
	st := &memStore{links: []store.Link{{
		ID: "l1", ServerID: "sharer", ServerName: "Sharer", Origins: []string{srv.URL},
		ActiveOrigin: srv.URL, Token: "tok", State: store.LinkStateConnected,
	}}}
	svc := newService(t, st, Options{})
	svc.relayCatalog = mirrorFake{}
	if _, err := svc.RelayNegotiate(context.Background(), playback.RelayRequest{LibraryID: "lib", TitleID: "t1", ReplacesSessionID: "rs1"}); err != nil {
		t.Fatalf("RelayNegotiate: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	_, present = sent["replacesSessionId"]
	return sent, present
}

func TestRelayOmitsReplacesSessionIdForASharerWithoutStepDown(t *testing.T) {
	if sent, present := relayReplaces(t, map[string]bool{"serverLinking": true}); present {
		t.Errorf("sharer without features.stepDown was sent replacesSessionId = %v", sent["replacesSessionId"])
	}
}

func TestRelayForwardsReplacesSessionIdToASharerWithStepDown(t *testing.T) {
	sent, present := relayReplaces(t, map[string]bool{"serverLinking": true, "stepDown": true})
	if !present || sent["replacesSessionId"] != "rs1" {
		t.Errorf("replacesSessionId = %v (present %v), want rs1", sent["replacesSessionId"], present)
	}
}

// probeSharer serves a sharer whose /server answers with status (200 carries
// features), counting the handshake reads, and records what the negotiations sent.
type probeSharer struct {
	mu       sync.Mutex
	probes   int
	sent     []map[string]any
	features map[string]bool
	status   int
}

func (p *probeSharer) handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.URL.Path == apiPrefix+"/server" {
		p.probes++
		if p.status != 0 && p.status != http.StatusOK {
			w.WriteHeader(p.status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "sharer", "name": "Sharer", "features": p.features, "linkProtocolVersion": 1})
		return
	}
	raw, _ := io.ReadAll(r.Body)
	m := map[string]any{}
	_ = json.Unmarshal(raw, &m)
	p.sent = append(p.sent, m)
	_, _ = w.Write([]byte(`{"sessionId":"rs2","tier":"directPlay","edition":{"id":"re1"}}`))
}

func (p *probeSharer) counts() (probes, negotiations int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.probes, len(p.sent)
}

func stepOnce(t *testing.T, svc *Service) {
	t.Helper()
	if _, err := svc.RelayNegotiate(context.Background(), playback.RelayRequest{LibraryID: "lib", TitleID: "t1", ReplacesSessionID: "rs1"}); err != nil {
		t.Fatalf("RelayNegotiate: %v", err)
	}
}

func TestRelayProbeErrorOmitsReplacesSessionIdAndIsNotRepeatedWithinFiveMinutes(t *testing.T) {
	sharer := &probeSharer{status: http.StatusInternalServerError}
	srv := httptest.NewServer(http.HandlerFunc(sharer.handler))
	t.Cleanup(srv.Close)
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	st := &memStore{links: []store.Link{{
		ID: "l1", ServerID: "sharer", ServerName: "Sharer", Origins: []string{srv.URL},
		ActiveOrigin: srv.URL, Token: "tok", State: store.LinkStateConnected,
	}}}
	svc := newService(t, st, Options{Now: func() time.Time { return now }})
	svc.relayCatalog = mirrorFake{}

	stepOnce(t, svc)
	now = now.Add(4 * time.Minute)
	stepOnce(t, svc)
	probes, negotiations := sharer.counts()
	if probes != 1 || negotiations != 2 {
		t.Fatalf("probes = %d, negotiations = %d; want 1 probe (the failure is cached) and 2 negotiations", probes, negotiations)
	}
	for i, m := range sharer.sent {
		if _, present := m["replacesSessionId"]; present {
			t.Errorf("negotiation %d carried replacesSessionId after a failed probe", i)
		}
	}

	now = now.Add(2 * time.Minute) // 6 minutes after the first probe
	stepOnce(t, svc)
	if probes, _ := sharer.counts(); probes != 2 {
		t.Errorf("probes = %d after the cache expired, want 2", probes)
	}
}

func TestRelayProbeAnswerIsCachedPerLink(t *testing.T) {
	sharer := &probeSharer{features: map[string]bool{"serverLinking": true, "stepDown": true}}
	srv := httptest.NewServer(http.HandlerFunc(sharer.handler))
	t.Cleanup(srv.Close)
	st := &memStore{links: []store.Link{{
		ID: "l1", ServerID: "sharer", ServerName: "Sharer", Origins: []string{srv.URL},
		ActiveOrigin: srv.URL, Token: "tok", State: store.LinkStateConnected,
	}}}
	svc := newService(t, st, Options{})
	svc.relayCatalog = mirrorFake{}

	stepOnce(t, svc)
	stepOnce(t, svc)
	probes, _ := sharer.counts()
	if probes != 1 {
		t.Errorf("probes = %d for two steps, want 1", probes)
	}
	for i, m := range sharer.sent {
		if m["replacesSessionId"] != "rs1" {
			t.Errorf("negotiation %d: replacesSessionId = %v, want rs1", i, m["replacesSessionId"])
		}
	}
}

func TestRelayProbeDoesNotTryADeadOriginTwice(t *testing.T) {
	var dead int32
	deadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&dead, 1)
		// Drop the connection: a transport failure, not an answer.
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close()
		}
	}))
	t.Cleanup(deadSrv.Close)
	sharer := &probeSharer{features: map[string]bool{"serverLinking": true, "stepDown": true}}
	live := httptest.NewServer(http.HandlerFunc(sharer.handler))
	t.Cleanup(live.Close)
	st := &memStore{links: []store.Link{{
		ID: "l1", ServerID: "sharer", ServerName: "Sharer", Origins: []string{deadSrv.URL, live.URL},
		ActiveOrigin: deadSrv.URL, Token: "tok", State: store.LinkStateConnected,
	}}}
	svc := newService(t, st, Options{})
	svc.relayCatalog = mirrorFake{}

	stepOnce(t, svc)
	if n := atomic.LoadInt32(&dead); n != 1 {
		t.Errorf("dead origin was hit %d times, want 1 (the probe finds the live one and the negotiation goes there)", n)
	}
	if st.links[0].ActiveOrigin != live.URL {
		t.Errorf("active origin = %q, want the live one %q", st.links[0].ActiveOrigin, live.URL)
	}
}

func TestRelayProbeOfAnotherServerIsNotTheLinksSharer(t *testing.T) {
	sharer := &probeSharer{features: map[string]bool{"serverLinking": true, "stepDown": true}}
	srv := httptest.NewServer(http.HandlerFunc(sharer.handler)) // answers as server "sharer"
	t.Cleanup(srv.Close)
	st := &memStore{links: []store.Link{{
		ID: "l1", ServerID: "someone-else", ServerName: "Other", Origins: []string{srv.URL},
		Token: "tok", State: store.LinkStateConnected,
	}}}
	svc := newService(t, st, Options{})
	l := st.links[0]

	if svc.sharerHasStepDown(context.Background(), &l) {
		t.Error("a handshake from another server counted as the Link's sharer having stepDown")
	}
	if l.ActiveOrigin != "" || st.links[0].ActiveOrigin != "" {
		t.Errorf("active origin = %q / %q, want it left alone after answering as another server", l.ActiveOrigin, st.links[0].ActiveOrigin)
	}
}
