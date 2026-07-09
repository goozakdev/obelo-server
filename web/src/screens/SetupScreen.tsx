import { useState, type FormEvent } from "react";
import { useNavigate } from "react-router-dom";
import { apiClient } from "../api/client";
import { useAuth } from "../auth/session";
import { errorMessage } from "./errorMessage";

// First-run setup (PRD user stories 1–2). Shown when the handshake reports
// setupRequired. The operator pastes the claim token (printed in the server
// logs, ADR-0013) plus a username/password to create the first Admin, then is
// auto-logged-in and lands on Home.
//
// We auto-login after setup (rather than dumping the user back at a login form)
// because they just chose those exact credentials a second ago — a friendlier
// first run. The token comes from the login call, not setup (setup returns only
// the user).

export default function SetupScreen() {
  const navigate = useNavigate();
  const { login } = useAuth();
  const [claimToken, setClaimToken] = useState("");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    setError(null);
    setSubmitting(true);
    try {
      await apiClient.setup({ claimToken, username, password });
      // Auto-login with the just-created credentials, then land on Home.
      await login(username, password);
      navigate("/", { replace: true });
    } catch (err) {
      setError(errorMessage(err));
      setSubmitting(false);
    }
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
