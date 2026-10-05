package link

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/store"
)

// TestCachedArtworkSkipsAStaleFileOfAnotherExtension: the sharer swapped a JPEG
// poster for a PNG. The fresh .png is cached, but the stale .jpg is found first —
// it must be stepped over, not treated as a miss for the whole name.
func TestCachedArtworkSkipsAStaleFileOfAnotherExtension(t *testing.T) {
	dir := t.TempDir()
	stamp := time.Now()
	stale := filepath.Join(dir, "poster.jpg")
	fresh := filepath.Join(dir, "poster.png")
	for _, p := range []string{stale, fresh} {
		if err := os.WriteFile(p, []byte("img"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(stale, stamp.Add(-time.Hour), stamp.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(fresh, stamp.Add(time.Hour), stamp.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, ok := cachedArtwork(dir, "poster", stamp)
	if !ok || got != fresh {
		t.Errorf("cachedArtwork = (%q, %v), want (%q, true)", got, ok, fresh)
	}
}

// TestWriteCachedArtworkRemovesTheOtherExtensions: a re-fetch that changes the
// image type leaves no stale sibling for the lookup to trip over.
func TestWriteCachedArtworkRemovesTheOtherExtensions(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "poster.jpg")
	if err := os.WriteFile(old, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, err := writeCachedArtwork(dir, "poster", "image/png", []byte("new"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Ext(path) != ".png" {
		t.Fatalf("wrote %q, want a .png", path)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("stale .jpg still present (stat err = %v)", err)
	}
}

// TestReadArtworkRefusesAnOversizedImage: a body past the cap is refused, not cached
// truncated; one exactly at the cap is kept whole.
func TestReadArtworkRefusesAnOversizedImage(t *testing.T) {
	at := bytes.Repeat([]byte{1}, maxRelayArtworkBytes)
	got, err := readArtwork(bytes.NewReader(at))
	if err != nil || len(got) != maxRelayArtworkBytes {
		t.Fatalf("image at the cap: len=%d err=%v, want it whole", len(got), err)
	}
	if _, err := readArtwork(bytes.NewReader(append(at, 1))); !errors.Is(err, ErrArtworkAbsent) {
		t.Errorf("image past the cap err = %v, want ErrArtworkAbsent", err)
	}
}

// TestRelayEndSessionFallsBackToTheListedOrigins: a Link whose active origin was
// cleared still has the origins its invite listed, and the sharer's session must
// still be ended there (relayFetch walks the same fallback).
func TestRelayEndSessionFallsBackToTheListedOrigins(t *testing.T) {
	var deleted string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted = r.URL.Path
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer peer.Close()

	st := &memStore{links: []store.Link{{ID: "l1", Token: "tok", Origins: []string{peer.URL}}}}
	svc := newService(t, st, Options{})
	if err := svc.RelayEndSession(context.Background(), "l1", "rs1"); err != nil {
		t.Fatalf("RelayEndSession: %v", err)
	}
	if deleted != apiPrefix+"/sessions/rs1" {
		t.Errorf("sharer saw DELETE %q, want %q", deleted, apiPrefix+"/sessions/rs1")
	}
}
