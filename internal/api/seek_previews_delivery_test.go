package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Delivery of Seek preview sets (ADR-0070, docs/api-contract.md "Seek previews —
// frozen contract" parts a, c, d). The set is seeded straight into the record and
// onto disk — what is under test is what the server serves, not how it was made —
// using the frozen fixture's manifest values and its two real sheets.

// previewFixture is the frozen fixture: the record it describes, its two sheets,
// and its manifest.json bytes.
func previewFixture(t *testing.T) (store.SeekPreviewSet, [][]byte, []byte) {
	t.Helper()
	dir := filepath.Join("testdata", "seekpreviews")
	var sheets [][]byte
	for _, n := range []string{"0.jpg", "1.jpg"} {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatal(err)
		}
		sheets = append(sheets, b)
	}
	manifest, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	set := store.SeekPreviewSet{
		Status:     store.SeekPreviewReady,
		IntervalMs: 10000, FrameWidth: 320, FrameHeight: 180, Columns: 10, Rows: 8, FrameCount: 150,
	}
	return set, sheets, manifest
}

// manifestFor is the fixture manifest with its example sheet prefix swapped for the
// real one, compacted (the decision embeds it unindented).
func manifestFor(t *testing.T, fixture []byte, prefix string, compact bool) []byte {
	t.Helper()
	b := bytes.ReplaceAll(fixture, []byte("/api/v1/stream/exampleStreamToken/seekPreviews"), []byte(prefix))
	if !compact {
		return b
	}
	var out bytes.Buffer
	if err := json.Compact(&out, b); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func failedSet() store.SeekPreviewSet {
	return store.SeekPreviewSet{Mtime: "m", SizeBytes: 1, Status: store.SeekPreviewFailed, Reason: "boom"}
}

// duneTitleFile scans the fixtures and returns the admin token, Dune's title id and
// its File id.
func duneTitleFile(t *testing.T, srv *testharness.Server) (token, titleID, fileID string) {
	t.Helper()
	requireFixtures(t)
	token = adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	titleID = findTitle(t, list, "Dune")
	return token, titleID, fileIDOfTitle(t, srv, token, titleID)
}

// rawDecision negotiates and returns the response's top-level keys, raw.
func rawDecision(t *testing.T, srv *testharness.Server, token, titleID string) (decisionResp, map[string]json.RawMessage) {
	t.Helper()
	var dec decisionResp
	var raw map[string]json.RawMessage
	status, body := srv.JSON(http.MethodPost, "/api/v1/titles/"+titleID+"/playback", token, mp4Profile(), &dec)
	if status != http.StatusOK {
		t.Fatalf("playback status = %d; body: %s", status, body)
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	return dec, raw
}

func TestSeekPreviewsInTheDecision(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		seed    func(t *testing.T, srv *testharness.Server, fileID string)
		present bool
	}{
		{"no record", func(*testing.T, *testharness.Server, string) {}, false},
		{"failed record", func(t *testing.T, srv *testharness.Server, id string) {
			srv.SeedSeekPreviews(id, failedSet(), nil)
		}, false},
		{"ready record", func(t *testing.T, srv *testharness.Server, id string) {
			set, sheets, _ := previewFixture(t)
			srv.SeedSeekPreviews(id, set, sheets)
		}, true},
		{"ready record, library toggle off", func(t *testing.T, srv *testharness.Server, id string) {
			set, sheets, _ := previewFixture(t)
			srv.SeedSeekPreviews(id, set, sheets)
			var libs struct {
				Libraries []struct {
					ID string `json:"id"`
				} `json:"libraries"`
			}
			srv.AuthGET("/api/v1/libraries", adminLogin(t, srv), &libs)
			for _, l := range libs.Libraries {
				srv.SetSeekPreviewsEnabled(l.ID, false)
			}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := testharness.New(t)
			token, titleID, fileID := duneTitleFile(t, srv)
			tc.seed(t, srv, fileID)
			dec, raw := rawDecision(t, srv, token, titleID)
			got, ok := raw["seekPreviews"]
			if ok != tc.present {
				t.Fatalf("seekPreviews present = %v, want %v", ok, tc.present)
			}
			if !tc.present {
				return
			}
			_, _, fixture := previewFixture(t)
			want := manifestFor(t, fixture, "/api/v1/stream/"+dec.StreamToken+"/seekPreviews", true)
			if !bytes.Equal(got, want) {
				t.Fatalf("manifest bytes differ from the frozen fixture\ngot:  %s\nwant: %s", got, want)
			}
		})
	}
}

// adminLogin signs the harness admin in again (same credentials adminToken sets up).
func adminLogin(t *testing.T, srv *testharness.Server) string {
	t.Helper()
	return srv.LoginAs("brandon", "hunter2hunter2")
}

// seededStream is a session over Dune with the fixture's set seeded.
type seededStream struct {
	srv    *testharness.Server
	token  string
	fileID string
	dec    decisionResp
	sheets [][]byte
}

func newSeededStream(t *testing.T) seededStream {
	t.Helper()
	srv := testharness.New(t)
	token, titleID, fileID := duneTitleFile(t, srv)
	set, sheets, _ := previewFixture(t)
	srv.SeedSeekPreviews(fileID, set, sheets)
	dec, _ := rawDecision(t, srv, token, titleID)
	return seededStream{srv, token, fileID, dec, sheets}
}

func (s seededStream) path(tail string) string {
	return "/api/v1/stream/" + s.dec.StreamToken + "/seekPreviews/" + tail
}

func TestSeekPreviewsStreamRoute(t *testing.T) {
	t.Parallel()
	s := newSeededStream(t)

	t.Run("serves the on-disk sheets", func(t *testing.T) {
		for i, want := range s.sheets {
			r := streamDo(t, s.srv, http.MethodGet, s.path(string(rune('0'+i))+".jpg"), "", nil)
			if r.status != http.StatusOK || !bytes.Equal(r.body, want) {
				t.Fatalf("sheet %d: status %d, %d bytes, want 200 and %d identical bytes", i, r.status, len(r.body), len(want))
			}
			if ct := r.header.Get("Content-Type"); ct != "image/jpeg" {
				t.Errorf("sheet %d Content-Type = %q", i, ct)
			}
		}
	})

	t.Run("HEAD", func(t *testing.T) {
		r := streamDo(t, s.srv, http.MethodHead, s.path("0.jpg"), "", nil)
		if r.status != http.StatusOK || len(r.body) != 0 || r.header.Get("Content-Type") != "image/jpeg" {
			t.Fatalf("HEAD: status %d, %d body bytes, type %q", r.status, len(r.body), r.header.Get("Content-Type"))
		}
		if r.header.Get("Content-Length") == "" {
			t.Error("HEAD carried no Content-Length")
		}
	})

	t.Run("CORS on the 200 and the preflight", func(t *testing.T) {
		r := streamDo(t, s.srv, http.MethodGet, s.path("0.jpg"), castOrigin, nil)
		assertStreamCORS(t, "sheet", r, castOrigin)
		p := streamDo(t, s.srv, http.MethodOptions, s.path("0.jpg"), castOrigin, nil)
		if p.status != http.StatusNoContent {
			t.Fatalf("preflight = %d, want 204", p.status)
		}
		assertStreamCORS(t, "preflight", p, castOrigin)
	})

	t.Run("methods other than GET HEAD OPTIONS are 405", func(t *testing.T) {
		for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
			r := streamDo(t, s.srv, m, s.path("0.jpg"), castOrigin, nil)
			if r.status != http.StatusMethodNotAllowed || r.header.Get("Allow") != "GET, HEAD" {
				t.Errorf("%s = %d (Allow %q), want 405 with Allow: GET, HEAD", m, r.status, r.header.Get("Allow"))
			}
		}
	})

	// Every refusal is the byte-identical 404 the other stream-token routes give.
	base := streamDo(t, s.srv, http.MethodGet, "/api/v1/stream/not-a-token/stream", castOrigin, nil)
	if base.status != http.StatusNotFound {
		t.Fatalf("baseline refusal = %d, want 404", base.status)
	}
	for _, tc := range []struct{ name, path string }{
		{"bad token", "/api/v1/stream/not-a-token/seekPreviews/0.jpg"},
		{"index out of range", s.path("2.jpg")},
		{"far out of range", s.path("999999.jpg")},
		{"negative", s.path("-1.jpg")},
		{"non-integer", s.path("abc.jpg")},
		{"non-canonical", s.path("01.jpg")},
		{"signed", s.path("+1.jpg")},
		{"no extension", s.path("0")},
		{"wrong extension", s.path("0.png")},
		{"empty name", s.path("")},
		{"nested", s.path("0.jpg/x")},
	} {
		t.Run("refusal: "+tc.name, func(t *testing.T) {
			r := streamDo(t, s.srv, http.MethodGet, tc.path, castOrigin, nil)
			if r.status != base.status || !bytes.Equal(r.body, base.body) {
				t.Fatalf("got %d %s, want %d %s", r.status, r.body, base.status, base.body)
			}
			assertStreamCORS(t, tc.name, r, castOrigin)
		})
	}

	t.Run("refusal: file sheet missing on disk", func(t *testing.T) {
		if err := os.Remove(filepath.Join(s.srv.DataDir, "seekpreviews", s.fileID, "1.jpg")); err != nil {
			t.Fatal(err)
		}
		r := streamDo(t, s.srv, http.MethodGet, s.path("1.jpg"), castOrigin, nil)
		if r.status != base.status || !bytes.Equal(r.body, base.body) {
			t.Fatalf("got %d %s", r.status, r.body)
		}
	})
}

func TestSeekPreviewsStreamRouteWithoutAReadySet(t *testing.T) {
	t.Parallel()
	for name, seed := range map[string]func(*testharness.Server, string){
		"no record":     func(*testharness.Server, string) {},
		"failed record": func(srv *testharness.Server, id string) { srv.SeedSeekPreviews(id, failedSet(), nil) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := testharness.New(t)
			token, titleID, fileID := duneTitleFile(t, srv)
			dec, _ := rawDecision(t, srv, token, titleID)
			seed(srv, fileID)
			base := streamDo(t, srv, http.MethodGet, "/api/v1/stream/not-a-token/stream", "", nil)
			r := streamDo(t, srv, http.MethodGet, "/api/v1/stream/"+dec.StreamToken+"/seekPreviews/0.jpg", "", nil)
			if r.status != http.StatusNotFound || !bytes.Equal(r.body, base.body) {
				t.Fatalf("got %d %s, want the stream-token 404 %s", r.status, r.body, base.body)
			}
		})
	}
}

func TestSeekPreviewsFileRoutes(t *testing.T) {
	t.Parallel()
	srv := testharness.New(t)
	token, _, fileID := duneTitleFile(t, srv)
	set, sheets, fixture := previewFixture(t)
	srv.SeedSeekPreviews(fileID, set, sheets)
	base := "/api/v1/files/" + fileID + "/seekPreviews"

	t.Run("manifest", func(t *testing.T) {
		status, body := srv.AuthGET(base, token, nil)
		if status != http.StatusOK {
			t.Fatalf("status %d; body: %s", status, body)
		}
		var got bytes.Buffer
		if err := json.Compact(&got, body); err != nil {
			t.Fatal(err)
		}
		if want := manifestFor(t, fixture, base, true); got.String() != string(want) {
			t.Fatalf("manifest\ngot:  %s\nwant: %s", got.String(), want)
		}
	})
	t.Run("sheets", func(t *testing.T) {
		for i, want := range sheets {
			status, body := srv.AuthGET(base+"/"+string(rune('0'+i))+".jpg", token, nil)
			if status != http.StatusOK || !bytes.Equal(body, want) {
				t.Fatalf("sheet %d: %d, %d bytes", i, status, len(body))
			}
		}
	})
	t.Run("?token= alone is refused like no credential", func(t *testing.T) {
		for _, p := range []string{base, base + "/0.jpg"} {
			noCred, noBody := srv.GET(p, nil)
			withTok, tokBody := srv.GET(p+"?token="+token, nil)
			if noCred != http.StatusUnauthorized || withTok != noCred || !bytes.Equal(noBody, tokBody) {
				t.Errorf("%s: no credential %d, ?token= %d (bodies equal: %v), want identical 401s", p, noCred, withTok, bytes.Equal(noBody, tokBody))
			}
		}
	})

	dl404Status, dl404 := srv.AuthGET("/api/v1/files/"+unknownFileID+"/download", token, nil)
	if dl404Status != http.StatusNotFound {
		t.Fatalf("download baseline = %d", dl404Status)
	}
	for _, tc := range []struct{ name, path string }{
		{"unknown file manifest", "/api/v1/files/" + unknownFileID + "/seekPreviews"},
		{"unknown file sheet", "/api/v1/files/" + unknownFileID + "/seekPreviews/0.jpg"},
		{"index out of range", base + "/2.jpg"},
		{"non-integer", base + "/abc.jpg"},
		{"non-canonical", base + "/00.jpg"},
		{"negative", base + "/-1.jpg"},
		{"signed zero", base + "/+0.jpg"},
		{"negative zero", base + "/-0.jpg"},
		{"no extension", base + "/0"},
	} {
		t.Run("refusal: "+tc.name, func(t *testing.T) {
			status, body := srv.AuthGET(tc.path, token, nil)
			if status != dl404Status || !bytes.Equal(body, dl404) {
				t.Fatalf("got %d %s, want %d %s", status, body, dl404Status, dl404)
			}
		})
	}

	t.Run("sheet unreadable on disk is file unavailable", func(t *testing.T) {
		if err := os.Remove(filepath.Join(srv.DataDir, "seekpreviews", fileID, "1.jpg")); err != nil {
			t.Fatal(err)
		}
		status, body := srv.AuthGET(base+"/1.jpg", token, nil)
		if status != http.StatusNotFound || !strings.Contains(string(body), `"file unavailable"`) {
			t.Fatalf("got %d %s, want 404 file unavailable", status, body)
		}
	})

	t.Run("Missing File", func(t *testing.T) {
		srv.Exec(`UPDATE files SET present = 0 WHERE id = ?`, fileID)
		for _, p := range []string{base, base + "/0.jpg"} {
			status, body := srv.AuthGET(p, token, nil)
			if status != dl404Status || !bytes.Equal(body, dl404) {
				t.Errorf("%s: got %d %s", p, status, body)
			}
		}
	})
}

func TestSeekPreviewsFileRoutesNoReadySet(t *testing.T) {
	t.Parallel()
	for name, seed := range map[string]func(*testharness.Server, string){
		"no record":     func(*testharness.Server, string) {},
		"failed record": func(srv *testharness.Server, id string) { srv.SeedSeekPreviews(id, failedSet(), nil) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := testharness.New(t)
			token, _, fileID := duneTitleFile(t, srv)
			seed(srv, fileID)
			dlStatus, dl := srv.AuthGET("/api/v1/files/"+unknownFileID+"/download", token, nil)
			for _, p := range []string{"", "/0.jpg"} {
				status, body := srv.AuthGET("/api/v1/files/"+fileID+"/seekPreviews"+p, token, nil)
				if status != dlStatus || !bytes.Equal(body, dl) {
					t.Errorf("%q: got %d %s, want %d %s", p, status, body, dlStatus, dl)
				}
			}
		})
	}
}

// A Member sees a File's previews only where they could download it: the Library
// grant and the Rating ceiling both apply, and the refusal is the download route's.
func TestSeekPreviewsFileRoutesRespectScope(t *testing.T) {
	t.Parallel()
	requireFixtures(t)
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	root2 := testharness.MutableLibraryDir(t, fixtureRoot(t))
	lib1 := createMovieLibrary(t, srv, admin, fixtureRoot(t))
	lib2 := createMovieLibrary(t, srv, admin, root2)
	scanLib(t, srv, admin, lib1, "")
	scanLib(t, srv, admin, lib2, "")
	var l1, l2 titlesListResp
	srv.AuthGET("/api/v1/libraries/"+lib1+"/titles?limit=50", admin, &l1)
	srv.AuthGET("/api/v1/libraries/"+lib2+"/titles?limit=50", admin, &l2)
	blade := findTitle(t, l1, "Blade Runner")
	dune := findTitle(t, l1, "Dune")
	srv.SetTitleContentRating(blade, "R")
	srv.SetTitleContentRating(dune, "PG-13")
	aboveCeiling := fileIDOfTitle(t, srv, admin, blade)
	allowed := fileIDOfTitle(t, srv, admin, dune)
	ungranted := fileIDOfTitle(t, srv, admin, findTitle(t, l2, "Dune"))
	set, sheets, _ := previewFixture(t)
	for _, id := range []string{aboveCeiling, allowed, ungranted} {
		srv.SeedSeekPreviews(id, set, sheets)
	}

	memberID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	grantLibraries(t, srv, admin, memberID, lib1)
	setRatingCeiling(t, srv, admin, memberID, "PG-13")
	member := srv.LoginAs("kid", "memberpass123")

	baseStatus, base := srv.AuthGET("/api/v1/files/"+unknownFileID+"/download", member, nil)
	for _, tc := range []struct {
		name, id string
		ok       bool
	}{
		{"granted, at ceiling", allowed, true},
		{"ungranted library", ungranted, false},
		{"above rating ceiling", aboveCeiling, false},
	} {
		for _, tail := range []string{"", "/0.jpg"} {
			status, body := srv.AuthGET("/api/v1/files/"+tc.id+"/seekPreviews"+tail, member, nil)
			switch {
			case tc.ok && status != http.StatusOK:
				t.Errorf("%s%s: %d, want 200", tc.name, tail, status)
			case !tc.ok && (status != baseStatus || !bytes.Equal(body, base)):
				t.Errorf("%s%s: %d %s, want the unknown-id 404 %s", tc.name, tail, status, body, base)
			}
		}
	}
	// The Admin is never capped.
	if st, _ := srv.AuthGET("/api/v1/files/"+aboveCeiling+"/seekPreviews", admin, nil); st != http.StatusOK {
		t.Errorf("admin above ceiling = %d, want 200", st)
	}
}

// A Linked Library's previews are the sharer's: the home decision carries the
// manifest with its OWN token's URLs, and a sheet fetched there is the sharer's
// bytes, spent through the relay. Nothing is generated or stored at home.
func TestSeekPreviewsRelayedFromTheSharer(t *testing.T) {
	t.Parallel()
	f := linkForRelay(t)
	set, sheets, _ := previewFixture(t)
	var list titlesListResp
	f.sharer.AuthGET("/api/v1/libraries/"+f.srcLib+"/titles?limit=50", f.sharerAdmin, &list)
	// Every File of the sharer's Dune, so whichever Edition it picks has the set.
	var detail titleDetailResp
	f.sharer.AuthGET("/api/v1/titles/"+findTitle(t, list, "Dune"), f.sharerAdmin, &detail)
	for _, ed := range detail.Editions {
		for _, file := range ed.Files {
			f.sharer.SeedSeekPreviews(file.ID, set, sheets)
		}
	}

	var raw map[string]json.RawMessage
	var dec decisionResp
	status, body := f.home.JSON(http.MethodPost, "/api/v1/titles/"+f.mirroredTitle(t, "Dune")+"/playback", f.homeAdmin, mp4Profile(), &dec)
	if status != http.StatusOK {
		t.Fatalf("relayed playback = %d; body: %s", status, body)
	}
	_ = json.Unmarshal(body, &raw)
	var m struct {
		IntervalMs int      `json:"intervalMs"`
		FrameCount int      `json:"frameCount"`
		Sheets     []string `json:"sheets"`
	}
	if err := json.Unmarshal(raw["seekPreviews"], &m); err != nil || m.FrameCount != 150 || m.IntervalMs != 10000 {
		t.Fatalf("relayed seekPreviews = %s (%v)", raw["seekPreviews"], err)
	}
	prefix := "/api/v1/stream/" + dec.StreamToken + "/seekPreviews/"
	if len(m.Sheets) != 2 || m.Sheets[0] != prefix+"0.jpg" || m.Sheets[1] != prefix+"1.jpg" {
		t.Fatalf("sheet URLs = %v, want this server's token route %s…", m.Sheets, prefix)
	}
	for i, want := range sheets {
		r := streamDo(t, f.home, http.MethodGet, m.Sheets[i], "", nil)
		if r.status != http.StatusOK || !bytes.Equal(r.body, want) || r.header.Get("Content-Type") != "image/jpeg" {
			t.Fatalf("relayed sheet %d: %d, %d bytes, type %q", i, r.status, len(r.body), r.header.Get("Content-Type"))
		}
	}
	if f.rec.countPath("/api/v1/sessions/") == 0 {
		t.Error("no sheet request reached the sharer")
	}
	// Out of range and malformed names are refused HERE, never forwarded.
	before := len(f.rec.snapshot())
	base := streamDo(t, f.home, http.MethodGet, "/api/v1/stream/not-a-token/stream", "", nil)
	for _, name := range []string{"x.jpg", "01.jpg", "0", "-1.jpg", "+0.jpg", "-0.jpg"} {
		r := streamDo(t, f.home, http.MethodGet, prefix+name, "", nil)
		if r.status != http.StatusNotFound || !bytes.Equal(r.body, base.body) {
			t.Errorf("%s: %d %s", name, r.status, r.body)
		}
	}
	if len(f.rec.snapshot()) != before {
		t.Error("a malformed sheet name was forwarded to the sharer")
	}
	// The sharer's own 404 (no such sheet) passes through as a 404.
	if r := streamDo(t, f.home, http.MethodGet, prefix+"2.jpg", "", nil); r.status != http.StatusNotFound {
		t.Errorf("relayed out-of-range sheet = %d, want 404", r.status)
	}

	// A sharer with no set sends none, and neither does the home decision.
	other := f.mirroredTitle(t, "Blade Runner")
	_, rawOther := rawDecision(t, f.home, f.homeAdmin, other)
	if _, ok := rawOther["seekPreviews"]; ok {
		t.Error("the home decision invented seekPreviews the sharer did not send")
	}
}

// A set recorded against an older mtime or size than the File row now has is stale
// (ADR-0070 §7): every route stops serving it at once, without waiting for the
// generator to delete it.
func TestSeekPreviewsStaleSetIsNotServed(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"size_bytes = size_bytes + 1", "mtime = mtime || 'x'"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			s := newSeededStream(t)
			s.srv.Exec(`UPDATE files SET `+change+` WHERE id = ?`, s.fileID)

			if r := streamDo(t, s.srv, http.MethodGet, s.path("0.jpg"), "", nil); r.status != http.StatusNotFound {
				t.Errorf("stream sheet of a stale set = %d, want 404", r.status)
			}
			dlStatus, dl := s.srv.AuthGET("/api/v1/files/"+unknownFileID+"/download", s.token, nil)
			for _, tail := range []string{"", "/0.jpg"} {
				status, body := s.srv.AuthGET("/api/v1/files/"+s.fileID+"/seekPreviews"+tail, s.token, nil)
				if status != dlStatus || !bytes.Equal(body, dl) {
					t.Errorf("file route %q of a stale set: %d %s, want %d %s", tail, status, body, dlStatus, dl)
				}
			}
			_, raw := rawDecisionOfFile(t, s)
			if _, ok := raw["seekPreviews"]; ok {
				t.Error("the decision still carries seekPreviews for a stale set")
			}
		})
	}
}

// rawDecisionOfFile negotiates a fresh decision for the seeded stream's Title.
func rawDecisionOfFile(t *testing.T, s seededStream) (decisionResp, map[string]json.RawMessage) {
	t.Helper()
	var list titlesListResp
	var lib struct {
		Libraries []struct {
			ID string `json:"id"`
		} `json:"libraries"`
	}
	s.srv.AuthGET("/api/v1/libraries", s.token, &lib)
	s.srv.AuthGET("/api/v1/libraries/"+lib.Libraries[0].ID+"/titles?limit=50", s.token, &list)
	return rawDecision(t, s.srv, s.token, findTitle(t, list, "Dune"))
}

// GET /sessions/{id}/seekPreviews/{n}.jpg is the session owner's: another User's
// bearer gets the stream-token 404, the owner gets the sheet.
func TestSeekPreviewsSessionRouteIsTheOwners(t *testing.T) {
	t.Parallel()
	s := newSeededStream(t)
	path := "/api/v1/sessions/" + s.dec.SessionID + "/seekPreviews/0.jpg"

	if status, body := s.srv.AuthGET(path, s.token, nil); status != http.StatusOK || !bytes.Equal(body, s.sheets[0]) {
		t.Fatalf("owner: %d, %d bytes, want 200 and the sheet", status, len(body))
	}
	s.srv.CreateUser(s.token, "other", "otherpass1234", "member")
	other := s.srv.LoginAs("other", "otherpass1234")
	base := streamDo(t, s.srv, http.MethodGet, "/api/v1/stream/not-a-token/stream", "", nil)
	status, body := s.srv.AuthGET(path, other, nil)
	if status != http.StatusNotFound || !bytes.Equal(body, base.body) {
		t.Fatalf("another user: %d %s, want the stream-token 404 %s", status, body, base.body)
	}
}

// A multi-part Edition's File frames would not line up with the Edition's timeline,
// so its decision carries no seekPreviews even when its Files have ready sets.
func TestSeekPreviewsMultiPartEditionGetsNone(t *testing.T) {
	t.Parallel()
	requireNamingFixtures(t)
	srv, token, libID := scanNamingLibrary(t)
	list := listAllTitles(t, srv, token, libID)
	id := findNamingTitle(t, list, "Split Movie")
	d := getDetail(t, srv, token, id)
	if len(d.Editions) != 1 || len(d.Editions[0].Files) != 2 {
		t.Fatalf("Split Movie = %+v, want 1 Edition of 2 Files", d.Editions)
	}
	set, sheets, _ := previewFixture(t)
	for _, f := range d.Editions[0].Files {
		srv.SeedSeekPreviews(f.ID, set, sheets)
	}
	_, raw := rawDecision(t, srv, token, id)
	if _, ok := raw["seekPreviews"]; ok {
		t.Error("a multi-part Edition's decision carries seekPreviews")
	}
}

// linkForRelayVia is linkForRelay with the sharer's playback answers passed through
// mutate before they reach the home Server.
func linkForRelayVia(t *testing.T, mutate func(map[string]any)) *relayFixture {
	t.Helper()
	requireFixtures(t)
	f := &relayFixture{rec: &relayRecorder{}}
	f.sharer = testharness.New(t)
	f.sharerAdmin = adminToken(t, f.sharer)
	f.srcLib = createMovieLibrary(t, f.sharer, f.sharerAdmin, fixtureRoot(t))
	scanLib(t, f.sharer, f.sharerAdmin, f.srcLib, "")
	f.remoteUser = createRemoteUser(t, f.sharer, f.sharerAdmin, "The other household")
	grantLibraries(t, f.sharer, f.sharerAdmin, f.remoteUser, f.srcLib)

	inner := f.rec.wrap(f.sharer.Handler())
	f.proxy = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/playback") {
			inner.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		inner.ServeHTTP(rec, r)
		body := rec.Body.Bytes()
		var m map[string]any
		if rec.Code == http.StatusOK && json.Unmarshal(body, &m) == nil {
			mutate(m)
			body, _ = json.Marshal(m)
		}
		for k, v := range rec.Header() {
			if k != "Content-Length" {
				w.Header()[k] = v
			}
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(body)
	}))
	t.Cleanup(f.proxy.Close)

	f.home = testharness.New(t)
	f.homeAdmin = adminToken(t, f.home)
	invite := mintInvite(t, f.sharer, f.sharerAdmin, f.remoteUser, f.proxy.URL).Invite
	var l linkResp
	if status, body := postLink(t, f.home, f.homeAdmin, invite, &l); status != http.StatusCreated {
		t.Fatalf("POST /links = %d, want 201; body: %s", status, body)
	}
	f.mirrorLib = l.Libraries[0].ID
	return f
}

func seedSharerDune(t *testing.T, f *relayFixture) {
	t.Helper()
	set, sheets, _ := previewFixture(t)
	var list titlesListResp
	f.sharer.AuthGET("/api/v1/libraries/"+f.srcLib+"/titles?limit=50", f.sharerAdmin, &list)
	var detail titleDetailResp
	f.sharer.AuthGET("/api/v1/titles/"+findTitle(t, list, "Dune"), f.sharerAdmin, &detail)
	for _, ed := range detail.Editions {
		for _, file := range ed.Files {
			f.sharer.SeedSeekPreviews(file.ID, set, sheets)
		}
	}
}

// The home decision's manifest is rebuilt, not copied: the sharer's token never
// appears, the key order is the fixture's whatever order the sharer used, and a key
// the contract does not name is dropped.
func TestSeekPreviewsRelayedManifestIsRebuilt(t *testing.T) {
	t.Parallel()
	// Re-marshalling a map sorts its keys, so the sharer's order is alphabetical;
	// the sheets carry a recognisable sharer token.
	f := linkForRelayVia(t, func(m map[string]any) {
		sp, ok := m["seekPreviews"].(map[string]any)
		if !ok {
			return
		}
		sp["bogusKey"] = "x"
		sp["sheets"] = []any{"/api/v1/stream/SHARERTOKEN/seekPreviews/0.jpg", "/api/v1/stream/SHARERTOKEN/seekPreviews/1.jpg"}
	})
	seedSharerDune(t, f)

	var dec decisionResp
	status, body := f.home.JSON(http.MethodPost, "/api/v1/titles/"+f.mirroredTitle(t, "Dune")+"/playback", f.homeAdmin, mp4Profile(), &dec)
	if status != http.StatusOK {
		t.Fatalf("relayed playback = %d; body: %s", status, body)
	}
	if bytes.Contains(body, []byte("SHARERTOKEN")) {
		t.Errorf("the home decision leaks the sharer's stream token: %s", body)
	}
	for _, m := range regexp.MustCompile(`/api/v1/stream/([^/"]+)`).FindAllSubmatch(body, -1) {
		if string(m[1]) != dec.StreamToken {
			t.Errorf("the decision names stream token %q, want only the local %q", m[1], dec.StreamToken)
		}
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	_, _, fixture := previewFixture(t)
	want := manifestFor(t, fixture, "/api/v1/stream/"+dec.StreamToken+"/seekPreviews", true)
	if !bytes.Equal(raw["seekPreviews"], want) {
		t.Fatalf("relayed manifest\ngot:  %s\nwant: %s", raw["seekPreviews"], want)
	}
}

// A sharer manifest that is not a manifest is omitted rather than relayed.
func TestSeekPreviewsRelayedMalformedManifestIsOmitted(t *testing.T) {
	t.Parallel()
	for name, bad := range map[string]any{
		"not an object": "garbage",
		"sheets absent": map[string]any{"intervalMs": 10000},
		"sheets wrong":  map[string]any{"intervalMs": 10000, "frameCount": 150, "sheets": "x"},
		"field wrong":   map[string]any{"intervalMs": "ten", "frameCount": 150, "sheets": []any{"a"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := linkForRelayVia(t, func(m map[string]any) { m["seekPreviews"] = bad })
			_, raw := rawDecision(t, f.home, f.homeAdmin, f.mirroredTitle(t, "Dune"))
			if v, ok := raw["seekPreviews"]; ok {
				t.Errorf("a malformed sharer manifest was relayed: %s", v)
			}
		})
	}
}

// A manifest with the right types but values no client can lay out is omitted:
// every number positive, and the sheet count exactly what the frames need.
func TestSeekPreviewsRelayedImplausibleManifestIsOmitted(t *testing.T) {
	t.Parallel()
	valid := func() map[string]any {
		return map[string]any{
			"intervalMs": 10000, "frameWidth": 320, "frameHeight": 180,
			"columns": 10, "rows": 8, "frameCount": 150,
			"sheets": []any{"a", "b"},
		}
	}
	with := func(k string, v any) map[string]any { m := valid(); m[k] = v; return m }
	without := func(k string) map[string]any { m := valid(); delete(m, k); return m }
	cases := map[string]map[string]any{
		"too many sheets": with("sheets", []any{"a", "b", "c"}),
		"too few sheets":  with("sheets", []any{"a"}),
		"huge grid":       with("columns", int64(1)<<32),
		"overflowing grid": func() map[string]any {
			m := with("columns", int64(1)<<32)
			m["rows"] = int64(1) << 32
			return m
		}(),
		"huge columns":    with("columns", 1001),
		"huge rows":       with("rows", 1001),
		"huge frameCount": with("frameCount", int64(1)<<62),
		// Sheet counts agree with the frames, so only the grid bound rejects these.
		"columns past bound, sheets agree": func() map[string]any {
			m := with("columns", 1001)
			m["rows"], m["frameCount"], m["sheets"] = 8, 8008, []any{"a"}
			return m
		}(),
		"rows past bound, sheets agree": func() map[string]any {
			m := with("rows", 1001)
			m["columns"], m["frameCount"], m["sheets"] = 8, 8008, []any{"a"}
			return m
		}(),
		"columns wrapping product": func() map[string]any {
			m := with("columns", int64(1)<<62)
			m["rows"] = 4
			return m
		}(),
		"rows wrapping product": func() map[string]any {
			m := with("rows", int64(1)<<62)
			m["columns"] = 4
			return m
		}(),
		// Zero sheets agree with a non-positive frameCount, so only that guard rejects these.
		"frameCount zero, no sheets":     with("frameCount", 0),
		"frameCount negative, no sheets": with("frameCount", -1),
	}
	for _, k := range []string{"frameCount zero, no sheets", "frameCount negative, no sheets"} {
		cases[k]["sheets"] = []any{}
	}
	for _, k := range []string{"intervalMs", "frameWidth", "frameHeight", "columns", "rows", "frameCount"} {
		cases[k+" zero"] = with(k, 0)
		cases[k+" negative"] = with(k, -1)
		cases[k+" missing"] = without(k)
	}

	var mu sync.Mutex
	var current any
	f := linkForRelayVia(t, func(m map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		m["seekPreviews"] = current
	})
	title := f.mirroredTitle(t, "Dune")
	relay := func(v any) (decisionResp, map[string]json.RawMessage) {
		mu.Lock()
		current = v
		mu.Unlock()
		return rawDecision(t, f.home, f.homeAdmin, title)
	}

	if _, raw := relay(valid()); raw["seekPreviews"] == nil {
		t.Fatal("a plausible sharer manifest was not relayed")
	}
	for name, bad := range cases {
		if _, raw := relay(bad); raw["seekPreviews"] != nil {
			t.Errorf("%s: an implausible manifest was relayed: %s", name, raw["seekPreviews"])
		}
	}
}

// The relay refuses a malformed sheet name itself: it never becomes a request to
// the sharer, whose credential it would be spent under.
func TestSeekPreviewsRelaySheetNamesAreChecked(t *testing.T) {
	t.Parallel()
	f := linkForRelay(t)
	var dec decisionResp
	status, body := f.home.JSON(http.MethodPost, "/api/v1/titles/"+f.mirroredTitle(t, "Dune")+"/playback", f.homeAdmin, mp4Profile(), &dec)
	if status != http.StatusOK {
		t.Fatalf("relayed playback = %d; body: %s", status, body)
	}
	remote := relayRemoteSessionOf(t, dec.StreamURL)
	base := "/api/v1/relay/" + dec.SessionID + "/sessions/" + remote + "/seekPreviews/"

	sharerSheets := "/api/v1/sessions/" + remote + "/seekPreviews/"
	// The well-formed name is allowed through (the sharer has no set: its 404).
	before := f.rec.countPath(sharerSheets)
	resp := authStream(t, f.home, base+"0.jpg", f.homeAdmin, "")
	resp.Body.Close()
	if f.rec.countPath(sharerSheets) != before+1 {
		t.Fatal("a well-formed sheet name was not relayed to the sharer")
	}

	before = len(f.rec.snapshot())
	for _, name := range []string{"-1.jpg", "01.jpg", "x.jpg"} {
		resp := authStream(t, f.home, base+name, f.homeAdmin, "")
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", name, resp.StatusCode)
		}
	}
	if len(f.rec.snapshot()) != before {
		t.Error("a malformed sheet name reached the sharer")
	}
}
