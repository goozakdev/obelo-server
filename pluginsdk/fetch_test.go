package pluginsdk_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk"
	"github.com/goozakdev/obelo-server/pluginsdk/sdktest"
)

// The fetch helpers, and the one distinction they exist to keep: a refusal, a
// transport failure and a status are three different facts, and a provider that
// folded them into "something went wrong" would make every outage look like a
// settled answer.

// TestAFetchErrorTellsARefusalFromAnOutageFromAnAnswer is the classification
// issues 04–07 will branch on.
func TestAFetchErrorTellsARefusalFromAnOutageFromAnAnswer(t *testing.T) {
	for _, tc := range []struct {
		name      string
		resp      pluginapi.FetchResponse
		refusal   bool
		transient bool
		notFound  bool
	}{
		{"the host refuses", pluginapi.FetchResponse{Refused: "host not in allowlist"}, true, false, false},
		{"the transport fails", pluginapi.FetchResponse{Error: "connection reset"}, false, true, false},
		{"the source is overloaded", pluginapi.FetchResponse{Status: 503}, false, true, false},
		{"the source is throttling", pluginapi.FetchResponse{Status: 429}, false, true, false},
		{"the source has no such record", pluginapi.FetchResponse{Status: 404}, false, false, true},
		{"the key is wrong", pluginapi.FetchResponse{Status: 401}, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := tc.resp
			host := sdktest.New(sdktest.WithFetch(
				func(context.Context, pluginapi.FetchRequest) (pluginapi.FetchResponse, error) {
					return resp, nil
				}))
			_, err := pluginsdk.Do(context.Background(), host, pluginapi.FetchRequest{URL: "https://example.test/x"})
			var fe *pluginsdk.FetchError
			if !errors.As(err, &fe) {
				t.Fatalf("err = %v, want a *FetchError", err)
			}
			if fe.IsRefusal() != tc.refusal || fe.IsTransient() != tc.transient || fe.IsNotFound() != tc.notFound {
				t.Errorf("refusal/transient/notFound = %v/%v/%v, want %v/%v/%v",
					fe.IsRefusal(), fe.IsTransient(), fe.IsNotFound(), tc.refusal, tc.transient, tc.notFound)
			}
			if fe.Error() == "" {
				t.Error("a FetchError with no sentence in it is one an operator cannot read")
			}
		})
	}
}

// TestGetJSONDecodesA2xxAndRefusesAnythingElse.
func TestGetJSONDecodesA2xxAndRefusesAnythingElse(t *testing.T) {
	host := sdktest.New(sdktest.WithHandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/movie/1" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("api_key") != "k" || r.Header.Get("Accept") != "application/json" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"title":"Dune"}`))
	}))

	var out struct {
		Title string `json:"title"`
	}
	q := url.Values{"api_key": {"k"}}
	err := pluginsdk.GetJSON(context.Background(), host, "https://example.test/movie/1", q, &out,
		pluginsdk.Header("Accept", "application/json"))
	if err != nil {
		t.Fatalf("GetJSON: %v", err)
	}
	if out.Title != "Dune" {
		t.Errorf("title = %q", out.Title)
	}

	if err := pluginsdk.GetJSON(context.Background(), host, "https://example.test/movie/2", q, &out); err == nil {
		t.Fatal("a 404 decoded as success")
	}
}

// TestURLWithQueryKeepsWhatTheOperatorTyped: a base URL that already carries a
// query is the OPERATOR's — a mirror, a proxy, or in this suite a mode marker —
// and dropping it is how a plugin stops honouring an override.
func TestURLWithQueryKeepsWhatTheOperatorTyped(t *testing.T) {
	got := pluginsdk.URLWithQuery("https://example.test/v1?mode=x", url.Values{"a": {"1"}})
	if got != "https://example.test/v1?mode=x&a=1" {
		t.Errorf("URLWithQuery = %q", got)
	}
	if got := pluginsdk.URLWithQuery("https://example.test/v1", nil); got != "https://example.test/v1" {
		t.Errorf("URLWithQuery with no query = %q", got)
	}
}
