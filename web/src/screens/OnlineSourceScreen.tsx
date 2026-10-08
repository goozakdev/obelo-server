import { useEffect, useRef, useState } from "react";
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

export default function OnlineSourceScreen() {
  const { sourceId = "" } = useParams<{ sourceId: string }>();
  const queue = useQueue();
  const [attempt, setAttempt] = useState(0);
  const sources = useAsync((signal) => apiClient.getOnlineSources(signal), []);
  const rows = useAsync((signal) => apiClient.getOnlineRows(sourceId, signal), [sourceId, attempt]);

  const sourceName =
    sources.status === "ready"
      ? (sources.data.find((s: OnlineSource) => s.id === sourceId)?.name ?? sourceId)
      : sourceId;

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

        {rows.status === "loading" && (
          <p className="status status-loading" data-testid="online-source-loading">
            Loading {sourceName}&hellip;
          </p>
        )}

        {rows.status === "error" && (
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

        {rows.status === "ready" && rows.data.length === 0 && (
          <p className="status status-loading" data-testid="online-source-empty">
            {sourceName} has nothing to show right now.
          </p>
        )}

        {rows.status === "ready" &&
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
          <li className="poster-tile online-item" key={item.id} data-testid="online-item">
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
