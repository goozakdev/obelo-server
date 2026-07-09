import { NavLink, Route, Routes } from "react-router-dom";
import AppHeader from "../browse/AppHeader";
import AdminLibrariesScreen from "../admin/AdminLibrariesScreen";
import AdminAttentionScreen from "../admin/AdminAttentionScreen";
import AdminDevicesScreen from "../admin/AdminDevicesScreen";
import AdminUsersScreen from "../admin/AdminUsersScreen";
import AdminProvidersScreen from "../admin/AdminProvidersScreen";
import AdminSubtitleProvidersScreen from "../admin/AdminSubtitleProvidersScreen";

// The /admin hub. Issue 06 built the libraries + scanning view; issue 07 adds the
// attention surfaces (needs-review / Unmatched / fix-match / overrides) and the
// devices management, as tabbed sub-routes that share this chrome:
//   /admin           libraries + scanning  (issue 06, unchanged)
//   /admin/attention needs-review / Unmatched / fix-match / overrides
//   /admin/devices   the signed-in user's devices, with revoke
//   /admin/users     manage Users — list + create Member + delete
//                    (access-control-admin-ui issue 01)
// All gated by RequireAdmin (App.tsx) and still server-enforced. This screen
// shares the site-wide AppHeader so the top chrome is identical everywhere; the
// admin-tabs nav below provides the sub-navigation.

export default function AdminScreen() {
  return (
    <div className="app-shell" data-testid="admin-screen">
      <AppHeader />
      <main className="app-main app-main-wide">
        <h1 className="app-title admin-page-title">Admin</h1>

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
            to="/admin/attention"
            className="admin-tab"
            data-testid="admin-tab-attention"
          >
            Attention
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
        </nav>

        <Routes>
          <Route index element={<AdminLibrariesScreen />} />
          <Route path="attention" element={<AdminAttentionScreen />} />
          <Route path="devices" element={<AdminDevicesScreen />} />
          <Route path="users" element={<AdminUsersScreen />} />
          <Route path="providers" element={<AdminProvidersScreen />} />
          <Route path="subtitles" element={<AdminSubtitleProvidersScreen />} />
        </Routes>
      </main>
    </div>
  );
}
