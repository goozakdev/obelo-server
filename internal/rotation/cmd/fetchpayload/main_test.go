package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/rotation"
)

func newKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func stub(t *testing.T, status int, v int, payload string, wantUA string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("User-Agent"), wantUA) {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(rotation.Envelope{V: v, MinAppVersion: "0.0.0", Payload: payload})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchPayload(t *testing.T) {
	key := newKey(t)
	payload, err := rotation.Encrypt(key, rotation.Keys{TMDB: "tmdb-secret", Fanart: "fanart-secret"})
	if err != nil {
		t.Fatal(err)
	}
	otherPayload, _ := rotation.Encrypt(newKey(t), rotation.Keys{TMDB: "x"})

	cases := []struct {
		name    string
		status  int
		v       int
		payload string
		encKey  string
		wantErr string
	}{
		{"success", 200, rotation.SupportedVersion, payload, key, ""},
		{"non-200", 500, rotation.SupportedVersion, payload, key, "HTTP 500"},
		{"bad version", 200, rotation.SupportedVersion + 1, payload, key, "version"},
		{"wrong key", 200, rotation.SupportedVersion, otherPayload, key, "decrypt"},
		{"missing enc key", 200, rotation.SupportedVersion, payload, "", "OBELO_APP_ENC_KEY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := stub(t, tc.status, tc.v, tc.payload, "obelo/")
			got, err := fetchPayload(context.Background(), srv.URL, tc.encKey)
			if tc.wantErr == "" {
				if err != nil || got != tc.payload {
					t.Fatalf("fetchPayload = (%q, %v), want payload verbatim", got, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
			if got != "" {
				t.Errorf("payload %q returned alongside error", got)
			}
			if strings.Contains(err.Error(), "tmdb-secret") {
				t.Error("error leaks decrypted key")
			}
		})
	}
}

func TestFetchPayloadSendsObeloUserAgent(t *testing.T) {
	key := newKey(t)
	payload, _ := rotation.Encrypt(key, rotation.Keys{TMDB: "a"})
	srv := stub(t, 200, rotation.SupportedVersion, payload, "obelo/")
	if _, err := fetchPayload(context.Background(), srv.URL, key); err != nil {
		t.Fatalf("Worker-style UA gate rejected the helper: %v", err)
	}
}
