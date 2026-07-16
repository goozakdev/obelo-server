import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { useParams } from "react-router-dom";
import { apiClient } from "../api/client";
import { errorMessage } from "./errorMessage";

// LinkScreen is the phone's half of the Device authorization grant (ADR-0036):
// the page a QR code on a TV sends you to. It sits behind RequireAuth, which is
// the entire reason the "sign in first, then approve" behaviour needs no code
// here — the guard bounces an anonymous visitor to /login and returns them.
//
// The code arrives as a PATH parameter (/link/:code), never a query string, and
// that is load-bearing rather than stylistic: LoginScreen restores only
// `location.state.from.pathname` after a bounce, so `/link?code=K7R9` would come
// back from the login screen as a bare `/link` with the code silently gone. A
// path segment survives the round trip. If that ever changes, this screen still
// works — it just makes the user type what the QR already knew.
//
// Approval is immediate: arriving with a code in the URL signs the TV in with no
// confirmation step. That is a deliberate product choice (fewest taps from
// scan to watching) and it is why this screen names the Device it authorized in
// the success state — with no confirm gate, that line is the only chance a user
// gets to notice they approved something they did not mean to. The recourse is
// Devices → revoke, which kills the token instantly (ADR-0015).

type Phase =
  | { kind: "entering" }
  | { kind: "approving" }
  | { kind: "approved"; deviceName: string }
  | { kind: "failed"; message: string };

export default function LinkScreen() {
  const { code: codeFromUrl } = useParams<{ code?: string }>();
  const [typed, setTyped] = useState("");
  const [phase, setPhase] = useState<Phase>({ kind: "entering" });

  // A scanned QR must approve exactly once. Without this guard React's dev-mode
  // double-invoked effect would fire two approvals, and the second would hit an
  // already-redeemed code and report a spurious failure over the real success.
  const attempted = useRef(false);

  const approve = useCallback(async (code: string) => {
    setPhase({ kind: "approving" });
    try {
      const res = await apiClient.approveDeviceCode(code);
      setPhase({ kind: "approved", deviceName: res.device.name });
    } catch (err) {
      setPhase({ kind: "failed", message: errorMessage(err) });
    }
  }, []);

  useEffect(() => {
    if (!codeFromUrl || attempted.current) return;
    attempted.current = true;
    void approve(codeFromUrl);
  }, [codeFromUrl, approve]);

  function onSubmit(e: FormEvent) {
    e.preventDefault();
    void approve(typed);
  }

  if (phase.kind === "approving") {
    return (
      <Shell testId="link-approving">
        <div className="auth-card">
          <h1 className="auth-title">Signing in your TV&hellip;</h1>
        </div>
      </Shell>
    );
  }

  if (phase.kind === "approved") {
    return (
      <Shell testId="link-approved">
        <div className="auth-card">
          <h1 className="auth-title">You&rsquo;re all set</h1>
        {/* Naming the Device is the whole point of this state — see the header
            comment on why there is no confirmation step to name it earlier. */}
          <p className="auth-subtitle" data-testid="link-approved-device">
            <strong>{phase.deviceName}</strong> is now signed in. You can put
            your phone down and keep going on the TV.
          </p>
        </div>
      </Shell>
    );
  }

  return (
    <Shell testId="link-screen">
      <form className="auth-card" onSubmit={onSubmit}>
        <h1 className="auth-title">Sign in your TV</h1>
        <p className="auth-subtitle">
          Enter the 4-character code shown on your TV screen.
        </p>

        <label className="field">
          <span className="field-label">Code from your TV</span>
          <input
            data-testid="link-code"
            className="field-input"
            type="text"
            // A 4-char code is short enough that autocorrect and autocapitalize
            // do more harm than good: iOS will happily "fix" K7R9 into a word.
            autoCapitalize="characters"
            autoCorrect="off"
            autoComplete="off"
            spellCheck={false}
            inputMode="text"
            maxLength={8}
            value={typed}
            onChange={(e) => setTyped(e.target.value)}
            required
          />
        </label>

        {phase.kind === "failed" && (
          <p className="auth-error" data-testid="link-error" role="alert">
            {phase.message}
          </p>
        )}

        <button className="auth-submit" data-testid="link-submit" type="submit">
          Sign in TV
        </button>
      </form>
    </Shell>
  );
}

// Shell is only the page frame; each state supplies its own .auth-card, because
// the entry state's card IS the <form> and a card wrapping a card would collect
// the padding and border twice.
function Shell({
  testId,
  children,
}: {
  testId: string;
  children: React.ReactNode;
}) {
  return (
    <div className="auth-shell" data-testid={testId}>
      {children}
    </div>
  );
}
