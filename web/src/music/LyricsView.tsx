import { useEffect, useRef, useState } from "react";
import { apiClient } from "../api/client";
import type { LyricLine } from "../api/types";
import { useAsync } from "../browse/useAsync";
import { useQueue } from "../player/queue/useQueue";
import { usePlaybackTransport } from "../player/transport";

// The lyrics view for one Track: whatever lyrics it has, shown in the shape they
// came in. Synced lyrics are a list of lines, and while THIS Track is the one
// playing the line the playback position is in is highlighted and kept in view;
// Plain lyrics are static text. A Track with none shows a quiet empty state —
// having no lyrics is normal, never an error. The view fetches when it mounts,
// so the caller mounts it only when someone opens it.

export interface LyricsViewProps {
  titleId: string;
}

// How often the view reads the playback position while following a song. A
// line lasts seconds, so a quarter second is well inside what anyone notices.
const FOLLOW_INTERVAL_MS = 250;

export default function LyricsView({ titleId }: LyricsViewProps) {
  const state = useAsync((signal) => apiClient.getLyrics(titleId, signal), [titleId]);

  return (
    <section className="lyrics-view" data-testid="lyrics-view" aria-label="Lyrics">
      {state.status === "loading" && (
        <p className="status status-loading" data-testid="lyrics-loading">
          Loading lyrics&hellip;
        </p>
      )}
      {state.status === "error" && (
        <p className="status status-error" data-testid="lyrics-error" role="alert">
          <span className="dot dot-error" aria-hidden="true" />
          {state.message}
        </p>
      )}
      {state.status === "ready" && state.data === null && (
        <p className="lyrics-empty" data-testid="lyrics-empty">
          No lyrics for this track yet.
        </p>
      )}
      {state.status === "ready" && state.data?.kind === "plain" && (
        <p className="lyrics-plain" data-testid="lyrics-plain">
          {state.data.text}
        </p>
      )}
      {state.status === "ready" && state.data?.kind === "synced" && (
        <SyncedLyrics titleId={titleId} lines={state.data.lines} />
      )}
    </section>
  );
}

function SyncedLyrics({ titleId, lines }: { titleId: string; lines: LyricLine[] }) {
  const queue = useQueue();
  const transport = usePlaybackTransport();
  const playingThis = queue.current?.title.id === titleId;
  const active = useActiveLine(lines, playingThis ? transport.positionMs : null);
  const activeRef = useRef<HTMLLIElement>(null);

  // Keep the current line in view as the song moves on.
  useEffect(() => {
    activeRef.current?.scrollIntoView?.({ block: "nearest", behavior: "smooth" });
  }, [active]);

  return (
    <ol className="lyrics-synced" data-testid="lyrics-synced">
      {lines.map((line, i) => (
        <li
          key={i}
          ref={i === active ? activeRef : undefined}
          className={i === active ? "lyric-line lyric-line-active" : "lyric-line"}
          aria-current={i === active ? "true" : undefined}
          data-testid="lyric-line"
        >
          {line.text}
        </li>
      ))}
    </ol>
  );
}

// useActiveLine is the index of the line the playback position is in — the last
// one that has started — or -1 before the first line and whenever there is no
// position to follow (readPosition null: this Track is not the one playing).
function useActiveLine(lines: LyricLine[], readPosition: (() => number) | null): number {
  const [active, setActive] = useState(-1);
  useEffect(() => {
    if (!readPosition) {
      setActive(-1);
      return;
    }
    const update = () => setActive(lineAt(lines, readPosition()));
    update();
    const timer = window.setInterval(update, FOLLOW_INTERVAL_MS);
    return () => window.clearInterval(timer);
  }, [lines, readPosition]);
  return active;
}

function lineAt(lines: LyricLine[], positionMs: number): number {
  let at = -1;
  for (let i = 0; i < lines.length && lines[i].startMs <= positionMs; i++) {
    at = i;
  }
  return at;
}
