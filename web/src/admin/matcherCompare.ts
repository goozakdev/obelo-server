// The comparison the file matcher exists to make: does this File actually belong
// on this Slot?
//
// It is deliberately a plain module with no React in it, because it is the part
// most likely to be subtly wrong and the part whose wrongness is most expensive.
// The screen's whole value is that a highlight means "look at this"; a comparison
// that lights up correctly-matched rows destroys that faster than showing nothing
// would, since the Admin learns to ignore it.
//
// Two comparisons, with different rules:
//
//   * NUMBERS are compared AS NUMBERS, and disagreement is highlighted hard on
//     both sides — because that disagreement IS the decision being made (PRD user
//     story 2: assigning against the filename must be conscious, never a slip).
//
//   * TITLES are compared only after normalizing away everything that is not the
//     title: the container-name prefix, the extension, the release tags, the case
//     and the punctuation. What survives is diffed CHARACTER BY CHARACTER with no
//     similarity threshold — so a provider's `Fillibuster` shows against a file's
//     `Filibuster`, while `Parks.and.Rec.S06E06.1080p.WEB-DL.x264-GRP` normalizes
//     to nothing at all and therefore lights up no row.
//
// Nothing here is named after a season or an episode: the Album matcher compares a
// track filename against a track title by exactly these rules.

/** Tokens that mean "everything after me is technical, not the title". A real
 * filename puts the title BEFORE its release tags, so the first one of these ends
 * the title — which is what makes a release group (`x264-GRP`, `[SPARKS]`)
 * disappear without a list of every group name ever minted. */
const TECHNICAL_TAGS = new Set([
  // resolution / scan
  "480p", "576p", "720p", "1080p", "1080i", "2160p", "4k", "uhd", "sd", "hd", "fhd",
  // source
  "web", "webrip", "web-dl", "webdl", "bluray", "blu-ray", "brrip", "bdrip", "bdremux",
  "hdtv", "pdtv", "dvdrip", "dvd", "hdrip", "remux", "dl", "rip", "cam", "ts",
  // platform
  "amzn", "nf", "netflix", "dsnp", "hmax", "atvp", "hulu", "pcok", "stan", "ip",
  // codec
  "x264", "x265", "h264", "h265", "h", "avc", "hevc", "xvid", "divx", "av1",
  "10bit", "8bit", "hi10p",
  // audio
  "aac", "aac2", "ac3", "eac3", "ddp", "ddp5", "dd", "dd5", "dts", "dtshd",
  "truehd", "atmos", "flac", "mp3", "opus", "2ch", "6ch",
  // grading / edition markers
  "hdr", "hdr10", "dv", "dolbyvision", "sdr", "proper", "repack", "internal",
  "limited", "extended", "uncut", "unrated", "readnfo", "subbed", "dubbed",
]);

/** Tokens deleted wherever they appear rather than truncating the title: a year
 * can sit BEFORE the title (`Show.2019.S01E01.The.Title`), so truncating at one
 * would throw the title away. */
const YEAR_RE = /^(19|20)\d{2}$/;
/** A channel layout written with a dot (`5.1`, `7.1`, `2.0`). Deleted BEFORE
 * tokenizing, because tokenizing would leave a bare `1` behind — and a bare digit
 * cannot be deleted as a token without also eating the `1` out of a real title
 * like *Chapter 1*. */
const CHANNEL_LAYOUT_RE = /\b[2567]\.[01]\b/g;
/** The naming convention's multi-part marker — part of the FILING, not the title. */
const PART_RE = /^(part|pt|cd|disc|disk)\s*\d+$/;
/** Everything a position token can look like: `s06e05`, `s06e05e06`, `06x05`,
 * `e05`, `ep05`, `s06`, and the bare words around them. */
const POSITION_RES = [
  /^s\d{1,4}(e\d{1,4})+$/,
  /^\d{1,3}x\d{1,4}$/,
  /^e(p|pisode)?\d{1,4}$/,
  /^s\d{1,4}$/,
  /^season$/,
  /^episode$/,
];

/** Split a path into its last segment, tolerating both separators. */
export function basename(path: string): string {
  const trimmed = path.replace(/[/\\]+$/, "");
  const idx = Math.max(trimmed.lastIndexOf("/"), trimmed.lastIndexOf("\\"));
  return idx >= 0 ? trimmed.slice(idx + 1) : trimmed;
}

/** Break a string into comparable lowercase word tokens: every separator a release
 * name uses (dots, underscores, hyphens, brackets, apostrophes) is a break, and
 * everything that is not a letter or a digit is dropped. */
function tokenize(text: string): string[] {
  return text
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, " ")
    .split(" ")
    .filter((t) => t !== "");
}

/** The normalized form of a title the PROVIDER gave us: case and punctuation only,
 * because a provider title has no extension, no tags and no prefix to remove. */
export function comparableTitle(raw: string): string {
  return tokenize(raw).join(" ");
}

/** True when two tokens are the same word, allowing the abbreviation a filename
 * routinely uses for a long show name (`Parks.and.Rec` for *Parks and
 * Recreation*). Three characters is the floor: shorter and every `a`/`of` in a
 * title would swallow a word it has nothing to do with. */
function tokensAlike(a: string, b: string): boolean {
  if (a === b) return true;
  if (a.length >= 3 && b.startsWith(a)) return true;
  if (b.length >= 3 && a.startsWith(b)) return true;
  return false;
}

/** Drop the container's own name from the front of a file's tokens. Done on the
 * TOKEN list rather than by string prefix so an abbreviated or differently
 * punctuated prefix still goes — which is the common case, since the folder name
 * and the provider's name for the same Show rarely agree exactly. */
function stripContainerPrefix(tokens: string[], containerTitle: string): string[] {
  const name = tokenize(containerTitle);
  let i = 0;
  while (i < tokens.length && i < name.length && tokensAlike(tokens[i], name[i])) i++;
  return tokens.slice(i);
}

/** The title a FILENAME claims, with everything that is not a title removed.
 *
 * Returns "" when the filename carries no title at all — which is the normal case
 * for a scene release (`Parks.and.Rec.S06E06.1080p.WEB-DL.x264-GRP`) and is why
 * such a row shows no highlighting: there is nothing to disagree with, and an
 * empty string diffed against a provider title would light up every character of
 * a perfectly correct match. */
export function titleFromFilename(path: string, containerTitle = ""): string {
  const withoutExt = basename(path)
    .replace(/\.[a-z0-9]{1,5}$/i, "")
    .replace(CHANNEL_LAYOUT_RE, " ");
  const tokens = tokenize(withoutExt);
  // Positions, years, part markers and channel-count leftovers are deleted in
  // place; a technical tag TRUNCATES, because everything after the first one is
  // the encoder's business (including the release group, which no list can name).
  const kept: string[] = [];
  for (const token of tokens) {
    if (TECHNICAL_TAGS.has(token)) break;
    if (YEAR_RE.test(token)) continue;
    if (PART_RE.test(token)) continue;
    if (POSITION_RES.some((re) => re.test(token))) continue;
    kept.push(token);
  }
  return stripContainerPrefix(kept, containerTitle).join(" ");
}

export type DiffKind = "same" | "removed" | "added";

/** One run of characters and where it lives: in both strings, only in the first
 * (`removed`), or only in the second (`added`). Rendering the first side uses
 * `same` + `removed`; the second uses `same` + `added`. */
export interface DiffSegment {
  text: string;
  kind: DiffKind;
}

/** A character-level diff with NO similarity threshold — the point being that a
 * one-letter disagreement is exactly the case a threshold would hide, and it is
 * also the case an Admin most needs to see (`Fillibuster` / `Filibuster`).
 *
 * Plain LCS. The inputs are normalized titles, so tens of characters; the guard
 * below is only there so a pathological input degrades to "wholly different"
 * instead of allocating a huge table. */
export function diffChars(a: string, b: string): DiffSegment[] {
  if (a === b) return a === "" ? [] : [{ text: a, kind: "same" }];
  if (a === "") return [{ text: b, kind: "added" }];
  if (b === "") return [{ text: a, kind: "removed" }];
  if (a.length > 400 || b.length > 400) {
    return [
      { text: a, kind: "removed" },
      { text: b, kind: "added" },
    ];
  }

  const n = a.length;
  const m = b.length;
  // lcs[i][j] = length of the longest common subsequence of a[i:] and b[j:].
  const lcs: number[][] = Array.from({ length: n + 1 }, () => new Array<number>(m + 1).fill(0));
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      lcs[i][j] = a[i] === b[j] ? lcs[i + 1][j + 1] + 1 : Math.max(lcs[i + 1][j], lcs[i][j + 1]);
    }
  }

  const out: DiffSegment[] = [];
  const push = (text: string, kind: DiffKind) => {
    const last = out[out.length - 1];
    if (last && last.kind === kind) last.text += text;
    else out.push({ text, kind });
  };
  let i = 0;
  let j = 0;
  while (i < n && j < m) {
    if (a[i] === b[j]) {
      push(a[i], "same");
      i++;
      j++;
    } else if (lcs[i + 1][j] >= lcs[i][j + 1]) {
      push(a[i], "removed");
      i++;
    } else {
      push(b[j], "added");
      j++;
    }
  }
  if (i < n) push(a.slice(i), "removed");
  if (j < m) push(b.slice(j), "added");
  return out;
}

/** The result of comparing one File against one Slot. */
export interface TitleComparison {
  /** The normalized provider title, "" when there is no record (degraded path). */
  slotTitle: string;
  /** The normalized title the filename claims, "" when it claims none. */
  fileTitle: string;
  /** True only when BOTH sides said something and the two disagree. Everything
   * else — a bare Slot, a scene release, an exact match — is silence. */
  differs: boolean;
  segments: DiffSegment[];
}

export function compareTitles(
  slotName: string | undefined,
  filePath: string,
  containerTitle = "",
): TitleComparison {
  const slotTitle = comparableTitle(slotName ?? "");
  const fileTitle = titleFromFilename(filePath, containerTitle);
  // Either side empty means there is nothing to compare, NOT a disagreement.
  // Getting this backwards is what would light up every scene-named file in the
  // library, which is most of them.
  if (slotTitle === "" || fileTitle === "" || slotTitle === fileTitle) {
    return { slotTitle, fileTitle, differs: false, segments: [] };
  }
  return { slotTitle, fileTitle, differs: true, segments: diffChars(slotTitle, fileTitle) };
}

/** The result of comparing the numbers a filename claims against a Slot's own. */
export interface PositionComparison {
  /** The position the filename claims, when it claims one. */
  claimed?: { group: number; slot: number };
  groupDiffers: boolean;
  slotDiffers: boolean;
  differs: boolean;
}

/** Compare the positions a File's filename claims against the Slot it now fills,
 * AS NUMBERS — `S3` and `Season 03` are the same season, and only a real numeric
 * disagreement is worth an Admin's attention.
 *
 * A File that claims the target among ANY of its parsed positions agrees: a file
 * named `S01E01-02` legitimately claims two Slots and disagrees with neither. */
export function comparePosition(
  parsed: readonly { group: number; slot: number }[],
  target: { group: number; slot: number },
): PositionComparison {
  if (parsed.length === 0) {
    return { groupDiffers: false, slotDiffers: false, differs: false };
  }
  if (parsed.some((p) => p.group === target.group && p.slot === target.slot)) {
    return { claimed: target, groupDiffers: false, slotDiffers: false, differs: false };
  }
  // Compare against the claim nearest the target so a two-Slot file is measured
  // against the half it is actually being argued with.
  const claimed = [...parsed].sort(
    (x, y) =>
      Math.abs(x.group - target.group) * 1000 +
      Math.abs(x.slot - target.slot) -
      (Math.abs(y.group - target.group) * 1000 + Math.abs(y.slot - target.slot)),
  )[0];
  const groupDiffers = claimed.group !== target.group;
  const slotDiffers = claimed.slot !== target.slot;
  return { claimed, groupDiffers, slotDiffers, differs: groupDiffers || slotDiffers };
}

/** The word runs of a string, with where each one sat. The breaks are exactly
 * `tokenize`'s; the offsets are what lets the ORIGINAL text be cut, so an elided
 * filename keeps its own punctuation instead of being rebuilt from tokens. */
function tokenSpans(text: string): { token: string; start: number; end: number }[] {
  const spans: { token: string; start: number; end: number }[] = [];
  const re = /[a-z0-9]+/gi;
  let m: RegExpExecArray | null;
  while ((m = re.exec(text)) !== null) {
    spans.push({ token: m[0].toLowerCase(), start: m.index, end: m.index + m[0].length });
  }
  return spans;
}

/** What stands in for the container's name at the front of a filename. */
export const ELISION = "…";

/** Words a title carries at the front and a filename may not (or the other way
 * round). Matches the backend's sort key and the browse letter-jump. */
const LEADING_ARTICLES = new Set(["the", "a", "an"]);

/** How many tokens to skip on `tokens` before comparing it against `other`: one,
 * when `tokens` opens with an article and `other` does not open with the SAME
 * one. Both sides keep their article when both have it, so `The Wire` still
 * matches `The Wire` on token one. */
function leadingArticles(tokens: string[], other: string[]): number {
  if (tokens.length === 0 || !LEADING_ARTICLES.has(tokens[0])) return 0;
  return other.length > 0 && other[0] === tokens[0] ? 0 : 1;
}

/** A filename with the container's own name cut off the front and replaced by an
 * ellipsis: `Show - S03E01 - Holiday Knights.mkv` becomes
 * `… - S03E01 - Holiday Knights.mkv`.
 *
 * DISPLAY ONLY — nothing is ever compared against this. Its whole job is to bring
 * the part of the filename that VARIES to the front, so it can be read against
 * the Slot's own one-line label directly above it. Every File in a container
 * repeats the same prefix, which makes that prefix the one run of characters that
 * can never tell the Admin anything, while costing the most horizontal room.
 *
 * The prefix is found by the same token rules the title comparison uses, so an
 * abbreviated one (`Parks.and.Rec` for *Parks and Recreation*) goes too, and the
 * cut then runs on to the position, taking the separators and stray punctuation
 * between them with it — everything up to `S03E01`, and nothing after. */
export function elideContainerPrefix(path: string, containerTitle: string): string {
  const { elided, rest } = splitContainerPrefix(path, containerTitle);
  return elided + rest;
}

/** The same label, cut where the elision ends, so the two halves can be given
 * different weight: the stand-in is identical on every File in the container and
 * is there only to say "a name was here", while `rest` is the half being read.
 * `elided` is "" when nothing was cut. */
export function splitContainerPrefix(
  path: string,
  containerTitle: string,
): { elided: string; rest: string } {
  const name = basename(path);
  // The extension is not part of the name being elided — it is not a word the
  // prefix could eat, and counting it would let `Show.mkv` elide down to `….mkv`.
  const stem = name.replace(/\.[a-z0-9]{1,5}$/i, "");
  const spans = tokenSpans(stem);
  const container = tokenize(containerTitle);
  // A leading article is dropped from EACH side independently, because the two
  // sides disagree about it constantly and the walk below stops dead at the first
  // token that does not match. A Show filed as `Fresh Prince of Bel-Air` against a
  // file named `The Fresh Prince of Bel-Air S05E01…` failed on token one and kept
  // the whole name. This is the same article rule the backend's sort key and the
  // browse letter-jump already apply, so all three agree on where a name starts.
  const fileStart = leadingArticles(spans.map((s) => s.token), container);
  const nameStart = leadingArticles(container, spans.map((s) => s.token));
  let i = 0;
  while (
    fileStart + i < spans.length &&
    nameStart + i < container.length &&
    tokensAlike(spans[fileStart + i].token, container[nameStart + i])
  ) {
    i++;
  }
  // Everything matched is measured from the front of the FILE, article included:
  // the article was never the point, and it belongs to the name being hidden.
  i = i === 0 ? 0 : fileStart + i;
  // Nothing matched, or the filename is nothing BUT the container's name: show it
  // whole. An ellipsis and an extension would name no file at all.
  if (i === 0 || i >= spans.length) return { elided: "", rest: name };
  // Having found the name, the cut runs FORWARD to the position, so that whatever
  // sits between the two goes with it: the punctuation the name ended in, a year,
  // a separator, an edition marker. All of it is as fixed across the container as
  // the name itself, and every character of it pushes the position — the one thing
  // being read — further from the left edge.
  //
  // The search starts AFTER the name, and only runs at all because the name
  // matched: a filename that never names its container keeps every word, or
  // `Holiday Knights S01E01.mkv` would lose the title to the elision.
  let cut = i;
  for (let j = i; j < spans.length; j++) {
    if (POSITION_RES.some((re) => re.test(spans[j].token))) {
      cut = j;
      break;
    }
  }
  return { elided: ELISION, rest: name.slice(spans[cut].start) };
}
