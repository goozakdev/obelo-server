import { useState } from "react";
import { useParams } from "react-router-dom";
import { apiClient } from "../api/client";
import type { OnlineItem, OnlineSource } from "../api/types";
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
            <section className="home-row" key={row.id} data-testid={`online-row-${row.id}`}>
              <h3 className="section-title">{row.label}</h3>
              <ul className="poster-grid poster-row">
                {row.items.map((item) => (
                  <li className="poster-tile online-item" key={item.id} data-testid="online-item">
                    <button
                      type="button"
                      className="poster-link online-item-play"
                      data-testid={`online-item-play-${item.id}`}
                      onClick={() => play(item)}
                    >
                      <div className="poster-frame online-thumb">
                        <img src={item.thumbnailUrl} alt="" className="poster-img" loading="lazy" />
                      </div>
                      <div className="poster-caption">
                        <span className="poster-title" data-testid="online-item-title">
                          {item.title}
                        </span>
                        <span className="poster-year">{formatTimecode(item.durationMs)}</span>
                        {item.description && (
                          <span className="online-item-description">{item.description}</span>
                        )}
                        {item.publishedAt && (
                          <span className="online-item-published">
                            {new Date(item.publishedAt).toLocaleDateString()}
                          </span>
                        )}
                      </div>
                    </button>
                  </li>
                ))}
              </ul>
            </section>
          ))}
      </main>
    </div>
  );
}
