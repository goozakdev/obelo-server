import type { BrowseFilter, HomeRow } from "../api/types";

// Home rows <-> Library browse. A Discovery row's "See all" is the Library browse
// filtered by the row's source; the filter lives in the URL query so the link is
// shareable, and the browse screen reads it back with filterFromSearch. The param
// names are the server's `filter[...]` names; a person's display name rides along
// as `personName` purely for the chip label.

const FILTER_TYPES = ["genre", "studio", "network", "decade", "person"] as const;

/** Where a row's "See all" goes, or null when the row has none. A Discovery row
 * with more than one Library links to the first (no cross-Library browse). My
 * List goes to the Playlists page, where the Watchlist is listed. */
export function seeAllPath(row: HomeRow): string | null {
  if (row.kind === "myList") return "/playlists";
  const src = row.source;
  if (row.kind !== "discovery" || !src || src.libraryIds.length === 0) return null;
  const params = new URLSearchParams({ [src.type]: src.value });
  if (src.type === "person" && src.displayValue) params.set("personName", src.displayValue);
  return `/libraries/${encodeURIComponent(src.libraryIds[0])}?${params.toString()}`;
}

/** The filter applied by a browse URL, with its chip label; null when none. */
export function filterFromSearch(
  params: URLSearchParams,
): (BrowseFilter & { label: string }) | null {
  for (const type of FILTER_TYPES) {
    const value = params.get(type);
    if (!value) continue;
    let label = value;
    if (type === "person") label = params.get("personName") || value;
    else if (type === "decade") label = `${value}s`;
    return { type, value, label };
  }
  return null;
}

/** The query with any filter removed, other params untouched. */
export function filterToSearch(params: URLSearchParams): URLSearchParams {
  const next = new URLSearchParams(params);
  for (const t of FILTER_TYPES) next.delete(t);
  next.delete("personName");
  return next;
}
