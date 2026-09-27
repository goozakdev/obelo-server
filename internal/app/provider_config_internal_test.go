package app

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/config"
)

// TestBootConfigSaysWhichURLsTheOperatorSet: before the first Reload, the provider
// settings come from the boot config, and a URL the operator set there — through
// the environment or the config itself — is theirs, so it counts as entered. A URL
// the operator left at its shipped default does not.
func TestBootConfigSaysWhichURLsTheOperatorSet(t *testing.T) {
	t.Setenv("OBELO_TMDB_BASE_URL", "http://10.0.0.5:8080")
	t.Setenv("OBELO_COVERART_BASE_URL", "http://10.0.0.6:8081")
	cfg := config.FromEnv()
	cfg.SetProviderURL(config.ProviderFanartTV, "http://10.0.0.7:8082")

	got := providerConfigFromConfig(cfg).ProviderEndpoints
	for _, tc := range []struct {
		what string
		got  bool
		want bool
	}{
		{"tmdb url, from the environment", got[config.ProviderTMDB].URLEntered, true},
		{"tmdb image url, left at the default", got[config.ProviderTMDB].URL2Entered, false},
		{"musicbrainz url, left at the default", got[config.ProviderMusicBrainz].URLEntered, false},
		{"cover art url, from the environment", got[config.ProviderMusicBrainz].URL2Entered, true},
		{"fanart url, set in the config", got[config.ProviderFanartTV].URLEntered, true},
		{"theaudiodb url, left at the default", got[config.ProviderTheAudioDB].URLEntered, false},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: entered = %v, want %v", tc.what, tc.got, tc.want)
		}
	}
}
