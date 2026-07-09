import { Link } from "react-router-dom";
import { apiClient } from "../api/client";
import type { TitleSummary } from "../api/types";
import { useAsync } from "../browse/useAsync";
import AppHeader from "../browse/AppHeader";
import PosterTile from "../browse/PosterTile";

// The Home landing (issue 05): the two per-User computed rows from GET /home —
// Continue Watching (in-progress titles, most-recent first) and Recently Added
// (newest-added first). Each card reuses the grid's PosterTile (so a card looks
// and behaves identically everywhere) and links to the Title's detail/play page.
//
// The server computes both rows; we just render what it returns. A title watched
// past the threshold drops out of Continue Watching server-side, so we render the
// result without any client-side watch logic. A brand-new/empty server yields two
// empty rows, each shown with a sensible empty state.
//
// This screen keeps the `home-screen` testid and a working link into the
// libraries area (the auth/browse E2E specs rely on `home-screen` and click
// `browse-link`), so issues 01–04 don't regress.

export default function HomeScreen() {
  const home = useAsync((signal) => apiClient.getHome(signal), []);

  return (
    <div className="app-shell" data-testid="home-screen">
      <AppHeader />
      <main className="app-main app-main-wide">
        {home.status === "loading" && (
          <p className="status status-loading" data-testid="home-loading">
            Loading your home&hellip;
          </p>
        )}

        {home.status === "error" && (
          <p className="status status-error" data-testid="home-error" role="alert">
            <span className="dot dot-error" aria-hidden="true" />
            {home.message}
          </p>
        )}

        {home.status === "ready" && (
          <>
            <HomeRow
              testId="home-continue-watching"
              heading="Continue Watching"
              titles={home.data.continueWatching}
              emptyMessage="Nothing in progress yet. Start a movie and it'll show up here."
            />
            {/* Up Next (TV-only, issue tv-music/02): the next unwatched Episode in
                Show order for each Show the user has started. The server computes
                and advances it (on crossing the ~90% threshold or a manual toggle);
                we render only what it returns. The row is hidden entirely when no
                Show is in progress, so a Movie-only library never shows an empty
                TV row. */}
            {(home.data.upNext ?? []).length > 0 && (
              <HomeRow
                testId="home-up-next"
                heading="Up Next"
                titles={home.data.upNext}
                emptyMessage=""
              />
            )}
            <HomeRow
              testId="home-recently-added"
              heading="Recently Added"
              titles={home.data.recentlyAdded}
              emptyMessage="Nothing added yet. Scan a library to fill this in."
            />
            <p className="home-browse">
              <Link className="nav-link" to="/libraries" data-testid="browse-link">
                Browse libraries
              </Link>
            </p>
          </>
        )}
      </main>
    </div>
  );
}

interface HomeRowProps {
  testId: string;
  heading: string;
  titles: TitleSummary[];
  emptyMessage: string;
}

// One Home row: a heading plus a horizontal strip of poster cards, or a sensible
// empty state when the row has no items. The strip reuses the same `poster-grid`
// cards as the library grid (PosterTile) so cards are identical everywhere; the
// `home-row` wrapper just makes the row scroll horizontally.
function HomeRow({ testId, heading, titles, emptyMessage }: HomeRowProps) {
  return (
    <section className="home-row" data-testid={testId}>
      <h2 className="section-title">{heading}</h2>
      {titles.length === 0 ? (
        <p
          className="status status-loading"
          data-testid={`${testId}-empty`}
        >
          {emptyMessage}
        </p>
      ) : (
        <ul className="poster-grid poster-row" data-testid={`${testId}-items`}>
          {titles.map((t) => (
            <PosterTile key={t.id} title={t} />
          ))}
        </ul>
      )}
    </section>
  );
}
