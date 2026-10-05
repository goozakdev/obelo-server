import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { apiClient } from "../api/client";
import type { AttachProof, ExternalIdentitiesView, SignInProvider } from "../api/types";
import AppHeader from "../browse/AppHeader";
import { forgetReauthGrant, keepReauthGrant, readReauthGrant, reauthGrantExpiresAt } from "../auth/reauthGrant";
import { useAuth } from "../auth/session";
import { errorMessage } from "./errorMessage";
import { rememberRedirectSignIn } from "./SignInCallbackScreen";

// The signed-in User's own profile: the sign-ins attached to their account, and
// a way to attach another (ADR-0063 decision 3) — the only way an existing User
// gains an External identity. A password-flow provider gets a username and
// password form; a redirect-flow provider gets a button that sends the browser
// away and back to /sign-in/callback, which finishes the attach. The server
// decides everything: which identity it is, and whether it is already somebody
// else's; this screen shows what it says.
//
// A session alone never attaches. A User with a Local password types it here and
// it goes with the attach. A User without one first confirms it is them through
// a sign-in already attached to their account — a password form or a redirect
// round trip — and the re-auth grant that answers goes with ONE attach; after
// that attempt, successful or not, it is spent and they confirm again. It is
// read afresh when an attach is sent, so one that has expired, or that a sign-out
// or switch has forgotten, is never sent — the User is asked to confirm again.

const reauthExpiredMessage = "Confirm it is you again: that confirmation has expired.";

type LoadState =
  | { status: "loading" }
  | { status: "error"; message: string }
  | { status: "ready"; view: ExternalIdentitiesView };

export default function ProfileScreen() {
  const { session } = useAuth();
  const [state, setState] = useState<LoadState>({ status: "loading" });
  const [error, setError] = useState<string | null>(null);
  const [currentPassword, setCurrentPassword] = useState("");
  // A grant that sessionStorage refused to keep (blocked site data) lives here
  // instead, so the User still sees "Confirmed" and can attach this once.
  const memGrant = useRef<{ grant: string; expiresAt: number } | null>(null);
  const liveMemGrant = () => {
    const m = memGrant.current;
    return m && m.expiresAt > Date.now() ? m : null;
  };
  // The in-memory grant is only ever the NEWEST one (it is set when storage
  // refused it), so it beats whatever older grant storage still holds.
  const liveGrant = () => liveMemGrant()?.grant ?? readReauthGrant();
  const [grant, setGrant] = useState<string | null>(() => readReauthGrant());
  // A change of User while the screen is open reads the kept grant again — a
  // sign-out or switch has forgotten it — so nobody is shown as confirmed on
  // somebody else's confirmation.
  const userId = session?.user.id ?? null;
  const [grantUser, setGrantUser] = useState(userId);
  if (grantUser !== userId) {
    setGrantUser(userId);
    memGrant.current = null;
    setGrant(readReauthGrant());
  }

  // A grant shown as confirmed stops being shown the moment it expires, so the
  // User is asked to confirm again before they press anything.
  useEffect(() => {
    if (grant === null) return;
    let timer: number | undefined;
    const check = () => {
      const expiresAt = liveMemGrant()?.expiresAt ?? reauthGrantExpiresAt();
      if (expiresAt === null) {
        setGrant(null);
        return;
      }
      timer = window.setTimeout(check, Math.max(0, expiresAt - Date.now()));
    };
    check();
    return () => window.clearTimeout(timer);
  }, [grant]);

  const load = useCallback(async (signal?: AbortSignal) => {
    try {
      const view = await apiClient.listExternalIdentities(signal);
      if (signal?.aborted) return;
      setState({ status: "ready", view });
    } catch (err) {
      if (signal?.aborted) return;
      setState({ status: "error", message: errorMessage(err) });
    }
  }, []);

  useEffect(() => {
    const ctrl = new AbortController();
    void load(ctrl.signal);
    return () => ctrl.abort();
  }, [load]);

  const hasPassword = state.status === "ready" && state.view.hasPassword;
  const ready = hasPassword ? currentPassword !== "" : grant !== null;

  // takeProof is the proof an attach sends, read at the moment it is sent: a
  // kept grant is checked again for being live and still the one shown, or the
  // User confirms again and nothing is sent.
  function takeProof(): AttachProof | null {
    if (hasPassword) return currentPassword ? { currentPassword } : null;
    const live = liveGrant();
    if (!grant || live !== grant) {
      spent();
      setError(reauthExpiredMessage);
      return null;
    }
    return { reauthGrant: live };
  }

  // spent is called once an attach has presented proof: a grant is good once,
  // and a typed password is not kept on screen.
  function spent() {
    setCurrentPassword("");
    setGrant(null);
    memGrant.current = null;
    forgetReauthGrant();
  }

  async function onRedirect(provider: string) {
    setError(null);
    const proof = takeProof();
    if (!proof) return;
    try {
      const url = await apiClient.startAttachRedirect(provider, proof);
      spent();
      rememberRedirectSignIn({ from: "/profile", remember: true, attach: true });
      window.location.assign(url);
    } catch (err) {
      spent();
      setError(errorMessage(err));
    }
  }

  async function onRedirectReauth(provider: string) {
    setError(null);
    try {
      const url = await apiClient.startReauthRedirect(provider);
      rememberRedirectSignIn({ from: "/profile", remember: true, reauth: true });
      window.location.assign(url);
    } catch (err) {
      setError(errorMessage(err));
    }
  }

  return (
    <div className="app-shell" data-testid="profile-screen">
      <AppHeader />
      <main className="app-main">
        <h2 className="section-title">{session?.user.username ?? "Profile"}</h2>
        <section className="card">
        <h3 className="card-title">Sign-ins</h3>
        {state.status === "loading" && <p className="auth-subtitle">Loading…</p>}
        {state.status === "error" && (
          <p className="auth-error" role="alert">
            {state.message}
          </p>
        )}
        {state.status === "ready" && (
          <>
            {state.view.identities.length === 0 ? (
              <p className="auth-subtitle" data-testid="profile-identities-empty">
                No outside sign-ins are attached to your account.
              </p>
            ) : (
              <ul>
                {state.view.identities.map((x) => (
                  <li key={`${x.provider}/${x.username}`} data-testid={`profile-identity-${x.provider}`}>
                    {x.providerName} — {x.username}
                  </li>
                ))}
              </ul>
            )}
            {(state.view.password.length > 0 || state.view.redirect.length > 0) && (
              <>
                <h3 className="card-title">Attach a sign-in</h3>
                <p className="auth-subtitle">
                  Sign in with an outside account to attach it to this one. Afterwards, signing in with it
                  signs you in here.
                </p>
                {error && (
                  <p className="auth-error" data-testid="profile-attach-error" role="alert">
                    {error}
                  </p>
                )}
                {state.view.hasPassword ? (
                  <label className="field">
                    <span className="field-label">Your password</span>
                    <input
                      className="field-input"
                      type="password"
                      data-testid="profile-current-password"
                      autoComplete="current-password"
                      value={currentPassword}
                      onChange={(e) => setCurrentPassword(e.target.value)}
                    />
                  </label>
                ) : (
                  <ConfirmItIsYou
                    view={state.view}
                    confirmed={grant !== null}
                    onError={setError}
                    onGrant={(g, expiresIn) => {
                      setError(null);
                      keepReauthGrant(g, expiresIn);
                      memGrant.current = readReauthGrant() === g ? null : { grant: g, expiresAt: Date.now() + expiresIn * 1000 };
                      setGrant(g);
                    }}
                    onRedirect={(p) => void onRedirectReauth(p)}
                  />
                )}
                {state.view.password.map((p) => (
                  <PasswordAttachForm
                    key={p.id}
                    provider={p}
                    ready={ready}
                    takeProof={takeProof}
                    onSpent={spent}
                    onError={setError}
                    onAttached={() => {
                      setError(null);
                      void load();
                    }}
                  />
                ))}
                {state.view.redirect.map((p) => (
                  <button
                    key={p.id}
                    type="button"
                    className="auth-submit"
                    data-testid={`profile-attach-redirect-${p.id}`}
                    disabled={!ready}
                    onClick={() => void onRedirect(p.id)}
                  >
                    Attach {p.name}
                  </button>
                ))}
              </>
            )}
          </>
        )}
        </section>
      </main>
    </div>
  );
}

// ConfirmItIsYou is how a User without a Local password proves it is them before
// an attach: a sign-in through one they already hold, by whichever flow its
// provider uses. The server answers a grant, which the next attach carries.
function ConfirmItIsYou({
  view,
  confirmed,
  onError,
  onGrant,
  onRedirect,
}: {
  view: ExternalIdentitiesView;
  confirmed: boolean;
  onError: (message: string) => void;
  onGrant: (grant: string, expiresIn: number) => void;
  onRedirect: (provider: string) => void;
}) {
  if (confirmed) {
    return (
      <p className="auth-subtitle" data-testid="profile-reauth-confirmed">
        Confirmed. You can attach one sign-in now.
      </p>
    );
  }
  const held = new Set(view.identities.map((x) => x.provider));
  const password = view.password.filter((p) => held.has(p.id));
  const redirect = view.redirect.filter((p) => held.has(p.id));
  return (
    <>
      <p className="auth-subtitle">
        First confirm it is you: sign in again with a sign-in already attached to your account.
      </p>
      {password.length === 0 && redirect.length === 0 && (
        <p className="auth-subtitle" data-testid="profile-reauth-none">
          None of the sign-ins attached to your account can be used right now, so another cannot be attached.
        </p>
      )}
      {password.map((p) => (
        <ReauthPasswordForm key={p.id} provider={p} onError={onError} onGrant={onGrant} />
      ))}
      {redirect.map((p) => (
        <button
          key={p.id}
          type="button"
          className="auth-submit"
          data-testid={`profile-reauth-redirect-${p.id}`}
          onClick={() => onRedirect(p.id)}
        >
          Confirm with {p.name}
        </button>
      ))}
    </>
  );
}

// ReauthPasswordForm confirms it is the caller through a password-flow sign-in
// they already hold.
function ReauthPasswordForm({
  provider,
  onError,
  onGrant,
}: {
  provider: SignInProvider;
  onError: (message: string) => void;
  onGrant: (grant: string, expiresIn: number) => void;
}) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    try {
      const g = await apiClient.reauthPassword(provider.id, username, password);
      onGrant(g.grant, g.expiresIn);
    } catch (err) {
      onError(errorMessage(err));
    } finally {
      setPassword("");
      setBusy(false);
    }
  }

  return (
    <form className="profile-attach-form" onSubmit={onSubmit}>
      <span className="field-label">{provider.name}</span>
      <label className="field">
        <span className="field-label">Username</span>
        <input
          className="field-input"
          data-testid={`profile-reauth-username-${provider.id}`}
          autoComplete="off"
          value={username}
          onChange={(e) => setUsername(e.target.value)}
          required
        />
      </label>
      <label className="field">
        <span className="field-label">Password</span>
        <input
          className="field-input"
          type="password"
          data-testid={`profile-reauth-password-${provider.id}`}
          autoComplete="off"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          required
        />
      </label>
      <button
        type="submit"
        className="auth-submit"
        data-testid={`profile-reauth-submit-${provider.id}`}
        disabled={busy}
      >
        Confirm
      </button>
    </form>
  );
}

// PasswordAttachForm puts a username and password to one password-flow Sign-in
// provider, with the caller's proof; on the server's yes the identity it names
// is attached to the caller. Without proof it cannot be sent.
function PasswordAttachForm({
  provider,
  ready,
  takeProof,
  onSpent,
  onError,
  onAttached,
}: {
  provider: SignInProvider;
  ready: boolean;
  takeProof: () => AttachProof | null;
  onSpent: () => void;
  onError: (message: string) => void;
  onAttached: () => void;
}) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    const proof = takeProof();
    if (!proof) return;
    setBusy(true);
    try {
      await apiClient.attachPasswordIdentity(provider.id, username, password, proof);
      setUsername("");
      setPassword("");
      onAttached();
    } catch (err) {
      onError(errorMessage(err));
    } finally {
      onSpent();
      setPassword("");
      setBusy(false);
    }
  }

  return (
    <form className="profile-attach-form" onSubmit={onSubmit}>
      <span className="field-label">{provider.name}</span>
      <label className="field">
        <span className="field-label">Username</span>
        <input
          className="field-input"
          data-testid={`profile-attach-username-${provider.id}`}
          autoComplete="off"
          value={username}
          onChange={(e) => setUsername(e.target.value)}
          required
        />
      </label>
      <label className="field">
        <span className="field-label">Password</span>
        <input
          className="field-input"
          type="password"
          data-testid={`profile-attach-password-${provider.id}`}
          autoComplete="off"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          required
        />
      </label>
      <button
        type="submit"
        className="auth-submit"
        data-testid={`profile-attach-submit-${provider.id}`}
        disabled={busy || !ready}
      >
        Attach
      </button>
    </form>
  );
}
