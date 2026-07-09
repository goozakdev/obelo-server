import { useCallback, useEffect, useState, type ReactNode } from "react";
import { Link, useParams } from "react-router-dom";
import { apiClient, ApiError } from "../api/client";
import type { PlaylistDetail } from "../api/types";
import { errorMessage } from "../screens/errorMessage";
import { useQueue } from "../player/queue/useQueue";
import { buildPlaylistQueue } from "../player/queue/buildQueue";
import AppHeader from "./AppHeader";
import PosterTile from "./PosterTile";

// A Playlist's detail (collections-playlists-ui issue 03 / PRD user stories 24,
// 26): GET /playlists/{id} rendered as the Playlist header plus its member Titles
// in POSITION ORDER, each a poster card with a per-entry remove. The members come
// back in the SAME summary shape a browse grid uses PLUS an `itemId`, so they
// render with PosterTile UNCHANGED — each card links to its Title — and removal is
// by `itemId` so removing one duplicate leaves the other (order preserved).
//
// Owner-private (hide-existence): a Playlist the caller doesn't own — including an
// Admin viewing someone else's — gets a 404 from the server. The screen treats
// that 404 as a readable "not found" state (no crash, no leak) — distinct from a
// transient error — consistent with the rest of browse.
//
// Issue 04 adds reorder controls (the full itemId permutation). The Play
// affordances start the auto-advancing play-through, now via the QUEUE (queue/01):
// a header "Play" that starts at the first member, and a per-member Play that
// starts at THAT member's index. Both build a Queue from the Playlist's ordered
// members (`buildPlaylistQueue` → `queue.playNow(entries, startIndex)`) and then
// navigate to the player on the chosen entry's Title; the player consumes
// `queue.current` and walks the Queue. The old `?playlist=&index=` URL context is
// gone — a duplicate Title's occurrence is disambiguated by the Queue's current
// index, not the URL.

type LoadState =
  | { status: "loading" }
  | { status: "not-found" }
  | { status: "error"; message: string }
  | { status: "ready"; data: PlaylistDetail };

export default function PlaylistDetailScreen() {
  const { id = "" } = useParams();
  const queue = useQueue();
  // A one-shot load that, unlike useAsync, keeps the 404 distinct from other
  // failures so the screen can render a dedicated "not found" state (the
  // owner-private hide-existence affordance) rather than a generic error.
  const [state, setState] = useState<LoadState>({ status: "loading" });
  // Reorder (issue 04). The save is in flight while `reordering` (controls
  // disabled); a refused save records a readable inline message in `reorderError`
  // and the PRIOR order is restored (see onMove).
  const [reordering, setReordering] = useState(false);
  const [reorderError, setReorderError] = useState<string | null>(null);

  const load = useCallback(
    async (signal?: AbortSignal, opts: { silent?: boolean } = {}) => {
      // A post-mutation refetch is `silent` so the screen doesn't flash its
      // loading state — the members just settle to the new server truth.
      if (!opts.silent) setState({ status: "loading" });
      try {
        const data = await apiClient.getPlaylist(id, signal);
        if (signal?.aborted) return;
        setState({ status: "ready", data });
      } catch (err) {
        if (signal?.aborted || isAbort(err)) return;
        if (err instanceof ApiError && err.status === 404) {
          setState({ status: "not-found" });
          return;
        }
        setState({ status: "error", message: errorMessage(err) });
      }
    },
    [id],
  );

  useEffect(() => {
    const ctrl = new AbortController();
    void load(ctrl.signal);
    return () => ctrl.abort();
  }, [load]);

  // Silent refetch after a remove (membership/count/order reflect server truth).
  const reload = useCallback(
    () => void load(undefined, { silent: true }),
    [load],
  );

  // Build a Queue from this Playlist's ordered members and start playing at
  // `startIndex` (the header Play uses 0; a per-member Play uses that member's
  // index). buildPlaylistQueue resolves the members → entries; playback begins in
  // the persistent Now Playing bar (now-playing-bar/01), NO navigation. A failed
  // build leaves the detail in place (the user can retry).
  const play = useCallback(
    async (startIndex: number) => {
      try {
        const entries = await buildPlaylistQueue(apiClient, id);
        if (entries.length === 0) return;
        const at = Math.min(Math.max(startIndex, 0), entries.length - 1);
        queue.playNow(entries, at);
      } catch {
        // Transient/notfound build failure — stay on the detail.
      }
    },
    [id, queue],
  );

  // Move the entry at `fromIndex` to `toIndex` and persist the FULL new order.
  //
  // Reflect-order strategy: OPTIMISTIC local reorder + revert-on-failure. The
  // local member list is reordered immediately (so the detail shows the new order
  // at once), then the whole permutation of `itemId`s is saved via
  // reorderPlaylistItems. On success the optimistic order already IS the server
  // truth, so it stays; on failure the PRIOR order is restored and a readable
  // inline error renders — a failed reorder never disturbs the visible order.
  //
  // The permutation is computed from the LIVE members (state.data at this render),
  // so it always uses the CURRENT item ids — after a remove changed the set, a
  // later reorder sends the right ids and never trips ITEM_SET_MISMATCH.
  async function onMove(fromIndex: number, toIndex: number) {
    if (state.status !== "ready" || reordering) return;
    const prev = state.data.members;
    if (toIndex < 0 || toIndex >= prev.length || fromIndex === toIndex) return;

    const next = [...prev];
    const [moved] = next.splice(fromIndex, 1);
    next.splice(toIndex, 0, moved);

    setReorderError(null);
    setReordering(true);
    // Optimistic: show the new order immediately (count/kind unchanged).
    setState({ status: "ready", data: { ...state.data, members: next } });
    try {
      await apiClient.reorderPlaylistItems(
        state.data.id,
        next.map((m) => m.itemId),
      );
      setReordering(false);
    } catch (err) {
      // Revert to the order captured before this move (the prior closure's data).
      setState({ status: "ready", data: { ...state.data, members: prev } });
      setReorderError(errorMessage(err));
      setReordering(false);
    }
  }

  return (
    <div className="app-shell" data-testid="playlist-detail-screen">
      <AppHeader />
      <main className="app-main app-main-wide">
        <Link className="nav-link back-link" to="/playlists">
          ← Playlists
        </Link>

        {state.status === "loading" && (
          <p className="status status-loading" data-testid="playlist-loading">
            Loading playlist&hellip;
          </p>
        )}

        {state.status === "not-found" && (
          <div className="card" data-testid="playlist-not-found">
            <p className="status status-error" role="alert">
              <span className="dot dot-error" aria-hidden="true" />
              Playlist not found.
            </p>
          </div>
        )}

        {state.status === "error" && (
          <p
            className="status status-error"
            data-testid="playlist-error"
            role="alert"
          >
            <span className="dot dot-error" aria-hidden="true" />
            {state.message}
          </p>
        )}

        {state.status === "ready" && (
          <article className="detail" data-testid="playlist-detail">
            {/* Header toolbar: the title + count, plus the queue "Play" — it
                starts the whole play-through at the FIRST member (index 0). Shown
                only when there's something to play. */}
            <div className="grid-toolbar">
              <h2 className="section-title" data-testid="playlist-title">
                {state.data.name}
              </h2>
              <span
                className="library-roots"
                data-testid="playlist-member-count"
              >
                {state.data.memberCount}{" "}
                {state.data.memberCount === 1 ? "item" : "items"}
              </span>
              {state.data.members.length > 0 && (
                <button
                  className="nav-link"
                  type="button"
                  data-testid="playlist-play"
                  onClick={() => void play(0)}
                >
                  ▶ Play
                </button>
              )}
            </div>

            {reorderError && (
              <p
                className="status status-error"
                data-testid="reorder-error"
                role="alert"
              >
                <span className="dot dot-error" aria-hidden="true" />
                {reorderError}
              </p>
            )}

            {state.data.members.length === 0 ? (
              <div className="card" data-testid="playlist-empty">
                <p className="status status-loading">
                  This playlist has no items yet. Add a Title from its page.
                </p>
              </div>
            ) : (
              // Members render in the server's POSITION order; PosterTile keeps
              // each card identical to a browse card, with the per-entry reorder
              // (move up/down) + remove controls as its overlay action. Keyed by
              // itemId so duplicate Titles stay distinct and move independently.
              <ul className="poster-grid" data-testid="playlist-members">
                {state.data.members.map((m, i) => (
                  <PosterTile
                    key={m.itemId}
                    title={m}
                    action={
                      <EntryControls
                        itemId={m.itemId}
                        index={i}
                        count={state.data.members.length}
                        reordering={reordering}
                        onPlay={() => void play(i)}
                        onMoveUp={() => onMove(i, i - 1)}
                        onMoveDown={() => onMove(i, i + 1)}
                        remove={
                          <RemoveItemControl
                            playlistId={state.data.id}
                            itemId={m.itemId}
                            disabled={reordering}
                            onRemoved={reload}
                          />
                        }
                      />
                    }
                  />
                ))}
              </ul>
            )}
          </article>
        )}
      </main>
    </div>
  );
}

// The per-entry overlay action: move-up / move-down reorder controls plus the
// remove control, grouped so the card keeps browse parity. Reorder is deterministic
// up/down (the simplest persistable gesture): a button is disabled at the end it
// can't move toward, and ALL move buttons disable while a save is in flight
// (`reordering`) so only one permutation is ever outstanding. The move/remove act
// on the `itemId`, so a duplicated Title's two entries move/remove independently.
function EntryControls({
  itemId,
  index,
  count,
  reordering,
  onPlay,
  onMoveUp,
  onMoveDown,
  remove,
}: {
  itemId: string;
  index: number;
  count: number;
  reordering: boolean;
  onPlay: () => void;
  onMoveUp: () => void;
  onMoveDown: () => void;
  remove: ReactNode;
}) {
  return (
    <div className="reorder-controls">
      {/* Per-member Play: start the play-through at THIS member's index (the
          itemId disambiguates a duplicate Title's occurrence — we pass the
          index, not just the title id). */}
      <button
        className="nav-link"
        type="button"
        data-testid="play-item-button"
        data-item-id={itemId}
        aria-label="Play from here"
        onClick={onPlay}
      >
        ▶
      </button>
      <button
        className="nav-link reorder-button"
        type="button"
        data-testid="move-up-button"
        data-item-id={itemId}
        aria-label="Move up"
        onClick={onMoveUp}
        disabled={reordering || index === 0}
      >
        ↑
      </button>
      <button
        className="nav-link reorder-button"
        type="button"
        data-testid="move-down-button"
        data-item-id={itemId}
        aria-label="Move down"
        onClick={onMoveDown}
        disabled={reordering || index === count - 1}
      >
        ↓
      </button>
      {remove}
    </div>
  );
}

// The per-entry remove control, rendered as a PosterTile overlay action so the
// card keeps browse parity. Removal is by `itemId` (the playlist-item id), so
// removing one entry of a duplicated Title leaves the other; on success the parent
// refetches and exactly that entry disappears (the count/order refresh). A refused
// remove surfaces inline and the card stays. Its own component so each entry owns
// its pending/error state independently. Disabled while a reorder save is in
// flight so a remove can't race a permutation (the item set would shift mid-save).
function RemoveItemControl({
  playlistId,
  itemId,
  onRemoved,
  disabled = false,
}: {
  playlistId: string;
  itemId: string;
  onRemoved: () => void;
  disabled?: boolean;
}) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function onRemove() {
    if (busy) return;
    setBusy(true);
    setError(null);
    try {
      await apiClient.removePlaylistItem(playlistId, itemId);
      // On success the parent refetch unmounts this tile; no need to clear busy.
      onRemoved();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  }

  return (
    <>
      <button
        className="nav-link nav-logout remove-item-button"
        type="button"
        data-testid="remove-item-button"
        data-item-id={itemId}
        onClick={onRemove}
        disabled={busy || disabled}
      >
        {busy ? "Removing…" : "Remove"}
      </button>
      {error && (
        <p
          className="status status-error"
          data-testid="remove-item-error"
          role="alert"
        >
          {error}
        </p>
      )}
    </>
  );
}

function isAbort(err: unknown): boolean {
  return err instanceof DOMException && err.name === "AbortError";
}
