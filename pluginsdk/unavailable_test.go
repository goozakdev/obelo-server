package pluginsdk_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk"
	"github.com/goozakdev/obelo-server/pluginsdk/sdktest"
)

// The one classification every Metadata provider needs and none should own a copy
// of: "could this source not answer right now?" versus "did it answer, and is the
// answer no use?".
//
// The stakes are asymmetric and that is why this is tested at the SDK rather than
// in seven plugins. Getting it wrong in one direction parks a matchable Title as
// 'failed' on a 503 and — because a Go error from a guest is a STRIKE — disables
// the whole plugin after three of them. Getting it wrong in the other direction
// retries a rejected API key quietly forever and never tells the operator.

// TestRetryableStatusMatchesTheServersOwnRule holds the SDK to the line
// internal/enrich draws for the compiled-in sources (ADR-0048). The two lists are
// in different modules and cannot import each other, so this is what stops them
// drifting.
func TestRetryableStatusMatchesTheServersOwnRule(t *testing.T) {
	retryable := []int{
		http.StatusRequestTimeout,      // 408 — the source gave up waiting
		http.StatusTooManyRequests,     // 429 — throttled; the item is fine
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503 — the classic metadata-source blip
		http.StatusGatewayTimeout,      // 504
		599,
	}
	for _, code := range retryable {
		if !pluginsdk.RetryableStatus(code) {
			t.Errorf("status %d is not retryable — a Title lost to it would park on the "+
				"attention list until somebody ran a full pass", code)
		}
	}
	permanent := []int{
		http.StatusOK,
		http.StatusBadRequest,          // 400 — our query is wrong
		http.StatusUnauthorized,        // 401 — the key is rejected; only the Admin can fix it
		http.StatusForbidden,           // 403
		http.StatusNotFound,            // 404
		http.StatusUnprocessableEntity, // 422
		600,
	}
	for _, code := range permanent {
		if pluginsdk.RetryableStatus(code) {
			t.Errorf("status %d is retryable — it would be asked again forever instead of "+
				"telling the Admin their credentials or query are wrong", code)
		}
	}
}

func TestUnavailableClassifiesAFetchFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
		why  string
	}{
		{
			name: "a host refusal",
			err:  &pluginsdk.FetchError{URL: "https://x.test", Refused: sdktest.RefusedAllowlist},
			want: true,
			why:  "the plugin learned nothing about the item",
		},
		{
			name: "a transport failure",
			err:  &pluginsdk.FetchError{URL: "https://x.test", Transport: "dial tcp: connection refused"},
			want: true,
			why:  "the request never reached the source",
		},
		{
			name: "a spent call budget",
			err:  &pluginsdk.FetchError{URL: "https://x.test", Transport: "the fetch did not finish before this call's deadline"},
			want: true,
			why:  "ADR-0059 decision 6: the host hands this back so the guest can say unavailable",
		},
		{
			name: "a 503",
			err:  &pluginsdk.FetchError{URL: "https://x.test", Status: 503},
			want: true,
			why:  "the source is briefly unwell; the item is fine",
		},
		{
			name: "a 429",
			err:  &pluginsdk.FetchError{URL: "https://x.test", Status: 429},
			want: true,
			why:  "we are being throttled, which says nothing about the item",
		},
		{
			name: "a 408",
			err:  &pluginsdk.FetchError{URL: "https://x.test", Status: 408},
			want: true,
			why:  "the source gave up waiting",
		},
		{
			name: "a 401",
			err:  &pluginsdk.FetchError{URL: "https://x.test", Status: 401},
			want: false,
			why:  "a rejected key must reach the operator, not be retried in silence",
		},
		{
			name: "a 404",
			err:  &pluginsdk.FetchError{URL: "https://x.test", Status: 404},
			want: false,
			why:  "the source answered about this record",
		},
		{
			name: "an unreadable document",
			err:  &pluginsdk.FetchError{URL: "https://x.test", Status: 200, Decode: errors.New("unexpected EOF")},
			want: false,
			why:  "the source answered; this plugin cannot read it, which is the plugin's bug",
		},
		{
			name: "an error that is not a fetch failure at all",
			err:  errors.New("something else entirely"),
			want: false,
			why:  "nothing can be concluded from an error this helper did not produce",
		},
		{
			name: "no error",
			err:  nil,
			want: false,
		},
	} {
		detail, ok := pluginsdk.Unavailable(tc.err)
		if ok != tc.want {
			t.Errorf("%s: Unavailable = %v, want %v — %s", tc.name, ok, tc.want, tc.why)
			continue
		}
		if ok && detail == "" {
			t.Errorf("%s: answered true with an empty detail; the Detail field is what an "+
				"operator reads to learn why the item was not enriched", tc.name)
		}
		if !ok && detail != "" {
			t.Errorf("%s: answered false but handed back %q", tc.name, detail)
		}
	}
}

// And the helpers produce errors this classifies: a plugin calls GetJSON and asks
// about what came back, which is the only path that matters in practice.
func TestUnavailableClassifiesWhatTheHelpersReturn(t *testing.T) {
	var code int
	host := sdktest.New(sdktest.WithHandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{}`))
	}))

	code = http.StatusServiceUnavailable
	err := pluginsdk.GetJSON(context.Background(), host, "https://x.test/thing", nil, &struct{}{})
	if _, ok := pluginsdk.Unavailable(err); !ok {
		t.Errorf("a 503 from GetJSON is not unavailable: %v", err)
	}

	code = http.StatusUnauthorized
	err = pluginsdk.GetJSON(context.Background(), host, "https://x.test/thing", nil, &struct{}{})
	if _, ok := pluginsdk.Unavailable(err); ok {
		t.Errorf("a 401 from GetJSON is unavailable, so a rejected key would be retried forever")
	}

	// A refusal the host authored, through the same path.
	refusing := sdktest.New(sdktest.WithAllowedHosts("allowed.test"))
	err = pluginsdk.GetJSON(context.Background(), refusing, "https://x.test/thing", nil, &struct{}{})
	if _, ok := pluginsdk.Unavailable(err); !ok {
		t.Errorf("a refusal from GetJSON is not unavailable: %v", err)
	}

	// And the fetch-level shape the host uses for a spent budget.
	timedOut := sdktest.New(sdktest.WithFetch(func(context.Context, pluginapi.FetchRequest) (pluginapi.FetchResponse, error) {
		return pluginapi.FetchResponse{Error: "the fetch did not finish before this call's deadline"}, nil
	}))
	err = pluginsdk.GetJSON(context.Background(), timedOut, "https://x.test/thing", nil, &struct{}{})
	if _, ok := pluginsdk.Unavailable(err); !ok {
		t.Errorf("a spent call budget is not unavailable: %v", err)
	}
}
