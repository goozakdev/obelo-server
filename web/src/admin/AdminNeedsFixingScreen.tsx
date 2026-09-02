import { useCallback, useEffect, useMemo, useState } from "react";
import { apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";
import { formatDate } from "../time";
import type {
  EnrichmentAttentionTitle,
  EnrichPassProgress,
  EnrichPassSummary,
  Library,
  MatchOverride,
  NeedsReviewItem,
  ShowProblems,
  UnmatchedFile,
} from "../api/types";
import { useAsync } from "../browse/useAsync";
import { appEvents, type EnrichProgress } from "../events/enrichEvents";
import { useNeedsReview } from "./useNeedsReview";
import { useFixCounts } from "./useFixCounts";
import AdminListPanel from "./AdminListPanel";
import FixItemRow from "./FixItemRow";
import {
  buildFixItems,
  EMPTY_BY_PROBLEM,
  FIX_PROBLEMS,
  hasProblem,
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
// A Show's episode-level problems are ONE row, not one per file (ADR-0044,
// file-matcher/07), and its action opens the file matcher. Chips, the row count and
// the library selector's badge all count those collapsed rows, so the queue counts
// problems rather than symptoms.
//
// Applying an identity correction only takes effect on the next scan (a Match
// override is folder-keyed and read by the scanner — ADR-0002/0014). Rather than
// firing a library scan per row, which is unusable on a queue of twenty, the screen
// counts the identity corrections made and offers ONE rescan when the Admin is done.
//
// It also carries the ONLY enrichment-pass trigger in the app (ADR-0051). A row on
// this queue can be stale rather than broken: the server got better at resolving
// its kind of item and nothing ever re-asked. Rescanning does not help — a scan's
// automatic pass is only-new, and every row here has already settled — so until
// this button existed the remedy was reachable only by hand-issuing an HTTP
// request, while the one action the screen did offer provably changed nothing.

/** The sentence the Re-check button leaves behind, built from the pass summary.
 *
 * `matched` is stated even when it is ZERO, and that is the whole point: the count
 * that cleared is the only thing that separates "the improvement does not apply to
 * my library" from "the improvement never ran" (ADR-0051), and it was the second
 * of those that produced this feature. The other counters appear only when they
 * are non-zero, so the common sentence stays short.
 *
 * The words are unchanged from the first version of this button; only where the
 * numbers come from moved. They used to be the POST's response body — which meant
 * the request was held open for the whole fifteen-minute pass. They now arrive on
 * the terminal `enrichProgress` event (ADR-0051's amendment). */
function recheckSummaryText(s: EnrichPassSummary): string {
  if (s.total === 0) {
    return "Nothing to re-check — nothing in this library is waiting on a metadata match.";
  }
  const parts = [`${s.matched} now matched`];
  if (s.unmatched > 0) parts.push(`${s.unmatched} still unmatched`);
  if (s.failed > 0) parts.push(`${s.failed} failed`);
  if (s.retrying > 0) parts.push(`${s.retrying} will be retried`);
  if (s.disabled > 0) parts.push(`${s.disabled} skipped (enrichment is switched off)`);
  return `Re-checked ${s.total} ${s.total === 1 ? "item" : "items"}: ${parts.join(", ")}.`;
}

/** The line under the button while a pass runs. It reports done-of-total once the
 * pass has counted its work, and says nothing but "working" before that.
 *
 * The blank first phase is real, not a gap in the reporting: a Music recheck
 * re-asks every unmatched ARTIST and ALBUM before a single track settles
 * (`collectMusicLeaves`), so the first minutes of a large pass legitimately have
 * no leaf progress to show. ADR-0051's amendment leaves that unfixed and says so;
 * making it visible is what stops it being mistaken for nothing happening — which
 * is the mistake that started all of this. */
function recheckProgressText(p: EnrichPassProgress | undefined): string {
  if (!p || p.total === 0) return "Re-checking… working out what to ask about.";
  return `Re-checking… ${p.done} of ${p.total}.`;
}

export default function AdminNeedsFixingScreen() {
  const libs = useAsync<Library[]>((signal) => apiClient.listLibraries(signal), []);
  const [selected, setSelected] = useState<string>("");

  // The recheck pass and its report.
  //
  // The pass is STARTED, not awaited (ADR-0051's amendment). So `rechecking` is no
  // longer "an await is outstanding" — it is "the server says a pass is in flight
  // for this library", and it has three sources: the press itself, the status route
  // on mount (which is what lets a RELOADED page rejoin a pass instead of showing
  // an idle button — the operator reloaded and killed theirs), and the terminal SSE
  // event that ends it. `reloadToken` is bumped at that terminal event, which is the
  // moment the lists below are worth fetching again.
  const [rechecking, setRechecking] = useState(false);
  const [recheckProgress, setRecheckProgress] = useState<EnrichPassProgress | undefined>(undefined);
  const [recheckResult, setRecheckResult] = useState<string | null>(null);
  const [recheckError, setRecheckError] = useState<string | null>(null);
  const [reloadToken, setReloadToken] = useState(0);

  async function recheck(libraryId: string) {
    setRechecking(true);
    setRecheckProgress(undefined);
    setRecheckResult(null);
    setRecheckError(null);
    try {
      const state = await apiClient.enrichLibrary(libraryId, { mode: "recheck" });
      // 202 means queued, not finished: the button stays busy until the pass's
      // terminal event arrives. `started: false` means one was already running,
      // which is the same in-flight state — the press simply joined it.
      setRecheckProgress(state.progress);
    } catch (err) {
      // A press that started nothing must SAY so — a server running no background
      // passes, or a full queue, used to be answered with silence, and silence is
      // indistinguishable from a pass that ran and found nothing. That confusion is
      // what this whole feature exists to end.
      setRecheckError(errorMessage(err));
      setRechecking(false);
    }
  }

  // Rejoin whatever is already happening. On mount and on every library change we
  // ask the server whether a pass is in flight, so a page loaded in the middle of
  // one shows it rather than an idle button. Best-effort: a failed status read
  // leaves the button idle rather than blocking the screen behind it.
  useEffect(() => {
    if (selected === "") return;
    const ctrl = new AbortController();
    // Idle is the assumption, applied immediately so switching library never shows
    // the previous one's pass; the read below only ever ADDS the in-flight state.
    setRecheckResult(null);
    setRecheckError(null);
    setRechecking(false);
    setRecheckProgress(undefined);
    void (async () => {
      try {
        const state = await apiClient.getEnrichPassState(selected, ctrl.signal);
        if (ctrl.signal.aborted || !state.running) return;
        setRechecking(true);
        setRecheckProgress(state.progress);
      } catch {
        // Best-effort: a status read that fails leaves the button idle rather than
        // blocking the screen behind a question nobody asked.
      }
    })();
    return () => ctrl.abort();
  }, [selected]);

  // Live progress, and the end of the pass. The server already published all of
  // this (ADR-0016); before this the screen simply was not listening, because the
  // POST was doing the waiting instead.
  useEffect(() => {
    if (selected === "") return;
    return appEvents.subscribe((type, data) => {
      if (type !== "enrichProgress" || !data) return;
      const p = data as EnrichProgress;
      if (p.libraryId !== selected) return;
      if (!p.complete) {
        setRechecking(true);
        setRecheckProgress({
          total: p.total ?? 0,
          done: p.done ?? 0,
          matched: p.matched ?? 0,
          unmatched: p.unmatched ?? 0,
          failed: p.failed ?? 0,
          disabled: p.disabled ?? 0,
          retrying: p.retrying ?? 0,
        });
        return;
      }
      // Terminal: report what the pass did, and go and re-read the lists it moved.
      setRechecking(false);
      setRecheckProgress(undefined);
      setRecheckResult(
        recheckSummaryText({
          libraryId: p.libraryId,
          total: p.total ?? 0,
          matched: p.matched ?? 0,
          unmatched: p.unmatched ?? 0,
          failed: p.failed ?? 0,
          disabled: p.disabled ?? 0,
          retrying: p.retrying ?? 0,
        }),
      );
      setReloadToken((n) => n + 1);
    });
  }, [selected]);

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
  const counts = useFixCounts(libraryIds, reloadToken);

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
          <div className="needs-fixing-toolbar">
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

            {/* Beside the selector, because "which library?" and "re-ask this
                library's settled rows" are one thought. Deliberately NOT inside the
                queue panel: it is not a fix for any one row, it is a question put
                to the whole library. */}
            <button
              className="auth-submit needs-fixing-recheck-button"
              type="button"
              data-testid="needs-fixing-recheck-button"
              disabled={rechecking || selected === ""}
              title="Ask the metadata provider again about everything in this library that is still unmatched. Items already matched are left alone."
              onClick={() => void recheck(selected)}
            >
              {rechecking ? "Re-checking…" : "Re-check unmatched items"}
            </button>
          </div>

          {rechecking && (
            <p className="status status-loading" data-testid="needs-fixing-recheck-progress" role="status">
              {recheckProgressText(recheckProgress)}
            </p>
          )}
          {recheckResult && (
            <p className="status" data-testid="needs-fixing-recheck-result" role="status">
              {recheckResult}
            </p>
          )}
          {recheckError && (
            <p className="status status-error" data-testid="needs-fixing-recheck-error" role="alert">
              <span className="dot dot-error" aria-hidden="true" />
              {recheckError}
            </p>
          )}

          {selected && (
            <LibraryQueue
              key={selected}
              libraryId={selected}
              libraryKind={library?.kind ?? "movie"}
              reloadToken={reloadToken}
            />
          )}
        </>
      )}
    </section>
  );
}

// The queue for one Library: the four server lists, folded into one row model, with
// the corrections log kept separate underneath.
//
// reloadToken is the parent's "the server moved underneath you" signal — bumped
// when the Re-check button's pass finishes. It is threaded into the four loaders'
// dependency lists rather than driving a second effect of its own: the four loads
// are already one effect keyed on those loaders, so this re-runs exactly that
// effect, and a separate effect would double-fetch every list on mount.
function LibraryQueue({
  libraryId,
  libraryKind,
  reloadToken = 0,
}: {
  libraryId: string;
  libraryKind: string;
  reloadToken?: number;
}) {
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

  // The per-Show unsettled counts — the half of a collapsed Show row that is in no
  // list above (an unassigned File produces neither a Title nor an Unmatched row).
  // Best-effort on purpose: the collapse itself is driven by the flagged Episodes,
  // which already name their Show, so a failure here costs the unassigned counts
  // and nothing else. Blanking the queue over it would be a much worse trade.
  const [showProblems, setShowProblems] = useState<ShowProblems[]>([]);

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
    [libraryId, reloadToken],
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
    [libraryId, reloadToken],
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
    [libraryId, reloadToken],
  );

  const loadShowProblems = useCallback(
    async (signal?: AbortSignal) => {
      try {
        const shows = await apiClient.listShowProblems(libraryId, signal);
        if (signal?.aborted) return;
        setShowProblems(shows);
      } catch {
        // Best-effort: the Show rows still collapse from the flagged Episodes.
        if (!signal?.aborted) setShowProblems([]);
      }
    },
    [libraryId, reloadToken],
  );

  useEffect(() => {
    const ctrl = new AbortController();
    void loadUnmatched(ctrl.signal);
    void loadOverrides(ctrl.signal);
    void loadEnrichment(ctrl.signal);
    void loadShowProblems(ctrl.signal);
    return () => ctrl.abort();
  }, [loadUnmatched, loadOverrides, loadEnrichment, loadShowProblems]);

  const reloadAll = useCallback(() => {
    void loadUnmatched();
    void loadOverrides();
    void loadEnrichment();
    void loadShowProblems();
    needsReview.reload();
  }, [loadUnmatched, loadOverrides, loadEnrichment, loadShowProblems, needsReview]);

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
        showProblems,
      }),
    [unmatched, needsReview.items, enrichment, overrides, showProblems],
  );

  // Chips count ROWS, following the collapsed shape: a Show with five broken
  // episodes contributes one, because it is one problem. A row holding several
  // classes counts under each of them, so the chips never hide work — which means
  // the chip counts can exceed the total, and honestly so.
  const counts = useMemo(() => {
    const out: Record<string, number> = {};
    for (const it of items) {
      for (const p of it.problems) out[p] = (out[p] ?? 0) + 1;
    }
    return out;
  }, [items]);

  const shown = filter === "all" ? items : items.filter((it) => hasProblem(it, filter));

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
