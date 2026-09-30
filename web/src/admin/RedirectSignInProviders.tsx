import { useEffect, useState } from "react";
import { apiClient } from "../api/client";
import type { RedirectSignInProvider } from "../api/types";

// The redirect-flow Sign-in providers (ADR-0063 decision 2), and what the server
// can say about the identity each one hands back. A provider that issues ID
// tokens has every token verified against the issuer the Admin typed; a plain
// OAuth2 provider has nothing to verify, and the server takes its word — which
// this card says in so many words, so the gap is visible rather than hidden.
//
// Like the sign-in order card it appears only when there is something to show
// (a provider not yet configured is not shown), and a list that will not load is
// the same absence.

export default function RedirectSignInProviders() {
  const [providers, setProviders] = useState<RedirectSignInProvider[] | null>(null);

  useEffect(() => {
    let live = true;
    (async () => {
      try {
        const view = await apiClient.getSignInProviders();
        if (live) setProviders((view.redirect ?? []).filter((p) => p.configured));
      } catch {
        // Left absent: see above.
      }
    })();
    return () => {
      live = false;
    };
  }, []);

  if (!providers || providers.length === 0) return null;

  return (
    <div className="provider-card" data-testid="redirect-sign-in">
      <div className="provider-head">
        <span className="provider-name">Sign-in with another service</span>
      </div>
      <p className="provider-desc">
        These appear as buttons on the login screen. Sign-in from a TV or an iPad goes through a
        code approved from a signed-in browser.
      </p>
      <ul className="provider-list">
        {providers.map((p) => (
          <li key={p.id} data-testid={`redirect-sign-in-${p.id}`}>
            <span className="provider-name">{p.name}</span>{" "}
            {p.verified ? (
              <span className="provider-desc" data-testid={`redirect-sign-in-verified-${p.id}`}>
                Identity verified against the issuer&apos;s signing keys
              </span>
            ) : (
              <span className="provider-desc" data-testid={`redirect-sign-in-unverified-${p.id}`}>
                Identity not independently verified
              </span>
            )}
          </li>
        ))}
      </ul>
    </div>
  );
}
