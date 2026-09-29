import { useEffect, useState } from "react";
import { apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";
import type { LyricProvider } from "../api/types";

// The order the lyrics view asks the Lyric providers in. The first acceptable
// Synced answer in this order wins; a Track's own Synced lyrics are never asked
// about at all.
//
// The card appears only when there is something to order: with fewer than two
// Lyric providers installed and switched on, the Plugins screen is exactly what
// it was. A list that will not load is the same absence, never an error on a
// screen that is about something else.

export default function LyricProviderOrder() {
  const [providers, setProviders] = useState<LyricProvider[] | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let live = true;
    (async () => {
      try {
        const view = await apiClient.getLyricProviders();
        if (live) setProviders(view.providers ?? []);
      } catch {
        // Left absent: see above.
      }
    })();
    return () => {
      live = false;
    };
  }, []);

  if (!providers || providers.length < 2) return null;

  async function move(index: number, by: -1 | 1) {
    if (!providers) return;
    const next = [...providers];
    const [p] = next.splice(index, 1);
    next.splice(index + by, 0, p);
    setBusy(true);
    setError(null);
    try {
      const view = await apiClient.setLyricProviderOrder(next.map((x) => x.slug));
      setProviders(view.providers ?? []);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="provider-card" data-testid="lyric-order">
      <div className="provider-head">
        <span className="provider-name">Lyric provider order</span>
      </div>
      <p className="provider-desc">
        When a track has no timed lyrics of its own, the Lyric providers below are
        asked from the top. The first timed answer that fits the track wins.
      </p>
      <ol>
        {providers.map((p, i) => (
          <li key={p.slug} data-testid={`lyric-order-${p.slug}`}>
            <span>{p.name || p.slug}</span>{" "}
            <button
              className="button-secondary"
              type="button"
              data-testid={`lyric-order-up-${p.slug}`}
              onClick={() => void move(i, -1)}
              disabled={busy || i === 0}
            >
              Move up
            </button>{" "}
            <button
              className="button-secondary"
              type="button"
              data-testid={`lyric-order-down-${p.slug}`}
              onClick={() => void move(i, 1)}
              disabled={busy || i === providers.length - 1}
            >
              Move down
            </button>
          </li>
        ))}
      </ol>
      {error && (
        <p className="auth-error" data-testid="lyric-order-error">
          {error}
        </p>
      )}
    </div>
  );
}
