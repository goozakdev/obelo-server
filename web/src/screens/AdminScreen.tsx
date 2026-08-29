import { Navigate, NavLink, Route, Routes } from "react-router-dom";
import AppHeader from "../browse/AppHeader";
import AdminLibrariesScreen from "../admin/AdminLibrariesScreen";
import AdminNeedsFixingScreen from "../admin/AdminNeedsFixingScreen";
import AdminDevicesScreen from "../admin/AdminDevicesScreen";
import AdminUsersScreen from "../admin/AdminUsersScreen";
import AdminProvidersScreen from "../admin/AdminProvidersScreen";
import AdminSubtitleProvidersScreen from "../admin/AdminSubtitleProvidersScreen";
import AdminTranscodingScreen from "../admin/AdminTranscodingScreen";
import AdminRemoteAccessScreen from "../admin/AdminRemoteAccessScreen";
import { useOptionalFeature } from "../serverInfoContext";

// The /admin hub. Issue 06 built the libraries + scanning view; issue 07 adds the
// attention surfaces (needs-review / Unmatched / fix-match / overrides) and the
// devices management, as tabbed sub-routes that share this chrome:
//   /admin           libraries + scanning  (issue 06, unchanged)
//   /admin/needs-fixing  the one queue of everything wrong in a library, with the
//                    provider search that fixes it (was /admin/attention, which
//                    still redirects here)
//   /admin/devices   the signed-in user's devices, with revoke
//   /admin/users     manage Users — list + create Member + delete
//                    (access-control-admin-ui issue 01)
//   /admin/remote-access  the Tailnet node's lifecycle (ADR-0043) — gated on the
//                    handshake's `tailscale` feature flag, so a build without it
//                    has neither the tab nor the route
// All gated by RequireAdmin (App.tsx) and still server-enforced. This screen
// shares the site-wide AppHeader so the top chrome is identical everywhere; the
// admin-tabs nav below provides the sub-navigation.

export default function AdminScreen() {
  // Remote access (ADR-0043) is compiled in only under `-tags tailscale`, and a
  // build without it says so in the handshake. ABSENT AND FALSE MUST LOOK THE
  // SAME: no tab, no route, and therefore no fetch — a build without the feature
  // should look like a build that never had it, not like a broken one. The
  // optional read also degrades to "off" outside a ServerInfoProvider, which is
  // the same answer for the same reason.
  const remoteAccess = useOptionalFeature("tailscale");
  return (
    <div className="app-shell" data-testid="admin-screen">
      <AppHeader />
      <main className="app-main app-main-wide">
        <h1 className="app-title admin-page-title">Admin</h1>

        {/* Side-rail layout: the tab nav is a vertical rail on the left, the
            active sub-screen fills the remaining width (admin-layout CSS). */}
        <div className="admin-layout">
          <nav className="admin-tabs" data-testid="admin-tabs">
            <NavLink
              to="/admin"
              end
              className="admin-tab"
              data-testid="admin-tab-libraries"
            >
              Libraries
            </NavLink>
            <NavLink
              to="/admin/needs-fixing"
              className="admin-tab"
              data-testid="admin-tab-needs-fixing"
            >
              Needs Fixing
            </NavLink>
            <NavLink
              to="/admin/devices"
              className="admin-tab"
              data-testid="admin-tab-devices"
            >
              Devices
            </NavLink>
            <NavLink
              to="/admin/users"
              className="admin-tab"
              data-testid="admin-tab-users"
            >
              Users
            </NavLink>
            <NavLink
              to="/admin/providers"
              className="admin-tab"
              data-testid="admin-tab-providers"
            >
              Metadata Providers
            </NavLink>
            <NavLink
              to="/admin/subtitles"
              className="admin-tab"
              data-testid="admin-tab-subtitles"
            >
              Subtitle Providers
            </NavLink>
            <NavLink
              to="/admin/transcoding"
              className="admin-tab"
              data-testid="admin-tab-transcoding"
            >
              Transcoding
            </NavLink>
            {remoteAccess && (
              <NavLink
                to="/admin/remote-access"
                className="admin-tab"
                data-testid="admin-tab-remote-access"
              >
                Remote access
              </NavLink>
            )}
          </nav>

          <div className="admin-content">
            <Routes>
              <Route index element={<AdminLibrariesScreen />} />
              <Route path="needs-fixing" element={<AdminNeedsFixingScreen />} />
              {/* The tab was called "Attention" through issue 07; keep its path
                  working so a bookmark or an old link still lands somewhere. */}
              <Route
                path="attention"
                element={<Navigate to="/admin/needs-fixing" replace />}
              />
              <Route path="devices" element={<AdminDevicesScreen />} />
              <Route path="users" element={<AdminUsersScreen />} />
              <Route path="providers" element={<AdminProvidersScreen />} />
              <Route
                path="subtitles"
                element={<AdminSubtitleProvidersScreen />}
              />
              <Route path="transcoding" element={<AdminTranscodingScreen />} />
              {remoteAccess && (
                <Route
                  path="remote-access"
                  element={<AdminRemoteAccessScreen />}
                />
              )}
            </Routes>
          </div>
        </div>
      </main>
    </div>
  );
}
