package onlinesource

import (
	"testing"
	"time"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

func TestURLSafeID(t *testing.T) {
	for id, want := range map[string]bool{
		"abc": true, "A-b_c.d~e": true, "123": true,
		"": false, ".": false, "..": false, "a/b": false, "a b": false, "a?b": false, "a%2fb": false,
	} {
		if got := urlSafeID(id); got != want {
			t.Errorf("urlSafeID(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestIsHTTPS(t *testing.T) {
	for u, want := range map[string]bool{
		"https://a.example/x": true, "http://a.example/x": false, "ftp://a.example/x": false,
		"https:///x": false, "//a.example/x": false, "": false,
	} {
		if got := isHTTPS(u); got != want {
			t.Errorf("isHTTPS(%q) = %v, want %v", u, got, want)
		}
	}
}

// TestSessionsEndByTheReaperAndTellWhoever: an idle session is reaped, a touched
// one is not, and the end callback hears of each.
func TestSessionsEndByTheReaperAndTellWhoever(t *testing.T) {
	s := New(pluginapi.NewRegistry(), nil)
	now := time.Now()
	s.now = func() time.Time { return now }
	var ended []string
	s.SetOnEnd(func(id string) { ended = append(ended, id) })
	s.sessions["idle"] = &Session{ID: "idle", LastSeen: now.Add(-2 * time.Minute)}
	s.sessions["busy"] = &Session{ID: "busy", LastSeen: now.Add(-2 * time.Minute)}
	s.Touch("busy")

	if n := s.Reap(time.Minute); n != 1 {
		t.Fatalf("Reap ended %d sessions, want 1", n)
	}
	if _, ok := s.Session("idle"); ok {
		t.Fatal("the idle session survived")
	}
	if _, ok := s.Session("busy"); !ok {
		t.Fatal("the touched session was reaped")
	}
	if len(ended) != 1 || ended[0] != "idle" {
		t.Fatalf("end callback heard %v, want [idle]", ended)
	}
	if s.End("idle") {
		t.Fatal("ending an ended session reported success")
	}
	if n := s.EndAll(); n != 1 || s.Count() != 0 {
		t.Fatalf("EndAll ended %d, %d left; want 1 and 0", n, s.Count())
	}
}
