package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// placement.go is the storage half of Apply (ADR-0044): one Show's whole
// rearrangement — the Admin's file-anchored decisions, the live Title rows
// re-keyed to their assigned Slots, the merges and splits those Slots imply, and
// the watch state folded onto the new shape — committed in a SINGLE transaction,
// so a failure part-way leaves the Show exactly as it was.
//
// It is deliberately a dumb executor. WHAT the arrangement should be is derived
// once, in scanner.ResolveEpisodes, and planned in catalog/placement.go; this
// file only writes it. That split is what keeps the second writer of Title
// structure from growing structural opinions of its own.
//
// The contrast to keep straight while reading: identity_correction.go's
// CorrectTitleIdentity RESETS watch state, because a Wrong-item correction says
// the file is a DIFFERENT WORK. Nothing here resets anything — a Placement says
// the same work was filed in the wrong place, so history follows the file
// (ADR-0044 vs ADR-0019). Every re-key below is an UPDATE for exactly that
// reason: delete-and-insert would mint a new title_id and silently drop every
// User's watch_state row with it.

// ShowFile is one catalogued File under a Show, with the Episode Title that
// currently owns it — the "before" picture Apply plans against.
type ShowFile struct {
	Path          string
	Present       bool
	DurationMs    int64
	TitleID       string
	IdentityKey   string
	SeasonNumber  int
	EpisodeNumber int
}

// ShowFiles lists every File under a Show (present or Missing), path-ordered, with
// its owning Episode Title. Missing Files are included on purpose: a File the
// Admin previously took off its Slot is soft-deleted, not gone, and re-placing it
// must reuse its stored row rather than mint a second one for the same path.
func (db *DB) ShowFiles(showID string) ([]ShowFile, error) {
	rows, err := db.Query(
		`SELECT f.path, f.present, f.duration_ms, t.id, t.identity_key,
		        t.season_number, t.episode_number
		   FROM files f
		   JOIN editions e ON e.id = f.edition_id
		   JOIN titles   t ON t.id = e.title_id
		   JOIN seasons  s ON s.id = t.season_id
		  WHERE s.show_id = ?
		  ORDER BY f.path, t.identity_key`, showID)
	if err != nil {
		return nil, fmt.Errorf("store: listing show files: %w", err)
	}
	defer rows.Close()
	var out []ShowFile
	for rows.Next() {
		var sf ShowFile
		var present int
		if err := rows.Scan(&sf.Path, &present, &sf.DurationMs, &sf.TitleID,
			&sf.IdentityKey, &sf.SeasonNumber, &sf.EpisodeNumber); err != nil {
			return nil, fmt.Errorf("store: scanning show file: %w", err)
		}
		sf.Present = present != 0
		out = append(out, sf)
	}
	return out, rows.Err()
}

// EpisodeTitleIDs returns every Episode Title under a Show keyed by identity_key.
// Apply needs the whole set, not just the ones it is moving: a Title emptied by a
// rearrangement still holds an identity_key, and that key may be the very one
// another Title is moving onto.
func (db *DB) EpisodeTitleIDs(showID string) (map[string]string, error) {
	rows, err := db.Query(
		`SELECT t.identity_key, t.id FROM titles t
		   JOIN seasons s ON s.id = t.season_id
		  WHERE s.show_id = ?`, showID)
	if err != nil {
		return nil, fmt.Errorf("store: listing episode titles: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var key, id string
		if err := rows.Scan(&key, &id); err != nil {
			return nil, fmt.Errorf("store: scanning episode title: %w", err)
		}
		out[key] = id
	}
	return out, rows.Err()
}

// TitleRekey re-points one existing Title row at the identity_key of the Slot it
// was assigned, IN PLACE. This is the whole reason Apply is not a delete-and-
// insert: title_id never moves, so every User's watch_state row survives, and the
// next scan replays the same decisions, computes the same key and finds the same
// row (ADR-0044).
type TitleRekey struct {
	TitleID     string
	IdentityKey string
}

// WatchFoldPart is one source Title contributing to a fold, in joint-timeline
// order, with the duration it contributes to that timeline.
type WatchFoldPart struct {
	TitleID    string
	DurationMs int64
}

// WatchFold folds several Titles' watch state onto the one Title that replaces
// them, addressed by the identity_key it will hold AFTER the re-keys and the tree
// write (its row id may not exist yet when the plan is built).
//
// One part is a COPY — the split case, where both resulting Titles inherit the
// original's state, which is what propagateToSiblingEpisodes already maintains
// between co-File siblings.
//
// Several parts is a MERGE onto the multi-part Edition's joint timeline. See
// foldWatchState for the rule; the short version is watched-only-if-EVERY-part-
// was, never watched-if-any.
type WatchFold struct {
	TargetKey string
	Parts     []WatchFoldPart
}

// ShowArrangement is everything Apply commits, in one transaction.
type ShowArrangement struct {
	ShowID    string
	LibraryID string
	// Decisions is the Show's new sparse decision set plus the scope it replaces.
	Decisions FileDecisionSet
	// Rekeys move existing Titles onto their assigned Slots' keys before Seasons is
	// written, so the tree write then finds and UPDATES those same rows.
	Rekeys []TitleRekey
	// Emptied are the Show's Episode Titles that no longer serve any Slot — the
	// other half of a merge, or a File the Admin unassigned. They are parked
	// alongside the movers so a re-key can pass through the key one of them still
	// holds; see applyRekeysTx for what becomes of them, and
	// reclaimEmptiedFilesTx for the one File row the tree write cannot take off
	// them by itself.
	Emptied []string
	// Seasons is the Show's target structure, built exactly as a scan would build
	// it. Only Seasons/Episodes are written: the Show row itself and all local
	// artwork are the Scanner's to own, and Apply must not disturb them.
	Seasons []SeasonTree
	// AbsentPaths are Files that belong to no Slot any more — the Admin's
	// Unassigned and Ignored decisions. They are soft-deleted (present = 0), never
	// removed, exactly as a scan's MarkFilesMissing pass would (ADR-0008); nothing
	// on disk is touched.
	AbsentPaths []string
	// Folds fold watch state onto merged and split Titles.
	Folds []WatchFold
	// PendingKeys are the identity_keys of Titles whose Slot changed: their
	// Enrichment is reset to 'pending' so the caller's re-enrich — which runs
	// AFTER this transaction — resolves the new position's record.
	PendingKeys []string
	// Pins repoint what DECORATES a Slot — the Episode pin (ADR-0044). They ride
	// in this transaction rather than in a write of their own because the Slots
	// this feature exists to repoint are ones the Admin has just placed files onto,
	// whose Titles do not exist until the tree write above has run: there is
	// nothing to pin at the moment of the gesture. It is also the only shape the
	// matcher's contract allows — nothing takes effect until Apply, and a pin
	// written eagerly would be the one change Revert could not undo.
	Pins []SlotPin
}

// SlotPin repoints one Slot's record, addressed by the identity_key the Slot will
// hold AFTER the re-keys and the tree write — the same late address WatchFold
// uses, and for the same reason: the row may not exist when the plan is built.
//
// It writes the ENRICHMENT columns only. identity_key, season_number,
// episode_number and every User's watch state are untouched (ADR-0014): the Slot
// keeps its position and its history and gains the right title, overview and
// still.
type SlotPin struct {
	IdentityKey string
	// SeriesID is the provider series the record is borrowed from. Empty leaves the
	// Title's current series alone, which is what a same-series pin means.
	SeriesID string
	// Season / Episode are the record's position IN THAT SERIES. Ignored when Clear.
	Season  int
	Episode int
	// Clear removes the pin, returning the Slot to its default record: this
	// series, this position. SeriesID is then the series to return TO (the Show's
	// own), which has to be written explicitly — leaving a borrowed series behind
	// with the numbers cleared would look up the Show's position in somebody else's
	// series.
	Clear bool
}

// ApplyShowArrangement commits one Show's rearrangement atomically.
//
// The order is load-bearing:
//
//  1. the decisions, so the record the Scanner replays and the rows Apply writes
//     can never disagree even if the process dies immediately after;
//  2. the re-keys, in two phases through a temporary key, because a swap between
//     two Slots would otherwise trip UNIQUE (library_id, identity_key) halfway
//     through;
//  3. the tree, through the SAME writeTitleRow / writeTitleSubtree the Scanner
//     uses, so the structural write itself has one implementation;
//  4. the reclaim of a File row an Emptied Title still holds for a path the tree
//     just wrote, then the soft-delete of Files that lost their Slot, then the
//     hidden recompute that empties a Season nobody claims any more;
//  5. the watch folds, last, because they address Titles by the key they only
//     hold once (2) and (3) have run.
func (db *DB) ApplyShowArrangement(a ShowArrangement) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin apply arrangement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := replaceFileDecisionsTx(tx, a.Decisions); err != nil {
		return err
	}
	if err := applyRekeysTx(tx, a.LibraryID, a.Rekeys, a.Emptied); err != nil {
		return err
	}

	// One shared "paths written this tx" set across all Episodes, so a File split
	// across two Slots keeps both File rows instead of the second reclaiming the
	// first — the same rule UpsertShowTree follows for a multi-episode file.
	written := map[string]bool{}
	// The rows the tree actually landed on. An Emptied Title can be one of them:
	// it keeps its key in phase three, and a Slot whose Files are all owned by
	// other Titles resolves onto it by that very key. Such a row is not empty at
	// all, so the reclaim below must leave it alone.
	served := map[string]bool{}
	for _, st := range a.Seasons {
		seasonID, err := upsertSeason(tx, a.ShowID, st)
		if err != nil {
			return err
		}
		for _, ep := range st.Episodes {
			titleID, err := upsertEpisodeTitle(tx, seasonID, ep, written)
			if err != nil {
				return err
			}
			served[titleID] = true
		}
	}
	if err := reclaimEmptiedFilesTx(tx, a.Emptied, written, served); err != nil {
		return err
	}

	if err := markPathsAbsentTx(tx, a.AbsentPaths); err != nil {
		return err
	}
	if err := recomputeHiddenTitlesTx(tx, a.LibraryID); err != nil {
		return err
	}
	if err := recomputeHiddenShowsTx(tx, a.LibraryID); err != nil {
		return err
	}

	for _, fold := range a.Folds {
		if err := foldWatchStateTx(tx, a.LibraryID, fold); err != nil {
			return err
		}
	}
	for _, key := range a.PendingKeys {
		if _, err := tx.Exec(
			`UPDATE titles SET enrichment_status = 'pending', `+clearEnrichmentRetry+`,
			     `+clearEnrichmentReason+`
			   WHERE library_id = ? AND identity_key = ?`, a.LibraryID, key); err != nil {
			return fmt.Errorf("store: resetting enrichment for %q: %w", key, err)
		}
	}
	// Last, because a pin addresses a Title by the key it only holds once the
	// re-keys and the tree write have run — and because the tree write rewrites
	// every Episode row it touches, so anything written before it would be undone.
	if err := applyPinsTx(tx, a.LibraryID, a.Pins); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit apply arrangement: %w", err)
	}
	return nil
}

// applyPinsTx writes the Episode pins of one Apply. A pin is an Enrichment
// override, so it lands in the enrichment columns and never in the identity ones
// the Scanner owns (ADR-0045) — which is what makes it survive the rescan the
// matcher triggers moments later.
//
// NULL is "not pinned — decorate from the Slot's own numbers", so clearing writes
// NULL rather than a number (migration 0047: season 0 is Specials, so 0 cannot
// mean unset). Both branches reset enrichment_status so the next pass resolves the
// record the Admin just chose; a pin whose Slot has no Title — a Placement onto a
// File the catalog has never probed — simply matches no row, and the file is
// already reported as Deferred.
func applyPinsTx(tx *sql.Tx, libraryID string, pins []SlotPin) error {
	for _, p := range pins {
		var err error
		if p.Clear {
			// Nothing is pinned any more, so the record stops being anybody's choice
			// (ADR-0045): the origin is released along with the numbers, whichever of
			// the two provenances put it there (ADR-0046).
			_, err = tx.Exec(
				`UPDATE titles SET enrichment_tmdb_id = ?, enrichment_id_origin = '',
				     enrichment_season = NULL,
				     enrichment_episode = NULL, enrichment_status = 'pending', `+clearEnrichmentRetry+`,
				     `+clearEnrichmentReason+`
				   WHERE library_id = ? AND identity_key = ?`,
				p.SeriesID, libraryID, p.IdentityKey)
		} else {
			_, err = tx.Exec(
				`UPDATE titles SET
				     enrichment_tmdb_id = CASE WHEN ? <> '' THEN ? ELSE enrichment_tmdb_id END,
				     enrichment_id_origin = CASE WHEN ? <> '' THEN 'chosen' ELSE enrichment_id_origin END,
				     enrichment_season = ?, enrichment_episode = ?,
				     enrichment_status = 'pending', `+clearEnrichmentRetry+`,
				     `+clearEnrichmentReason+`
				   WHERE library_id = ? AND identity_key = ?`,
				p.SeriesID, p.SeriesID, p.SeriesID, p.Season, p.Episode, libraryID, p.IdentityKey)
		}
		if err != nil {
			return fmt.Errorf("store: pinning the record for %q: %w", p.IdentityKey, err)
		}
	}
	return nil
}

// applyRekeysTx moves each Title onto its assigned Slot's identity_key in two
// phases through a temporary key.
//
// The temporary key is not defensive clutter: swapping two Files' Slots asks for
// A→B's key and B→A's key, and whichever runs first collides with the row that
// still holds the target. Parking every mover on a key nothing else can hold
// clears the constraint for the whole set at once.
//
// A Title left holding the temp key after phase two is one the arrangement
// emptied — its File went to another Slot, or the Admin unassigned it. It gets
// its ORIGINAL key back, so it stays exactly what a scan would leave behind: an
// Episode with no present Files, hidden by the recompute below and revived by the
// same key if the Admin ever undoes the change. It is deleted only when another
// Title has meanwhile taken that key, since two rows cannot share it — and in that
// case the row is both empty and superseded, so there is nothing left to keep.
func applyRekeysTx(tx *sql.Tx, libraryID string, rekeys []TitleRekey, emptied []string) error {
	if len(rekeys) == 0 && len(emptied) == 0 {
		return nil
	}
	// A parked Title with no target is an emptied one: it is moved out of the way
	// so a mover can take its key, and phase three decides its fate.
	all := append([]TitleRekey(nil), rekeys...)
	for _, id := range emptied {
		all = append(all, TitleRekey{TitleID: id})
	}
	type parked struct{ id, original, target string }
	var moving []parked
	for _, rk := range all {
		var original string
		if err := tx.QueryRow(`SELECT identity_key FROM titles WHERE id = ?`, rk.TitleID).
			Scan(&original); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("store: re-keying title %q: %w", rk.TitleID, ErrNotFound)
			}
			return fmt.Errorf("store: reading title %q: %w", rk.TitleID, err)
		}
		moving = append(moving, parked{id: rk.TitleID, original: original, target: rk.IdentityKey})
	}

	// Phase one: park every mover on a key nothing can collide with.
	for _, m := range moving {
		if _, err := tx.Exec(
			`UPDATE titles SET identity_key = ? WHERE id = ?`, tempIdentityKey(m.id), m.id,
		); err != nil {
			return fmt.Errorf("store: parking title %q: %w", m.id, err)
		}
	}
	// Phase two: the real keys, now that every target is free.
	for _, m := range moving {
		if m.target == "" {
			continue
		}
		if _, err := tx.Exec(
			`UPDATE titles SET identity_key = ? WHERE id = ?`, m.target, m.id,
		); err != nil {
			return fmt.Errorf("store: re-keying title %q: %w", m.id, err)
		}
	}
	// Phase three: anything still parked is an emptied Title. Restore its key, or
	// drop it when that key now belongs to someone else.
	for _, m := range moving {
		if m.target != "" {
			continue
		}
		var holder string
		err := tx.QueryRow(
			`SELECT id FROM titles WHERE library_id = ? AND identity_key = ?`,
			libraryID, m.original).Scan(&holder)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.Exec(
				`UPDATE titles SET identity_key = ? WHERE id = ?`, m.original, m.id,
			); err != nil {
				return fmt.Errorf("store: restoring title %q: %w", m.id, err)
			}
		case err != nil:
			return fmt.Errorf("store: resolving key %q: %w", m.original, err)
		default:
			if _, err := tx.Exec(`DELETE FROM titles WHERE id = ?`, m.id); err != nil {
				return fmt.Errorf("store: dropping superseded title %q: %w", m.id, err)
			}
		}
	}
	return nil
}

// tempIdentityKey is the parking key one Title wears mid-transaction. It embeds
// the row id so two parked Titles never collide with each other, and carries a
// prefix no derived key can produce (every real key starts with a Show's own key)
// so it can never be mistaken for a real one if a transaction is ever inspected.
func tempIdentityKey(titleID string) string { return "\x00apply-rekey:" + titleID }

// reclaimEmptiedFilesTx drops the File rows an Emptied Title still holds for a
// path the tree write has just claimed for somebody else.
//
// It exists because a SHARED path defeats the tree write's own reclaim. That
// reclaim — writeTitleSubtree's "the prior row for this path belongs to another
// Title, take it" — is deliberately skipped for a path a co-File sibling already
// wrote in the SAME transaction, and skipping it is what keeps a legitimate split
// (S01E05-E06: two Titles, one file) from having its two rows steal each other,
// which crashed on `UNIQUE constraint failed: files.id` (multiepisode_test.go).
// So when an Admin takes one Slot of a split away, the parked Title keeps its row
// for the still-present shared file, recomputeHiddenTitlesTx sees a present File,
// and the Episode the Admin removed stays in browse forever — no rescan repairs
// it, because the stale Title is in no tree the Scanner builds and the soft-delete
// pass only marks Files gone from DISK (file-matcher/08).
//
// Two boundaries make this safe:
//
//   - only paths in `written`. A Missing File — one the Admin took off its Slot,
//     soft-deleted rather than removed (ADR-0008) — is in no tree, so its row
//     survives untouched and ShowFiles/placementInputs can still offer it back for
//     re-placing. Clearing an Emptied Title's Editions wholesale, the obvious fix,
//     would destroy exactly those rows.
//   - only Titles the tree did NOT write (`served`). An Emptied Title that the
//     tree resolved back onto by key owns those rows legitimately — it is serving
//     that Slot after all.
//
// Anything the tree write's ordinary cross-Title reclaim already took has moved to
// another Edition by now and is not matched here, so a merge still keeps its
// File's id and added_at.
func reclaimEmptiedFilesTx(tx *sql.Tx, emptied []string, written, served map[string]bool) error {
	if len(emptied) == 0 || len(written) == 0 {
		return nil
	}
	// Chunked, because the whole Show's paths are bound as parameters and a Show
	// really can have more files than SQLite will bind in one statement.
	const chunk = 500
	paths := sortedKeys(written)
	for _, titleID := range emptied {
		if served[titleID] {
			continue
		}
		var taken int64
		for start := 0; start < len(paths); start += chunk {
			batch := paths[start:min(start+chunk, len(paths))]
			args := make([]any, 0, len(batch)+1)
			args = append(args, titleID)
			for _, p := range batch {
				args = append(args, p)
			}
			res, err := tx.Exec(`DELETE FROM files
			     WHERE edition_id IN (SELECT id FROM editions WHERE title_id = ?)
			       AND path IN (?`+strings.Repeat(", ?", len(batch)-1)+`)`, args...)
			if err != nil {
				return fmt.Errorf("store: reclaiming the files of emptied title %q: %w", titleID, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("store: reclaiming the files of emptied title %q: %w", titleID, err)
			}
			taken += n
		}
		if taken == 0 {
			continue
		}
		// An Edition the reclaim emptied is not a shape anything reads: the Title
		// keeps its key as the empty, hidden, revivable row a scan would leave, and
		// a later re-placement rebuilds its Editions from scratch anyway.
		if _, err := tx.Exec(
			`DELETE FROM editions WHERE title_id = ?
			   AND NOT EXISTS (SELECT 1 FROM files f WHERE f.edition_id = editions.id)`,
			titleID); err != nil {
			return fmt.Errorf("store: dropping the emptied editions of %q: %w", titleID, err)
		}
	}
	return nil
}

// markPathsAbsentTx soft-deletes the Files that lost their Slot. It marks the row
// Missing exactly as a scan's soft-delete pass would; nothing on disk is touched
// and no row is removed, so re-placing the File later reuses the same row
// (ADR-0008).
func markPathsAbsentTx(tx *sql.Tx, paths []string) error {
	for _, p := range paths {
		if _, err := tx.Exec(`UPDATE files SET present = 0 WHERE path = ?`, p); err != nil {
			return fmt.Errorf("store: soft-deleting %q: %w", p, err)
		}
	}
	return nil
}

// foldWatchStateTx folds every User's watch state from a fold's source Titles onto
// the Title now holding TargetKey.
//
// The rule (ADR-0044, CONTEXT.md "Watched threshold"):
//
//   - WATCHED only if EVERY part was watched. Watched-if-any is precisely the bug
//     the multi-part duration work exists to prevent: finishing part 1 of a
//     two-part Episode would mark the whole Episode watched at its halfway point,
//     clear the resume, and move the Up Next anchor with it (ADR-0028).
//   - Otherwise RESUME at the earliest unfinished part, mapped onto the combined
//     timeline: the running start of that part plus its own resume position.
//     Watching part 1 to the end and stopping 32% into part 2 resumes at
//     part1.duration + 0.32×part2.duration, which is where the viewer actually
//     stopped once the two are one Edition.
//   - played_at (and, when a personal rating lands on watch_state, the rating)
//     takes the MOST RECENT of the parts: the recency signal Up Next anchors on is
//     about when the viewer last played something, and merging two files does not
//     un-play either of them.
//
// Nothing is reset. A single-part fold degenerates to a straight copy, which is
// the split case.
func foldWatchStateTx(tx *sql.Tx, libraryID string, fold WatchFold) error {
	if len(fold.Parts) == 0 {
		return nil
	}
	var targetID string
	err := tx.QueryRow(
		`SELECT id FROM titles WHERE library_id = ? AND identity_key = ?`,
		libraryID, fold.TargetKey).Scan(&targetID)
	if errors.Is(err, sql.ErrNoRows) {
		// The Episode was deferred (a placed File the catalog has never probed), so
		// there is no row to fold onto. The decision is stored either way and the
		// next scan builds the Episode; folding then has nothing to fold, because
		// the sources still hold their own state.
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: resolving fold target %q: %w", fold.TargetKey, err)
	}

	type state struct {
		resume   int64
		watched  bool
		playedAt sql.NullString
		updated  string
	}
	// Per User, the state of each part by index. A part with no row is "not
	// started", which is emphatically NOT watched — that is what makes a fold over
	// a never-played part refuse to call the merged Episode finished.
	perUser := map[string]map[int]state{}
	for i, part := range fold.Parts {
		rows, err := tx.Query(
			`SELECT user_id, resume_position_ms, watched, played_at, updated_at
			   FROM watch_state WHERE title_id = ?`, part.TitleID)
		if err != nil {
			return fmt.Errorf("store: reading watch state to fold: %w", err)
		}
		for rows.Next() {
			var userID string
			var st state
			var watched int
			if err := rows.Scan(&userID, &st.resume, &watched, &st.playedAt, &st.updated); err != nil {
				_ = rows.Close()
				return fmt.Errorf("store: scanning watch state to fold: %w", err)
			}
			st.watched = watched != 0
			if perUser[userID] == nil {
				perUser[userID] = map[int]state{}
			}
			perUser[userID][i] = st
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}

	for _, userID := range sortedKeys(perUser) {
		parts := perUser[userID]
		watchedAll := true
		var resume, start int64
		resumeSet := false
		var playedAt sql.NullString
		var updated string
		for i := range fold.Parts {
			st, ok := parts[i]
			if !ok || !st.watched {
				watchedAll = false
				if !resumeSet {
					// The earliest unfinished part, on the combined timeline.
					resume = start + st.resume
					resumeSet = true
				}
			}
			if ok {
				if st.playedAt.Valid && (!playedAt.Valid || st.playedAt.String > playedAt.String) {
					playedAt = st.playedAt
				}
				if st.updated > updated {
					updated = st.updated
				}
			}
			start += fold.Parts[i].DurationMs
		}
		if watchedAll {
			// A row with watched = 1 always has resume 0: crossing the ceiling clears
			// the resume (migration 0007).
			resume = 0
		}
		if _, err := tx.Exec(
			`INSERT INTO watch_state (id, user_id, title_id, resume_position_ms, watched, played_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT (user_id, title_id) DO UPDATE SET
			   resume_position_ms = excluded.resume_position_ms,
			   watched            = excluded.watched,
			   played_at          = excluded.played_at,
			   updated_at         = excluded.updated_at`,
			uuid.NewString(), userID, targetID, resume, boolToInt(watchedAll), playedAt, updated,
		); err != nil {
			return fmt.Errorf("store: folding watch state: %w", err)
		}
	}
	return nil
}

// sortedKeys orders a map's keys so a fold writes its rows in a deterministic
// order (the set is one Show's Users, so this is never large).
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// replaceFileDecisionsTx is ReplaceFileDecisions' body, taking a transaction so
// Apply can commit the decisions and the rows they imply together. Neither half
// is safe alone: decisions without rows is a Show that only rearranges on the next
// scan, and rows without decisions is a rearrangement the next scan undoes.
func replaceFileDecisionsTx(tx *sql.Tx, set FileDecisionSet) error {
	if set.LibraryID == "" {
		return fmt.Errorf("store: replacing file decisions: library id required")
	}
	scope := map[string]bool{}
	for _, p := range set.Paths {
		scope[p] = true
	}
	if len(set.Paths) == 0 {
		for _, d := range set.Decisions {
			scope[d.Path] = true
		}
	}
	// A row written outside its own replace scope could never be cleared by a
	// later Apply of the same screen, which is a caller bug and a silent one —
	// refuse it here rather than leaving an unreachable correction behind.
	for _, d := range set.Decisions {
		if !scope[d.Path] {
			return fmt.Errorf("store: replacing file decisions: path %q is outside the replace scope", d.Path)
		}
	}

	paths := make([]string, 0, len(scope))
	for path := range scope {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if _, err := tx.Exec(
			`DELETE FROM file_decisions WHERE library_id = ? AND path = ?`,
			set.LibraryID, path); err != nil {
			return fmt.Errorf("store: clearing file decisions for %q: %w", path, err)
		}
	}

	for _, d := range set.Decisions {
		if d.ID == "" {
			d.ID = uuid.NewString()
		}
		// Ordinal is 1-based and only meaningful when Files share a Slot, so an
		// unset value means the ordinary single-File case rather than an error.
		if d.Ordinal < 1 {
			d.Ordinal = 1
		}
		// The two settled states name no Slot, and the schema refuses to store
		// half a Placement for them (a stray group/slot a later reader would take
		// for real). Pass NULL rather than silently keeping whatever the caller
		// left in the struct.
		var group, slot any
		if d.State == DecisionPlaced {
			group, slot = d.GroupNumber, d.SlotNumber
		}
		// A freshly asserted decision is never orphaned: the Admin just chose a
		// file that exists. Only a scan that fails to find the path sets the flag.
		if _, err := tx.Exec(
			`INSERT INTO file_decisions
			   (id, library_id, path, state, group_number, slot_number, ordinal, orphaned)
			 VALUES (?, ?, ?, ?, ?, ?, ?, 0)`,
			d.ID, set.LibraryID, d.Path, d.State, group, slot, d.Ordinal,
		); err != nil {
			return fmt.Errorf("store: inserting %s decision for %q: %w", d.State, d.Path, err)
		}
	}
	return nil
}

// recomputeHiddenTitlesTx / recomputeHiddenShowsTx are the transaction-scoped
// bodies of the two public recomputes, so Apply's soft-delete and the visibility
// it implies land in the same commit. A Season emptied by a reassignment
// disappears here, and one conjured by an assignment appears, with no folder on
// disk either way (ADR-0044).
func recomputeHiddenTitlesTx(tx *sql.Tx, libraryID string) error {
	if _, err := tx.Exec(
		`UPDATE titles SET hidden = CASE
		     WHEN (SELECT COUNT(*) FROM editions e JOIN files f ON f.edition_id = e.id
		             WHERE e.title_id = titles.id AND f.present = 1) > 0
		     THEN 0 ELSE 1 END
		   WHERE library_id = ?`, libraryID); err != nil {
		return fmt.Errorf("store: recomputing hidden titles: %w", err)
	}
	return nil
}

func recomputeHiddenShowsTx(tx *sql.Tx, libraryID string) error {
	if _, err := tx.Exec(
		`UPDATE seasons SET hidden = CASE
		     WHEN (SELECT COUNT(*) FROM titles t WHERE t.season_id = seasons.id AND t.hidden = 0) > 0
		     THEN 0 ELSE 1 END
		   WHERE show_id IN (SELECT id FROM shows WHERE library_id = ?)`, libraryID); err != nil {
		return fmt.Errorf("store: recomputing hidden seasons: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE shows SET hidden = CASE
		     WHEN (SELECT COUNT(*) FROM seasons s WHERE s.show_id = shows.id AND s.hidden = 0) > 0
		     THEN 0 ELSE 1 END
		   WHERE library_id = ?`, libraryID); err != nil {
		return fmt.Errorf("store: recomputing hidden shows: %w", err)
	}
	return nil
}

// IsTempIdentityKey reports whether a key is one of applyRekeysTx's parking keys.
// Nothing in production reads it; it exists so a test can assert that no such key
// ever survives a commit.
func IsTempIdentityKey(key string) bool { return strings.HasPrefix(key, "\x00apply-rekey:") }

// EpisodeSlot is one Episode Title of a Show as the matcher sees it: the Slot it
// occupies, the row that serves it, and — when the Admin repointed it — the
// provider record that decorates it.
//
// The pin is carried because a Slot has two independent halves (ADR-0044): its
// POSITION, always the local library's own numbering, and its RECORD. A matcher
// that showed only positions could not tell an Admin that S03E62 is decorated
// from a DIFFERENT series, which is the whole point of the Batman → New Batman
// Adventures case the pin exists for.
type EpisodeSlot struct {
	TitleID       string
	SeasonNumber  int
	EpisodeNumber int
	// RecordSeries is the provider series this Episode's record resolves against
	// (the record id: enrichment_tmdb_id, else tmdb_id — ADR-0045). It is the
	// Show's own series unless an Enrichment override moved it, and empty for an
	// Episode with no record of its own. A value here is NOT evidence that anyone
	// chose it — enrichment_id_origin is that, and this row does not carry it.
	RecordSeries string
	// RecordSeason / RecordEpisode are the pinned provider position, both
	// NoEpisodePin when the Slot is decorated from its own numbers.
	RecordSeason  int
	RecordEpisode int
}

// ShowEpisodeSlots lists every Episode Title of a Show — hidden ones included —
// with its Slot and its Episode pin, in (season, episode) order.
//
// Hidden rows are deliberately in: a Title emptied by a rearrangement keeps its
// key and its Slot, and the matcher is exactly the screen on which an Admin puts
// a File back on it. Excluding them would make a Slot vanish the moment it went
// empty, which is true of browse (CONTEXT.md "Slot": invisible there, present
// here) and false of the matcher.
func (db *DB) ShowEpisodeSlots(showID string) ([]EpisodeSlot, error) {
	rows, err := db.Query(
		`SELECT t.id, t.season_number, t.episode_number, `+recordTMDBID("t.")+`,
		        t.enrichment_season, t.enrichment_episode
		   FROM titles t
		   JOIN seasons s ON s.id = t.season_id
		  WHERE s.show_id = ?
		  ORDER BY t.season_number, t.episode_number, t.identity_key`, showID)
	if err != nil {
		return nil, fmt.Errorf("store: listing show episode slots: %w", err)
	}
	defer rows.Close()
	var out []EpisodeSlot
	for rows.Next() {
		var es EpisodeSlot
		var pinSeason, pinEpisode sql.NullInt64
		if err := rows.Scan(&es.TitleID, &es.SeasonNumber, &es.EpisodeNumber,
			&es.RecordSeries, &pinSeason, &pinEpisode); err != nil {
			return nil, fmt.Errorf("store: scanning show episode slot: %w", err)
		}
		// NULL is the ordinary case — decorate from the Slot's own numbers — and -1
		// is its in-memory spelling, because season 0 (Specials) is a real value.
		es.RecordSeason, es.RecordEpisode = NoEpisodePin, NoEpisodePin
		if pinSeason.Valid && pinEpisode.Valid {
			es.RecordSeason, es.RecordEpisode = int(pinSeason.Int64), int(pinEpisode.Int64)
		}
		out = append(out, es)
	}
	return out, rows.Err()
}
