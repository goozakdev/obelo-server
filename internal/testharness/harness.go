// Package testharness boots the full server over net/http/httptest against a
// temp SQLite database and a temp data root, for black-box HTTP API tests.
//
// This is the single testing seam for the whole project (PRD: assert external
// behavior through the HTTP API, never internal module shapes). Every later
// slice reuses this: arrange fixtures + temp data dir, drive the server with
// HTTP requests, assert on responses and observable state.
//
// Conventions:
//   - One Server per test (or subtest), torn down via t.Cleanup.
//   - Temp data root via t.TempDir(); the SQLite DB lives inside it, exactly as
//     in production (app.New owns that wiring).
//   - Checked-in fixtures live under each package's testdata/ tree; helpers
//     here resolve paths into it.
package testharness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/marioquake/juicebox/internal/app"
	"github.com/marioquake/juicebox/internal/auth"
	"github.com/marioquake/juicebox/internal/config"
	"github.com/marioquake/juicebox/internal/enrich"
	"github.com/marioquake/juicebox/internal/gpu"
	"github.com/marioquake/juicebox/internal/subfetch"
	"github.com/marioquake/juicebox/internal/transcode"
)

// Server is a running test server plus the handles a test needs to drive and
// inspect it.
type Server struct {
	t       *testing.T
	app     *app.App
	http    *httptest.Server
	DataDir string
}

// Option customizes how a Server boots. Most options tweak the config; the
// Enrichment seam options inject fakes into the app wiring so the black-box
// tests drive enrichment with zero network (mirroring how the scanner fakes the
// Prober).
type Option func(*builder)

// builder accumulates the boot config plus any app-level wiring overrides.
type builder struct {
	cfg     config.Config
	appOpts []app.Option
}

// WithDataDir overrides the data directory (default: a fresh t.TempDir()).
func WithDataDir(dir string) Option {
	return func(b *builder) { b.cfg.DataDir = dir }
}

// WithScanInterval sets the scheduled-scan cadence (0 disables it). Tests that
// want to observe the periodic scan firing set a short interval; the default
// harness disables it so unrelated tests are deterministic and quiet.
func WithScanInterval(d time.Duration) Option {
	return func(b *builder) { b.cfg.ScanInterval = d }
}

// WithSessionIdleTimeout sets the Playback-session reaper's idle window (0
// disables the reaper). The default harness disables it so unrelated tests stay
// deterministic; the reaping test opts in with a short value to watch a silent
// session get ended.
func WithSessionIdleTimeout(d time.Duration) Option {
	return func(b *builder) { b.cfg.SessionIdleTimeout = d }
}

// WithTranscodeCap sets the global concurrent-transcode cap (ADR-0009
// governance; 0 = unlimited). The governance tests set it to 1 to drive the
// SERVER_BUSY path — start one transcode, then assert a second transcode-
// requiring request is rejected while direct play / remux still succeed.
func WithTranscodeCap(n int) Option {
	return func(b *builder) { b.cfg.MaxConcurrentTranscodes = n }
}

// WithHardwareAccel sets the hardware-acceleration knob (ADR-0009; off by default
// → the CPU libx264 path) to a specific backend. VideoToolbox drives a real GPU
// encoder (the gated macOS e2e test uses this); the other named backends and auto
// still resolve to CPU in the args builder for now.
func WithHardwareAccel(a config.HWAccel) Option {
	return func(b *builder) { b.cfg.HardwareAccel = a }
}

// WithBackendResolution pins the setup-time hardware-accel Resolution (ADR-0009)
// so the admin /transcoding surface's backend projection — active/requested/
// reason/degraded — can be asserted deterministically on a GPU-less box. It
// injects a transcode.StaticDetector, bypassing the real ffmpeg-probing detector
// (which only ever resolves to CPU without hardware, so it cannot exercise the
// degraded or hardware-active states). Unlike WithHardwareAccel (which sets the
// requested config knob and runs the real detector), this fixes the whole outcome.
func WithBackendResolution(res transcode.Resolution) Option {
	return func(b *builder) {
		b.appOpts = append(b.appOpts, app.WithDetector(transcode.StaticDetector{Resolution: res}))
	}
}

// WithGPUProbe injects the best-effort GPU-telemetry probe (ADR-0029) so the
// /transcoding gpu block can be driven across every availability state — populated
// telemetry, unavailable, and (via a non-NVENC backend) never-queried — without a
// real GPU, mirroring the encode-probe discipline of detect_test.go.
func WithGPUProbe(p gpu.Probe) Option {
	return func(b *builder) {
		b.appOpts = append(b.appOpts, app.WithGPUProbe(p))
	}
}

// WithEnrichmentKey sets the TMDB API key so Enrichment is ENABLED (otherwise it
// is the disabled no-op). The value is opaque to the fake provider; it only flips
// config.EnrichmentEnabled. Pair with WithMetadataProvider to drive a real pass.
func WithEnrichmentKey(key string) Option {
	return func(b *builder) { b.cfg.TMDBAPIKey = key }
}

// WithEnrichmentConsent seeds the first-run Enrichment consent decision (ADR-0032)
// on the harness's fresh DB: true grants (the harness default), false declines,
// and passing the undecided state is done by seeding declined then driving the API
// — a fresh install with NO decision is modeled by WithoutEnrichmentConsent. Use
// this to prove the gate: a declined server makes zero outbound provider calls even
// with a provider configured.
func WithEnrichmentConsent(granted bool) Option {
	return func(b *builder) { b.cfg.EnrichmentConsentGranted = &granted }
}

// WithoutEnrichmentConsent leaves the first-run consent UNDECIDED on the fresh DB
// (ADR-0032), modeling a brand-new install that has not yet answered the prompt —
// the state under which the server must make no outbound enrichment calls and the
// SPA shows the consent prompt.
func WithoutEnrichmentConsent() Option {
	return func(b *builder) { b.cfg.EnrichmentConsentGranted = nil }
}

// WithMusicBrainzEnabled turns ON Music enrichment without a TMDB key (MusicBrainz
// + Cover Art Archive need none). Video kinds stay disabled unless WithEnrichmentKey
// is also set. Pair with WithMetadataProvider to drive a Music pass with no network.
func WithMusicBrainzEnabled(on bool) Option {
	return func(b *builder) { b.cfg.MusicBrainzEnabled = on }
}

// WithAutoEnrich enables (or disables) the auto-after-scan background Enrichment
// trigger. The default harness disables it so the manual-enrich tests stay
// deterministic (a scan won't race a background pass); the auto-enrich test opts
// in. Pair with WithEnrichmentKey + WithMetadataProvider to drive a real pass.
func WithAutoEnrich(on bool) Option {
	return func(b *builder) { b.cfg.AutoEnrichAfterScan = on }
}

// WithEnrichInterval sets the scheduled-enrich cadence (0 disables it). The
// default harness disables it; the scheduled-enrich test sets a short value to
// watch the sweep fire.
func WithEnrichInterval(d time.Duration) Option {
	return func(b *builder) { b.cfg.EnrichInterval = d }
}

// WithArtworkCandidateCacheTTL sets the artwork-picker candidate cache lifetime
// (0 disables the cache with no behavior change). The default harness leaves the
// production default; the candidate-cache tests set a tiny TTL to observe expiry
// re-query, or 0 to assert the disabled path re-queries every open.
func WithArtworkCandidateCacheTTL(d time.Duration) Option {
	return func(b *builder) { b.cfg.ArtworkCandidateCacheTTL = d }
}

// WithMetadataProvider injects a fake Enrichment MetadataProvider (no network).
func WithMetadataProvider(p enrich.MetadataProvider) Option {
	return func(b *builder) { b.appOpts = append(b.appOpts, app.WithMetadataProvider(p)) }
}

// WithKeyRotation points the key-rotation channel (ADR-0032, layer 2) at a stub
// endpoint with a known decryption key, and sets the re-poll interval (0 = fetch on
// startup / on demand only, no periodic timer — the deterministic choice for a
// test, which then drives polls via RefreshRotationKeys). It enables the channel
// regardless of the default disable state, so a black-box test can drive the full
// fetch→decrypt→cache→propagate loop — including a rotated key adopted on the next
// poll — with no deployed Worker. Pair with WithProviderBuilder to observe the
// rotated key reach the rebuilt provider with zero network.
func WithKeyRotation(url, encKeyB64 string, interval time.Duration) Option {
	return func(b *builder) {
		b.cfg.KeyRotationEnabled = true
		b.cfg.KeyRotationInterval = interval
		b.appOpts = append(b.appOpts, app.WithKeyRotation(url, encKeyB64))
	}
}

// WithArtworkFetcher injects a fake Enrichment ArtworkFetcher (no network).
func WithArtworkFetcher(f enrich.ArtworkFetcher) Option {
	return func(b *builder) { b.appOpts = append(b.appOpts, app.WithArtworkFetcher(f)) }
}

// WithProviderBuilder substitutes the function the provider Manager uses to
// compose a provider + enablement from the DB settings (default:
// enrich.BuildProvider). Unlike WithMetadataProvider (a pinned fixed provider),
// this keeps the manager active, so a settings PUT rebuilds + hot-swaps via the
// fake builder — letting a black-box test drive the write→rebuild→enrich loop
// with ZERO network by mapping settings → fake sub-providers.
func WithProviderBuilder(build enrich.BuildFunc) Option {
	return func(b *builder) { b.appOpts = append(b.appOpts, app.WithProviderBuilder(build)) }
}

// WithSubtitleProviderBuilder substitutes how the subtitle-fetch Manager composes a
// SubtitleProvider from the DB settings (default: subfetch.BuildProvider). It keeps
// the manager active, so a subtitle-provider settings PUT rebuilds + hot-swaps via
// the fake builder — letting a black-box test drive the "search online → pick →
// track appears" flow with ZERO network (ADR-0021).
func WithSubtitleProviderBuilder(build subfetch.BuildFunc) Option {
	return func(b *builder) { b.appOpts = append(b.appOpts, app.WithSubtitleProviderBuilder(build)) }
}

// New boots a server against a temp data root and returns it ready to serve.
// The httptest server and database are torn down automatically via t.Cleanup.
func New(t *testing.T, opts ...Option) *Server {
	t.Helper()

	b := &builder{cfg: config.Defaults()}
	b.cfg.DataDir = t.TempDir()
	b.cfg.ListenAddr = ""             // unused: httptest owns the listener
	b.cfg.ScanInterval = 0            // scheduled scan off by default; opt in with WithScanInterval
	b.cfg.SessionIdleTimeout = 0      // session reaper off by default; opt in with WithSessionIdleTimeout
	b.cfg.AutoEnrichAfterScan = false // auto-after-scan enrich off by default; opt in with WithAutoEnrich
	b.cfg.EnrichInterval = 0          // scheduled enrich off by default; opt in with WithEnrichInterval
	// First-run Enrichment consent (ADR-0032) is GRANTED by default so a harness
	// represents an operator who has already opted in — existing enrichment tests
	// enrich as before. A consent-gate test overrides this with WithEnrichmentConsent.
	granted := true
	b.cfg.EnrichmentConsentGranted = &granted
	for _, o := range opts {
		o(b)
	}

	application, err := app.New(b.cfg, b.appOpts...)
	if err != nil {
		t.Fatalf("testharness: booting app: %v", err)
	}

	ts := httptest.NewServer(application.Handler)

	s := &Server{t: t, app: application, http: ts, DataDir: b.cfg.DataDir}
	t.Cleanup(func() {
		ts.Close()
		_ = application.Close()
	})
	return s
}

// URL returns the absolute URL for an API path. Pass a path under the version
// prefix, e.g. "/api/v1/server".
func (s *Server) URL(path string) string {
	return s.http.URL + path
}

// Handler returns the fully wired root http.Handler the server serves. Tests
// that need to re-serve the same composition over a different transport — e.g.
// the media-cookie Secure-flag test re-serving over an httptest TLS server to
// exercise the HTTPS path (ADR-0005) — use this instead of reaching into the
// app's internals.
func (s *Server) Handler() http.Handler {
	return s.app.Handler
}

// GET issues a GET and returns the decoded status, raw body, and the body
// decoded into out (if non-nil). It fails the test on transport errors.
func (s *Server) GET(path string, out any) (status int, body []byte) {
	s.t.Helper()
	resp, err := http.Get(s.URL(path))
	if err != nil {
		s.t.Fatalf("testharness: GET %s: %v", path, err)
	}
	defer resp.Body.Close()

	body, err = io.ReadAll(resp.Body)
	if err != nil {
		s.t.Fatalf("testharness: reading body of GET %s: %v", path, err)
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			s.t.Fatalf("testharness: decoding body of GET %s: %v\nbody: %s", path, err, body)
		}
	}
	return resp.StatusCode, body
}

// Do issues an arbitrary request against the server and returns the response.
// The caller owns closing resp.Body. Useful for non-GET methods later slices add.
func (s *Server) Do(method, path string, body io.Reader) *http.Response {
	s.t.Helper()
	return s.do(method, path, body, "")
}

// do is the shared request path. token, when non-empty, is sent as a bearer
// credential. Keeping it private lets the public helpers stay intention-named.
func (s *Server) do(method, path string, body io.Reader, token string) *http.Response {
	s.t.Helper()
	req, err := http.NewRequest(method, s.URL(path), body)
	if err != nil {
		s.t.Fatalf("testharness: building %s %s: %v", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("testharness: %s %s: %v", method, path, err)
	}
	return resp
}

// Multipart issues a multipart/form-data POST carrying a single file part (the
// artwork upload path, ADR-0026), with an optional bearer token. fieldName is the
// form field (the upload handler reads "image"); filename + contentType label the
// part; content is the raw bytes. It returns the status and raw response body,
// decoding into out when non-nil. Unlike JSON it sets the multipart Content-Type
// with the generated boundary rather than application/json.
func (s *Server) Multipart(method, path, token, fieldName, filename, contentType string, content []byte, out any) (status int, body []byte) {
	s.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	hdr := make(textproto.MIMEHeader)
	hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, fieldName, filename))
	if contentType != "" {
		hdr.Set("Content-Type", contentType)
	}
	part, err := mw.CreatePart(hdr)
	if err != nil {
		s.t.Fatalf("testharness: building multipart part: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		s.t.Fatalf("testharness: writing multipart part: %v", err)
	}
	if err := mw.Close(); err != nil {
		s.t.Fatalf("testharness: closing multipart writer: %v", err)
	}

	req, err := http.NewRequest(method, s.URL(path), &buf)
	if err != nil {
		s.t.Fatalf("testharness: building %s %s: %v", method, path, err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("testharness: %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	body, err = io.ReadAll(resp.Body)
	if err != nil {
		s.t.Fatalf("testharness: reading body of %s %s: %v", method, path, err)
	}
	if out != nil && len(body) > 0 {
		if err := json.Unmarshal(body, out); err != nil {
			s.t.Fatalf("testharness: decoding body of %s %s: %v\nbody: %s", method, path, err, body)
		}
	}
	return resp.StatusCode, body
}

// ClaimToken returns the one-time first-Admin claim token the booting server
// generated (ADR-0013), or "" if setup is already closed. Tests use it to drive
// POST /setup without scraping logs.
func (s *Server) ClaimToken() string {
	return s.app.Auth.ClaimToken()
}

// JSON issues a request with an optional JSON body (marshaled from in) and an
// optional bearer token, decoding the response body into out (if non-nil). It
// returns the status code and raw body. This is the workhorse for the auth
// endpoints (setup/login/logout/devices) which are POST/DELETE with JSON.
func (s *Server) JSON(method, path, token string, in, out any) (status int, body []byte) {
	s.t.Helper()
	var rdr io.Reader
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			s.t.Fatalf("testharness: marshaling body for %s %s: %v", method, path, err)
		}
		rdr = bytes.NewReader(buf)
	}
	resp := s.do(method, path, rdr, token)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		s.t.Fatalf("testharness: reading body of %s %s: %v", method, path, err)
	}
	if out != nil && len(body) > 0 {
		if err := json.Unmarshal(body, out); err != nil {
			s.t.Fatalf("testharness: decoding body of %s %s: %v\nbody: %s", method, path, err, body)
		}
	}
	return resp.StatusCode, body
}

// AuthGET issues an authenticated GET (bearer token) and decodes into out.
func (s *Server) AuthGET(path, token string, out any) (status int, body []byte) {
	s.t.Helper()
	return s.JSON(http.MethodGet, path, token, nil, out)
}

// CreateUser creates a User through the real Admin API (POST /users) with the
// given Admin bearer token, returning the new User's id. role may be "" to
// default to a Member. This is the first-class way tests build Users now that
// the management API exists; the direct-insert CreateMember below remains for
// the rare pre-API baseline (e.g. seeding a Member before any Admin exists).
func (s *Server) CreateUser(adminToken, username, password, role string) string {
	s.t.Helper()
	body := map[string]any{"username": username, "password": password}
	if role != "" {
		body["role"] = role
	}
	var out struct {
		ID string `json:"id"`
	}
	status, raw := s.JSON(http.MethodPost, "/api/v1/users", adminToken, body, &out)
	if status != http.StatusCreated {
		s.t.Fatalf("testharness: creating user %q: status %d; body: %s", username, status, raw)
	}
	return out.ID
}

// LoginAs logs in with the given credentials using default Device descriptors
// and returns a usable bearer token, asserting a clean 200. It is the companion
// to CreateUser for building a restricted caller in a test.
func (s *Server) LoginAs(username, password string) string {
	s.t.Helper()
	var out struct {
		Token string `json:"token"`
	}
	status, raw := s.JSON(http.MethodPost, "/api/v1/auth/login", "", map[string]any{
		"username": username,
		"password": password,
		"device": map[string]any{
			"name":     username + "-device",
			"platform": "test",
			"clientId": username + "-client",
		},
	}, &out)
	if status != http.StatusOK {
		s.t.Fatalf("testharness: logging in %q: status %d; body: %s", username, status, raw)
	}
	return out.Token
}

// SetTitleContentRating sets a Title's Content rating directly — the value
// Enrichment would otherwise supply — so a Rating-ceiling test can establish
// fixture ratings without driving a fake metadata provider. A direct seam (like
// CreateMember) for fixture state the public API doesn't author.
func (s *Server) SetTitleContentRating(titleID, rating string) {
	s.t.Helper()
	if _, err := s.app.DB.Exec(
		`UPDATE titles SET content_rating = ? WHERE id = ?`, rating, titleID,
	); err != nil {
		s.t.Fatalf("testharness: setting content rating on title %q: %v", titleID, err)
	}
}

// SetTitleHidden flips a Title's derived hidden flag directly — the soft-delete
// state the scanner sets when every File of a Title goes Missing (ADR-0008). A
// direct seam (like SetTitleContentRating) so a Collection/Playlist test can drive
// the "Missing member is omitted from the resolved view but its membership
// persists" behavior deterministically, without mutating a library on disk and
// rescanning. Pass hidden=false to restore the Title (Files returned) and assert
// it reappears in the grouping with no re-add.
func (s *Server) SetTitleHidden(titleID string, hidden bool) {
	s.t.Helper()
	v := 0
	if hidden {
		v = 1
	}
	if _, err := s.app.DB.Exec(
		`UPDATE titles SET hidden = ? WHERE id = ?`, v, titleID,
	); err != nil {
		s.t.Fatalf("testharness: setting hidden on title %q: %v", titleID, err)
	}
}

// SetShowContentRating sets a Show's ENRICHED Content rating (stored in
// entity_enrichment, not on the shows row) — the parent-entity equivalent of
// SetTitleContentRating, for cross-system ceiling tests.
func (s *Server) SetShowContentRating(showID, rating string) {
	s.t.Helper()
	if _, err := s.app.DB.Exec(
		`INSERT INTO entity_enrichment (entity_type, entity_id, content_rating, enrichment_status)
		 VALUES ('show', ?, ?, 'matched')
		 ON CONFLICT (entity_type, entity_id) DO UPDATE SET content_rating = excluded.content_rating`,
		showID, rating,
	); err != nil {
		s.t.Fatalf("testharness: setting content rating on show %q: %v", showID, err)
	}
}

// CountPlaylistRowsForOwner returns the raw number of playlists and playlist_items
// rows owned (directly, or via their parent Playlist) by ownerUserID. It is a
// direct-DB seam (like SetTitleHidden) so a cascade test can assert that deleting a
// User removed their Playlists AND their items — the observable owner-private API
// cannot prove the rows are gone (a non-owner just 404s either way).
func (s *Server) CountPlaylistRowsForOwner(ownerUserID string) (playlists, items int) {
	s.t.Helper()
	if err := s.app.DB.QueryRow(
		`SELECT COUNT(*) FROM playlists WHERE owner_user_id = ?`, ownerUserID,
	).Scan(&playlists); err != nil {
		s.t.Fatalf("testharness: counting playlists for %q: %v", ownerUserID, err)
	}
	if err := s.app.DB.QueryRow(
		`SELECT COUNT(*) FROM playlist_items pi
		   JOIN playlists p ON p.id = pi.playlist_id
		  WHERE p.owner_user_id = ?`, ownerUserID,
	).Scan(&items); err != nil {
		s.t.Fatalf("testharness: counting playlist items for %q: %v", ownerUserID, err)
	}
	return playlists, items
}

// CreateMember inserts a non-Admin (role "member") User directly into the
// database with the given credentials. Prefer CreateUser (which drives the real
// admin API); this direct-insert seam remains for the pre-API baseline — seeding
// a Member before any Admin exists, where there is no Admin token to call the
// API with.
func (s *Server) CreateMember(username, password string) {
	s.t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		s.t.Fatalf("testharness: hashing member password: %v", err)
	}
	if _, err := s.app.DB.Exec(
		`INSERT INTO users (id, username, role, password_hash) VALUES (?, ?, 'member', ?)`,
		uuid.NewString(), username, hash,
	); err != nil {
		s.t.Fatalf("testharness: inserting member %q: %v", username, err)
	}
}

// RefreshRotationKeys forces one synchronous key-rotation poll (ADR-0032): fetch
// the stub endpoint, decrypt, cache, propagate the default key into the running
// provider, and rebuild it. It is the deterministic driver for the rotation
// black-box test — a poll on demand instead of waiting on the interval timer — and
// fails the test on error. Pair with WithKeyRotation.
func (s *Server) RefreshRotationKeys() {
	s.t.Helper()
	if err := s.app.RefreshRotationKeysNow(context.Background()); err != nil {
		s.t.Fatalf("testharness: refreshing rotation keys: %v", err)
	}
}

// TestdataPath joins parts onto the calling package's testdata directory.
func TestdataPath(parts ...string) string {
	return filepath.Join(append([]string{"testdata"}, parts...)...)
}

// MutableLibraryDir copies the tree rooted at src into a fresh t.TempDir() and
// returns the absolute copy path. Issue-06 tests MUTATE the library between
// scans (add/remove/rename files); they must never touch the checked-in fixture
// trees, so they point the Library at this throwaway copy. Empty files are
// padded to clear the scanner's sample-size floor so a copied real fixture stays
// recognized media.
func MutableLibraryDir(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	if err := copyTree(src, dst); err != nil {
		t.Fatalf("testharness: copying library tree %q: %v", src, err)
	}
	abs, err := filepath.Abs(dst)
	if err != nil {
		t.Fatalf("testharness: resolving mutable dir: %v", err)
	}
	return abs
}

// copyTree recursively copies src into dst (dst already exists).
func copyTree(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := os.MkdirAll(d, 0o755); err != nil {
				return err
			}
			if err := copyTree(s, d); err != nil {
				return err
			}
			continue
		}
		b, err := os.ReadFile(s)
		if err != nil {
			return err
		}
		if err := os.WriteFile(d, b, 0o644); err != nil {
			return err
		}
	}
	return nil
}
