import { useEffect, useRef, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { keepReauthGrant } from "../auth/reauthGrant";
import { safeReturnPath } from "../auth/returnPath";
import { apiClient } from "../api/client";
import { useAuth } from "../auth/session";
import { errorMessage } from "./errorMessage";

// Where a redirect-flow Sign-in provider sends the browser back to (ADR-0063
// decision 2). The screen owns nothing of the round trip: it posts the state and
// code it was handed to the server, which checks them against what it minted —
// and against the cookie it set on this browser when the sign-in started — and
// either signs the person in or refuses. On success it returns to wherever the
// login screen was headed; otherwise it says why and links back.
//
// A round trip the profile started is an ATTACH, not a sign-in (ADR-0063
// decision 3): the same return address, so an operator registers one redirect
// URI, but the state and code go to the attach callback as the signed-in User,
// and the screen goes back to the profile either way. So does a RE-AUTH the
// profile started: the grant it answers is kept for the profile's next attach.

const PENDING_KEY = "obelo.signIn.redirect";

interface PendingRedirect {
  from: string;
  remember: boolean;
  /** Set by the profile: finish an attach rather than a sign-in. */
  attach?: boolean;
  /** Set by the profile: finish a re-auth rather than a sign-in. */
  reauth?: boolean;
}

/** Remember, across the round trip to the provider, where to go afterwards and
 * whether to keep the session. Called by the login screen just before leaving. */
export function rememberRedirectSignIn(p: PendingRedirect): void {
  try {
    window.sessionStorage.setItem(PENDING_KEY, JSON.stringify(p));
  } catch {
    // Storage unavailable: the callback falls back to Home, remembered.
  }
}

function takePendingRedirect(): PendingRedirect {
  try {
    const raw = window.sessionStorage.getItem(PENDING_KEY);
    window.sessionStorage.removeItem(PENDING_KEY);
    if (raw) {
      const p = JSON.parse(raw) as Partial<PendingRedirect>;
      return {
        from: safeReturnPath(p.from),
        remember: p.remember !== false,
        attach: p.attach === true,
        reauth: p.reauth === true,
      };
    }
  } catch {
    /* fall through */
  }
  return { from: "/", remember: true };
}

export default function SignInCallbackScreen() {
  const [params] = useSearchParams();
  const navigate = useNavigate();
  const { completeRedirectSignIn } = useAuth();
  const [error, setError] = useState<string | null>(null);
  const [mode, setMode] = useState<"signIn" | "attach" | "reauth">("signIn");
  const started = useRef(false);

  useEffect(() => {
    // Once: a state is good for one attempt, and a second post would be refused.
    if (started.current) return;
    started.current = true;
    const state = params.get("state");
    const code = params.get("code");
    const pending = takePendingRedirect();
    setMode(pending.reauth ? "reauth" : pending.attach ? "attach" : "signIn");
    if (params.get("error") || !state || !code) {
      setError("The sign-in provider did not sign you in.");
      return;
    }
    if (pending.reauth) {
      apiClient
        .completeReauthRedirect(state, code)
        .then((g) => {
          keepReauthGrant(g.grant, g.expiresIn);
          navigate(pending.from, { replace: true });
        })
        .catch((err) => setError(errorMessage(err)));
      return;
    }
    if (pending.attach) {
      apiClient
        .completeAttachRedirect(state, code)
        .then(() => navigate(pending.from, { replace: true }))
        .catch((err) => setError(errorMessage(err)));
      return;
    }
    completeRedirectSignIn(state, code, pending.remember)
      .then(() => navigate(pending.from, { replace: true }))
      .catch((err) => setError(errorMessage(err)));
  }, [params, navigate, completeRedirectSignIn]);

  const fromProfile = mode !== "signIn";
  return (
    <div className="auth-shell" data-testid="sign-in-callback">
      <div className="auth-card">
        <h1 className="auth-title">
          {mode === "reauth" ? "Confirm it is you" : mode === "attach" ? "Attach a sign-in" : "Sign in"}
        </h1>
        {error ? (
          <>
            <p className="auth-error" data-testid="sign-in-callback-error" role="alert">
              {error}
            </p>
            <Link to={fromProfile ? "/profile" : "/login"} data-testid="sign-in-callback-back">
              {fromProfile ? "Back to your profile" : "Back to sign in"}
            </Link>
          </>
        ) : (
          <p className="auth-subtitle">
            {mode === "reauth" ? "Confirming…" : mode === "attach" ? "Attaching…" : "Signing you in…"}
          </p>
        )}
      </div>
    </div>
  );
}
