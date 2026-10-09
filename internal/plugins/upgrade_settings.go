package plugins

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/goozakdev/obelo-server/internal/store"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// Settings on upgrade (ADR-0069, .scratch/plugin-inplace-upgrade issue 04). The
// plugin is the same, so what its Admin entered stays wherever the new manifest still
// declares the same key with the same type; anything else is reported, never guessed.

// SettingDropped is one stored setting an upgrade could not keep, and why.
type SettingDropped struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

// SettingsReport is what an upgrade did to a plugin's settings. It carries KEYS and
// reasons only: a secret's value is never in it.
type SettingsReport struct {
	// Kept are the keys whose stored value survived (same key, same type).
	Kept []string `json:"kept"`
	// Added are keys the new manifest declares and the old one did not; nothing is
	// stored for them and they read their default.
	Added []string `json:"added"`
	// Dropped are stored values discarded because the key's type changed.
	Dropped []SettingDropped `json:"dropped"`
	// Deleted are keys the new manifest no longer declares; their rows are removed.
	Deleted []string `json:"deleted"`
	// NeedsValue are added keys that are required and have no default, so the plugin
	// has nothing to read until an Admin fills them in. Reported, never blocking.
	NeedsValue []string `json:"needsValue"`
}

// settingsMigration is the rows an upgraded plugin keeps and the report of the rest.
type settingsMigration struct {
	rows   []store.PluginSetting
	report SettingsReport
}

// migrateSettings decides, per stored row, whether the new manifest keeps it. old and
// next are the declared fields of the installed and the offered version. A row is
// kept only when both declare its key with the same type and the value still fits the
// new definition (its options and bounds); a value is never coerced into a new type. A
// row the installed version never declared is stale: it is left alone unless the new
// version declares its key, when it is discarded and reported rather than revived with
// a value nobody has seen for this setting. What was never stored is not reported.
func migrateSettings(old, next []pluginapi.SettingsField, stored []store.PluginSetting) settingsMigration {
	oldType := make(map[string]pluginapi.SettingsFieldType, len(old))
	for _, f := range old {
		oldType[f.Key] = f.Type
	}
	nextField := make(map[string]pluginapi.SettingsField, len(next))
	for _, f := range next {
		nextField[f.Key] = f
	}

	mig := settingsMigration{report: SettingsReport{
		Kept: []string{}, Added: []string{}, Dropped: []SettingDropped{}, Deleted: []string{}, NeedsValue: []string{},
	}}
	for _, row := range stored {
		nf, declared := nextField[row.Key]
		ot, wasDeclared := oldType[row.Key]
		switch {
		case !wasDeclared && declared:
			mig.report.Dropped = append(mig.report.Dropped, SettingDropped{
				Key: row.Key, Reason: "a leftover value from before this setting was declared is not carried over"})
		case !wasDeclared:
			// A row neither version declares is stale already; this upgrade did not lose
			// it, so it is neither judged nor reported.
			mig.rows = append(mig.rows, row)
		case !declared:
			mig.report.Deleted = append(mig.report.Deleted, row.Key)
		case ot != nf.Type:
			mig.report.Dropped = append(mig.report.Dropped, SettingDropped{
				Key: row.Key, Reason: fmt.Sprintf("type changed from %s to %s", ot, nf.Type)})
		case !valueFits(nf, row.Value):
			mig.report.Dropped = append(mig.report.Dropped, SettingDropped{Key: row.Key, Reason: misfitReason(nf.Type)})
		default:
			mig.rows = append(mig.rows, row)
			mig.report.Kept = append(mig.report.Kept, row.Key)
		}
	}
	for _, f := range next {
		if _, was := oldType[f.Key]; was {
			continue
		}
		mig.report.Added = append(mig.report.Added, f.Key)
		if f.Required && len(f.Default) == 0 {
			mig.report.NeedsValue = append(mig.report.NeedsValue, f.Key)
		}
	}
	sort.Strings(mig.report.Kept)
	sort.Strings(mig.report.Added)
	sort.Strings(mig.report.Deleted)
	sort.Strings(mig.report.NeedsValue)
	sort.Slice(mig.report.Dropped, func(i, j int) bool { return mig.report.Dropped[i].Key < mig.report.Dropped[j].Key })
	return mig
}

// valueFits reports whether a stored value satisfies the field as the new version
// declares it. The stored text is the field's own JSON.
func valueFits(f pluginapi.SettingsField, stored string) bool {
	_, err := decodeFieldValue(f, json.RawMessage(stored))
	return err == nil
}

// misfitReason says why a stored value of an unchanged type no longer fits. It never
// quotes the value: a secret's must not leave the database.
func misfitReason(t pluginapi.SettingsFieldType) string {
	switch t {
	case pluginapi.FieldEnum, pluginapi.FieldMultiSelect:
		return "the stored value is no longer one of the options"
	case pluginapi.FieldInteger:
		return "the stored value is outside the new bounds"
	}
	return "the stored value no longer satisfies the setting"
}

// lossy is true when applying the migration discards a stored value.
func (m settingsMigration) lossy() bool {
	return len(m.report.Dropped) > 0 || len(m.report.Deleted) > 0
}

func droppedKeys(d []SettingDropped) []string {
	out := make([]string, 0, len(d))
	for _, x := range d {
		out = append(out, x.Key)
	}
	return out
}
