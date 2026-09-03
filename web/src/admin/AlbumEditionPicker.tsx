import { useCallback, useEffect, useState } from "react";
import { apiClient } from "../api/client";
import type {
  AlbumEdition,
  AlbumEditions,
  CascadeSummary,
  EditionSource,
  EntityEnrichmentDetail,
} from "../api/types";
import { errorMessage } from "../screens/errorMessage";
import { cascadeSummaryText } from "./cascadeSummary";

// The Edition section (ADR-0052, album-resolves-its-tracks/12) — the last half of
// the correction the operator could only make on someone else's website:
//
//   "requires going to the web page and getting a URL because i was unable to
//    select a specific edition. It shows the best guess, but I cant choose a
//    specific edition out of that release-group."
//
// An album IS a release-group (ADR-0038), and a release-group holds editions: the
// original, the remaster, the deluxe with three bonus tracks, the Japanese pressing
// with four. WHICH one an album's files are decides which track list decorates them
// — and until this section existed the system chose by track-count fit and never
// showed its work.
//
// Three things this section must do, in order of how badly their absence hurt:
//
//   1. LIST them, with the four facts that tell two pressings apart.
//   2. State the LOCAL track count beside the list. The decision is almost always
//      "the one with my number of tracks", and a list of counts with no reference
//      count asks the Admin to do arithmetic against a number that isn't on screen.
//   3. Say which one is in use AND ON WHOSE AUTHORITY. "In use — best guess" is the
//      sentence the operator was owed: it names the thing they were arguing with.
//
// Choosing applies through the album's EXISTING override with the cascade — the same
// call the record picker makes, carrying the release id — and reports the mapped /
// attention counts, which is the operator's proof the pin was right (ADR-0052: "a
// wrong pin now propagates confidently… the cascade reports how many tracks it
// mapped, so a pin that produces a nonsense count is visible immediately").
//
// A provider that cannot answer renders a QUIET HINT, never an error page: the
// pasted-URL escape hatch above still works, it is how this was done before, and
// issue 10 made it stick.

/** What the "in use" marker says, per tier of ADR-0052's precedence. The word
 * "guess" is deliberate on the `fit` tier — it is the system admitting which of the
 * three answers this is, which is the whole reason the list is worth showing. */
const IN_USE_LABEL: Record<EditionSource, string> = {
  chosen: "In use — your choice",
  tagged: "In use — from the file tags",
  fit: "In use — best guess",
};

/** One edition as a single line: date · country · format · N tracks. Missing fields
 * are dropped rather than rendered as "Unknown" — a digital release with no country
 * is normal, and four "Unknown"s in a row is noise the Admin has to read past. The
 * track count is ALWAYS present, because it is the one they are actually comparing. */
export function editionLine(e: AlbumEdition): string {
  const tracks = `${e.trackCount} ${e.trackCount === 1 ? "track" : "tracks"}`;
  return [e.date, e.country, e.format, tracks].filter((p) => (p ?? "") !== "").join(" · ");
}

export default function AlbumEditionPicker({
  albumId,
  onApplied,
}: {
  albumId: string;
  /** Called with the re-enriched album detail after an edition is applied, so the
   * screen or queue row that owns this section can refresh itself. */
  onApplied?: (detail: EntityEnrichmentDetail) => void;
}) {
  const [data, setData] = useState<AlbumEditions | null>(null);
  const [loading, setLoading] = useState(true);
  // `unavailable` is NOT an error state. The provider being unable to list editions
  // means this section has nothing to offer, and the Admin still has the paste box.
  const [unavailable, setUnavailable] = useState(false);
  const [applying, setApplying] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [summary, setSummary] = useState<CascadeSummary | null>(null);

  const load = useCallback(
    async (signal?: AbortSignal) => {
      setLoading(true);
      try {
        const res = await apiClient.listAlbumEditions(albumId, signal);
        setData(res);
        setUnavailable(false);
      } catch {
        // An aborted read says nothing about the provider — it is this component
        // being unmounted, or StrictMode's double-mount discarding its first effect.
        // Reporting it as "unavailable" would race the live read and could leave the
        // section claiming the source is down while its answer sits in state.
        if (signal?.aborted) return;
        // Unconfigured, disabled, or unreachable — all the same to this section, and
        // all of them degrade to the escape hatch rather than to an error page.
        setData(null);
        setUnavailable(true);
      } finally {
        if (!signal?.aborted) setLoading(false);
      }
    },
    [albumId],
  );

  useEffect(() => {
    const ac = new AbortController();
    void load(ac.signal);
    return () => ac.abort();
  }, [load]);

  async function choose(e: AlbumEdition) {
    if (applying || !data?.releaseGroupId) return;
    setApplying(e.releaseId);
    setError(null);
    try {
      // The album's existing apply, carrying the edition (ADR-0052). `cascade: true`
      // is the point of the gesture: the pin is only worth making because the picked
      // release's track list then lands on this album's tracks in one go.
      const detail = await apiClient.applyEntityEnrichmentOverride(
        "albums",
        albumId,
        data.releaseGroupId,
        true,
        e.releaseId,
      );
      setSummary(detail.cascade ?? null);
      onApplied?.(detail);
      // Re-read so the "in use" marker moves onto the row they just picked, and says
      // "your choice" rather than "best guess". Without this the section would still
      // be describing the state before the click.
      await load();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setApplying(null);
    }
  }

  if (loading && !data) return null;
  if (unavailable) {
    return (
      <section className="album-editions" data-testid="album-editions">
        <h3 className="section-title">Edition</h3>
        <p className="detail-hint" data-testid="album-editions-unavailable">
          The metadata provider can&rsquo;t list this album&rsquo;s editions right now.
          You can still paste a MusicBrainz release URL above to name one.
        </p>
      </section>
    );
  }
  // No release-group, or a release-group with no releases: there is nothing to
  // choose, and the album's own match — the picker above — is what to fix.
  if (!data || data.editions.length === 0) return null;

  return (
    <section className="album-editions" data-testid="album-editions">
      <h3 className="section-title">Edition</h3>
      {/* The local count, stated beside the list so a fitting edition is obvious
          without arithmetic. */}
      <p className="detail-hint" data-testid="album-editions-local-count">
        This album has {data.localTrackCount}{" "}
        {data.localTrackCount === 1 ? "track" : "tracks"}. Pick the edition these files
        are and its track list is what names them.
      </p>

      <ul className="album-edition-list" data-testid="album-edition-list">
        {data.editions.map((e) => {
          const inUse = e.releaseId === data.inUseReleaseId;
          const fits = e.trackCount === data.localTrackCount;
          // The button stays on a row that is in use by TAG or by FIT: choosing it is
          // not a no-op there. It converts the system's guess into a human's
          // assertion, which is what licenses position-alone mapping (ADR-0052) — the
          // difference between ten unmatched tracks and none.
          const alreadyChosen = inUse && data.inUseSource === "chosen";
          return (
            <li
              className={`album-edition${inUse ? " is-in-use" : ""}${fits ? " is-fitting" : ""}`}
              key={e.releaseId}
              data-testid="album-edition"
              data-release-id={e.releaseId}
              data-in-use={inUse ? "true" : undefined}
            >
              <div className="album-edition-body">
                <span className="album-edition-line" data-testid="album-edition-line">
                  {editionLine(e)}
                </span>
                {e.disambiguation && (
                  <span className="album-edition-hint">{e.disambiguation}</span>
                )}
                {fits && (
                  <span className="album-edition-fits" data-testid="album-edition-fits">
                    matches this album&rsquo;s track count
                  </span>
                )}
                {inUse && (
                  <span className="album-edition-in-use" data-testid="album-edition-in-use">
                    {IN_USE_LABEL[data.inUseSource ?? "fit"]}
                  </span>
                )}
              </div>
              {!alreadyChosen && (
                <button
                  className="nav-link album-edition-use"
                  type="button"
                  data-testid="album-edition-use"
                  disabled={applying !== null}
                  onClick={() => void choose(e)}
                >
                  {applying === e.releaseId ? "Applying…" : "Use this edition"}
                </button>
              )}
            </li>
          );
        })}
      </ul>

      {/* What the pick actually did. One decision on behalf of N tracks, so the count
          it moved is the only thing that separates a fix from a hopeful click. */}
      {summary && (
        <p className="status" data-testid="album-edition-cascade" role="status">
          {cascadeSummaryText(summary)}
        </p>
      )}
      {error && (
        <p className="status status-error" data-testid="album-edition-error" role="alert">
          <span className="dot dot-error" aria-hidden="true" />
          {error}
        </p>
      )}
    </section>
  );
}
