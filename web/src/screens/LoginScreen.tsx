import { useState, type FormEvent } from "react";
import { useLocation, useNavigate, useSearchParams } from "react-router-dom";
import { useAuth } from "../auth/session";
import { errorMessage } from "./errorMessage";

// Login (PRD user story 3). Exchanges credentials for a session via the auth
// provider, then returns the user to wherever they were headed before the guard
// bounced them here (location.state.from), defaulting to Home.
//
// Remember me (appletv-parity/10) governs token retention only — NO password is
// ever stored, only the durable bearer token. Checked (the default, matching the
// historical always-persist behaviour) keeps the token in localStorage so the
// session survives a tab close; unchecked keeps it session-only, gone on close.
// A `?user=` param pre-fills the username when arriving from a Known roster
// entry's switch-user affordance.

interface FromState {
  from?: { pathname?: string };
}

export default function LoginScreen() {
  const navigate = useNavigate();
  const location = useLocation();
  const [searchParams] = useSearchParams();
  const { login } = useAuth();
  const [username, setUsername] = useState(() => searchParams.get("user") ?? "");
  const [password, setPassword] = useState("");
  const [remember, setRemember] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  const from = (location.state as FromState | null)?.from?.pathname ?? "/";

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    setError(null);
    setSubmitting(true);
    try {
      await login(username, password, remember);
      navigate(from, { replace: true });
    } catch (err) {
      setError(errorMessage(err));
      setSubmitting(false);
    }
  }

  return (
    <div className="auth-shell" data-testid="login-screen">
      <form className="auth-card" onSubmit={onSubmit}>
        <h1 className="auth-title">Sign in</h1>
        <p className="auth-subtitle">Sign in to browse and watch your library.</p>

        <label className="field">
          <span className="field-label">Username</span>
          <input
            data-testid="login-username"
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
            data-testid="login-password"
            className="field-input"
            type="password"
            autoComplete="current-password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            required
          />
        </label>

        <label className="field-checkbox">
          <input
            data-testid="login-remember"
            type="checkbox"
            checked={remember}
            onChange={(e) => setRemember(e.target.checked)}
          />
          <span className="field-checkbox-label">Remember me on this device</span>
        </label>

        {error && (
          <p className="auth-error" data-testid="login-error" role="alert">
            {error}
          </p>
        )}

        <button
          className="auth-submit"
          data-testid="login-submit"
          type="submit"
          disabled={submitting}
        >
          {submitting ? "Signing in…" : "Sign in"}
        </button>
      </form>
    </div>
  );
}
