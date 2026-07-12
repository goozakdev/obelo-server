package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/marioquake/juicebox/internal/events"
	"github.com/marioquake/juicebox/internal/scanner"
	"github.com/marioquake/juicebox/internal/store"
)

// targeted_scan_handlers.go serves the Targeted scan (ADR-0030): POST
// /{titles|shows|albums|artists}/{id}/scan re-walks just that browsable entity's
// on-disk folders. It resolves the scope (the entity's Library + present File
// paths → their anchor folders), then hands the folder set to the scanner, which
// shares the per-Library scan lock with a full scan (ADR-0031). Admin-only — the
// caller wires it behind requireAdmin. Returns 202 with the (scope-tagged) scan
// status, exactly like the full-Library scan.

// targetedScanEntity is the browsable entity a Targeted scan is launched from. It
// binds the route's scope resolver to the anchor kind that maps a File path to the
// folder the scanner keys on (NeedsReviewAnchor): a Movie Title → its folder /
// bare file, a Show → its top-level folder, an Album/Artist → its Tracks' folders.
type targetedScanEntity struct {
	resolve    func(ScanScopeResolver, string) (store.EntityScanScope, error)
	anchorKind string // "movie" | "show" | "track" (per store.NeedsReviewAnchor)
}

var targetedScanEntities = map[string]targetedScanEntity{
	"title":  {resolve: func(r ScanScopeResolver, id string) (store.EntityScanScope, error) { return r.TitleScanScope(id) }, anchorKind: "movie"},
	"show":   {resolve: func(r ScanScopeResolver, id string) (store.EntityScanScope, error) { return r.ShowScanScope(id) }, anchorKind: "show"},
	"album":  {resolve: func(r ScanScopeResolver, id string) (store.EntityScanScope, error) { return r.AlbumScanScope(id) }, anchorKind: "track"},
	"artist": {resolve: func(r ScanScopeResolver, id string) (store.EntityScanScope, error) { return r.ArtistScanScope(id) }, anchorKind: "track"},
}

// handleTargetedScan builds the POST handler for one entity kind ("title" /
// "show" / "album" / "artist"). id is the entity id already parsed from the path.
func handleTargetedScan(deps Deps, entityKind, id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ent, ok := targetedScanEntities[entityKind]
		if !ok || id == "" || deps.ScanScope == nil {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}

		sc, err := ent.resolve(deps.ScanScope, id)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, codeNotFound, "item not found", nil)
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "scan failed", nil)
			return
		}

		lib, err := deps.ScanScope.LibraryByID(sc.LibraryID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "scan failed", nil)
			return
		}

		// Map the entity's present File paths to the distinct anchor folders the
		// scanner walks — the same folders the full scan keys overrides on. A hidden
		// entity (every File Missing) yields no paths, hence no folders: nothing to
		// scan (resurrection is out of scope for v1 — ADR-0030).
		folders := scopeFolders(ent.anchorKind, sc.Paths, lib.Roots)
		if len(folders) == 0 {
			writeError(w, http.StatusConflict, codeNoFiles,
				"nothing to scan: this item has no files on disk", nil)
			return
		}

		scope := scanner.TargetedScope{Folders: folders, Label: sc.Label}

		// Progress + terminal events carry the scope label so a client renders
		// "Scanning <label>…" and matches the terminal delta to this entity.
		var onProgress func(scanner.Progress)
		if deps.Events != nil {
			onProgress = func(p scanner.Progress) {
				ev := toScanEvent(p)
				ev.Scope = sc.Label
				deps.Events.PublishScanProgress(ev)
			}
		}
		// Post-scan side-effects, identical to the full scan: auto-enrich the touched
		// Titles (EnrichPending → only the new/never-enriched ones, honoring the
		// Library's Enrichment policy, so it no-ops when enrichment is off) and nudge
		// clients to refetch. On error, emit a terminal event so a client's indicator
		// clears (ADR-0006 keeps this off the scanner's events-free core).
		libID := lib.ID
		label := sc.Label
		done := func(scanErr error) {
			if scanErr != nil {
				if deps.Events != nil {
					deps.Events.PublishScanProgress(events.ScanProgress{LibraryID: libID, Scope: label, Complete: true})
				}
				return
			}
			if deps.EnrichTrigger != nil {
				deps.EnrichTrigger(libID)
			}
			if deps.Events != nil {
				deps.Events.PublishLibraryUpdated(libID)
			}
		}

		err = deps.Scanner.StartTargetedScan(context.Background(), libID, scope, onProgress, done)
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "item not found", nil)
			return
		case errors.Is(err, scanner.ErrScanInProgress):
			// A scan (full or targeted) is already running for this Library: report the
			// in-flight status instead of starting a second one (idempotent — ADR-0031).
			st, sErr := deps.ScanStatus.ScanStatusByLibrary(libID)
			if sErr != nil {
				writeError(w, http.StatusInternalServerError, codeInternal, "scan failed", nil)
				return
			}
			writeJSON(w, http.StatusAccepted, toScanStatus(st))
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "scan failed", nil)
			return
		}

		st, err := deps.ScanStatus.ScanStatusByLibrary(libID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "scan failed", nil)
			return
		}
		writeJSON(w, http.StatusAccepted, toScanStatus(st))
	}
}

// scopeFolders maps an entity's present File paths to the distinct anchor folders
// a Targeted scan walks, via the same per-kind rule the needs-review/fix-match
// surfaces use (store.NeedsReviewAnchor). Empty or unfixable paths are dropped;
// the result is de-duplicated, preserving first-seen order for determinism.
func scopeFolders(anchorKind string, paths []string, roots []store.LibraryRoot) []string {
	rootPaths := make([]string, 0, len(roots))
	for _, r := range roots {
		rootPaths = append(rootPaths, r.Path)
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		folder := store.NeedsReviewAnchor(anchorKind, p, rootPaths)
		if folder == "" || seen[folder] {
			continue
		}
		seen[folder] = true
		out = append(out, folder)
	}
	return out
}
