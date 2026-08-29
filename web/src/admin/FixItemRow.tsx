import { useState } from "react";
import { Link } from "react-router-dom";
import { apiClient } from "../api/client";
import type { EnrichmentCandidate, EpisodeCandidate } from "../api/types";
import { errorMessage } from "../screens/errorMessage";
import Poster, { initials } from "../browse/Poster";
import EpisodeChooser from "./EpisodeChooser";
import FixItemPicker from "./FixItemPicker";
import { kindLabel, type FixItem } from "./needsFixing";
import type { Provider } from "./searchRef";

// One row of the Needs-Fixing queue. Every row — whatever went wrong — answers the
// same four questions in the same places, which is the whole point of collapsing
// four lists into one: what it is (kind badge + breadcrumb), which file, what is
// wrong (one sentence), and what to do about it.
//
// The breadcrumb is load-bearing, not decoration. The old screen printed an
// enrichment row as a bare episode name, so "Pilot" told the Admin nothing about
// which show, which season, or which file — and two shows can both have a "Pilot".
//
// Opening a row mounts the picker, which is what triggers its search: search is a
// live, rate-limited provider call, so it happens when the Admin actually looks at
// a row, never for the whole list at once.

export default function FixItemRow({
  item,
  libraryId,
  provider,
  onResolved,
  onIdentityCorrected,
}: {
  item: FixItem;
  libraryId: string;
  /** Which provider a pasted bare id belongs to (from the Library's media kind). */
  provider: Provider;
  /** Called after the row's problem is resolved, so the queue refetches. */
  onResolved: () => void;
  /** Called after an identity correction, which only takes effect on the next scan
   * — the screen collects these and offers ONE rescan rather than scanning per row. */
  onIdentityCorrected: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const canFix = item.route !== "none";

  // --- applying a picked record ---------------------------------------------

  // An identity correction: record the folder-keyed Match override so the next scan
  // re-files it (ADR-0002/0014), and — when the item already exists as a Title —
  // pin the same record for metadata so the poster and description land NOW instead
  // of after the scan. A needs-review row is also dismissed, because the Admin just
  // resolved the very uncertainty the flag was raised about.
  async function applyIdentity(candidate: EnrichmentCandidate) {
    await apiClient.fixMatch(libraryId, {
      folderPath: item.folderPath,
      title: candidate.title,
      year: candidate.year,
      // Only the video providers issue ids fix-match can store; a MusicBrainz id has
      // no column, and music identity comes from tags anyway, so the title carries it.
      tmdbId: provider === "tmdb" ? candidate.externalId : undefined,
    });
    if (item.titleId !== "" && provider === "tmdb") {
      await apiClient.applyEnrichmentOverride(item.titleId, candidate.externalId);
    }
    if (item.canDismiss) {
      if (item.showId !== "") await apiClient.reviewShow(item.showId);
      else if (item.titleId !== "") await apiClient.reviewTitle(item.titleId);
    }
    onIdentityCorrected();
  }

  // A metadata correction: pin which record decorates this Title and re-enrich just
  // it. Identity and every User's watch state are untouched (ADR-0014) — no rescan
  // is needed or wanted, so this route never reports an identity correction.
  async function applyMetadata(candidate: EnrichmentCandidate) {
    await apiClient.applyEnrichmentOverride(item.titleId, candidate.externalId);
  }

  // An Episode additionally pins WHICH episode of the picked series it is. Without
  // this the lookup falls back to the season/episode parsed from the filename, so a
  // file the provider numbers differently — a run moved into the next season — can
  // never be matched however often the series is re-picked. Still metadata only:
  // the file keeps its place in the library and its watch history.
  async function applyEpisode(series: EnrichmentCandidate, ep: EpisodeCandidate) {
    await apiClient.applyEnrichmentOverride(item.titleId, series.externalId, undefined, {
      season: ep.season,
      episode: ep.episode,
    });
    setOpen(false);
    onResolved();
  }

  async function onApply(candidate: EnrichmentCandidate) {
    if (item.route === "fix-match") await applyIdentity(candidate);
    else await applyMetadata(candidate);
    setOpen(false);
    onResolved();
  }

  // --- the non-search actions -----------------------------------------------

  async function run(action: () => Promise<unknown>) {
    setBusy(true);
    setError(null);
    try {
      await action();
      onResolved();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }

  const dismiss = () =>
    run(() =>
      item.showId !== ""
        ? apiClient.reviewShow(item.showId)
        : apiClient.reviewTitle(item.titleId),
    );

  const discard = () => run(() => apiClient.deleteOverride(libraryId, item.overrideId));

  // --- rendering -------------------------------------------------------------

  const search = (query: string, page: number) =>
    item.route === "enrichment-override"
      ? apiClient.searchEnrichmentCandidates(item.titleId, query, { page })
      : apiClient.searchLibraryEnrichmentCandidates(libraryId, query, { page });

  const preview = (ref: string) =>
    item.route === "enrichment-override"
      ? apiClient.previewExternalCandidate(item.titleId, ref)
      : apiClient.previewLibraryExternalCandidate(libraryId, ref);

  // Applying means genuinely different things on the two routes, and the hint is
  // where that difference is stated — the Admin should never have to know the
  // vocabulary to predict the blast radius.
  const applyHint =
    item.route === "enrichment-override"
      ? "Updates artwork and details only — keeps watch history and your edits."
      : "Re-files this item under the correct title on the next scan.";

  return (
    <li
      className={`fix-item admin-panel-row${open ? " is-open" : ""}`}
      data-testid="fix-item"
      data-problem={item.problem}
      data-kind={item.kind}
    >
      {/* The poster is the fastest confirmation there is: an Admin recognizes the
          right film or show at a glance, where a title and a path only tell them
          what the scanner already thought. Without it, "Looks right" is a guess.
          Poster falls back to a placeholder when the item has no image, so a row
          never breaks over a missing one. */}
      <div className="fix-item-art" data-testid="fix-item-art">
        {item.artworkUrl !== "" ? (
          <Poster
            titleId={item.titleId || item.showId || item.key}
            title={item.name}
            src={item.artworkUrl}
            version={item.artworkVersion}
          />
        ) : (
          // No entity to fetch artwork for (an Unmatched file, an orphaned
          // correction). Keep the slot so rows stay aligned, but show the item's
          // initials the way every other placeholder in the app does.
          <div
            className="poster poster-placeholder"
            role="img"
            aria-label={`${item.name} (no artwork)`}
          >
            <span className="poster-initials" aria-hidden="true">
              {initials(item.name)}
            </span>
          </div>
        )}
      </div>

      <div className="fix-item-body">
      <div className="fix-item-head">
        <span className="fix-item-kind" data-testid="fix-item-kind">
          {kindLabel(item.kind)}
        </span>
        <div className="fix-item-identity">
          {item.breadcrumb.length > 0 && (
            <span className="fix-item-breadcrumb" data-testid="fix-item-breadcrumb">
              {item.breadcrumb.join(" › ")}
              {" › "}
            </span>
          )}
          <span className="fix-item-name" data-testid="fix-item-name">
            {item.name}
            {item.year > 0 ? ` (${item.year})` : ""}
          </span>
        </div>
        {canFix && (
          <button
            className={`nav-link fix-item-toggle${open ? " is-active" : ""}`}
            type="button"
            data-testid="fix-item-toggle"
            aria-expanded={open}
            onClick={() => setOpen((v) => !v)}
          >
            {open ? "Close" : "Fix this"}
          </button>
        )}
      </div>

      {/* What the item actually resolved to. A needs-review row is flagged for an
          uncertain parse — most often a MISSING YEAR — so its own parsed name can
          never settle whether the filing is right; the matched record can, and it
          carries the very year the parse lacks. */}
      {item.matchedAs !== "" && (
        <p className="fix-item-matched" data-testid="fix-item-matched">
          <span className="fix-item-matched-label">Matched to</span> {item.matchedAs}
        </p>
      )}
      {item.matchedAs === "" && !item.hasMatch && item.canDismiss && (
        <p className="fix-item-matched is-unmatched" data-testid="fix-item-matched">
          Not matched to any provider record yet — there is nothing to check this
          against.
        </p>
      )}

      <p className="fix-item-problem" data-testid="fix-item-problem">
        {item.problemText}
      </p>

      {item.path !== "" && (
        <code className="fix-item-path" data-testid="fix-item-path" title={item.path}>
          {item.path}
        </code>
      )}
      {item.path === "" && (
        <span className="fix-item-path fix-item-path-missing" data-testid="fix-item-path">
          No file on disk — every file for this item is missing.
        </span>
      )}

      <div className="fix-item-actions">
        {item.canDismiss && (
          <button
            className="nav-link"
            type="button"
            data-testid="fix-item-dismiss"
            disabled={busy}
            title="The way this was filed is correct — stop flagging it"
            onClick={() => void dismiss()}
          >
            {busy ? "Working…" : "Looks right"}
          </button>
        )}
        {item.overrideId !== "" && (
          <button
            className="nav-link"
            type="button"
            data-testid="fix-item-discard"
            disabled={busy}
            title="Remove this correction — its folder is gone, so it can never apply"
            onClick={() => void discard()}
          >
            {busy ? "Working…" : "Discard correction"}
          </button>
        )}
        {item.detailPath !== "" && (
          <Link className="nav-link" to={item.detailPath} data-testid="fix-item-open">
            Open item
          </Link>
        )}
      </div>

      {error && (
        <p className="status status-error" data-testid="fix-item-error" role="alert">
          <span className="dot dot-error" aria-hidden="true" />
          {error}
        </p>
      )}

      </div>

      {open && canFix && (
        <FixItemPicker
          seed={item.searchSeed}
          provider={provider}
          applyLabel="Use this"
          applyHint={applyHint}
          search={search}
          preview={preview}
          onApply={onApply}
          onCancel={() => setOpen(false)}
          chooseEpisode={
            // Only an Episode needs the second step: for every other kind the picked
            // record IS the answer. A metadata row is the one that can pin an
            // episode — an identity fix-match re-files a whole folder, not a file.
            item.kind === "episode" && item.route === "enrichment-override"
              ? (series, back) => (
                  <EpisodeChooser
                    titleId={item.titleId}
                    seriesTitle={series.title}
                    externalId={series.externalId}
                    onPick={(ep) => applyEpisode(series, ep)}
                    onBack={back}
                  />
                )
              : undefined
          }
        />
      )}
    </li>
  );
}
