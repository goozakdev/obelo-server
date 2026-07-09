import { useState, type FormEvent } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { useAuth } from "../auth/session";
import { errorMessage } from "./errorMessage";

// Login (PRD user story 3). Exchanges credentials for a session via the auth
// provider, then returns the user to wherever they were headed before the guard
// bounced them here (location.state.from), defaulting to Home.

interface FromState {
  from?: { pathname?: string };
}

export default function LoginScreen() {
  const navigate = useNavigate();
  const location = useLocation();
  const { login } = useAuth();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  const from = (location.state as FromState | null)?.from?.pathname ?? "/";

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    setError(null);
    setSubmitting(true);
    try {
      await login(username, password);
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
