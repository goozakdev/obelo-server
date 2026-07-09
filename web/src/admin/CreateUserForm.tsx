import { useState, type FormEvent } from "react";
import { ApiError, apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";
import type { Role } from "../api/types";

// The create-User form (access-control-admin-ui issue 01). An Admin gives a
// username + password and picks a role, defaulting to Member so an Admin is never
// minted by accident (PRD story 7); "Admin" is an explicit opt-in (story 8).
//
// A duplicate username comes back as a 409 USERNAME_TAKEN ApiError, which the
// client deliberately does NOT swallow. We catch it here and render the server's
// readable message inline (flagging it via data-taken) without clearing the typed
// input, so the Admin can pick another name without retyping (story 10). Any other
// failure shows the same inline error slot; the form never crashes.

const TAKEN_CODE = "USERNAME_TAKEN";

// The roles an Admin can create. The server defaults an omitted role to "member";
// we send the chosen value explicitly so the outgoing call is unambiguous.
const ROLES: { value: Role; label: string }[] = [
  { value: "member", label: "Member" },
  { value: "admin", label: "Admin" },
];

export default function CreateUserForm({ onCreated }: { onCreated: () => void }) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [role, setRole] = useState<Role>("member");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<{ message: string; taken: boolean } | null>(
    null,
  );

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    if (submitting) return;
    const trimmedName = username.trim();
    if (!trimmedName || password.length === 0) {
      setError({
        message: "Enter a username and a password.",
        taken: false,
      });
      return;
    }
    setSubmitting(true);
    setError(null);
    try {
      await apiClient.createUser({
        username: trimmedName,
        password,
        role,
      });
      // Reset to a fresh form, then let the hub reload the list.
      setUsername("");
      setPassword("");
      setRole("member");
      onCreated();
    } catch (err) {
      const taken = err instanceof ApiError && err.code === TAKEN_CODE;
      setError({ message: errorMessage(err), taken });
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <form
      className="card create-user-form"
      data-testid="create-user-form"
      onSubmit={onSubmit}
    >
      <h2 className="card-title">Create a user</h2>

      <div className="field">
        <label className="field-label" htmlFor="user-username">
          Username
        </label>
        <input
          id="user-username"
          className="field-input"
          data-testid="user-username-input"
          type="text"
          value={username}
          placeholder="ada"
          autoComplete="off"
          onChange={(e) => setUsername(e.target.value)}
          disabled={submitting}
        />
      </div>

      <div className="field">
        <label className="field-label" htmlFor="user-password">
          Password
        </label>
        <input
          id="user-password"
          className="field-input"
          data-testid="user-password-input"
          type="password"
          value={password}
          placeholder="A strong password"
          autoComplete="new-password"
          onChange={(e) => setPassword(e.target.value)}
          disabled={submitting}
        />
      </div>

      <div className="field">
        <label className="field-label" htmlFor="user-role">
          Role
        </label>
        <select
          id="user-role"
          className="field-input"
          data-testid="user-role-select"
          value={role}
          onChange={(e) => setRole(e.target.value as Role)}
          disabled={submitting}
        >
          {ROLES.map((r) => (
            <option key={r.value} value={r.value}>
              {r.label}
            </option>
          ))}
        </select>
      </div>

      {error && (
        <p
          className="auth-error"
          data-testid="create-user-error"
          data-taken={error.taken ? "true" : undefined}
          role="alert"
        >
          {error.message}
        </p>
      )}

      <button
        className="auth-submit"
        data-testid="create-user-submit"
        type="submit"
        disabled={submitting}
      >
        {submitting ? "Creating…" : "Create user"}
      </button>
    </form>
  );
}
