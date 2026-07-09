import { useState, type FormEvent, type KeyboardEvent } from "react";
import { apiClient } from "../api/client";
import type {
  CascadeSummary,
  EnrichmentCandidate,
  EntityEnrichmentDetail,
} from "../api/types";
import { errorMessage } from "../screens/errorMessage";
import { looksLikeRef, type Provider } from "./searchRef";

// Edit-item unified "Search" tab on a browse PARENT — Show / Artist / Album
// (item-editing/02, ADR-0019). The parent analogue of EnrichmentOverridePicker:
// ONE input that accepts a search term, a provider URL, or a bare id (a URL/id
// routes to externalPreview and auto-selects the resolved record; a term searches
// TMDB tv for a Show or MusicBrainz for an Artist/Album). The Admin selects a
// candidate row, optionally ticks "also apply to children", then applies at the
// bottom:
//   • Update (primary) — a parent Enrichment override (Fix info): re-points WHICH
//     record decorates the parent; identity/watch untouched, Locks honored.
//   • Replace (danger) — a parent identity correction (Wrong item): the parent is a
//     genuinely different work. Destructive; rendered ONLY when the caller passes
//     onReplace (a Show; an Artist/Album has no per-item identity anchor). One click.
// The cascade opt-in applies to WHICHEVER button is pressed. An ALBUM candidate can
// be expanded to preview its tracklist before applying.

export default function EntityEnrichmentOverridePicker({
  entityType,
  entityId,
  currentExternalId,
  artistScope,
  onApplied,
  onReplace,
}: {
  entityType: "shows" | "artists" | "albums";
  entityId: string;
  /** The external id currently pinned (from the parent's enrichmentOverride), so the
   * box can show which override is in effect. */
  currentExternalId?: string;
  /** When provided (an Album), the artist-scope input is shown pre-filled so the album
   * search can be narrowed to the item's artist. Omit for a Show/Artist. */
  artistScope?: string;
  /** Called with the re-enriched parent detail so the page can reflect the fix. */
  onApplied: (detail: EntityEnrichmentDetail) => void;
  /** Identity correction for the selected candidate (the destructive "Replace"),
   * carrying the cascade opt-in. When present, the Replace button is shown — the
   * caller passes it for a Show ONLY (Artist/Album have no identity anchor). */
  onReplace?: (
    candidate: EnrichmentCandidate,
    cascade: boolean,
  ) => Promise<EntityEnrichmentDetail>;
}) {
  // A Show resolves against TMDB (tv); an Artist/Album against MusicBrainz — so a
  // pasted bare id is detected against the right provider.
  const provider: Provider = entityType === "shows" ? "tmdb" : "musicbrainz";

  const [query, setQuery] = useState("");
  const [artist, setArtist] = useState(artistScope ?? "");
  const [candidates, setCandidates] = useState<EnrichmentCandidate[] | null>(null);
  const [selected, setSelected] = useState<EnrichmentCandidate | null>(null);
  const [page, setPage] = useState(0);
  const [hasMore, setHasMore] = useState(false);
  const [searching, setSearching] = useState(false);
  const [applying, setApplying] = useState<"update" | "replace" | null>(null);
  const [expandedId, setExpandedId] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  // "Also apply to children" (item-editing/05): a Show/Artist/Album always HAS
  // children, so the option is always offered on this parent picker. It applies to
  // whichever button (Update → cascaded Fix info, Replace → cascaded Wrong item).
  const [cascade, setCascade] = useState(false);
  const [summary, setSummary] = useState<CascadeSummary | null>(null);

  async function runSearch(nextPage: number, append: boolean) {
    const q = query.trim();
    if (q === "") return;
    setSearching(true);
    setError(null);
    try {
      const res = await apiClient.searchEntityEnrichmentCandidates(entityType, entityId, q, {
        artist,
        page: nextPage,
      });
      setCandidates((prev) =>
        append && prev ? [...prev, ...res.candidates] : res.candidates,
      );
      setHasMore(res.hasMore ?? false);
      setPage(nextPage);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setSearching(false);
    }
  }

  // A pasted provider URL/id resolves a single candidate via externalPreview and
  // AUTO-SELECTS it; an error surfaces the server's actionable message.
  async function runPreview(ref: string) {
    setSearching(true);
    setError(null);
    try {
      const candidate = await apiClient.previewEntityExternalCandidate(
        entityType,
        entityId,
        ref,
      );
      setCandidates([candidate]);
      setSelected(candidate);
      setHasMore(false);
    } catch (err) {
      setCandidates(null);
      setSelected(null);
      setError(errorMessage(err));
    } finally {
      setSearching(false);
    }
  }

  function submit(e: FormEvent) {
    e.preventDefault();
    if (searching) return;
    const q = query.trim();
    if (q === "") return;
    setSelected(null);
    if (looksLikeRef(q, provider)) {
      void runPreview(q);
    } else {
      void runSearch(0, false);
    }
  }

  async function doApply(mode: "update" | "replace") {
    if (applying || !selected) return;
    setApplying(mode);
    setError(null);
    try {
      const detail =
        mode === "replace" && onReplace
          ? await onReplace(selected, cascade)
          : await apiClient.applyEntityEnrichmentOverride(
              entityType,
              entityId,
              selected.externalId,
              cascade,
            );
      onApplied(detail);
      setSummary(detail.cascade ?? null);
      setCandidates(null);
      setSelected(null);
      setQuery("");
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setApplying(null);
    }
  }

  const toggle = (id: string) => setExpandedId(expandedId === id ? null : id);

  return (
    <section
      className="enrichment-override-picker card"
      data-testid="entity-enrichment-override-picker"
    >
      <h2 className="section-title">Search</h2>
      <p className="detail-hint">
        Search the metadata provider — or paste a provider URL or id — then pick the
        right record and apply it.
      </p>
      {currentExternalId && (
        <p className="detail-hint" data-testid="entity-enrichment-override-current">
          Current record: <code>{currentExternalId}</code>
        </p>
      )}

      <form className="field" onSubmit={submit}>
        <span className="field-label">Search</span>
        <input
          className="field-input"
          data-testid="entity-enrichment-search-input"
          type="text"
          value={query}
          placeholder="Search, or paste a provider URL or id"
          disabled={searching}
          onChange={(e) => setQuery(e.target.value)}
        />
        {artistScope !== undefined && (
          <input
            className="field-input"
            data-testid="entity-enrichment-artist-input"
            type="text"
            value={artist}
            placeholder="Artist (optional, narrows results)"
            disabled={searching}
            onChange={(e) => setArtist(e.target.value)}
          />
        )}
        <button
          className="nav-link"
          data-testid="entity-enrichment-search-button"
          type="submit"
          disabled={searching || query.trim() === ""}
        >
          {searching ? "Searching…" : "Search"}
        </button>
      </form>

      {/* "Also apply to children": a parent always has children, so offer the cascade. */}
      <label className="field-checkbox" data-testid="entity-enrichment-cascade">
        <input
          type="checkbox"
          checked={cascade}
          disabled={applying !== null}
          onChange={(e) => setCascade(e.target.checked)}
        />
        <span>Also apply to children</span>
      </label>

      {summary && (
        <p className="status" data-testid="entity-enrichment-cascade-summary" role="status">
          Applied to children: {summary.updated} updated, {summary.attention} sent to
          the attention list.
        </p>
      )}

      {candidates && candidates.length === 0 && (
        <p className="status" data-testid="entity-enrichment-no-candidates">
          No matches found.
        </p>
      )}

      {candidates && candidates.length > 0 && (
        <>
          <ul
            className="enrichment-candidate-list"
            data-testid="entity-enrichment-candidate-list"
          >
            {candidates.map((c) => (
              <EntityCandidateRow
                key={c.externalId}
                c={c}
                selected={selected?.externalId === c.externalId}
                expanded={expandedId === c.externalId}
                onSelect={() => setSelected(c)}
                onToggle={() => toggle(c.externalId)}
              />
            ))}
          </ul>
          {hasMore && (
            <button
              className="nav-link"
              data-testid="entity-enrichment-show-more"
              type="button"
              disabled={searching}
              onClick={() => void runSearch(page + 1, true)}
            >
              {searching ? "Loading…" : "Show more"}
            </button>
          )}
        </>
      )}

      {selected && (
        <div className="edit-apply-actions" data-testid="edit-apply-actions">
          <button
            className="auth-submit edit-apply-update-button"
            data-testid="edit-apply-update"
            type="button"
            disabled={applying !== null}
            onClick={() => void doApply("update")}
          >
            {applying === "update" ? "Updating…" : "Update"}
          </button>
          <p className="edit-apply-hint">keeps watch history &amp; your edits</p>

          {onReplace && (
            <>
              <button
                className="nav-link nav-link-danger edit-apply-replace-button"
                data-testid="edit-apply-replace"
                type="button"
                disabled={applying !== null}
                onClick={() => void doApply("replace")}
              >
                {applying === "replace" ? "Replacing…" : "Replace"}
              </button>
              <p className="edit-apply-hint edit-apply-hint-danger">
                different work — resets watch state &amp; your edits
              </p>
            </>
          )}
        </div>
      )}

      {error && (
        <p
          className="status status-error"
          data-testid="entity-enrichment-override-error"
          role="alert"
        >
          <span className="dot dot-error" aria-hidden="true" />
          {error}
        </p>
      )}
    </section>
  );
}

// EntityCandidateRow renders one selectable parent candidate card with the type
// badge and an expandable album tracklist preview. Clicking the row selects it;
// the tracklist toggle is isolated (it doesn't change the selection).
function EntityCandidateRow({
  c,
  selected,
  expanded,
  onSelect,
  onToggle,
}: {
  c: EnrichmentCandidate;
  selected: boolean;
  expanded: boolean;
  onSelect: () => void;
  onToggle: () => void;
}) {
  const onKeyDown = (e: KeyboardEvent<HTMLLIElement>) => {
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      onSelect();
    }
  };
  return (
    <li
      className={`enrichment-candidate card${selected ? " is-selected" : ""}`}
      data-testid="entity-enrichment-candidate"
      data-external-id={c.externalId}
      role="button"
      tabIndex={0}
      aria-pressed={selected}
      onClick={onSelect}
      onKeyDown={onKeyDown}
    >
      {c.thumbnailUrl && (
        <img className="enrichment-candidate-thumb" src={c.thumbnailUrl} alt="" loading="lazy" />
      )}
      <div className="enrichment-candidate-body">
        <span
          className="enrichment-candidate-title"
          data-testid="entity-enrichment-candidate-title"
        >
          {c.title}
          {c.year ? ` (${c.year})` : ""}
        </span>
        {c.typeLabel && (
          <span
            className="enrichment-candidate-type"
            data-testid="entity-enrichment-candidate-type"
          >
            {c.typeLabel}
          </span>
        )}
        {c.disambiguation && (
          <span className="enrichment-candidate-hint">{c.disambiguation}</span>
        )}
        {c.tracklist && c.tracklist.length > 0 && (
          <>
            <button
              className="nav-link enrichment-tracklist-toggle"
              data-testid="entity-enrichment-tracklist-toggle"
              type="button"
              onClick={(e) => {
                // The tracklist toggle must not also select/deselect the row.
                e.stopPropagation();
                onToggle();
              }}
            >
              {expanded ? "Hide tracklist" : `Preview ${c.tracklist.length} tracks`}
            </button>
            {expanded && (
              <ol className="enrichment-tracklist" data-testid="entity-enrichment-tracklist">
                {c.tracklist.map((t) => (
                  <li key={`${t.disc ?? 1}-${t.position}`}>
                    {t.position}. {t.title}
                  </li>
                ))}
              </ol>
            )}
          </>
        )}
      </div>
    </li>
  );
}
