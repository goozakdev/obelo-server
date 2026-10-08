import { useEffect, useRef, useState, type FormEvent } from "react";
import { useParams } from "react-router-dom";
import { apiClient } from "../api/client";
import type { OnlineItem, OnlineRow, OnlineSource } from "../api/types";
import { useAsync } from "../browse/useAsync";
import AppHeader from "../browse/AppHeader";
import { entryFromOnlineItem } from "../player/queue/model";
import { useQueue } from "../player/queue/useQueue";
import { formatTimecode } from "../time";

// An Online source's page (ADR-0068): its Online rows and Online items, and a
// click on an item plays it in the persistent player, replacing the Queue with
// that one item. The page is a live read of the source — nothing is stored — and
// a failed load says "{source} isn't responding" with a retry, in place of rows.
//
// The source's name comes from the tile list (cheap, never calls a Plugin), so the
// failure message can name the source even when the rows call is what failed.
//
// A search box sits under the name (this page only, never global search). A
// submitted query replaces the rows with the source's results, each submit asks the
// Plugin afresh, and a failure says the same "isn't responding" in place of the
// results while the box stays usable. A blank submit returns to the rows.

type SearchState =
  | { status: "idle" }
  | { status: "loading"; query: string }
  | { status: "error"; query: string }
  | { status: "ready"; query: string; items: OnlineItem[] };

export default function OnlineSourceScreen() {
  const { sourceId = "" } = useParams<{ sourceId: string }>();
  const queue = useQueue();
  const [attempt, setAttempt] = useState(0);
  const [search, setSearch] = useState<SearchState>({ status: "idle" });
  const [draft, setDraft] = useState("");
  // Only the latest submit may land: an older one still in flight is aborted.
  const searchAbort = useRef<AbortController | null>(null);
  useEffect(() => () => searchAbort.current?.abort(), []);
  const sources = useAsync((signal) => apiClient.getOnlineSources(signal), []);
  const rows = useAsync((signal) => apiClient.getOnlineRows(sourceId, signal), [sourceId, attempt]);

  const sourceName =
    sources.status === "ready"
      ? (sources.data.find((s: OnlineSource) => s.id === sourceId)?.name ?? sourceId)
      : sourceId;

  async function submitSearch(e: FormEvent) {
    e.preventDefault();
    searchAbort.current?.abort();
    const query = draft.trim();
    if (query === "") {
      setSearch({ status: "idle" });
      return;
    }
    const controller = new AbortController();
    searchAbort.current = controller;
    setSearch({ status: "loading", query });
    try {
      const items = await apiClient.searchOnlineItems(sourceId, query, controller.signal);
      if (!controller.signal.aborted) setSearch({ status: "ready", query, items });
    } catch {
      if (!controller.signal.aborted) setSearch({ status: "error", query });
    }
  }

  function play(item: OnlineItem) {
    queue.playNow([
      entryFromOnlineItem({
        sourceId,
        sourceName,
        itemId: item.id,
        title: item.title,
        thumbnailUrl: item.thumbnailUrl,
        durationMs: item.durationMs,
      }),
    ]);
  }

  return (
    <div className="app-shell" data-testid="online-source-screen">
      <AppHeader />
      <main className="app-main app-main-wide">
        <h2 className="section-title" data-testid="online-source-name">
          {sourceName}
        </h2>

        <form className="online-search" role="search" data-testid="online-search-form" onSubmit={submitSearch}>
          <input
            type="search"
            className="online-search-input"
            data-testid="online-search-input"
            aria-label={`Search ${sourceName}`}
            placeholder={`Search ${sourceName}`}
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
          />
          <button type="submit" className="nav-link" data-testid="online-search-submit">
            Search
          </button>
        </form>

        {search.status !== "idle" && <SearchResults sourceName={sourceName} search={search} onPlay={play} />}

        {search.status === "idle" && rows.status === "loading" && (
          <p className="status status-loading" data-testid="online-source-loading">
            Loading {sourceName}&hellip;
          </p>
        )}

        {search.status === "idle" && rows.status === "error" && (
          <div className="status status-error" role="alert" data-testid="online-source-error">
            <span className="dot dot-error" aria-hidden="true" />
            {sourceName} isn&rsquo;t responding.{" "}
            <button
              type="button"
              className="nav-link"
              data-testid="online-source-retry"
              onClick={() => setAttempt((n) => n + 1)}
            >
              Retry
            </button>
          </div>
        )}

        {search.status === "idle" && rows.status === "ready" && rows.data.length === 0 && (
          <p className="status status-loading" data-testid="online-source-empty">
            {sourceName} has nothing to show right now.
          </p>
        )}

        {search.status === "idle" &&
          rows.status === "ready" &&
          rows.data.map((row) => (
            <OnlineRowSection
              key={`${sourceId}-${attempt}-${row.id}`}
              sourceId={sourceId}
              sourceName={sourceName}
              row={row}
              onPlay={play}
            />
          ))}
      </main>
    </div>
  );
}

// What a submitted query shows in place of the rows: loading, the source's
// "isn't responding" message, "nothing matched", or the results.
function SearchResults({
  sourceName,
  search,
  onPlay,
}: {
  sourceName: string;
  search: Exclude<SearchState, { status: "idle" }>;
  onPlay: (item: OnlineItem) => void;
}) {
  if (search.status === "loading") {
    return (
      <p className="status status-loading" data-testid="online-search-loading">
        Searching {sourceName}&hellip;
      </p>
    );
  }
  if (search.status === "error") {
    return (
      <div className="status status-error" role="alert" data-testid="online-search-error">
        <span className="dot dot-error" aria-hidden="true" />
        {sourceName} isn&rsquo;t responding.
      </div>
    );
  }
  if (search.items.length === 0) {
    return (
      <p className="status status-loading" data-testid="online-search-empty">
        {sourceName} found nothing for &ldquo;{search.query}&rdquo;.
      </p>
    );
  }
  return (
    <section className="home-row" data-testid="online-search-results">
      <h3 className="section-title">Results for &ldquo;{search.query}&rdquo;</h3>
      <ul className="poster-grid poster-row">
        {search.items.map((item) => (
          <OnlineItemTile key={item.id} item={item} onPlay={onPlay} />
        ))}
      </ul>
    </section>
  );
}

// One Online item: a button that plays it.
function OnlineItemTile({ item, onPlay }: { item: OnlineItem; onPlay: (item: OnlineItem) => void }) {
  return (
    <li className="poster-tile online-item" data-testid="online-item">
      <button
        type="button"
        className="poster-link online-item-play"
        data-testid={`online-item-play-${item.id}`}
        onClick={() => onPlay(item)}
      >
        <div className="poster-frame online-thumb">
          <img src={item.thumbnailUrl} alt="" className="poster-img" loading="lazy" />
        </div>
        <div className="poster-caption">
          <span className="poster-title" data-testid="online-item-title">
            {item.title}
          </span>
          <span className="poster-year">{formatTimecode(item.durationMs)}</span>
          {item.description && <span className="online-item-description">{item.description}</span>}
          {item.publishedAt && (
            <span className="online-item-published">{new Date(item.publishedAt).toLocaleDateString()}</span>
          )}
        </div>
      </button>
    </li>
  );
}

// One Online row. It starts with the items rows() gave and pages forward by the
// opaque cursor the source named, appending each page; "More" is shown only while
// a cursor exists, and a failed page leaves the items and the cursor as they were
// so the same button retries it.
function OnlineRowSection({
  sourceId,
  sourceName,
  row,
  onPlay,
}: {
  sourceId: string;
  sourceName: string;
  row: OnlineRow;
  onPlay: (item: OnlineItem) => void;
}) {
  const [extra, setExtra] = useState<OnlineItem[]>([]);
  const [cursor, setCursor] = useState<string | null>(row.nextCursor);
  const [loading, setLoading] = useState(false);
  const [failed, setFailed] = useState(false);
  // A page still in flight when the row goes away (a retry, another source) is
  // abandoned, not appended to a row that is no longer there.
  const abort = useRef(new AbortController());
  useEffect(() => {
    const controller = new AbortController();
    abort.current = controller;
    return () => controller.abort();
  }, []);

  async function more() {
    if (cursor === null || loading) return;
    setLoading(true);
    setFailed(false);
    try {
      const page = await apiClient.getOnlineRowPage(sourceId, row.id, cursor, abort.current.signal);
      if (abort.current.signal.aborted) return;
      // An item already in the row (named by rows() or an earlier page) is not
      // added again.
      setExtra((prev) => {
        const seen = new Set([...row.items, ...prev].map((i) => i.id));
        return [...prev, ...page.items.filter((i) => !seen.has(i.id) && seen.add(i.id))];
      });
      setCursor(page.nextCursor);
    } catch {
      if (!abort.current.signal.aborted) setFailed(true);
    } finally {
      if (!abort.current.signal.aborted) setLoading(false);
    }
  }

  return (
    <section className="home-row" data-testid={`online-row-${row.id}`}>
      <h3 className="section-title">{row.label}</h3>
      <ul className="poster-grid poster-row">
        {[...row.items, ...extra].map((item) => (
          <OnlineItemTile key={item.id} item={item} onPlay={onPlay} />
        ))}
      </ul>
      {failed && (
        <p className="status status-error" role="alert" data-testid={`online-row-more-error-${row.id}`}>
          {sourceName} isn&rsquo;t responding.
        </p>
      )}
      {cursor !== null && (
        <button
          type="button"
          className="nav-link"
          data-testid={`online-row-more-${row.id}`}
          disabled={loading}
          onClick={more}
        >
          {loading ? "Loading\u2026" : failed ? "Retry" : "More"}
        </button>
      )}
    </section>
  );
}
