import { useState } from "react";
import { apiClient } from "../api/client";
import type { MarkerAutoSkip } from "../api/types";
import { useAsync } from "../browse/useAsync";

// The user menu's Auto-skip switches (ADR-0065 §6): one per Marker kind. An on
// kind is skipped by the player without a click instead of offering Skip. The
// setting is the server's, per User — read when the menu opens and saved whole on
// every toggle — so it follows the User to every client. A setting that cannot
// be read shows no switches rather than switches that may be wrong.

const KINDS: ReadonlyArray<{ kind: keyof MarkerAutoSkip; label: string }> = [
  { kind: "intro", label: "Intros" },
  { kind: "recap", label: "Recaps" },
  { kind: "credits", label: "Credits" },
  { kind: "preview", label: "Previews" },
];

export default function MarkerAutoSkipMenu() {
  const state = useAsync((signal) => apiClient.getMarkerAutoSkip(signal), []);
  const [saved, setSaved] = useState<MarkerAutoSkip | null>(null);
  const [saving, setSaving] = useState(false);
  const setting = saved ?? (state.status === "ready" ? state.data : null);

  async function toggle(kind: keyof MarkerAutoSkip) {
    if (!setting) return;
    setSaving(true);
    try {
      setSaved(await apiClient.setMarkerAutoSkip({ ...setting, [kind]: !setting[kind] }));
    } catch {
      // Keep showing what the server last said.
    } finally {
      setSaving(false);
    }
  }

  if (!setting) return null;
  return (
    <>
      <li role="none" className="nav-dropdown-section" aria-hidden="true">
        Auto-skip
      </li>
      {KINDS.map(({ kind, label }) => (
        <li key={kind} role="none">
          <button
            role="menuitemcheckbox"
            aria-checked={setting[kind]}
            className="nav-dropdown-item nav-dropdown-item-button"
            data-testid={`auto-skip-${kind}`}
            type="button"
            disabled={saving}
            onClick={() => void toggle(kind)}
          >
            {label}
            <span className="auto-skip-state" aria-hidden="true">
              {setting[kind] ? "On" : "Off"}
            </span>
          </button>
        </li>
      ))}
    </>
  );
}
