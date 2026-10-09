package bundled

import (
	"path/filepath"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/plugins/signing"
)

// The boot-time assertion and an Obelo-signed upload upgrade (ADR-0069, issue 03): an
// upgraded Bundled copy keeps origin bundled with the Obelo key and a source other
// than the shipped-copy marker, and boot replaces it only with a strictly newer
// shipped version, never by downgrade.

// upgradedCopy puts a Bundled plugin on disk at diskVersion the way an upload upgrade
// leaves it, and returns the source asserting a build that ships shippedVersion.
func upgradedCopy(t *testing.T, st *assertStore, diskVersion, shippedVersion string, origin, source string) (*Source, string) {
	t.Helper()
	pub, priv := throwawayKey(t)
	supplyFake(t, shippedVersion, priv)
	st.put(fakeID, origin, diskVersion)
	row := st.rows[fakeID]
	row.Source = source
	row.SignerName, row.SignerKey, row.SignerKeyID = "Obelo", signing.EncodeKey(pub), signing.KeyID(pub)
	st.rows[fakeID] = row
	src, dir := releaseSource(t, st, pub)
	mustMkdir(t, dir)
	writeFile(t, filepath.Join(dir, plugins.ManifestFile), string(fakeManifest(diskVersion)))
	writeFile(t, filepath.Join(dir, plugins.DefaultModuleFile), "upgraded module")
	return src, dir
}

func TestBootLeavesAnUpgradedBundledCopyAloneUnlessTheShippedVersionIsStrictlyNewer(t *testing.T) {
	for _, tc := range []struct {
		name          string
		disk, shipped string
		replaced      bool
	}{
		{"a lower shipped version", "3.0.0", "2.0.0", false},
		{"an equal shipped version", "3.0.0", "3.0.0", false},
		{"an unparseable shipped version", "3.0.0", "weird", false},
		{"an unparseable installed version", "weird", "2.0.0", false},
		{"a strictly newer shipped version", "2.0.0", "3.0.0", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newAssertStore()
			src, dir := upgradedCopy(t, st, tc.disk, tc.shipped, plugins.OriginBundled, plugins.SourceUpload)

			mustAssert(t, src, fakeID)

			gotModule := readFile(t, filepath.Join(dir, plugins.DefaultModuleFile))
			if replaced := gotModule != "upgraded module"; replaced != tc.replaced {
				t.Fatalf("replaced = %v (module %q), want %v", replaced, gotModule, tc.replaced)
			}
			if tc.replaced {
				if v := versionOnDisk(t, dir); v != tc.shipped {
					t.Fatalf("version on disk = %q, want the shipped %q", v, tc.shipped)
				}
				// It is the shipped copy again, so it must read as one: the row's source
				// goes back to the marker and it is no longer an upload upgrade.
				row, _ := st.row(fakeID)
				if row.Source != SourceShipped || src.isUploadUpgrade(row) {
					t.Fatalf("source = %q (upload upgrade %v), want the shipped marker", row.Source, src.isUploadUpgrade(row))
				}
				return
			}
			if v := versionOnDisk(t, dir); v != tc.disk {
				t.Fatalf("version on disk = %q, want the upgraded copy's %q untouched", v, tc.disk)
			}
			if st.updates != 0 {
				t.Fatalf("the row was updated %d times for a copy that must be left alone", st.updates)
			}
		})
	}
}

// A copy this server installed itself, from the shipped files, is still refreshed
// when its version cannot be read: only an upgraded copy is protected from that.
func TestBootStillRefreshesAnUnreadableShippedCopy(t *testing.T) {
	st := newAssertStore()
	src, dir := upgradedCopy(t, st, "weird", "2.0.0", plugins.OriginBundled, SourceShipped)

	mustAssert(t, src, fakeID)

	if v := versionOnDisk(t, dir); v != "2.0.0" {
		t.Fatalf("version on disk = %q, want the shipped 2.0.0", v)
	}
}

// After an upload upgrade on a dev build the row is admin-owned, and boot leaves it
// alone whatever the shipped version is.
func TestBootLeavesAnAdminOwnedUpgradeAloneOnADevBuild(t *testing.T) {
	for _, shipped := range []string{"1.0.0", "9.0.0"} {
		st := newAssertStore()
		_, priv := throwawayKey(t)
		supplyFake(t, shipped, priv)
		st.put(fakeID, plugins.OriginAdmin, "5.0.0")
		src, dir := releaseSource(t, st, nil)
		mustMkdir(t, dir)
		writeFile(t, filepath.Join(dir, plugins.ManifestFile), string(fakeManifest("5.0.0")))
		writeFile(t, filepath.Join(dir, plugins.DefaultModuleFile), "admin upgrade")

		mustAssert(t, src, fakeID)

		if got := readFile(t, filepath.Join(dir, plugins.DefaultModuleFile)); got != "admin upgrade" || st.updates != 0 {
			t.Fatalf("shipped %s: module %q, %d updates; an admin-owned copy must be left alone", shipped, got, st.updates)
		}
	}
}
