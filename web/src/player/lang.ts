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

// ISO 639-2 codes (terminologic AND bibliographic) → their ISO 639-1 primary subtag, for
// the languages a media library carries. Anything else compares as itself.
const ISO_639_2_TO_1: Record<string, string> = {
  afr: "af", ara: "ar", bul: "bg", cat: "ca", ces: "cs", cze: "cs", chi: "zh", zho: "zh",
  dan: "da", deu: "de", ger: "de", ell: "el", gre: "el", eng: "en", spa: "es", est: "et",
  eus: "eu", baq: "eu", fas: "fa", per: "fa", fin: "fi", fra: "fr", fre: "fr", heb: "he",
  hin: "hi", hrv: "hr", hun: "hu", hye: "hy", arm: "hy", ind: "id", isl: "is", ice: "is",
  ita: "it", jpn: "ja", kor: "ko", lit: "lt", lav: "lv", mkd: "mk", mac: "mk", msa: "ms",
  may: "ms", nld: "nl", dut: "nl", nor: "no", nob: "nb", nno: "nn", pol: "pl", por: "pt",
  ron: "ro", rum: "ro", rus: "ru", slk: "sk", slo: "sk", slv: "sl", srp: "sr", swe: "sv",
  tha: "th", tur: "tr", ukr: "uk", vie: "vi", cym: "cy", wel: "cy", kat: "ka", geo: "ka",
};

/** A language tag reduced to one comparable key: lower-cased, region/script dropped
 * ("en-US" → "en"), and a 3-letter ISO 639-2 code (either the bibliographic or the
 * terminologic form: ger/deu, fre/fra) mapped to its 2-letter ISO 639-1 form. "" for
 * an empty/unknown tag. */
export function normalizeLang(language: string | undefined): string {
  const primary = (language ?? "").trim().toLowerCase().split(/[-_]/)[0] ?? "";
  return ISO_639_2_TO_1[primary] ?? primary;
}

/** Whether two language tags name the same language (normalizeLang equal). False
 * when either is empty, so two unknowns never match. */
export function sameLang(a: string | undefined, b: string | undefined): boolean {
  const na = normalizeLang(a);
  return na !== "" && na === normalizeLang(b);
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
