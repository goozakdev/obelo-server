package config_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marioquake/obelo-server/internal/config"
)

// TestScanIntervalFromEnv: the scheduled-scan cadence is env-overridable and a
// sensible default; "0" disables it; garbage falls back to the default.
func TestScanIntervalFromEnv(t *testing.T) {
	if d := config.Defaults().ScanInterval; d != config.DefaultScanInterval {
		t.Errorf("default scan interval = %v, want %v", d, config.DefaultScanInterval)
	}

	t.Setenv("OBELO_SCAN_INTERVAL", "30m")
	if d := config.FromEnv().ScanInterval; d != 30*time.Minute {
		t.Errorf("env scan interval = %v, want 30m", d)
	}

	t.Setenv("OBELO_SCAN_INTERVAL", "0")
	if d := config.FromEnv().ScanInterval; d != 0 {
		t.Errorf("scan interval = %v, want 0 (disabled)", d)
	}

	t.Setenv("OBELO_SCAN_INTERVAL", "not-a-duration")
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

	t.Setenv("OBELO_MAX_CONCURRENT_TRANSCODES", "1")
	if n := config.FromEnv().MaxConcurrentTranscodes; n != 1 {
		t.Errorf("env cap = %d, want 1", n)
	}

	t.Setenv("OBELO_MAX_CONCURRENT_TRANSCODES", "0")
	if n := config.FromEnv().MaxConcurrentTranscodes; n != 0 {
		t.Errorf("cap = %d, want 0 (unlimited)", n)
	}

	t.Setenv("OBELO_MAX_CONCURRENT_TRANSCODES", "not-an-int")
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
		t.Setenv("OBELO_HARDWARE_ACCEL", tc.env)
		if got := config.FromEnv().HardwareAccel; got != tc.want {
			t.Errorf("env HARDWARE_ACCEL=%q → %q, want %q", tc.env, got, tc.want)
		}
	}

	// Garbage leaves the default off.
	t.Setenv("OBELO_HARDWARE_ACCEL", "not-a-backend")
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

	t.Setenv("OBELO_TMDB_API_KEY", "secret")
	t.Setenv("OBELO_METADATA_LANGUAGE", "fr-FR")
	t.Setenv("OBELO_TMDB_BASE_URL", "http://stub/3")
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
// key via OBELO_MUSICBRAINZ_ENABLED (MusicBrainz + Cover Art Archive need
// none), while video stays off until a key is set. A fresh install enables neither.
func TestMusicEnrichmentWithoutKey(t *testing.T) {
	def := config.Defaults()
	if def.VideoEnrichmentEnabled() || def.MusicEnrichmentEnabled() || def.EnrichmentEnabled() {
		t.Errorf("defaults enable enrichment; want all off: video=%v music=%v any=%v",
			def.VideoEnrichmentEnabled(), def.MusicEnrichmentEnabled(), def.EnrichmentEnabled())
	}

	// Music opt-in, no TMDB key: music on, video off, master switch on.
	t.Setenv("OBELO_MUSICBRAINZ_ENABLED", "true")
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
	t.Setenv("OBELO_MUSICBRAINZ_ENABLED", "")
	t.Setenv("OBELO_TMDB_API_KEY", "secret")
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
	t.Setenv("OBELO_MUSICBRAINZ_BASE_URL", "https://mirror.example/ws/2")
	t.Setenv("OBELO_MUSICBRAINZ_RATE_LIMIT", "200ms")
	c := config.FromEnv()
	if c.MusicBrainzBaseURL != "https://mirror.example/ws/2" {
		t.Errorf("MusicBrainz base URL not overridden: got %q", c.MusicBrainzBaseURL)
	}
	if c.MusicBrainzRateLimit != 200*time.Millisecond {
		t.Errorf("MusicBrainz rate limit = %v, want 200ms", c.MusicBrainzRateLimit)
	}

	// "0" disables throttling on a self-hosted mirror with no rate policy.
	t.Setenv("OBELO_MUSICBRAINZ_RATE_LIMIT", "0")
	if got := config.FromEnv().MusicBrainzRateLimit; got != 0 {
		t.Errorf("MusicBrainz rate limit = %v, want 0 (no throttling)", got)
	}

	// An unparseable value keeps the safe default.
	t.Setenv("OBELO_MUSICBRAINZ_RATE_LIMIT", "not-a-duration")
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

	t.Setenv("OBELO_AUTO_ENRICH", "false")
	t.Setenv("OBELO_ENRICH_INTERVAL", "0")
	c := config.FromEnv()
	if c.AutoEnrichAfterScan {
		t.Error("auto-enrich not disabled by OBELO_AUTO_ENRICH=false")
	}
	if c.EnrichInterval != 0 {
		t.Errorf("enrich interval = %v, want 0 (disabled)", c.EnrichInterval)
	}

	t.Setenv("OBELO_AUTO_ENRICH", "not-a-bool")
	t.Setenv("OBELO_ENRICH_INTERVAL", "45m")
	c = config.FromEnv()
	if !c.AutoEnrichAfterScan {
		t.Error("an unparseable OBELO_AUTO_ENRICH should keep the default (true)")
	}
	if c.EnrichInterval != 45*time.Minute {
		t.Errorf("enrich interval = %v, want 45m", c.EnrichInterval)
	}
}

// TestDiscoveryOverridesFromEnv: the two mDNS escape hatches (ADR-0034). Both are
// empty by default — a bare-metal install needs neither — and both exist for the
// container case, where the host's own interfaces and the kernel's default
// multicast interface are not what the responder should be using.
func TestDiscoveryOverridesFromEnv(t *testing.T) {
	if c := config.Defaults(); len(c.AdvertiseIPs) != 0 || c.MDNSInterface != "" {
		t.Fatalf("defaults advertise %v via %q, want neither set", c.AdvertiseIPs, c.MDNSInterface)
	}

	t.Setenv("OBELO_ADVERTISE_IP", " 192.168.1.50 , fd00::1 ,, ")
	t.Setenv("OBELO_MDNS_INTERFACE", " eth0 ")
	c := config.FromEnv()
	if len(c.AdvertiseIPs) != 2 || c.AdvertiseIPs[0] != "192.168.1.50" || c.AdvertiseIPs[1] != "fd00::1" {
		t.Errorf("AdvertiseIPs = %v, want the two addresses, trimmed, empties dropped", c.AdvertiseIPs)
	}
	if c.MDNSInterface != "eth0" {
		t.Errorf("MDNSInterface = %q, want %q", c.MDNSInterface, "eth0")
	}
}

// TestTLSConfigFromEnv: the native-TLS knobs (ADR-0041) are OFF by default, take
// the same off/false/0 spellings as the other knobs, and — unlike every other
// enum here — carry an unrecognized value through to Validate instead of
// swallowing it. A typo'd mode must not resolve to "plain HTTP, quietly".
func TestTLSConfigFromEnv(t *testing.T) {
	d := config.Defaults()
	if d.TLSMode != config.TLSModeOff {
		t.Errorf("default TLS mode = %q, want %q — TLS is opt-in (ADR-0041)", d.TLSMode, config.TLSModeOff)
	}
	if d.TLSListenAddr != config.DefaultTLSListenAddr {
		t.Errorf("default TLS listen addr = %q, want %q", d.TLSListenAddr, config.DefaultTLSListenAddr)
	}
	if d.TLSCertFile != "" || d.TLSKeyFile != "" {
		t.Errorf("defaults name certificate files (%q/%q), want neither set", d.TLSCertFile, d.TLSKeyFile)
	}

	t.Setenv("OBELO_TLS_MODE", " Files ")
	t.Setenv("OBELO_TLS_CERT", " /etc/obelo/fullchain.pem ")
	t.Setenv("OBELO_TLS_KEY", " /etc/obelo/privkey.pem ")
	t.Setenv("OBELO_TLS_LISTEN_ADDR", " :9443 ")
	c := config.FromEnv()
	if c.TLSMode != config.TLSModeFiles {
		t.Errorf("TLS mode = %q, want %q (case and surrounding space are tolerated)", c.TLSMode, config.TLSModeFiles)
	}
	if c.TLSCertFile != "/etc/obelo/fullchain.pem" || c.TLSKeyFile != "/etc/obelo/privkey.pem" {
		t.Errorf("cert/key = %q/%q, want the trimmed paths", c.TLSCertFile, c.TLSKeyFile)
	}
	if c.TLSListenAddr != ":9443" {
		t.Errorf("TLS listen addr = %q, want %q", c.TLSListenAddr, ":9443")
	}

	t.Setenv("OBELO_TLS_MODE", "off")
	if got := config.FromEnv().TLSMode; got != config.TLSModeOff {
		t.Errorf("TLS mode = %q, want %q", got, config.TLSModeOff)
	}

	// The load-bearing difference from OBELO_HARDWARE_ACCEL: garbage is KEPT so
	// Validate can reject it and name it. If this ever starts resolving to "off",
	// an operator who mistyped the mode gets a server that boots happily and sends
	// their password over the internet in the clear.
	t.Setenv("OBELO_TLS_MODE", "fiels")
	c = config.FromEnv()
	if c.TLSMode != config.TLSMode("fiels") {
		t.Errorf("TLS mode = %q, want the raw value kept for Validate to reject", c.TLSMode)
	}
	if err := c.Validate(); err == nil {
		t.Error("Validate accepted an unknown OBELO_TLS_MODE; want a refusal to boot")
	} else if !strings.Contains(err.Error(), "fiels") {
		t.Errorf("Validate error = %v, want it to quote the value the operator typed", err)
	}
}

// TestValidateTLSFiles: with `files` mode explicitly asked for, a certificate
// that cannot be used is a refusal to boot, not a fallback to plain HTTP
// (ADR-0041 + the issue's boot posture). Every rejection must name the offending
// path, because the operator's next action is to go and look at that file.
func TestValidateTLSFiles(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeTestKeyPair(t, dir, "obelo.test")
	_, otherKey := writeTestKeyPair(t, filepath.Join(dir, "other"), "somewhere.else")

	base := func() config.Config {
		c := config.Defaults()
		c.TLSMode = config.TLSModeFiles
		c.TLSCertFile = certPath
		c.TLSKeyFile = keyPath
		return c
	}

	if err := base().Validate(); err != nil {
		t.Fatalf("Validate rejected a good certificate pair: %v", err)
	}

	missing := filepath.Join(dir, "not-here.pem")
	cases := []struct {
		name     string
		mutate   func(*config.Config)
		wantText string
	}{
		{"cert path empty", func(c *config.Config) { c.TLSCertFile = "" }, "OBELO_TLS_CERT"},
		{"key path empty", func(c *config.Config) { c.TLSKeyFile = "" }, "OBELO_TLS_KEY"},
		{"cert missing", func(c *config.Config) { c.TLSCertFile = missing }, missing},
		{"key missing", func(c *config.Config) { c.TLSKeyFile = missing }, missing},
		// The one a stat cannot catch: two perfectly readable PEM files that are
		// simply not each other's pair.
		{"key belongs to another certificate", func(c *config.Config) { c.TLSKeyFile = otherKey }, otherKey},
		{"TLS port equals the HTTP port", func(c *config.Config) { c.TLSListenAddr = c.ListenAddr }, "ADDITIONAL listener"},
		{"TLS port empty", func(c *config.Config) { c.TLSListenAddr = "" }, "OBELO_TLS_LISTEN_ADDR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.mutate(&c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %s; want a refusal so the server does not boot believing it has TLS", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("Validate error = %v, want it to mention %q", err, tc.wantText)
			}
		})
	}

	// An unreadable certificate is the same story as a missing one. Skipped for
	// root, who can read it regardless (containers commonly run as root).
	if os.Geteuid() != 0 {
		unreadable := filepath.Join(dir, "unreadable.pem")
		if err := os.WriteFile(unreadable, []byte("irrelevant"), 0o000); err != nil {
			t.Fatalf("writing the unreadable fixture: %v", err)
		}
		c := base()
		c.TLSCertFile = unreadable
		if err := c.Validate(); err == nil {
			t.Error("Validate accepted an unreadable certificate file; want a refusal")
		} else if !strings.Contains(err.Error(), unreadable) {
			t.Errorf("Validate error = %v, want it to name %q", err, unreadable)
		}
	}
}

// TestValidateTLSOffIgnoresCertificateKnobs: with TLS off nothing binds the HTTPS
// port and nothing reads the certificate, so leftover values in those fields are
// inert. Validating them anyway would refuse to boot a perfectly good plain-HTTP
// server over a field it does not use — e.g. an operator who set
// OBELO_LISTEN_ADDR=:8443 and never turned TLS on.
func TestValidateTLSOffIgnoresCertificateKnobs(t *testing.T) {
	c := config.Defaults()
	c.TLSCertFile = "/nowhere/fullchain.pem"
	c.TLSKeyFile = "/nowhere/privkey.pem"
	c.TLSListenAddr = c.ListenAddr
	if err := c.Validate(); err != nil {
		t.Errorf("Validate = %v, want nil — the TLS fields are inert while the mode is off", err)
	}

	// The zero value is what a hand-built Config carries (tests, embedders). It
	// must mean the same thing as an explicit "off" rather than "unknown mode".
	c = config.Defaults()
	c.TLSMode = ""
	if err := c.Validate(); err != nil {
		t.Errorf("Validate = %v on the zero-value TLS mode, want nil (it means off)", err)
	}
}

// TestValidateTLSACMEReserved: `acme` is a KNOWN value that is not implemented
// yet (ADR-0041 phase 2). It must fail loudly with "not yet supported" rather
// than fall through to the unknown-mode error or, worse, to off — so the mode
// that lands later needs no renaming, and an operator who tried it early is told
// why instead of silently running plain HTTP.
func TestValidateTLSACMEReserved(t *testing.T) {
	c := config.Defaults()
	c.TLSMode = config.TLSModeACME
	err := c.Validate()
	if err == nil {
		t.Fatal("Validate accepted OBELO_TLS_MODE=acme; want it rejected until the mode exists")
	}
	if !strings.Contains(err.Error(), "not yet supported") {
		t.Errorf("Validate error = %v, want it to say the mode is not yet supported", err)
	}
	if strings.Contains(err.Error(), "unknown OBELO_TLS_MODE") {
		t.Errorf("Validate error = %v, want the reserved-mode message, not the unknown-value one", err)
	}
}

// writeTestKeyPair generates a throwaway self-signed certificate and its key into
// dir, returning the two paths. In-process with crypto/x509 rather than fixture
// files: a committed certificate expires, and a test that regenerates its own
// cannot rot.
func writeTestKeyPair(t *testing.T, dir, commonName string) (certPath, keyPath string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{commonName},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating the certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshaling the key: %v", err)
	}

	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatalf("writing the certificate: %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("writing the key: %v", err)
	}
	return certPath, keyPath
}
