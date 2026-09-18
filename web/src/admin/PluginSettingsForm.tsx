import { useState } from "react";
import { apiClient, ApiError } from "../api/client";
import { errorMessage } from "../screens/errorMessage";
import MaskedKeyInput from "./MaskedKeyInput";
import type {
  InstalledPlugin,
  InstalledPluginsView,
  PluginSettingsField,
} from "../api/types";

// The schema-driven settings form (plugin-system/13): one control per field an
// Installed plugin's MANIFEST declared for itself.
//
// It is not the plugin's Extension-point settings. Which events a sink hears,
// which URL it posts to, the key it signs with — those are still the FIXED shape,
// on the screen for the seam, in the same dialog as the Built-in beside it, and
// ADR-0057's whole claim is that nothing downstream can tell the two apart. What
// is here is the other half: fields this server learned exist by reading a file an
// author shipped, which no Built-in has and no compiled-in screen could draw.
//
// # The server decides, this form only asks well
//
// `required`, `options`, `min` and `max` are used to draw a control an operator
// can get right — a select rather than a text box, a number input with bounds.
// They are NOT used to decide whether a save is allowed. Every constraint is
// checked server-side against the manifest on disk, and a refusal comes back as a
// 400 carrying one message per field, which is what this form displays. A browser
// that skipped the client-side hints would still be refused exactly the same way,
// which is the only version of validation worth relying on.
//
// # Secrets
//
// A secret field is rendered with MaskedKeyInput and behaves as an API key does
// everywhere else in this app: the server never returns the value, the box starts
// empty, typing replaces it, leaving it empty leaves it alone, and Clear unsets
// it. That is why the submitted document OMITS a secret the Admin did not touch —
// a form that sent every field it could see would clear, on every save, the one
// field it cannot.

/** fieldErrorsOf pulls the per-field messages out of a refusal.
 *
 * The server sends `details.fields = [{key, message}]` beside the ordinary
 * message. Anything else — a network failure, a 500, an older server — has no
 * field detail, and the caller falls back to the one sentence it did get. */
function fieldErrorsOf(err: unknown): Record<string, string> {
  if (!(err instanceof ApiError) || !err.details) return {};
  const raw = err.details.fields;
  if (!Array.isArray(raw)) return {};
  const out: Record<string, string> = {};
  for (const entry of raw) {
    if (entry && typeof entry === "object") {
      const { key, message } = entry as { key?: unknown; message?: unknown };
      if (typeof key === "string" && typeof message === "string") out[key] = message;
    }
  }
  return out;
}

/** initialValues is what the controls start holding: what is saved, falling back
 * to the manifest's declared default for a field that has never been saved — so
 * an operator opening the panel sees what the plugin will actually use rather
 * than a row of empty boxes that mean something else. */
function initialValues(
  schema: PluginSettingsField[],
  saved: Record<string, unknown>,
): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const f of schema) {
    if (f.type === "secret") continue;
    if (Object.prototype.hasOwnProperty.call(saved, f.key)) out[f.key] = saved[f.key];
    else if (f.default !== undefined) out[f.key] = f.default;
  }
  return out;
}

function asString(v: unknown): string {
  if (v === undefined || v === null) return "";
  if (typeof v === "string") return v;
  if (typeof v === "number" || typeof v === "boolean") return String(v);
  return "";
}

function asStringList(v: unknown): string[] {
  return Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : [];
}

export default function PluginSettingsForm({
  plugin,
  disabled,
  onSaved,
}: {
  plugin: InstalledPlugin;
  disabled?: boolean;
  onSaved: (view: InstalledPluginsView) => void;
}) {
  const schema = plugin.settingsSchema ?? [];
  const saved = plugin.settings?.values ?? {};
  const secretsOnFile = plugin.settings?.secrets ?? {};

  const [values, setValues] = useState<Record<string, unknown>>(() =>
    initialValues(schema, saved),
  );
  // A secret the Admin is typing now. "" means "not typing", which means the
  // stored value is left alone.
  const [secretDrafts, setSecretDrafts] = useState<Record<string, string>>({});
  const [cleared, setCleared] = useState<Record<string, boolean>>({});
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});
  const [formError, setFormError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  if (schema.length === 0) return null;
  const busy = saving || disabled === true;
  const { id } = plugin;

  function setValue(key: string, value: unknown) {
    setValues((v) => ({ ...v, [key]: value }));
  }

  /** submitted is the document sent to the server: every non-secret field as it
   * stands, and a secret only when the Admin typed one (the value) or cleared one
   * (an explicit null). An untouched secret is absent, which is how it survives. */
  function submitted(): Record<string, unknown> {
    const out: Record<string, unknown> = {};
    for (const f of schema) {
      if (f.type === "secret") {
        const typed = secretDrafts[f.key] ?? "";
        if (typed !== "") out[f.key] = typed;
        else if (cleared[f.key]) out[f.key] = null;
        continue;
      }
      const v = values[f.key];
      if (f.type === "integer") {
        const text = asString(v).trim();
        if (text === "") continue; // absent, not zero
        const n = Number(text);
        // A number that is not one is sent as the TEXT the operator typed, so the
        // server's "must be a whole number" names the field instead of this form
        // silently dropping what they wrote.
        out[f.key] = Number.isFinite(n) ? n : text;
        continue;
      }
      if (f.type === "bool") {
        out[f.key] = v === true;
        continue;
      }
      if (f.type === "multi-select") {
        out[f.key] = asStringList(v);
        continue;
      }
      out[f.key] = asString(v);
    }
    return out;
  }

  async function onSave() {
    setSaving(true);
    setFieldErrors({});
    setFormError(null);
    setNotice(null);
    try {
      const view = await apiClient.savePluginSettings(id, { values: submitted() });
      setSecretDrafts({});
      setCleared({});
      onSaved(view);
      setNotice("Saved.");
    } catch (e) {
      const perField = fieldErrorsOf(e);
      setFieldErrors(perField);
      // The envelope's own message is shown too, and not only when there is no
      // field detail: it names the first refusal, and a panel whose failing
      // control has scrolled out of view would otherwise look like nothing
      // happened.
      setFormError(errorMessage(e));
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="plugin-settings" data-testid={`plugin-settings-${id}`}>
      <h4 className="plugin-settings-title">Settings</h4>
      <p className="admin-section-note">
        Declared by this plugin's own manifest. What it does for this server — the
        events it hears, the URL it posts to, the key it signs with — is configured
        on the screen for its extension point.
      </p>

      {schema.map((f) => (
        <PluginSettingsControl
          key={f.key}
          pluginId={id}
          field={f}
          value={values[f.key]}
          secretDraft={secretDrafts[f.key] ?? ""}
          secretOnFile={secretsOnFile[f.key] === true}
          secretCleared={cleared[f.key] === true}
          error={fieldErrors[f.key]}
          disabled={busy}
          onChange={(v) => setValue(f.key, v)}
          onSecretChange={(v) => setSecretDrafts((s) => ({ ...s, [f.key]: v }))}
          onSecretClear={() => setCleared((c) => ({ ...c, [f.key]: true }))}
        />
      ))}

      {formError && (
        <p className="form-error" data-testid={`plugin-settings-error-${id}`}>
          {formError}
        </p>
      )}
      {notice && (
        <p className="form-note" data-testid={`plugin-settings-notice-${id}`}>
          {notice}
        </p>
      )}

      <div className="admin-actions">
        <button
          className="btn btn-primary"
          type="button"
          data-testid={`plugin-settings-save-${id}`}
          onClick={() => void onSave()}
          disabled={busy}
        >
          {saving ? "Saving…" : "Save settings"}
        </button>
      </div>
    </div>
  );
}

/** PluginSettingsControl is one field: the label, the control its type calls for,
 * the author's help line, and — when the server refused this field — the server's
 * sentence directly beneath it. */
function PluginSettingsControl({
  pluginId,
  field,
  value,
  secretDraft,
  secretOnFile,
  secretCleared,
  error,
  disabled,
  onChange,
  onSecretChange,
  onSecretClear,
}: {
  pluginId: string;
  field: PluginSettingsField;
  value: unknown;
  secretDraft: string;
  secretOnFile: boolean;
  secretCleared: boolean;
  error?: string;
  disabled: boolean;
  onChange: (value: unknown) => void;
  onSecretChange: (value: string) => void;
  onSecretClear: () => void;
}) {
  const id = `plugin-${pluginId}-${field.key}`;
  const label = field.label || field.key;
  const testid = `plugin-field-${pluginId}-${field.key}`;

  return (
    <div className="field" data-testid={`${testid}-row`}>
      <label className="field-label" htmlFor={id}>
        {label}
        {field.required && <span className="field-required"> *</span>}
      </label>

      {field.type === "secret" ? (
        <MaskedKeyInput
          slug={`${pluginId}-${field.key}`}
          hasKey={secretOnFile}
          value={secretDraft}
          cleared={secretCleared}
          onChange={onSecretChange}
          onClear={onSecretClear}
          disabled={disabled}
        />
      ) : field.type === "bool" ? (
        <input
          id={id}
          data-testid={testid}
          type="checkbox"
          checked={value === true}
          onChange={(e) => onChange(e.target.checked)}
          disabled={disabled}
        />
      ) : field.type === "enum" ? (
        <select
          id={id}
          className="field-input"
          data-testid={testid}
          value={asString(value)}
          onChange={(e) => onChange(e.target.value)}
          disabled={disabled}
        >
          {/* An empty option is offered even for a required field: the server
              decides whether empty is acceptable, and a select that cannot express
              "nothing chosen" would silently save its first option. */}
          <option value="">—</option>
          {(field.options ?? []).map((o) => (
            <option key={o} value={o}>
              {o}
            </option>
          ))}
        </select>
      ) : field.type === "multi-select" ? (
        <div className="plugin-field-options" data-testid={testid}>
          {(field.options ?? []).map((o) => {
            const chosen = asStringList(value);
            return (
              <label key={o} className="plugin-field-option">
                <input
                  type="checkbox"
                  data-testid={`${testid}-${o}`}
                  checked={chosen.includes(o)}
                  onChange={(e) =>
                    onChange(
                      e.target.checked
                        ? [...chosen, o]
                        : chosen.filter((c) => c !== o),
                    )
                  }
                  disabled={disabled}
                />
                {o}
              </label>
            );
          })}
        </div>
      ) : field.type === "integer" ? (
        <input
          id={id}
          className="field-input"
          data-testid={testid}
          type="number"
          inputMode="numeric"
          min={field.min}
          max={field.max}
          value={asString(value)}
          onChange={(e) => onChange(e.target.value)}
          disabled={disabled}
        />
      ) : field.type === "string" || field.type === "url" ? (
        <input
          id={id}
          className="field-input"
          data-testid={testid}
          type="text"
          inputMode={field.type === "url" ? "url" : undefined}
          autoComplete="off"
          spellCheck={false}
          value={asString(value)}
          onChange={(e) => onChange(e.target.value)}
          disabled={disabled}
        />
      ) : (
        /* A field type this bundle does not know. The plugin is not broken and the
           server may well be a version ahead, so the form says what it cannot draw
           instead of rendering the wrong control over the value. */
        <p className="form-note" data-testid={`${testid}-unknown`}>
          This server describes “{label}” as a {field.type}, which this page cannot
          show yet.
        </p>
      )}

      {field.help && <p className="field-help">{field.help}</p>}
      {error && (
        <p className="form-error" data-testid={`${testid}-error`}>
          {error}
        </p>
      )}
    </div>
  );
}
