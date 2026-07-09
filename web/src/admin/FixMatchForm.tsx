import { useState, type FormEvent } from "react";
import { apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";

// The fix-match form (issue 07): given a folder anchor (the directory of an
// Unmatched file, or a needs-review Title's folder), the Admin supplies a
// corrected identity — a Title (+ optional Year), or an embedded tmdb/imdb id —
// and POSTs it to /libraries/{id}/fix-match. The override persists across
// rescans (server-owned). On success we refresh the surrounding surfaces via
// onApplied; server validation (missing identity, etc.) surfaces as a readable
// inline error rather than swallowing or crashing.
//
// At least one identity signal is required; the server enforces this (400
// BAD_REQUEST), but we also keep the submit disabled until the Admin has typed
// something so the common case never round-trips to fail. An ID is a sufficient
// signal on its own: when only a TMDB/IMDB id is given, the server resolves the
// canonical title + year from it, so the Admin never has to type them.
//
// When the anchor is an existing needs-review Title (titleId set) AND the Admin
// supplied an external id, we ALSO immediately enrichment-match that Title
// (PUT /titles/{id}/enrichmentMatch) so the corrected metadata + artwork are
// fetched and applied right away — the identity fix-match alone only re-files the
// folder on the next scan and never touches metadata (ADR-0002/0014). Without a
// titleId (the Unmatched-file case, where no Title exists yet) this step is
// skipped and the behavior is the pure folder override as before.

export default function FixMatchForm({
  libraryId,
  folderPath,
  titleId,
  onApplied,
  onCancel,
}: {
  libraryId: string;
  /** The on-disk folder the override anchors to (server-required, absolute). */
  folderPath: string;
  /** The existing Title this fix resolves, when fixing a needs-review item. When
   * set and an external id is supplied, the Title is enrichment-matched so its
   * metadata/artwork update immediately. Omitted for an Unmatched file (no Title). */
  titleId?: string;
  /** Called after a successful fix-match so the caller can refresh its lists. */
  onApplied: () => void;
  /** Optional: dismiss the form without applying. */
  onCancel?: () => void;
}) {
  const [title, setTitle] = useState("");
  const [year, setYear] = useState("");
  const [tmdbId, setTmdbId] = useState("");
  const [imdbId, setImdbId] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // At least one identity signal must be present to bother submitting.
  const hasIdentity =
    title.trim() !== "" ||
    tmdbId.trim() !== "" ||
    imdbId.trim() !== "";

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    if (submitting || !hasIdentity) return;
    setSubmitting(true);
    setError(null);
    const yearNum = year.trim() === "" ? undefined : Number(year.trim());
    if (yearNum != null && Number.isNaN(yearNum)) {
      setError("Year must be a number.");
      setSubmitting(false);
      return;
    }
    const tmdb = tmdbId.trim();
    const imdb = imdbId.trim();
    try {
      await apiClient.fixMatch(libraryId, {
        folderPath,
        title: title.trim() || undefined,
        year: yearNum,
        tmdbId: tmdb || undefined,
        imdbId: imdb || undefined,
      });
      // Resolving an existing needs-review Title with an external id: fetch and
      // apply the corrected metadata + artwork now (the identity override alone
      // only re-files on the next scan). Done after the override is recorded so a
      // rescan re-resolves to the same corrected identity.
      if (titleId && (tmdb || imdb)) {
        await apiClient.setEnrichmentMatch(titleId, {
          tmdbId: tmdb || undefined,
          imdbId: imdb || undefined,
        });
      }
      onApplied();
    } catch (err) {
      setError(errorMessage(err));
      setSubmitting(false);
    }
  }

  return (
    <form
      className="fix-match-form card"
      data-testid="fix-match-form"
      data-folder-path={folderPath}
      onSubmit={onSubmit}
    >
      <p className="fix-match-anchor">
        Fix match for{" "}
        <code className="fix-match-folder" data-testid="fix-match-folder">
          {folderPath}
        </code>
      </p>

      <label className="field">
        <span className="field-label">Title</span>
        <input
          className="field-input"
          data-testid="fix-match-title"
          type="text"
          value={title}
          onChange={(e) => setTitle(e.target.value)}
          placeholder="Corrected title"
        />
      </label>

      <label className="field">
        <span className="field-label">Year</span>
        <input
          className="field-input"
          data-testid="fix-match-year"
          type="text"
          inputMode="numeric"
          value={year}
          onChange={(e) => setYear(e.target.value)}
          placeholder="e.g. 2021"
        />
      </label>

      <label className="field">
        <span className="field-label">TMDB id</span>
        <input
          className="field-input"
          data-testid="fix-match-tmdb"
          type="text"
          value={tmdbId}
          onChange={(e) => setTmdbId(e.target.value)}
          placeholder="optional"
        />
      </label>

      <label className="field">
        <span className="field-label">IMDB id</span>
        <input
          className="field-input"
          data-testid="fix-match-imdb"
          type="text"
          value={imdbId}
          onChange={(e) => setImdbId(e.target.value)}
          placeholder="optional"
        />
      </label>

      <p className="fix-match-hint" data-testid="fix-match-hint">
        Enter a TMDB or IMDB id and the title and year are looked up from it —
        you only need the title/year when you don&rsquo;t have an id.
      </p>

      <div className="fix-match-actions">
        <button
          className="nav-link"
          type="submit"
          data-testid="fix-match-submit"
          disabled={submitting || !hasIdentity}
        >
          {submitting ? "Applying…" : "Apply fix-match"}
        </button>
        {onCancel && (
          <button
            className="nav-link"
            type="button"
            data-testid="fix-match-cancel"
            onClick={onCancel}
            disabled={submitting}
          >
            Cancel
          </button>
        )}
      </div>

      {error && (
        <p
          className="status status-error"
          data-testid="fix-match-error"
          role="alert"
        >
          <span className="dot dot-error" aria-hidden="true" />
          {error}
        </p>
      )}
    </form>
  );
}

/** Derive the folder anchor for a fix-match from a file path: the directory the
 * file lives in (the override anchors to the on-disk folder, not the file). A
 * bare file at a root yields the root dir; a file inside a movie folder yields
 * that folder. Handles both POSIX and Windows separators defensively. */
export function folderOf(filePath: string): string {
  const trimmed = filePath.replace(/[/\\]+$/, "");
  const idx = Math.max(trimmed.lastIndexOf("/"), trimmed.lastIndexOf("\\"));
  return idx > 0 ? trimmed.slice(0, idx) : trimmed;
}
