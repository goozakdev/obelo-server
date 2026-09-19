package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// A Title's RECORD ids live in title_external_ids, one row per (Title, namespace),
// and titles.enrichment_id_namespace names the row that is the Title's record — the
// one a pin is keyed on (ADR-0060 decision 2, migration 0072). A parent keeps one
// id in entity_enrichment.external_id with its namespace beside it in
// external_id_namespace (decision 3).
//
// A namespace is where an id means something: `tmdb`, `imdb`, `musicbrainz`,
// `thetvdb`, `anidb`, or a third-party source's own (its plugin id). The three the
// host has always kept ids for are named here because the derived read conveniences
// (Title.TMDBID / IMDBID / MusicbrainzID) and ExternalMatch's named fields are
// spelled in them.
//
// IDENTITY IS NOT HERE. titles.tmdb_id / imdb_id are the Scanner's identity columns
// (ADR-0002); a record row in the same namespace outranks them on every read
// (ADR-0045), which is recordIDExpr's COALESCE.
const (
	NamespaceTMDB        = "tmdb"
	NamespaceIMDB        = "imdb"
	NamespaceMusicBrainz = "musicbrainz"
)

// identityColumnFor names the Scanner-owned identity column that also carries an id
// in namespace ns, or "" when no identity column does. ADR-0045's fill-only rule
// ranks a folder's id above a pass's echo, so the fill has to consult it.
func identityColumnFor(ns string) string {
	switch ns {
	case NamespaceTMDB:
		return "tmdb_id"
	case NamespaceIMDB:
		return "imdb_id"
	}
	return ""
}

// titleIDRef is the SQL spelling of the outer Title's id for a correlated subquery:
// alias is a table alias with its dot ("t.") or "" for an unaliased `FROM titles`.
func titleIDRef(alias string) string {
	if alias == "" {
		return "titles.id"
	}
	return alias + "id"
}

// recordRowExpr selects the Title's record row in namespace ns alone — empty when it
// has none. ns is always a fixed literal from this package, never input.
func recordRowExpr(alias, ns string) string {
	return "IFNULL((SELECT xid.external_id FROM title_external_ids xid" +
		" WHERE xid.title_id = " + titleIDRef(alias) + " AND xid.namespace = '" + ns + "'), '')"
}

// recordIDExpr is "the record-or-identity id in namespace ns": the record row when
// there is one, else the identity column the Scanner filled from the folder's name
// (ADR-0045). For a namespace with no identity column it is the record row alone.
func recordIDExpr(alias, ns string) string {
	row := recordRowExpr(alias, ns)
	col := identityColumnFor(ns)
	if col == "" {
		return row
	}
	return "COALESCE(NULLIF(" + row + ", ''), " + alias + col + ")"
}

// recordIDsColumns is the SELECT pair (enrichment_id_namespace, every record row as
// a JSON object) that decodeRecordIDs turns into Title.RecordNamespace and
// Title.RecordIDs. Carried by the reads that feed enrichment.
func recordIDsColumns(alias string) string {
	return alias + "enrichment_id_namespace, " +
		"(SELECT json_group_object(xid.namespace, xid.external_id) FROM title_external_ids xid" +
		" WHERE xid.title_id = " + titleIDRef(alias) + ")"
}

// decodeRecordIDs turns recordIDsColumns' JSON object into the map a Title carries;
// nil when the Title has no record rows.
func decodeRecordIDs(raw sql.NullString) (map[string]string, error) {
	if !raw.Valid || raw.String == "" || raw.String == "{}" {
		return nil, nil
	}
	out := map[string]string{}
	if err := json.Unmarshal([]byte(raw.String), &out); err != nil {
		return nil, fmt.Errorf("store: decoding record ids: %w", err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// RecordID returns this Title's record id in namespace ns — the ROW only, never the
// identity id — or "" when it holds none, or when the read that built the Title did
// not carry the rows (only the enrichment-feeding reads do).
func (t Title) RecordID(ns string) string { return t.RecordIDs[ns] }

// namespacedID is one (namespace, id) pair an ExternalMatch asks to write.
type namespacedID struct{ ns, id string }

// ids lists the non-empty ids m names, in RECORD order: the explicit Namespace/ID
// pair first, then the named fields in the order the old reads resolved a record
// (the TMDB column first, ADR-0045). A namespace named twice keeps its first id,
// so the explicit pair wins over a named field that disagrees with it.
func (m ExternalMatch) ids() []namespacedID {
	var out []namespacedID
	seen := map[string]bool{}
	add := func(ns, id string) {
		if ns == "" || id == "" || seen[ns] {
			return
		}
		seen[ns] = true
		out = append(out, namespacedID{ns, id})
	}
	add(m.Namespace, m.ID)
	add(NamespaceTMDB, m.TMDBID)
	add(NamespaceMusicBrainz, m.MusicbrainzID)
	add(NamespaceIMDB, m.IMDBID)
	return out
}

// execer is what a record-row write needs: a *sql.Tx or the DB itself.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// putRecordID writes (or replaces) a Title's record row in namespace ns. It does
// NOT touch enrichment_id_namespace; the caller decides whether this row is the
// record. An unchanged id is not rewritten, so it does not bump the Title's
// updated_at (the Linked-Library export's delta key).
func putRecordID(q execer, titleID, ns, id string) error {
	if _, err := q.Exec(
		`INSERT INTO title_external_ids (title_id, namespace, external_id) VALUES (?, ?, ?)
		 ON CONFLICT(title_id, namespace) DO UPDATE SET external_id = excluded.external_id
		   WHERE external_id <> excluded.external_id`,
		titleID, ns, id,
	); err != nil {
		return fmt.Errorf("store: writing %s record id: %w", ns, err)
	}
	return nil
}

// deleteRecordID removes a Title's record row in namespace ns, and blanks the
// record namespace when that row was the record.
func deleteRecordID(q execer, titleID, ns string) error {
	if _, err := q.Exec(
		`DELETE FROM title_external_ids WHERE title_id = ? AND namespace = ?`, titleID, ns,
	); err != nil {
		return fmt.Errorf("store: deleting %s record id: %w", ns, err)
	}
	if _, err := q.Exec(
		`UPDATE titles SET enrichment_id_namespace = '' WHERE id = ? AND enrichment_id_namespace = ?`,
		titleID, ns,
	); err != nil {
		return fmt.Errorf("store: clearing record namespace: %w", err)
	}
	return nil
}

// clearRecordIDs deletes every record row a Title holds and blanks its record
// namespace — the one spelling of "this Title no longer has a record", used by
// every path that blanked the old record columns.
func clearRecordIDs(q execer, titleID string) error {
	if _, err := q.Exec(`DELETE FROM title_external_ids WHERE title_id = ?`, titleID); err != nil {
		return fmt.Errorf("store: clearing record ids: %w", err)
	}
	if _, err := q.Exec(
		`UPDATE titles SET enrichment_id_namespace = '' WHERE id = ? AND enrichment_id_namespace <> ''`,
		titleID,
	); err != nil {
		return fmt.Errorf("store: clearing record namespace: %w", err)
	}
	return nil
}

// setRecordNamespace names ns as the Title's record.
func setRecordNamespace(q execer, titleID, ns string) error {
	if _, err := q.Exec(
		`UPDATE titles SET enrichment_id_namespace = ? WHERE id = ? AND enrichment_id_namespace <> ?`,
		ns, titleID, ns,
	); err != nil {
		return fmt.Errorf("store: setting record namespace: %w", err)
	}
	return nil
}

// defaultEntityNamespace is the namespace a parent's id is read in when nothing
// says otherwise: the kind's default lead (ADR-0060 decision 4).
func defaultEntityNamespace(entityType string) string {
	switch entityType {
	case EntityShow, EntitySeason:
		return NamespaceTMDB
	case EntityArtist, EntityAlbum:
		return NamespaceMusicBrainz
	}
	return ""
}

// authoritativeNamespaces are the sources whose name, as a pass's
// enrichment_source, is the namespace of the id written in the same statement —
// the Authoritative leads (ADR-0060 decision 4).
var authoritativeNamespaces = map[string]bool{
	"tmdb": true, "anidb": true, "thetvdb": true, "musicbrainz": true,
}

// entityNamespace resolves the namespace a parent's id is written under: the
// caller's when it names one, else the source that resolved it when that is an
// Authoritative namespace, else the kind's default lead. No id, no namespace.
func entityNamespace(entityType, explicit, source, externalID string) string {
	switch {
	case externalID == "":
		return ""
	case explicit != "":
		return explicit
	case authoritativeNamespaces[source]:
		return source
	}
	return defaultEntityNamespace(entityType)
}
