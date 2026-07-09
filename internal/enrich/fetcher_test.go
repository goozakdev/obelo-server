package enrich

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The production HTTPArtworkFetcher's guards, exercised against an httptest
// server — no live network. A 404 must surface as ErrArtworkNotFound (the benign
// "no image here" case the enrich service skips quietly); an oversized body and a
// non-image content-type must surface as real errors.

func TestArtworkFetcherSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("\xff\xd8\xff\xe0 jpeg bytes"))
	}))
	defer srv.Close()

	data, ct, err := HTTPArtworkFetcher{}.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if ct != "image/jpeg" || len(data) == 0 {
		t.Errorf("got ct=%q len=%d", ct, len(data))
	}
}

func TestArtworkFetcherNotFoundIsSentinel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no cover", http.StatusNotFound)
	}))
	defer srv.Close()

	_, _, err := HTTPArtworkFetcher{}.Fetch(context.Background(), srv.URL)
	if !errors.Is(err, ErrArtworkNotFound) {
		t.Fatalf("want ErrArtworkNotFound, got %v", err)
	}
}

func TestArtworkFetcherOtherStatusIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, _, err := HTTPArtworkFetcher{}.Fetch(context.Background(), srv.URL)
	if err == nil || errors.Is(err, ErrArtworkNotFound) {
		t.Fatalf("want a non-sentinel error, got %v", err)
	}
}

func TestArtworkFetcherOversizeIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte(strings.Repeat("x", 64)))
	}))
	defer srv.Close()

	_, _, err := HTTPArtworkFetcher{MaxBytes: 16}.Fetch(context.Background(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("want oversize error, got %v", err)
	}
}

func TestArtworkFetcherNonImageIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>error page</html>"))
	}))
	defer srv.Close()

	_, _, err := HTTPArtworkFetcher{}.Fetch(context.Background(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "non-image") {
		t.Fatalf("want non-image error, got %v", err)
	}
}
