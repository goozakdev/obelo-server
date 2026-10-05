import { apiClient } from "../api/client";
import type { EnrichmentCandidate, TitleDetail } from "../api/types";
import { CandidateRow, useCandidateSearch } from "./enrichmentCandidates";

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
  const {
    query,
    setQuery,
    artist,
    setArtist,
    candidates,
    selected,
    setSelected,
    hasMore,
    searching,
    applying,
    error,
    submit,
    showMore,
    apply,
  } = useCandidateSearch({
    initialQuery,
    artistScope,
    search: (q, opts) => apiClient.searchEnrichmentCandidates(titleId, q, opts),
  });

  // Update re-points the record; Replace (when offered) is the identity correction.
  // Either way the page gets the re-enriched Title detail.
  function doApply(mode: "update" | "replace") {
    return apply(
      mode,
      (c) =>
        mode === "replace" && onReplace
          ? onReplace(c)
          : apiClient.applyEnrichmentOverride(
              titleId,
              c.externalId,
              // The namespace the pick was found in (ADR-0060 decision 5).
              c.source,
            ),
      onApplied,
    );
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
                testIdPrefix="enrichment"
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
              onClick={showMore}
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
