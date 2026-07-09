import type { TitleDetail, TitleSummary } from "../api/types";

// Project a Title's full {@link TitleDetail} down to the {@link TitleSummary} a
// Queue entry carries (the single-Title build + the fallback need it). The LIVE
// watch state (the local watched/resume, which a manual toggle may have moved)
// wins over the server's snapshot, so a single-Title Queue resumes from the right
// spot. Shared by the movie/episode detail (TitleDetailScreen) and the music
// track detail (music/TrackDetailScreen).
export function titleDetailSummary(
  title: TitleDetail,
  watched: boolean,
  resumeMs: number,
): TitleSummary {
  return {
    id: title.id,
    kind: title.kind,
    title: title.title,
    year: title.year,
    needsReview: title.needsReview,
    ambiguous: title.ambiguous,
    tmdbId: title.tmdbId,
    imdbId: title.imdbId,
    addedAt: title.addedAt,
    resumePositionMs: resumeMs,
    watched,
    genres: title.genres,
    contentRating: title.contentRating || undefined,
    enrichmentStatus: title.enrichmentStatus || undefined,
  };
}
