import { useCallback, useEffect, useState } from "react";
import { apiClient } from "../api/client";
import type { EpisodeCandidate, SeasonSummary } from "../api/types";
import { errorMessage } from "../screens/errorMessage";

// Step two of correcting a TV episode: having picked the series, pick the episode
// within it that this file actually is.
//
// It exists because picking the series alone could never fix a whole class of file.
// Enrichment resolves an Episode as /tv/{show}/season/{S}/episode/{E}, where S and
// E come from the FILENAME (ADR-0002 — local naming is the identity authority). So
// when a provider numbers a series differently from the disk — the classic case
// being a run of episodes at the end of a season that the provider counts as the
// start of the next one — re-picking the series just asks for the same wrong
// episode again. The Admin was stuck with a button that looked like it worked.
//
// Choosing here writes a lookup-only pin: the file keeps its place in the library,
// its parsed numbers, and every User's watch history, and simply gains the right
// title, overview and still (ADR-0014).

export default function EpisodeChooser({
  titleId,
  seriesTitle,
  externalId,
  onPick,
  onBack,
}: {
  titleId: string;
  /** The series just picked, named so the Admin can see what they are inside. */
  seriesTitle: string;
  /** That series' provider id — what the episode list is fetched against. */
  externalId: string;
  /** Apply: pin this file to the chosen provider episode. */
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

  // `which` undefined asks the server for its default — the season this file is
  // already filed under, which is the list the Admin is most likely looking for
  // (right season / wrong episode, or a run that slipped into the next season).
  const load = useCallback(
    async (which?: number) => {
      setLoading(true);
      setError(null);
      try {
        const res = await apiClient.listEpisodeCandidates(titleId, externalId, which);
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
    [titleId, externalId],
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
        Pins which episode&rsquo;s details this file shows. It stays where it is in
        your library and keeps its watch history.
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
