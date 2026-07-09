package subfetch

import (
	"context"
	"testing"

	"github.com/marioquake/juicebox/internal/store"
)

// fakeManagerStore is an in-memory ManagerStore + SeedStore for driving Reload and
// SeedIfEmpty without a database.
type fakeManagerStore struct {
	rows     []store.SubtitleProviderRow
	autoLang string
	seeded   bool
}

func (s *fakeManagerStore) SubtitleProviders() ([]store.SubtitleProviderRow, error) {
	return s.rows, nil
}
func (s *fakeManagerStore) SubtitleSettingsEmpty() (bool, error) { return !s.seeded, nil }
func (s *fakeManagerStore) UpsertSubtitleProvider(u store.SubtitleProviderUpsert) error {
	s.rows = append(s.rows, store.SubtitleProviderRow{
		Slug: u.Slug, Enabled: u.Enabled, APIKey: u.APIKey, BaseURL: u.BaseURL,
	})
	s.seeded = true
	return nil
}
func (s *fakeManagerStore) SetSubtitleAutoFetchLang(lang string) error {
	s.autoLang = lang
	s.seeded = true
	return nil
}

func TestBuildProviderGatesOnEnabledAndKey(t *testing.T) {
	// No rows → disabled.
	if _, ok := BuildProvider(nil).(disabledProvider); !ok {
		t.Fatalf("no rows should yield the disabled provider")
	}
	// Enabled but no key (RequiresKey) → disabled.
	rows := []store.SubtitleProviderRow{{Slug: SlugOpenSubtitles, Enabled: true}}
	if _, ok := BuildProvider(rows).(disabledProvider); !ok {
		t.Fatalf("enabled-without-key should yield the disabled provider")
	}
	// Disabled with a key → disabled.
	rows = []store.SubtitleProviderRow{{Slug: SlugOpenSubtitles, Enabled: false, APIKey: "k"}}
	if _, ok := BuildProvider(rows).(disabledProvider); !ok {
		t.Fatalf("disabled-with-key should yield the disabled provider")
	}
	// Enabled with a key → the real provider.
	rows = []store.SubtitleProviderRow{{Slug: SlugOpenSubtitles, Enabled: true, APIKey: "k"}}
	if _, ok := BuildProvider(rows).(*OpenSubtitlesProvider); !ok {
		t.Fatalf("enabled-with-key should yield the OpenSubtitles provider")
	}
}

func TestManagerReloadHotSwaps(t *testing.T) {
	st := &fakeManagerStore{}
	svc := NewService(&fakeStore{}, t.TempDir())
	mgr := NewManager(st, svc, BuildProvider)

	// Initially disabled: Search yields nothing, no error.
	if err := mgr.Reload(context.Background()); err != nil {
		t.Fatalf("initial reload: %v", err)
	}
	cands, err := svc.Search(context.Background(), FetchRef{Title: "Dune"}, "en")
	if err != nil || cands != nil {
		t.Fatalf("disabled search = (%v, %v), want (nil, nil)", cands, err)
	}

	// Enable via settings + reload → the live provider swaps to the real one (which
	// would make a call; here we only assert it is no longer the disabled provider).
	st.rows = []store.SubtitleProviderRow{{Slug: SlugOpenSubtitles, Enabled: true, APIKey: "k"}}
	if err := mgr.Reload(context.Background()); err != nil {
		t.Fatalf("reload after enable: %v", err)
	}
	if _, ok := svc.current().(*OpenSubtitlesProvider); !ok {
		t.Fatalf("after enabling, the live provider should be OpenSubtitles, got %T", svc.current())
	}
}

func TestSeedIfEmptySeedsOnce(t *testing.T) {
	st := &fakeManagerStore{}
	seeded, err := SeedIfEmpty(st, SeedInput{OpenSubtitlesAPIKey: "key", AutoFetchLang: ""})
	if err != nil || !seeded {
		t.Fatalf("first seed = (%v, %v), want (true, nil)", seeded, err)
	}
	if len(st.rows) != 1 || !st.rows[0].Enabled || st.rows[0].APIKey != "key" {
		t.Fatalf("seed rows = %+v, want one enabled opensubtitles row", st.rows)
	}
	// A second call is a no-op (settings no longer empty).
	seeded, err = SeedIfEmpty(st, SeedInput{OpenSubtitlesAPIKey: "other"})
	if err != nil || seeded {
		t.Fatalf("second seed = (%v, %v), want (false, nil)", seeded, err)
	}
}
