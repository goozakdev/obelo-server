import { useRef, useState, type FormEvent, type KeyboardEvent, type ReactNode } from "react";
import type { EnrichmentCandidate, EnrichmentCandidatesResult } from "../api/types";
import { errorMessage } from "../screens/errorMessage";

// The search / paging / apply state machine and the candidate card shared by the
// two Enrichment override pickers (EnrichmentOverridePicker for a leaf Title,
// EntityEnrichmentOverridePicker for a browse parent). They differ only in which
// endpoints they call, so each passes its search call in and its apply call to
// `apply`; everything else — including "Show more" paging against the query that
// PRODUCED the list, not whatever is now in the box — lives here once.

export type ApplyMode = "update" | "replace";

type SearchFn = (
  q: string,
  opts: { artist: string; page: number },
) => Promise<EnrichmentCandidatesResult>;

export function useCandidateSearch({
  initialQuery,
  artistScope,
  search,
}: {
  initialQuery?: string;
  artistScope?: string;
  search: SearchFn;
}) {
  const [query, setQuery] = useState(initialQuery ?? "");
  const [artist, setArtist] = useState(artistScope ?? "");
  const [candidates, setCandidates] = useState<EnrichmentCandidate[] | null>(null);
  const [selected, setSelected] = useState<EnrichmentCandidate | null>(null);
  const [page, setPage] = useState(0);
  const [hasMore, setHasMore] = useState(false);
  const [searching, setSearching] = useState(false);
  const [applying, setApplying] = useState<ApplyMode | null>(null);
  const [error, setError] = useState<string | null>(null);
  // The query/artist that produced the current list. "Show more" pages THIS, so an
  // edit to the (still editable) inputs after a search can't splice a different
  // query's page onto the list.
  const lastSearch = useRef({ q: "", artist: "" });

  // One page. append=false replaces (a fresh search), append=true adds the next.
  async function runSearch(nextPage: number, append: boolean) {
    const { q, artist: a } = lastSearch.current;
    if (q === "") return;
    setSearching(true);
    setError(null);
    try {
      const res = await search(q, { artist: a, page: nextPage });
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
    lastSearch.current = { q, artist };
    setSelected(null);
    void runSearch(0, false);
  }

  function showMore() {
    void runSearch(page + 1, true);
  }

  // Runs one apply call. `onDone` gets the result before the working state clears,
  // so the caller can hand it to the page first.
  async function apply<T>(
    mode: ApplyMode,
    run: (c: EnrichmentCandidate) => Promise<T>,
    onDone: (r: T) => void,
  ) {
    if (applying || !selected) return;
    setApplying(mode);
    setError(null);
    try {
      onDone(await run(selected));
      setCandidates(null);
      setSelected(null);
      setQuery("");
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setApplying(null);
    }
  }

  return {
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
  };
}

// CandidateRow renders one selectable candidate card (thumbnail, title/year, type
// badge, hint). Clicking the row selects it (highlighted); applying happens at the
// bottom via Update/Replace. `testIdPrefix` keeps each picker's test ids; `children`
// is the picker's extra body content (the album tracklist preview).
export function CandidateRow({
  c,
  selected,
  onSelect,
  testIdPrefix,
  children,
}: {
  c: EnrichmentCandidate;
  selected: boolean;
  onSelect: () => void;
  testIdPrefix: string;
  children?: ReactNode;
}) {
  const onKeyDown = (e: KeyboardEvent<HTMLLIElement>) => {
    // A nested control (the tracklist toggle) handles its own keys.
    if (e.target !== e.currentTarget) return;
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      onSelect();
    }
  };
  return (
    <li
      className={`enrichment-candidate card${selected ? " is-selected" : ""}`}
      data-testid={`${testIdPrefix}-candidate`}
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
          data-testid={`${testIdPrefix}-candidate-title`}
        >
          {c.title}
          {c.year ? ` (${c.year})` : ""}
        </span>
        {c.typeLabel && (
          <span
            className="enrichment-candidate-type"
            data-testid={`${testIdPrefix}-candidate-type`}
          >
            {c.typeLabel}
          </span>
        )}
        {c.disambiguation && (
          <span className="enrichment-candidate-hint">{c.disambiguation}</span>
        )}
        {children}
      </div>
    </li>
  );
}
