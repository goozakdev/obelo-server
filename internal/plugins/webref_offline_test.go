package plugins

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// TestAnOfflineCallRefusesEveryFetchBeforeItIsSent is the no-network rule of the
// Web reference provider seam, as the fetch policy it is.
//
// The target is one the fetch policy would otherwise ALLOW — the operator's own
// URL, which skips both the allowlist and the address check — so the only thing
// that can stop the second request is the offline flag. The control proves the
// same request reaches the source without it.
func TestAnOfflineCallRefusesEveryFetchBeforeItIsSent(t *testing.T) {
	var hits atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("ok"))
	}))
	defer source.Close()
	target, err := url.Parse(source.URL)
	if err != nil {
		t.Fatal(err)
	}

	p := &Plugin{id: "refs", hosts: map[string]struct{}{}, opts: Options{
		Logf: func(string, ...any) {},
	}.withDefaults()}
	p.resolveLimits()
	h := &hostFuncs{p: p}
	req := pluginapi.FetchRequest{URL: source.URL + "/probe"}

	if err := p.beginCall(normalizeHost(target.Hostname()), false); err != nil {
		t.Fatal(err)
	}
	if resp := h.fetch(context.Background(), req); resp.Status != http.StatusOK || hits.Load() != 1 {
		t.Fatalf("control: an online call to the operator's target answered %+v with %d requests, want 200 and 1",
			resp, hits.Load())
	}
	p.endCall()

	if err := p.beginCall(normalizeHost(target.Hostname()), true); err != nil {
		t.Fatal(err)
	}
	resp := h.fetch(context.Background(), req)
	p.endCall()
	if resp.Refused != refusedOffline {
		t.Fatalf("an offline call's fetch answered %+v, want refused %q", resp, refusedOffline)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("the source saw %d requests, want 1: the offline fetch must never be sent", n)
	}
	if st := p.Status(); st.LastError == "" {
		t.Fatal("the refused fetch left no last error; a pure computation reaching for the network is a violation")
	}
}
