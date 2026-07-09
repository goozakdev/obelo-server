import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";
import { formatDate } from "../time";
import type {
  EnrichmentAttentionTitle,
  Library,
  MatchOverride,
  NeedsReviewItem,
  UnmatchedFile,
} from "../api/types";
import { useAsync } from "../browse/useAsync";
import { useNeedsReview } from "./useNeedsReview";
import FixMatchForm, { folderOf } from "./FixMatchForm";
import EnrichmentMatchForm from "./EnrichmentMatchForm";

// The admin attention surfaces (issue 07), behind RequireAdmin (App.tsx) and
// still server-enforced. Three lists, all scoped to one Library the Admin picks
// from a selector (these endpoints are per-library):
//   - needs-review Titles: collected client-side by paging the library's titles
//     and filtering to `needsReview` (see useNeedsReview), each linking to its
//     detail so the Admin can confirm/fix it,
//   - Unmatched files: recognized media with no extractable identity, each with
//     a fix-match action that re-points the file's FOLDER to a correct identity,
//   - Match overrides: the persisted fix-matches, with folder-rename orphans
//     clearly highlighted.
//
// Picking a library is just a one-shot fetch of the libraries list (reusing the
// browse API). The per-library surfaces remount on the selected id.

export default function AdminAttentionScreen() {
  const libs = useAsync<Library[]>((signal) => apiClient.listLibraries(signal), []);
  const [selected, setSelected] = useState<string>("");

  // Default the selection to the first library once the list loads.
  useEffect(() => {
    if (libs.status === "ready" && selected === "" && libs.data.length > 0) {
      setSelected(libs.data[0].id);
    }
  }, [libs, selected]);

  return (
    <section className="admin-attention" data-testid="admin-attention">
      <h2 className="section-title">Attention</h2>

      {libs.status === "loading" && (
        <p className="status status-loading" data-testid="attention-libraries-loading">
          Loading libraries&hellip;
        </p>
      )}
      {libs.status === "error" && (
        <p className="status status-error" data-testid="attention-libraries-error" role="alert">
          <span className="dot dot-error" aria-hidden="true" />
          {libs.message}
        </p>
      )}
      {libs.status === "ready" && libs.data.length === 0 && (
        <div className="card" data-testid="attention-no-libraries">
          <p className="status status-loading">
            No libraries yet. Create one in the Libraries tab first.
          </p>
        </div>
      )}

      {libs.status === "ready" && libs.data.length > 0 && (
        <>
          <label className="field attention-library-picker">
            <span className="field-label">Library</span>
            <select
              className="field-input"
              data-testid="attention-library-select"
              value={selected}
              onChange={(e) => setSelected(e.target.value)}
            >
              {libs.data.map((lib) => (
                <option key={lib.id} value={lib.id}>
                  {lib.name}
                </option>
              ))}
            </select>
          </label>

          {selected && <LibraryAttention key={selected} libraryId={selected} />}
        </>
      )}
    </section>
  );
}

// The three per-library lists, plus the fix-match flow shared between Unmatched
// files and needs-review Titles. A single `activeFolder` drives which inline
// fix-match form is open; applying it refreshes all three lists (a fix-match can
// clear a needs-review Title / Unmatched file AND create an override).
function LibraryAttention({ libraryId }: { libraryId: string }) {
  const needsReview = useNeedsReview(libraryId);

  const [unmatched, setUnmatched] = useState<UnmatchedFile[]>([]);
  const [unmatchedState, setUnmatchedState] = useState<"loading" | "error" | "ready">("loading");
  const [unmatchedError, setUnmatchedError] = useState<string | null>(null);

  const [overrides, setOverrides] = useState<MatchOverride[]>([]);
  const [overridesState, setOverridesState] = useState<"loading" | "error" | "ready">("loading");
  const [overridesError, setOverridesError] = useState<string | null>(null);

  // Enrichment attention (issue 05): Titles whose Enrichment is unmatched/failed,
  // awaiting a hand-match. A NEW dimension, separate from the identity Unmatched
  // files and needs-review above.
  const [enrichment, setEnrichment] = useState<EnrichmentAttentionTitle[]>([]);
  const [enrichmentState, setEnrichmentState] = useState<"loading" | "error" | "ready">("loading");
  const [enrichmentError, setEnrichmentError] = useState<string | null>(null);

  const [activeFolder, setActiveFolder] = useState<string | null>(null);
  // Which enrichment-attention Title currently has its match form open.
  const [activeMatchTitle, setActiveMatchTitle] = useState<string | null>(null);
  // Which needs-review Movie currently has its fix-identity form open, keyed by
  // the item's unique id (NOT its folder — bare movies at a library root share a
  // folder, so keying on folder would open every one of them at once).
  const [activeReviewItem, setActiveReviewItem] = useState<string | null>(null);
  // needs-review items with a mark-reviewed POST in flight (disables the button).
  const [reviewing, setReviewing] = useState<Set<string>>(new Set());
  // The last mark-reviewed failure, surfaced inline.
  const [reviewError, setReviewError] = useState<string | null>(null);
  // True while a fix-identity is being applied (records the override, then
  // rescans so the corrected identity/year takes effect without a manual scan).
  const [applyingFix, setApplyingFix] = useState(false);

  const loadUnmatched = useCallback(
    async (signal?: AbortSignal) => {
      setUnmatchedState("loading");
      setUnmatchedError(null);
      try {
        const files = await apiClient.listUnmatched(libraryId, signal);
        if (signal?.aborted) return;
        setUnmatched(files);
        setUnmatchedState("ready");
      } catch (err) {
        if (signal?.aborted) return;
        setUnmatchedError(errorMessage(err));
        setUnmatchedState("error");
      }
    },
    [libraryId],
  );

  const loadOverrides = useCallback(
    async (signal?: AbortSignal) => {
      setOverridesState("loading");
      setOverridesError(null);
      try {
        const list = await apiClient.listOverrides(libraryId, signal);
        if (signal?.aborted) return;
        setOverrides(list);
        setOverridesState("ready");
      } catch (err) {
        if (signal?.aborted) return;
        setOverridesError(errorMessage(err));
        setOverridesState("error");
      }
    },
    [libraryId],
  );

  const loadEnrichment = useCallback(
    async (signal?: AbortSignal) => {
      setEnrichmentState("loading");
      setEnrichmentError(null);
      try {
        const titles = await apiClient.listEnrichmentAttention(libraryId, signal);
        if (signal?.aborted) return;
        setEnrichment(titles);
        setEnrichmentState("ready");
      } catch (err) {
        if (signal?.aborted) return;
        setEnrichmentError(errorMessage(err));
        setEnrichmentState("error");
      }
    },
    [libraryId],
  );

  useEffect(() => {
    const ctrl = new AbortController();
    void loadUnmatched(ctrl.signal);
    void loadOverrides(ctrl.signal);
    void loadEnrichment(ctrl.signal);
    return () => ctrl.abort();
  }, [loadUnmatched, loadOverrides, loadEnrichment]);

  // After a successful enrichment-match the corrected Title leaves the list, so
  // refetch it (and close the open form).
  const onMatchApplied = useCallback(() => {
    setActiveMatchTitle(null);
    void loadEnrichment();
  }, [loadEnrichment]);

  const onApplied = useCallback(() => {
    setActiveFolder(null);
    void loadUnmatched();
    void loadOverrides();
    needsReview.reload();
  }, [loadUnmatched, loadOverrides, needsReview]);

  // After a needs-review Movie's fix-identity is applied, close its form, record
  // the new override, and resolve the item: the Admin corrected it, so mark it
  // reviewed (clearing the flag now and across rescans) and drop it from the list.
  // The corrected metadata/artwork were already fetched by the form (when an
  // external id was supplied); the identity override re-files on the next scan.
  const resolveReviewFix = useCallback(
    async (item: NeedsReviewItem) => {
      setActiveReviewItem(null);
      setReviewError(null);
      setApplyingFix(true);
      try {
        // Dismiss the old row (a Show via the show endpoint, a Movie/Track via the
        // title endpoint), then apply the identity correction now instead of waiting
        // for a manual scan: an incremental rescan re-resolves the folder through
        // the new override (overrides apply on every scan; unchanged files aren't
        // re-probed, so it's fast) and the auto-after-scan Enrichment pass refreshes
        // the re-filed item's metadata + artwork.
        if (item.kind === "show") {
          await apiClient.reviewShow(item.id);
        } else {
          await apiClient.reviewTitle(item.id);
        }
        await apiClient.scanLibrary(libraryId);
      } catch (err) {
        setReviewError(errorMessage(err));
      } finally {
        setApplyingFix(false);
      }
      void loadOverrides();
      needsReview.reload();
    },
    [libraryId, loadOverrides, needsReview],
  );

  // Dismiss a needs-review item: confirm the uncertain parse is fine. Shows POST
  // to /shows/{id}/review, everything else to /titles/{id}/review.
  const markReviewed = useCallback(
    async (item: NeedsReviewItem) => {
      setReviewError(null);
      setReviewing((cur) => new Set(cur).add(item.id));
      try {
        if (item.kind === "show") {
          await apiClient.reviewShow(item.id);
        } else {
          await apiClient.reviewTitle(item.id);
        }
        needsReview.reload();
      } catch (err) {
        setReviewError(errorMessage(err));
      } finally {
        setReviewing((cur) => {
          const next = new Set(cur);
          next.delete(item.id);
          return next;
        });
      }
    },
    [needsReview],
  );

  return (
    <div className="attention-lists">
      {/* --- needs-review ------------------------------------------------ */}
      <h3 className="subsection-title">Needs review</h3>
      {needsReview.loading && (
        <p className="status status-loading" data-testid="needs-review-loading">
          Loading needs review&hellip;
        </p>
      )}
      {needsReview.error && (
        <p className="status status-error" data-testid="needs-review-error" role="alert">
          <span className="dot dot-error" aria-hidden="true" />
          {needsReview.error}
        </p>
      )}
      {reviewError && (
        <p className="status status-error" data-testid="needs-review-action-error" role="alert">
          <span className="dot dot-error" aria-hidden="true" />
          {reviewError}
        </p>
      )}
      {applyingFix && (
        <p className="status status-loading" data-testid="needs-review-applying">
          Applying correction (rescanning library)&hellip;
        </p>
      )}
      {!needsReview.loading && !needsReview.error && needsReview.items.length === 0 && (
        <p className="status status-empty" data-testid="needs-review-empty">
          Nothing needs review.
        </p>
      )}
      {!needsReview.loading && needsReview.items.length > 0 && (
        <ul className="needs-review-list" data-testid="needs-review-list">
          {needsReview.items.map((t) => {
            const detailPath = t.kind === "show" ? `/shows/${t.id}` : `/titles/${t.id}`;
            const canFix = t.folderPath !== "";
            const fixOpen = canFix && activeReviewItem === t.id;
            return (
              <li
                key={t.id}
                className="needs-review-item card"
                data-testid="needs-review-item"
                data-title-id={t.id}
                data-kind={t.kind}
              >
                <Link className="needs-review-link" to={detailPath}>
                  {t.title}
                  {t.year > 0 ? ` (${t.year})` : ""}
                </Link>
                <span className="needs-review-flag">needs review</span>
                <button
                  className="nav-link"
                  type="button"
                  data-testid="needs-review-mark-button"
                  disabled={reviewing.has(t.id)}
                  onClick={() => void markReviewed(t)}
                >
                  {reviewing.has(t.id) ? "Marking…" : "Mark reviewed"}
                </button>
                {canFix && (
                  <button
                    className="nav-link"
                    type="button"
                    data-testid="needs-review-fix-button"
                    onClick={() =>
                      setActiveReviewItem((cur) => (cur === t.id ? null : t.id))
                    }
                  >
                    {fixOpen ? "Close" : "Fix identity"}
                  </button>
                )}
                {fixOpen && (
                  <FixMatchForm
                    libraryId={libraryId}
                    folderPath={t.folderPath}
                    // Only a Movie has a title-scoped enrichment match to refresh
                    // immediately; a Show/Track relies on the post-fix rescan +
                    // auto-enrich instead, so its form skips that step.
                    titleId={t.kind === "movie" ? t.id : undefined}
                    onApplied={() => void resolveReviewFix(t)}
                    onCancel={() => setActiveReviewItem(null)}
                  />
                )}
              </li>
            );
          })}
        </ul>
      )}

      {/* --- Unmatched --------------------------------------------------- */}
      <h3 className="subsection-title">Unmatched files</h3>
      {unmatchedState === "loading" && (
        <p className="status status-loading" data-testid="unmatched-loading">
          Loading unmatched&hellip;
        </p>
      )}
      {unmatchedState === "error" && (
        <p className="status status-error" data-testid="unmatched-error" role="alert">
          <span className="dot dot-error" aria-hidden="true" />
          {unmatchedError}
        </p>
      )}
      {unmatchedState === "ready" && unmatched.length === 0 && (
        <p className="status status-empty" data-testid="unmatched-empty">
          No unmatched files.
        </p>
      )}
      {unmatchedState === "ready" && unmatched.length > 0 && (
        <ul className="unmatched-list" data-testid="unmatched-list">
          {unmatched.map((f) => {
            const folder = folderOf(f.path);
            return (
              <li
                key={f.id}
                className="unmatched-item card"
                data-testid="unmatched-item"
                data-folder-path={folder}
              >
                <code className="unmatched-path" data-testid="unmatched-path">
                  {f.path}
                </code>
                {f.reason && (
                  <span className="unmatched-reason" data-testid="unmatched-reason">
                    {f.reason}
                  </span>
                )}
                <button
                  className="nav-link"
                  type="button"
                  data-testid="unmatched-fix-button"
                  onClick={() =>
                    setActiveFolder((cur) => (cur === folder ? null : folder))
                  }
                >
                  {activeFolder === folder ? "Close" : "Fix match"}
                </button>
                {activeFolder === folder && (
                  <FixMatchForm
                    libraryId={libraryId}
                    folderPath={folder}
                    onApplied={onApplied}
                    onCancel={() => setActiveFolder(null)}
                  />
                )}
              </li>
            );
          })}
        </ul>
      )}

      {/* --- Overrides --------------------------------------------------- */}
      <h3 className="subsection-title">Match overrides</h3>
      {overridesState === "loading" && (
        <p className="status status-loading" data-testid="overrides-loading">
          Loading overrides&hellip;
        </p>
      )}
      {overridesState === "error" && (
        <p className="status status-error" data-testid="overrides-error" role="alert">
          <span className="dot dot-error" aria-hidden="true" />
          {overridesError}
        </p>
      )}
      {overridesState === "ready" && overrides.length === 0 && (
        <p className="status status-empty" data-testid="overrides-empty">
          No overrides yet.
        </p>
      )}
      {overridesState === "ready" && overrides.length > 0 && (
        <ul className="overrides-list" data-testid="overrides-list">
          {overrides.map((o) => (
            <li
              key={o.id}
              className={`override-item card${o.orphaned ? " override-item-orphaned" : ""}`}
              data-testid="override-item"
              data-orphaned={o.orphaned ? "true" : "false"}
            >
              <span className="override-title" data-testid="override-title">
                {o.title}
                {o.year > 0 ? ` (${o.year})` : ""}
              </span>
              <code className="override-folder" data-testid="override-folder">
                {o.folderPath}
              </code>
              {(o.tmdbId || o.imdbId) && (
                <span className="override-ids">
                  {o.tmdbId ? `tmdb:${o.tmdbId}` : ""}
                  {o.tmdbId && o.imdbId ? " " : ""}
                  {o.imdbId ? `imdb:${o.imdbId}` : ""}
                </span>
              )}
              {o.createdAt && (
                <span className="override-created">{formatDate(o.createdAt)}</span>
              )}
              {o.orphaned && (
                <span className="override-orphaned-badge" data-testid="override-orphaned">
                  orphaned
                </span>
              )}
            </li>
          ))}
        </ul>
      )}

      {/* --- Enrichment attention (issue 05) ----------------------------- */}
      <h3 className="subsection-title">Metadata match</h3>
      {enrichmentState === "loading" && (
        <p className="status status-loading" data-testid="enrichment-attention-loading">
          Loading metadata matches&hellip;
        </p>
      )}
      {enrichmentState === "error" && (
        <p className="status status-error" data-testid="enrichment-attention-error" role="alert">
          <span className="dot dot-error" aria-hidden="true" />
          {enrichmentError}
        </p>
      )}
      {enrichmentState === "ready" && enrichment.length === 0 && (
        <p className="status status-empty" data-testid="enrichment-attention-empty">
          Nothing to match.
        </p>
      )}
      {enrichmentState === "ready" && enrichment.length > 0 && (
        <ul className="enrichment-attention-list" data-testid="enrichment-attention-list">
          {enrichment.map((t) => (
            <li
              key={t.id}
              className="enrichment-attention-item card"
              data-testid="enrichment-attention-item"
              data-title-id={t.id}
            >
              <Link className="enrichment-attention-link" to={`/titles/${t.id}`}>
                {t.title}
                {t.year > 0 ? ` (${t.year})` : ""}
              </Link>
              <span
                className="enrichment-attention-status"
                data-testid="enrichment-attention-status"
              >
                {t.enrichmentStatus}
              </span>
              <button
                className="nav-link"
                type="button"
                data-testid="enrichment-match-button"
                onClick={() =>
                  setActiveMatchTitle((cur) => (cur === t.id ? null : t.id))
                }
              >
                {activeMatchTitle === t.id ? "Close" : "Fix metadata match"}
              </button>
              {activeMatchTitle === t.id && (
                <EnrichmentMatchForm
                  titleId={t.id}
                  onApplied={onMatchApplied}
                  onCancel={() => setActiveMatchTitle(null)}
                />
              )}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
