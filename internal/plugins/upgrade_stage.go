package plugins

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"reflect"
	"time"
)

// Preview and confirm for an upgrade (ADR-0069, .scratch/plugin-inplace-upgrade issue
// 06). An upgrade that widens what a plugin may do, removes an extension point, loses a
// stored setting, or comes from an author nobody can confirm does not apply on upload.
// Everything is checked, the module is compiled, the package is held in memory and the
// Admin is shown what would change. Any Admin may confirm it within StagedUpgradeTTL;
// confirming re-runs every check against the server as it is then, and applies exactly
// the bytes that were staged.

// StagedUpgradeTTL is how long a staged upgrade waits for its confirmation.
const StagedUpgradeTTL = 10 * time.Minute

// UpgradePreview is what an Admin is shown before confirming an upgrade.
type UpgradePreview struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Publisher is set only when a pinned key (or the Obelo key) verified the package;
	// otherwise a signed package's name is a claim, in ClaimedPublisher, beside the
	// KeyID that is the fact.
	Publisher        string `json:"publisher,omitempty"`
	ClaimedPublisher string `json:"claimedPublisher,omitempty"`
	KeyID            string `json:"keyId,omitempty"`
	// AuthorUnconfirmed is true whenever the installed copy has no recorded key to
	// continue: the screen must say the author cannot be confirmed.
	AuthorUnconfirmed      bool             `json:"authorUnconfirmed"`
	HostsAdded             []string         `json:"hostsAdded"`
	HostsRemoved           []string         `json:"hostsRemoved"`
	ExtensionPointsAdded   []string         `json:"extensionPointsAdded"`
	ExtensionPointsRemoved []string         `json:"extensionPointsRemoved"`
	SocketGrantAdded       bool             `json:"socketGrantAdded"`
	SettingsDropped        []SettingDropped `json:"settingsDropped"`
	SettingsDeleted        []string         `json:"settingsDeleted"`
}

// widensOrRemoves is true when the new version asks for more than the installed one
// had, or takes an extension point away. Hosts removed alone change nothing an Admin
// granted, so they do not count.
func (p UpgradePreview) widensOrRemoves() bool {
	return len(p.HostsAdded) > 0 || len(p.ExtensionPointsAdded) > 0 || len(p.ExtensionPointsRemoved) > 0 || p.SocketGrantAdded
}

// consentEqual is whether two previews ask the Admin to accept the same things. Who
// signed it is left out: pinning a matching key in between changes the label, not what
// is being accepted.
func (p UpgradePreview) consentEqual(q UpgradePreview) bool {
	p.Publisher, p.ClaimedPublisher, p.KeyID = "", "", ""
	q.Publisher, q.ClaimedPublisher, q.KeyID = "", "", ""
	return reflect.DeepEqual(p, q)
}

// StagedUpgrade is the answer to an upload that waits for confirmation.
type StagedUpgrade struct {
	// Staged is the token that confirms or cancels it.
	Staged    string         `json:"staged"`
	ExpiresAt time.Time      `json:"expiresAt"`
	Preview   UpgradePreview `json:"preview"`
}

// stagedUpgrade is one held package. It is memory only: it is not on disk, so it is not
// loaded, called or listed, and a restart forgets it.
type stagedUpgrade struct {
	token   string
	expires time.Time
	pkg     uploadedPackage
	digest  [sha256.Size]byte
	preview UpgradePreview
	// stagedBy is who uploaded it.
	stagedBy string
}

// packageDigest is a hash over every byte of a package, each part length-prefixed so
// bytes cannot move between parts unnoticed.
func packageDigest(p uploadedPackage) [sha256.Size]byte {
	h := sha256.New()
	for _, part := range [][]byte{p.manifestRaw, p.module, p.signatureRaw, p.icon, []byte(p.source)} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		h.Write(n[:])
		h.Write(part)
	}
	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out
}

type actorKey struct{}

// WithActor names the Admin behind a call, for the record of who staged and who
// confirmed an upgrade. It carries no authority; the API has already authorised.
func WithActor(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, actorKey{}, name)
}

func actorOf(ctx context.Context) string {
	name, _ := ctx.Value(actorKey{}).(string)
	return name
}

// stageUpgrade checks the module as an applied upgrade would, then holds the package
// and answers with the plugin as it still is plus the preview. Caller holds mu.
func (m *Manager) stageUpgrade(ctx context.Context, up uploadedPackage, plan *upgradePlan, stagedBy string) (Installed, error) {
	// The module is compiled and instantiated in a scratch directory that is removed at
	// once: a package that cannot load is refused here and never staged.
	staging, _, err := m.stage(ctx, up.man, up.manifestRaw, up.module, up.signatureRaw, up.icon)
	if staging != "" {
		_ = os.RemoveAll(staging)
	}
	if err != nil {
		return Installed{}, err
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return Installed{}, fmt.Errorf("plugins: staging an upgrade of %s: %w", up.man.ID, err)
	}
	now := m.now()
	m.purgeStaged(now)
	s := &stagedUpgrade{
		token: hex.EncodeToString(raw[:]), expires: now.Add(StagedUpgradeTTL),
		pkg: up, digest: packageDigest(up), preview: plan.preview, stagedBy: stagedBy,
	}
	m.staged[up.man.ID] = s
	view, err := m.view(ctx, up.man.ID)
	if err != nil {
		return Installed{}, err
	}
	view.Staged = &StagedUpgrade{Staged: s.token, ExpiresAt: s.expires, Preview: plan.preview}
	m.logf("obelo: plugin %s: an upgrade from %s to %s was staged by %q and waits for confirmation until %s",
		up.man.ID, plan.preview.From, plan.preview.To, stagedBy, s.expires.Format(time.RFC3339))
	return view, nil
}

// purgeStaged forgets every staged upgrade that has expired. Caller holds mu.
func (m *Manager) purgeStaged(now time.Time) {
	for id, s := range m.staged {
		if !now.Before(s.expires) {
			delete(m.staged, id)
		}
	}
}

// ConfirmUpgrade applies the upgrade staged for id under token: exactly the staged
// bytes, after every check an upload makes has been run again against the server as it
// is now. Any Admin may confirm; the API has already required one.
func (m *Manager) ConfirmUpgrade(ctx context.Context, id, token string) (Installed, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.staged[id]
	if !ok || subtle.ConstantTimeCompare([]byte(s.token), []byte(token)) != 1 {
		return Installed{}, refuse(ReasonStaged,
			"there is no staged upgrade of %s with that token: it was cancelled, replaced by a newer upload, or has expired. Upload the package again", id)
	}
	if !m.now().Before(s.expires) {
		delete(m.staged, id)
		return Installed{}, refuse(ReasonStaged,
			"the staged upgrade of %s expired after %d minutes and was discarded. Upload the package again", id, int(StagedUpgradeTTL/time.Minute))
	}
	stale := func(format string, args ...any) error {
		delete(m.staged, id)
		return refuse(ReasonStaged, format+". The staged upgrade was discarded; upload the package again", args...)
	}
	if packageDigest(s.pkg) != s.digest {
		return Installed{}, stale("the staged package of %s no longer matches what was previewed", id)
	}
	if _, err := os.Stat(m.pluginDir(id)); err != nil {
		return Installed{}, stale("%s is no longer installed", id)
	}
	current, err := readManifest(m.pluginDir(id))
	if err != nil {
		return Installed{}, stale("the installed copy of %s cannot be read (%v)", id, err)
	}
	if current.Version != s.preview.From {
		return Installed{}, stale("%s was %s when the upgrade was previewed and is %s now", id, s.preview.From, current.Version)
	}

	// Everything an upload checks, again: the publisher policy as pinned today, the
	// recorded key, the dependent state, the version order.
	man, err := decodeManifest(s.pkg.manifestRaw)
	if err != nil {
		return Installed{}, err
	}
	up := s.pkg
	up.man = man
	signedBy, err := m.checkSignature(man, up.manifestRaw, up.module, up.signatureRaw, up.icon)
	if err != nil {
		return Installed{}, err
	}
	plan, err := m.planUpgrade(ctx, up, signedBy)
	if err != nil {
		return Installed{}, err
	}
	if !plan.preview.consentEqual(s.preview) {
		return Installed{}, stale("what the upgrade of %s would change is no longer what was previewed", id)
	}

	delete(m.staged, id)
	return m.applyUpgrade(ctx, up, plan, signedBy, s.stagedBy, actorOf(ctx))
}

// CancelUpgrade discards the upgrade staged for id under token.
func (m *Manager) CancelUpgrade(ctx context.Context, id, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.staged[id]
	if !ok || subtle.ConstantTimeCompare([]byte(s.token), []byte(token)) != 1 {
		return refuse(ReasonUnknown, "there is no staged upgrade of %s with that token", id)
	}
	delete(m.staged, id)
	m.logf("obelo: plugin %s: the staged upgrade to %s was cancelled by %q", id, s.preview.To, actorOf(ctx))
	return nil
}
