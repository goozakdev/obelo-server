// Path helpers shared by the Admin correction surfaces. A Match override is keyed
// to an on-disk FOLDER (ADR-0002/0014), so every surface that offers a fix-match —
// the Needs-Fixing queue's Unmatched rows, and the row model that builds them —
// needs the same file-path → folder derivation, and must agree on it exactly.

/** Derive the folder anchor for a fix-match from a file path: the directory the
 * file lives in (the override anchors to the on-disk folder, not the file). A
 * bare file at a root yields the root dir; a file inside a movie folder yields
 * that folder. Handles both POSIX and Windows separators defensively. */
export function folderOf(filePath: string): string {
  const trimmed = filePath.replace(/[/\\]+$/, "");
  const idx = Math.max(trimmed.lastIndexOf("/"), trimmed.lastIndexOf("\\"));
  return idx > 0 ? trimmed.slice(0, idx) : trimmed;
}
