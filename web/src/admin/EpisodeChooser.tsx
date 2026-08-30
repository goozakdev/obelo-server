import { useCallback, useEffect, useState } from "react";
import type { EpisodeCandidate, SeasonSummary } from "../api/types";
import { errorMessage } from "../screens/errorMessage";

// Step two of pointing something at a provider episode: having picked the series,
// pick the episode within it.
//
// It exists because picking the series alone could never fix a whole class of file.
// Enrichment resolves an Episode as /tv/{show}/season/{S}/episode/{E}, where S and
// E come from the FILENAME (ADR-0002 — local naming is the identity authority). So
// when a provider numbers a series differently from the disk — the classic case
// being a run of episodes at the end of a season that the provider counts as the
// start of the next one — re-picking the series just asks for the same wrong
// episode again. The Admin was stuck with a button that looked like it worked.
//
// WHERE THIS IS USED. Inside the file matcher, as step two of repointing a Slot's
// RECORD (CONTEXT.md "Episode pin", ADR-0044): ShowMatcherScreen's record picker
// searches a series and then mounts this to pick within it.
//
// It used to live in the Needs-Fixing queue, one flagged Episode at a time, in a
// place where the Admin could not see the season they were fixing. That flow
// retired with file-matcher/07 — a Show's episode problems are one row now, and
// their fix is the matcher — and the pin came with it, narrowed to its real job.
//
// Its one change for that move: it no longer knows WHERE the episodes come from.
// The queue anchored the fetch on a Title; the matcher anchors it on a Show and a
// chosen series (`listSeriesSlots`). So the fetch arrives as `load` and the
// component stays anchor-agnostic — which is also what lets the matcher fill a
// whole run from one pick, since the caller owns the page it loaded.
//
// Choosing here writes a lookup-only pin: the Slot keeps its position, so the
// Episode keeps its place in the library, its identity_key and every User's watch
// history, and simply gains the right title, overview and still (ADR-0014).

/** One page of a series' episode list: the season just loaded, its episodes, and —
 * on the first call only — the series' whole season list. */
export interface EpisodeChooserPage {
  /** Sent once, on the first load; keep it thereafter. */
  seasons?: SeasonSummary[];
  season: number;
  episodes: EpisodeCandidate[];
}

/** Fetch one season's episodes. `which` undefined asks for the caller's default —
 * the season the thing being pinned is already filed under, which is the list the
 * Admin is most likely looking for. */
export type EpisodeChooserLoad = (which?: number) => Promise<EpisodeChooserPage>;

export default function EpisodeChooser({
  seriesTitle,
  load: loadPage,
  onPick,
  onBack,
}: {
  /** The series just picked, named so the Admin can see what they are inside. */
  seriesTitle: string;
  /** Where the episodes come from. See {@link EpisodeChooserLoad}. */
  load: EpisodeChooserLoad;
  /** Apply: pin to the chosen provider episode. */
  onPick: (candidate: EpisodeCandidate) => Promise<void>;
  /** Return to the series list (they picked the wrong show). */
  onBack: () => void;
}) {
  const [seasons, setSeasons] = useState<SeasonSummary[] | null>(null);
  const [season, setSeason] = useState<number | null>(null);
  const [episodes, setEpisodes] = useState<EpisodeCandidate[]>([]);
  const [loading, setLoading] = useState(true);
  const [applying, setApplying] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(
    async (which?: number) => {
      setLoading(true);
      setError(null);
      try {
        const res = await loadPage(which);
        // The season list comes back only on the first request; keep it thereafter.
        if (res.seasons) setSeasons(res.seasons);
        setSeason(res.season);
        setEpisodes(res.episodes);
      } catch (err) {
        setError(errorMessage(err));
      } finally {
        setLoading(false);
      }
    },
    [loadPage],
  );

  useEffect(() => {
    void load();
  }, [load]);

  async function pick(candidate: EpisodeCandidate) {
    const key = `${candidate.season}x${candidate.episode}`;
    if (applying) return;
    setApplying(key);
    setError(null);
    try {
      await onPick(candidate);
    } catch (err) {
      setError(errorMessage(err));
      setApplying(null);
    }
  }

  return (
    <div className="episode-chooser" data-testid="episode-chooser">
      <div className="episode-chooser-head">
        <p className="episode-chooser-series" data-testid="episode-chooser-series">
          <span className="episode-chooser-label">Series</span> {seriesTitle}
        </p>
        <button
          className="nav-link"
          type="button"
          data-testid="episode-chooser-back"
          disabled={applying !== null}
          onClick={onBack}
        >
          Wrong series
        </button>
      </div>

      {seasons && seasons.length > 0 && (
        <label className="field episode-chooser-season">
          <span className="field-label">Season</span>
          <select
            className="field-input"
            data-testid="episode-chooser-season-select"
            value={season ?? ""}
            disabled={loading || applying !== null}
            onChange={(e) => void load(Number(e.target.value))}
          >
            {seasons.map((s) => (
              <option key={s.season} value={s.season}>
                {s.season === 0 ? "Specials" : `Season ${s.season}`}
                {s.episodeCount > 0 ? ` (${s.episodeCount})` : ""}
              </option>
            ))}
          </select>
        </label>
      )}

      {loading && (
        <p className="status status-loading" data-testid="episode-chooser-loading">
          Loading episodes&hellip;
        </p>
      )}

      {!loading && episodes.length === 0 && !error && (
        <p className="status status-empty" data-testid="episode-chooser-empty">
          This season has no episodes listed. Try another season.
        </p>
      )}

      {!loading && episodes.length > 0 && (
        <ul className="episode-choice-list" data-testid="episode-choice-list">
          {episodes.map((e) => {
            const key = `${e.season}x${e.episode}`;
            return (
              <li key={key}>
                <button
                  className="episode-choice"
                  type="button"
                  data-testid="episode-choice"
                  data-season={e.season}
                  data-episode={e.episode}
                  disabled={applying !== null}
                  onClick={() => void pick(e)}
                >
                  {e.stillUrl && (
                    <img className="episode-choice-still" src={e.stillUrl} alt="" loading="lazy" />
                  )}
                  <span className="episode-choice-body">
                    <span className="episode-choice-title">
                      <span className="episode-choice-code">
                        S{String(e.season).padStart(2, "0")}E{String(e.episode).padStart(2, "0")}
                      </span>{" "}
                      {e.name}
                    </span>
                    {e.airDate && <span className="episode-choice-meta">{e.airDate}</span>}
                    {e.overview && (
                      <span className="episode-choice-overview">{e.overview}</span>
                    )}
                  </span>
                  <span className="episode-choice-action">
                    {applying === key ? "Applying…" : "Use this"}
                  </span>
                </button>
              </li>
            );
          })}
        </ul>
      )}

      <p className="fix-apply-hint">
        Pins which episode&rsquo;s details this shows. It stays where it is in your
        library and keeps its watch history.
      </p>

      {error && (
        <p className="status status-error" data-testid="episode-chooser-error" role="alert">
          <span className="dot dot-error" aria-hidden="true" />
          {error}
        </p>
      )}
    </div>
  );
}
