import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type FormEvent,
  type KeyboardEvent,
  type ReactNode,
} from "react";
import type { EnrichmentCandidate, EnrichmentCandidatesResult } from "../api/types";
import { errorMessage } from "../screens/errorMessage";
import { looksLikeRef, type Provider } from "./searchRef";

// The fix surface on a Needs-Fixing row: the provider search the old Attention
// screen never had.
//
// Two things make it different from the Edit-item picker it shares a look with.
// First, it SEARCHES ON ITS OWN as soon as the row opens, using a seed derived from
// what the scanner already read (the show title, the album, the cleaned filename) —
// so the common case is confirm-and-move-on rather than type-then-read. Second, the
// top hit is called out as the **best guess** and applied with one button, because
// on this screen the Admin is working a list of twenty, not perfecting one item.
//
// It stays ignorant of WHICH endpoint backs it: the caller passes `search` and
// `preview`, because a row whose fix is an identity correction searches the Library
// (an Unmatched file has no Title to anchor a per-item search to) while a row whose
// fix is a metadata correction searches through its own Title. The two apply to
// different things (ADR-0002/0014) and the row, not this component, decides which.

/** Optional field-scoped narrowing AND-ed into a music search, in the shape the
 * search callback forwards to the API client. A term is present only when its box
 * is both rendered and non-blank, so a video row — and a row whose Admin blanked a
 * box to widen — sends the same URL it always did. */
export interface FixSearchScope {
  artist?: string;
  release?: string;
}

export default function FixItemPicker({
  seed,
  artistScope,
  albumScope,
  provider,
  applyLabel,
  applyHint,
  search,
  preview,
  onApply,
  onCancel,
  chooseEpisode,
}: {
  /** The query to search with on open — what the Admin would otherwise have typed. */
  seed: string;
  /** When defined (a music row), an artist box is rendered pre-filled with this and
   * AND-ed into the search. Undefined on a video row, which has no artist axis.
   *
   * It is a BOX and not a silent narrowing: the value comes from the same tags that
   * produced the row's problem, so it can itself be the thing that is wrong. Blank
   * it and the search widens. This mirrors EnrichmentOverridePicker's `artistScope`
   * exactly, so the queue and the detail page are one search UI to learn. */
  artistScope?: string;
  /** The album counterpart of {@link artistScope}, sent as `release` — a recording
   * search narrowed to the release the track sits on, which is what makes "Intro"
   * or "She" answerable at all. Same rules: editable, blank widens, undefined on a
   * row with no release axis. */
  albumScope?: string;
  /** Which provider a pasted bare id belongs to, so a UUID isn't read as a TMDB id. */
  provider: Provider;
  /** Label for the apply button ("Use this"), which differs by what applying means. */
  applyLabel: string;
  /** One line under the buttons saying what applying will actually do. */
  applyHint: string;
  /** Run a free-text search (page is 0-based), narrowed by whatever scope terms the
   * Admin left filled in. `scope` is `{}` on every row with no narrowing axis. */
  search: (
    query: string,
    page: number,
    scope: FixSearchScope,
  ) => Promise<EnrichmentCandidatesResult>;
  /** Resolve a pasted provider URL/id to a single candidate. */
  preview: (ref: string) => Promise<EnrichmentCandidate>;
  /** Apply the chosen record. Rejecting leaves the picker open with the error. */
  onApply: (candidate: EnrichmentCandidate) => Promise<void>;
  /** Close the picker without applying. */
  onCancel: () => void;
  /** A second step after the Admin picks a SERIES: which episode within it. It is
   * needed wherever the series is only half the answer, because the lookup would
   * otherwise use the numbers parsed from the filename — the whole reason a
   * mis-numbered file is unfixable. Rendered instead of applying the series
   * directly; omitted where picking the record IS the fix.
   *
   * No queue row passes it: the per-episode pin flow it served moved into the file
   * matcher with file-matcher/07. Its caller is now the matcher's record picker
   * (ShowMatcherScreen), which repoints a Slot's record at another series — that
   * flow is search-a-series-then-pick-an-episode exactly as written here. */
  chooseEpisode?: (candidate: EnrichmentCandidate, back: () => void) => ReactNode;
}) {
  const [query, setQuery] = useState(seed);
  const [artist, setArtist] = useState(artistScope ?? "");
  const [album, setAlbum] = useState(albumScope ?? "");
  const [candidates, setCandidates] = useState<EnrichmentCandidate[] | null>(null);
  const [selected, setSelected] = useState<EnrichmentCandidate | null>(null);
  const [page, setPage] = useState(0);
  const [hasMore, setHasMore] = useState(false);
  const [searching, setSearching] = useState(false);
  const [applying, setApplying] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // The series chosen on an Episode row, pending the episode choice.
  const [chosenSeries, setChosenSeries] = useState<EnrichmentCandidate | null>(null);
  // The auto-search runs once per mount. Without the guard a re-render from the
  // search's own state updates would re-fire it, and every open row would hammer
  // the provider (which is rate-limited and shared).
  const autoSearched = useRef(false);

  // The narrowing terms as the wire wants them: a blank box is OMITTED rather than
  // sent empty, so widening a search restores exactly the URL a video row sends.
  const scope = useCallback((): FixSearchScope => {
    const s: FixSearchScope = {};
    const a = artist.trim();
    const r = album.trim();
    if (a !== "") s.artist = a;
    if (r !== "") s.release = r;
    return s;
  }, [artist, album]);

  const runSearch = useCallback(
    async (q: string, nextPage: number, append: boolean) => {
      const term = q.trim();
      if (term === "") return;
      setSearching(true);
      setError(null);
      try {
        const res = await search(term, nextPage, scope());
        setCandidates((prev) => (append && prev ? [...prev, ...res.candidates] : res.candidates));
        setHasMore(res.hasMore ?? false);
        setPage(nextPage);
      } catch (err) {
        setError(errorMessage(err));
      } finally {
        setSearching(false);
      }
    },
    [search, scope],
  );

  const runPreview = useCallback(
    async (ref: string) => {
      setSearching(true);
      setError(null);
      try {
        const candidate = await preview(ref);
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
    },
    [preview],
  );

  // Open with the answer already on screen where we can guess it. A seed that is
  // itself a bare id/URL previews instead — same branch the submit takes.
  useEffect(() => {
    if (autoSearched.current) return;
    autoSearched.current = true;
    const term = seed.trim();
    if (term === "") return;
    if (looksLikeRef(term, provider)) {
      void runPreview(term);
    } else {
      void runSearch(term, 0, false);
    }
  }, [seed, provider, runSearch, runPreview]);

  function submit(e: FormEvent) {
    e.preventDefault();
    if (searching) return;
    const q = query.trim();
    if (q === "") return;
    setSelected(null);
    if (looksLikeRef(q, provider)) {
      void runPreview(q);
    } else {
      void runSearch(q, 0, false);
    }
  }

  async function apply(candidate: EnrichmentCandidate) {
    if (applying) return;
    // On an Episode row the series is only half the answer; advance to the episode
    // chooser rather than applying, so the Admin never presses a button that cannot
    // finish the job — which is exactly what the old one-step flow did.
    if (chooseEpisode) {
      setChosenSeries(candidate);
      return;
    }
    setApplying(true);
    setError(null);
    try {
      await onApply(candidate);
      // On success the row leaves the queue and this component unmounts; on failure
      // the error below is what the Admin sees, with the candidates still up.
    } catch (err) {
      setError(errorMessage(err));
      setApplying(false);
    }
  }

  const best = candidates && candidates.length > 0 ? candidates[0] : null;
  const rest = candidates ? candidates.slice(1) : [];

  // Step two: having chosen a series on an Episode row, the chooser replaces the
  // search entirely — the remaining question is which episode, not which show.
  if (chooseEpisode && chosenSeries) {
    return (
      <div className="fix-picker" data-testid="fix-picker">
        {/* `back` returns to the SERIES search, not out of the row: picking the
            wrong show is the likeliest mistake here, and the provider may model
            the episodes under a different series entirely (a spin-off, a
            re-numbered continuation), so re-searching is part of the normal path
            rather than an abort. */}
        {chooseEpisode(chosenSeries, () => setChosenSeries(null))}
      </div>
    );
  }

  return (
    <div className="fix-picker" data-testid="fix-picker">
      {searching && candidates === null && (
        <p className="status status-loading" data-testid="fix-picker-searching">
          Looking up &ldquo;{seed}&rdquo;&hellip;
        </p>
      )}

      {best && (
        <div className="fix-best-guess" data-testid="fix-best-guess">
          <span className="fix-best-guess-label">Best guess</span>
          <CandidateCard
            c={best}
            selected={selected?.externalId === best.externalId}
            onSelect={() => setSelected(best)}
          />
          <div className="fix-best-guess-actions">
            <button
              className="auth-submit"
              type="button"
              data-testid="fix-use-best-guess"
              disabled={applying}
              onClick={() => void apply(best)}
            >
              {applying ? "Applying…" : chooseEpisode ? "Pick an episode…" : applyLabel}
            </button>
            <p className="fix-apply-hint">
              {chooseEpisode
                ? "Then choose which episode in this series the file actually is."
                : applyHint}
            </p>
          </div>
        </div>
      )}

      {candidates !== null && candidates.length === 0 && (
        <p className="status" data-testid="fix-picker-no-candidates">
          No matches for &ldquo;{query}&rdquo;. Try a different spelling, or paste the
          record&rsquo;s provider URL below.
        </p>
      )}

      <form className="fix-picker-search" onSubmit={submit}>
        <label className="field">
          <span className="field-label">
            {best ? "Not it? Search again" : "Search"}
          </span>
          <input
            className="field-input"
            data-testid="fix-picker-input"
            type="text"
            value={query}
            placeholder="Search by name, or paste a provider URL or id"
            disabled={searching || applying}
            onChange={(e) => setQuery(e.target.value)}
          />
        </label>
        {/* The narrowing boxes, on a music row only. They carry what the scanner
            already read off the file, so the common case needs no typing — and they
            are editable, so a wrong artist tag widens instead of stranding the row
            with no results and no control (the whole reason a silent narrowing was
            rejected). */}
        {artistScope !== undefined && (
          <label className="field">
            <span className="field-label">Artist</span>
            <input
              className="field-input"
              data-testid="fix-picker-artist"
              type="text"
              value={artist}
              placeholder="Artist (optional, narrows results)"
              disabled={searching || applying}
              onChange={(e) => setArtist(e.target.value)}
            />
          </label>
        )}
        {albumScope !== undefined && (
          <label className="field">
            <span className="field-label">Album</span>
            <input
              className="field-input"
              data-testid="fix-picker-album"
              type="text"
              value={album}
              placeholder="Album (optional, narrows results)"
              disabled={searching || applying}
              onChange={(e) => setAlbum(e.target.value)}
            />
          </label>
        )}
        <div className="fix-picker-search-actions">
          <button
            className="nav-link"
            type="submit"
            data-testid="fix-picker-search-button"
            disabled={searching || applying || query.trim() === ""}
          >
            {searching ? "Searching…" : "Search"}
          </button>
          <button
            className="nav-link"
            type="button"
            data-testid="fix-picker-cancel"
            disabled={applying}
            onClick={onCancel}
          >
            Cancel
          </button>
        </div>
      </form>

      {rest.length > 0 && (
        <>
          <ul className="fix-candidate-list" data-testid="fix-candidate-list">
            {rest.map((c) => (
              <li key={c.externalId}>
                <CandidateCard
                  c={c}
                  selected={selected?.externalId === c.externalId}
                  onSelect={() => setSelected(c)}
                />
              </li>
            ))}
          </ul>
          {hasMore && (
            <button
              className="nav-link"
              type="button"
              data-testid="fix-picker-show-more"
              disabled={searching || applying}
              onClick={() => void runSearch(query, page + 1, true)}
            >
              {searching ? "Loading…" : "Show more"}
            </button>
          )}
        </>
      )}

      {/* Applying a NON-top candidate: the top hit has its own button above, so this
          only appears once the Admin has actively chosen a different record. */}
      {selected && best && selected.externalId !== best.externalId && (
        <div className="fix-picker-apply" data-testid="fix-picker-apply">
          <button
            className="auth-submit"
            type="button"
            data-testid="fix-use-selected"
            disabled={applying}
            onClick={() => void apply(selected)}
          >
            {applying
              ? "Applying…"
              : chooseEpisode
                ? `Pick an episode in ${selected.title}…`
                : `${applyLabel}: ${selected.title}`}
          </button>
          <p className="fix-apply-hint">{applyHint}</p>
        </div>
      )}

      {error && (
        <p className="status status-error" data-testid="fix-picker-error" role="alert">
          <span className="dot dot-error" aria-hidden="true" />
          {error}
        </p>
      )}
    </div>
  );
}

// One candidate: thumbnail, title + year, record-type badge, disambiguation hint —
// the same four facts the Edit-item picker shows, because they are what separate
// two same-named works.
function CandidateCard({
  c,
  selected,
  onSelect,
}: {
  c: EnrichmentCandidate;
  selected: boolean;
  onSelect: () => void;
}) {
  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      onSelect();
    }
  };
  return (
    <div
      className={`fix-candidate${selected ? " is-selected" : ""}`}
      data-testid="fix-candidate"
      data-external-id={c.externalId}
      role="button"
      tabIndex={0}
      aria-pressed={selected}
      onClick={onSelect}
      onKeyDown={onKeyDown}
    >
      {c.thumbnailUrl && (
        <img className="fix-candidate-thumb" src={c.thumbnailUrl} alt="" loading="lazy" />
      )}
      <div className="fix-candidate-body">
        <span className="fix-candidate-title" data-testid="fix-candidate-title">
          {c.title}
          {c.year ? ` (${c.year})` : ""}
        </span>
        {c.typeLabel && <span className="fix-candidate-type">{c.typeLabel}</span>}
        {c.disambiguation && (
          <span className="fix-candidate-hint">{c.disambiguation}</span>
        )}
      </div>
    </div>
  );
}
