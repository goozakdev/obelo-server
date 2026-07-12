import { Link } from "react-router-dom";
import { useLibraries } from "./librariesContext";

// The parent "Back" link shown at the top of a detail screen. Unlike a browser
// Back button, it always returns to the item's PARENT — a Movie to its Library, an
// Episode to its Show, an Album to its Artist, and so on — and names the
// destination so the link reads "← Movies" / "← The Bear" rather than a bare
// "Back". Each detail screen computes {to, label} from the loaded item (its
// parent id is deterministic, so a deep-link / refresh returns to the same place)
// and renders this.

export default function BackLink({ to, label }: { to: string; label: string }) {
  return (
    <Link className="nav-link back-link back-link-lg" data-testid="back-link" to={to}>
      ← {label}
    </Link>
  );
}

// useLibraryName resolves a Library id to its display name from the app-wide
// Libraries list (loaded once per session, kept warm across navigations). Used by
// the detail screens whose parent is a Library (Movie / Show / Artist) so the
// Back link can name it. Falls back to `fallback` while the list is loading or
// when the id isn't found (e.g. an inaccessible Library).
export function useLibraryName(
  libraryId: string | undefined,
  fallback = "Library",
): string {
  const libraries = useLibraries();
  if (libraryId && libraries.status === "ready") {
    const found = libraries.data.find((l) => l.id === libraryId);
    if (found) return found.name;
  }
  return fallback;
}
