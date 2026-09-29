import { useEffect, useState } from "react";
import { apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";
import type { SignInProvider } from "../api/types";

// The order the login form asks the password-flow Sign-in providers in (ADR-0063
// decision 6). The Local password is always tried first; then each provider here,
// top to bottom, and the first to accept wins.
//
// The card appears only when there is something to order: with no password-flow
// Sign-in provider installed — every server that has not added one — the Plugins
// screen is exactly what it was. A list that will not load is the same absence,
// never an error on a screen that is about something else.

export default function SignInProviderOrder() {
  const [providers, setProviders] = useState<SignInProvider[] | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let live = true;
    (async () => {
      try {
        const view = await apiClient.getSignInProviders();
        if (live) setProviders(view.providers ?? []);
      } catch {
        // Left absent: see above.
      }
    })();
    return () => {
      live = false;
    };
  }, []);

  if (!providers || providers.length === 0) return null;

  async function move(index: number, by: -1 | 1) {
    if (!providers) return;
    const next = [...providers];
    const [p] = next.splice(index, 1);
    next.splice(index + by, 0, p);
    setBusy(true);
    setError(null);
    try {
      const view = await apiClient.setSignInProviderOrder(next.map((x) => x.id));
      setProviders(view.providers ?? []);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="provider-card" data-testid="sign-in-order">
      <div className="provider-head">
        <span className="provider-name">Sign-in order</span>
      </div>
      <p className="provider-desc">
        When someone signs in with a password, their Local password is tried first,
        then each Sign-in provider below from the top. The first to accept signs
        them in.
      </p>
      <ol>
        {providers.map((p, i) => (
          <li key={p.id} data-testid={`sign-in-order-${p.id}`}>
            <span>{p.name}</span>{" "}
            <button
              className="button-secondary"
              type="button"
              data-testid={`sign-in-order-up-${p.id}`}
              onClick={() => void move(i, -1)}
              disabled={busy || i === 0}
            >
              Move up
            </button>{" "}
            <button
              className="button-secondary"
              type="button"
              data-testid={`sign-in-order-down-${p.id}`}
              onClick={() => void move(i, 1)}
              disabled={busy || i === providers.length - 1}
            >
              Move down
            </button>
          </li>
        ))}
      </ol>
      {error && (
        <p className="auth-error" data-testid="sign-in-order-error">
          {error}
        </p>
      )}
    </div>
  );
}
