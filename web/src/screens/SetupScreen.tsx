import { useState, type FormEvent } from "react";
import { useNavigate } from "react-router-dom";
import { apiClient } from "../api/client";
import { useAuth } from "../auth/session";
import type { MetadataCredentialSource } from "../api/types";
import {
  CONSENT_DECLINE_LABEL,
  CONSENT_ENABLE_LABEL,
  consentExplanation,
  consentReversibleNote,
  credentialNote,
} from "../admin/metadataConsentCopy";
import { errorMessage } from "./errorMessage";

// First-run setup (PRD user stories 1–2). Shown when the handshake reports
// setupRequired. Two steps:
//
//   1. credentials — paste the claim token (printed in the server logs, ADR-0013)
//      plus a username/password to create the first Admin, then auto-login.
//   2. metadata    — decide whether this server may contact TMDB / fanart.tv
//      (the ADR-0032 consent gate), before it ever has.
//
// We auto-login after step 1 (rather than dumping the user back at a login form)
// because they just chose those exact credentials a second ago — a friendlier
// first run. The token comes from the login call, not setup (setup returns only
// the user). Step 2 needs that token: consent is an Admin-scope endpoint.
//
// WHY STEP 2 IS PART OF SETUP rather than a prompt after landing on Home: official
// binaries and Docker images ship with working metadata credentials (ADR-0032), so
// without an explicit ask, a fresh install would be one step away from making
// outbound calls with a key the operator never registered. Setting up the server
// IS the moment the operator is deciding what this server may do, so the question
// belongs here, in the flow they are already giving their attention to — not in a
// modal that appears after they think they are finished. EnrichmentConsentGate
// remains mounted in the app for the servers this step cannot reach: one that was
// set up before this step existed, or a browser closed mid-wizard.

type Step = "credentials" | "metadata";

export default function SetupScreen() {
  const navigate = useNavigate();
  const { login } = useAuth();
  const [step, setStep] = useState<Step>("credentials");
  const [claimToken, setClaimToken] = useState("");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  // What the server says it would enrich WITH — bundled default keys, the
  // operator's own, or none. Undefined until the consent GET answers (and on an
  // older server that doesn't report it), which the copy handles as "unknown".
  const [credentialSource, setCredentialSource] = useState<
    MetadataCredentialSource | undefined
  >(undefined);

  const finish = () => navigate("/", { replace: true });

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    setError(null);
    setSubmitting(true);
    try {
      await apiClient.setup({ claimToken, username, password });
      // Auto-login with the just-created credentials; every call after this one is
      // authenticated as the new Admin.
      await login(username, password);
    } catch (err) {
      setError(errorMessage(err));
      setSubmitting(false);
      return;
    }

    // The admin exists — from here on, nothing may bounce the operator back to a
    // form they already completed. Read the current consent decision: a headless
    // deploy can pre-seed it (OBELO_ENRICHMENT_CONSENT), and a question already
    // answered must not be asked again. Anything unexpected (an older server, a
    // failed read) skips straight to Home rather than blocking setup on a
    // secondary concern — the consent gate still holds server-side, so skipping
    // the ask can only ever leave enrichment OFF.
    try {
      const consent = await apiClient.getEnrichmentConsent();
      if (consent.state !== "unset") {
        finish();
        return;
      }
      setCredentialSource(consent.credentialSource);
      setStep("metadata");
      setSubmitting(false);
    } catch {
      finish();
    }
  }

  // Record the metadata decision and land on Home. A failed save is shown rather
  // than swallowed — but "Continue anyway" is always available, because an
  // unrecorded decision is the same as an unanswered one (enrichment stays off)
  // and nobody should be trapped in setup by it.
  async function decide(granted: boolean) {
    setError(null);
    setSubmitting(true);
    try {
      await apiClient.setEnrichmentConsent(granted);
      finish();
    } catch {
      setError("Couldn't save your choice. Metadata stays off until it is saved.");
      setSubmitting(false);
    }
  }

  if (step === "metadata") {
    return (
      <div className="auth-shell" data-testid="setup-screen">
        <div className="auth-card" data-testid="setup-metadata-step">
          <h1 className="auth-title">Enable metadata services?</h1>
          <p className="auth-subtitle" data-testid="setup-metadata-explanation">
            {consentExplanation}
          </p>
          <p
            className="auth-subtitle"
            data-testid="setup-metadata-credentials"
            data-source={credentialSource ?? "unknown"}
          >
            {credentialNote(credentialSource)}
          </p>
          <p className="auth-subtitle" data-testid="setup-metadata-reversible">
            {consentReversibleNote}
          </p>

          {error && (
            <p className="auth-error" data-testid="setup-error" role="alert">
              {error}
            </p>
          )}

          <button
            className="auth-submit"
            data-testid="setup-metadata-enable"
            type="button"
            onClick={() => void decide(true)}
            disabled={submitting}
          >
            {CONSENT_ENABLE_LABEL}
          </button>
          <button
            className="nav-link"
            data-testid="setup-metadata-decline"
            type="button"
            onClick={() => void decide(false)}
            disabled={submitting}
          >
            {CONSENT_DECLINE_LABEL}
          </button>
          {error && (
            <button
              className="nav-link"
              data-testid="setup-metadata-skip"
              type="button"
              onClick={finish}
            >
              Continue anyway
            </button>
          )}
        </div>
      </div>
    );
  }

  return (
    <div className="auth-shell" data-testid="setup-screen">
      <form className="auth-card" onSubmit={onSubmit}>
        <h1 className="auth-title">Set up your server</h1>
        <p className="auth-subtitle">
          This server has no admin yet. Paste the claim token from the server
          logs and choose your admin credentials.
        </p>

        <label className="field">
          <span className="field-label">Claim token</span>
          <input
            data-testid="setup-claim-token"
            className="field-input"
            type="text"
            autoComplete="off"
            value={claimToken}
            onChange={(e) => setClaimToken(e.target.value)}
            required
          />
        </label>

        <label className="field">
          <span className="field-label">Username</span>
          <input
            data-testid="setup-username"
            className="field-input"
            type="text"
            autoComplete="username"
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            required
          />
        </label>

        <label className="field">
          <span className="field-label">Password</span>
          <input
            data-testid="setup-password"
            className="field-input"
            type="password"
            autoComplete="new-password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            required
          />
        </label>

        {error && (
          <p className="auth-error" data-testid="setup-error" role="alert">
            {error}
          </p>
        )}

        <button
          className="auth-submit"
          data-testid="setup-submit"
          type="submit"
          disabled={submitting}
        >
          {submitting ? "Creating admin…" : "Create admin & continue"}
        </button>
      </form>
    </div>
  );
}
