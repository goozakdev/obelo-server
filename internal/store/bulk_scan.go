package store

import (
	"fmt"
	"sort"
)

// bulk_scan.go holds the set-at-a-time readers (and one writer) the Scanner uses so
// a scan costs a handful of queries rather than a few per File. Each answers what
// a per-path call elsewhere in this package answers, for a whole Library at once.

// StoredFilesByLibrary returns every stored File of the Library (with its Streams),
// keyed by path — LoadStoredFile for all of them in two queries. An incremental scan
// reads it once so an unchanged File reuses its prior ffprobe result without a
// round trip of its own.
func (db *DB) StoredFilesByLibrary(libraryID string) (map[string]File, error) {
	rows, err := db.Query(
		`SELECT f.id, f.edition_id, f.path, f.container, f.video_codec, f.audio_codec,
		        f.width, f.height, f.bitrate, f.duration_ms, f.size_bytes, f.added_at,
		        f.mtime, f.present, f.part_ordinal
		   FROM files f
		   JOIN editions e ON f.edition_id = e.id
		   JOIN titles   t ON e.title_id   = t.id
		  WHERE t.library_id = ?`, libraryID)
	if err != nil {
		return nil, fmt.Errorf("store: listing stored files: %w", err)
	}
	defer rows.Close()

	out := map[string]File{}
	byID := map[string]string{} // file id → path
	for rows.Next() {
		var f File
		var present int
		if err := rows.Scan(&f.ID, &f.EditionID, &f.Path, &f.Container, &f.VideoCodec,
			&f.AudioCodec, &f.Width, &f.Height, &f.Bitrate, &f.DurationMs, &f.SizeBytes,
			&f.AddedAt, &f.Mtime, &present, &f.PartOrdinal); err != nil {
			return nil, fmt.Errorf("store: scanning stored file: %w", err)
		}
		f.Present = present != 0
		out[f.Path] = f
		byID[f.ID] = f.Path
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	srows, err := db.Query(
		`SELECT s.id, s.file_id, s.stream_index, s.kind, s.codec, s.language, s.width,
		        s.height, s.channels, s.is_default, s.forced, s.title, s.commentary,
		        s.hearing_impaired
		   FROM streams s
		   JOIN files    f ON s.file_id    = f.id
		   JOIN editions e ON f.edition_id = e.id
		   JOIN titles   t ON e.title_id   = t.id
		  WHERE t.library_id = ?
		  ORDER BY s.file_id, s.stream_index`, libraryID)
	if err != nil {
		return nil, fmt.Errorf("store: listing streams: %w", err)
	}
	defer srows.Close()
	for srows.Next() {
		var s Stream
		var isDefault, forced, commentary, hearingImpaired int
		if err := srows.Scan(&s.ID, &s.FileID, &s.Index, &s.Kind, &s.Codec, &s.Language,
			&s.Width, &s.Height, &s.Channels, &isDefault, &forced,
			&s.Title, &commentary, &hearingImpaired); err != nil {
			return nil, fmt.Errorf("store: scanning stream: %w", err)
		}
		s.IsDefault = isDefault != 0
		s.Forced = forced != 0
		s.Commentary = commentary != 0
		s.HearingImpaired = hearingImpaired != 0
		path, ok := byID[s.FileID]
		if !ok {
			continue
		}
		f := out[path]
		f.Streams = append(f.Streams, s)
		out[path] = f
	}
	return out, srows.Err()
}

// LocalMarkerState is what the Scanner stored as the Local Markers of one File:
// the spans, and whether they were read from its `.edl`.
type LocalMarkerState struct {
	FromEDL bool
	Markers []Marker
}

// LocalMarkersByLibrary returns the Local Markers stored for every File of the
// Library, keyed by path — what LocalMarkersFromEDL (and a comparison with the
// stored rows) would answer for each File, in one query. A File with none is
// absent. Markers are in (start, kind, end) order.
func (db *DB) LocalMarkersByLibrary(libraryID string) (map[string]LocalMarkerState, error) {
	rows, err := db.Query(
		`SELECT m.file_path, m.kind, m.start_ms, m.end_ms, m.from_edl
		   FROM markers m
		  WHERE m.source = 'local'
		    AND m.file_path IN (
		        SELECT f.path FROM files f
		          JOIN editions e ON f.edition_id = e.id
		          JOIN titles   t ON e.title_id   = t.id
		         WHERE t.library_id = ?)
		  ORDER BY m.file_path, m.start_ms, m.kind, m.end_ms`, libraryID)
	if err != nil {
		return nil, fmt.Errorf("store: listing local markers: %w", err)
	}
	defer rows.Close()

	out := map[string]LocalMarkerState{}
	for rows.Next() {
		var path string
		var m Marker
		var fromEDL bool
		if err := rows.Scan(&path, &m.Kind, &m.StartMs, &m.EndMs, &fromEDL); err != nil {
			return nil, fmt.Errorf("store: scanning local marker: %w", err)
		}
		m.Source = "local"
		st := out[path]
		st.FromEDL = st.FromEDL || fromEDL
		st.Markers = append(st.Markers, m)
		out[path] = st
	}
	return out, rows.Err()
}

// SameMarkerSpans reports whether a and b name the same spans (kind, start, end),
// whatever their order or Source.
func SameMarkerSpans(a, b []Marker) bool {
	if len(a) != len(b) {
		return false
	}
	key := func(ms []Marker) []Marker {
		out := append([]Marker(nil), ms...)
		sort.Slice(out, func(i, j int) bool {
			if out[i].StartMs != out[j].StartMs {
				return out[i].StartMs < out[j].StartMs
			}
			if out[i].Kind != out[j].Kind {
				return out[i].Kind < out[j].Kind
			}
			return out[i].EndMs < out[j].EndMs
		})
		return out
	}
	ka, kb := key(a), key(b)
	for i := range ka {
		if ka[i].Kind != kb[i].Kind || ka[i].StartMs != kb[i].StartMs || ka[i].EndMs != kb[i].EndMs {
			return false
		}
	}
	return true
}

// AddUnmatched records files in the Library's Unmatched list WITHOUT clearing the
// rest of it (ReplaceUnmatched, a full scan's whole-list rewrite, does). A path
// already listed has its kind and reason refreshed in place. A Targeted scan uses
// it to keep what it newly found unreadable, which it has no business rewriting
// the rest of the list to say.
func (db *DB) AddUnmatched(libraryID string, files []UnmatchedFile) error {
	if len(files) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin add unmatched: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, f := range files {
		if _, err := tx.Exec(
			`INSERT INTO unmatched_files (id, library_id, path, reason, kind) VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT(path) DO UPDATE SET library_id = excluded.library_id,
			     reason = excluded.reason, kind = excluded.kind`,
			f.ID, libraryID, f.Path, f.Reason, f.KindOrDefault(),
		); err != nil {
			return fmt.Errorf("store: adding unmatched %q: %w", f.Path, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit add unmatched: %w", err)
	}
	return nil
}

// ShowFilesByLibrary is ShowFiles for every Show of the Library at once, keyed by
// Show id, each list in ShowFiles' own order (path, then identity key). The
// Needs-Fixing queue reads it once instead of once per Show.
func (db *DB) ShowFilesByLibrary(libraryID string) (map[string][]ShowFile, error) {
	rows, err := db.Query(
		`SELECT s.show_id, f.path, f.present, f.duration_ms, t.id, t.identity_key,
		        t.season_number, t.episode_number
		   FROM files f
		   JOIN editions e ON e.id = f.edition_id
		   JOIN titles   t ON t.id = e.title_id
		   JOIN seasons  s ON s.id = t.season_id
		   JOIN shows   sh ON sh.id = s.show_id
		  WHERE sh.library_id = ?
		  ORDER BY s.show_id, f.path, t.identity_key`, libraryID)
	if err != nil {
		return nil, fmt.Errorf("store: listing show files by library: %w", err)
	}
	defer rows.Close()
	out := map[string][]ShowFile{}
	for rows.Next() {
		var showID string
		var sf ShowFile
		var present int
		if err := rows.Scan(&showID, &sf.Path, &present, &sf.DurationMs, &sf.TitleID,
			&sf.IdentityKey, &sf.SeasonNumber, &sf.EpisodeNumber); err != nil {
			return nil, fmt.Errorf("store: scanning show file: %w", err)
		}
		sf.Present = present != 0
		out[showID] = append(out[showID], sf)
	}
	return out, rows.Err()
}
