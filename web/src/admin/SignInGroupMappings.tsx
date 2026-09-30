import { useEffect, useState } from "react";
import { apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";
import { formatAgo } from "../time";
import type { GroupMappingRule, GroupMappingView, Library, SignInProvider } from "../api/types";

// Each Sign-in provider's Group mapping (ADR-0063 decision 4): which of its
// groups give which role and which Libraries. The server re-applies it at every
// sign-in and at each re-check between sign-ins, and never to a User who holds
// a Local password. A provider whose re-checks are failing is flagged here from
// the first failure; "Re-sync now" asks it at once.
//
// Like the other sign-in cards it appears only when there is a configured provider,
// and a list that will not load is the same absence.

export default function SignInGroupMappings() {
  const [providers, setProviders] = useState<SignInProvider[] | null>(null);
  const [libraries, setLibraries] = useState<Library[]>([]);

  useEffect(() => {
    let live = true;
    (async () => {
      try {
        const view = await apiClient.getSignInProviders();
        const all = [...(view.providers ?? []), ...(view.redirect ?? []).filter((p) => p.configured)];
        const seen = new Set<string>();
        const unique = all.filter((p) => !seen.has(p.id) && seen.add(p.id));
        if (live) setProviders(unique);
      } catch {
        // Left absent: see above.
      }
      try {
        const libs = await apiClient.listLibraries();
        if (live) setLibraries(libs ?? []);
      } catch {
        // Rules can still be read and saved without Library names.
      }
    })();
    return () => {
      live = false;
    };
  }, []);

  if (!providers || providers.length === 0) return null;

  return (
    <>
      {providers.map((p) => (
        <GroupMappingCard key={p.id} provider={p} libraries={libraries} />
      ))}
    </>
  );
}

function GroupMappingCard({ provider, libraries }: { provider: SignInProvider; libraries: Library[] }) {
  const [view, setView] = useState<GroupMappingView | null>(null);
  const [rules, setRules] = useState<GroupMappingRule[]>([]);
  const [hours, setHours] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [note, setNote] = useState<string | null>(null);

  function show(v: GroupMappingView) {
    setView(v);
    setRules(v.rules.map((r) => ({ ...r, libraryIds: [...(r.libraryIds ?? [])] })));
    setHours(v.defaultInterval ? "" : String(v.intervalHours));
  }

  useEffect(() => {
    let live = true;
    (async () => {
      try {
        const v = await apiClient.getGroupMapping(provider.id);
        if (live) show(v);
      } catch {
        // Left absent: the card says nothing it could not read.
      }
    })();
    return () => {
      live = false;
    };
  }, [provider.id]);

  if (!view) return null;

  function edit(i: number, patch: Partial<GroupMappingRule>) {
    setRules((rs) => rs.map((r, j) => (j === i ? { ...r, ...patch } : r)));
  }

  async function save() {
    setBusy(true);
    setError(null);
    setNote(null);
    try {
      const every = hours.trim() === "" ? 0 : Number(hours);
      show(await apiClient.setGroupMapping(provider.id, rules, every));
      setNote("Saved. It applies at each person's next sign-in or re-check.");
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }

  async function resync() {
    setBusy(true);
    setError(null);
    setNote(null);
    try {
      const res = await apiClient.resyncSignInProvider(provider.id);
      setNote(
        `Re-synced: ${res.checked} asked, ${res.remapped} re-mapped, ${res.failed} failed, ` +
          `${res.revoked} signed out.`,
      );
      show(await apiClient.getGroupMapping(provider.id));
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }

  const id = provider.id;
  return (
    <div className="provider-card" data-testid={`group-mapping-${id}`}>
      <div className="provider-head">
        <span className="provider-name">Group mapping: {provider.name}</span>
      </div>
      <p className="provider-desc">
        Each group below gives its members a role and Libraries, applied at every sign-in and
        re-check. It never changes anyone who has a Local password. With no groups mapped, this
        provider changes nobody&apos;s access.
      </p>
      {view.failing && (
        <p className="auth-error" data-testid={`group-mapping-failing-${id}`}>
          Re-checks are failing ({view.failing.reason}; first failed{" "}
          {formatAgo(view.failing.since) || "just now"}). Everyone keeps their last known access
          until it answers.
        </p>
      )}
      {!view.recheck && (
        <p className="provider-desc" data-testid={`group-mapping-sign-in-only-${id}`}>
          This provider cannot be asked between sign-ins: its mapping is applied when someone signs
          in, or when you re-sync now.
        </p>
      )}
      <ul className="provider-list">
        {rules.map((r, i) => (
          <li key={i} data-testid={`group-mapping-rule-${id}-${i}`}>
            <input
              className="field-input plugin-inline-input"
              aria-label="Group"
              value={r.group}
              onChange={(e) => edit(i, { group: e.target.value })}
            />{" "}
            <select
              className="field-input plugin-inline-input"
              aria-label="Role"
              value={r.role}
              onChange={(e) => edit(i, { role: e.target.value as GroupMappingRule["role"] })}
            >
              <option value="member">Member</option>
              <option value="admin">Admin</option>
            </select>{" "}
            {r.role === "member" &&
              libraries.map((lib) => (
                <label key={lib.id}>
                  <input
                    type="checkbox"
                    checked={r.libraryIds.includes(lib.id)}
                    onChange={(e) =>
                      edit(i, {
                        libraryIds: e.target.checked
                          ? [...r.libraryIds, lib.id]
                          : r.libraryIds.filter((x) => x !== lib.id),
                      })
                    }
                  />{" "}
                  {lib.name}
                </label>
              ))}{" "}
            <button
              className="button-danger"
              type="button"
              onClick={() => setRules((rs) => rs.filter((_, j) => j !== i))}
              disabled={busy}
            >
              Remove
            </button>
          </li>
        ))}
      </ul>
      <div className="admin-actions">
        <button
          className="button-secondary"
          type="button"
          data-testid={`group-mapping-add-${id}`}
          onClick={() => setRules((rs) => [...rs, { group: "", role: "member", libraryIds: [] }])}
          disabled={busy}
        >
          Add group
        </button>
      </div>
      {view.recheck && (
        <label>
          {" "}
          Re-check every{" "}
          <input
            className="field-input plugin-hours-input"
            aria-label="Re-check interval in hours"
            data-testid={`group-mapping-interval-${id}`}
            inputMode="numeric"
            placeholder="24"
            value={hours}
            onChange={(e) => setHours(e.target.value)}
          />{" "}
          hours
        </label>
      )}
      <div className="admin-actions">
        <button
          className="auth-submit"
          type="button"
          data-testid={`group-mapping-save-${id}`}
          onClick={() => void save()}
          disabled={busy}
        >
          Save
        </button>{" "}
        <button
          className="button-secondary"
          type="button"
          data-testid={`group-mapping-resync-${id}`}
          onClick={() => void resync()}
          disabled={busy}
        >
          Re-sync now
        </button>
      </div>
      {error && (
        <p className="auth-error" data-testid={`group-mapping-error-${id}`}>
          {error}
        </p>
      )}
      {note && (
        <p className="admin-section-note" data-testid={`group-mapping-note-${id}`}>
          {note}
        </p>
      )}
    </div>
  );
}
