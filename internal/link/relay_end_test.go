package link

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// TestRelayEndSessionWalksTheListedOrigins: ending a relayed session is as
// patient as playing one. An active origin that no longer answers, or none at
// all, must not strand the session on the sharer while another listed address
// still reaches it.
func TestRelayEndSessionWalksTheListedOrigins(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + ln.Addr().String()
	_ = ln.Close()

	var mu sync.Mutex
	var deleted []string
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		deleted = append(deleted, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(live.Close)

	for _, c := range []struct{ name, active string }{
		{"active origin is dead", dead},
		{"no active origin", ""},
	} {
		mu.Lock()
		deleted = nil
		mu.Unlock()
		st := &memStore{links: []store.Link{{
			ID: "l1", ServerID: "sharer", ServerName: "Sharer", Origins: []string{dead, live.URL},
			ActiveOrigin: c.active, Token: "tok", State: store.LinkStateConnected,
		}}}
		svc := newService(t, st, Options{})
		if err := svc.RelayEndSession(context.Background(), "l1", "rs1"); err != nil {
			t.Fatalf("%s: RelayEndSession: %v", c.name, err)
		}
		mu.Lock()
		got := append([]string(nil), deleted...)
		mu.Unlock()
		if len(got) != 1 || !strings.HasPrefix(got[0], "DELETE ") || !strings.HasSuffix(got[0], "/sessions/rs1") {
			t.Errorf("%s: the live origin saw %v, want one DELETE of the session", c.name, got)
		}
	}
}
