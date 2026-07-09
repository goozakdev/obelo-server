// Package config loads and validates server configuration.
//
// Configuration is intentionally small for the walking skeleton: the server
// needs to know which address to listen on and where its writable data
// directory lives. The data directory holds the single SQLite database and
// (in later slices) the artwork/transcode caches, per ADR-0007.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// HWAccel is the hardware-acceleration knob's vocabulary (ADR-0009): off (CPU),
// auto (detect the best backend), or an explicit backend. The string values are
// the operator-facing names parsed from JUICEBOX_HARDWARE_ACCEL and match the
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

// parseHWAccel maps a JUICEBOX_HARDWARE_ACCEL value to a HWAccel, reporting
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

// Config is the resolved server configuration.
type Config struct {
	// ListenAddr is the host:port the HTTP API binds to (e.g. ":8080").
	ListenAddr string

	// DataDir is the single writable data directory. It holds the SQLite
	// database and, in later slices, filesystem caches (ADR-0007).
	DataDir string

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
	// JUICEBOX_MUSICBRAINZ_BASE_URL / JUICEBOX_COVERART_BASE_URL.
	MusicBrainzBaseURL string
	CoverArtBaseURL    string

	// MusicBrainzRateLimit is the minimum interval between requests to the
	// MusicBrainz host, serializing the whole process under the public ~1 req/sec
	// policy (it answers 503 once you exceed it). It defaults to
	// DefaultMusicBrainzRateLimit. A mirror may set its own policy, so this is
	// tunable; 0 disables throttling entirely (appropriate for a self-hosted mirror
	// with no rate limit). Env: JUICEBOX_MUSICBRAINZ_RATE_LIMIT.
	MusicBrainzRateLimit time.Duration

	// MusicBrainzEnabled turns ON Music enrichment (MusicBrainz + Cover Art
	// Archive) independently of the TMDB key. Because those services need no API
	// key, this explicit opt-in is what keeps a fresh install from making surprise
	// outbound calls (ADR-0001): Music enrichment stays OFF until either this is
	// set or a TMDBAPIKey is present (a key still turns on every kind). Off by
	// default. Env: JUICEBOX_MUSICBRAINZ_ENABLED.
	MusicBrainzEnabled bool

	// FanartTVAPIKey enables artist imagery from fanart.tv — the source for the one
	// thing MusicBrainz lacks (artist images). Empty by default: with no key the
	// chain is not wired and Music enrichment behaves byte-for-byte as before, with
	// zero calls to fanart.tv (ADR-0001 explicit opt-in). Env:
	// JUICEBOX_FANART_TV_API_KEY.
	FanartTVAPIKey string
	// FanartTVBaseURL overrides the fanart.tv API host. It defaults to the public
	// endpoint; tests point it at a local server so the chain is exercised with no
	// real network (mirrors TMDBBaseURL / MusicBrainzBaseURL). Env:
	// JUICEBOX_FANART_TV_BASE_URL.
	FanartTVBaseURL string

	// TheAudioDBAPIKey enables the second artist source, TheAudioDB — the one that
	// also matches by NAME (so artists MusicBrainz didn't MBID-match still get an
	// image) and that carries a real biography (preferred over MusicBrainz's
	// synthesized stub). Empty by default: with no key TheAudioDB is not wired and
	// Music enrichment makes zero calls to it (ADR-0001 explicit opt-in). Env:
	// JUICEBOX_THEAUDIODB_API_KEY.
	TheAudioDBAPIKey string
	// TheAudioDBBaseURL overrides the TheAudioDB API host. It defaults to the public
	// endpoint; tests point it at a local server so the chain is exercised with no
	// real network (mirrors FanartTVBaseURL). Env:
	// JUICEBOX_THEAUDIODB_BASE_URL.
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
	// admin settings UI is authoritative. Env: JUICEBOX_OPENSUBTITLES_API_KEY.
	OpenSubtitlesAPIKey string
	// OpenSubtitlesBaseURL overrides the OpenSubtitles API host. It defaults to the
	// public endpoint; tests point it at a local stub so the fetch flow is exercised
	// with no real network (mirrors TMDBBaseURL). Env:
	// JUICEBOX_OPENSUBTITLES_BASE_URL.
	OpenSubtitlesBaseURL string
	// SubtitleAutoFetchLang is the ISO-639-1 language a completed scan auto-fetches
	// subtitles for. Empty by default (OFF) — OpenSubtitles' small download quota
	// makes bulk auto-fetch a footgun, so it is strictly opt-in (ADR-0021). Seeds the
	// DB knob on first boot. Env: JUICEBOX_SUBTITLE_AUTO_FETCH_LANG.
	SubtitleAutoFetchLang string
}

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

// Defaults returns a Config populated with sensible defaults. Callers may
// override fields before calling Validate.
func Defaults() Config {
	return Config{
		ListenAddr:               ":8080",
		DataDir:                  "./data",
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

// SubtitleCacheDir returns the durable on-disk cache for externally fetched
// Subtitle tracks, under the data dir (ADR-0007/0021), a sibling of the artwork
// cache. Like it, this is durable cache — never written into library folders and
// not cleared at boot, so a fetched subtitle survives restarts and rescans.
func (c Config) SubtitleCacheDir() string {
	return filepath.Join(c.DataDir, "subtitles")
}

// FromEnv builds a Config from defaults overlaid with environment variables:
//
//	JUICEBOX_LISTEN_ADDR    -> ListenAddr
//	JUICEBOX_DATA_DIR       -> DataDir
//	JUICEBOX_SCAN_INTERVAL  -> ScanInterval (a Go duration, e.g. "30m";
//	                               "0" disables the scheduled scan)
//	JUICEBOX_SESSION_IDLE_TIMEOUT -> SessionIdleTimeout (a Go duration, e.g.
//	                               "2m"; "0" disables the session reaper)
//	JUICEBOX_MAX_CONCURRENT_TRANSCODES -> MaxConcurrentTranscodes (an integer;
//	                               "0" or negative disables the cap / unlimited)
//	JUICEBOX_HARDWARE_ACCEL -> HardwareAccel (an enum: off|auto|nvenc|vaapi|
//	                               qsv|videotoolbox; off/false/0 → off, the legacy
//	                               true/1 → auto; default off → CPU libx264 path)
//	JUICEBOX_MUSICBRAINZ_ENABLED -> MusicBrainzEnabled (a bool; default false —
//	                               turns on Music enrichment without a TMDB key)
//	JUICEBOX_MUSICBRAINZ_BASE_URL -> MusicBrainzBaseURL (the MusicBrainz host;
//	                               point at a mirror, default is the public host)
//	JUICEBOX_MUSICBRAINZ_RATE_LIMIT -> MusicBrainzRateLimit (a Go duration, e.g.
//	                               "1s"; "0" disables throttling for a mirror with
//	                               no rate policy; default DefaultMusicBrainzRateLimit)
//	JUICEBOX_FANART_TV_API_KEY -> FanartTVAPIKey (enables artist imagery from
//	                               fanart.tv; empty = off, no fanart.tv calls)
//	JUICEBOX_THEAUDIODB_API_KEY -> TheAudioDBAPIKey (enables the name-matching
//	                               artist image + biography source; empty = off,
//	                               no TheAudioDB calls)
//	JUICEBOX_AUTO_ENRICH    -> AutoEnrichAfterScan (a bool; default true)
//	JUICEBOX_ENRICH_INTERVAL -> EnrichInterval (a Go duration, e.g. "6h";
//	                               "0" disables the scheduled enrich)
//
// An unparseable scan-interval falls back to the default rather than failing
// boot (the scheduled scan is a safety net, not a critical path); the same
// lenient policy applies to the transcode-cap and HW-accel knobs.
func FromEnv() Config {
	c := Defaults()
	if v := os.Getenv("JUICEBOX_LISTEN_ADDR"); v != "" {
		c.ListenAddr = v
	}
	if v := os.Getenv("JUICEBOX_DATA_DIR"); v != "" {
		c.DataDir = v
	}
	if v := os.Getenv("JUICEBOX_SCAN_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			c.ScanInterval = d
		}
	}
	if v := os.Getenv("JUICEBOX_SESSION_IDLE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			c.SessionIdleTimeout = d
		}
	}
	if v := os.Getenv("JUICEBOX_MAX_CONCURRENT_TRANSCODES"); v != "" {
		// A parseable integer overrides the default; 0/negative means unlimited.
		// An unparseable value keeps the default rather than failing boot.
		if n, err := strconv.Atoi(v); err == nil {
			c.MaxConcurrentTranscodes = n
		}
	}
	if v := os.Getenv("JUICEBOX_HARDWARE_ACCEL"); v != "" {
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
	if v := os.Getenv("JUICEBOX_TMDB_API_KEY"); v != "" {
		c.TMDBAPIKey = v
	}
	if v := os.Getenv("JUICEBOX_METADATA_LANGUAGE"); v != "" {
		c.MetadataLanguage = v
	}
	if v := os.Getenv("JUICEBOX_TMDB_BASE_URL"); v != "" {
		c.TMDBBaseURL = v
	}
	if v := os.Getenv("JUICEBOX_TMDB_IMAGE_BASE_URL"); v != "" {
		c.TMDBImageBaseURL = v
	}
	if v := os.Getenv("JUICEBOX_MUSICBRAINZ_BASE_URL"); v != "" {
		c.MusicBrainzBaseURL = v
	}
	if v := os.Getenv("JUICEBOX_COVERART_BASE_URL"); v != "" {
		c.CoverArtBaseURL = v
	}
	// MusicBrainzRateLimit: a Go duration ("0" disables throttling, for a mirror
	// with no rate policy). An unparseable value keeps the default rather than
	// failing boot (throttling is a politeness knob, not a critical path).
	if v := os.Getenv("JUICEBOX_MUSICBRAINZ_RATE_LIMIT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			c.MusicBrainzRateLimit = d
		}
	}
	if v := os.Getenv("JUICEBOX_FANART_TV_API_KEY"); v != "" {
		c.FanartTVAPIKey = v
	}
	if v := os.Getenv("JUICEBOX_FANART_TV_BASE_URL"); v != "" {
		c.FanartTVBaseURL = v
	}
	if v := os.Getenv("JUICEBOX_THEAUDIODB_API_KEY"); v != "" {
		c.TheAudioDBAPIKey = v
	}
	if v := os.Getenv("JUICEBOX_THEAUDIODB_BASE_URL"); v != "" {
		c.TheAudioDBBaseURL = v
	}
	// MusicBrainzEnabled: opt Music enrichment in without a TMDB key. Off by
	// default; only a clearly-true value flips it on (an unparseable value leaves
	// the offline-safe default).
	if v := os.Getenv("JUICEBOX_MUSICBRAINZ_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.MusicBrainzEnabled = b
		}
	}
	// AutoEnrichAfterScan defaults ON; only a clearly-false value turns it off (an
	// unparseable value leaves the default).
	if v := os.Getenv("JUICEBOX_AUTO_ENRICH"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.AutoEnrichAfterScan = b
		}
	}
	// EnrichInterval: a Go duration ("0" disables the scheduled enrich). An
	// unparseable value keeps the default (the sweep is a safety net, not critical).
	if v := os.Getenv("JUICEBOX_ENRICH_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			c.EnrichInterval = d
		}
	}
	// ArtworkCandidateCacheTTL: a Go duration ("0" disables the picker's candidate
	// cache). An unparseable value keeps the default (a pure performance knob).
	if v := os.Getenv("JUICEBOX_ARTWORK_CANDIDATE_CACHE_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			c.ArtworkCandidateCacheTTL = d
		}
	}
	// External subtitle fetching (ADR-0021): these only SEED the DB-backed
	// subtitle-provider settings on first boot; thereafter the admin UI is
	// authoritative. Empty means "no provider configured" (zero outbound calls).
	if v := os.Getenv("JUICEBOX_OPENSUBTITLES_API_KEY"); v != "" {
		c.OpenSubtitlesAPIKey = v
	}
	if v := os.Getenv("JUICEBOX_OPENSUBTITLES_BASE_URL"); v != "" {
		c.OpenSubtitlesBaseURL = v
	}
	if v := os.Getenv("JUICEBOX_SUBTITLE_AUTO_FETCH_LANG"); v != "" {
		c.SubtitleAutoFetchLang = v
	}
	return c
}

// Validate checks the configuration for obvious mistakes. It does not touch
// the filesystem; see EnsureDataDir for that.
func (c Config) Validate() error {
	if c.ListenAddr == "" {
		return fmt.Errorf("config: ListenAddr must not be empty")
	}
	if c.DataDir == "" {
		return fmt.Errorf("config: DataDir must not be empty")
	}
	return nil
}

// DBPath returns the absolute path to the SQLite database file inside the
// data directory.
func (c Config) DBPath() string {
	return filepath.Join(c.DataDir, "juicebox.db")
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
