package plugins_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/store"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The audit line of an applied upgrade (ADR-0069, .scratch/plugin-inplace-upgrade
// issue 09): one line per applied upgrade, none for one that was not applied.

const upgradedMarker = "was upgraded"

func auditFixture(t *testing.T) (*managerFixture, *fakeClock, *lockedBuf) {
	t.Helper()
	var logs lockedBuf
	clk := &fakeClock{t: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}
	f := newManagerFixtureWith(t, func(c *plugins.ManagerConfig) { c.Now = clk.Now; c.Logf = logs.logf })
	return f, clk, &logs
}

// upgradedLines are the log lines that claim an upgrade was applied.
func upgradedLines(logs *lockedBuf) []string {
	var out []string
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, upgradedMarker) {
			out = append(out, l)
		}
	}
	return out
}

func asAdmin(name string) context.Context { return plugins.WithActor(context.Background(), name) }

func TestAOneStepUpgradeWritesOneAuditLineNamingTheSameAdminTwice(t *testing.T) {
	plugins.Parallel(t)
	f, _, logs := auditFixture(t)
	_, priv := newKey(t)
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, nil))

	got, err := f.manager.InstallPackage(asAdmin("ada"), upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher, nil), plugins.SourceUpload)
	if err != nil || got.Upgrade == nil {
		t.Fatalf("expected a one-step upgrade: %+v / %v", got, err)
	}

	lines := upgradedLines(logs)
	if len(lines) != 1 {
		t.Fatalf("%d upgraded lines, want exactly 1:\n%s", len(lines), logs.String())
	}
	for _, want := range []string{"up-sink", "1.0.0 -> 1.1.0", upgradePublisher, `staged by "ada"`, `confirmed by "ada"`, "dropped []", "deleted []"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("the line lacks %q: %s", want, lines[0])
		}
	}
}

func TestAConfirmedStagedUpgradeWritesOneLineNamingBothAdminsAndTheLostKeys(t *testing.T) {
	plugins.Parallel(t)
	f, _, logs := auditFixture(t)
	_, priv := newKey(t)
	secrets := func(keys ...string) []pluginapi.SettingsField {
		var out []pluginapi.SettingsField
		for _, k := range keys {
			out = append(out, pluginapi.SettingsField{Key: k, Type: pluginapi.FieldSecret})
		}
		return out
	}
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, withFields(secrets("kept-key", "retyped-key", "removed-key")...)))
	if err := f.store.ReplacePluginSettings("up-sink", []store.PluginSetting{
		{Key: "kept-key", Value: secretValue, Secret: true},
		{Key: "removed-key", Value: secretValue, Secret: true},
		{Key: "retyped-key", Value: secretValue, Secret: true},
	}); err != nil {
		t.Fatal(err)
	}
	staged, err := f.manager.InstallPackage(asAdmin("ada"), upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher,
		withFields(secrets("kept-key")[0], pluginapi.SettingsField{Key: "retyped-key", Type: pluginapi.FieldString})), plugins.SourceUpload)
	if err != nil || staged.Staged == nil {
		t.Fatalf("expected a preview: %+v / %v", staged, err)
	}
	if n := len(upgradedLines(logs)); n != 0 {
		t.Fatalf("a staged upgrade wrote %d upgraded lines before it was confirmed:\n%s", n, logs.String())
	}

	if _, err := f.manager.ConfirmUpgrade(asAdmin("bea"), "up-sink", staged.Staged.Staged); err != nil {
		t.Fatal(err)
	}

	lines := upgradedLines(logs)
	if len(lines) != 1 {
		t.Fatalf("%d upgraded lines, want exactly 1:\n%s", len(lines), logs.String())
	}
	for _, want := range []string{"up-sink", "1.0.0 -> 1.1.0", upgradePublisher, `staged by "ada"`, `confirmed by "bea"`, "dropped [retyped-key]", "deleted [removed-key]"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("the line lacks %q: %s", want, lines[0])
		}
	}
	if strings.Contains(logs.String(), "s3cret-value-do-not-leak") {
		t.Fatalf("the log carries a secret's value:\n%s", logs.String())
	}
}

func TestAnUnconfirmedAuthorIsNamedAsSuchInTheLine(t *testing.T) {
	plugins.Parallel(t)
	f, _, logs := auditFixture(t)
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", nil, "", nil))
	s, err := f.manager.InstallPackage(asAdmin("ada"), upgradeArchive(t, "up-sink", "1.1.0", "v2", nil, "", nil), plugins.SourceUpload)
	if err != nil || s.Staged == nil {
		t.Fatalf("expected a preview: %+v / %v", s, err)
	}
	if _, err := f.manager.ConfirmUpgrade(asAdmin("ada"), "up-sink", s.Staged.Staged); err != nil {
		t.Fatal(err)
	}
	lines := upgradedLines(logs)
	if len(lines) != 1 || !strings.Contains(lines[0], "author unconfirmed") {
		t.Fatalf("want one line saying the author is unconfirmed:\n%s", logs.String())
	}
}

func TestARefusedACancelledAndAnExpiredUpgradeWriteNoUpgradedLine(t *testing.T) {
	plugins.Parallel(t)
	f, clk, logs := auditFixture(t)
	_, priv := newKey(t)
	mustInstall(t, f, upgradeArchive(t, "up-sink", "1.0.0", "v1", priv, upgradePublisher, nil))

	// Refused: the same version.
	if _, err := f.manager.InstallPackage(asAdmin("ada"), upgradeArchive(t, "up-sink", "1.0.0", "v2", priv, upgradePublisher, nil), plugins.SourceUpload); refusalReason(err) != plugins.ReasonVersion {
		t.Fatalf("the same version was not refused: %v", err)
	}
	// Cancelled.
	s, err := f.manager.InstallPackage(asAdmin("ada"), upgradeArchive(t, "up-sink", "1.1.0", "v2", priv, upgradePublisher, widening), plugins.SourceUpload)
	if err != nil || s.Staged == nil {
		t.Fatalf("expected a preview: %+v / %v", s, err)
	}
	if err := f.manager.CancelUpgrade(asAdmin("ada"), "up-sink", s.Staged.Staged); err != nil {
		t.Fatal(err)
	}
	// Expired.
	s, err = f.manager.InstallPackage(asAdmin("ada"), upgradeArchive(t, "up-sink", "1.2.0", "v3", priv, upgradePublisher, widening), plugins.SourceUpload)
	if err != nil || s.Staged == nil {
		t.Fatalf("expected a preview: %+v / %v", s, err)
	}
	clk.Advance(11 * time.Minute)
	if _, err := f.manager.ConfirmUpgrade(asAdmin("bea"), "up-sink", s.Staged.Staged); refusalReason(err) != plugins.ReasonStaged {
		t.Fatalf("the expired upgrade was not refused: %v", err)
	}

	if lines := upgradedLines(logs); len(lines) != 0 {
		t.Fatalf("an upgrade that was not applied wrote a line:\n%s", strings.Join(lines, "\n"))
	}
}
