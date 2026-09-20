package config_test

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/config"
)

// The provider environment table and the first-boot seeding table
// (.scratch/bundled-plugins: issue 01).
//
// Ten named Config fields, ten named `if os.Getenv(...)` blocks and a ladder of
// enablement rules in the enrichment domain became two tables. The rules are
// unchanged, and these are the tests that say so — including the two that read
// oddly on purpose, because the behaviour they preserve is odd: a video key turns
// the music rows on.
//
// OBELO_COVERART_BASE_URL now fills the MUSICBRAINZ row's SECOND host
// (.scratch/bundled-plugins: issue 06). The variable, its default and its meaning
// are unchanged — it is still "where the album covers come from" — but the Cover
// Art Archive is no longer a provider of its own, so there is no `coverart` row to
// seed and no switch of its own to ride.

// TestProviderEnvTableIsTheEnvironment: every variable an operator has ever set
// still lands where it did, and the table is the only place it is named.
func TestProviderEnvTableIsTheEnvironment(t *testing.T) {
	t.Setenv("OBELO_TMDB_API_KEY", "tk")
	t.Setenv("OBELO_TMDB_BASE_URL", "http://tmdb.stub")
	t.Setenv("OBELO_TMDB_IMAGE_BASE_URL", "http://img.stub")
	t.Setenv("OBELO_MUSICBRAINZ_ENABLED", "true")
	t.Setenv("OBELO_MUSICBRAINZ_BASE_URL", "http://mb.stub")
	t.Setenv("OBELO_COVERART_BASE_URL", "http://caa.stub")
	t.Setenv("OBELO_FANART_TV_API_KEY", "fk")
	t.Setenv("OBELO_FANART_TV_BASE_URL", "http://fanart.stub")
	t.Setenv("OBELO_THEAUDIODB_API_KEY", "ak")
	t.Setenv("OBELO_THEAUDIODB_BASE_URL", "http://adb.stub")

	c := config.FromEnv()
	for _, tc := range []struct {
		what string
		got  string
		want string
	}{
		{"tmdb key", c.ProviderKey(config.ProviderTMDB), "tk"},
		{"tmdb url", c.ProviderURL(config.ProviderTMDB), "http://tmdb.stub"},
		{"tmdb image url", c.ProviderURL2(config.ProviderTMDB), "http://img.stub"},
		{"musicbrainz url", c.ProviderURL(config.ProviderMusicBrainz), "http://mb.stub"},
		// The cover-art host arrives as the MUSIC LEAD's second URL, not as a row of
		// its own (.scratch/bundled-plugins: issue 06).
		{"cover art url", c.ProviderURL2(config.ProviderMusicBrainz), "http://caa.stub"},
		{"fanart key", c.ProviderKey(config.ProviderFanartTV), "fk"},
		{"fanart url", c.ProviderURL(config.ProviderFanartTV), "http://fanart.stub"},
		{"theaudiodb key", c.ProviderKey(config.ProviderTheAudioDB), "ak"},
		{"theaudiodb url", c.ProviderURL(config.ProviderTheAudioDB), "http://adb.stub"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.what, tc.got, tc.want)
		}
	}
	if !c.ProviderEnabled(config.ProviderMusicBrainz) {
		t.Error("OBELO_MUSICBRAINZ_ENABLED=true did not opt the keyless source in")
	}
	// An unparseable opt-in leaves the offline-safe default (ADR-0001).
	t.Setenv("OBELO_MUSICBRAINZ_ENABLED", "yes-please")
	if config.FromEnv().ProviderEnabled(config.ProviderMusicBrainz) {
		t.Error("a garbage opt-in turned a source on; the default must be off")
	}
}

// TestSeedProviderRowsReproducesTheEnvRules: the enablement ladder SeedIfEmpty used
// to spell out, now one table, asserted case by case.
func TestSeedProviderRowsReproducesTheEnvRules(t *testing.T) {
	enabled := func(rows []config.ProviderSeedRow) map[string]config.ProviderSeedRow {
		out := map[string]config.ProviderSeedRow{}
		for _, r := range rows {
			out[r.Provider] = r
		}
		return out
	}

	t.Run("a video key seeds video AND the keyless music rows", func(t *testing.T) {
		c := config.Defaults()
		c.SetProviderKey(config.ProviderTMDB, "tk")
		got := enabled(c.SeedProviderRows(nil))
		for _, id := range []string{config.ProviderTMDB, config.ProviderMusicBrainz} {
			if r, ok := got[id]; !ok || !r.Enabled {
				t.Errorf("%s not seeded enabled; a video key historically turned on every kind", id)
			}
		}
		if _, ok := got[config.ProviderFanartTV]; ok {
			t.Error("fanart.tv seeded with no key of its own")
		}
		if _, ok := got[config.ProviderTheAudioDB]; ok {
			t.Error("theaudiodb seeded with no key of its own")
		}
	})

	t.Run("the music opt-in alone seeds only the music rows", func(t *testing.T) {
		c := config.Defaults()
		c.SetProviderEnabled(config.ProviderMusicBrainz, true)
		got := enabled(c.SeedProviderRows(nil))
		if _, ok := got[config.ProviderTMDB]; ok {
			t.Error("tmdb seeded with no key")
		}
		if r, ok := got[config.ProviderMusicBrainz]; !ok || !r.Enabled {
			t.Error("musicbrainz not seeded from its own opt-in")
		}
		if _, ok := got["coverart"]; ok {
			t.Error("a `coverart` row was seeded; it stopped being a provider in " +
				".scratch/bundled-plugins issue 06 and is now the music lead's second host")
		}
	})

	t.Run("an image key alone never turns music on", func(t *testing.T) {
		c := config.Defaults()
		c.SetProviderKey(config.ProviderFanartTV, "fk")
		got := enabled(c.SeedProviderRows(nil))
		if _, ok := got[config.ProviderMusicBrainz]; ok {
			t.Error("a fanart.tv key turned MusicBrainz on; only the video lead's key ever did that")
		}
		if r, ok := got[config.ProviderFanartTV]; !ok || r.APIKey != "fk" {
			t.Errorf("fanart.tv seed = %+v, want its key", r)
		}
	})

	t.Run("nothing configured seeds no provider row at all", func(t *testing.T) {
		if rows := config.Defaults().SeedProviderRows(nil); len(rows) != 0 {
			t.Errorf("seeded %+v on an unconfigured install; ADR-0001 says nothing", rows)
		}
	})

	t.Run("a resolved default key seeds and enables its row", func(t *testing.T) {
		// The ADR-0032 chain hands the rotator's/bootstrap's key in by provider id; the
		// enablement decision is made on THAT key, not on the (absent) env one.
		c := config.Defaults()
		got := enabled(c.SeedProviderRows(map[string]string{config.ProviderTMDB: "bootstrap-key"}))
		r, ok := got[config.ProviderTMDB]
		if !ok || !r.Enabled || r.APIKey != "bootstrap-key" {
			t.Errorf("tmdb seed = %+v (ok %v), want enabled with the resolved default key", r, ok)
		}
		if _, ok := got[config.ProviderMusicBrainz]; !ok {
			t.Error("the music rows did not ride the resolved video key")
		}
	})

	t.Run("host overrides travel with the row", func(t *testing.T) {
		t.Setenv("OBELO_TMDB_BASE_URL", "http://tmdb.stub")
		t.Setenv("OBELO_TMDB_IMAGE_BASE_URL", "http://img.stub")
		t.Setenv("OBELO_COVERART_BASE_URL", "http://caa.stub")
		t.Setenv("OBELO_TMDB_API_KEY", "tk")
		got := enabled(config.FromEnv().SeedProviderRows(nil))
		if r := got[config.ProviderTMDB]; r.BaseURL != "http://tmdb.stub" || r.ImageBaseURL != "http://img.stub" {
			t.Errorf("tmdb seed hosts = %+v, want both overrides captured verbatim", r)
		}
		// The operator's cover-art override rides the MUSIC row, as its image host.
		if r := got[config.ProviderMusicBrainz]; r.ImageBaseURL != "http://caa.stub" {
			t.Errorf("musicbrainz seed image base URL = %q, want the OBELO_COVERART_BASE_URL "+
				"override — that variable now fills this field", r.ImageBaseURL)
		}
	})
}
