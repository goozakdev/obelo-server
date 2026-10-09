package plugins

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/goozakdev/obelo-server/internal/plugins/signing"
	"github.com/goozakdev/obelo-server/internal/store"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// In-place upgrade of an Installed plugin (ADR-0069, .scratch/plugin-inplace-upgrade
// issues 03 and 06).
//
// An upload of a package whose id is already Installed is an UPGRADE when the new
// version is strictly higher semver and, where the installed copy recorded a signer's
// key, the package is signed by that key. The files are replaced where they stand, the
// row is updated, and the settings, secrets and stored data stay, because the id is the
// same plugin. Everything else is refused before a byte is written.
//
// An upgrade that changes nothing an Admin has not already accepted applies in ONE
// step. One that widens what the plugin may do, removes an extension point, loses a
// stored setting, or comes from an author nobody can confirm is STAGED instead, with a
// preview of what changes; an Admin confirms it (upgrade_stage.go) or it expires.

// Reasons an upgrade was refused. They sit beside the install reasons and are not
// ReasonDuplicate, which stays the answer for an id a Built-in holds.
const (
	// ReasonVersion: the new version is not strictly higher, or one side is not
	// semver, so there is no order to upgrade along.
	ReasonVersion = "version"
	// ReasonPublisher: the package is not signed by the key the installed copy was
	// first installed with (or, for a Bundled plugin, by the Obelo release key).
	ReasonPublisher = "publisher"
	// ReasonDependents: the new version drops an extension point that other records
	// depend on (Sign-in identities and Users, Online source grants), so applying it
	// would orphan them. Uninstalling is the way to remove the extension point.
	ReasonDependents = "dependents"
)

// UpgradeSummary is what an applied upgrade reports about itself.
type UpgradeSummary struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Publisher is set only when a pinned key (or the Obelo release key) verified the
	// package. On an unpinned server the name is a claim: ClaimedPublisher carries it,
	// beside the KeyID that is the actual fact.
	Publisher        string `json:"publisher,omitempty"`
	ClaimedPublisher string `json:"claimedPublisher,omitempty"`
	KeyID            string `json:"keyId,omitempty"`
	// Settings is what the upgrade did to the plugin's stored settings.
	Settings SettingsReport `json:"settings"`
	// StagedBy is who uploaded the package and ConfirmedBy who confirmed its preview
	// (empty for an upgrade that applied in one step). Both are the Admins' usernames
	// as the API gave them, or empty when the caller named nobody.
	StagedBy    string `json:"stagedBy,omitempty"`
	ConfirmedBy string `json:"confirmedBy,omitempty"`
}

// releaseKeyed is what a BundledSource built into a release implements: the Obelo
// release key, which every Bundled plugin is signed with. release is false on a dev
// build, and true with an empty key when a key was compiled in but cannot be read, so
// that nothing can then be verified.
type releaseKeyed interface {
	ReleaseKey() (name, publicKey string, release bool)
}

func (m *Manager) releaseKey() (name, publicKey string, release bool) {
	if r, ok := m.bundled.(releaseKeyed); ok {
		return r.ReleaseKey()
	}
	return "", "", false
}

// uploadedPackage is one package's files as they arrived, with the provenance the row
// will record.
type uploadedPackage struct {
	man                                     pluginapi.Manifest
	manifestRaw, module, signatureRaw, icon []byte
	source                                  string
}

// upgradePlan is what an upgrade would do, worked out and checked but not done.
type upgradePlan struct {
	old            pluginapi.Manifest
	rec            recordedSigner
	bundledRelease bool
	mig            settingsMigration
	preview        UpgradePreview
	// confirm is true when an Admin must see the preview before this applies.
	confirm bool
}

// upgrade is install's other half for an id whose directory already exists. Caller
// holds mu, and checkSignature (the pinned-publisher policy) has already run.
func (m *Manager) upgrade(ctx context.Context, up uploadedPackage, signedBy signer) (Installed, error) {
	plan, err := m.planUpgrade(ctx, up, signedBy)
	if err != nil {
		return Installed{}, err
	}
	if plan.confirm {
		return m.stageUpgrade(ctx, up, plan, actorOf(ctx))
	}
	return m.applyUpgrade(ctx, up, plan, signedBy, actorOf(ctx), "")
}

// planUpgrade runs every check an upgrade must pass, whether it applies at once or
// waits for a confirmation, and works out what it would change. It writes nothing.
// Caller holds mu.
func (m *Manager) planUpgrade(ctx context.Context, up uploadedPackage, signedBy signer) (*upgradePlan, error) {
	man, id := up.man, up.man.ID
	old, err := readManifest(m.pluginDir(id))
	if err != nil {
		return nil, refuse(ReasonDuplicate,
			"a plugin with the id %q is already installed and its manifest cannot be read (%v); uninstall it before installing another under the same id", id, err)
	}
	row, hasRow, err := m.rowOf(id)
	if err != nil {
		return nil, err
	}
	if !hasRow && m.store != nil {
		return nil, refuse(ReasonDuplicate,
			"a plugin with the id %q is already installed without a record of where it came from; uninstall it before installing another under the same id", id)
	}
	if err := checkUpgradeVersion(id, old.Version, man.Version); err != nil {
		return nil, err
	}
	rec, bundledRelease, unconfirmed, err := m.checkContinuity(row, hasRow, man, up.manifestRaw, up.module, up.signatureRaw, up.icon, signedBy)
	if err != nil {
		return nil, err
	}
	if err := m.checkDroppedDependents(ctx, id, old, man); err != nil {
		return nil, err
	}
	var stored []store.PluginSetting
	if m.store != nil {
		if stored, err = m.store.PluginSettings(id); err != nil {
			return nil, err
		}
	}
	mig := migrateSettings(settingsFields(old), settingsFields(man), stored)

	preview := diffManifests(old, man)
	preview.From, preview.To = old.Version, man.Version
	preview.AuthorUnconfirmed = unconfirmed
	preview.KeyID = rec.keyID
	if signedBy.Publisher != "" || bundledRelease {
		preview.Publisher = rec.name
	} else {
		preview.ClaimedPublisher = rec.name
	}
	preview.SettingsDropped, preview.SettingsDeleted = mig.report.Dropped, mig.report.Deleted
	return &upgradePlan{
		old: old, rec: rec, bundledRelease: bundledRelease, mig: mig, preview: preview,
		confirm: unconfirmed || preview.widensOrRemoves() || mig.lossy(),
	}, nil
}

// applyUpgrade swaps the files, writes the row and the settings, and rebuilds. The
// package has been through planUpgrade under the same lock. Caller holds mu.
func (m *Manager) applyUpgrade(ctx context.Context, up uploadedPackage, plan *upgradePlan, signedBy signer,
	stagedBy, confirmedBy string) (Installed, error) {
	man, id, old, rec, mig := up.man, up.man.ID, plan.old, plan.rec, plan.mig
	bundledRelease := plan.bundledRelease

	staging, staged, err := m.stage(ctx, man, up.manifestRaw, up.module, up.signatureRaw, up.icon)
	if staging != "" {
		defer os.RemoveAll(staging)
	}
	if err != nil {
		return Installed{}, err
	}

	// The old files are moved ASIDE under a dot-prefixed name the loader skips, the new
	// ones renamed into place, and the aside copy deleted only once the swap has gone
	// through. Until then a failure puts the old files back exactly as they were.
	dir := m.pluginDir(id)
	aside := filepath.Join(m.dir, fmt.Sprintf("%s%s-%d-%d", asidePrefix, id, os.Getpid(), m.staging.Add(1)))
	if err := os.Rename(dir, aside); err != nil {
		return Installed{}, fmt.Errorf("plugins: upgrading %s: %w", id, err)
	}
	if m.onAside != nil {
		m.onAside(aside)
	}
	restore := func() {
		_ = os.RemoveAll(dir)
		if err := os.Rename(aside, dir); err != nil {
			m.logf("obelo: plugin %s could not be put back after a failed upgrade: %v", id, err)
		}
	}
	if err := os.Rename(staged, dir); err != nil {
		restore()
		return Installed{}, fmt.Errorf("plugins: upgrading %s: %w", id, err)
	}
	if m.store != nil {
		origin := OriginAdmin
		if bundledRelease {
			origin = OriginBundled
		}
		if err := m.store.UpgradePlugin(store.PluginUpgrade{
			ID:         id,
			Name:       man.Name,
			Version:    man.Version,
			APIVersion: man.APIVersion,
			Provides:   providesOf(man),
			Source:     up.source,
			Origin:     origin,
			// Only a pinned key that verified THIS package may fill these; whatever the
			// previous version had is overwritten, so the API never shows it.
			Publisher: signedBy.Publisher, KeyID: signedBy.KeyID,
			SignerName: rec.name, SignerKey: rec.key, SignerKeyID: rec.keyID,
			// Only an upgrade that loses a value rewrites the settings; any other leaves
			// every row byte-identical. Same transaction as the row.
			ReplaceSettings: mig.lossy(), Settings: mig.rows,
		}); err != nil {
			restore()
			return Installed{}, err
		}
	}
	rebuildErr := m.rebuild(ctx)
	if err := os.RemoveAll(aside); err != nil {
		m.logf("obelo: plugin %s was upgraded but its previous files could not be deleted: %v", id, err)
	}
	// The Online rows cache answered under the previous build.
	if m.onChange != nil {
		m.onChange(id)
	}
	if rebuildErr != nil {
		return Installed{}, rebuildErr
	}
	summary := &UpgradeSummary{From: old.Version, To: man.Version, KeyID: rec.keyID, Settings: mig.report,
		StagedBy: stagedBy, ConfirmedBy: confirmedBy}
	if signedBy.Publisher != "" || bundledRelease {
		summary.Publisher = rec.name
		m.logf("obelo: plugin %s was upgraded from %s to %s (signed by %q, key id %s) from %s",
			id, old.Version, man.Version, rec.name, rec.keyID, up.source)
	} else {
		summary.ClaimedPublisher = rec.name
		m.logf("obelo: plugin %s was upgraded from %s to %s (signed with key id %s, which claims to be %q) from %s",
			id, old.Version, man.Version, rec.keyID, rec.name, up.source)
	}
	if stagedBy != "" || confirmedBy != "" {
		m.logf("obelo: plugin %s: the upgrade to %s was uploaded by %q and confirmed by %q", id, man.Version, stagedBy, confirmedBy)
	}
	// Keys and reasons only: a secret's value is never logged.
	m.logf("obelo: plugin %s: settings on upgrade: %d kept, %d added, dropped %v, deleted %v, %d added need a value",
		id, len(mig.report.Kept), len(mig.report.Added), droppedKeys(mig.report.Dropped), mig.report.Deleted, len(mig.report.NeedsValue))
	view, err := m.view(ctx, id)
	if err != nil {
		return Installed{}, err
	}
	view.Upgrade = summary
	return view, nil
}

// asidePrefix names the directory an upgrade moves the old files to while it swaps.
// It is dot-prefixed so the loader skips it, and RecoverUpgrades finds it by it.
const asidePrefix = ".upgrade-"

// RecoverUpgrades finishes or undoes an upgrade a crash interrupted, and runs at boot
// BEFORE anything reads the plugins directory. An upgrade moves the old files aside,
// renames the new ones in, writes the row and then deletes the aside copy, so a crash
// can leave:
//
//   - the aside copy and NO plugin directory (killed between the renames): the old
//     files are put back;
//   - the aside copy beside a directory whose version is the row's: the upgrade went
//     through, and the aside copy is deleted;
//   - the aside copy beside a directory whose version is NOT the row's (killed before
//     the row was written): the new files stay only if they verify under the key the
//     row recorded, and the row is brought up to them; otherwise the old files are put
//     back. Either way the line says so, because the disk and the database disagreed.
//
// It never stops a boot: every failure is a logged line.
func RecoverUpgrades(dir string, st ManagerStore, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), asidePrefix) {
			continue
		}
		id, ok := asideID(e.Name())
		if !ok {
			continue
		}
		recoverOne(dir, id, filepath.Join(dir, e.Name()), st, logf)
	}
}

// asideID is the plugin id in ".upgrade-<id>-<pid>-<n>".
func asideID(name string) (string, bool) {
	rest := strings.TrimPrefix(name, asidePrefix)
	for i := 0; i < 2; i++ {
		cut := strings.LastIndex(rest, "-")
		if cut <= 0 {
			return "", false
		}
		rest = rest[:cut]
	}
	return rest, true
}

func recoverOne(dir, id, aside string, st ManagerStore, logf func(string, ...any)) {
	live := filepath.Join(dir, id)
	keep := func(why string) {
		logf("obelo: plugin %s: leftover files of an interrupted upgrade were LEFT IN PLACE at %s: %s", id, aside, why)
	}
	if st == nil {
		keep("there is no database to check them against")
		return
	}
	rows, err := st.Plugins()
	if err != nil {
		keep(fmt.Sprintf("the plugin rows could not be read: %v", err))
		return
	}
	var row store.PluginRow
	found := false
	for _, r := range rows {
		if r.ID == id {
			row, found = r, true
		}
	}
	if !found {
		// Nothing says these files were ever installed here, so they are not promoted.
		keep("the plugin has no row")
		return
	}
	// The aside copy is the old version. It comes back only if it is exactly what the
	// row describes and, when the row recorded a key, verifies under it.
	asideOK := func() (bool, string) {
		man, err := readManifest(aside)
		switch {
		case err != nil:
			return false, fmt.Sprintf("its manifest cannot be read: %v", err)
		case man.Version != row.Version:
			return false, fmt.Sprintf("its version is %s and the database says %s", man.Version, row.Version)
		case row.SignerKey != "" && !verifiesOnDisk(aside, man, row.SignerKey):
			return false, "its files do not verify under the key the plugin was installed with"
		}
		return true, ""
	}
	restore := func(why string) {
		if ok, bad := asideOK(); !ok {
			keep(bad)
			return
		}
		if _, err := os.Stat(live); err == nil {
			if err := os.RemoveAll(live); err != nil {
				keep(fmt.Sprintf("the new files could not be removed: %v", err))
				return
			}
		}
		if err := os.Rename(aside, live); err != nil {
			logf("obelo: plugin %s: an interrupted upgrade could not be undone (%s): %v", id, why, err)
			return
		}
		logf("obelo: plugin %s: an interrupted upgrade was undone (%s); the previous version is back", id, why)
	}
	drop := func() {
		if err := os.RemoveAll(aside); err != nil {
			logf("obelo: plugin %s: the leftover files of an upgrade could not be deleted: %v", id, err)
		}
	}
	if _, err := os.Stat(live); err != nil {
		restore("the new files never arrived")
		return
	}
	man, err := readManifest(live)
	if err == nil && man.Version == row.Version {
		drop()
		return
	}
	if err != nil {
		restore("the new manifest cannot be read")
		return
	}
	logf("obelo: plugin %s: the files on disk are version %s but the database says %s, after an interrupted upgrade", id, man.Version, row.Version)
	if row.SignerKey == "" || !verifiesOnDisk(live, man, row.SignerKey) {
		restore("the new files do not verify under the key the plugin was installed with")
		return
	}
	// The settings are migrated exactly as a normal upgrade migrates them, which needs
	// the version they were stored under: the aside copy. Without it nothing says what
	// the Admin entered under which definition, so the new files are not adopted.
	oldMan, err := readManifest(aside)
	if err != nil || oldMan.Version != row.Version {
		restore("the previous version is not there to migrate the settings from")
		return
	}
	stored, err := st.PluginSettings(id)
	if err != nil {
		restore(fmt.Sprintf("the stored settings could not be read: %v", err))
		return
	}
	mig := migrateSettings(settingsFields(oldMan), settingsFields(man), stored)
	// Written as a normal upgrade writes it: the pinned columns only when a pinned key
	// verifies these files, the recorded signer kept, the source an upload.
	up := store.PluginUpgrade{
		ID: id, Name: man.Name, Version: man.Version, APIVersion: man.APIVersion, Provides: providesOf(man),
		Source: SourceUpload, Origin: row.Origin,
		SignerName: row.SignerName, SignerKey: row.SignerKey, SignerKeyID: row.SignerKeyID,
		ReplaceSettings: mig.lossy(), Settings: mig.rows,
	}
	up.Publisher, up.KeyID = pinnedOnDisk(live, man, st)
	if err := st.UpgradePlugin(up); err != nil {
		restore(fmt.Sprintf("the database could not be brought up to the new files: %v", err))
		return
	}
	logf("obelo: plugin %s: the interrupted upgrade to %s was finished; the files verify under the recorded key", id, man.Version)
	drop()
}

// pinnedOnDisk is the pinned publisher and key id that verify the plugin directory's
// files, or empty when no pinned key does — what an upgrade records in the pinned
// columns.
func pinnedOnDisk(dir string, man pluginapi.Manifest, st ManagerStore) (publisher, keyID string) {
	raw, err := os.ReadFile(filepath.Join(dir, pluginapi.SignatureFile))
	if err != nil {
		return "", ""
	}
	sig, err := signing.Parse(raw)
	if err != nil {
		return "", ""
	}
	pinned, err := st.PluginPublishers()
	if err != nil {
		return "", ""
	}
	for _, k := range pinned {
		if samePublisher(k.Publisher, sig.Publisher) && verifiesOnDisk(dir, man, k.PublicKey) {
			return k.Publisher, k.KeyID
		}
	}
	return "", ""
}

// verifiesOnDisk reports whether the plugin directory's signature file verifies its
// own manifest, module and icon under the given base64 key.
func verifiesOnDisk(dir string, man pluginapi.Manifest, key string) bool {
	pub, err := signing.ParsePublicKey(key)
	if err != nil {
		return false
	}
	manifest, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		return false
	}
	module, err := os.ReadFile(filepath.Join(dir, moduleFile(man)))
	if err != nil {
		return false
	}
	raw, err := os.ReadFile(filepath.Join(dir, pluginapi.SignatureFile))
	if err != nil {
		return false
	}
	sig, err := signing.Parse(raw)
	if err != nil {
		return false
	}
	icon, _ := os.ReadFile(filepath.Join(dir, IconFile))
	return signing.VerifyWithIcon(sig, pub, manifest, module, icon) == nil
}

// rowOf is the plugins row for an id, hasRow false when there is none.
func (m *Manager) rowOf(id string) (row store.PluginRow, hasRow bool, err error) {
	rows, err := m.rows()
	if err != nil {
		return store.PluginRow{}, false, err
	}
	for _, r := range rows {
		if r.ID == id {
			return r, true, nil
		}
	}
	return store.PluginRow{}, false, nil
}

// recordedSigner is who an upgrade continues to be signed by.
type recordedSigner struct{ name, key, keyID string }

// checkContinuity decides whether the package is signed by the key the installed copy
// is bound to. For a Bundled plugin on a release build that key is the compiled-in
// Obelo release key, whether or not the row recorded it (a row from a release before
// keys were recorded has none); otherwise it is the key recorded at first install. A
// plugin with no key to continue is never upgraded in one step: unconfirmed comes back
// true, rec is whatever the package itself was signed with (nothing if unsigned), and
// the upgrade waits for an Admin who is told the author cannot be confirmed.
// bundledRelease is true when the package verified under the Obelo key for a
// bundled-origin row, which is what lets it keep its origin.
func (m *Manager) checkContinuity(row store.PluginRow, hasRow bool, man pluginapi.Manifest, manifestRaw, module, signatureRaw, icon []byte,
	signedBy signer) (rec recordedSigner, bundledRelease, unconfirmed bool, err error) {
	rec = recordedSigner{row.SignerName, row.SignerKey, row.SignerKeyID}
	if relName, relKey, release := m.releaseKey(); hasRow && row.Origin == OriginBundled && release {
		if relKey == "" {
			return recordedSigner{}, false, false, refuse(ReasonPublisher,
				"%s ships with Obelo, and the Obelo release key compiled into this build cannot be read, so no upload can be verified against it; "+
					"uninstall the plugin and install the new version instead", man.ID)
		}
		rec, bundledRelease = recordedSigner{name: relName, key: relKey}, true
	}
	if rec.key == "" {
		// Nothing to continue: the installed copy was not signed, or was installed before
		// this server recorded signers. Anything may upgrade it, and the Admin is told so.
		return recordedSigner{signedBy.SignerName, signedBy.SignerKey, signedBy.SignerKeyID}, false, true, nil
	}
	expected, err := signing.ParsePublicKey(rec.key)
	if err != nil {
		return recordedSigner{}, false, false, refuse(ReasonPublisher,
			"the signing key recorded for %s cannot be read (%v), so an upgrade cannot be verified against it; uninstall the plugin and install the new version instead", man.ID, err)
	}
	rec.keyID = signing.KeyID(expected)

	// A name in a signature document is a CLAIM unless a pinned key verified it (the
	// row's pinned publisher column) or it is the compiled-in Obelo key: on an unpinned
	// server anyone can write any name beside their own key, so the key id is the fact.
	who := fmt.Sprintf("was installed under key id %s (claims %q)", rec.keyID, rec.name)
	if row.Publisher != "" {
		who = fmt.Sprintf("was signed by %q (key id %s) when it was installed", rec.name, rec.keyID)
	}
	if bundledRelease {
		who = fmt.Sprintf("ships with Obelo and can be upgraded only by a package signed with the Obelo release key (publisher %q, key id %s)", rec.name, rec.keyID)
	}
	tail := "A plugin keeps the key it was first installed with: there is no key rotation in this version. " +
		"To change publisher, uninstall the plugin and install the new one (its settings are not kept)."

	if len(signatureRaw) == 0 {
		return recordedSigner{}, false, false, refuse(ReasonPublisher,
			"%s %s, but this package is not signed. %s", man.ID, who, tail)
	}
	sig, err := signing.Parse(signatureRaw)
	if err != nil {
		return recordedSigner{}, false, false, refuse(ReasonPublisher,
			"%s %s, but the signature that came with this package could not be read: %v. %s", man.ID, who, err, tail)
	}
	verr := signing.VerifyWithIcon(sig, expected, manifestRaw, module, icon)
	if verr == nil {
		return rec, bundledRelease, false, nil
	}

	// Who the package says it is from. Stated as fact only when a pinned key
	// verified it; on an unpinned server the name is just what the document says.
	offered, offeredID, pinned := signedBy.SignerName, signedBy.SignerKeyID, signedBy.Publisher != ""
	if signedBy.SignerKey == "" {
		offered, offeredID = strings.TrimSpace(sig.Publisher), sig.KeyID
		if pub, perr := signing.ParsePublicKey(sig.PublicKey); perr == nil {
			offeredID = signing.KeyID(pub)
		}
	}
	if offeredID != "" && offeredID != rec.keyID {
		if pinned {
			return recordedSigner{}, false, false, refuse(ReasonPublisher,
				"%s %s, but this package is signed by %q (key id %s). %s", man.ID, who, offered, offeredID, tail)
		}
		return recordedSigner{}, false, false, refuse(ReasonPublisher,
			"%s %s, but this package is signed with key id %s (claims %q). %s", man.ID, who, offeredID, offered, tail)
	}
	if errors.Is(verr, signing.ErrDigestMismatch) {
		return recordedSigner{}, false, false, refuse(ReasonPublisher,
			"%s %s, but this package's signature covers different files than the ones that arrived — the manifest or the module has changed since it was signed. %s",
			man.ID, who, tail)
	}
	return recordedSigner{}, false, false, refuse(ReasonPublisher,
		"%s %s, but this package's signature does not verify under that key. %s", man.ID, who, tail)
}

// checkUpgradeVersion orders the installed version against the offered one. Both must
// be semantic versions and the offered one strictly greater.
func checkUpgradeVersion(id, installed, offered string) error {
	a, okA := parseSemver(installed)
	b, okB := parseSemver(offered)
	if !okA || !okB {
		return refuse(ReasonVersion,
			"%s is installed at version %q and this package is version %q; an upgrade needs both to be semantic versions like 1.2.3 so they can be ordered. "+
				"Uninstall the plugin and install the new one instead (its settings are not kept)",
			id, installed, offered)
	}
	if compareSemver(b, a) <= 0 {
		return refuse(ReasonVersion,
			"%s is installed at version %s and this package is version %s, which is not newer. An upgrade must be a higher version; "+
				"to install an older or equal one, uninstall the plugin first and install that one (its settings are not kept)",
			id, installed, offered)
	}
	return nil
}

// diffManifests is what the new manifest widens or removes against the installed one:
// the hosts, extension points and socket grant, as the preview lists them.
func diffManifests(old, next pluginapi.Manifest) UpgradePreview {
	p := UpgradePreview{HostsAdded: []string{}, HostsRemoved: []string{},
		ExtensionPointsAdded: []string{}, ExtensionPointsRemoved: []string{},
		SettingsDropped: []SettingDropped{}, SettingsDeleted: []string{}}

	hostKey := func(h string) string { return strings.ToLower(strings.TrimSpace(h)) }
	oldHosts, newHosts := map[string]bool{}, map[string]bool{}
	for _, h := range old.Network.Hosts {
		oldHosts[hostKey(h)] = true
	}
	for _, h := range next.Network.Hosts {
		if k := hostKey(h); !oldHosts[k] && !newHosts[k] {
			p.HostsAdded = append(p.HostsAdded, h)
		}
		newHosts[hostKey(h)] = true
	}
	for _, h := range old.Network.Hosts {
		if k := hostKey(h); !newHosts[k] {
			p.HostsRemoved = append(p.HostsRemoved, h)
			newHosts[k] = true
		}
	}

	oldKinds, oldSocket := map[pluginapi.ExtensionPoint]bool{}, map[pluginapi.ExtensionPoint]bool{}
	for _, e := range old.Provides {
		oldKinds[e.Kind] = true
		oldSocket[e.Kind] = oldSocket[e.Kind] || e.Socket
	}
	newKinds := map[pluginapi.ExtensionPoint]bool{}
	for _, e := range next.Provides {
		if !oldKinds[e.Kind] && !newKinds[e.Kind] {
			p.ExtensionPointsAdded = append(p.ExtensionPointsAdded, string(e.Kind))
		}
		newKinds[e.Kind] = true
		if e.Socket && !oldSocket[e.Kind] {
			p.SocketGrantAdded = true
		}
	}
	seen := map[pluginapi.ExtensionPoint]bool{}
	for _, e := range old.Provides {
		if !newKinds[e.Kind] && !seen[e.Kind] {
			p.ExtensionPointsRemoved = append(p.ExtensionPointsRemoved, string(e.Kind))
		}
		seen[e.Kind] = true
	}
	return p
}

// dependentStateStore is the part of the store that counts what other records hold
// against a plugin. A store without it has no such records, so nothing is refused.
type dependentStateStore interface {
	SignInCasualties(pluginID string, otherProviders []string) ([]store.SignInCasualty, error)
	ExternalIdentityCount(pluginID string) (int, error)
	OnlineSourceGrantCount(sourceID string) (int, error)
}

// The production store must keep satisfying it, or the dependents check would
// silently stop running.
var _ dependentStateStore = (*store.DB)(nil)

// checkDroppedDependents refuses an upgrade that drops an extension point whose
// removal would orphan stored state: External identities and the Users who sign in
// only through a Sign-in provider, Users' grants of an Online source. Dropping any
// other extension point, or one nothing depends on, passes this check. Caller holds
// mu.
func (m *Manager) checkDroppedDependents(ctx context.Context, id string, old, next pluginapi.Manifest) error {
	st, ok := m.store.(dependentStateStore)
	if !ok {
		return nil
	}
	kept := map[pluginapi.ExtensionPoint]bool{}
	for _, p := range next.Provides {
		kept[p.Kind] = true
	}
	var dropped []string
	details := map[string]any{}
	var facts []string
	for _, p := range old.Provides {
		if kept[p.Kind] {
			continue
		}
		kept[p.Kind] = true
		switch p.Kind {
		case pluginapi.ExtensionSignInProvider:
			others, _, err := m.signInProviders(ctx, id)
			if err != nil {
				return err
			}
			users, err := st.SignInCasualties(id, others)
			if err != nil {
				return err
			}
			identities, err := st.ExternalIdentityCount(id)
			if err != nil {
				return err
			}
			if identities == 0 && len(users) == 0 {
				continue
			}
			details["identities"], details["users"] = identities, len(users)
			facts = append(facts, fmt.Sprintf("%d identities and %d users who sign in only through it", identities, len(users)))
		case pluginapi.ExtensionOnlineSourceProvider:
			grants, err := st.OnlineSourceGrantCount(id)
			if err != nil {
				return err
			}
			if grants == 0 {
				continue
			}
			details["grants"] = grants
			facts = append(facts, fmt.Sprintf("%d grants", grants))
		default:
			continue
		}
		dropped = append(dropped, string(p.Kind))
	}
	if len(dropped) == 0 {
		return nil
	}
	details["extensionPoints"] = dropped
	r := refuse(ReasonDependents,
		"this version drops %s; %s depend on it; uninstall %s to remove it",
		strings.Join(dropped, " and "), strings.Join(facts, " and "), id)
	r.Details = details
	return r
}

// --- Semantic versions ---------------------------------------------------------

// semver is a parsed semantic version (semver.org): the core and the prerelease
// identifiers. Build metadata is validated and then ignored, as the spec says to.
type semver struct {
	core [3]string
	pre  []string
}

// parseSemver reads a strict semantic version: MAJOR.MINOR.PATCH with no leading
// "v", no leading zeros, an optional -prerelease and an optional +build.
func parseSemver(v string) (semver, bool) {
	if v == "" || v != strings.TrimSpace(v) {
		return semver{}, false
	}
	rest, build, hasBuild := strings.Cut(v, "+")
	if hasBuild && !validIdentifiers(build, false) {
		return semver{}, false
	}
	core, pre, hasPre := strings.Cut(rest, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	var out semver
	for i, p := range parts {
		if !numeric(p) || (len(p) > 1 && p[0] == '0') {
			return semver{}, false
		}
		out.core[i] = p
	}
	if hasPre {
		if !validIdentifiers(pre, true) {
			return semver{}, false
		}
		out.pre = strings.Split(pre, ".")
	}
	return out, true
}

func numeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// validIdentifiers checks a dot-separated run of [0-9A-Za-z-] identifiers; in a
// prerelease a numeric one may not have a leading zero.
func validIdentifiers(s string, prerelease bool) bool {
	if s == "" {
		return false
	}
	for _, id := range strings.Split(s, ".") {
		if id == "" {
			return false
		}
		for _, r := range id {
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '-') {
				return false
			}
		}
		if prerelease && numeric(id) && len(id) > 1 && id[0] == '0' {
			return false
		}
	}
	return true
}

// compareNumeric orders two digit strings without a leading zero, however long.
func compareNumeric(a, b string) int {
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

// compareSemver is semver.org's precedence: the core numerically, then a version with
// a prerelease sorts below the same core without one, then identifier by identifier
// (numbers below words, numbers by value, words by ASCII, a shorter run below a longer
// one it prefixes).
func compareSemver(a, b semver) int {
	for i := range a.core {
		if c := compareNumeric(a.core[i], b.core[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(a.pre) == 0 && len(b.pre) == 0:
		return 0
	case len(a.pre) == 0:
		return 1
	case len(b.pre) == 0:
		return -1
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		x, y := a.pre[i], b.pre[i]
		switch xn, yn := numeric(x), numeric(y); {
		case xn && yn:
			if c := compareNumeric(x, y); c != 0 {
				return c
			}
		case xn:
			return -1
		case yn:
			return 1
		default:
			if c := strings.Compare(x, y); c != 0 {
				return c
			}
		}
	}
	return len(a.pre) - len(b.pre)
}
