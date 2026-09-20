import { useState, type FormEvent, type KeyboardEvent } from "react";
import { apiClient } from "../api/client";
import type { EnrichmentCandidate, TitleDetail } from "../api/types";
import { errorMessage } from "../screens/errorMessage";

// Edit-item unified "Search" tab for a leaf Title (Movie/Episode/Track — ADR-0019).
// The Admin types ONE input that accepts a search term, a provider URL, or a bare
// id. The picker never decides which: the search endpoint asks the Library's lead
// to read the input as a reference first, and a resolved one comes back flagged
// `resolvedRef` and is auto-selected; anything else is a free-text search by that
// lead (.scratch/bundled-plugins issue 12). The Admin selects a candidate
// row, then applies with a button at the bottom:
//   • Update (primary) — an Enrichment override (Fix info): re-points WHICH record
//     decorates the item; identity_key and watch state are never touched, Locked
//     fields are honored. Available on every kind.
//   • Replace (danger) — an identity correction (Wrong item): the file is a
//     genuinely different work. Destructive — resets watch state + clears Locks.
//     Rendered ONLY when the caller passes onReplace (Movie only for a leaf); a
//     single click applies it (the red styling + hint are the guardrail).
//
// This merges the former separate "Fix info" and "Wrong item" tabs into one flow,
// reusing the same search/paste/candidate code (item-editing/search-improvements:
// artist scope, "show more" paging, type badges, and the paste-an-id escape hatch,
// now folded into the single input).

export default function EnrichmentOverridePicker({
  titleId,
  currentExternalId,
  artistScope,
  initialQuery,
  onApplied,
  onReplace,
}: {
  titleId: string;
  /** The external id currently pinned on the item (tmdbId / musicbrainzId), so the
   * box can show which override is in effect. */
  currentExternalId?: string;
  /** The item's current title, used to pre-fill the search box — the Admin is usually
   * only correcting part of an already-close title, so seeding it saves retyping. */
  initialQuery?: string;
  /** When provided (music leaf), the artist-scope input is shown pre-filled with this
   * value so an album/track search can be narrowed to the item's artist. Omit for a
   * video leaf, where narrowing by artist has no meaning. */
  artistScope?: string;
  /** Called with the re-enriched Title detail so the page reflects the fix (both an
   * Update and a Replace return a full, fresh TitleDetail). */
  onApplied: (detail: TitleDetail) => void;
  /** Identity correction for the selected candidate (the destructive "Replace").
   * When present, the Replace button is shown — the caller passes it for a Movie
   * ONLY (Episode/Track have no identity-correction endpoint). */
  onReplace?: (candidate: EnrichmentCandidate) => Promise<TitleDetail>;
}) {
  const [query, setQuery] = useState(initialQuery ?? "");
  const [artist, setArtist] = useState(artistScope ?? "");
  const [candidates, setCandidates] = useState<EnrichmentCandidate[] | null>(null);
  const [selected, setSelected] = useState<EnrichmentCandidate | null>(null);
  const [page, setPage] = useState(0);
  const [hasMore, setHasMore] = useState(false);
  const [searching, setSearching] = useState(false);
  const [applying, setApplying] = useState<"update" | "replace" | null>(null);
  const [error, setError] = useState<string | null>(null);

  // runSearch fetches one page. append=false replaces (a fresh search), append=true
  // adds the next page for "show more".
  async function runSearch(nextPage: number, append: boolean) {
    const q = query.trim();
    if (q === "") return;
    setSearching(true);
    setError(null);
    try {
      const res = await apiClient.searchEnrichmentCandidates(titleId, q, {
        artist,
        page: nextPage,
      });
      setCandidates((prev) =>
        append && prev ? [...prev, ...res.candidates] : res.candidates,
      );
      // A pasted URL/id the lead resolved auto-selects its one record.
      if (res.resolvedRef) setSelected(res.candidates[0] ?? null);
      setHasMore(res.hasMore ?? false);
      setPage(nextPage);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setSearching(false);
    }
  }

  // The single input: the server reads a URL/id and searches for a term.
  function submit(e: FormEvent) {
    e.preventDefault();
    if (searching) return;
    const q = query.trim();
    if (q === "") return;
    setSelected(null);
    void runSearch(0, false);
  }

  async function doApply(mode: "update" | "replace") {
    if (applying || !selected) return;
    setApplying(mode);
    setError(null);
    try {
      const detail =
        mode === "replace" && onReplace
          ? await onReplace(selected)
          : await apiClient.applyEnrichmentOverride(
              titleId,
              selected.externalId,
              // The namespace the pick was found in (ADR-0060 decision 5).
              selected.source,
            );
      onApplied(detail);
      // Reflect the newly-applied change and clear the working state.
      setCandidates(null);
      setSelected(null);
      setQuery("");
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setApplying(null);
    }
  }

  return (
    <section className="enrichment-override-picker card" data-testid="enrichment-override-picker">
      <h2 className="section-title">Search</h2>
      <p className="detail-hint">
        Search the metadata provider — or paste a provider URL or id — then pick the
        right record and apply it.
      </p>
      {currentExternalId && (
        <p className="detail-hint" data-testid="enrichment-override-current">
          Current record: <code>{currentExternalId}</code>
        </p>
      )}

      <form className="field" onSubmit={submit}>
        <span className="field-label">Search</span>
        <input
          className="field-input"
          data-testid="enrichment-search-input"
          type="text"
          value={query}
          placeholder="Search, or paste a provider URL or id"
          disabled={searching}
          onChange={(e) => setQuery(e.target.value)}
        />
        {artistScope !== undefined && (
          <input
            className="field-input"
            data-testid="enrichment-artist-input"
            type="text"
            value={artist}
            placeholder="Artist (optional, narrows results)"
            disabled={searching}
            onChange={(e) => setArtist(e.target.value)}
          />
        )}
        <button
          className="nav-link"
          data-testid="enrichment-search-button"
          type="submit"
          disabled={searching || query.trim() === ""}
        >
          {searching ? "Searching…" : "Search"}
        </button>
      </form>

      {candidates && candidates.length === 0 && (
        <p className="status" data-testid="enrichment-no-candidates">
          No matches found.
        </p>
      )}

      {candidates && candidates.length > 0 && (
        <>
          <ul className="enrichment-candidate-list" data-testid="enrichment-candidate-list">
            {candidates.map((c) => (
              <CandidateRow
                key={c.externalId}
                c={c}
                selected={selected?.externalId === c.externalId}
                onSelect={() => setSelected(c)}
              />
            ))}
          </ul>
          {hasMore && (
            <button
              className="nav-link"
              data-testid="enrichment-show-more"
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
                className="auth-submit auth-submit-danger edit-apply-replace-button"
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
        <p className="status status-error" data-testid="enrichment-override-error" role="alert">
          <span className="dot dot-error" aria-hidden="true" />
          {error}
        </p>
      )}
    </section>
  );
}

// CandidateRow renders one selectable candidate card (thumbnail, title/year, type
// badge, hint). Clicking the row selects it (highlighted); applying happens at the
// bottom via Update/Replace.
function CandidateRow({
  c,
  selected,
  onSelect,
}: {
  c: EnrichmentCandidate;
  selected: boolean;
  onSelect: () => void;
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
      data-testid="enrichment-candidate"
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
        <span className="enrichment-candidate-title" data-testid="enrichment-candidate-title">
          {c.title}
          {c.year ? ` (${c.year})` : ""}
        </span>
        {c.typeLabel && (
          <span className="enrichment-candidate-type" data-testid="enrichment-candidate-type">
            {c.typeLabel}
          </span>
        )}
        {c.disambiguation && (
          <span className="enrichment-candidate-hint">{c.disambiguation}</span>
        )}
      </div>
    </li>
  );
}
