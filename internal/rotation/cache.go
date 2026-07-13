package rotation

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Cache is the last successfully fetched rotation payload, persisted under the data
// dir as metadata-keys.json (ADR-0007) with the fetch time. It is what makes the
// rotation layer survive restarts and brief endpoint outages (ADR-0032 story 14):
// a boot with the endpoint unreachable reuses the last cached keys rather than
// dropping straight to the bootstrap key. FetchedAt lets the caller reason about
// staleness; V records which envelope version produced it.
type Cache struct {
	TMDB      string    `json:"tmdb"`
	Fanart    string    `json:"fanart"`
	FetchedAt time.Time `json:"fetchedAt"`
	V         int       `json:"v"`
}

// Keys projects the cached credentials as the plaintext Keys the resolver layers
// in — the persistence shape (Cache) and the wire shape (Keys) are kept distinct
// so the cache can carry metadata (fetch time, version) the resolver ignores.
func (c Cache) Keys() Keys {
	return Keys{TMDB: c.TMDB, Fanart: c.Fanart}
}

// LoadCache reads the cached rotation keys from path. A missing file returns the
// zero Cache with found=false and NO error — an install that has never fetched (or
// a wiped data dir) is a normal state that simply falls through to the bootstrap
// key, not a failure. A present-but-corrupt file is also treated as absent (logged
// by the caller) so a truncated write from a crash can't wedge boot; the next
// successful fetch overwrites it.
func LoadCache(path string) (c Cache, found bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Cache{}, false, nil
	}
	if err != nil {
		return Cache{}, false, fmt.Errorf("rotation: reading cache %q: %w", path, err)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		// Corrupt cache: treat as absent so a bad file degrades to bootstrap rather
		// than blocking. The caller logs; the next fetch rewrites it.
		return Cache{}, false, fmt.Errorf("rotation: cache %q is corrupt: %w", path, err)
	}
	return c, true, nil
}

// SaveCache writes the cache to path atomically (temp file + rename) so a crash
// mid-write can never leave a half-written metadata-keys.json that LoadCache would
// reject. The parent directory is expected to exist (the data dir, ensured at boot).
func SaveCache(path string, c Cache) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("rotation: encoding cache: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".metadata-keys-*.tmp")
	if err != nil {
		return fmt.Errorf("rotation: creating temp cache file: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail before the rename.
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("rotation: writing temp cache file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("rotation: closing temp cache file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rotation: renaming cache into place: %w", err)
	}
	return nil
}
