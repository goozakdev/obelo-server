import { describe, it, expect } from "vitest";
import {
  normalizeHome,
  normalizeMatchOverride,
  normalizeScanStatus,
  normalizeTitleSummary,
  normalizeTitlesPage,
  normalizeTitleDetail,
  normalizeUnmatchedFile,
} from "./normalize";

describe("normalize (omitempty holes filled)", () => {
  it("treats absent booleans as false and absent numbers as 0 on a summary", () => {
    // The server prunes false/0/absent fields (omitempty). A lean Title arrives
    // with only id/kind/title; components must still see watched=false etc.
    const s = normalizeTitleSummary({ id: "t1", kind: "movie", title: "Dune" });
    expect(s.watched).toBe(false);
    expect(s.needsReview).toBe(false);
    expect(s.ambiguous).toBe(false);
    expect(s.year).toBe(0);
    expect(s.resumePositionMs).toBe(0);
    expect(s.addedAt).toBeUndefined();
  });

  it("preserves present values on a summary", () => {
    const s = normalizeTitleSummary({
      id: "t1",
      kind: "movie",
      title: "Dune",
      year: 2021,
      watched: true,
      resumePositionMs: 5000,
      addedAt: "2026-06-22T10:00:00Z",
    });
    expect(s.year).toBe(2021);
    expect(s.watched).toBe(true);
    expect(s.resumePositionMs).toBe(5000);
    expect(s.addedAt).toBe("2026-06-22T10:00:00Z");
  });

  it("maps an absent nextCursor to null (last page) and titles to an array", () => {
    expect(normalizeTitlesPage({ titles: [] }).nextCursor).toBeNull();
    expect(normalizeTitlesPage({ titles: [], nextCursor: "abc" }).nextCursor).toBe(
      "abc",
    );
    // A wholly empty body still yields an array, not undefined.
    expect(normalizeTitlesPage({} as never).titles).toEqual([]);
  });

  it("normalizes the Home rows (each row → array of normalized summaries)", () => {
    const h = normalizeHome({
      continueWatching: [
        // A Continue Watching entry: the server omits `watched`; it normalizes
        // to false so the card renders a resume marker, not a watched badge.
        { id: "t1", kind: "movie", title: "Dune", resumePositionMs: 42000 },
      ],
      recentlyAdded: [
        { id: "t2", kind: "movie", title: "Zulu", year: 1964, addedAt: "2026-06-22T10:00:00Z" },
      ],
    });
    expect(h.continueWatching).toHaveLength(1);
    expect(h.continueWatching[0]).toMatchObject({ watched: false, resumePositionMs: 42000 });
    expect(h.recentlyAdded[0]).toMatchObject({ year: 1964, resumePositionMs: 0 });
  });

  it("normalizes the Up Next row and the Episode parent context", () => {
    const h = normalizeHome({
      upNext: [
        {
          id: "e2",
          kind: "episode",
          title: "Hands",
          episode: {
            showId: "sh1",
            showTitle: "The Bear",
            seasonId: "s1",
            seasonNumber: 1,
            episodeNumber: 2,
          },
        },
      ],
    });
    expect(h.upNext).toHaveLength(1);
    expect(h.upNext[0].episode?.showTitle).toBe("The Bear");
    expect(h.upNext[0].episode?.episodeNumber).toBe(2);
    // A Movie summary carries no episode context.
    expect(h.continueWatching).toEqual([]);
  });

  it("maps absent/empty Home rows to empty arrays (brand-new server)", () => {
    expect(normalizeHome({})).toEqual({ continueWatching: [], upNext: [], recentlyAdded: [] });
    expect(normalizeHome({ continueWatching: [] })).toEqual({
      continueWatching: [],
      upNext: [],
      recentlyAdded: [],
    });
  });

  it("fills a scan status's omitted counts with 0 and keeps present values", () => {
    // A fresh, never-scanned library: the server omits zero counts and the
    // timestamps; counts must still be numbers so the admin hub can render them.
    const fresh = normalizeScanStatus({ libraryId: "lib1", state: "idle" });
    expect(fresh.titlesFound).toBe(0);
    expect(fresh.filesFound).toBe(0);
    expect(fresh.startedAt).toBeUndefined();
    expect(fresh.errorMessage).toBeUndefined();

    const done = normalizeScanStatus({
      libraryId: "lib1",
      state: "idle",
      titlesFound: 7,
      filesFound: 9,
      finishedAt: "2026-06-23T10:00:00Z",
    });
    expect(done).toMatchObject({ titlesFound: 7, filesFound: 9 });

    const failed = normalizeScanStatus({
      libraryId: "lib1",
      state: "error",
      errorMessage: "root missing",
    });
    expect(failed).toMatchObject({ state: "error", errorMessage: "root missing" });

    // A running Targeted scan carries the entity label through as `scope`.
    const targeted = normalizeScanStatus({
      libraryId: "lib1",
      state: "running",
      scope: "The Wire",
    });
    expect(targeted).toMatchObject({ state: "running", scope: "The Wire" });
  });

  it("fills nested holes on a detail (editions/files/streams → arrays)", () => {
    const d = normalizeTitleDetail({
      id: "t1",
      kind: "movie",
      title: "Dune",
      editions: [{ id: "e1", name: "1080p", files: [{ id: "f1", path: "/x.mp4", container: "mp4" }] }],
    });
    expect(d.watched).toBe(false);
    expect(d.editions[0].files[0].missing).toBe(false);
    expect(d.editions[0].files[0].streams).toEqual([]);
    expect(d.artwork).toEqual([]);
  });

  it("fills an Unmatched file's absent reason with an empty string", () => {
    // The server omits an empty reason; the attention list renders it
    // unconditionally, so it must be a string.
    const lean = normalizeUnmatchedFile({ id: "f1", path: "/m/1080p.mkv" });
    expect(lean.reason).toBe("");
    expect(lean.addedAt).toBeUndefined();

    const full = normalizeUnmatchedFile({
      id: "f2",
      path: "/m/x.mkv",
      reason: "no title in filename",
      addedAt: "2026-06-23T10:00:00Z",
    });
    expect(full).toMatchObject({ reason: "no title in filename", addedAt: "2026-06-23T10:00:00Z" });
  });

  it("normalizes a Match override (absent orphaned/year → false/0)", () => {
    // omitempty: a non-orphaned override omits `orphaned`; it must normalize to
    // a present false so the list can branch on it unconditionally.
    const ov = normalizeMatchOverride({
      id: "o1",
      folderPath: "/m/Yearless Movie",
      title: "Yearless Movie",
      identityKey: "k1",
    });
    expect(ov.orphaned).toBe(false);
    expect(ov.year).toBe(0);

    const orphan = normalizeMatchOverride({
      id: "o2",
      folderPath: "/m/Gone",
      title: "Gone",
      year: 2021,
      tmdbId: "123",
      identityKey: "k2",
      orphaned: true,
      createdAt: "2026-06-23T10:00:00Z",
    });
    expect(orphan).toMatchObject({ orphaned: true, year: 2021, tmdbId: "123" });
  });
});
