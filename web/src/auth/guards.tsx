import type { ReactNode } from "react";
import { Navigate, useLocation } from "react-router-dom";
import { useAuth } from "./session";

// Route guards (PRD: role gating + a global 401 path).
//
// RequireAuth gates any authenticated area: no session → bounce to /login,
// remembering where the user was headed so login can return them. RequireAdmin
// layers role on top: a logged-in non-Admin is sent to the authed landing (not
// the login screen — they ARE logged in, just not an Admin). Both wait for the
// provider's initial hydrate (`ready`) so a stored session isn't misjudged as
// logged-out on the first paint.

export function RequireAuth({ children }: { children: ReactNode }) {
  const { isAuthenticated, ready } = useAuth();
  const location = useLocation();

  if (!ready) return <HydrationGate />;
  if (!isAuthenticated) {
    return <Navigate to="/login" replace state={{ from: location }} />;
  }
  return <>{children}</>;
}

export function RequireAdmin({ children }: { children: ReactNode }) {
  const { isAuthenticated, isAdmin, ready } = useAuth();
  const location = useLocation();

  if (!ready) return <HydrationGate />;
  if (!isAuthenticated) {
    return <Navigate to="/login" replace state={{ from: location }} />;
  }
  if (!isAdmin) {
    // Logged in but not permitted: send to the landing rather than login. The
    // server still enforces this (403/404); the gate just keeps the UI honest.
    return <Navigate to="/" replace />;
  }
  return <>{children}</>;
}

// HydrationGate is the brief placeholder shown while the auth provider reads the
// stored session on first mount (one tick). Keeping it minimal avoids a flash of
// the login screen for a returning, already-authenticated user.
function HydrationGate() {
  return (
    <div className="app-shell" data-testid="auth-hydrating">
      <main className="app-main">
        <p className="status status-loading">Restoring session&hellip;</p>
      </main>
    </div>
  );
}
