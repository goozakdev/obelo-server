package api

import (
	"bytes"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
)

// TestLogRequestsRedactsNonCanonicalTargets: the stream token must stay out of
// the access log for request-target forms the mux routes to /api/v1/stream/ but
// whose raw RequestURI does not start with that literal prefix.
func TestLogRequestsRedactsNonCanonicalTargets(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	h := LogRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	addr := ln.Addr().String()

	for _, target := range []string{
		"http://" + addr + "/api/v1/stream/SECRETTOKEN/stream", // absolute-form
		"/api/v1/%73tream/SECRETTOKEN/stream",                  // percent-encoded
	} {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = c.Write([]byte("GET " + target + " HTTP/1.1\r\nHost: " + addr + "\r\nConnection: close\r\n\r\n"))
		var resp bytes.Buffer
		_, _ = resp.ReadFrom(c)
		c.Close()
		if !strings.Contains(resp.String(), "200") {
			t.Fatalf("%q: unexpected response %q", target, resp.String())
		}
	}
	if strings.Contains(buf.String(), "SECRETTOKEN") {
		t.Errorf("token leaked into the access log: %q", buf.String())
	}
	if got := strings.Count(buf.String(), "/api/v1/stream/"); got != 2 {
		t.Errorf("want both log lines to keep the redacted path, got %d in %q", got, buf.String())
	}
}
