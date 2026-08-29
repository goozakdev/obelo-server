import { useCallback, useEffect, useMemo, useState } from "react";
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
import { useFixCounts } from "./useFixCounts";
import AdminListPanel from "./AdminListPanel";
import FixItemRow from "./FixItemRow";
import {
  buildFixItems,
  EMPTY_BY_PROBLEM,
  FIX_PROBLEMS,
  type FixItem,
  type FixProblem,
} from "./needsFixing";
import type { Provider } from "./searchRef";

// The Admin "Needs Fixing" tab — what used to be "Attention".
//
// The old screen showed four panels (needs-review, Unmatched files, Match
// overrides, Metadata match), each with its own vocabulary and its own primitive
// form that asked the Admin to type a raw TMDB/IMDB/MusicBrainz id. To fix one item
// you had to know which panel its problem lived in, then leave the app to look an id
// up. One of the four panels wasn't work at all — Match overrides is a log of
// corrections already made.
//
// This is one queue instead. Every problem is a uniform row (FixItemRow) that names
// the item unambiguously, shows its file, states the problem in a sentence, and
// offers the provider search the Edit-item dialog has always had. Chips filter by
// problem with live counts; the library selector carries each library's open count
// so it is obvious where the work is; and the corrections log moves below the queue,
// out of the way — except for ORPHANED corrections, which are genuinely broken and
// so are promoted into the queue as rows.
//
// Applying an identity correction only takes effect on the next scan (a Match
// override is folder-keyed and read by the scanner — ADR-0002/0014). Rather than
// firing a library scan per row, which is unusable on a queue of twenty, the screen
// counts the identity corrections made and offers ONE rescan when the Admin is done.

export default function AdminNeedsFixingScreen() {
  const libs = useAsync<Library[]>((signal) => apiClient.listLibraries(signal), []);
  const [selected, setSelected] = useState<string>("");

  // Default the selection to the first library once the list loads.
  useEffect(() => {
    if (libs.status === "ready" && selected === "" && libs.data.length > 0) {
      setSelected(libs.data[0].id);
    }
  }, [libs, selected]);

  const library = useMemo(
    () => (libs.status === "ready" ? libs.data.find((l) => l.id === selected) : undefined),
    [libs, selected],
  );

  // Per-Library open counts, so the selector answers "where is the work?" before the
  // Admin has to click through each Library to find out. Best-effort — an
  // uncountable Library just shows no number.
  const libraryIds = useMemo(
    () => (libs.status === "ready" ? libs.data.map((l) => l.id) : []),
    [libs],
  );
  const counts = useFixCounts(libraryIds);

  return (
    <section className="admin-needs-fixing" data-testid="admin-needs-fixing">
      {libs.status === "loading" && (
        <p className="status status-loading" data-testid="needs-fixing-libraries-loading">
          Loading libraries&hellip;
        </p>
      )}
      {libs.status === "error" && (
        <p className="status status-error" data-testid="needs-fixing-libraries-error" role="alert">
          <span className="dot dot-error" aria-hidden="true" />
          {libs.message}
        </p>
      )}
      {libs.status === "ready" && libs.data.length === 0 && (
        <div className="card" data-testid="needs-fixing-no-libraries">
          <p className="status status-loading">
            No libraries yet. Create one in the Libraries tab first.
          </p>
        </div>
      )}

      {libs.status === "ready" && libs.data.length > 0 && (
        <>
          <label className="field needs-fixing-library-picker">
            <span className="field-label">Library</span>
            <select
              className="field-input"
              data-testid="needs-fixing-library-select"
              value={selected}
              onChange={(e) => setSelected(e.target.value)}
            >
              {libs.data.map((lib) => (
                <option key={lib.id} value={lib.id}>
                  {lib.name}
                  {counts[lib.id] === undefined
                    ? ""
                    : counts[lib.id] === 0
                      ? " — all clear"
                      : ` — ${counts[lib.id]} to fix`}
                </option>
              ))}
            </select>
          </label>

          {selected && (
            <LibraryQueue
              key={selected}
              libraryId={selected}
              libraryKind={library?.kind ?? "movie"}
            />
          )}
        </>
      )}
    </section>
  );
}

// The queue for one Library: the four server lists, folded into one row model, with
// the corrections log kept separate underneath.
function LibraryQueue({ libraryId, libraryKind }: { libraryId: string; libraryKind: string }) {
  const needsReview = useNeedsReview(libraryId);

  const [unmatched, setUnmatched] = useState<UnmatchedFile[]>([]);
  const [unmatchedState, setUnmatchedState] = useState<"loading" | "error" | "ready">("loading");
  const [unmatchedError, setUnmatchedError] = useState<string | null>(null);

  const [overrides, setOverrides] = useState<MatchOverride[]>([]);
  const [overridesState, setOverridesState] = useState<"loading" | "error" | "ready">("loading");
  const [overridesError, setOverridesError] = useState<string | null>(null);

  const [enrichment, setEnrichment] = useState<EnrichmentAttentionTitle[]>([]);
  const [enrichmentState, setEnrichmentState] = useState<"loading" | "error" | "ready">("loading");
  const [enrichmentError, setEnrichmentError] = useState<string | null>(null);

  // "All" until the Admin narrows to one class of problem.
  const [filter, setFilter] = useState<FixProblem | "all">("all");
  // Identity corrections applied since the last scan — a Match override is read by
  // the scanner, so until one runs the corrected items are still filed the old way.
  const [pendingRescan, setPendingRescan] = useState(0);
  const [scanning, setScanning] = useState(false);
  const [scanError, setScanError] = useState<string | null>(null);
  const [correctionsOpen, setCorrectionsOpen] = useState(false);

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

  const reloadAll = useCallback(() => {
    void loadUnmatched();
    void loadOverrides();
    void loadEnrichment();
    needsReview.reload();
  }, [loadUnmatched, loadOverrides, loadEnrichment, needsReview]);

  const loading =
    needsReview.loading ||
    unmatchedState === "loading" ||
    overridesState === "loading" ||
    enrichmentState === "loading";

  // A failure in any one list is reported, but never blanks the others: three of the
  // four problems staying fixable beats an all-or-nothing screen.
  const errors = [
    needsReview.error,
    unmatchedError,
    overridesError,
    enrichmentError,
  ].filter((e): e is string => e !== null);

  const items: FixItem[] = useMemo(
    () =>
      buildFixItems({
        unmatched,
        needsReview: needsReview.items as NeedsReviewItem[],
        enrichment,
        overrides,
      }),
    [unmatched, needsReview.items, enrichment, overrides],
  );

  const counts = useMemo(() => {
    const out: Record<string, number> = {};
    for (const it of items) out[it.problem] = (out[it.problem] ?? 0) + 1;
    return out;
  }, [items]);

  const shown = filter === "all" ? items : items.filter((it) => it.problem === filter);

  // A bare id pasted into a picker is provider-specific, and a Library's kind is
  // what decides which provider its records come from.
  const provider: Provider = libraryKind === "music" ? "musicbrainz" : "tmdb";

  const settled = overrides.filter((o) => !o.orphaned);

  async function rescan() {
    setScanning(true);
    setScanError(null);
    try {
      await apiClient.scanLibrary(libraryId);
      setPendingRescan(0);
      reloadAll();
    } catch (err) {
      setScanError(errorMessage(err));
    } finally {
      setScanning(false);
    }
  }

  return (
    <div className="needs-fixing">
      <AdminListPanel
        title="Needs fixing"
        count={loading ? undefined : String(items.length)}
        countTestId="needs-fixing-count"
      >
        {errors.map((e) => (
          <p className="status status-error" key={e} data-testid="needs-fixing-error" role="alert">
            <span className="dot dot-error" aria-hidden="true" />
            {e}
          </p>
        ))}

        {/* Identity corrections are read by the scanner, so they land on the next
            scan. Say so plainly and offer the scan once, rather than running one per
            row (unusable on a long queue) or leaving the Admin to wonder. */}
        {pendingRescan > 0 && (
          <div className="needs-fixing-rescan" data-testid="needs-fixing-rescan">
            <p className="status">
              {pendingRescan === 1
                ? "1 correction recorded."
                : `${pendingRescan} corrections recorded.`}{" "}
              Rescan the library to re-file the corrected items.
            </p>
            <button
              className="auth-submit"
              type="button"
              data-testid="needs-fixing-rescan-button"
              disabled={scanning}
              onClick={() => void rescan()}
            >
              {scanning ? "Scanning…" : "Rescan library"}
            </button>
          </div>
        )}
        {scanError && (
          <p className="status status-error" data-testid="needs-fixing-scan-error" role="alert">
            <span className="dot dot-error" aria-hidden="true" />
            {scanError}
          </p>
        )}

        {loading && (
          <p className="status status-loading" data-testid="needs-fixing-loading">
            Checking this library&hellip;
          </p>
        )}

        {!loading && items.length > 0 && (
          <div className="needs-fixing-filters" role="group" aria-label="Filter by problem">
            <FilterChip
              label="All"
              count={items.length}
              active={filter === "all"}
              onClick={() => setFilter("all")}
              testId="needs-fixing-chip-all"
            />
            {FIX_PROBLEMS.map((p) => (
              <FilterChip
                key={p.problem}
                label={p.label}
                count={counts[p.problem] ?? 0}
                active={filter === p.problem}
                onClick={() => setFilter(p.problem)}
                testId={`needs-fixing-chip-${p.problem}`}
              />
            ))}
          </div>
        )}

        {!loading && items.length === 0 && (
          <p className="status status-empty" data-testid="needs-fixing-empty">
            Nothing needs fixing in this library.
          </p>
        )}

        {!loading && items.length > 0 && shown.length === 0 && filter !== "all" && (
          <p className="status status-empty" data-testid="needs-fixing-filter-empty">
            {EMPTY_BY_PROBLEM[filter]}
          </p>
        )}

        {shown.length > 0 && (
          <ul className="needs-fixing-list" data-testid="needs-fixing-list">
            {shown.map((item) => (
              <FixItemRow
                key={item.key}
                item={item}
                libraryId={libraryId}
                provider={provider}
                onResolved={reloadAll}
                onIdentityCorrected={() => setPendingRescan((n) => n + 1)}
              />
            ))}
          </ul>
        )}
      </AdminListPanel>

      {/* The corrections log. Not work, so not in the queue — but the Admin still
          needs to see what they have overridden, and an override is the only thing
          that can explain why a folder resolves to something its name doesn't say. */}
      <section className="needs-fixing-corrections" data-testid="needs-fixing-corrections">
        <button
          className="nav-link needs-fixing-corrections-toggle"
          type="button"
          data-testid="needs-fixing-corrections-toggle"
          aria-expanded={correctionsOpen}
          onClick={() => setCorrectionsOpen((v) => !v)}
        >
          {correctionsOpen ? "▾" : "▸"} Corrections you&rsquo;ve made ({settled.length})
        </button>
        {correctionsOpen && (
          <>
            {settled.length === 0 && (
              <p className="status status-empty" data-testid="needs-fixing-corrections-empty">
                You haven&rsquo;t corrected anything in this library yet.
              </p>
            )}
            {settled.length > 0 && (
              <ul className="overrides-list" data-testid="overrides-list">
                {settled.map((o) => (
                  <li key={o.id} className="override-item admin-panel-row" data-testid="override-item">
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
                    {o.createdAt && <span className="override-created">{formatDate(o.createdAt)}</span>}
                  </li>
                ))}
              </ul>
            )}
          </>
        )}
      </section>
    </div>
  );
}

// One filter chip. A chip with a zero count stays rendered but dimmed, so the set of
// things that can go wrong is a stable, learnable list rather than one that appears
// and disappears under the Admin.
function FilterChip({
  label,
  count,
  active,
  onClick,
  testId,
}: {
  label: string;
  count: number;
  active: boolean;
  onClick: () => void;
  testId: string;
}) {
  return (
    <button
      className={`needs-fixing-chip${active ? " is-active" : ""}${count === 0 ? " is-empty" : ""}`}
      type="button"
      data-testid={testId}
      aria-pressed={active}
      onClick={onClick}
    >
      {label} <span className="needs-fixing-chip-count">{count}</span>
    </button>
  );
}
