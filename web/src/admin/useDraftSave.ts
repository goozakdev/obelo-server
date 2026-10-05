import { useCallback, useEffect, useState } from "react";
import { errorMessage } from "../screens/errorMessage";

// The load → edit a draft → partial-update Save → re-seed scaffold shared by the
// Subtitle Providers and Event Sinks screens. Each screen supplies its own fetch,
// update call, draft shape and payload builder; the loading, saving, error and
// "Saved." bookkeeping lives here once so the two cannot drift.

export function useDraftSave<V, D, P>({
  fetchView,
  update,
  draftFromView,
  buildPayload,
}: {
  fetchView: () => Promise<V>;
  update: (payload: P) => Promise<V>;
  draftFromView: (view: V) => D;
  buildPayload: (view: V, draft: D) => P;
}) {
  const [view, setView] = useState<V | null>(null);
  const [draft, setDraft] = useState<D | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);

  // The callers pass inline functions; the load runs once on mount, as the
  // screens always did.
  const load = useCallback(async () => {
    try {
      const v = await fetchView();
      setView(v);
      setDraft(draftFromView(v));
    } catch (e) {
      setError(errorMessage(e));
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  async function save() {
    if (!view || !draft) return;
    setSaving(true);
    setError(null);
    setSaved(false);
    try {
      const updated = await update(buildPayload(view, draft));
      setView(updated);
      setDraft(draftFromView(updated));
      setSaved(true);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setSaving(false);
    }
  }

  return { view, draft, setDraft, error, saving, saved, save };
}
