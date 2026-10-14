import { describe, it, expect } from "vitest";
import { seeAllPath, filterFromSearch, filterToSearch } from "./homeRows";
import type { HomeRow } from "../api/types";

function discovery(type: string, value: string, extra: Record<string, unknown> = {}): HomeRow {
  return {
    kind: "discovery",
    title: "x",
    mediaKind: "movie",
    titles: [],
    shows: [],
    source: { type, value, libraryIds: ["libA", "libB"], ...extra },
  } as HomeRow;
}

describe("seeAllPath", () => {
  it.each([
    ["genre", "Comedy", "/libraries/libA?genre=Comedy"],
    ["studio", "A24", "/libraries/libA?studio=A24"],
    ["network", "HBO", "/libraries/libA?network=HBO"],
    ["decade", "1980", "/libraries/libA?decade=1980"],
  ])("maps a %s source 1:1 onto the first Library's browse", (type, value, want) => {
    expect(seeAllPath(discovery(type, value))).toBe(want);
  });

  it("carries a person's display name for the chip and escapes values", () => {
    expect(
      seeAllPath(discovery("person", "tmdb:31", { displayValue: "Tom Hanks" })),
    ).toBe("/libraries/libA?person=tmdb%3A31&personName=Tom+Hanks");
    expect(seeAllPath(discovery("genre", "Sci-Fi & Fantasy"))).toBe(
      "/libraries/libA?genre=Sci-Fi+%26+Fantasy",
    );
  });

  it("sends My List to the Playlists page (which lists the Watchlist) and others nowhere", () => {
    expect(seeAllPath({ kind: "myList", title: "My List", titles: [], shows: [] })).toBe("/playlists");
    expect(seeAllPath({ kind: "recentlyAdded", title: "Recently Added", titles: [], shows: [] })).toBeNull();
  });
});

describe("filterFromSearch / filterToSearch", () => {
  it("reads a filter and labels it", () => {
    expect(filterFromSearch(new URLSearchParams("studio=A24"))).toEqual({ type: "studio", value: "A24", label: "A24" });
    expect(filterFromSearch(new URLSearchParams("decade=1980"))?.label).toBe("1980s");
    expect(filterFromSearch(new URLSearchParams("person=tmdb:31&personName=Tom%20Hanks"))).toEqual({
      type: "person", value: "tmdb:31", label: "Tom Hanks",
    });
  });
  it("is null with no filter and ignores unrelated params", () => {
    expect(filterFromSearch(new URLSearchParams(""))).toBeNull();
    expect(filterFromSearch(new URLSearchParams("foo=bar"))).toBeNull();
  });
  it("clearing removes the filter params and keeps others", () => {
    const next = filterToSearch(new URLSearchParams("person=x&personName=Y&foo=bar"));
    expect(next.toString()).toBe("foo=bar");
  });
});
