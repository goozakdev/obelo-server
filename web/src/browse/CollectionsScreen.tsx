import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";
import { useAuth } from "../auth/session";
import type { CollectionSummary } from "../api/types";
import AppHeader from "./AppHeader";
import Poster from "./Poster";
import CreateCollectionForm from "./CreateCollectionForm";

// The Collections browse list (collections-playlists-ui issue 01 / PRD user
// stories 1–2, 7): GET /collections rendered as a grid of cards, each showing a
// representative poster, the Collection name, and a per-viewer member count, and
// linking to the Collection's detail. The server already access-filters the list
// per viewer (a Member's restricted Collections — and any with zero visible
// members — aren't returned), so the screen renders exactly what it gets; an
// empty list is a clean empty state.
//
// Issue 02 adds the Admin "New collection" action (role-gated by
// useAuth().isAdmin; the server enforces the scope regardless): a Member sees the
// same screen read-only. Because a create mutates the very list shown, the screen
// loads via a reloadable loader (not the load-once useAsync) so it can refetch
// after a create and reflect the server's truth (the new card appears).

type ListState =
  | { status: "loading" }
  | { status: "error"; message: string }
  | { status: "ready"; collections: CollectionSummary[] };

export default function CollectionsScreen() {
  const { isAdmin } = useAuth();
  const [state, setState] = useState<ListState>({ status: "loading" });

  const load = useCallback(async (signal?: AbortSignal) => {
    setState({ status: "loading" });
    try {
      const collections = await apiClient.listCollections(signal);
      if (signal?.aborted) return;
      setState({ status: "ready", collections });
    } catch (err) {
      if (signal?.aborted) return;
      setState({ status: "error", message: errorMessage(err) });
    }
  }, []);

  useEffect(() => {
    const ctrl = new AbortController();
    void load(ctrl.signal);
    return () => ctrl.abort();
  }, [load]);

  const reload = useCallback(() => void load(), [load]);

  return (
    <div className="app-shell" data-testid="collections-screen">
      <AppHeader />
      <main className="app-main app-main-wide">
        <h2 className="section-title">Collections</h2>

        {/* Admin-only curation: the "New collection" action. A Member never
            renders it (and the server enforces the scope regardless). */}
        {isAdmin && <CreateCollectionForm onCreated={reload} />}

        {state.status === "loading" && (
          <p className="status status-loading" data-testid="collections-loading">
            Loading collections&hellip;
          </p>
        )}

        {state.status === "error" && (
          <p
            className="status status-error"
            data-testid="collections-error"
            role="alert"
          >
            <span className="dot dot-error" aria-hidden="true" />
            {state.message}
          </p>
        )}

        {state.status === "ready" && state.collections.length === 0 && (
          <div className="card" data-testid="collections-empty">
            <p className="status status-loading">
              No collections yet. An Admin can curate one from a Title's page.
            </p>
          </div>
        )}

        {state.status === "ready" && state.collections.length > 0 && (
          <ul className="poster-grid" data-testid="collections">
            {state.collections.map((c) => (
              <li
                className="poster-tile"
                data-testid="collection-tile"
                data-collection-id={c.id}
                key={c.id}
              >
                <Link
                  className="poster-link"
                  to={`/collections/${c.id}`}
                  data-testid="collection-item"
                >
                  <div className="poster-frame">
                    <Poster
                      titleId={c.id}
                      title={c.name}
                      src={c.posterUrl}
                    />
                  </div>
                  <div className="poster-caption">
                    <span
                      className="poster-title"
                      data-testid="collection-name"
                      title={c.name}
                    >
                      {c.name}
                    </span>
                    <span
                      className="poster-year"
                      data-testid="collection-count"
                    >
                      {c.memberCount}{" "}
                      {c.memberCount === 1 ? "item" : "items"}
                    </span>
                  </div>
                </Link>
              </li>
            ))}
          </ul>
        )}
      </main>
    </div>
  );
}
