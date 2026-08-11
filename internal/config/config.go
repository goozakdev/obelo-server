// Package config loads and validates server configuration.
//
// Configuration is intentionally small for the walking skeleton: the server
// needs to know which address to listen on and where its writable data
// directory lives. The data directory holds the single SQLite database and
// (in later slices) the artwork/transcode caches, per ADR-0007.
package config

import (
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// HWAccel is the hardware-acceleration knob's vocabulary (ADR-0009): off (CPU),
// auto (detect the best backend), or an explicit backend. The string values are
// the operator-facing names parsed from OBELO_HARDWARE_ACCEL and match the
// transcode tier's Accel values 1:1 (the app wires one to the other), so a backend
// added there needs only a constant here. HWAccelOff is the zero-ish default.
type HWAccel string

const (
	// HWAccelOff is the default — the always-available CPU libx264 path.
	HWAccelOff HWAccel = "off"
	// HWAccelAuto asks the server to detect and use the best available backend.
	// INTERIM: detection is a later slice, so this resolves to CPU for now.
	HWAccelAuto HWAccel = "auto"
	// HWAccelNVENC forces NVIDIA NVENC. RESERVED: resolves to CPU until wired.
	HWAccelNVENC HWAccel = "nvenc"
	// HWAccelVAAPI forces VAAPI (Intel/AMD on Linux). RESERVED: CPU until wired.
	HWAccelVAAPI HWAccel = "vaapi"
	// HWAccelQSV forces Intel Quick Sync. RESERVED: CPU until wired.
	HWAccelQSV HWAccel = "qsv"
	// HWAccelVideoToolbox forces Apple VideoToolbox (macOS) — the one backend wired
	// to a real encoder today.
	HWAccelVideoToolbox HWAccel = "videotoolbox"
)

// parseHWAccel maps a OBELO_HARDWARE_ACCEL value to a HWAccel, reporting
// whether it was recognized. It is lenient and back-compatible (mirrors the
// "garbage stays off" policy of the other knobs): the explicit names parse to
// themselves; off/false/0/no/"" → off; and the legacy bool-true spellings
// (true/1/on/yes) → auto, preserving the old "true turns HW on" behavior (auto
// itself resolves to CPU until the detector lands). An unrecognized value is not
// recognized (false), so the caller keeps the safe default.
func parseHWAccel(v string) (HWAccel, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "off", "false", "0", "no":
		return HWAccelOff, true
	case "auto", "true", "1", "on", "yes":
		return HWAccelAuto, true
	case "nvenc":
		return HWAccelNVENC, true
	case "vaapi":
		return HWAccelVAAPI, true
	case "qsv":
		return HWAccelQSV, true
	case "videotoolbox":
		return HWAccelVideoToolbox, true
	default:
		return HWAccelOff, false
	}
}

// TLSMode is the native-TLS knob's vocabulary (ADR-0041): whether the server
// terminates HTTPS itself, and from where it gets the certificate. Parsed from
// OBELO_TLS_MODE, defaulting to TLSModeOff.
//
// It is the one enum here that does NOT follow the "unrecognized value keeps the
// safe default" policy of parseHWAccel and friends, and the asymmetry is
// deliberate. Silently ignoring a typo in a performance knob costs a slower
// encode; silently ignoring `OBELO_TLS_MODE=fiels` gives the operator a server
// that boots cleanly, reports nothing wrong, and carries their password and
// session cookie across the internet in cleartext — while they believe they
// turned TLS on. So FromEnv keeps whatever was typed and Validate refuses to
// boot on it (see Validate).
type TLSMode string

const (
	// TLSModeOff is the default: one plain-HTTP listener, exactly as before, with
	// transport security left to the operator's reverse proxy (ADR-0005).
	TLSModeOff TLSMode = "off"
	// TLSModeFiles terminates TLS with an operator-supplied certificate and key
	// on disk, from any CA they like, re-read without a restart when they change.
	TLSModeFiles TLSMode = "files"
	// TLSModeACME obtains and renews a certificate automatically from an ACME CA
	// (Let's Encrypt by default) using the TLS-ALPN-01 challenge, which completes
	// on the HTTPS port itself — so the household forwards exactly one port and
	// never needs port 80 reachable (ADR-0041). It requires TLSDomains.
	TLSModeACME TLSMode = "acme"
)

// parseTLSMode maps an OBELO_TLS_MODE value to a TLSMode, reporting whether it
// was recognized. It mirrors parseHWAccel's shape — the same lenient spellings
// for "off" — but not its failure policy: an unrecognized value is returned
// VERBATIM rather than replaced with the default, so Validate can quote back
// exactly what the operator typed instead of a value nobody wrote.
func parseTLSMode(v string) (TLSMode, bool) {
	trimmed := strings.TrimSpace(v)
	switch strings.ToLower(trimmed) {
	case "", "off", "false", "0", "no", "disabled":
		return TLSModeOff, true
	case "files":
		return TLSModeFiles, true
	case "acme":
		return TLSModeACME, true
	default:
		return TLSMode(trimmed), false
	}
}

// Config is the resolved server configuration.
type Config struct {
	// ListenAddr is the host:port the HTTP API binds to (e.g. ":8080").
	ListenAddr string

	// TLSMode selects native TLS termination (ADR-0041): TLSModeOff (the
	// default), TLSModeFiles (a certificate on disk), or TLSModeACME (one obtained
	// automatically over ACME/TLS-ALPN-01, which additionally needs TLSDomains).
	//
	// When it is on, the HTTPS listener is an ADDITION — the plain-HTTP listener
	// keeps serving ListenAddr unchanged. That is the load-bearing part of the
	// decision, not an interim step: no public CA will issue for the raw LAN
	// address or the `.local` name the mDNS advertisement publishes (ADR-0034),
	// so a server that dropped its HTTP listener when TLS came on would break
	// discovery for the entire household in exchange for encrypting a hop that
	// never leaves the house.
	//
	// Env: OBELO_TLS_MODE.
	TLSMode TLSMode

	// TLSCertFile and TLSKeyFile are the PEM certificate chain (leaf first) and
	// its private key, used in TLSModeFiles. Give ABSOLUTE paths: they are re-read
	// while the server runs, so a relative path resolves against whatever working
	// directory the process happened to start in.
	//
	// They are re-read on change rather than loaded once at boot because a
	// renewal (certbot, a cron job, a Makefile) rewrites them in place, and a
	// server still holding the boot-time copy serves a chain that is dead the
	// moment the old one expires — with nothing in any log to say so until every
	// client in the house stops trusting it on the same afternoon.
	//
	// Env: OBELO_TLS_CERT / OBELO_TLS_KEY.
	TLSCertFile string
	TLSKeyFile  string

	// TLSListenAddr is the host:port the HTTPS listener binds (default
	// DefaultTLSListenAddr). Separate from ListenAddr, and necessarily so: both
	// listeners run at once, so they cannot share a port. Validate says that in
	// words rather than letting the second bind fail at boot with a bare
	// "address already in use" that names no cause.
	//
	// Env: OBELO_TLS_LISTEN_ADDR.
	TLSListenAddr string

	// TLSDomains are the public DNS names this server answers to in TLSModeACME.
	// They become autocert's HostWhitelist, and Validate REQUIRES at least one.
	//
	// The requirement is the load-bearing part, not a convenience. autocert's host
	// policy defaults to "allow anything", and an ACME manager with no policy asks
	// the CA for a certificate for whatever name arrives in a client's SNI. On a
	// port-forwarded box that is not hypothetical: internet-wide scanners connect
	// with other people's host names in the SNI all day. The server would then
	// burn the CA's per-account issuance rate limit on names it does not own — and
	// become an unwitting participant in somebody else's traffic — while looking
	// completely healthy. So the whitelist is not optional, and neither is this
	// field.
	//
	// Only exact matches count (autocert's HostWhitelist does no wildcards, and
	// TLS-ALPN-01 cannot validate one anyway — RFC 8737 §3 restricts wildcards to
	// DNS-01, which ADR-0041 rules out). List every name, `www.` included.
	//
	// Env: OBELO_TLS_DOMAINS (comma-separated).
	TLSDomains []string

	// ACMEEmail is the optional contact address registered with the ACME account.
	// The CA uses it for expiry warnings and for reaching an operator whose
	// certificate is about to lapse — which, on a box nobody watches, is the one
	// notification worth having. Empty registers an account with no contact, which
	// Let's Encrypt allows.
	//
	// Env: OBELO_ACME_EMAIL.
	ACMEEmail string

	// ACMEDirectoryURL is the ACME directory to obtain certificates from,
	// defaulting to Let's Encrypt production (DefaultACMEDirectoryURL).
	//
	// This override is not a nicety. Let's Encrypt production allows 5 failed
	// authorizations per account per hostname per hour and applies duplicate-
	// certificate limits on top; anyone debugging a port-forward against it runs
	// out of attempts in minutes and is then locked out for an hour with no way to
	// tell a fixed setup from a rate-limited one. Point this at the staging
	// directory — https://acme-staging-v02.api.letsencrypt.org/directory — while
	// setting things up. Staging issues from an untrusted root, so browsers will
	// warn; that is the point, and it means the certificate is real enough to
	// prove the plumbing works.
	//
	// Env: OBELO_ACME_DIRECTORY.
	ACMEDirectoryURL string

	// DataDir is the single writable data directory. It holds the SQLite
	// database and, in later slices, filesystem caches (ADR-0007).
	DataDir string

	// ServerName is the Server identity's human-facing display name (ADR-0034):
	// what a client shows in a discovery picker, and what GET /server reports as
	// "name". Empty means "derive one" — the host's name, else a constant — so a
	// server always has something printable without configuration.
	//
	// Purely cosmetic: nothing keys on it, and renaming never invalidates a Device
	// token (that is the id's job, and the two are separate for exactly this
	// reason).
	ServerName string

	// AdvertiseIPs overrides the addresses published in the mDNS A/AAAA records
	// (ADR-0034). Empty — the normal case — means the server discovers its own
	// link-capable interface addresses. Set it when that guess is wrong: an
	// unusual bridge topology, or a multi-homed host that should only be found on
	// one segment.
	AdvertiseIPs []string

	// MDNSInterface pins the network interface the mDNS responder listens on, and
	// restricts the advertised addresses to that interface's own. Empty — the
	// normal case — lets the kernel pick the default multicast interface.
	//
	// Set it on a Docker host, where that default is frequently docker0 rather
	// than the LAN NIC: the responder then joins the multicast group on a bridge
	// nobody queries and never sees the query, which looks exactly like a server
	// that is not running.
	MDNSInterface string

	// ScanInterval is how often the always-on scheduled scan re-walks every
	// Library as the safety net for changes manual/filesystem triggers missed
	// (ADR-0008). The scheduled scan is incremental, so an unchanged library is
	// cheap (no re-ffprobe). 0 disables the scheduler entirely (manual scans
	// still work). Filesystem watching is explicitly out of scope (ADR-0008).
	ScanInterval time.Duration

	// SessionIdleTimeout is how long a Playback session may go without a progress
	// report before the reaper ends it (issue 08). Progress reports double as
	// keepalive; a session silent for longer than this is assumed abandoned and
	// removed (for direct play there is no transcode scratch to free). 0 disables
	// the reaper entirely. Tests set a short value to observe reaping quickly.
	SessionIdleTimeout time.Duration

	// MaxConcurrentTranscodes is the global cap on simultaneously-running
	// transcode sessions (ADR-0009 governance). Only the transcode tier is
	// metered — direct play and direct stream (remux) are cheap and never count
	// against this cap. When the cap is full a transcode-requiring playback is
	// rejected with 503 SERVER_BUSY (reject-don't-queue) so the client can retry
	// at a lower quality. A value <= 0 means "unlimited" (no governance); the
	// production default is DefaultMaxConcurrentTranscodes.
	MaxConcurrentTranscodes int

	// HardwareAccel selects hardware-accelerated encoding for the transcode tier
	// (ADR-0009). It is an enum: HWAccelOff (the default — CPU libx264), HWAccelAuto
	// (detect the best available backend), or an explicit backend (HWAccelNVENC /
	// HWAccelVAAPI / HWAccelQSV / HWAccelVideoToolbox). The CPU path is the
	// always-available fallback; an explicit backend that is not really present
	// surfaces at transcode time (the warn-then-CPU startup safety and per-session
	// fallback are later slices). VideoToolbox is the one backend wired to a real
	// encoder today; the rest currently resolve to CPU in the args builder.
	//
	// INTERIM: detection is a later slice, so HWAccelAuto resolves to CPU for now
	// (an un-resolved auto is the safe software path). The env back-compat true→auto
	// therefore behaves as CPU until the detector lands.
	HardwareAccel HWAccel

	// --- Enrichment (external-metadata-enrichment) -----------------------------
	//
	// SOURCE-OF-TRUTH HANDOFF (metadata-providers 02): the PROVIDER knobs below —
	// which sources are on, their API keys, base-URL overrides, and MetadataLanguage
	// — are the RUNTIME source of truth ONLY on a fresh install. On first boot (when
	// the DB-backed provider settings are empty) they are SEEDED into the database
	// once; thereafter the DB is authoritative and these env values are IGNORED at
	// runtime. An Admin manages providers from the web UI (GET/PUT
	// /settings/metadata-providers), which takes effect with no restart. So to
	// change a key/host/language on an existing deployment, use the UI — editing the
	// env var here will NOT change enrichment (it only seeded the initial values).
	//
	// The three behavior knobs below — AutoEnrichAfterScan, EnrichInterval, and
	// MusicBrainzRateLimit — are ALSO DB-authoritative after first boot as of
	// enrichment-runtime-settings: they seed the DB-backed settings once (SeedIfEmpty
	// on a fresh install; an idempotent per-column backfill on an upgrade boot), then
	// the running server reads them from the DB, so an Admin changes them from the
	// same settings UI with no restart and this env value is ignored at runtime.
	//
	// Enrichment is OFF until configured (ADR-0001 offline-first): with no
	// TMDBAPIKey the optional Enrichment step is a logged no-op and every Title's
	// status is 'disabled' — a fresh install makes no surprise outbound calls.
	TMDBAPIKey string
	// MetadataLanguage is the preferred metadata language/region for lookups
	// (e.g. "en-US"). Defaults to DefaultMetadataLanguage.
	MetadataLanguage string
	// TMDBBaseURL / TMDBImageBaseURL override the TMDB API + image hosts. They
	// default to the public TMDB endpoints; tests (and the e2e stub) point them at
	// a local server so enrichment is exercised with no real network.
	TMDBBaseURL      string
	TMDBImageBaseURL string
	// MusicBrainzBaseURL / CoverArtBaseURL override the MusicBrainz web service +
	// Cover Art Archive hosts used to enrich the Music kind (issue 03). They
	// default to the public endpoints. MusicBrainz explicitly allows mirroring, so
	// an operator can point MusicBrainzBaseURL at their own mirror (and tests/e2e
	// point both at a local server). These hosts need no key of their own — Music
	// enrichment is gated by MusicBrainzEnabled (or, for backward compatibility, a
	// TMDBAPIKey), not by an API key on these hosts. Env:
	// OBELO_MUSICBRAINZ_BASE_URL / OBELO_COVERART_BASE_URL.
	MusicBrainzBaseURL string
	CoverArtBaseURL    string

	// MusicBrainzRateLimit is the minimum interval between requests to the
	// MusicBrainz host, serializing the whole process under the public ~1 req/sec
	// policy (it answers 503 once you exceed it). It defaults to
	// DefaultMusicBrainzRateLimit. A mirror may set its own policy, so this is
	// tunable; 0 disables throttling entirely (appropriate for a self-hosted mirror
	// with no rate limit). Env: OBELO_MUSICBRAINZ_RATE_LIMIT.
	MusicBrainzRateLimit time.Duration

	// MusicBrainzEnabled turns ON Music enrichment (MusicBrainz + Cover Art
	// Archive) independently of the TMDB key. Because those services need no API
	// key, this explicit opt-in is what keeps a fresh install from making surprise
	// outbound calls (ADR-0001): Music enrichment stays OFF until either this is
	// set or a TMDBAPIKey is present (a key still turns on every kind). Off by
	// default. Env: OBELO_MUSICBRAINZ_ENABLED.
	MusicBrainzEnabled bool

	// FanartTVAPIKey enables artist imagery from fanart.tv — the source for the one
	// thing MusicBrainz lacks (artist images). Empty by default: with no key the
	// chain is not wired and Music enrichment behaves byte-for-byte as before, with
	// zero calls to fanart.tv (ADR-0001 explicit opt-in). Env:
	// OBELO_FANART_TV_API_KEY.
	FanartTVAPIKey string
	// FanartTVBaseURL overrides the fanart.tv API host. It defaults to the public
	// endpoint; tests point it at a local server so the chain is exercised with no
	// real network (mirrors TMDBBaseURL / MusicBrainzBaseURL). Env:
	// OBELO_FANART_TV_BASE_URL.
	FanartTVBaseURL string

	// TheAudioDBAPIKey enables the second artist source, TheAudioDB — the one that
	// also matches by NAME (so artists MusicBrainz didn't MBID-match still get an
	// image) and that carries a real biography (preferred over MusicBrainz's
	// synthesized stub). Empty by default: with no key TheAudioDB is not wired and
	// Music enrichment makes zero calls to it (ADR-0001 explicit opt-in). Env:
	// OBELO_THEAUDIODB_API_KEY.
	TheAudioDBAPIKey string
	// TheAudioDBBaseURL overrides the TheAudioDB API host. It defaults to the public
	// endpoint; tests point it at a local server so the chain is exercised with no
	// real network (mirrors FanartTVBaseURL). Env:
	// OBELO_THEAUDIODB_BASE_URL.
	TheAudioDBBaseURL string

	// AutoEnrichAfterScan triggers a background Enrichment pass for the newly-
	// added/changed ('pending') Titles right after a scan completes (manual or
	// scheduled). ON by default in production; the scan itself stays synchronous +
	// offline (the pass runs in a background goroutine, never blocking the scan
	// response). A no-op unless EnrichmentEnabled.
	AutoEnrichAfterScan bool

	// EnrichInterval is the always-on safety-net cadence that sweeps every Library
	// enriching still-'pending' Titles — the enrichment analogue of ScanInterval.
	// 0 disables the scheduled enrich. A no-op unless EnrichmentEnabled.
	EnrichInterval time.Duration

	// ArtworkCandidateCacheTTL is how long a provider candidate-list result is
	// reused before the artwork picker re-queries the metadata providers — a
	// per-session optimization that keeps auto-search-on-tab-open from burning
	// TMDB/fanart/etc. rate-limits under tab toggling (PRD artwork-management). 0
	// disables the cache with no behavior change (every open re-queries). A pure
	// performance knob: correctness never depends on it.
	ArtworkCandidateCacheTTL time.Duration

	// OpenSubtitlesAPIKey enables external subtitle fetching from OpenSubtitles
	// (ADR-0021). Empty by default: with no key the provider is not wired and a
	// "search online" makes zero outbound calls (ADR-0001 explicit opt-in). It only
	// SEEDS the DB-backed subtitle-provider settings on first boot; thereafter the
	// admin settings UI is authoritative. Env: OBELO_OPENSUBTITLES_API_KEY.
	OpenSubtitlesAPIKey string
	// OpenSubtitlesBaseURL overrides the OpenSubtitles API host. It defaults to the
	// public endpoint; tests point it at a local stub so the fetch flow is exercised
	// with no real network (mirrors TMDBBaseURL). Env:
	// OBELO_OPENSUBTITLES_BASE_URL.
	OpenSubtitlesBaseURL string
	// SubtitleAutoFetchLang is the ISO-639-1 language a completed scan auto-fetches
	// subtitles for. Empty by default (OFF) — OpenSubtitles' small download quota
	// makes bulk auto-fetch a footgun, so it is strictly opt-in (ADR-0021). Seeds the
	// DB knob on first boot. Env: OBELO_SUBTITLE_AUTO_FETCH_LANG.
	SubtitleAutoFetchLang string

	// KeyRotationEnabled turns the optional maintainer-hosted key-rotation channel
	// (ADR-0032, layer 2) ON. Default true, but it only ever contacts the endpoint on
	// an OFFICIAL build (one with a build-injected kAppEncKey) AND after the operator
	// grants enrichment consent — a build-from-source binary or an unconsented server
	// makes zero rotation calls regardless. Set OBELO_KEY_ROTATION=off to disable
	// the channel entirely (one of two independent ways, alongside BYOK, to reach
	// zero maintainer contact — ADR-0001).
	KeyRotationEnabled bool
	// KeyRotationURL is the endpoint the server polls for a replacement default-key
	// payload (ADR-0032). Defaults to DefaultKeyRotationURL; tests point it at a stub.
	// Empty disables the fetch as surely as KeyRotationEnabled=false.
	KeyRotationURL string
	// KeyRotationInterval is how often the server re-polls the rotation endpoint after
	// the first successful fetch — the "every N hours" cadence (ADR-0032). Defaults to
	// DefaultKeyRotationInterval; 0 disables the periodic re-poll (a single startup
	// fetch only). Tests set a short value to observe a rotated key being picked up.
	KeyRotationInterval time.Duration

	// EnrichmentConsentGranted pre-seeds the first-run Enrichment consent decision
	// (ADR-0032) on first boot, for a HEADLESS deploy that wants to answer the prompt
	// without a UI (and the mechanism the test harness uses). nil (the default) leaves
	// consent UNDECIDED so a fresh install prompts and makes no outbound enrichment
	// calls until the operator answers; a non-nil value records that decision. It only
	// SEEDS on first boot; thereafter the DB (the admin toggle) is authoritative.
	// Env: OBELO_ENRICHMENT_CONSENT (granted/true/1 → grant, declined/false/0 →
	// decline, unset → leave undecided).
	EnrichmentConsentGranted *bool
}

// DefaultTLSListenAddr is the port the HTTPS listener binds when TLS is turned
// on (ADR-0041). 8443 is the conventional unprivileged HTTPS port, and pairing
// it with the unprivileged 8080 keeps the server runnable as a normal user —
// binding 443 would need root or a capability, which is a much larger ask than
// "forward a port on the router", the deployment this exists for.
const DefaultTLSListenAddr = ":8443"

// DefaultACMEDirectoryURL is the ACME directory used when the operator names
// none: Let's Encrypt production. Spelled out rather than taken from
// acme.LetsEncryptURL so this package does not pull in x/crypto/acme just to
// hold a string; it is the same URL, and it is stable enough that a CA changing
// it would be news.
//
// Test and set up against the STAGING directory instead —
// https://acme-staging-v02.api.letsencrypt.org/directory — see
// Config.ACMEDirectoryURL for why the production rate limits make iterating
// against this one miserable.
const DefaultACMEDirectoryURL = "https://acme-v02.api.letsencrypt.org/directory"

// DefaultScanInterval is the periodic safety-net scan cadence (ADR-0008). One
// hour balances "pick up changes without being told" against pointless churn;
// the scan is incremental so an unchanged library costs only a directory walk.
const DefaultScanInterval = time.Hour

// DefaultSessionIdleTimeout is how long an idle Playback session lives before
// the reaper ends it. Progress arrives every ~10–15s (api-contract.md), so a
// minute-plus of silence is a safe "abandoned" signal that tolerates transient
// network hiccups without prematurely killing an active stream.
const DefaultSessionIdleTimeout = 90 * time.Second

// SessionReapInterval is how often the reaper goroutine sweeps for idle
// sessions. Sweeping faster than the timeout bounds how long a reaped session
// lingers in the map before its stream/progress/delete answer 404.
const SessionReapInterval = 30 * time.Second

// DefaultMaxConcurrentTranscodes is the production cap on simultaneous
// transcode sessions (ADR-0009). A small number keeps a single host from being
// saturated by CPU re-encodes (each transcode is the one operation that can peg
// the box); operators with more cores raise it via the env knob. Direct play
// and remux are unmetered, so this only bounds the expensive tier. 3 is a
// conservative default that leaves headroom for the rest of the server.
const DefaultMaxConcurrentTranscodes = 3

// DefaultMetadataLanguage is the metadata language/region used for external
// lookups when the operator sets none. TMDB returns localized overviews/titles
// for this tag; en-US is the broadest default.
const DefaultMetadataLanguage = "en-US"

// DefaultTMDBBaseURL / DefaultTMDBImageBaseURL are the public TMDB endpoints.
// Overridable so tests point enrichment at a local server (no real network).
const (
	DefaultTMDBBaseURL      = "https://api.themoviedb.org/3"
	DefaultTMDBImageBaseURL = "https://image.tmdb.org/t/p/original"
)

// DefaultMusicBrainzBaseURL / DefaultCoverArtBaseURL are the public Music
// metadata endpoints (issue 03). Overridable so tests point enrichment at a local
// server (no real network).
const (
	DefaultMusicBrainzBaseURL = "https://musicbrainz.org/ws/2"
	DefaultCoverArtBaseURL    = "https://coverartarchive.org"
)

// DefaultMusicBrainzRateLimit is the minimum interval between MusicBrainz
// requests when the operator sets none — one second, matching the public host's
// ~1 req/sec policy (it answers 503 above that). Operators pointing at a mirror
// with a different policy override it (0 disables throttling).
const DefaultMusicBrainzRateLimit = time.Second

// DefaultFanartTVBaseURL is the public fanart.tv API host (artist imagery).
// Overridable so tests point the chain at a local server (no real network).
const DefaultFanartTVBaseURL = "https://webservice.fanart.tv/v3"

// DefaultTheAudioDBBaseURL is the public TheAudioDB API host (artist image + bio).
// Overridable so tests point the chain at a local server (no real network).
const DefaultTheAudioDBBaseURL = "https://www.theaudiodb.com/api/v1/json"

// DefaultEnrichInterval is the safety-net scheduled-enrich cadence (the
// enrichment analogue of DefaultScanInterval). A few hours is plenty: most
// Titles enrich the moment a scan adds them (auto-after-scan); the sweep just
// backfills anything still 'pending' (e.g. a Title added while the provider was
// down).
const DefaultEnrichInterval = 6 * time.Hour

// DefaultArtworkCandidateCacheTTL is the default lifetime of a cached provider
// candidate-list result (enrich.DefaultCandidateCacheTTL). A couple of minutes
// absorbs artwork-tab toggling without a fresh provider hit each time, while
// staying short enough that a refreshed record surfaces new options soon.
const DefaultArtworkCandidateCacheTTL = 2 * time.Minute

// DefaultKeyRotationURL is a BUILD-INJECTED var, empty in source, declared in
// bootstrap.go alongside the other `-ldflags -X` values so the maintainer host is
// not committed to the public repo. A build-from-source binary therefore has no
// default rotation URL and never polls. Overridable at runtime via
// OBELO_KEY_ROTATION_URL (tests point it at a stub).

// DefaultKeyRotationInterval is how often an official build re-polls the rotation
// endpoint after its first successful fetch (ADR-0032). Six hours matches the
// enrichment sweep cadence: routine key rotation is not time-critical (a leaked key
// is revoked at the provider immediately; the replacement just needs to reach
// installs within hours), and a long interval keeps maintainer contact minimal.
const DefaultKeyRotationInterval = 6 * time.Hour

// Defaults returns a Config populated with sensible defaults. Callers may
// override fields before calling Validate.
func Defaults() Config {
	return Config{
		ListenAddr:               ":8080",
		TLSMode:                  TLSModeOff,
		TLSListenAddr:            DefaultTLSListenAddr,
		ACMEDirectoryURL:         DefaultACMEDirectoryURL,
		DataDir:                  "./data",
		KeyRotationEnabled:       true,
		KeyRotationURL:           DefaultKeyRotationURL,
		KeyRotationInterval:      DefaultKeyRotationInterval,
		ScanInterval:             DefaultScanInterval,
		SessionIdleTimeout:       DefaultSessionIdleTimeout,
		MaxConcurrentTranscodes:  DefaultMaxConcurrentTranscodes,
		HardwareAccel:            HWAccelOff,
		MetadataLanguage:         DefaultMetadataLanguage,
		TMDBBaseURL:              DefaultTMDBBaseURL,
		TMDBImageBaseURL:         DefaultTMDBImageBaseURL,
		MusicBrainzBaseURL:       DefaultMusicBrainzBaseURL,
		CoverArtBaseURL:          DefaultCoverArtBaseURL,
		MusicBrainzRateLimit:     DefaultMusicBrainzRateLimit,
		FanartTVBaseURL:          DefaultFanartTVBaseURL,
		TheAudioDBBaseURL:        DefaultTheAudioDBBaseURL,
		AutoEnrichAfterScan:      true,
		EnrichInterval:           DefaultEnrichInterval,
		ArtworkCandidateCacheTTL: DefaultArtworkCandidateCacheTTL,
	}
}

// VideoEnrichmentEnabled reports whether the Movie/TV kinds enrich. TMDB is the
// video provider and requires an API key, so video enrichment is on exactly when
// a key is configured; otherwise those Titles report status 'disabled'.
func (c Config) VideoEnrichmentEnabled() bool {
	return c.TMDBAPIKey != ""
}

// MusicEnrichmentEnabled reports whether the Music kind enriches. MusicBrainz +
// Cover Art Archive need no key, so Music turns on via its own MusicBrainzEnabled
// opt-in — or alongside a TMDB key, which enables every kind (backward compatible
// with the original single-switch behavior). Off by default so a fresh install
// makes no surprise outbound calls (ADR-0001 offline-first).
func (c Config) MusicEnrichmentEnabled() bool {
	return c.MusicBrainzEnabled || c.TMDBAPIKey != ""
}

// MusicImageEnabled reports whether an artist-image source is configured for the
// Music kind. True iff at least one image key is set (fanart.tv OR TheAudioDB). It
// is strictly additive: app.New wraps MusicBrainz in the image-filling chain only
// when this AND MusicEnrichmentEnabled are true, so an image key never turns Music
// enrichment on by itself, and no key keeps the offline-first no-image behavior
// (ADR-0001 — no public/default key is baked in).
func (c Config) MusicImageEnabled() bool {
	return c.FanartTVAPIKey != "" || c.TheAudioDBAPIKey != ""
}

// EnrichmentEnabled reports whether ANY kind enriches. The app uses it as the
// master switch deciding whether to run the enrich worker and the auto-after-scan
// / scheduled triggers at all; the per-kind gates above decide which Titles a pass
// actually looks up vs. records 'disabled' (ADR-0001 offline-first).
func (c Config) EnrichmentEnabled() bool {
	return c.VideoEnrichmentEnabled() || c.MusicEnrichmentEnabled()
}

// ArtworkCacheDir returns the durable on-disk cache for Enrichment-fetched
// artwork, under the data dir (ADR-0007: bulk files on the filesystem, not the
// DB). Unlike the transcode scratch this is durable cache — it survives restarts
// and is NOT cleared at boot, so re-enrichment and restarts don't re-download.
func (c Config) ArtworkCacheDir() string {
	return filepath.Join(c.DataDir, "artwork")
}

// MetadataKeysPath returns the file under the data dir that caches the last
// successfully fetched rotation-endpoint keys (ADR-0032/0007). It is durable cache
// — like the artwork cache it survives restarts and is not cleared at boot, so a
// brief endpoint outage reuses the last good keys rather than dropping to the
// bootstrap key (ADR-0032 story 14).
func (c Config) MetadataKeysPath() string {
	return filepath.Join(c.DataDir, "metadata-keys.json")
}

// ACMECacheDir returns the directory autocert stores its state in (ADR-0041,
// TLSModeACME): the ACME ACCOUNT KEY and every issued certificate's PRIVATE KEY.
// Under the data dir like every other durable file (ADR-0007), and created 0700
// by the caller in cmd/obelo — this is the most sensitive directory the server
// owns, and anyone who can read it can impersonate the server to every client in
// the house.
//
// It must survive restarts, and that is not merely an optimization. autocert
// re-issues anything it cannot find here, so a cache on tmpfs, or one pointed at
// a container layer that is rebuilt on deploy, means a fresh certificate on every
// restart — which walks straight into the CA's duplicate-certificate rate limit
// and leaves the operator unable to get a certificate at all for a week.
func (c Config) ACMECacheDir() string {
	return filepath.Join(c.DataDir, "acme")
}

// SubtitleCacheDir returns the durable on-disk cache for externally fetched
// Subtitle tracks, under the data dir (ADR-0007/0021), a sibling of the artwork
// cache. Like it, this is durable cache — never written into library folders and
// not cleared at boot, so a fetched subtitle survives restarts and rescans.
func (c Config) SubtitleCacheDir() string {
	return filepath.Join(c.DataDir, "subtitles")
}

// FromEnv builds a Config from defaults overlaid with environment variables:
//
//	OBELO_LISTEN_ADDR    -> ListenAddr
//	OBELO_TLS_MODE       -> TLSMode (off|files|acme; default off. `files` and
//	                               `acme` both ADD an HTTPS listener alongside the
//	                               plain-HTTP one — neither replaces it. An
//	                               unrecognized value is a Validate error, unlike
//	                               the knobs below)
//	OBELO_TLS_CERT       -> TLSCertFile (absolute path to the PEM chain)
//	OBELO_TLS_KEY        -> TLSKeyFile (absolute path to the PEM private key)
//	OBELO_TLS_LISTEN_ADDR -> TLSListenAddr (host:port for HTTPS, default :8443;
//	                               must differ from OBELO_LISTEN_ADDR)
//	OBELO_TLS_DOMAINS    -> TLSDomains (comma-separated public DNS names;
//	                               REQUIRED in acme mode — it becomes autocert's
//	                               HostWhitelist, and there is deliberately no
//	                               "allow anything" default)
//	OBELO_ACME_EMAIL     -> ACMEEmail (optional CA contact for expiry notices)
//	OBELO_ACME_DIRECTORY -> ACMEDirectoryURL (the ACME directory; defaults to
//	                               Let's Encrypt production. Point it at
//	                               https://acme-staging-v02.api.letsencrypt.org/directory
//	                               while setting up, or the production rate limits
//	                               will lock you out mid-debug)
//	OBELO_DATA_DIR       -> DataDir
//	OBELO_SERVER_NAME    -> ServerName (the Server identity's display name,
//	                               ADR-0034; empty derives one from the hostname)
//	OBELO_ADVERTISE_IP   -> AdvertiseIPs (comma-separated IPs to publish in the
//	                               mDNS records; empty auto-discovers them)
//	OBELO_MDNS_INTERFACE -> MDNSInterface (an interface name, e.g. "eth0", to
//	                               pin the mDNS responder to; empty uses the
//	                               system default multicast interface)
//	OBELO_SCAN_INTERVAL  -> ScanInterval (a Go duration, e.g. "30m";
//	                               "0" disables the scheduled scan)
//	OBELO_SESSION_IDLE_TIMEOUT -> SessionIdleTimeout (a Go duration, e.g.
//	                               "2m"; "0" disables the session reaper)
//	OBELO_MAX_CONCURRENT_TRANSCODES -> MaxConcurrentTranscodes (an integer;
//	                               "0" or negative disables the cap / unlimited)
//	OBELO_HARDWARE_ACCEL -> HardwareAccel (an enum: off|auto|nvenc|vaapi|
//	                               qsv|videotoolbox; off/false/0 → off, the legacy
//	                               true/1 → auto; default off → CPU libx264 path)
//	OBELO_MUSICBRAINZ_ENABLED -> MusicBrainzEnabled (a bool; default false —
//	                               turns on Music enrichment without a TMDB key)
//	OBELO_MUSICBRAINZ_BASE_URL -> MusicBrainzBaseURL (the MusicBrainz host;
//	                               point at a mirror, default is the public host)
//	OBELO_MUSICBRAINZ_RATE_LIMIT -> MusicBrainzRateLimit (a Go duration, e.g.
//	                               "1s"; "0" disables throttling for a mirror with
//	                               no rate policy; default DefaultMusicBrainzRateLimit)
//	OBELO_FANART_TV_API_KEY -> FanartTVAPIKey (enables artist imagery from
//	                               fanart.tv; empty = off, no fanart.tv calls)
//	OBELO_THEAUDIODB_API_KEY -> TheAudioDBAPIKey (enables the name-matching
//	                               artist image + biography source; empty = off,
//	                               no TheAudioDB calls)
//	OBELO_AUTO_ENRICH    -> AutoEnrichAfterScan (a bool; default true)
//	OBELO_ENRICH_INTERVAL -> EnrichInterval (a Go duration, e.g. "6h";
//	                               "0" disables the scheduled enrich)
//
// An unparseable scan-interval falls back to the default rather than failing
// boot (the scheduled scan is a safety net, not a critical path); the same
// lenient policy applies to the transcode-cap and HW-accel knobs.
func FromEnv() Config {
	c := Defaults()
	if v := os.Getenv("OBELO_LISTEN_ADDR"); v != "" {
		c.ListenAddr = v
	}
	// Native TLS (ADR-0041). Note the missing `if ok` the other enum knobs have:
	// an unrecognized mode is stored as typed and rejected by Validate, because
	// falling back to "off" here would answer "I asked for TLS" with a silently
	// plain-HTTP server.
	if v := os.Getenv("OBELO_TLS_MODE"); v != "" {
		c.TLSMode, _ = parseTLSMode(v)
	}
	if v := os.Getenv("OBELO_TLS_CERT"); v != "" {
		c.TLSCertFile = strings.TrimSpace(v)
	}
	if v := os.Getenv("OBELO_TLS_KEY"); v != "" {
		c.TLSKeyFile = strings.TrimSpace(v)
	}
	if v := os.Getenv("OBELO_TLS_LISTEN_ADDR"); v != "" {
		c.TLSListenAddr = strings.TrimSpace(v)
	}
	if v := os.Getenv("OBELO_TLS_DOMAINS"); v != "" {
		// Split and trim only; the names are checked in Validate, which can refuse
		// to boot and say why. Dropping a malformed entry here would leave the
		// operator with a host policy quietly missing one of the names they listed
		// — and the symptom, months later, is one hostname that never gets a
		// certificate while the others do.
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				c.TLSDomains = append(c.TLSDomains, part)
			}
		}
	}
	if v := os.Getenv("OBELO_ACME_EMAIL"); v != "" {
		c.ACMEEmail = strings.TrimSpace(v)
	}
	if v := os.Getenv("OBELO_ACME_DIRECTORY"); v != "" {
		c.ACMEDirectoryURL = strings.TrimSpace(v)
	}
	if v := os.Getenv("OBELO_DATA_DIR"); v != "" {
		c.DataDir = v
	}
	if v := os.Getenv("OBELO_SERVER_NAME"); v != "" {
		c.ServerName = v
	}
	if v := os.Getenv("OBELO_ADVERTISE_IP"); v != "" {
		// Split only; the addresses are parsed where they are used, so a typo is
		// reported as "not a valid IP address" in the boot log rather than
		// silently dropped here.
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				c.AdvertiseIPs = append(c.AdvertiseIPs, part)
			}
		}
	}
	if v := os.Getenv("OBELO_MDNS_INTERFACE"); v != "" {
		c.MDNSInterface = strings.TrimSpace(v)
	}
	if v := os.Getenv("OBELO_SCAN_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			c.ScanInterval = d
		}
	}
	if v := os.Getenv("OBELO_SESSION_IDLE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			c.SessionIdleTimeout = d
		}
	}
	if v := os.Getenv("OBELO_MAX_CONCURRENT_TRANSCODES"); v != "" {
		// A parseable integer overrides the default; 0/negative means unlimited.
		// An unparseable value keeps the default rather than failing boot.
		if n, err := strconv.Atoi(v); err == nil {
			c.MaxConcurrentTranscodes = n
		}
	}
	if v := os.Getenv("OBELO_HARDWARE_ACCEL"); v != "" {
		// Off by default; an explicit backend name (or the legacy bool true→auto)
		// selects it. An unrecognized value leaves the default off (the safe CPU
		// path), mirroring the other knobs' lenient "garbage stays safe" policy.
		if a, ok := parseHWAccel(v); ok {
			c.HardwareAccel = a
		}
	}
	// Enrichment knobs (external-metadata-enrichment). A key turns enrichment on;
	// its absence keeps the offline-first no-op posture. The base-URL overrides
	// exist so tests/e2e point at a local stub.
	if v := os.Getenv("OBELO_TMDB_API_KEY"); v != "" {
		c.TMDBAPIKey = v
	}
	if v := os.Getenv("OBELO_METADATA_LANGUAGE"); v != "" {
		c.MetadataLanguage = v
	}
	if v := os.Getenv("OBELO_TMDB_BASE_URL"); v != "" {
		c.TMDBBaseURL = v
	}
	if v := os.Getenv("OBELO_TMDB_IMAGE_BASE_URL"); v != "" {
		c.TMDBImageBaseURL = v
	}
	if v := os.Getenv("OBELO_MUSICBRAINZ_BASE_URL"); v != "" {
		c.MusicBrainzBaseURL = v
	}
	if v := os.Getenv("OBELO_COVERART_BASE_URL"); v != "" {
		c.CoverArtBaseURL = v
	}
	// MusicBrainzRateLimit: a Go duration ("0" disables throttling, for a mirror
	// with no rate policy). An unparseable value keeps the default rather than
	// failing boot (throttling is a politeness knob, not a critical path).
	if v := os.Getenv("OBELO_MUSICBRAINZ_RATE_LIMIT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			c.MusicBrainzRateLimit = d
		}
	}
	if v := os.Getenv("OBELO_FANART_TV_API_KEY"); v != "" {
		c.FanartTVAPIKey = v
	}
	if v := os.Getenv("OBELO_FANART_TV_BASE_URL"); v != "" {
		c.FanartTVBaseURL = v
	}
	if v := os.Getenv("OBELO_THEAUDIODB_API_KEY"); v != "" {
		c.TheAudioDBAPIKey = v
	}
	if v := os.Getenv("OBELO_THEAUDIODB_BASE_URL"); v != "" {
		c.TheAudioDBBaseURL = v
	}
	// MusicBrainzEnabled: opt Music enrichment in without a TMDB key. Off by
	// default; only a clearly-true value flips it on (an unparseable value leaves
	// the offline-safe default).
	if v := os.Getenv("OBELO_MUSICBRAINZ_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.MusicBrainzEnabled = b
		}
	}
	// AutoEnrichAfterScan defaults ON; only a clearly-false value turns it off (an
	// unparseable value leaves the default).
	if v := os.Getenv("OBELO_AUTO_ENRICH"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.AutoEnrichAfterScan = b
		}
	}
	// EnrichInterval: a Go duration ("0" disables the scheduled enrich). An
	// unparseable value keeps the default (the sweep is a safety net, not critical).
	if v := os.Getenv("OBELO_ENRICH_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			c.EnrichInterval = d
		}
	}
	// ArtworkCandidateCacheTTL: a Go duration ("0" disables the picker's candidate
	// cache). An unparseable value keeps the default (a pure performance knob).
	if v := os.Getenv("OBELO_ARTWORK_CANDIDATE_CACHE_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			c.ArtworkCandidateCacheTTL = d
		}
	}
	// External subtitle fetching (ADR-0021): these only SEED the DB-backed
	// subtitle-provider settings on first boot; thereafter the admin UI is
	// authoritative. Empty means "no provider configured" (zero outbound calls).
	if v := os.Getenv("OBELO_OPENSUBTITLES_API_KEY"); v != "" {
		c.OpenSubtitlesAPIKey = v
	}
	if v := os.Getenv("OBELO_OPENSUBTITLES_BASE_URL"); v != "" {
		c.OpenSubtitlesBaseURL = v
	}
	if v := os.Getenv("OBELO_SUBTITLE_AUTO_FETCH_LANG"); v != "" {
		c.SubtitleAutoFetchLang = v
	}
	// Key rotation (ADR-0032, layer 2). OBELO_KEY_ROTATION=off (or false/0/no)
	// disables the channel entirely — one of two independent ways (with BYOK) to
	// reach zero maintainer contact; any other value leaves it on. The URL and
	// interval overrides exist mainly so tests point the fetch at a stub and observe
	// a rotated key promptly.
	if v := os.Getenv("OBELO_KEY_ROTATION"); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "off", "false", "0", "no", "disabled":
			c.KeyRotationEnabled = false
		default:
			c.KeyRotationEnabled = true
		}
	}
	if v := os.Getenv("OBELO_KEY_ROTATION_URL"); v != "" {
		c.KeyRotationURL = v
	}
	if v := os.Getenv("OBELO_KEY_ROTATION_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			c.KeyRotationInterval = d
		}
	}
	// Enrichment consent (ADR-0032): accept the friendly "granted"/"declined"
	// spellings as well as the generic bool forms; anything else (or unset) leaves
	// consent undecided so a fresh install still prompts.
	if v := os.Getenv("OBELO_ENRICHMENT_CONSENT"); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "granted":
			c.EnrichmentConsentGranted = boolPtr(true)
		case "declined":
			c.EnrichmentConsentGranted = boolPtr(false)
		default:
			if b, err := strconv.ParseBool(v); err == nil {
				c.EnrichmentConsentGranted = boolPtr(b)
			}
		}
	}
	return c
}

// boolPtr returns a pointer to v — used for the optional tri-state config knobs
// where nil is meaningfully distinct from false (e.g. EnrichmentConsentGranted:
// nil = undecided, &false = declined).
func boolPtr(v bool) *bool { return &v }

// Validate checks the configuration for obvious mistakes. It touches the
// filesystem in exactly one place — loading the TLS key pair in files mode,
// which is the only way to tell a mistyped path or a mismatched pair from a
// working one — and is otherwise a pure check; see EnsureDataDir for the data
// directory.
func (c Config) Validate() error {
	if c.ListenAddr == "" {
		return fmt.Errorf("config: ListenAddr must not be empty")
	}
	if c.DataDir == "" {
		return fmt.Errorf("config: DataDir must not be empty")
	}
	if err := c.validateTLS(); err != nil {
		return err
	}
	return nil
}

// validateTLS checks the native-TLS knobs (ADR-0041).
//
// THE TWO MODES HAVE OPPOSITE BOOT POSTURES ON PURPOSE. Do not reconcile them.
//
//   - `files` is STRICT: an unreadable path or a mismatched pair REFUSES TO BOOT.
//     The operator typed a path; a path that is wrong is a typo, not an outage,
//     and a server that started anyway would leave them believing they had HTTPS
//     while their password and session cookie crossed the internet in the clear.
//     A loud failure at the moment they can still fix it is strictly better.
//
//   - `acme` is LENIENT about the certificate and strict only about the
//     CONFIGURATION. Everything checked here is something the operator typed
//     (the domain list, the ports). Whether a certificate can actually be
//     obtained is not checked at all, and must not be: a CA outage, DNS that has
//     not propagated, a router whose port-forward is not set up yet, or a
//     rate limit are the world being temporarily unavailable, not a mistake, and
//     ADR-0041 is explicit that refusing to serve the LAN over one of them is a
//     bad trade. That side lives in cmd/obelo (newServers/acmeCertificates),
//     which builds the listener regardless and logs when a handshake cannot be
//     answered.
//
// The dividing line is "did the operator get something wrong, or did the world?"
// — not the mode. Both readings follow from the same ADR sentence.
func (c Config) validateTLS() error {
	switch c.TLSMode {
	case "", TLSModeOff:
		// No HTTPS listener is built, so the cert/key/domain/address fields are
		// inert and there is nothing to check — including TLSListenAddr, which
		// nothing binds. "" is the zero value a hand-built Config (tests,
		// embedders) carries and means the same as off; only FromEnv/Defaults
		// populate the constant.
		return nil
	case TLSModeFiles, TLSModeACME:
		// Checked below.
	default:
		return fmt.Errorf("config: unknown OBELO_TLS_MODE %q (want off, files, or acme)", string(c.TLSMode))
	}

	// Both modes bind the HTTPS listener, so both need an address of its own.
	// Checked before the mode-specific parts so the message an operator gets is
	// about the port they set rather than about a certificate they have not got
	// round to yet.
	if c.TLSListenAddr == "" {
		return fmt.Errorf("config: OBELO_TLS_LISTEN_ADDR must not be empty when OBELO_TLS_MODE=%s (default %q)", string(c.TLSMode), DefaultTLSListenAddr)
	}
	if c.TLSListenAddr == c.ListenAddr {
		return fmt.Errorf("config: OBELO_TLS_LISTEN_ADDR and OBELO_LISTEN_ADDR are both %q — HTTPS is an ADDITIONAL listener, not a replacement for the plain-HTTP one (ADR-0041), so the two need separate ports", c.TLSListenAddr)
	}

	if c.TLSMode == TLSModeACME {
		return c.validateACME()
	}

	if c.TLSCertFile == "" {
		return fmt.Errorf("config: OBELO_TLS_MODE=files needs OBELO_TLS_CERT (an absolute path to the PEM certificate chain)")
	}
	if c.TLSKeyFile == "" {
		return fmt.Errorf("config: OBELO_TLS_MODE=files needs OBELO_TLS_KEY (an absolute path to the PEM private key)")
	}
	// Load the pair here, at the one moment a bad path is cheap to fix. The error
	// from LoadX509KeyPair names the file it failed on ("open /etc/…: no such
	// file"), so both paths are restated around it: the common mistakes are a
	// wrong path, an unreadable file, and a key that belongs to a different
	// certificate, and the third one is invisible without actually pairing them.
	if _, err := tls.LoadX509KeyPair(c.TLSCertFile, c.TLSKeyFile); err != nil {
		return fmt.Errorf("config: TLS certificate %q with key %q does not load as a usable pair: %w", c.TLSCertFile, c.TLSKeyFile, err)
	}
	return nil
}

// validateACME checks the knobs the operator types for TLSModeACME. It makes no
// network call and touches no file — see validateTLS on why a certificate that
// cannot be obtained is emphatically not this function's business.
//
// THE HOST LIST IS THE WHOLE POINT OF THIS FUNCTION. Everything else here is
// spelling; the empty check below is what stops the server becoming a
// certificate vending machine for whatever host name a stranger puts in an SNI.
func (c Config) validateACME() error {
	if len(c.TLSDomains) == 0 {
		return fmt.Errorf("config: OBELO_TLS_MODE=acme needs OBELO_TLS_DOMAINS (a comma-separated list of the public DNS names this server answers to, e.g. \"media.example.com\"). It is required and has no default on purpose: the list becomes autocert's HostWhitelist, and a manager with no host policy asks the CA for a certificate for whatever name arrives in a client's SNI — so a port-forwarded server would spend the account's issuance rate limit on names it does not own, on behalf of whoever pointed them here, while looking perfectly healthy")
	}
	for _, d := range c.TLSDomains {
		if err := validateACMEDomain(d); err != nil {
			return fmt.Errorf("config: OBELO_TLS_DOMAINS entry %q: %w", d, err)
		}
	}
	if c.ACMEDirectoryURL == "" {
		return fmt.Errorf("config: OBELO_TLS_MODE=acme needs an ACME directory URL (OBELO_ACME_DIRECTORY); leave it unset to use %s, or point it at https://acme-staging-v02.api.letsencrypt.org/directory while you are setting things up", DefaultACMEDirectoryURL)
	}
	return nil
}

// validateACMEDomain rejects the entries that could never work, as opposed to
// the ones that merely might not. Each of these fails silently and permanently
// otherwise: autocert's HostWhitelist "silently ignores" a name it cannot
// convert, and every rejection below then surfaces months later as one hostname
// that never gets a certificate while its siblings do, or as an opaque error
// from the CA that names nothing an operator can act on.
func validateACMEDomain(d string) error {
	if strings.ContainsAny(d, " \t/\\") || strings.Contains(d, "://") {
		return fmt.Errorf("looks like a URL or has a stray character — give a bare host name, e.g. \"media.example.com\", with no scheme, path, or spaces")
	}
	if strings.HasPrefix(d, "*") {
		return fmt.Errorf("wildcards are not possible here — TLS-ALPN-01 cannot validate one (RFC 8737 §3 allows wildcards only over DNS-01, which ADR-0041 rules out), and autocert's host whitelist matches exactly in any case. List each name in full")
	}
	if strings.Contains(d, ":") {
		return fmt.Errorf("carries a port — the certificate is issued for a name, not a name:port. Set the port with OBELO_TLS_LISTEN_ADDR")
	}
	if net.ParseIP(d) != nil {
		return fmt.Errorf("is an IP address — autocert only ever asks the CA to authorize a DNS identifier, so this can never be issued. Use the DNS name that points at this address (ADR-0041: the LAN keeps plain HTTP precisely because no CA will certify an address)")
	}
	if !strings.Contains(strings.Trim(d, "."), ".") {
		return fmt.Errorf("is a single label — autocert refuses a name with no dot, so this would be rejected at every handshake. Give the fully qualified name")
	}
	return nil
}

// DBPath returns the absolute path to the SQLite database file inside the
// data directory.
func (c Config) DBPath() string {
	return filepath.Join(c.DataDir, "obelo.db")
}

// TranscodeScratchDir returns the directory under the data dir that holds the
// per-session remux/transcode scratch (HLS playlists + segments), per ADR-0007
// (one mounted volume holds everything). Each Playback session gets its own
// subdirectory beneath this, created on demand and deleted when the session ends
// or is reaped. It is deliberately ephemeral — nothing here survives a restart.
func (c Config) TranscodeScratchDir() string {
	return filepath.Join(c.DataDir, "transcode")
}

// EnsureDataDir creates the data directory if it does not exist and verifies
// that it is a writable directory. A missing data directory is created; a path
// that exists but is not a writable directory yields a clear error.
func (c Config) EnsureDataDir() error {
	info, err := os.Stat(c.DataDir)
	switch {
	case os.IsNotExist(err):
		if mkErr := os.MkdirAll(c.DataDir, 0o755); mkErr != nil {
			return fmt.Errorf("config: creating data directory %q: %w", c.DataDir, mkErr)
		}
	case err != nil:
		return fmt.Errorf("config: inspecting data directory %q: %w", c.DataDir, err)
	case !info.IsDir():
		return fmt.Errorf("config: data directory %q exists but is not a directory", c.DataDir)
	}

	// Verify writability by creating and removing a probe file. This catches
	// read-only mounts up front rather than at first DB write.
	probe := filepath.Join(c.DataDir, ".write-probe")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("config: data directory %q is not writable: %w", c.DataDir, err)
	}
	_ = f.Close()
	_ = os.Remove(probe)
	return nil
}
