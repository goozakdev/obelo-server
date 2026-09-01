package enrich

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The 503 retry loop, which nothing guarded before. MusicBrainz answers 503 when
// it wants us to slow down, so the delay after one has to be real. The bug these
// cover: the delay used to be attempt x MinInterval, and MinInterval=0 is the
// DOCUMENTED setting for a mirror with no rate policy — so exactly the operator
// who disabled client-side pacing got four back-to-back requests at a host that
// had just asked for a pause.

// TestMusicBrainzRetryBackoffNeverZero: the base delay is independent of
// MinInterval and never zero, however the provider was built. This is the direct
// regression guard — it reads the computed delay rather than sleeping it.
func TestMusicBrainzRetryBackoffNeverZero(t *testing.T) {
	cases := []struct {
		name string
		p    *MusicBrainzProvider
		want time.Duration
	}{
		{"throttling disabled (mirror)", &MusicBrainzProvider{MinInterval: 0}, defaultMusicBrainzRetryBackoff},
		{"bare struct literal", &MusicBrainzProvider{}, defaultMusicBrainzRetryBackoff},
		{"negative is nonsense, use the default", &MusicBrainzProvider{RetryBackoff: -time.Second}, defaultMusicBrainzRetryBackoff},
		{"explicit value wins", &MusicBrainzProvider{RetryBackoff: 250 * time.Millisecond}, 250 * time.Millisecond},
		{"large MinInterval does not shrink it", &MusicBrainzProvider{MinInterval: time.Hour}, defaultMusicBrainzRetryBackoff},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.p.retryBackoff(); got != c.want {
				t.Errorf("retryBackoff() = %v, want %v", got, c.want)
			}
			if c.p.retryBackoff() <= 0 {
				t.Error("a zero backoff retries instantly at a host that just said 503")
			}
		})
	}
}

// TestMusicBrainz503BacksOffWithThrottlingDisabled: end to end with MinInterval=0,
// the retries are actually spaced — and the delay grows per attempt.
func TestMusicBrainz503BacksOffWithThrottlingDisabled(t *testing.T) {
	const base = 40 * time.Millisecond
	var at []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		at = append(at, time.Now())
		if len(at) <= 2 { // two throttle answers, then success
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"mbid-1","title":"Doolittle"}`))
	}))
	defer srv.Close()

	p := NewMusicBrainzProvider(srv.URL, srv.URL, "en")
	p.MinInterval = 0 // the mirror setting that used to collapse the backoff to zero
	p.RetryBackoff = base

	md, err := p.Lookup(context.Background(), TitleRef{Kind: "album", MusicbrainzID: "mbid-1"})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !md.Matched {
		t.Error("the retry should have recovered the lookup")
	}
	if len(at) != 3 {
		t.Fatalf("made %d requests, want 3 (two 503s then a success)", len(at))
	}
	// Attempt N waits N x base, so the second gap must exceed the first.
	first, second := at[1].Sub(at[0]), at[2].Sub(at[1])
	if first < base {
		t.Errorf("first retry came after %v, want at least %v", first, base)
	}
	if second < 2*base {
		t.Errorf("second retry came after %v, want at least %v (the delay must grow)", second, 2*base)
	}
}

// TestMusicBrainz503GivesUpAfterMaxAttempts: a host that stays down surfaces the
// 503 as an error rather than retrying forever.
func TestMusicBrainz503GivesUpAfterMaxAttempts(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	p := NewMusicBrainzProvider(srv.URL, srv.URL, "en")
	p.MinInterval = 0
	p.RetryBackoff = time.Millisecond

	_, err := p.Lookup(context.Background(), TitleRef{Kind: "album", MusicbrainzID: "mbid-1"})
	if err == nil {
		t.Fatal("a persistent 503 must surface as an error")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error %q should name the status", err)
	}
	if calls != 4 {
		t.Errorf("made %d requests, want 4 (maxAttempts)", calls)
	}
}

// TestRetryAfterHeaderWins: the host's own Retry-After beats our computed delay;
// anything unusable falls back to it.
func TestRetryAfterHeaderWins(t *testing.T) {
	const fallback = 7 * time.Second
	cases := []struct {
		name   string
		header string
		want   time.Duration
	}{
		{"whole seconds are honoured", "2", 2 * time.Second},
		{"absent falls back", "", fallback},
		{"zero is not a licence to retry now", "0", fallback},
		{"negative falls back", "-5", fallback},
		{"an HTTP-date we cannot parse falls back", "Wed, 21 Oct 2026 07:28:00 GMT", fallback},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := http.Header{}
			if c.header != "" {
				h.Set("Retry-After", c.header)
			}
			if got := retryAfter(h, fallback); got != c.want {
				t.Errorf("retryAfter(%q) = %v, want %v", c.header, got, c.want)
			}
		})
	}
}
