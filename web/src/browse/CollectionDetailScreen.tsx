import { useCallback, useEffect, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { apiClient, ApiError } from "../api/client";
import type { CollectionDetail } from "../api/types";
import { useAuth } from "../auth/session";
import { errorMessage } from "../screens/errorMessage";
import AppHeader from "./AppHeader";
import PosterTile from "./PosterTile";

// A Collection's detail (collections-playlists-ui issue 01 / PRD user stories
// 3–4, 6): GET /collections/{id} rendered as the Collection header plus its
// member Titles as a poster grid. The members come back in the SAME summary
// shape a browse grid uses, so they render with PosterTile UNCHANGED — each card
// links to its Title's detail/play page exactly like a Library grid.
//
// Hide-existence: a Member who can see none of a Collection's members gets a 404
// from the server. The screen treats that 404 as a readable "not found" state
// (no crash, no leak) — distinct from a transient error — consistent with the
// rest of browse. Any other failure renders the generic error message.
//
// Curation (issue 02, role-gated by useAuth().isAdmin; server-enforced): an Admin
// gets rename / edit-description / delete controls in the header and a per-member
// remove control on each card. After any successful write the screen refetches
// the detail so the poster/count/membership reflect the server's truth. A Member
// sees none of these controls — the same screen, read-only.

type LoadState =
  | { status: "loading" }
  | { status: "not-found" }
  | { status: "error"; message: string }
  | { status: "ready"; data: CollectionDetail };

export default function CollectionDetailScreen() {
  const { id = "" } = useParams();
  const navigate = useNavigate();
  const { isAdmin } = useAuth();
  // A one-shot load that, unlike useAsync, keeps the 404 distinct from other
  // failures so the screen can render a dedicated "not found" state (the
  // hide-existence affordance) rather than a generic error.
  const [state, setState] = useState<LoadState>({ status: "loading" });

  const load = useCallback(
    async (signal?: AbortSignal, opts: { silent?: boolean } = {}) => {
      // A post-mutation refetch is `silent` so the screen doesn't flash its
      // loading state — the members just settle to the new server truth.
      if (!opts.silent) setState({ status: "loading" });
      try {
        const data = await apiClient.getCollection(id, signal);
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

  // Silent refetch after a curation write (poster/count/membership reflect truth).
  const reload = useCallback(() => void load(undefined, { silent: true }), [load]);

  return (
    <div className="app-shell" data-testid="collection-detail-screen">
      <AppHeader />
      <main className="app-main app-main-wide">
        <Link className="nav-link back-link" to="/collections">
          ← Collections
        </Link>

        {state.status === "loading" && (
          <p className="status status-loading" data-testid="collection-loading">
            Loading collection&hellip;
          </p>
        )}

        {state.status === "not-found" && (
          <div className="card" data-testid="collection-not-found">
            <p className="status status-error" role="alert">
              <span className="dot dot-error" aria-hidden="true" />
              Collection not found.
            </p>
          </div>
        )}

        {state.status === "error" && (
          <p
            className="status status-error"
            data-testid="collection-error"
            role="alert"
          >
            <span className="dot dot-error" aria-hidden="true" />
            {state.message}
          </p>
        )}

        {state.status === "ready" && (
          <article className="detail" data-testid="collection-detail">
            <div className="grid-toolbar">
              <h2 className="section-title" data-testid="collection-title">
                {state.data.name}
              </h2>
              <span className="library-roots" data-testid="collection-member-count">
                {state.data.memberCount}{" "}
                {state.data.memberCount === 1 ? "item" : "items"}
              </span>
            </div>

            {state.data.description && (
              <p
                className="detail-overview"
                data-testid="collection-description"
              >
                {state.data.description}
              </p>
            )}

            {/* Admin curation header: rename / edit-description / delete. A
                Member never renders it (the server enforces the scope too). */}
            {isAdmin && (
              <CollectionCurationHeader
                detail={state.data}
                onUpdated={reload}
                onDeleted={() => navigate("/collections")}
              />
            )}

            {state.data.members.length === 0 ? (
              <div className="card" data-testid="collection-empty">
                <p className="status status-loading">
                  This collection has no items yet.
                </p>
              </div>
            ) : (
              <ul className="poster-grid" data-testid="collection-members">
                {state.data.members.map((t) =>
                  isAdmin ? (
                    <PosterTile
                      key={t.id}
                      title={t}
                      action={
                        <RemoveMemberControl
                          collectionId={state.data.id}
                          titleId={t.id}
                          onRemoved={reload}
                        />
                      }
                    />
                  ) : (
                    <PosterTile key={t.id} title={t} />
                  ),
                )}
              </ul>
            )}
          </article>
        )}
      </main>
    </div>
  );
}

// The Admin curation header (issue 02): a "Rename / edit" toggle reveals an inline
// editor (name + description → PUT), and a two-step delete (DELETE → navigate back
// to the list). Each write shows a pending state and surfaces a refused write
// (e.g. a blank name → 400 BAD_REQUEST) as a readable inline message without
// losing the page; on success it asks the parent to refetch (rename/description)
// or navigate away (delete).
function CollectionCurationHeader({
  detail,
  onUpdated,
  onDeleted,
}: {
  detail: CollectionDetail;
  onUpdated: () => void;
  onDeleted: () => void;
}) {
  const [editing, setEditing] = useState(false);
  const [name, setName] = useState(detail.name);
  const [description, setDescription] = useState(detail.description);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);

  const [confirming, setConfirming] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [deleteError, setDeleteError] = useState<string | null>(null);

  function openEditor() {
    setName(detail.name);
    setDescription(detail.description);
    setSaveError(null);
    setEditing(true);
  }

  async function onSave() {
    if (saving) return;
    const trimmedName = name.trim();
    if (!trimmedName) {
      setSaveError("Enter a collection name.");
      return;
    }
    const trimmedDesc = description.trim();
    setSaving(true);
    setSaveError(null);
    try {
      await apiClient.updateCollection(detail.id, {
        name: trimmedName,
        // Always send the description so clearing it persists (a blank value is
        // a deliberate "no blurb", not "leave unchanged").
        description: trimmedDesc,
      });
      setEditing(false);
      onUpdated();
    } catch (err) {
      // A refused write (e.g. blank name → 400) surfaces; the editor stays put.
      setSaveError(errorMessage(err));
    } finally {
      setSaving(false);
    }
  }

  async function onConfirmDelete() {
    if (deleting) return;
    setDeleting(true);
    setDeleteError(null);
    try {
      await apiClient.deleteCollection(detail.id);
      onDeleted();
    } catch (err) {
      setDeleteError(errorMessage(err));
      setDeleting(false);
      setConfirming(false);
    }
  }

  return (
    <div className="collection-curation" data-testid="collection-curation">
      {!editing ? (
        <div className="collection-curation-actions">
          <button
            className="nav-link"
            type="button"
            data-testid="edit-collection-button"
            onClick={openEditor}
          >
            Rename / edit
          </button>

          {!confirming ? (
            <button
              className="nav-link nav-logout"
              type="button"
              data-testid="delete-collection-button"
              onClick={() => {
                setDeleteError(null);
                setConfirming(true);
              }}
              disabled={deleting}
            >
              Delete collection
            </button>
          ) : (
            <span
              className="collection-delete-confirm"
              data-testid="delete-collection-confirm"
            >
              <span className="confirm-prompt">Delete {detail.name}?</span>
              <button
                className="nav-link nav-logout"
                type="button"
                data-testid="delete-collection-confirm-button"
                onClick={onConfirmDelete}
                disabled={deleting}
              >
                {deleting ? "Deleting…" : "Confirm delete"}
              </button>
              <button
                className="nav-link"
                type="button"
                data-testid="delete-collection-cancel-button"
                onClick={() => setConfirming(false)}
                disabled={deleting}
              >
                Cancel
              </button>
            </span>
          )}
        </div>
      ) : (
        <div className="collection-edit-form" data-testid="collection-edit-form">
          <div className="field">
            <label className="field-label" htmlFor="edit-collection-name">
              Name
            </label>
            <input
              id="edit-collection-name"
              className="field-input"
              data-testid="edit-collection-name-input"
              type="text"
              value={name}
              onChange={(e) => setName(e.target.value)}
              disabled={saving}
            />
          </div>
          <div className="field">
            <label className="field-label" htmlFor="edit-collection-description">
              Description <span className="field-optional">(optional)</span>
            </label>
            <textarea
              id="edit-collection-description"
              className="field-input"
              data-testid="edit-collection-description-input"
              rows={2}
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              disabled={saving}
            />
          </div>
          <div className="form-actions">
            <button
              className="auth-submit"
              type="button"
              data-testid="save-collection-button"
              onClick={onSave}
              disabled={saving}
            >
              {saving ? "Saving…" : "Save"}
            </button>
            <button
              className="nav-link"
              type="button"
              data-testid="cancel-collection-edit-button"
              onClick={() => setEditing(false)}
              disabled={saving}
            >
              Cancel
            </button>
          </div>
          {saveError && (
            <p
              className="status status-error"
              data-testid="collection-edit-error"
              role="alert"
            >
              <span className="dot dot-error" aria-hidden="true" />
              {saveError}
            </p>
          )}
        </div>
      )}

      {deleteError && (
        <p
          className="status status-error"
          data-testid="collection-delete-error"
          role="alert"
        >
          <span className="dot dot-error" aria-hidden="true" />
          {deleteError}
        </p>
      )}
    </div>
  );
}

// The per-member remove control (issue 02), rendered as a PosterTile overlay
// action so the card keeps browse parity. On success the parent refetches and the
// member disappears (and the count/poster refresh); a refused remove surfaces
// inline and the card stays. The control is its own component so each member owns
// its pending/error state independently.
function RemoveMemberControl({
  collectionId,
  titleId,
  onRemoved,
}: {
  collectionId: string;
  titleId: string;
  onRemoved: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function onRemove() {
    if (busy) return;
    setBusy(true);
    setError(null);
    try {
      await apiClient.removeCollectionItem(collectionId, titleId);
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
        className="nav-link nav-logout remove-member-button"
        type="button"
        data-testid="remove-member-button"
        data-title-id={titleId}
        onClick={onRemove}
        disabled={busy}
      >
        {busy ? "Removing…" : "Remove"}
      </button>
      {error && (
        <p
          className="status status-error"
          data-testid="remove-member-error"
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
