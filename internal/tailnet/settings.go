package tailnet

import (
	"github.com/marioquake/obelo-server/internal/discovery"
	"github.com/marioquake/obelo-server/internal/store"
)

// FallbackHostname is the MagicDNS hostname a Server joins under when nothing
// else yields a usable DNS label — no operator-supplied name, and a display name
// that sanitizes down to nothing (ADR-0043). It matches the name discovery falls
// back to on the link for the same reason: this is simply what this server is
// called when it has not been told otherwise.
const FallbackHostname = "obelo"

// SeedInput is the first-boot seed source, decoupled from config.Config (ADR-0006
// — the tailnet domain never imports config; app maps one to the other). It
// mirrors the OBELO_TAILSCALE_* knobs, plus the Server's display name, which is
// where the hostname comes from when the operator names none.
type SeedInput struct {
	// Enabled seeds OBELO_TAILSCALE_ENABLED — a headless deploy that wants the node
	// up on first boot without touching a UI.
	Enabled bool
	// Hostname seeds OBELO_TAILSCALE_HOSTNAME. Empty falls back to ServerName.
	Hostname string
	// ControlURL seeds OBELO_TAILSCALE_CONTROL_URL, empty for Tailscale's own
	// coordination server.
	ControlURL string
	// HTTPSEnabled seeds OBELO_TAILSCALE_HTTPS (tailnet :443, issue 03).
	HTTPSEnabled bool
	// ServerName is this Server's display name (ADR-0034), the source the MagicDNS
	// hostname is DERIVED FROM ONCE and then never again — see SeedHostname.
	ServerName string
}

// SeedStore is the persistence SeedIfEmpty writes through.
type SeedStore interface {
	TailnetSettingsEmpty() (bool, error)
	SetTailnetSettings(u store.TailnetSettingsUpsert) error
}

// SeedIfEmpty seeds the DB-backed Tailnet settings from config exactly once —
// only when the settings row has never been written (first boot). After seeding
// (or on any boot where the row already exists) it is a no-op: the DB is
// authoritative and the OBELO_TAILSCALE_* env vars are IGNORED at runtime, which
// is the whole point of the handoff and is documented in config.Config in the
// same words the enrichment knobs use. Returns whether it seeded.
//
// The row is always written when empty (even for the all-defaults case), because
// the row's existence IS the "already seeded" marker — the same shape
// subfetch.SeedIfEmpty uses with its auto-fetch-language singleton. Without it,
// every boot would re-consult the environment and an operator who cleared a
// value in the UI would find it back after a restart.
//
// It never touches OBELO_TAILSCALE_AUTHKEY. That is not a setting: it is read
// from the environment at join time and dropped, has no column, and is never
// logged (ADR-0043 — no long-lived Tailnet credential is stored in this database).
func SeedIfEmpty(s SeedStore, in SeedInput) (bool, error) {
	empty, err := s.TailnetSettingsEmpty()
	if err != nil {
		return false, err
	}
	if !empty {
		return false, nil
	}
	if err := s.SetTailnetSettings(store.TailnetSettingsUpsert{
		Enabled:      in.Enabled,
		Hostname:     SeedHostname(in.Hostname, in.ServerName),
		ControlURL:   in.ControlURL,
		HTTPSEnabled: in.HTTPSEnabled,
	}); err != nil {
		return false, err
	}
	return true, nil
}

// SeedHostname resolves the MagicDNS hostname to seed: the operator's chosen name
// if they gave one, else the Server's display name, each sanitized to a legal DNS
// label, falling back to FallbackHostname when nothing survives. The sanitizer is
// discovery's — the same rule that produces the `.local` name — because two
// spellings of one server's name is a bug nobody would ever think to look for.
//
// This resolution happens ONCE, at seed time, and the result is stored. The
// hostname is deliberately NOT derived live from ServerName: ServerName is
// documented as purely cosmetic (ADR-0034), and making a rename silently change
// the address every roaming client has stored is exactly the coupling ADR-0034
// refused when it split a Server's id from its name.
func SeedHostname(hostname, serverName string) string {
	if label := discovery.DNSLabel(hostname); label != "" {
		return label
	}
	if label := discovery.DNSLabel(serverName); label != "" {
		return label
	}
	return FallbackHostname
}
