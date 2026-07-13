import { useEffect, useRef, useState } from "react";
import { apiClient } from "../api/client";
import { useAuth } from "../auth/session";

// The first-run Enrichment consent prompt (ADR-0032). Distributed default
// metadata credentials make external enrichment work out of the box, so a fresh
// install must ASK before any provider is contacted. This gate fetches the
// consent state once the current user is a logged-in Admin; while it is "unset"
// it shows a native <dialog> modal offering Enable / Not now, and records the
// decision. After a decision (or for a Member / logged-out visitor) it renders
// nothing.
//
// Mounted ONCE in the authed app scope (App.tsx), like NowPlayingBar. Only Admins
// can grant consent, so the fetch is gated on isAdmin — a Member never hits the
// Admin-only endpoint (which would 403).

export default function EnrichmentConsentGate() {
  const { ready, isAuthenticated, isAdmin } = useAuth();
  const [show, setShow] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const dialogRef = useRef<HTMLDialogElement>(null);

  // Fetch the decision once we know the viewer is an authenticated Admin. A
  // non-Admin or logged-out viewer never calls the Admin-only endpoint.
  useEffect(() => {
    if (!ready || !isAuthenticated || !isAdmin) return;
    const ctrl = new AbortController();
    apiClient
      .getEnrichmentConsent(ctrl.signal)
      .then((c) => setShow(c.state === "unset"))
      .catch(() => {
        // A transient failure just leaves the prompt hidden; the operator can
        // still decide from the Metadata Providers settings later.
      });
    return () => ctrl.abort();
  }, [ready, isAuthenticated, isAdmin]);

  // Open the native modal when we decide to show it.
  useEffect(() => {
    const dialog = dialogRef.current;
    if (show && dialog && !dialog.open) dialog.showModal();
  }, [show]);

  if (!show) return null;

  const decide = async (granted: boolean) => {
    setBusy(true);
    setError(null);
    try {
      await apiClient.setEnrichmentConsent(granted);
      setShow(false);
    } catch {
      setError("Couldn't save your choice. Please try again.");
    } finally {
      setBusy(false);
    }
  };

  return (
    <dialog
      ref={dialogRef}
      className="library-dialog confirm-dialog"
      data-testid="enrichment-consent-dialog"
      onCancel={(e) => {
        // ESC must not silently dismiss a first-run decision — keep it open.
        e.preventDefault();
      }}
    >
      <div className="library-dialog-panel">
        <header className="library-dialog-header">
          <h2 className="library-dialog-title">Enable metadata enrichment?</h2>
        </header>

        <div className="library-dialog-body">
          <p className="confirm-dialog-message" data-testid="enrichment-consent-message">
            Juice Box can fetch posters, descriptions, cast, and artwork for your
            library from <strong>TMDB</strong> and <strong>fanart.tv</strong>. This
            contacts those services over the internet. You can change this anytime
            under Admin → Metadata Providers.
          </p>
          {error && (
            <p className="auth-error" data-testid="enrichment-consent-error" role="alert">
              {error}
            </p>
          )}
        </div>

        <footer className="library-dialog-footer library-dialog-footer-end">
          <button
            className="nav-link"
            type="button"
            data-testid="enrichment-consent-decline"
            onClick={() => void decide(false)}
            disabled={busy}
          >
            Not now
          </button>
          <button
            className="auth-submit"
            type="button"
            data-testid="enrichment-consent-enable"
            onClick={() => void decide(true)}
            disabled={busy}
          >
            {busy ? "Saving…" : "Enable enrichment"}
          </button>
        </footer>
      </div>
    </dialog>
  );
}
