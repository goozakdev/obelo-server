import type { CascadeSummary } from "../api/types";

/** What an album cascade actually did, in the Admin's terms.
 *
 * The numbers matter because the claim being made is quantitative — one gesture on
 * behalf of fourteen tracks — and a summary the Admin never sees would make one good
 * album pick indistinguishable from a hopeful click. `attention` is stated whenever
 * it is non-zero rather than buried: a track the picked release's track list could
 * not place is still queued, and they need to know that before deciding the album is
 * done.
 *
 * It lives in its own module because BOTH surfaces that apply an album correction
 * say it — the collapsed queue row (album-resolves-its-tracks/09) and the edition
 * picker (issue 12, ADR-0052) — and the one thing worse than no summary is two
 * surfaces reporting the same cascade in two different sentences. */
export function cascadeSummaryText(s: CascadeSummary): string {
  const total = s.updated + s.attention;
  if (total === 0) {
    return "Applied to the album — its track list matched none of these tracks, so they are still queued.";
  }
  const noun = total === 1 ? "track" : "tracks";
  const rest =
    s.attention > 0
      ? ` ${s.attention} still ${s.attention === 1 ? "needs" : "need"} attention.`
      : "";
  return `${s.updated} of ${total} ${noun} matched from the album’s track list.${rest}`;
}
