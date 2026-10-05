// Language and label helpers shared by the captions, audio and queue surfaces.

/** The viewer's preferred language as an ISO-639-1 primary subtag, derived from the
 * browser (navigator.language, e.g. "en-US" → "en"). One value for both axes: it is
 * the `preferredSubtitleLang` / `preferredAudioLang` the capability profile sends AND
 * the key the captions and Audio menus order by, so the menu order matches what the
 * server was told. "" when unknown. */
export function preferredLang(): string {
  if (typeof navigator === "undefined") return "";
  const lang = navigator.language || (navigator.languages && navigator.languages[0]) || "";
  return lang.split(/[-_]/)[0]?.toLowerCase() ?? "";
}

/** Sort rank of a track/stream language against the viewer's preference: 0 for the
 * preferred language, 1 otherwise (so `langRank(a) - langRank(b)` sorts preferred
 * first). Nothing is preferred when `preferred` is "". Case-insensitive. */
export function langRank(language: string | undefined, preferred: string): 0 | 1 {
  const pref = preferred.toLowerCase();
  return pref !== "" && (language ?? "").toLowerCase() === pref ? 0 : 1;
}

/** A friendly noun for a Title's media kind (the now-playing label's degraded
 * fallback when the detail fetch fails, and the queue rows' kind line). */
export function kindLabel(kind: string): string {
  switch (kind) {
    case "movie":
      return "Movie";
    case "episode":
      return "Episode";
    case "track":
      return "Track";
    case "show":
      return "Show";
    default:
      return kind;
  }
}
