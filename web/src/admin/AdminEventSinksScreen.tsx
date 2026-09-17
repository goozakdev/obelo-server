import { useCallback, useEffect, useState } from "react";
import { apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";
import type {
  EventSink,
  EventSinksView,
  EventSinkUpdate,
  UpdateEventSinksInput,
} from "../api/types";
import MaskedKeyInput from "./MaskedKeyInput";
import EventsControl from "./EventsControl";

// The Event Sinks admin screen (ADR-0057 decision 6, plugin-system/05). The shape
// of the Subtitle Providers screen — the same card, the same masked secret, the
// same partial-update Save that the server applies with no restart — plus the one
// control only a sink has: which events it is told about.
//
// Kept deliberately minimal. Issue 06 adds the delivery / dropped / failure
// counters beside each sink, which is the number an Admin actually acts on once
// their receiver starts falling behind.
//
// Behind RequireAdmin and still server-enforced: a sink is the operator's outbound
// integration, not a Member's business.

interface Draft {
  enabled: Record<string, boolean>;
  secret: Record<string, string>;
  clearSecret: Record<string, boolean>;
  url: Record<string, string>;
  events: Record<string, string[]>;
}

function draftFromView(view: EventSinksView): Draft {
  const draft: Draft = { enabled: {}, secret: {}, clearSecret: {}, url: {}, events: {} };
  for (const s of view.sinks) {
    draft.enabled[s.slug] = s.enabled;
    draft.secret[s.slug] = "";
    draft.clearSecret[s.slug] = false;
    draft.url[s.slug] = s.url;
    draft.events[s.slug] = s.events;
  }
  return draft;
}

function sameEvents(a: string[], b: string[]): boolean {
  return a.length === b.length && a.every((ev, i) => ev === b[i]);
}

// buildPayload emits ONLY the sinks the Admin changed (partial update), so an
// untouched sink is never dragged into a "secret required" rejection.
function buildPayload(view: EventSinksView, draft: Draft): UpdateEventSinksInput {
  const sinks: EventSinkUpdate[] = [];
  for (const s of view.sinks) {
    const u: EventSinkUpdate = { slug: s.slug };
    let changed = false;
    if (draft.enabled[s.slug] !== s.enabled) {
      u.enabled = draft.enabled[s.slug];
      changed = true;
    }
    if (draft.clearSecret[s.slug]) {
      u.secret = ""; // clear
      changed = true;
    } else if (draft.secret[s.slug] !== "") {
      u.secret = draft.secret[s.slug]; // set
      changed = true;
    }
    if (draft.url[s.slug] !== s.url) {
      u.url = draft.url[s.slug];
      changed = true;
    }
    if (!sameEvents(draft.events[s.slug], s.events)) {
      u.events = draft.events[s.slug];
      changed = true;
    }
    if (changed) sinks.push(u);
  }
  const payload: UpdateEventSinksInput = {};
  if (sinks.length) payload.sinks = sinks;
  return payload;
}

export default function AdminEventSinksScreen() {
  const [view, setView] = useState<EventSinksView | null>(null);
  const [draft, setDraft] = useState<Draft | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);

  const load = useCallback(async () => {
    try {
      const v = await apiClient.getEventSinks();
      setView(v);
      setDraft(draftFromView(v));
    } catch (e) {
      setError(errorMessage(e));
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  async function onSave() {
    if (!view || !draft) return;
    setSaving(true);
    setError(null);
    setSaved(false);
    try {
      const updated = await apiClient.updateEventSinks(buildPayload(view, draft));
      setView(updated);
      setDraft(draftFromView(updated));
      setSaved(true);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setSaving(false);
    }
  }

  if (error && !view) {
    return (
      <div className="admin-section" data-testid="event-sinks-error">
        <p className="form-error">{error}</p>
      </div>
    );
  }
  if (!view || !draft) {
    return (
      <div className="admin-section" data-testid="event-sinks-loading">
        Loading…
      </div>
    );
  }

  return (
    <div className="admin-section" data-testid="event-sinks-screen">
      <h2 className="admin-section-title">Event Sinks</h2>
      <p className="admin-section-note">
        Have this server tell something else when a scan finishes. Delivery is
        best-effort and in memory — it never slows a scan or a play down, and it does
        not survive a restart.
      </p>

      {view.sinks.map((s: EventSink) => (
        <div className="provider-card" key={s.slug} data-testid={`event-sink-${s.slug}`}>
          <div className="provider-head">
            <label className="provider-enable">
              <input
                type="checkbox"
                data-testid={`event-sink-enable-${s.slug}`}
                checked={draft.enabled[s.slug]}
                onChange={(e) =>
                  setDraft({
                    ...draft,
                    enabled: { ...draft.enabled, [s.slug]: e.target.checked },
                  })
                }
              />
              <span className="provider-name">{s.name}</span>
            </label>
            {s.docsURL && (
              <a className="provider-docs" href={s.docsURL} target="_blank" rel="noreferrer">
                Docs
              </a>
            )}
          </div>
          <p className="provider-desc">{s.description}</p>

          <div className="field">
            <label className="field-label" htmlFor={`event-sink-url-${s.slug}`}>
              Target URL
            </label>
            <input
              id={`event-sink-url-${s.slug}`}
              className="field-input"
              data-testid={`event-sink-url-${s.slug}`}
              placeholder="https://example.local/obelo"
              value={draft.url[s.slug]}
              onChange={(e) =>
                setDraft({ ...draft, url: { ...draft.url, [s.slug]: e.target.value } })
              }
              disabled={saving}
            />
          </div>

          {s.requiresSecret && (
            <div className="field">
              <label className="field-label">Signing secret</label>
              <MaskedKeyInput
                slug={s.slug}
                hasKey={s.hasSecret}
                value={draft.secret[s.slug]}
                cleared={draft.clearSecret[s.slug]}
                onChange={(v) =>
                  setDraft({
                    ...draft,
                    secret: { ...draft.secret, [s.slug]: v },
                    clearSecret: { ...draft.clearSecret, [s.slug]: false },
                  })
                }
                onClear={() =>
                  setDraft({
                    ...draft,
                    clearSecret: { ...draft.clearSecret, [s.slug]: !draft.clearSecret[s.slug] },
                    secret: { ...draft.secret, [s.slug]: "" },
                  })
                }
                disabled={saving}
              />
              <p className="field-hint">
                Each request carries an HMAC-SHA256 of the body under this secret in
                the <code>X-Obelo-Signature</code> header, so your receiver can reject
                anything this server did not send.
              </p>
            </div>
          )}

          <EventsControl
            slug={s.slug}
            available={view.availableEvents}
            selected={draft.events[s.slug]}
            onChange={(events) =>
              setDraft({ ...draft, events: { ...draft.events, [s.slug]: events } })
            }
            disabled={saving}
          />
        </div>
      ))}

      {error && (
        <p className="form-error" data-testid="event-sinks-save-error">
          {error}
        </p>
      )}
      {saved && (
        <p className="form-note" data-testid="event-sinks-saved">
          Saved.
        </p>
      )}

      <div className="admin-actions">
        <button
          className="btn btn-primary"
          type="button"
          data-testid="event-sinks-save"
          onClick={onSave}
          disabled={saving}
        >
          {saving ? "Saving…" : "Save"}
        </button>
      </div>
    </div>
  );
}
