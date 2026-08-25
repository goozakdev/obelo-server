import { useEffect, useRef, useState } from "react";
import { useLocation } from "react-router-dom";
import { apiClient } from "../api/client";
import { useAuth } from "../auth/session";
import type { MetadataCredentialSource } from "../api/types";
import {
  CONSENT_DECLINE_LABEL,
  CONSENT_ENABLE_LABEL,
  consentExplanation,
  consentReversibleNote,
  credentialNote,
} from "./metadataConsentCopy";

// The CATCH-UP Enrichment consent prompt (ADR-0032). Distributed default metadata
// credentials make external enrichment work out of the box, so a server must ASK
// before any provider is contacted. The first-run setup wizard (SetupScreen) now
// asks as an explicit step, which is where the question belongs — so this gate is
// the net beneath it, not the primary ask: it catches a server set up before that
// step existed, and a browser closed between "create admin" and the decision. It
// fetches the consent state once the current user is a logged-in Admin; while it
// is "unset" it shows a native <dialog> modal offering Enable / Not now, and
// records the decision. After a decision (or for a Member / logged-out visitor) it
// renders nothing.
//
// The wording is imported, not written here: this modal and the setup step ask the
// same question and must not drift (see metadataConsentCopy) — including the part
// that says whose API key is at stake, which differs between an official build and
// one built from source.
//
// Mounted ONCE in the authed app scope (App.tsx), like NowPlayingBar — which means
// OUTSIDE <Routes>, so it renders on every route including /setup. It therefore
// stands down while the setup wizard is on screen: the wizard auto-logs-in as the
// new Admin, which makes this component's precondition (authenticated Admin,
// consent unset) true while the operator is looking at the wizard's own version of
// the question. Left unguarded, a native modal opens on top of the wizard step and
// swallows the clicks meant for it — the question asked twice, with the second copy
// blocking the first. Route suppression alone is not enough: the answer given on
// /setup lands while this component already holds a stale "unset", so leaving
// /setup RE-READS the decision rather than trusting what was fetched before it.
//
// Only Admins can grant consent, so the fetch is gated on isAdmin — a Member never
// hits the Admin-only endpoint (which would 403).

export default function EnrichmentConsentGate() {
  const { ready, isAuthenticated, isAdmin } = useAuth();
  const [show, setShow] = useState(false);
  const [credentialSource, setCredentialSource] = useState<
    MetadataCredentialSource | undefined
  >(undefined);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const dialogRef = useRef<HTMLDialogElement>(null);

  // True while the first-run setup wizard owns the question (see the note above).
  const onSetup = useLocation().pathname.startsWith("/setup");

  // Fetch the decision once we know the viewer is an authenticated Admin. A
  // non-Admin or logged-out viewer never calls the Admin-only endpoint. Re-runs when
  // the wizard is left, so a decision made there is seen instead of re-asked.
  useEffect(() => {
    if (!ready || !isAuthenticated || !isAdmin || onSetup) {
      setShow(false);
      return;
    }
    const ctrl = new AbortController();
    apiClient
      .getEnrichmentConsent(ctrl.signal)
      .then((c) => {
        setCredentialSource(c.credentialSource);
        setShow(c.state === "unset");
      })
      .catch(() => {
        // A transient failure just leaves the prompt hidden; the operator can
        // still decide from the Metadata Providers settings later.
      });
    return () => ctrl.abort();
  }, [ready, isAuthenticated, isAdmin, onSetup]);

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
          <h2 className="library-dialog-title">Enable metadata services?</h2>
        </header>

        <div className="library-dialog-body">
          <p className="confirm-dialog-message" data-testid="enrichment-consent-message">
            {consentExplanation}
          </p>
          <p
            className="confirm-dialog-message"
            data-testid="enrichment-consent-credentials"
            data-source={credentialSource ?? "unknown"}
          >
            {credentialNote(credentialSource)}
          </p>
          <p className="confirm-dialog-message" data-testid="enrichment-consent-reversible">
            {consentReversibleNote}
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
            {CONSENT_DECLINE_LABEL}
          </button>
          <button
            className="auth-submit"
            type="button"
            data-testid="enrichment-consent-enable"
            onClick={() => void decide(true)}
            disabled={busy}
          >
            {busy ? "Saving…" : CONSENT_ENABLE_LABEL}
          </button>
        </footer>
      </div>
    </dialog>
  );
}
