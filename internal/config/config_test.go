package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marioquake/juicebox/internal/config"
)

// TestScanIntervalFromEnv: the scheduled-scan cadence is env-overridable and a
// sensible default; "0" disables it; garbage falls back to the default.
func TestScanIntervalFromEnv(t *testing.T) {
	if d := config.Defaults().ScanInterval; d != config.DefaultScanInterval {
		t.Errorf("default scan interval = %v, want %v", d, config.DefaultScanInterval)
	}

	t.Setenv("JUICEBOX_SCAN_INTERVAL", "30m")
	if d := config.FromEnv().ScanInterval; d != 30*time.Minute {
		t.Errorf("env scan interval = %v, want 30m", d)
	}

	t.Setenv("JUICEBOX_SCAN_INTERVAL", "0")
	if d := config.FromEnv().ScanInterval; d != 0 {
		t.Errorf("scan interval = %v, want 0 (disabled)", d)
	}

	t.Setenv("JUICEBOX_SCAN_INTERVAL", "not-a-duration")
	if d := config.FromEnv().ScanInterval; d != config.DefaultScanInterval {
		t.Errorf("garbage scan interval = %v, want default %v", d, config.DefaultScanInterval)
	}
}

// TestTranscodeCapFromEnv: the concurrent-transcode cap (ADR-0009) has a sensible
// default and is env-overridable; "0" disables it (unlimited); garbage falls back
// to the default.
func TestTranscodeCapFromEnv(t *testing.T) {
	if n := config.Defaults().MaxConcurrentTranscodes; n != config.DefaultMaxConcurrentTranscodes {
		t.Errorf("default cap = %d, want %d", n, config.DefaultMaxConcurrentTranscodes)
	}

	t.Setenv("JUICEBOX_MAX_CONCURRENT_TRANSCODES", "1")
	if n := config.FromEnv().MaxConcurrentTranscodes; n != 1 {
		t.Errorf("env cap = %d, want 1", n)
	}

	t.Setenv("JUICEBOX_MAX_CONCURRENT_TRANSCODES", "0")
	if n := config.FromEnv().MaxConcurrentTranscodes; n != 0 {
		t.Errorf("cap = %d, want 0 (unlimited)", n)
	}

	t.Setenv("JUICEBOX_MAX_CONCURRENT_TRANSCODES", "not-an-int")
	if n := config.FromEnv().MaxConcurrentTranscodes; n != config.DefaultMaxConcurrentTranscodes {
		t.Errorf("garbage cap = %d, want default %d", n, config.DefaultMaxConcurrentTranscodes)
	}
}

// TestHardwareAccelFromEnv: the widened HW-accel knob (ADR-0009) defaults OFF,
// parses each explicit backend name, accepts off/false/0 → off, keeps the legacy
// bool true → auto (back-compat), and leaves garbage off (the safe CPU path).
func TestHardwareAccelFromEnv(t *testing.T) {
	if d := config.Defaults().HardwareAccel; d != config.HWAccelOff {
		t.Errorf("HardwareAccel default = %q, want %q (CPU path by default)", d, config.HWAccelOff)
	}

	// Explicit backend names parse to themselves.
	for _, tc := range []struct {
		env  string
		want config.HWAccel
	}{
		{"off", config.HWAccelOff},
		{"auto", config.HWAccelAuto},
		{"nvenc", config.HWAccelNVENC},
		{"vaapi", config.HWAccelVAAPI},
		{"qsv", config.HWAccelQSV},
		{"videotoolbox", config.HWAccelVideoToolbox},
		{"VideoToolbox", config.HWAccelVideoToolbox}, // case-insensitive
		// off/false/0/no → off (the full "turn it off" vocabulary).
		{"false", config.HWAccelOff},
		{"0", config.HWAccelOff},
		{"no", config.HWAccelOff},
		// Legacy bool true → auto (back-compat: old "true turns HW on"); on/yes too.
		{"true", config.HWAccelAuto},
		{"1", config.HWAccelAuto},
		{"on", config.HWAccelAuto},
		{"yes", config.HWAccelAuto},
		// Surrounding whitespace is trimmed before matching.
		{"  videotoolbox  ", config.HWAccelVideoToolbox},
		{" auto ", config.HWAccelAuto},
	} {
		t.Setenv("JUICEBOX_HARDWARE_ACCEL", tc.env)
		if got := config.FromEnv().HardwareAccel; got != tc.want {
			t.Errorf("env HARDWARE_ACCEL=%q → %q, want %q", tc.env, got, tc.want)
		}
	}

	// Garbage leaves the default off.
	t.Setenv("JUICEBOX_HARDWARE_ACCEL", "not-a-backend")
	if got := config.FromEnv().HardwareAccel; got != config.HWAccelOff {
		t.Errorf("garbage HardwareAccel = %q, want %q (stays off)", got, config.HWAccelOff)
	}
}

func TestEnsureDataDirCreatesMissing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "data")
	c := config.Defaults()
	c.DataDir = dir

	if err := c.EnsureDataDir(); err != nil {
		t.Fatalf("EnsureDataDir: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		t.Fatalf("data dir not created: stat err=%v", err)
	}
}

func TestEnsureDataDirRejectsNonDir(t *testing.T) {
	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := config.Defaults()
	c.DataDir = file

	if err := c.EnsureDataDir(); err == nil {
		t.Fatalf("expected clear error when data dir path is a file, got nil")
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*config.Config)
		wantErr bool
	}{
		{"defaults ok", func(*config.Config) {}, false},
		{"empty listen addr", func(c *config.Config) { c.ListenAddr = "" }, true},
		{"empty data dir", func(c *config.Config) { c.DataDir = "" }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := config.Defaults()
			tc.mutate(&c)
			err := c.Validate()
			if tc.wantErr != (err != nil) {
				t.Fatalf("Validate err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// TestEnrichmentConfig: enrichment is OFF until a provider key is set, the
// language/base-URL knobs are env-overridable, and the artwork cache lives under
// the data dir (ADR-0007, external-metadata-enrichment).
func TestEnrichmentConfig(t *testing.T) {
	if config.Defaults().EnrichmentEnabled() {
		t.Error("enrichment enabled by default; want disabled until a key is set")
	}
	if got := config.Defaults().MetadataLanguage; got != config.DefaultMetadataLanguage {
		t.Errorf("default metadata language = %q, want %q", got, config.DefaultMetadataLanguage)
	}

	t.Setenv("JUICEBOX_TMDB_API_KEY", "secret")
	t.Setenv("JUICEBOX_METADATA_LANGUAGE", "fr-FR")
	t.Setenv("JUICEBOX_TMDB_BASE_URL", "http://stub/3")
	c := config.FromEnv()
	if !c.EnrichmentEnabled() {
		t.Error("enrichment not enabled after setting a key")
	}
	if c.MetadataLanguage != "fr-FR" {
		t.Errorf("metadata language = %q, want fr-FR", c.MetadataLanguage)
	}
	if c.TMDBBaseURL != "http://stub/3" {
		t.Errorf("tmdb base url = %q, want the override", c.TMDBBaseURL)
	}

	c.DataDir = "/data"
	if c.ArtworkCacheDir() != "/data/artwork" {
		t.Errorf("artwork cache dir = %q, want /data/artwork", c.ArtworkCacheDir())
	}
}

// TestMusicEnrichmentWithoutKey: Music enrichment can be turned on without a TMDB
// key via JUICEBOX_MUSICBRAINZ_ENABLED (MusicBrainz + Cover Art Archive need
// none), while video stays off until a key is set. A fresh install enables neither.
func TestMusicEnrichmentWithoutKey(t *testing.T) {
	def := config.Defaults()
	if def.VideoEnrichmentEnabled() || def.MusicEnrichmentEnabled() || def.EnrichmentEnabled() {
		t.Errorf("defaults enable enrichment; want all off: video=%v music=%v any=%v",
			def.VideoEnrichmentEnabled(), def.MusicEnrichmentEnabled(), def.EnrichmentEnabled())
	}

	// Music opt-in, no TMDB key: music on, video off, master switch on.
	t.Setenv("JUICEBOX_MUSICBRAINZ_ENABLED", "true")
	c := config.FromEnv()
	if !c.MusicEnrichmentEnabled() {
		t.Error("music enrichment off with MUSICBRAINZ_ENABLED=true and no key")
	}
	if c.VideoEnrichmentEnabled() {
		t.Error("video enrichment on without a TMDB key")
	}
	if !c.EnrichmentEnabled() {
		t.Error("master switch off while music is enabled")
	}

	// A TMDB key alone still turns on every kind (backward compatible).
	t.Setenv("JUICEBOX_MUSICBRAINZ_ENABLED", "")
	t.Setenv("JUICEBOX_TMDB_API_KEY", "secret")
	c = config.FromEnv()
	if !c.VideoEnrichmentEnabled() || !c.MusicEnrichmentEnabled() {
		t.Errorf("TMDB key did not enable both kinds: video=%v music=%v",
			c.VideoEnrichmentEnabled(), c.MusicEnrichmentEnabled())
	}
}

// TestMusicBrainzServerConfig: the MusicBrainz host and its request rate limit are
// env-overridable so an operator can point at a mirror with its own throttling
// policy. The base URL defaults to the public host; the rate limit defaults to
// DefaultMusicBrainzRateLimit and a "0" value disables throttling entirely. An
// unparseable rate limit keeps the safe default rather than failing boot.
func TestMusicBrainzServerConfig(t *testing.T) {
	d := config.Defaults()
	if d.MusicBrainzBaseURL != config.DefaultMusicBrainzBaseURL {
		t.Errorf("default MusicBrainz base URL = %q, want %q", d.MusicBrainzBaseURL, config.DefaultMusicBrainzBaseURL)
	}
	if d.MusicBrainzRateLimit != config.DefaultMusicBrainzRateLimit {
		t.Errorf("default MusicBrainz rate limit = %v, want %v", d.MusicBrainzRateLimit, config.DefaultMusicBrainzRateLimit)
	}

	// Point at a mirror and relax its throttle.
	t.Setenv("JUICEBOX_MUSICBRAINZ_BASE_URL", "https://mirror.example/ws/2")
	t.Setenv("JUICEBOX_MUSICBRAINZ_RATE_LIMIT", "200ms")
	c := config.FromEnv()
	if c.MusicBrainzBaseURL != "https://mirror.example/ws/2" {
		t.Errorf("MusicBrainz base URL not overridden: got %q", c.MusicBrainzBaseURL)
	}
	if c.MusicBrainzRateLimit != 200*time.Millisecond {
		t.Errorf("MusicBrainz rate limit = %v, want 200ms", c.MusicBrainzRateLimit)
	}

	// "0" disables throttling on a self-hosted mirror with no rate policy.
	t.Setenv("JUICEBOX_MUSICBRAINZ_RATE_LIMIT", "0")
	if got := config.FromEnv().MusicBrainzRateLimit; got != 0 {
		t.Errorf("MusicBrainz rate limit = %v, want 0 (no throttling)", got)
	}

	// An unparseable value keeps the safe default.
	t.Setenv("JUICEBOX_MUSICBRAINZ_RATE_LIMIT", "not-a-duration")
	if got := config.FromEnv().MusicBrainzRateLimit; got != config.DefaultMusicBrainzRateLimit {
		t.Errorf("MusicBrainz rate limit = %v, want default %v on garbage input", got, config.DefaultMusicBrainzRateLimit)
	}
}

// TestEnrichTriggerConfig: auto-after-scan is ON by default and the scheduled-
// enrich interval defaults to DefaultEnrichInterval; both are env-overridable and
// a "0" interval disables the sweep (external-metadata-enrichment issue 02).
func TestEnrichTriggerConfig(t *testing.T) {
	d := config.Defaults()
	if !d.AutoEnrichAfterScan {
		t.Error("auto-enrich-after-scan disabled by default; want enabled in production")
	}
	if d.EnrichInterval != config.DefaultEnrichInterval {
		t.Errorf("default enrich interval = %v, want %v", d.EnrichInterval, config.DefaultEnrichInterval)
	}

	t.Setenv("JUICEBOX_AUTO_ENRICH", "false")
	t.Setenv("JUICEBOX_ENRICH_INTERVAL", "0")
	c := config.FromEnv()
	if c.AutoEnrichAfterScan {
		t.Error("auto-enrich not disabled by JUICEBOX_AUTO_ENRICH=false")
	}
	if c.EnrichInterval != 0 {
		t.Errorf("enrich interval = %v, want 0 (disabled)", c.EnrichInterval)
	}

	t.Setenv("JUICEBOX_AUTO_ENRICH", "not-a-bool")
	t.Setenv("JUICEBOX_ENRICH_INTERVAL", "45m")
	c = config.FromEnv()
	if !c.AutoEnrichAfterScan {
		t.Error("an unparseable JUICEBOX_AUTO_ENRICH should keep the default (true)")
	}
	if c.EnrichInterval != 45*time.Minute {
		t.Errorf("enrich interval = %v, want 45m", c.EnrichInterval)
	}
}
