package opensubtitles

import (
	"context"
	"net/http"
	"strings"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk/sdktest"
)

// The OpenSubtitles plugin against an in-memory host — no network. The Built-in's
// own tests were about its HTTP client's redirect policy; that policy is the
// host's now, and internal/api's bundled OpenSubtitles suite holds it to the same
// promise through the real sandbox. What is left to prove here is the port: the
// same requests, the same candidates, and where each failure goes.

const (
	base      = "https://api.opensubtitles.test/api/v1"
	sampleSRT = "1\n00:00:01,000 --> 00:00:02,000\nHello world\n"
)

// searchDoc is a /subtitles document with one candidate.
const searchDoc = `{"data":[{"attributes":{"language":"pt-BR","hearing_impaired":true,
"foreign_parts_only":false,"download_count":321,"release":"Movie.2010.1080p",
"files":[{"file_id":42,"file_name":"Movie.2010.1080p.ass"}]}}]}`

// stub is an OpenSubtitles stand-in: /subtitles answers searchDoc for the
// narrowings listed in hits and an empty list otherwise, /download hands back a
// link on the public host, and the link serves body.
type stub struct {
	hits     map[string]bool // narrowing parameter → answers with a candidate
	body     string
	dlStatus int
	dlBody   string
}

func (s *stub) host(t *testing.T) *sdktest.Host {
	t.Helper()
	return sdktest.New(
		sdktest.WithSettings(pluginapi.Settings{Enabled: true, Secret: "the-key", URL: base}),
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/api/v1/subtitles":
				if r.Header.Get("Api-Key") != "the-key" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				for param := range s.hits {
					if r.URL.Query().Get(param) != "" {
						_, _ = w.Write([]byte(searchDoc))
						return
					}
				}
				_, _ = w.Write([]byte(`{"data":[]}`))
			case r.URL.Path == "/api/v1/download" && r.Method == http.MethodPost:
				if s.dlStatus != 0 {
					w.WriteHeader(s.dlStatus)
					_, _ = w.Write([]byte(s.dlBody))
					return
				}
				_, _ = w.Write([]byte(`{"link":"https://www.opensubtitles.test/download/abc/movie.srt","file_name":"movie.srt"}`))
			case r.URL.Path == "/download/abc/movie.srt":
				w.Header().Set("Content-Type", "application/x-subrip")
				_, _ = w.Write([]byte(s.body))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}),
	)
}

func search(t *testing.T, h *sdktest.Host, ref pluginapi.SubtitleRef) pluginapi.SubtitleSearchResponse {
	t.Helper()
	resp, err := New(h).SearchSubtitles(context.Background(), pluginapi.SubtitleSearchRequest{Ref: ref, Language: "pt"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	return resp
}

// TestSearchWalksTheMatchOrder: moviehash, then imdb_id, then the title query,
// stopping at the first narrowing that yields anything, and tagging the
// candidates with the one that did (ADR-0021).
func TestSearchWalksTheMatchOrder(t *testing.T) {
	ref := pluginapi.SubtitleRef{Title: "Movie", Year: 2010, IMDBID: "tt1375666", MovieHash: "8e245d9679d31e12"}
	for _, tc := range []struct {
		hit       string
		matchedBy string
		requests  int
	}{
		{"moviehash", "moviehash", 1},
		{"imdb_id", "imdb", 2},
		{"query", "query", 3},
	} {
		s := &stub{hits: map[string]bool{tc.hit: true}}
		h := s.host(t)
		resp := search(t, h, ref)
		if resp.Outcome != pluginapi.OutcomeMatched || len(resp.Candidates) != 1 {
			t.Fatalf("%s: %+v, want one matched candidate", tc.hit, resp)
		}
		if got := resp.Candidates[0].MatchedBy; got != tc.matchedBy {
			t.Errorf("%s: matchedBy = %q, want %q", tc.hit, got, tc.matchedBy)
		}
		if got := len(h.Requests()); got != tc.requests {
			t.Errorf("%s: %d requests, want %d", tc.hit, got, tc.requests)
		}
	}
}

// TestSearchSendsTheBuiltInsRequest: the query parameters are the Go provider's,
// character for character — tt stripped from the IMDb id, the year beside the
// title, the language on every narrowing — and the key rides in Api-Key.
func TestSearchSendsTheBuiltInsRequest(t *testing.T) {
	s := &stub{hits: map[string]bool{}}
	h := s.host(t)
	resp := search(t, h, pluginapi.SubtitleRef{Title: "Movie", Year: 2010, IMDBID: "tt1375666", MovieHash: "8e245d9679d31e12"})
	if resp.Outcome != pluginapi.OutcomeNoMatch {
		t.Fatalf("outcome = %q, want no-match once every narrowing is empty", resp.Outcome)
	}
	want := []string{
		base + "/subtitles?languages=pt&moviehash=8e245d9679d31e12",
		base + "/subtitles?imdb_id=1375666&languages=pt",
		base + "/subtitles?languages=pt&query=Movie&year=2010",
	}
	reqs := h.Requests()
	if len(reqs) != len(want) {
		t.Fatalf("requests = %d, want %d", len(reqs), len(want))
	}
	for i, r := range reqs {
		if r.URL != want[i] {
			t.Errorf("request %d = %s, want %s", i, r.URL, want[i])
		}
		for _, hd := range r.Headers {
			if strings.EqualFold(hd.Name, "User-Agent") {
				t.Errorf("request %d sets its own User-Agent; that is the host's", i)
			}
		}
	}
}

// TestSearchMapsTheCandidate: every field the Built-in filled, filled the same
// way. The language goes up lowercased and otherwise as the source spelled it:
// the host maps it onto ISO 639-1.
func TestSearchMapsTheCandidate(t *testing.T) {
	s := &stub{hits: map[string]bool{"query": true}}
	resp := search(t, s.host(t), pluginapi.SubtitleRef{Title: "Movie"})
	got := resp.Candidates[0]
	want := pluginapi.SubtitleCandidate{
		ID: "42", Language: "pt-br", Format: "ass", Release: "Movie.2010.1080p",
		HearingImpaired: true, MatchedBy: "query", Downloads: 321,
	}
	if got != want {
		t.Fatalf("candidate = %+v, want %+v", got, want)
	}
}

// TestSearchWithNoLanguageAsksNothing: nothing to find, no call.
func TestSearchWithNoLanguageAsksNothing(t *testing.T) {
	h := (&stub{}).host(t)
	resp, err := New(h).SearchSubtitles(context.Background(), pluginapi.SubtitleSearchRequest{
		Ref: pluginapi.SubtitleRef{Title: "Movie"}, Language: " ",
	})
	if err != nil || resp.Outcome != pluginapi.OutcomeNoMatch || len(h.Requests()) != 0 {
		t.Fatalf("got %+v / %v / %d requests, want no-match and no request", resp, err, len(h.Requests()))
	}
}

// TestDownloadIsTwoSteps: POST /download with the file id, then GET the link.
func TestDownloadIsTwoSteps(t *testing.T) {
	s := &stub{body: sampleSRT}
	h := s.host(t)
	resp, err := New(h).DownloadSubtitle(context.Background(), pluginapi.SubtitleDownloadRequest{
		Candidate: pluginapi.SubtitleCandidate{ID: "42"}, MaxBytes: 8 << 20,
	})
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if resp.Outcome != pluginapi.OutcomeMatched || string(resp.Data) != sampleSRT {
		t.Fatalf("got %+v, want the sample srt", resp)
	}
	if resp.Format != "srt" || resp.ContentType != "application/x-subrip" {
		t.Errorf("format/content type = %q/%q, want the file name's and the link's", resp.Format, resp.ContentType)
	}
	reqs := h.Requests()
	if len(reqs) != 2 || reqs[0].Method != "POST" || string(reqs[0].Body) != `{"file_id":42}` {
		t.Fatalf("requests = %+v, want POST {\"file_id\":42} then the link", reqs)
	}
}

// TestDownloadFailuresCarryTheirClassification is the table the package comment
// argues for. A subtitle provider's Go error is a strike, so which answers are
// unavailable decides whether a bad afternoon — or one download too many —
// takes OpenSubtitles off the server.
func TestDownloadFailuresCarryTheirClassification(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		wantErr   bool
		wantInDet string
	}{
		{"quota", 406, `{"message":"You have downloaded your allowed 5 subtitles for 24h. Your quota will be renewed in 03 hours and 50 minutes"}`, false, "allowed 5 subtitles"},
		{"rate limited", 429, `{}`, false, "429"},
		{"outage", 503, `{}`, false, "503"},
		{"rejected key", 401, `{}`, true, ""},
		{"forbidden", 403, `{}`, true, ""},
	} {
		s := &stub{dlStatus: tc.status, dlBody: tc.body}
		resp, err := New(s.host(t)).DownloadSubtitle(context.Background(), pluginapi.SubtitleDownloadRequest{
			Candidate: pluginapi.SubtitleCandidate{ID: "42"},
		})
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: no error, want one — only an operator can fix this", tc.name)
			}
			continue
		}
		if err != nil || resp.Outcome != pluginapi.OutcomeUnavailable {
			t.Errorf("%s: %+v / %v, want unavailable and no error", tc.name, resp, err)
			continue
		}
		if !strings.Contains(resp.Detail, tc.wantInDet) {
			t.Errorf("%s: detail %q does not say %q", tc.name, resp.Detail, tc.wantInDet)
		}
	}
}

// TestSearchOutageIsUnavailable: the search half of the same rule.
func TestSearchOutageIsUnavailable(t *testing.T) {
	h := sdktest.New(
		sdktest.WithSettings(pluginapi.Settings{Secret: "k", URL: base}),
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }),
	)
	resp := search(t, h, pluginapi.SubtitleRef{Title: "Movie"})
	if resp.Outcome != pluginapi.OutcomeUnavailable {
		t.Fatalf("outcome = %q, want unavailable", resp.Outcome)
	}
}

// TestDownloadOverTheStatedCapIsUnavailable: a body the host let through but the
// caller said it will not take is answered as unavailable, naming both numbers,
// rather than handed up to be refused as a violation.
func TestDownloadOverTheStatedCapIsUnavailable(t *testing.T) {
	s := &stub{body: sampleSRT}
	resp, err := New(s.host(t)).DownloadSubtitle(context.Background(), pluginapi.SubtitleDownloadRequest{
		Candidate: pluginapi.SubtitleCandidate{ID: "42"}, MaxBytes: 10,
	})
	if err != nil || resp.Outcome != pluginapi.OutcomeUnavailable || len(resp.Data) != 0 {
		t.Fatalf("got %+v / %v, want unavailable with no data", resp, err)
	}
	if !strings.Contains(resp.Detail, "over the 10") {
		t.Errorf("detail %q does not name the cap", resp.Detail)
	}
}

// TestDownloadOfAVanishedCandidateIsNoMatch: no link is an answer.
func TestDownloadOfAVanishedCandidateIsNoMatch(t *testing.T) {
	s := &stub{dlStatus: http.StatusOK, dlBody: `{"link":""}`}
	resp, err := New(s.host(t)).DownloadSubtitle(context.Background(), pluginapi.SubtitleDownloadRequest{
		Candidate: pluginapi.SubtitleCandidate{ID: "42"},
	})
	if err != nil || resp.Outcome != pluginapi.OutcomeNoMatch {
		t.Fatalf("got %+v / %v, want no-match", resp, err)
	}
}

func TestFormatFromFilename(t *testing.T) {
	for in, want := range map[string]string{
		"a.srt": "srt", "a.ASS": "ass", "a.ssa": "ssa", "a.vtt": "vtt", "a.sub": "sub",
		"a.txt": "srt", "noext": "srt", "trailing.": "srt",
	} {
		if got := formatFromFilename(in); got != want {
			t.Errorf("formatFromFilename(%q) = %q, want %q", in, got, want)
		}
	}
}
