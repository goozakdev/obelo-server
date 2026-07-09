import { Link } from "react-router-dom";
import { apiClient } from "../api/client";
import { useAsync } from "./useAsync";
import AppHeader from "./AppHeader";

// The library list (issue 03 / PRD user story 8): fetch GET /libraries and let
// the user open one into its poster grid. Single library is the common case, so
// 0/1/many are all handled: zero → a clean empty state; the list otherwise
// renders each Library as a link to its grid.
//
// Note: GET /libraries is Admin-only on the current backend (single-Admin); a
// non-Admin would get a 403 the API client surfaces as a readable error here.

export default function LibraryListScreen() {
  const state = useAsync((signal) => apiClient.listLibraries(signal), []);

  return (
    <div className="app-shell" data-testid="library-list-screen">
      <AppHeader />
      <main className="app-main app-main-wide">
        <h2 className="section-title">Libraries</h2>

        {state.status === "loading" && (
          <p className="status status-loading" data-testid="libraries-loading">
            Loading libraries&hellip;
          </p>
        )}

        {state.status === "error" && (
          <p className="status status-error" data-testid="libraries-error" role="alert">
            <span className="dot dot-error" aria-hidden="true" />
            {state.message}
          </p>
        )}

        {state.status === "ready" && state.data.length === 0 && (
          <div className="card" data-testid="libraries-empty">
            <p className="status status-loading">
              No libraries yet. An Admin can create one and run a scan.
            </p>
          </div>
        )}

        {state.status === "ready" && state.data.length > 0 && (
          <ul className="library-list" data-testid="libraries">
            {state.data.map((lib) => (
              <li key={lib.id}>
                <Link
                  className="library-card"
                  // A music library opens the separate music experience
                  // (/music/...); TV/Movie libraries use the shared grid.
                  to={
                    lib.kind === "music"
                      ? `/music/libraries/${lib.id}`
                      : `/libraries/${lib.id}`
                  }
                  data-testid="library-item"
                  data-library-id={lib.id}
                >
                  <span className="library-name">{lib.name}</span>
                  <span className="library-kind">{lib.kind}</span>
                  <span className="library-roots">
                    {lib.rootFolders.length}{" "}
                    {lib.rootFolders.length === 1 ? "folder" : "folders"}
                  </span>
                </Link>
              </li>
            ))}
          </ul>
        )}
      </main>
    </div>
  );
}
