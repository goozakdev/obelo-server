import { useEffect, useRef, useState, type ChangeEvent, type DragEvent } from "react";
import type { ArtworkCandidate } from "../api/types";
import { errorMessage } from "../screens/errorMessage";

/** The image types the upload affordance accepts (ADR-0026); the server validates
 * these again by sniffing the bytes and rejects anything else with a clear error. */
const UPLOAD_ACCEPT = "image/jpeg,image/png,image/webp";

// Shared Edit-item primitives (item-editing/03, ADR-0019): the Locked badge +
// release control (used by the "Fix label" descriptive-field forms) and the
// ArtworkPicker. As of artwork-management/01 the picker no longer lives inside the
// "Fix label" tab — it is the body of a dedicated per-role artwork tab (Poster /
// Background / Artist Photo / Album Cover) in the Edit-item dialog, auto-searching
// on open and applying + Locking on click. Both primitives Lock what they write so
// re-enrichment won't overwrite it; each is per-item and NEVER cascades, and stays
// deliberately DISTINCT from "Fix info" (which record decorates the item) and the
// "Wrong item" identity correction so a rename/repaint is never a re-identification.

/** LockBadge shows a "Locked" pill for a hand-edited field, with an inline Release
 * control that returns it to auto (re-enrichment refreshes it again). */
export function LockBadge({
  field,
  locked,
  onRelease,
  busy,
}: {
  field: string;
  locked: boolean;
  onRelease: (field: string) => void;
  busy?: boolean;
}) {
  if (!locked) return null;
  return (
    <span className="fixlabel-lock" data-testid={`lock-badge-${field}`}>
      <span className="badge badge-locked">Locked</span>
      <button
        className="nav-link fixlabel-release"
        type="button"
        data-testid={`release-${field}`}
        disabled={busy}
        onClick={() => onRelease(field)}
      >
        Release
      </button>
    </span>
  );
}

// ArtworkPicker lets an Admin choose a specific provider image for a role
// (poster/background/cover) from its dedicated Edit-item tab (artwork-management/01,
// ADR-0026). It AUTO-SEARCHES on open: because EditItemDialog mounts only the active
// tab's node, the picker queries the providers on mount — no "Choose image"
// pre-click. Clicking a candidate applies it (sets + Locks the role server-side) and
// the on-screen image reloads via the caller's `artworkVersion` bump; the picked one
// is marked "Applied". Releasing the role's Lock returns it to auto. A picked
// provider image is stored Fetched, so local on-disk artwork still wins (server
// precedence); an unreachable/unconfigured provider degrades to a clear, non-blocking
// message rather than a hang (ADR-0001).
//
// It also carries the UPLOAD affordance (artwork-management/03): a drag-drop zone +
// Browse button. Uploading IS selecting — the dropped/chosen file becomes the
// artwork in one step (the server stores it, fills the role, and Locks it), and an
// Uploaded image OUTRANKS every other source, even a library-folder poster.jpg. The
// zone needs no provider, so it stays available even when the candidate grid is
// empty/offline (the graceful upload-only state). A rejected upload (wrong type /
// too big) surfaces its error and leaves the current image unchanged.
export function ArtworkPicker({
  role,
  label,
  locked,
  listCandidates,
  pick,
  release,
  upload,
}: {
  role: string;
  label: string;
  locked: boolean;
  /** Loads the provider's images for this role. */
  listCandidates: (role: string) => Promise<ArtworkCandidate[]>;
  /** Applies the picked image URL to this role (sets + Locks it). */
  pick: (role: string, url: string) => Promise<void>;
  /** Releases this role's lock back to auto. */
  release: (role: string) => Promise<void>;
  /** Uploads your own image for this role — uploading is selecting (sets + Locks). */
  upload: (role: string, file: File) => Promise<void>;
}) {
  const [candidates, setCandidates] = useState<ArtworkCandidate[] | null>(null);
  const [loading, setLoading] = useState(true);
  const [pickingUrl, setPickingUrl] = useState<string | null>(null);
  const [appliedUrl, setAppliedUrl] = useState<string | null>(null);
  const [releasing, setReleasing] = useState(false);
  const [uploading, setUploading] = useState(false);
  const [dragOver, setDragOver] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);

  // Auto-search on open: the picker is mounted only while its tab is active (the
  // dialog renders just the active tab's node), so loading candidates on mount IS
  // "auto-search on open". role is fixed for a given picker instance, so this runs
  // once per activation.
  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setError(null);
    listCandidates(role)
      .then((cs) => {
        if (!cancelled) setCandidates(cs);
      })
      .catch((err) => {
        if (!cancelled) setError(errorMessage(err));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [role]);

  const busy = pickingUrl !== null || releasing || uploading;

  async function choose(url: string) {
    if (busy) return;
    setPickingUrl(url);
    setError(null);
    try {
      await pick(role, url);
      // Uploading/picking keeps the grid up so the choice reads as "Applied" and a
      // second pick is possible without re-opening the tab.
      setAppliedUrl(url);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setPickingUrl(null);
    }
  }

  async function doRelease() {
    if (busy) return;
    setReleasing(true);
    setError(null);
    try {
      await release(role);
      setAppliedUrl(null);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setReleasing(false);
    }
  }

  async function doUpload(file: File) {
    if (busy) return;
    setUploading(true);
    setError(null);
    try {
      await upload(role, file);
      // The upload is now the artwork; it isn't one of the candidate URLs, so clear
      // any prior "Applied" marker. The caller's refetch reloads the on-screen image.
      setAppliedUrl(null);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setUploading(false);
    }
  }

  function onDrop(e: DragEvent) {
    e.preventDefault();
    setDragOver(false);
    const file = e.dataTransfer.files?.[0];
    if (file) void doUpload(file);
  }

  function onFileChosen(e: ChangeEvent<HTMLInputElement>) {
    const file = e.target.files?.[0];
    // Reset so choosing the same file again re-fires change (a retry after error).
    e.target.value = "";
    if (file) void doUpload(file);
  }

  return (
    <div className="fixlabel-artwork" data-testid={`artwork-picker-${role}`}>
      <div className="fixlabel-artwork-head">
        <span className="metadata-label">
          {label}
          <LockBadge field={role} locked={locked} onRelease={() => void doRelease()} busy={busy} />
        </span>
      </div>

      <div className="fixlabel-artwork-panel" data-testid={`artwork-panel-${role}`}>
        {loading && (
          <p className="status status-loading" data-testid={`artwork-loading-${role}`}>
            Loading images&hellip;
          </p>
        )}
        {!loading && !error && candidates && candidates.length === 0 && (
          <p className="status" data-testid={`artwork-none-${role}`}>
            No images offered for this item.
          </p>
        )}
        {!loading && candidates && candidates.length > 0 && (
          <ul className="fixlabel-artwork-grid" data-testid={`artwork-grid-${role}`}>
            {candidates.map((c) => {
              const applied = appliedUrl === c.url;
              const dims = c.width && c.height ? `${c.width}×${c.height}` : undefined;
              return (
                <li key={c.url} className="fixlabel-artwork-item">
                  <button
                    className={`fixlabel-artwork-choice${applied ? " is-applied" : ""}`}
                    type="button"
                    data-testid="artwork-choice"
                    data-url={c.url}
                    aria-pressed={applied}
                    disabled={busy}
                    onClick={() => void choose(c.url)}
                    title={dims}
                  >
                    <img src={c.url} alt="" loading="lazy" />
                    {dims && (
                      <span className="fixlabel-artwork-dims" data-testid="artwork-dims">
                        {dims}
                      </span>
                    )}
                    {applied && pickingUrl !== c.url && (
                      <span className="fixlabel-artwork-applied">Applied</span>
                    )}
                    {pickingUrl === c.url && (
                      <span className="fixlabel-artwork-applying">Applying…</span>
                    )}
                  </button>
                </li>
              );
            })}
          </ul>
        )}
        {error && (
          <p className="status status-error" data-testid={`artwork-error-${role}`} role="alert">
            <span className="dot dot-error" aria-hidden="true" />
            {error}
          </p>
        )}

        {/* Upload your own (ADR-0026): drag-drop or Browse. Needs no provider, so it
            stays available even when the grid above is empty/offline. Uploading IS
            selecting — the file becomes the artwork, outranking every other source. */}
        <div
          className={`fixlabel-artwork-upload${dragOver ? " is-dragover" : ""}`}
          data-testid={`artwork-upload-${role}`}
          onDrop={onDrop}
          onDragOver={(e) => {
            e.preventDefault();
            setDragOver(true);
          }}
          onDragLeave={() => setDragOver(false)}
        >
          <p className="fixlabel-artwork-upload-hint">
            {uploading ? "Uploading…" : "Drag an image here, or"}
          </p>
          <button
            className="nav-link fixlabel-artwork-browse"
            type="button"
            data-testid={`artwork-browse-${role}`}
            disabled={busy}
            onClick={() => fileInputRef.current?.click()}
          >
            Upload your own
          </button>
          <input
            ref={fileInputRef}
            className="visually-hidden"
            type="file"
            accept={UPLOAD_ACCEPT}
            data-testid={`artwork-file-${role}`}
            onChange={onFileChosen}
          />
          <p className="fixlabel-artwork-upload-note">JPEG, PNG, or WebP, up to 16 MiB.</p>
        </div>
      </div>
    </div>
  );
}
