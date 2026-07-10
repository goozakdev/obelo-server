import { useState, type ReactNode } from "react";
import { apiClient } from "../api/client";
import type { EntityMetadataEditInput } from "../api/types";
import { errorMessage } from "../screens/errorMessage";
import type { EditItemTab } from "./EditItemDialog";
import { ArtworkPicker, LockBadge } from "./FixLabel";

// EntityMetadataEditor is the Admin-only "Fix label" affordance for a browse PARENT
// — Show / Artist / Album (item-editing/03, ADR-0019): hand-edit the parent's
// descriptive fields. Each save Locks the fields it wrote so re-enrichment never
// overwrites the correction; a Locked field shows a badge + Release. Renaming here
// changes only the DISPLAY label (never identity, watch state, or the active
// Enrichment override), and this box is kept visibly distinct from "Fix info" so a
// rename is never mistaken for a re-identification. Per-item; never cascades to
// children. Seasons get no editor. The image pickers moved OUT of this form into
// dedicated per-role artwork tabs (artwork-management/01); this keeps only the
// descriptive-field editing.

type EntityKind = "shows" | "artists" | "albums";

// The editable fields per parent kind (the parent schema only holds what each kind
// actually carries — an Album has no content rating / network).
const KIND_CONFIG: Record<EntityKind, { fields: EntityFieldKey[] }> = {
  shows: {
    fields: ["title", "overview", "contentRating", "network", "genres"],
  },
  artists: {
    fields: ["title", "overview", "genres"],
  },
  albums: {
    fields: ["title", "genres"],
  },
};

type EntityFieldKey = "title" | "overview" | "contentRating" | "network" | "genres";

/** The per-role artwork tabs for a browse PARENT's Edit-item dialog
 * (artwork-management/01, ADR-0026): Show → Poster + Background + Logo; Artist →
 * Artist Photo; Album → Album Cover. Each tab's body is an ArtworkPicker wired to the
 * entity artwork endpoints — it auto-searches on open (the dialog mounts only the
 * active tab) and applies + Locks on click. `onChanged` refetches the detail so the
 * served parent artwork reloads (its URL carries its own version) and the Locked
 * badge reflects the pick/release. The music tabs are relabels only: the role KEYS
 * stay `poster` (Artist) and `cover` (Album); just the display label changes to
 * "Artist Photo" / "Album Cover". */
export function entityArtworkTabs(
  entityType: EntityKind,
  entityId: string,
  lockedFields: string[] | undefined,
  onChanged: () => void,
): EditItemTab[] {
  const isLocked = (f: string) => (lockedFields ?? []).includes(f);
  const picker = (role: string, label: string) => (
    <ArtworkPicker
      role={role}
      label={label}
      locked={isLocked(role)}
      listCandidates={(r) => apiClient.searchEntityArtworkCandidates(entityType, entityId, r)}
      pick={async (r, url) => {
        await apiClient.pickEntityArtwork(entityType, entityId, r, url);
        onChanged();
      }}
      release={async (field) => {
        await apiClient.releaseEntityLock(entityType, entityId, field);
        onChanged();
      }}
      upload={async (r, file) => {
        await apiClient.uploadEntityArtwork(entityType, entityId, r, file);
        onChanged();
      }}
    />
  );
  switch (entityType) {
    case "shows":
      return [
        { key: "poster", label: "Poster", node: picker("poster", "Poster") },
        { key: "background", label: "Background", node: picker("background", "Background") },
        { key: "logo", label: "Logo", node: picker("logo", "Logo") },
      ];
    case "artists":
      return [{ key: "artist-photo", label: "Artist Photo", node: picker("poster", "Artist Photo") }];
    case "albums":
      return [{ key: "album-cover", label: "Album Cover", node: picker("cover", "Album Cover") }];
  }
}

export default function EntityMetadataEditor({
  entityType,
  entityId,
  displayName,
  overview,
  contentRating,
  network,
  genres,
  lockedFields,
  onChanged,
}: {
  entityType: EntityKind;
  entityId: string;
  displayName: string;
  overview?: string;
  contentRating?: string;
  network?: string;
  genres?: string[];
  lockedFields?: string[];
  /** Called after any successful edit/pick so the detail refetches (a rename
   * changes the parent title, which the edit response doesn't carry). */
  onChanged: () => void;
}) {
  const cfg = KIND_CONFIG[entityType];
  const locked = (f: string) => (lockedFields ?? []).includes(f);
  const [name, setName] = useState(displayName);
  const [ov, setOv] = useState(overview ?? "");
  const [rating, setRating] = useState(contentRating ?? "");
  const [net, setNet] = useState(network ?? "");
  const [gs, setGs] = useState((genres ?? []).join(", "));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);

  async function run(fn: () => Promise<unknown>) {
    setBusy(true);
    setError(null);
    setSaved(false);
    try {
      await fn();
      onChanged();
      setSaved(true);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }

  const save = () =>
    run(() => {
      const edit: EntityMetadataEditInput = {};
      if (cfg.fields.includes("title") && name !== displayName) edit.title = name;
      if (cfg.fields.includes("overview") && ov !== (overview ?? "")) edit.overview = ov;
      if (cfg.fields.includes("contentRating") && rating !== (contentRating ?? "")) edit.contentRating = rating;
      if (cfg.fields.includes("network") && net !== (network ?? "")) edit.network = net;
      if (cfg.fields.includes("genres")) {
        const next = gs.split(",").map((g) => g.trim()).filter(Boolean);
        if (next.join(", ") !== (genres ?? []).join(", ")) edit.genres = next;
      }
      return apiClient.editEntityMetadata(entityType, entityId, edit);
    });

  const release = (field: string) =>
    run(() => apiClient.releaseEntityLock(entityType, entityId, field));

  return (
    <section className="metadata-editor" data-testid="entity-fix-label-editor">
      <h2 className="section-title">Details</h2>
      <p className="detail-hint">
        Type your own information or choose a different provider image. Each edit is
        kept (Locked) so re-enrichment won&rsquo;t overwrite it. This changes only how
        this item is labelled — it never changes identity, and never affects its
        children.
      </p>

      {cfg.fields.includes("title") && (
        <Field label="Display name" lock="title" locked={locked("title")} onRelease={release} busy={busy}>
          <input className="metadata-input" data-testid="entity-edit-title" type="text" value={name}
            disabled={busy} onChange={(e) => setName(e.target.value)} />
        </Field>
      )}
      {cfg.fields.includes("overview") && (
        <Field label="Overview" lock="overview" locked={locked("overview")} onRelease={release} busy={busy}>
          <textarea className="metadata-input" data-testid="entity-edit-overview" rows={3} value={ov}
            disabled={busy} onChange={(e) => setOv(e.target.value)} />
        </Field>
      )}
      {cfg.fields.includes("contentRating") && (
        <Field label="Content rating" lock="content_rating" locked={locked("content_rating")} onRelease={release} busy={busy}>
          <input className="metadata-input" data-testid="entity-edit-content-rating" type="text" value={rating}
            disabled={busy} onChange={(e) => setRating(e.target.value)} />
        </Field>
      )}
      {cfg.fields.includes("network") && (
        <Field label="Network / label" lock="network" locked={locked("network")} onRelease={release} busy={busy}>
          <input className="metadata-input" data-testid="entity-edit-network" type="text" value={net}
            disabled={busy} onChange={(e) => setNet(e.target.value)} />
        </Field>
      )}
      {cfg.fields.includes("genres") && (
        <Field label="Genres (comma-separated)" lock="genres" locked={locked("genres")} onRelease={release} busy={busy}>
          <input className="metadata-input" data-testid="entity-edit-genres" type="text" value={gs}
            disabled={busy} onChange={(e) => setGs(e.target.value)} />
        </Field>
      )}

      <div className="metadata-actions">
        <button className="auth-submit" data-testid="entity-save-metadata" type="button" disabled={busy} onClick={save}>
          {busy ? "Saving…" : "Save & lock"}
        </button>
        {saved && (
          <span className="status status-ok" data-testid="entity-metadata-saved" role="status">
            Saved.
          </span>
        )}
      </div>

      {error && (
        <p className="status status-error" data-testid="entity-metadata-edit-error" role="alert">
          <span className="dot dot-error" aria-hidden="true" />
          {error}
        </p>
      )}
    </section>
  );
}

function Field({
  label,
  lock,
  locked,
  onRelease,
  busy,
  children,
}: {
  label: string;
  lock: string;
  locked: boolean;
  onRelease: (field: string) => void;
  busy: boolean;
  children: ReactNode;
}) {
  return (
    <label className="metadata-field">
      <span className="metadata-label">
        {label}
        <LockBadge field={lock} locked={locked} onRelease={onRelease} busy={busy} />
      </span>
      {children}
    </label>
  );
}
