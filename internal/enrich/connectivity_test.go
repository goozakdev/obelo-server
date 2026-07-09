package enrich

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestConnection recognizes and probes every registered provider — including the
// video supplements OMDb and TheTVDB (metadata-providers 03/04), which were added
// to the registry after the settings surface. A probe against a healthy stub
// answers ok; an unknown slug is rejected without any call.

func TestTestConnectionOMDbProbes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"Title":"Inception","Response":"True"}`))
	}))
	defer srv.Close()

	ok, detail := TestConnection(context.Background(), SlugOMDb, "key", srv.URL, "en-US")
	if !ok {
		t.Fatalf("omdb probe = ok:false (%q), want ok:true against a healthy stub", detail)
	}
}

func TestTestConnectionTheTVDBProbes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/login":
			_, _ = w.Write([]byte(`{"status":"success","data":{"token":"tok"}}`))
		case r.URL.Path == "/search":
			_, _ = w.Write([]byte(`{"data":[{"tvdb_id":"81189","name":"Breaking Bad"}]}`))
		case strings.HasPrefix(r.URL.Path, "/series/"):
			_, _ = w.Write([]byte(`{"data":{"name":"Breaking Bad","overview":"A chemistry teacher.","image":"https://art/bb.jpg","genres":[{"name":"Drama"}]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	ok, detail := TestConnection(context.Background(), SlugTheTVDB, "key", srv.URL, "en-US")
	if !ok {
		t.Fatalf("thetvdb probe = ok:false (%q), want ok:true against a healthy stub", detail)
	}
}

func TestTestConnectionUnknownSlug(t *testing.T) {
	ok, detail := TestConnection(context.Background(), "nope", "key", "http://unused", "en-US")
	if ok || detail == "" {
		t.Errorf("unknown slug = ok:%v detail:%q, want ok:false with a detail", ok, detail)
	}
}
