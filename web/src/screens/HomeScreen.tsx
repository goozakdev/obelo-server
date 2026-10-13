import { useState } from "react";
import { Link } from "react-router-dom";
import { apiClient } from "../api/client";
import type { HomeRow, HomeRows as HomeRowsData, OnlineSource, TitleSummary } from "../api/types";
import { useAsync } from "../browse/useAsync";
import AppHeader from "../browse/AppHeader";
import PosterTile from "../browse/PosterTile";
import { ShowTile } from "../browse/ShowGrid";
import { seeAllPath } from "../browse/homeRows";

// The Home landing (issue 05): the per-User rows from GET /home — Continue
// Watching, Up Next, My List, Recently Added and the Discovery rows, in the
// server's order (or the three fixed rows from an older Server). Each card reuses the grid's PosterTile (so a card looks
// and behaves identically everywhere) and links to the Title's detail/play page.
//
// The server computes both rows; we just render what it returns. A title watched
// past the threshold drops out of Continue Watching server-side, so we render the
// result without any client-side watch logic. A brand-new/empty server yields no
// rows with items, shown as one Home-level empty state.
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
            <OnlineSourceTiles />
            {home.data.rows ? (
              <HomeRows rows={home.data.rows} />
            ) : (
              <LegacyRows data={home.data} />
            )}
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

// The server's ordered `rows` (features.homeRows): rendered as sent — the order
// and the choice of Discovery rows are the server's, never hard-coded here. The
// server omits empty rows, and we skip any that arrive empty anyway, along with
// any kind this client does not know (forward compatibility). When nothing is
// left, Home shows one empty state instead.
const KNOWN_KINDS: readonly string[] = ["continueWatching", "upNext", "myList", "recentlyAdded", "discovery"];
const BUILT_IN_TEST_IDS: Partial<Record<HomeRow["kind"], string>> = {
  continueWatching: "home-continue-watching",
  upNext: "home-up-next",
  recentlyAdded: "home-recently-added",
};

function HomeRows({ rows }: { rows: HomeRow[] }) {
  const shown = rows.filter(
    (r) => KNOWN_KINDS.includes(r.kind) && r.titles.length + r.shows.length > 0,
  );
  if (shown.length === 0) {
    return (
      <p className="status status-loading" data-testid="home-empty">
        Nothing added yet. Scan a library to fill this in.
      </p>
    );
  }
  return (
    <div data-testid="home-rows">
      {shown.map((r, i) => {
        const seeAll = seeAllPath(r);
        // The built-in rows keep the testids the fixed layout gave them.
        const testId = BUILT_IN_TEST_IDS[r.kind] ?? `home-row-${i}`;
        return (
          <section className="home-row" data-testid={testId} data-row-kind={r.kind} key={`${r.kind}-${i}`}>
            <h2 className="section-title">{r.title}</h2>
            {seeAll && (
              <Link className="nav-link" to={seeAll} data-testid="home-row-see-all">
                See all
              </Link>
            )}
            <ul className="poster-grid poster-row" data-testid={`${testId}-items`}>
              {r.shows.map((s) => (
                <ShowTile key={s.id} show={s} />
              ))}
              {r.titles.map((t) => (
                <PosterTile key={t.id} title={t} />
              ))}
            </ul>
          </section>
        );
      })}
    </div>
  );
}

// The three fixed rows, for a Server that predates `rows`.
type LegacyHome = Pick<HomeRowsData, "continueWatching" | "upNext" | "recentlyAdded">;

function LegacyRows({ data }: { data: LegacyHome }) {
  return (
    <>
      <HomeRow
        testId="home-continue-watching"
        heading="Continue Watching"
        titles={data.continueWatching}
        emptyMessage="Nothing in progress yet. Start a movie and it'll show up here."
      />
      {/* Up Next (TV-only, issue tv-music/02): the next unwatched Episode in
          Show order for each Show the user has started. The server computes
          and advances it (on crossing the ~90% threshold or a manual toggle);
          we render only what it returns. The row is hidden entirely when no
          Show is in progress, so a Movie-only library never shows an empty
          TV row. */}
      {(data.upNext ?? []).length > 0 && (
        <HomeRow
          testId="home-up-next"
          heading="Up Next"
          titles={data.upNext}
          emptyMessage=""
        />
      )}
      <HomeRow
        testId="home-recently-added"
        heading="Recently Added"
        titles={data.recentlyAdded}
        emptyMessage="Nothing added yet. Scan a library to fill this in."
      />
    </>
  );
}

// One tile per enabled Online source (ADR-0068), from its own endpoint — the Home
// response never carries sources, and listing them calls no Plugin. It renders
// nothing while loading, on failure, and when the caller has no source to show (a
// Member, or a server with none enabled), so Home is exactly what it was for them;
// a failure here must never cost the Home screen its own rows.
function OnlineSourceTiles() {
  const sources = useAsync((signal) => apiClient.getOnlineSources(signal), []);
  if (sources.status !== "ready" || sources.data.length === 0) return null;
  return (
    <section className="home-row" data-testid="home-online-sources">
      <h2 className="section-title">Online</h2>
      <ul className="home-online-sources">
        {sources.data.map((s) => (
          <li key={s.id}>
            <OnlineSourceTile source={s} />
          </li>
        ))}
      </ul>
    </section>
  );
}

// One source tile: the icon from the Server's own path (a browser <img> carries the
// media cookie), or a generic tile when the package had none or the image will not
// load. The source's name is always shown, so the tile reads the same either way.
function OnlineSourceTile({ source }: { source: OnlineSource }) {
  const [broken, setBroken] = useState(false);
  return (
    <Link
      className="nav-link online-source-tile"
      to={`/online/${encodeURIComponent(source.id)}`}
      data-testid="online-source-tile"
    >
      {source.iconUrl && !broken ? (
        <img
          src={source.iconUrl}
          alt=""
          className="online-source-tile-icon"
          data-testid="online-source-tile-icon"
          onError={() => setBroken(true)}
        />
      ) : (
        <span className="online-source-tile-icon online-source-tile-generic" data-testid="online-source-tile-generic" aria-hidden="true">
          {source.name.trim().charAt(0).toUpperCase()}
        </span>
      )}
      <span className="online-source-tile-name">{source.name}</span>
    </Link>
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
