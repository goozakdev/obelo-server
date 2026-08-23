package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/goozakdev/obelo-server/internal/safefetch"
)

// The metadata-provider image proxy: GET /providerImage?ref=<signed reference>.
//
// # Why this exists
//
// The admin Edit-item pickers (record search + Fix-label image grid) show
// candidate images that live on a metadata provider's own host — image.tmdb.org,
// the Cover Art Archive (which redirects into archive.org), assets.fanart.tv,
// theaudiodb. Pointing an <img src> straight at those hosts makes the ADMIN'S
// BROWSER contact them: it hands a third party the viewer's IP address, and the
// Referer would have handed it this server's (usually home-DDNS) hostname.
//
// ADR-0001 says this product depends on no third-party service the operator does
// not control. Server-side enrichment is the deliberate, documented exception —
// ONE process, on the operator's own box, making those calls. A browser quietly
// phoning out on every Fix Match is not covered by that exception, and nothing in
// the UI told the operator it was happening. So the bytes come through here
// instead, the pickers stay same-origin, and the CSP can say img-src 'self'
// (internal/webui/webui.go) rather than trusting the whole https: scheme.
//
// # The part that must not be "simplified"
//
// This handler takes a URL-ish parameter. The obvious tidy-up — accept the plain
// provider URL in a query parameter and fetch it — turns this server into an OPEN
// PROXY and a first-class SSRF gadget: any authenticated admin (or anything that
// gets one request through their browser) could make the server GET
// http://127.0.0.1:PORT/…, a cloud metadata endpoint, or a LAN admin panel, and
// read the bytes back. DO NOT DO THAT.
//
// The rule here is: the server only ever fetches a URL IT MINTED ITSELF. The
// candidate emit path signs the absolute provider URL with a per-boot HMAC key
// (proxyURL); this handler verifies that signature — constant-time — before it
// opens a socket. A reference this process did not sign is refused, so the set of
// fetchable URLs is exactly the set the providers handed us, and a caller cannot
// author one.
//
// Two designs were considered. The other was an opaque provider+path reference
// (?provider=tmdb&path=/abc.jpg) rebuilt against the CONFIGURED base URL, which
// removes SSRF by construction. It was rejected because it does not fit the data:
// the artwork-candidate URLs (enrich.ArtworkCandidate.URL, emitted by anidb.go,
// fanarttv.go, musicbrainz.go, theaudiodb.go and tmdb.go) are ABSOLUTE URLs that
// come out of the providers' own JSON payloads on hosts unrelated to any base URL
// we configured — there is no path to decompose. Signing is uniform across all
// three emit sites and needs no per-provider special-casing.
//
// # Per-boot key, and why that is fine
//
// The key is generated at boot and lives only in memory, like the claim token
// (ADR-0013) and the abuse counters (auth/fixed_window_limiter.go), and for the
// same reason: it is safety state, not a record of anything. The consequence is
// that references do not survive a restart — a picker left open across one gets
// broken thumbnails until it refetches its candidate list. That costs nothing,
// because the candidate lists themselves are already per-process and short-lived
// (enrich/candidate_cache.go holds them for DefaultCandidateCacheTTL = 2 minutes,
// in memory), so a restart re-queries the provider regardless.

const (
	// providerImagePath is the proxy route, under the API prefix. The reference
	// travels in ?ref= rather than the path because it is opaque base64url with a
	// separator, and a path segment would invite someone to "clean it up" with
	// path.Clean.
	providerImagePath = "/providerImage"

	// providerImageRefSep separates the encoded URL from its MAC in a reference.
	// "." is not produced by base64url encoding, so the split is unambiguous.
	providerImageRefSep = "."

	// maxProviderImageBytes caps a proxied image. The SAME 16 MiB ceiling artwork
	// uploads and the server-side artwork fetcher use (ADR-0026), deliberately: a
	// picker thumbnail can be the full-size poster the grid scales down, so a
	// tighter cap here would refuse images the pick path happily downloads.
	maxProviderImageBytes = maxArtworkUploadBytes

	// providerImageTimeout bounds the whole upstream request. A provider that
	// hangs must yield a broken thumbnail promptly, never a held connection: the
	// image grid opens a dozen of these at once.
	providerImageTimeout = 15 * time.Second

	// providerImageCacheSeconds lets the browser reuse a thumbnail while the
	// picker is open (private: it is behind auth and admin-only). Short, because
	// the reference dies with the process anyway.
	providerImageCacheSeconds = 300
)

// providerImageProxy mints and verifies the signed references, and owns the HTTP
// client that fetches them. One instance per process, built by Handler.
type providerImageProxy struct {
	key    []byte
	client *http.Client
}

// newProviderImageProxy builds the proxy with a fresh per-boot signing key.
//
// The client comes from safefetch, which owns the redirect policy this proxy used
// to carry alone: bounded hops, and a hop that resolves inward refused. The
// signature only vouches for the FIRST url — where that host chooses to redirect is
// the host's decision, not ours — and the same shape turned up in the artwork
// fetcher and the subtitle download, so the rule moved somewhere all three could
// share ONE copy of it. safefetch.CheckRedirect's doc comment carries the reasoning,
// including why the initial request is deliberately not checked.
//
// A crypto/rand failure panics rather than degrading to a weak or empty key: it
// happens at boot, before the listener is up, and the alternative — a server that
// silently signs with a guessable key — is precisely the open proxy this file
// exists to prevent.
func newProviderImageProxy() *providerImageProxy {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic("api: generating provider-image signing key: " + err.Error())
	}
	return &providerImageProxy{
		key:    key,
		client: safefetch.Client(providerImageTimeout),
	}
}

// proxyURL turns an absolute provider image URL into the same-origin proxy URL
// the web UI renders. It is the ONLY way a URL becomes fetchable by the handler.
//
// A blank result means "no thumbnail" and the picker simply shows none. That is
// the deliberate failure direction for every case it can't sign — a nil proxy (a
// handler wired outside Handler), a relative or non-http(s) URL — because falling
// back to the raw provider URL would put the browser back in touch with the third
// party, which is the whole thing this removes, and would do it invisibly.
func (p *providerImageProxy) proxyURL(raw string) string {
	if p == nil {
		return ""
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	enc := base64.RawURLEncoding.EncodeToString([]byte(raw))
	ref := enc + providerImageRefSep + base64.RawURLEncoding.EncodeToString(p.mac(enc))
	return APIPrefix + providerImagePath + "?ref=" + url.QueryEscape(ref)
}

// resolveRef verifies a reference's signature and returns the URL it names. A
// reference this process did not sign — forged, tampered, truncated, or minted
// before a restart — yields ("", false) and the handler fetches nothing.
func (p *providerImageProxy) resolveRef(ref string) (string, bool) {
	if p == nil || ref == "" {
		return "", false
	}
	enc, sig, ok := strings.Cut(ref, providerImageRefSep)
	if !ok || enc == "" || sig == "" {
		return "", false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return "", false
	}
	// Constant time: a byte-at-a-time comparison here would leak the expected MAC
	// to a caller willing to time a few thousand requests, and the whole guarantee
	// of this file is that a caller cannot author a reference.
	if !hmac.Equal(got, p.mac(enc)) {
		return "", false
	}
	rawURL, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return "", false
	}
	return string(rawURL), true
}

// mac is the HMAC-SHA256 tag over the ENCODED url. Signing the encoded form (not
// the decoded one) means the bytes that were verified are exactly the bytes that
// get decoded — no room for two encodings of one URL to disagree.
func (p *providerImageProxy) mac(encodedURL string) []byte {
	m := hmac.New(sha256.New, p.key)
	m.Write([]byte(encodedURL))
	return m.Sum(nil)
}

// handleProviderImage serves a metadata provider's candidate image through this
// server so the admin pickers never point a browser at a third-party host
// (ADR-0001). GET /providerImage?ref=<signed reference>, Admin-only.
//
// Auth is bearer OR the media cookie (requireAuthAllowCookie) because this is a
// browser <img src>, which cannot set an Authorization header — the same reason
// the artwork and stream GETs accept it. Wiring it bearer-only would leave every
// Go test green and every thumbnail broken. requireAdmin layers on top: the Edit
// -item pickers are an Admin surface and this is part of them.
//
// EVERY failure is one 404 with an empty-ish envelope: an unsigned reference, a
// dead provider, a redirect we refused, a non-image body, an oversized body. The
// caller is an <img> that can only render or not render, so a status taxonomy
// buys nobody anything — and one answer keeps this from becoming an oracle that
// reports what some other host did when we asked it.
func handleProviderImage(p *providerImageProxy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		target, ok := p.resolveRef(r.URL.Query().Get("ref"))
		if !ok {
			// Note the ordering: nothing has been fetched. A caller cannot use this
			// endpoint to make the server touch a URL of their choosing.
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		data, contentType, err := p.fetch(r, target)
		if err != nil {
			writeError(w, http.StatusNotFound, codeNotFound, "provider image unavailable", nil)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Header().Set("Cache-Control", "private, max-age="+strconv.Itoa(providerImageCacheSeconds))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}
}

// fetch GETs a verified provider URL and returns the validated image bytes and
// their SNIFFED content type.
//
// Sniffed, never the provider's declared header: a Content-Type is a claim by a
// host we do not control, and this response is re-served from OUR origin, where a
// mislabelled text/html body would be a stored-XSS-shaped problem. allowedImageType
// (the ADR-0026 JPEG/PNG/WebP allowlist that guards artwork uploads) decides, so
// the picker can only ever render what the pick path would accept anyway.
//
// The body is read through a LimitReader at cap+1 — the same shape as
// enrich/fetcher.go — so an endless response is bounded by memory we chose rather
// than by whatever the far end feels like sending.
func (p *providerImageProxy) fetch(r *http.Request, target string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", errors.New("api: provider image status " + strconv.Itoa(resp.StatusCode))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxProviderImageBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > maxProviderImageBytes {
		return nil, "", errors.New("api: provider image exceeds the size cap")
	}
	contentType := http.DetectContentType(data)
	if !allowedImageType(contentType) {
		return nil, "", errors.New("api: provider image is not an allowed image type")
	}
	return data, contentType, nil
}
