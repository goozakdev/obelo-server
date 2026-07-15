package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// identityFile is the data-dir filename holding the Server identity's id. It sits
// beside the SQLite database rather than inside it deliberately: the id must be
// readable before the DB is opened (the mDNS advertiser wants it at boot), and it
// shares the DataDir durability boundary either way — so "reset the data dir", the
// documented cheapest way back to a known state, correctly mints a new identity.
const identityFile = "server-id"

// Identity is a Server's stable id plus its operator-chosen display name
// (ADR-0034). The id is machine-facing and permanent; the name is human-facing and
// freely changeable.
//
// The two are separate on purpose: renaming a server must not orphan a single
// Device token. Collapsing them into one field would couple those lifetimes and
// guarantee that bug.
type Identity struct {
	// ID is minted once, on first boot, and never changes for the life of a data
	// dir. It is what lets a client recognize the same Server after its address
	// changes — the property that makes a DHCP lease change survivable.
	ID string
	// Name is what a human sees in a discovery picker. Cosmetic; nothing keys on it.
	Name string
}

// LoadOrCreateIdentity reads the Server identity from dataDir, minting and
// persisting an id on first call. name is the operator's configured display name;
// when empty it falls back to the host's name, and then to a constant, so the
// Identity always has something printable.
//
// The id is a UUID rather than anything derived (a key hash, the MAC): derivation
// would tie identity to a rotatable secret or to hardware, and this must survive
// both.
func LoadOrCreateIdentity(dataDir, name string) (Identity, error) {
	id, err := loadOrCreateID(dataDir)
	if err != nil {
		return Identity{}, err
	}
	return Identity{ID: id, Name: resolveName(name)}, nil
}

func loadOrCreateID(dataDir string) (string, error) {
	path := filepath.Join(dataDir, identityFile)

	b, err := os.ReadFile(path)
	if err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			return id, nil
		}
		// An empty or whitespace-only file is corruption, not a valid identity.
		// Fall through and re-mint rather than advertising "".
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("server: reading identity: %w", err)
	}

	id := uuid.NewString()
	// 0600: not a secret (GET /server is unauthenticated and hands it to anyone
	// who asks), but nothing else has any business writing it.
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("server: writing identity: %w", err)
	}
	return id, nil
}

// resolveName picks the display name: the operator's value, else the hostname,
// else a constant. Never empty — a nameless server in a picker is a blank row.
func resolveName(configured string) string {
	if n := strings.TrimSpace(configured); n != "" {
		return n
	}
	if h, err := os.Hostname(); err == nil {
		// macOS hands back "Brandons-Mac.local"; the suffix is noise in a picker.
		h = strings.TrimSuffix(strings.TrimSpace(h), ".local")
		if h != "" {
			return h
		}
	}
	return "Juice Box"
}
