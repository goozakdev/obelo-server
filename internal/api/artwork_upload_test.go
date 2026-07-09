package api_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/marioquake/juicebox/internal/enrich"
	"github.com/marioquake/juicebox/internal/testharness"
)

// artwork-management issue 03 black-box tests: the one genuinely new capability —
// uploading your own image, where uploading IS selecting (ADR-0026). Driven
// through the real HTTP surface with the FAKE MetadataProvider/ArtworkFetcher
// (zero network). The hard invariants: an upload is served + Locked; it OUTRANKS a
// local folder image (uploaded > local > fetched); a bad type/size is rejected and
// leaves the current image untouched; releasing the Lock deletes the upload and
// reverts to the auto image; a locked upload survives re-enrichment; all actions
// are Admin-only and emit the realtime SSE nudge. Prior art: fixlabel_test.go.

// --- image byte builders (correct magic so http.DetectContentType classifies) ---

// jpegImage returns bytes that sniff as image/jpeg (FF D8 FF …), carrying a marker
// so a served response can be matched to the exact upload.
func jpegImage(marker string) []byte {
	b := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00}
	return append(b, []byte(marker)...)
}

// pngImage returns bytes that sniff as image/png.
func pngImage(marker string) []byte {
	b := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	return append(b, []byte(marker)...)
}

// gifImage returns bytes that sniff as image/gif — a DISALLOWED type.
func gifImage() []byte { return []byte("GIF89a\x01\x00\x01\x00") }

// pdfBytes returns bytes that sniff as application/pdf — a DISALLOWED type.
func pdfBytes() []byte { return []byte("%PDF-1.7\n%\xE2\xE3\xCF\xD3\n") }

// --- helpers ----------------------------------------------------------------

// uploadArtwork POSTs a single image part to a role's upload endpoint and returns
// the status. It is the multipart analogue of the pick PUT.
func uploadArtwork(t *testing.T, srv *testharness.Server, token, path, contentType string, content []byte, out any) int {
	t.Helper()
	st, _ := srv.Multipart(http.MethodPost, path, token, "image", "upload.bin", contentType, content, out)
	return st
}

// servedArtwork does an authenticated raw GET of an artwork byte route, returning
// status, body, and the response Content-Type (so a test can assert an image type).
func servedArtwork(t *testing.T, srv *testharness.Server, token, path string) (int, []byte, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL(path), nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body, resp.Header.Get("Content-Type")
}

// --- Leaf: upload → serve + Lock; survives re-enrichment --------------------

// TestArtworkUploadLeafServeLockAndSurvivesEnrich: an Admin uploads a poster to a
// Movie with no local art; the exact bytes are served with an image content-type,
// the poster role is reported Locked and sourced 'uploaded', and a full re-enrich
// (which would refetch an unlocked role) leaves the upload in place.
func TestArtworkUploadLeafServeLockAndSurvivesEnrich(t *testing.T) {
	requireNamingFixtures(t)
	prov := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("AUTOPOSTER")}),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, namingRoot(t))
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")

	id := titleIDByName(t, srv, token, libID, "Pinned Movie") // no local poster

	up := jpegImage("UPLOADEDPOSTER")
	var afterUpload labelDetailResp
	if st := uploadArtwork(t, srv, token, "/api/v1/titles/"+id+"/artworkUpload?role=poster", "image/jpeg", up, &afterUpload); st != http.StatusOK {
		t.Fatalf("upload poster = %d, want 200", st)
	}
	if !contains(afterUpload.LockedFields, "poster") {
		t.Errorf("poster not Locked after upload: %+v", afterUpload.LockedFields)
	}
	if src := posterSourceOf(afterUpload); src != "uploaded" {
		t.Errorf("poster source after upload = %q, want uploaded", src)
	}

	st, body, ct := servedArtwork(t, srv, token, "/api/v1/titles/"+id+"/artwork/poster")
	if st != http.StatusOK || string(body) != string(up) {
		t.Fatalf("served poster after upload = %d %q, want the uploaded bytes", st, body)
	}
	if !strings.HasPrefix(ct, "image/") {
		t.Errorf("served poster content-type = %q, want an image/* type", ct)
	}

	// A full re-enrich would refetch an unlocked poster; the locked upload stays.
	enrichLib(t, srv, token, libID, "full")
	if !contains(getLabelDetail(t, srv, token, id).LockedFields, "poster") {
		t.Errorf("poster lock lost across re-enrich")
	}
	if st, body, _ := servedArtwork(t, srv, token, "/api/v1/titles/"+id+"/artwork/poster"); st != http.StatusOK || string(body) != string(up) {
		t.Errorf("upload did not survive re-enrich: served %d %q", st, body)
	}
}

// --- Leaf: uploaded outranks a local folder image ---------------------------

// TestArtworkUploadBeatsLocal: with a local poster.jpg present, an upload still
// wins at serve time (uploaded > local, ADR-0026) — the sole source that beats a
// folder image. A picked provider image (fetched) would still lose to local.
func TestArtworkUploadBeatsLocal(t *testing.T) {
	requireNamingFixtures(t)
	prov := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("AUTOPOSTER")}),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, namingRoot(t))
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")

	id := titleIDByName(t, srv, token, libID, "Extras Movie") // ships a local poster.jpg

	// Before upload: local wins (serve is NOT the auto/fetched bytes).
	if _, body, _ := servedArtwork(t, srv, token, "/api/v1/titles/"+id+"/artwork/poster"); string(body) == "AUTOPOSTER" {
		t.Fatalf("precondition: expected local poster to win before upload")
	}

	up := pngImage("UPLOADEDOVERLOCAL")
	var afterUpload labelDetailResp
	if st := uploadArtwork(t, srv, token, "/api/v1/titles/"+id+"/artworkUpload?role=poster", "image/png", up, &afterUpload); st != http.StatusOK {
		t.Fatalf("upload poster (over local) = %d, want 200", st)
	}
	if src := posterSourceOf(afterUpload); src != "uploaded" {
		t.Errorf("poster source with a local file present = %q, want uploaded (upload wins)", src)
	}
	if st, body, _ := servedArtwork(t, srv, token, "/api/v1/titles/"+id+"/artwork/poster"); st != http.StatusOK || string(body) != string(up) {
		t.Errorf("served poster = %d %q, want the uploaded bytes (uploaded beats local)", st, body)
	}
}

// --- Validation: bad type / oversize rejected, image untouched --------------

// TestArtworkUploadValidation: a non-allowlisted type (GIF/PDF) is 415 and an
// over-16-MiB file is 413; in every case the current image is UNCHANGED (a bad
// file never blanks or corrupts the artwork).
func TestArtworkUploadValidation(t *testing.T) {
	requireNamingFixtures(t)
	prov := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("AUTOPOSTER")}),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, namingRoot(t))
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")

	id := titleIDByName(t, srv, token, libID, "Pinned Movie")
	url := "/api/v1/titles/" + id + "/artworkUpload?role=poster"

	// Establish a known good current image (an upload we can prove stays put).
	good := jpegImage("GOODPOSTER")
	if st := uploadArtwork(t, srv, token, url, "image/jpeg", good, nil); st != http.StatusOK {
		t.Fatalf("seed upload = %d, want 200", st)
	}

	// A GIF is rejected 415.
	if st := uploadArtwork(t, srv, token, url, "image/gif", gifImage(), nil); st != http.StatusUnsupportedMediaType {
		t.Errorf("GIF upload = %d, want 415", st)
	}
	// A PDF is rejected 415.
	if st := uploadArtwork(t, srv, token, url, "application/pdf", pdfBytes(), nil); st != http.StatusUnsupportedMediaType {
		t.Errorf("PDF upload = %d, want 415", st)
	}
	// An over-16-MiB file is rejected 413.
	oversize := append(jpegImage("BIG"), make([]byte, 16<<20)...)
	if st := uploadArtwork(t, srv, token, url, "image/jpeg", oversize, nil); st != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize upload = %d, want 413", st)
	}

	// After every rejection the current image is unchanged (still the good upload).
	if st, body, _ := servedArtwork(t, srv, token, "/api/v1/titles/"+id+"/artwork/poster"); st != http.StatusOK || string(body) != string(good) {
		t.Errorf("current image changed by a rejected upload: served %d %q", st, body)
	}
}

// --- Undo via release: deletes the upload, reverts to auto -------------------

// TestArtworkUploadReleaseUndo: releasing the role's Lock deletes the uploaded
// file/row and serve reverts to the auto image (Fetched here); the poster is no
// longer Locked, so a subsequent enrich refreshes it normally.
func TestArtworkUploadReleaseUndo(t *testing.T) {
	requireNamingFixtures(t)
	prov := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("AUTOPOSTER")}),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, namingRoot(t))
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")

	id := titleIDByName(t, srv, token, libID, "Pinned Movie")
	up := jpegImage("UPLOADEDPOSTER")
	if st := uploadArtwork(t, srv, token, "/api/v1/titles/"+id+"/artworkUpload?role=poster", "image/jpeg", up, nil); st != http.StatusOK {
		t.Fatalf("upload = %d, want 200", st)
	}
	if st, body, _ := servedArtwork(t, srv, token, "/api/v1/titles/"+id+"/artwork/poster"); string(body) != string(up) {
		t.Fatalf("precondition: upload not served (%d %q)", st, body)
	}

	// Release the poster Lock → undo the upload.
	var afterRelease labelDetailResp
	if st, body := srv.JSON(http.MethodDelete, "/api/v1/titles/"+id+"/metadata/locks/poster", token, nil, &afterRelease); st != http.StatusOK {
		t.Fatalf("release poster lock = %d; body: %s", st, body)
	}
	if contains(afterRelease.LockedFields, "poster") {
		t.Errorf("poster still Locked after release: %+v", afterRelease.LockedFields)
	}
	// Serve reverts to the auto (fetched) image — the upload bytes are gone.
	st, body, _ := servedArtwork(t, srv, token, "/api/v1/titles/"+id+"/artwork/poster")
	if st != http.StatusOK || string(body) != "AUTOPOSTER" {
		t.Errorf("serve after release = %d %q, want the auto image AUTOPOSTER", st, body)
	}
}

// --- Parent: upload → serve + Lock on a Show --------------------------------

// TestArtworkUploadParent: the upload affordance is uniform across kinds — an
// Admin uploads a Show poster and the exact bytes are served + Locked, exactly as
// for a leaf.
func TestArtworkUploadParent(t *testing.T) {
	requireTVFixtures(t)
	prov := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) {
		return enrich.TitleMetadata{Matched: true, Source: "tmdb", ExternalID: "show-x"}, nil
	}}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("SHOWAUTO")}),
	)
	token := adminToken(t, srv)
	libID := createTVLibrary(t, srv, token, tvRoot(t))
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")

	shows := listShows(t, srv, token, libID)
	if len(shows.Shows) == 0 {
		t.Skip("no shows in tv fixture")
	}
	showID := shows.Shows[0].ID

	up := jpegImage("SHOWUPLOAD")
	var afterUpload entityDetailResp
	if st := uploadArtwork(t, srv, token, "/api/v1/shows/"+showID+"/artworkUpload?role=poster", "image/jpeg", up, &afterUpload); st != http.StatusOK {
		t.Fatalf("upload show poster = %d, want 200", st)
	}
	if !contains(afterUpload.LockedFields, "poster") {
		t.Errorf("show poster not Locked after upload: %+v", afterUpload.LockedFields)
	}
	if st, body, _ := servedArtwork(t, srv, token, "/api/v1/shows/"+showID+"/artwork/poster"); st != http.StatusOK || string(body) != string(up) {
		t.Errorf("served show poster = %d %q, want the uploaded bytes", st, body)
	}
}

// --- Access + realtime ------------------------------------------------------

// TestArtworkUploadAdminOnlyAndSSE: a Member cannot upload (403); an Admin upload
// emits a libraryUpdated SSE nudge so other signed-in clients see the new art.
func TestArtworkUploadAdminOnlyAndSSE(t *testing.T) {
	requireFixtures(t)
	prov := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("AUTOPOSTER")}),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")
	id := titleIDByName(t, srv, token, libID, "Dune")
	url := "/api/v1/titles/" + id + "/artworkUpload?role=poster"

	srv.CreateMember("uploadmember", "memberpass123")
	mTok := srv.LoginAs("uploadmember", "memberpass123")

	if st := uploadArtwork(t, srv, mTok, url, "image/jpeg", jpegImage("m"), nil); st != http.StatusForbidden {
		t.Errorf("member upload = %d, want 403", st)
	}

	// SSE: an Admin upload emits libraryUpdated.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lines := openEventStream(t, ctx, srv, token)
	if st := uploadArtwork(t, srv, token, url, "image/jpeg", jpegImage("SSE"), nil); st != http.StatusOK {
		t.Fatalf("admin upload = %d, want 200", st)
	}
	waitForLine(t, lines, func(s string) bool {
		return strings.Contains(s, "event: libraryUpdated") || strings.Contains(s, `"libraryId":"`+libID+`"`)
	})
}
