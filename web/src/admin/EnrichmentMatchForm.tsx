import { useState, type FormEvent } from "react";
import { apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";

// The enrichment-match form (external-metadata-enrichment issue 05): given a
// Title whose Enrichment could not settle on a record, the Admin supplies the
// correct external id (TMDB / IMDB / MusicBrainz) and PUTs it to
// /titles/{id}/enrichmentMatch. The server re-enriches just that Title and NEVER
// touches identity / watch state (ADR-0014) — this is deliberately distinct from
// an identity fix-match. On success we refresh the attention list via onApplied;
// server validation (no id supplied) surfaces as a readable inline error.
//
// At least one id is required; the server enforces this (400), but we also keep
// the submit disabled until the Admin has typed something so the common case
// never round-trips to fail.

export default function EnrichmentMatchForm({
  titleId,
  onApplied,
  onCancel,
}: {
  /** The Title to re-point + re-enrich. */
  titleId: string;
  /** Called after a successful match so the caller can refresh its list. */
  onApplied: () => void;
  /** Optional: dismiss the form without applying. */
  onCancel?: () => void;
}) {
  const [tmdbId, setTmdbId] = useState("");
  const [imdbId, setImdbId] = useState("");
  const [musicbrainzId, setMusicbrainzId] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // At least one external id must be present to bother submitting.
  const hasId =
    tmdbId.trim() !== "" ||
    imdbId.trim() !== "" ||
    musicbrainzId.trim() !== "";

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    if (submitting || !hasId) return;
    setSubmitting(true);
    setError(null);
    try {
      await apiClient.setEnrichmentMatch(titleId, {
        tmdbId: tmdbId.trim() || undefined,
        imdbId: imdbId.trim() || undefined,
        musicbrainzId: musicbrainzId.trim() || undefined,
      });
      onApplied();
    } catch (err) {
      setError(errorMessage(err));
      setSubmitting(false);
    }
  }

  return (
    <form
      className="enrichment-match-form card"
      data-testid="enrichment-match-form"
      data-title-id={titleId}
      onSubmit={onSubmit}
    >
      <label className="field">
        <span className="field-label">TMDB id</span>
        <input
          className="field-input"
          data-testid="enrichment-match-tmdb"
          type="text"
          value={tmdbId}
          onChange={(e) => setTmdbId(e.target.value)}
          placeholder="e.g. 12345"
        />
      </label>

      <label className="field">
        <span className="field-label">IMDB id</span>
        <input
          className="field-input"
          data-testid="enrichment-match-imdb"
          type="text"
          value={imdbId}
          onChange={(e) => setImdbId(e.target.value)}
          placeholder="optional"
        />
      </label>

      <label className="field">
        <span className="field-label">MusicBrainz id</span>
        <input
          className="field-input"
          data-testid="enrichment-match-musicbrainz"
          type="text"
          value={musicbrainzId}
          onChange={(e) => setMusicbrainzId(e.target.value)}
          placeholder="optional"
        />
      </label>

      <div className="enrichment-match-actions">
        <button
          className="nav-link"
          type="submit"
          data-testid="enrichment-match-submit"
          disabled={submitting || !hasId}
        >
          {submitting ? "Matching…" : "Apply match"}
        </button>
        {onCancel && (
          <button
            className="nav-link"
            type="button"
            data-testid="enrichment-match-cancel"
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
          data-testid="enrichment-match-error"
          role="alert"
        >
          <span className="dot dot-error" aria-hidden="true" />
          {error}
        </p>
      )}
    </form>
  );
}
