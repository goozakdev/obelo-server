import { describe, it, expect } from "vitest";
import { normalizeHome } from "./normalize";

describe("normalizeHome rows", () => {
  it("leaves rows undefined when the Server predates homeRows", () => {
    expect(normalizeHome({ continueWatching: [] }).rows).toBeUndefined();
  });

  it("normalizes Title and Show items by row kind/mediaKind, in order", () => {
    const home = normalizeHome({
      rows: [
        { kind: "myList", title: "My List", items: [{ id: "m1", kind: "movie", title: "A" }] },
        {
          kind: "discovery", title: "On HBO", mediaKind: "tv",
          items: [{ id: "s1", libraryId: "tv", kind: "show", title: "S" }],
          source: { type: "network", value: "HBO", libraryIds: ["tv"] }, total: 9,
        },
      ],
    } as never);
    expect(home.rows?.map((r) => r.kind)).toEqual(["myList", "discovery"]);
    expect(home.rows?.[0].titles[0]).toMatchObject({ id: "m1", watched: false, genres: [] });
    expect(home.rows?.[1].shows[0]).toMatchObject({ id: "s1", unwatchedEpisodeCount: 0 });
    expect(home.rows?.[1].titles).toEqual([]);
    expect(home.rows?.[1].source?.value).toBe("HBO");
    expect(home.rows?.[1].total).toBe(9);
  });
});
