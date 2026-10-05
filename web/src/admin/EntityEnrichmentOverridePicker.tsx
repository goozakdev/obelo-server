import { useState } from "react";
import { apiClient } from "../api/client";
import type {
  CascadeSummary,
  EnrichmentCandidate,
  EntityEnrichmentDetail,
} from "../api/types";
import AlbumEditionPicker from "./AlbumEditionPicker";
import { CandidateRow, useCandidateSearch } from "./enrichmentCandidates";

// Edit-item unified "Search" tab on a browse PARENT — Show / Artist / Album
// (item-editing/02, ADR-0019). The parent analogue of EnrichmentOverridePicker:
// ONE input that accepts a search term, a provider URL, or a bare id. The server's
// search asks the Library's lead to read it as a reference first and auto-selects a
// resolved one (`resolvedRef`); a term is searched by that same lead. The Admin selects a
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
  initialQuery,
  onApplied,
  onReplace,
}: {
  entityType: "shows" | "artists" | "albums";
  entityId: string;
  /** The external id currently pinned (from the parent's enrichmentOverride), so the
   * box can show which override is in effect. */
  currentExternalId?: string;
  /** The parent's current title/name, used to pre-fill the search box — the Admin is
   * usually only correcting part of an already-close title, so seeding it saves
   * retyping. */
  initialQuery?: string;
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
    search: (q, opts) =>
      apiClient.searchEntityEnrichmentCandidates(entityType, entityId, q, opts),
  });
  const [expandedId, setExpandedId] = useState<string | null>(null);
  // "Also apply to children" (item-editing/05): a Show/Artist/Album always HAS
  // children, so the option is always offered on this parent picker. It applies to
  // whichever button (Update → cascaded Fix info, Replace → cascaded Wrong item).
  const [cascade, setCascade] = useState(false);
  const [summary, setSummary] = useState<CascadeSummary | null>(null);

  function doApply(mode: "update" | "replace") {
    return apply(
      mode,
      (c) =>
        mode === "replace" && onReplace
          ? onReplace(c, cascade)
          : apiClient.applyEntityEnrichmentOverride(
              entityType,
              entityId,
              c.externalId,
              // The namespace the pick was found in (ADR-0060 decision 5).
              c.source,
              cascade,
              // The EDITION, when the Admin pasted a /release/ URL (ADR-0052). A
              // search hit carries none, and applying one CLEARS any edition the
              // album had — which is right: they just named a less specific thing.
              c.releaseId,
            ),
      (detail) => {
        onApplied(detail);
        setSummary(detail.cascade ?? null);
      },
    );
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

      {/* The EDITION section (ADR-0052, issue 12), under the matched album. An album
          is a release-group and a release-group holds editions; this is where one is
          chosen, which is the correction that previously required a trip to
          musicbrainz.org and a pasted URL. It renders nothing for an unmatched album
          (there is nothing to choose) and a quiet hint when the provider cannot list
          — the paste box above is still the escape hatch it always was. */}
      {entityType === "albums" && (
        <AlbumEditionPicker albumId={entityId} onApplied={onApplied} />
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
              <CandidateRow
                key={c.externalId}
                testIdPrefix="entity-enrichment"
                c={c}
                selected={selected?.externalId === c.externalId}
                onSelect={() => setSelected(c)}
              >
                {c.tracklist && c.tracklist.length > 0 && (
                  <Tracklist
                    tracklist={c.tracklist}
                    expanded={expandedId === c.externalId}
                    onToggle={() => toggle(c.externalId)}
                  />
                )}
              </CandidateRow>
            ))}
          </ul>
          {hasMore && (
            <button
              className="nav-link"
              data-testid="entity-enrichment-show-more"
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

// Tracklist is an album candidate's expandable tracklist preview. The toggle is
// isolated: it doesn't change the row's selection (click or keyboard).
function Tracklist({
  tracklist,
  expanded,
  onToggle,
}: {
  tracklist: NonNullable<EnrichmentCandidate["tracklist"]>;
  expanded: boolean;
  onToggle: () => void;
}) {
  return (
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
        {expanded ? "Hide tracklist" : `Preview ${tracklist.length} tracks`}
      </button>
      {expanded && (
        <ol className="enrichment-tracklist" data-testid="entity-enrichment-tracklist">
          {tracklist.map((t) => (
            <li key={`${t.disc ?? 1}-${t.position}`}>
              {t.position}. {t.title}
            </li>
          ))}
        </ol>
      )}
    </>
  );
}
