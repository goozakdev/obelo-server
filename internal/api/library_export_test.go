package api_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Black-box tests for the Library Export (.scratch/linked-servers issue 05,
// ADR-0056 §4): the flat, incremental, tombstoned feed a linked Server's mirror
// pulls, and the single place this Server decides what another household may
// learn about a Title.

// --- wire shapes -------------------------------------------------------------

type exportEntityResp struct {
	Type      string         `json:"type"`
	ID        string         `json:"id"`
	ParentID  string         `json:"parentId"`
	UpdatedAt string         `json:"updatedAt"`
	DeletedAt string         `json:"deletedAt"`
	Data      map[string]any `json:"data"`
}

type exportResp struct {
	LinkProtocolVersion int `json:"linkProtocolVersion"`
	Library             struct {
		ID   string `json:"id"`
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"library"`
	Entities   []exportEntityResp `json:"entities"`
	NextCursor string             `json:"nextCursor"`
	Checkpoint string             `json:"checkpoint"`
}

// --- helpers -----------------------------------------------------------------

// linkedServerToken creates a `remote` User granted libIDs and returns a Device
// token for it — the credential a redeemed Invite leaves behind (issue 03).
func linkedServerToken(t *testing.T, srv *testharness.Server, adminTok string, libIDs ...string) string {
	t.Helper()
	// A distinct label (and clientId) per call: a test may stand up two linked
	// Servers against one sharer, and the username is unique.
	n := atomic.AddInt64(&linkedServerSeq, 1)
	id := createRemoteUser(t, srv, adminTok, fmt.Sprintf("Brandon's server %d", n))
	if len(libIDs) > 0 {
		grantLibraries(t, srv, adminTok, id, libIDs...)
	}
	return srv.IssueTokenForUser(id, fmt.Sprintf("peer-server-%d", n))
}

var linkedServerSeq int64

// exportPage fetches one page of the feed, asserting the status.
func exportPage(t *testing.T, srv *testharness.Server, token, libID, query string, wantStatus int) (exportResp, []byte) {
	t.Helper()
	path := "/api/v1/libraries/" + libID + "/export"
	if query != "" {
		path += "?" + query
	}
	var out exportResp
	status, body := srv.AuthGET(path, token, &out)
	if status != wantStatus {
		t.Fatalf("GET %s = %d, want %d; body: %s", path, status, wantStatus, body)
	}
	return out, body
}

// fullExport walks every page of the feed from `since` (empty = a full pull) and
// returns the entities in feed order plus the final checkpoint. It asserts the
// walk terminates rather than looping on a cursor that never advances.
func fullExport(t *testing.T, srv *testharness.Server, token, libID, since string) ([]exportEntityResp, string, []byte) {
	t.Helper()
	var (
		all      []exportEntityResp
		cursor   string
		lastBody []byte
		check    string
	)
	for page := 0; ; page++ {
		if page > 50 {
			t.Fatal("the export never reached its last page: the cursor is not advancing")
		}
		q := "limit=25"
		if since != "" {
			q += "&since=" + since
		}
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		resp, body := exportPage(t, srv, token, libID, q, http.StatusOK)
		lastBody = body
		all = append(all, resp.Entities...)
		check = resp.Checkpoint
		if resp.NextCursor == "" {
			break
		}
		if resp.NextCursor == cursor {
			t.Fatal("nextCursor repeated: the walk would never end")
		}
		cursor = resp.NextCursor
	}
	return all, check, lastBody
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func num(m map[string]any, k string) int {
	f, _ := m[k].(float64)
	return int(f)
}

func boolean(m map[string]any, k string) int {
	if b, _ := m[k].(bool); b {
		return 1
	}
	return 0
}

// nullable turns "" into a NULL argument, so an absent year stays absent rather
// than becoming 0 (the browse API distinguishes them via omitempty).
func nullableInt(m map[string]any, k string) any {
	if _, ok := m[k]; !ok {
		return nil
	}
	return num(m, k)
}

// replay writes an export feed into a second Server's catalog — the mirror's job
// (issue 07), done here by the test because the claim under assertion is that
// the FEED alone is enough. Entities are grouped by type and written
// parent-first: the feed is ordered by change time, not by hierarchy, so a
// Stream can legitimately arrive before the Title it hangs under.
func replay(t *testing.T, dst *testharness.Server, lib struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Name string `json:"name"`
}, entities []exportEntityResp) {
	t.Helper()
	dst.Exec(`INSERT INTO libraries (id, name, kind) VALUES (?, ?, ?)`, lib.ID, lib.Name, lib.Kind)

	byType := map[string][]exportEntityResp{}
	for _, e := range entities {
		byType[e.Type] = append(byType[e.Type], e)
	}

	for _, e := range byType["show"] {
		d := e.Data
		dst.Exec(`INSERT INTO shows (id, library_id, title, year, identity_key, sort_title,
		            tmdb_id, imdb_id, needs_review, hidden, added_at)
		          VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			e.ID, lib.ID, str(d, "title"), nullableInt(d, "year"), str(d, "identityKey"),
			str(d, "sortTitle"), str(d, "tmdbId"), str(d, "imdbId"), boolean(d, "needsReview"),
			tombstoneFlag(e), str(d, "addedAt"))
		replayEntityExtras(dst, "show", e)
	}
	for _, e := range byType["season"] {
		d := e.Data
		dst.Exec(`INSERT INTO seasons (id, show_id, season_number, identity_key, hidden, added_at)
		          VALUES (?,?,?,?,?,?)`,
			e.ID, e.ParentID, num(d, "seasonNumber"), str(d, "identityKey"),
			tombstoneFlag(e), str(d, "addedAt"))
		replayEntityExtras(dst, "season", e)
	}
	for _, e := range byType["artist"] {
		d := e.Data
		dst.Exec(`INSERT INTO artists (id, library_id, name, identity_key, sort_name, hidden,
		            added_at, musicbrainz_id)
		          VALUES (?,?,?,?,?,?,?,?)`,
			e.ID, lib.ID, str(d, "name"), str(d, "identityKey"), str(d, "sortName"),
			tombstoneFlag(e), str(d, "addedAt"), str(d, "musicbrainzId"))
		replayEntityExtras(dst, "artist", e)
	}
	for _, e := range byType["album"] {
		d := e.Data
		dst.Exec(`INSERT INTO albums (id, artist_id, title, year, identity_key, sort_title, hidden,
		            added_at, release_type, musicbrainz_id, musicbrainz_release_id)
		          VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			e.ID, e.ParentID, str(d, "title"), nullableInt(d, "year"), str(d, "identityKey"),
			str(d, "sortTitle"), tombstoneFlag(e), str(d, "addedAt"), str(d, "releaseType"),
			str(d, "musicbrainzId"), str(d, "musicbrainzReleaseId"))
		replayEntityExtras(dst, "album", e)
	}
	for _, typ := range []string{"title", "episode", "track"} {
		kind := map[string]string{"title": "movie", "episode": "episode", "track": "track"}[typ]
		for _, e := range byType[typ] {
			d := e.Data
			var seasonID, albumID any
			if typ == "episode" && e.ParentID != "" {
				seasonID = e.ParentID
			}
			if typ == "track" && e.ParentID != "" {
				albumID = e.ParentID
			}
			status := str(d, "enrichmentStatus")
			if status == "" {
				status = "pending"
			}
			dst.Exec(`INSERT INTO titles (id, library_id, kind, title, year, identity_key, sort_title,
			            added_at, tmdb_id, imdb_id, needs_review, ambiguous, hidden,
			            season_id, season_number, episode_number, episode_label,
			            album_id, disc_number, track_number,
			            overview, tagline, content_rating, release_date, runtime_minutes, studio,
			            musicbrainz_id, musicbrainz_recording_id, enrichment_status, enriched_title)
			          VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				e.ID, lib.ID, kind, str(d, "title"), nullableInt(d, "year"), str(d, "identityKey"),
				str(d, "sortTitle"), str(d, "addedAt"), str(d, "tmdbId"), str(d, "imdbId"),
				boolean(d, "needsReview"), boolean(d, "ambiguous"), tombstoneFlag(e),
				seasonID, num(d, "seasonNumber"), num(d, "episodeNumber"), str(d, "episodeLabel"),
				albumID, num(d, "discNumber"), num(d, "trackNumber"),
				str(d, "overview"), str(d, "tagline"), str(d, "contentRating"), str(d, "releaseDate"),
				num(d, "runtimeMinutes"), str(d, "studio"),
				str(d, "musicbrainzId"), str(d, "musicbrainzRecordingId"), status, str(d, "displayTitle"))
			for i, g := range strList(d, "genres") {
				dst.Exec(`INSERT INTO title_genres (title_id, genre, ord) VALUES (?,?,?)`, e.ID, g, i)
			}
			for i, c := range creditList(d) {
				dst.Exec(`INSERT INTO title_credits (title_id, person, role, character, kind, ord, person_ref)
				          VALUES (?,?,?,?,?,?,?)`,
					e.ID, c["person"], c["role"], c["character"], c["kind"], i, c["personId"])
			}
		}
	}
	for _, e := range byType["edition"] {
		dst.Exec(`INSERT INTO editions (id, title_id, name, added_at) VALUES (?,?,?,?)`,
			e.ID, e.ParentID, str(e.Data, "name"), str(e.Data, "addedAt"))
	}
	for _, e := range byType["file"] {
		d := e.Data
		present := 1
		if e.DeletedAt != "" {
			present = 0
		}
		// The mirror never learns a path and never needs one: it plays through the
		// relay (ADR-0056 §5). The column is NOT NULL and UNIQUE per Edition, so it
		// holds an opaque local placeholder keyed by the sharer's File id.
		dst.Exec(`INSERT INTO files (id, edition_id, path, container, video_codec, audio_codec,
		            width, height, bitrate, duration_ms, size_bytes, added_at, present, part_ordinal)
		          VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			e.ID, e.ParentID, "remote:"+e.ID, str(d, "container"), str(d, "videoCodec"),
			str(d, "audioCodec"), num(d, "width"), num(d, "height"), num(d, "bitrate"),
			num(d, "durationMs"), num(d, "sizeBytes"), str(d, "addedAt"), present, num(d, "partOrdinal"))
	}
	for _, e := range byType["stream"] {
		d := e.Data
		dst.Exec(`INSERT INTO streams (id, file_id, stream_index, kind, codec, language,
		            width, height, channels, is_default, forced, title, commentary, hearing_impaired)
		          VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			e.ID, e.ParentID, num(d, "index"), str(d, "kind"), str(d, "codec"), str(d, "language"),
			num(d, "width"), num(d, "height"), num(d, "channels"), boolean(d, "isDefault"),
			boolean(d, "forced"), str(d, "title"), boolean(d, "commentary"), boolean(d, "hearingImpaired"))
	}
}

func tombstoneFlag(e exportEntityResp) int {
	if e.DeletedAt != "" {
		return 1
	}
	return 0
}

func strList(d map[string]any, k string) []string {
	raw, _ := d[k].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func creditList(d map[string]any) []map[string]string {
	raw, _ := d["cast"].([]any)
	out := make([]map[string]string, 0, len(raw))
	for _, v := range raw {
		m, _ := v.(map[string]any)
		out = append(out, map[string]string{
			"person": str(m, "person"), "role": str(m, "role"),
			"character": str(m, "character"), "kind": str(m, "kind"),
			"personId": str(m, "personId"),
		})
	}
	return out
}

func replayEntityExtras(dst *testharness.Server, typ string, e exportEntityResp) {
	d := e.Data
	if str(d, "overview")+str(d, "contentRating")+str(d, "network")+str(d, "enrichmentStatus") != "" {
		status := str(d, "enrichmentStatus")
		if status == "" {
			status = "pending"
		}
		dst.Exec(`INSERT INTO entity_enrichment (entity_type, entity_id, overview, content_rating,
		            network, enrichment_status) VALUES (?,?,?,?,?,?)`,
			typ, e.ID, str(d, "overview"), str(d, "contentRating"), str(d, "network"), status)
	}
	for i, g := range strList(d, "genres") {
		dst.Exec(`INSERT INTO entity_genres (entity_type, entity_id, genre, ord) VALUES (?,?,?,?)`,
			typ, e.ID, g, i)
	}
	for i, c := range creditList(d) {
		dst.Exec(`INSERT INTO entity_credits (id, entity_type, entity_id, person, character, kind, ord, person_ref)
		          VALUES (?,?,?,?,?,?,?,?)`,
			fmt.Sprintf("%s-%s-%d", typ, e.ID, i), typ, e.ID, c["person"], c["character"], c["kind"], i, c["personId"])
	}
}

// dropped names the browse fields the Export deliberately does not carry, so the
// replay comparison is about the catalog and not about the two things ADR-0056
// hands to other mechanisms: the local Watch state (§5 — it is written HERE,
// only) and artwork, which is relayed and cached rather than mirrored (§5). Disk
// paths are dropped for the reason the whole Export exists — see
// TestExportNamesNoPath, which asserts they are absent from the feed itself.
var dropped = map[string]bool{
	"path": true, "artwork": true, "artworkVersion": true, "hasArtwork": true,
	"extras": true, "subtitles": true,
	"resumePositionMs": true, "watched": true, "unwatchedEpisodeCount": true,
	"resumePoint": true, "photoVersion": true, "lockedFields": true,
}

// normalize strips the dropped keys (and every *Url field) recursively.
func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range t {
			if dropped[k] || strings.HasSuffix(k, "Url") || strings.HasSuffix(k, "URL") {
				continue
			}
			out[k] = normalize(val)
		}
		return out
	case []any:
		out := make([]any, 0, len(t))
		for _, val := range t {
			out = append(out, normalize(val))
		}
		return out
	default:
		return v
	}
}

func browseJSON(t *testing.T, srv *testharness.Server, token, path string) any {
	t.Helper()
	var raw json.RawMessage
	status, body := srv.AuthGET(path, token, &raw)
	if status != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200; body: %s", path, status, body)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decoding %s: %v", path, err)
	}
	return normalize(v)
}

// sameBrowse asserts two Servers answer one browse path identically.
func sameBrowse(t *testing.T, a *testharness.Server, tokA string, b *testharness.Server, tokB, path string) {
	t.Helper()
	wantV, gotV := browseJSON(t, a, tokA, path), browseJSON(t, b, tokB, path)
	if reflect.DeepEqual(wantV, gotV) {
		return
	}
	want, _ := json.MarshalIndent(wantV, "", "  ")
	got, _ := json.MarshalIndent(gotV, "", "  ")
	t.Errorf("replayed export does not reproduce %s\n--- sharer ---\n%s\n--- mirror ---\n%s", path, want, got)
}

// --- the tests ---------------------------------------------------------------

// TestExportReplayedReproducesBrowse is the issue's first acceptance criterion:
// a full export of the fixtures, replayed into an empty database, answers the
// browse API exactly as the Server it came from — Watch state and artwork aside.
// It runs over all three Library kinds, because the TV and music hierarchies are
// where the parent entities (Show/Season, Artist/Album) actually appear.
func TestExportReplayedReproducesBrowse(t *testing.T) {
	requireFixtures(t)

	for _, kind := range []string{"movie", "tv", "music"} {
		t.Run(kind, func(t *testing.T) {
			src := testharness.New(t)
			srcAdmin := adminToken(t, src)
			var libID string
			switch kind {
			case "movie":
				libID = createMovieLibrary(t, src, srcAdmin, fixtureRoot(t))
			case "tv":
				libID = createTVLibrary(t, src, srcAdmin, tvRoot(t))
			default:
				libID = createMusicLibrary(t, src, srcAdmin, musicRoot(t))
			}
			scanLib(t, src, srcAdmin, libID, "")

			peer := linkedServerToken(t, src, srcAdmin, libID)
			first, _ := exportPage(t, src, peer, libID, "limit=25", http.StatusOK)
			if first.Library.ID != libID || first.Library.Kind != kind {
				t.Fatalf("export library block = %+v, want id %s kind %s", first.Library, libID, kind)
			}
			if first.LinkProtocolVersion != 1 {
				t.Errorf("linkProtocolVersion = %d, want 1", first.LinkProtocolVersion)
			}
			entities, _, _ := fullExport(t, src, peer, libID, "")
			if len(entities) == 0 {
				t.Fatal("the export of a scanned Library is empty")
			}

			dst := testharness.New(t)
			dstAdmin := adminToken(t, dst)
			replay(t, dst, first.Library, entities)

			// The top-level grid, whatever the kind's shape (titles / shows / artists).
			grid := "/api/v1/libraries/" + libID + "/titles?limit=100"
			sameBrowse(t, src, srcAdmin, dst, dstAdmin, grid)

			for _, path := range browsePathsUnder(t, src, srcAdmin, kind, libID) {
				sameBrowse(t, src, srcAdmin, dst, dstAdmin, path)
			}
		})
	}
}

// browsePathsUnder walks the sharer's hierarchy and returns every detail path
// worth comparing for the Library's kind.
func browsePathsUnder(t *testing.T, srv *testharness.Server, token, kind, libID string) []string {
	t.Helper()
	var paths []string
	grid := browseJSON(t, srv, token, "/api/v1/libraries/"+libID+"/titles?limit=100").(map[string]any)

	switch kind {
	case "movie":
		for _, raw := range grid["titles"].([]any) {
			paths = append(paths, "/api/v1/titles/"+str(raw.(map[string]any), "id"))
		}
	case "tv":
		for _, raw := range grid["shows"].([]any) {
			showID := str(raw.(map[string]any), "id")
			seasonsPath := "/api/v1/shows/" + showID + "/seasons"
			paths = append(paths, seasonsPath)
			seasons := browseJSON(t, srv, token, seasonsPath).(map[string]any)
			for _, s := range seasons["seasons"].([]any) {
				seasonID := str(s.(map[string]any), "id")
				epPath := "/api/v1/seasons/" + seasonID + "/episodes"
				paths = append(paths, epPath)
				eps := browseJSON(t, srv, token, epPath).(map[string]any)
				for _, e := range eps["episodes"].([]any) {
					paths = append(paths, "/api/v1/titles/"+str(e.(map[string]any), "id"))
				}
			}
		}
	case "music":
		for _, raw := range grid["artists"].([]any) {
			artistID := str(raw.(map[string]any), "id")
			albumsPath := "/api/v1/artists/" + artistID + "/albums"
			paths = append(paths, albumsPath)
			albums := browseJSON(t, srv, token, albumsPath).(map[string]any)
			for _, a := range albums["albums"].([]any) {
				albumID := str(a.(map[string]any), "id")
				trackPath := "/api/v1/albums/" + albumID + "/tracks"
				paths = append(paths, trackPath)
				tracks := browseJSON(t, srv, token, trackPath).(map[string]any)
				for _, tr := range tracks["tracks"].([]any) {
					paths = append(paths, "/api/v1/titles/"+str(tr.(map[string]any), "id"))
				}
			}
		}
	}
	sort.Strings(paths)
	return paths
}

// TestIncrementalExportAfterOneRename is the second acceptance criterion: a
// `since` pull after a single Admin rename returns that Title and nothing else.
// This is the whole reason the feed exists — a mirror that had to re-read a
// thousand-episode library on every edit would not be worth having.
func TestIncrementalExportAfterOneRename(t *testing.T) {
	requireFixtures(t)
	src := testharness.New(t)
	admin := adminToken(t, src)
	libID := createMovieLibrary(t, src, admin, fixtureRoot(t))
	scanLib(t, src, admin, libID, "")
	peer := linkedServerToken(t, src, admin, libID)

	_, checkpoint, _ := fullExport(t, src, peer, libID, "")
	if checkpoint == "" {
		t.Fatal("a full export handed back no checkpoint to resume from")
	}

	// Nothing has changed: the incremental pull is empty and the checkpoint holds.
	quiet, held, _ := fullExport(t, src, peer, libID, checkpoint)
	if len(quiet) != 0 {
		t.Fatalf("an idle Library exported %d entities: %+v", len(quiet), quiet)
	}
	if held != checkpoint {
		t.Errorf("an empty page moved the checkpoint: %q -> %q", checkpoint, held)
	}

	// One Admin rename, through the real hand-edit endpoint.
	titleID := firstTitleID(t, src, admin, libID)
	var ignored json.RawMessage
	if status, body := src.JSON(http.MethodPut, "/api/v1/titles/"+titleID+"/metadata", admin,
		map[string]any{"title": "Dune: Part One"}, &ignored); status != http.StatusOK {
		t.Fatalf("rename: status %d, want 200; body: %s", status, body)
	}

	changed, _, _ := fullExport(t, src, peer, libID, checkpoint)
	if len(changed) != 1 {
		var got []string
		for _, e := range changed {
			got = append(got, e.Type+"/"+e.ID)
		}
		t.Fatalf("one rename exported %d entities (%v), want exactly the renamed Title", len(changed), got)
	}
	if changed[0].ID != titleID || changed[0].Type != "title" {
		t.Errorf("incremental returned %s/%s, want title/%s", changed[0].Type, changed[0].ID, titleID)
	}
	if got := str(changed[0].Data, "displayTitle"); got != "Dune: Part One" {
		t.Errorf("renamed Title's displayTitle = %q, want the new name", got)
	}
}

func firstTitleID(t *testing.T, srv *testharness.Server, token, libID string) string {
	t.Helper()
	var list struct {
		Titles []struct {
			ID string `json:"id"`
		} `json:"titles"`
	}
	if status, body := srv.AuthGET("/api/v1/libraries/"+libID+"/titles?limit=100", token, &list); status != http.StatusOK {
		t.Fatalf("list titles: %d; body: %s", status, body)
	}
	if len(list.Titles) == 0 {
		t.Fatal("the scanned Library has no Titles")
	}
	return list.Titles[0].ID
}

// TestExportTombstonesAMissingFile: a File that leaves the disk is soft-deleted
// (ADR-0008), and the feed says so with deletedAt so the mirror can tombstone
// rather than quietly keep serving a film that is gone. A Title whose Files are
// all Missing is tombstoned too.
func TestExportTombstonesAMissingFile(t *testing.T) {
	requireFixtures(t)
	src := testharness.New(t)
	admin := adminToken(t, src)
	libID := createMovieLibrary(t, src, admin, fixtureRoot(t))
	scanLib(t, src, admin, libID, "")
	peer := linkedServerToken(t, src, admin, libID)

	full, checkpoint, _ := fullExport(t, src, peer, libID, "")
	for _, e := range full {
		if e.DeletedAt != "" {
			t.Fatalf("a freshly scanned Library exported a tombstone: %s/%s", e.Type, e.ID)
		}
	}

	titleID := firstTitleID(t, src, admin, libID)
	src.Exec(`UPDATE files SET present = 0
	            WHERE edition_id IN (SELECT id FROM editions WHERE title_id = ?)`, titleID)
	src.SetTitleHidden(titleID, true)

	changed, _, _ := fullExport(t, src, peer, libID, checkpoint)
	sawTitle, sawFile := false, false
	for _, e := range changed {
		switch e.Type {
		case "title":
			if e.ID == titleID {
				sawTitle = true
				if e.DeletedAt == "" {
					t.Error("a Title whose Files are all Missing exported with no deletedAt")
				}
			}
		case "file":
			sawFile = true
			if e.DeletedAt == "" {
				t.Errorf("Missing File %s exported with no deletedAt", e.ID)
			}
		}
	}
	if !sawTitle || !sawFile {
		t.Errorf("the incremental pull missed the tombstones (title=%v file=%v)", sawTitle, sawFile)
	}

	// The tombstoned Title is still IN the feed — a mirror that never saw it
	// again could not tell "deleted" from "not my page".
	if len(changed) == 0 {
		t.Fatal("soft-deleting a Title changed nothing in the feed")
	}
}

// TestExportNamesNoPath is the third acceptance criterion: nothing in the body
// is a disk path. The Export is the one place the sharer decides what a peer
// learns, and where their films live is not on the list.
func TestExportNamesNoPath(t *testing.T) {
	requireFixtures(t)
	src := testharness.New(t)
	admin := adminToken(t, src)
	root := fixtureRoot(t)
	libID := createMovieLibrary(t, src, admin, root)
	scanLib(t, src, admin, libID, "")
	peer := linkedServerToken(t, src, admin, libID)

	var raw json.RawMessage
	status, body := src.AuthGET("/api/v1/libraries/"+libID+"/export?limit=500", peer, &raw)
	if status != http.StatusOK {
		t.Fatalf("export: %d; body: %s", status, body)
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		t.Fatalf("decoding export: %v", err)
	}
	var walk func(path string, v any)
	walk = func(where string, v any) {
		switch t2 := v.(type) {
		case map[string]any:
			for k, val := range t2 {
				if strings.EqualFold(k, "path") || strings.HasSuffix(k, "Path") {
					t.Errorf("%s.%s: the export carries a path-shaped FIELD", where, k)
				}
				walk(where+"."+k, val)
			}
		case []any:
			for i, val := range t2 {
				walk(fmt.Sprintf("%s[%d]", where, i), val)
			}
		case string:
			if strings.HasPrefix(t2, "/") {
				t.Errorf("%s = %q: a /-rooted value crossed the Link", where, t2)
			}
			if strings.Contains(t2, root) {
				t.Errorf("%s = %q: names the sharer's library folder", where, t2)
			}
		}
	}
	walk("$", tree)

	// And the browse API, which the mirror is NOT allowed to walk, does carry
	// them — so the test above is asserting a real difference, not an empty one.
	titleID := firstTitleID(t, src, admin, libID)
	var detail struct {
		Editions []struct {
			Files []struct {
				Path string `json:"path"`
			} `json:"files"`
		} `json:"editions"`
	}
	if status, body := src.AuthGET("/api/v1/titles/"+titleID, admin, &detail); status != http.StatusOK {
		t.Fatalf("title detail: %d; body: %s", status, body)
	}
	if len(detail.Editions) == 0 || len(detail.Editions[0].Files) == 0 ||
		!strings.HasPrefix(detail.Editions[0].Files[0].Path, "/") {
		t.Fatal("the browse API stopped carrying File paths; this test no longer proves anything")
	}
}

// TestExportRefusesAnAncientCursor: a `since` older than the retention of
// soft-deleted rows answers 410 RESYNC, and the home Server pulls in full
// (ADR-0056 §4). A well-formed request about a position that is gone, so 410 —
// not the 400 a garbled cursor gets.
func TestExportRefusesAnAncientCursor(t *testing.T) {
	requireFixtures(t)
	src := testharness.New(t)
	admin := adminToken(t, src)
	libID := createMovieLibrary(t, src, admin, fixtureRoot(t))
	scanLib(t, src, admin, libID, "")
	peer := linkedServerToken(t, src, admin, libID)

	ancient := base64.RawURLEncoding.EncodeToString(
		[]byte(`{"u":"2019-01-01T00:00:00Z","t":"title","i":"whatever"}`))

	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	status, body := src.AuthGET("/api/v1/libraries/"+libID+"/export?since="+ancient, peer, &env)
	if status != http.StatusGone {
		t.Fatalf("ancient since = %d, want 410; body: %s", status, body)
	}
	if env.Error.Code != "RESYNC" {
		t.Errorf("error code = %q, want RESYNC; body: %s", env.Error.Code, body)
	}

	// A garbled cursor is a different failure: the request itself is wrong.
	status, body = src.AuthGET("/api/v1/libraries/"+libID+"/export?since=not-base64!!", peer, &env)
	if status != http.StatusBadRequest {
		t.Fatalf("garbled since = %d, want 400; body: %s", status, body)
	}

	// And a fresh checkpoint is served normally, so the 410 is about age and not
	// about `since` being present at all.
	_, checkpoint, _ := fullExport(t, src, peer, libID, "")
	exportPage(t, src, peer, libID, "since="+checkpoint, http.StatusOK)
}

// TestExportIsHiddenFromEveryoneElse: the route belongs to a linked Server and
// to an Admin. A Member gets the 404 an unknown route gives (never a 403 — that
// would tell them it exists), and so does a `remote` User asking about a Library
// nobody granted it.
func TestExportIsHiddenFromEveryoneElse(t *testing.T) {
	requireFixtures(t)
	src := testharness.New(t)
	admin := adminToken(t, src)
	libID := createMovieLibrary(t, src, admin, fixtureRoot(t))
	scanLib(t, src, admin, libID, "")

	// A Member — even one granted the Library — must not see the feed.
	src.CreateMember("member", "hunter2hunter2")
	memberTok := src.LoginAs("member", "hunter2hunter2")
	memberID := userIDForName(t, src, admin, "member")
	grantLibraries(t, src, admin, memberID, libID)
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	status, body := src.AuthGET("/api/v1/libraries/"+libID+"/export", memberTok, &env)
	if status != http.StatusNotFound {
		t.Fatalf("member export = %d, want 404; body: %s", status, body)
	}
	if env.Error.Code != "NOT_FOUND" {
		t.Errorf("member export code = %q, want NOT_FOUND", env.Error.Code)
	}
	// The Member can still browse the same Library: the 404 is about this route.
	if status, body := src.AuthGET("/api/v1/libraries/"+libID+"/titles", memberTok, nil); status != http.StatusOK {
		t.Fatalf("member browse = %d, want 200; body: %s", status, body)
	}

	// A `remote` User with no grant is told nothing either.
	ungranted := linkedServerToken(t, src, admin)
	if status, body := src.AuthGET("/api/v1/libraries/"+libID+"/export", ungranted, nil); status != http.StatusNotFound {
		t.Fatalf("ungranted remote export = %d, want 404; body: %s", status, body)
	}

	// An Admin may call it — they can call everything.
	exportPage(t, src, admin, libID, "", http.StatusOK)

	// An unknown Library is the same 404, from a granted peer.
	peer := linkedServerToken(t, src, admin, libID)
	if status, body := src.AuthGET("/api/v1/libraries/nope/export", peer, nil); status != http.StatusNotFound {
		t.Fatalf("unknown library export = %d, want 404; body: %s", status, body)
	}
	// And no credential at all is a 401, not a 404: the route is authenticated
	// like every other, and only the ROLE decision hides it.
	if status, _ := src.GET("/api/v1/libraries/"+libID+"/export", nil); status != http.StatusUnauthorized {
		t.Errorf("anonymous export = %d, want 401", status)
	}
}

func userIDForName(t *testing.T, srv *testharness.Server, adminTok, name string) string {
	t.Helper()
	var list struct {
		Users []struct {
			ID       string `json:"id"`
			Username string `json:"username"`
		} `json:"users"`
	}
	if status, body := srv.AuthGET("/api/v1/users", adminTok, &list); status != http.StatusOK {
		t.Fatalf("list users: %d; body: %s", status, body)
	}
	for _, u := range list.Users {
		if u.Username == name {
			return u.ID
		}
	}
	t.Fatalf("user %q not found", name)
	return ""
}

// TestExportPaginationIsAStableKeysetWalk: the feed pages on (updated_at, type,
// id) rather than an offset, so every entity appears exactly once across the
// walk and the limit is honoured and capped.
func TestExportPaginationIsAStableKeysetWalk(t *testing.T) {
	requireFixtures(t)
	src := testharness.New(t)
	admin := adminToken(t, src)
	libID := createTVLibrary(t, src, admin, tvRoot(t))
	scanLib(t, src, admin, libID, "")
	peer := linkedServerToken(t, src, admin, libID)

	oneShot, _ := exportPage(t, src, peer, libID, "limit=500", http.StatusOK)
	if oneShot.NextCursor != "" {
		t.Fatal("the TV fixture does not fit in one 500-row page; this test needs a smaller library")
	}

	paged, _, _ := fullExport(t, src, peer, libID, "")
	if len(paged) != len(oneShot.Entities) {
		t.Fatalf("paged walk returned %d entities, one-shot returned %d", len(paged), len(oneShot.Entities))
	}
	seen := map[string]bool{}
	for i, e := range paged {
		key := e.Type + "/" + e.ID
		if seen[key] {
			t.Errorf("%s appeared twice in one walk", key)
		}
		seen[key] = true
		if i > 0 {
			prev := paged[i-1]
			if prev.UpdatedAt > e.UpdatedAt {
				t.Errorf("feed went backwards at %d: %q then %q", i, prev.UpdatedAt, e.UpdatedAt)
			}
		}
	}
	// A small page is respected; an over-large ask is clamped, not refused.
	small, _ := exportPage(t, src, peer, libID, "limit=3", http.StatusOK)
	if len(small.Entities) != 3 || small.NextCursor == "" {
		t.Errorf("limit=3 returned %d entities (nextCursor %q)", len(small.Entities), small.NextCursor)
	}
	huge, _ := exportPage(t, src, peer, libID, "limit=99999", http.StatusOK)
	if len(huge.Entities) != len(oneShot.Entities) {
		t.Errorf("an over-large limit was not clamped to a served page")
	}
}

// TestExportCarriesEveryEntityType: the feed's vocabulary is the ten types
// ADR-0056 §4 names, and a mirror that only ever saw `title` could not rebuild a
// Show or an Album.
func TestExportCarriesEveryEntityType(t *testing.T) {
	requireFixtures(t)
	want := map[string][]string{
		"movie": {"title", "edition", "file", "stream"},
		"tv":    {"show", "season", "episode", "edition", "file", "stream"},
		"music": {"artist", "album", "track", "edition", "file", "stream"},
	}
	for kind, types := range want {
		t.Run(kind, func(t *testing.T) {
			src := testharness.New(t)
			admin := adminToken(t, src)
			var libID string
			switch kind {
			case "movie":
				libID = createMovieLibrary(t, src, admin, fixtureRoot(t))
			case "tv":
				libID = createTVLibrary(t, src, admin, tvRoot(t))
			default:
				libID = createMusicLibrary(t, src, admin, musicRoot(t))
			}
			scanLib(t, src, admin, libID, "")
			peer := linkedServerToken(t, src, admin, libID)
			entities, _, _ := fullExport(t, src, peer, libID, "")

			seen := map[string]bool{}
			for _, e := range entities {
				seen[e.Type] = true
				if e.UpdatedAt == "" {
					t.Errorf("%s/%s has no updatedAt: the feed cannot be ordered", e.Type, e.ID)
				}
			}
			for _, typ := range types {
				if !seen[typ] {
					t.Errorf("a %s Library exported no %q entity (saw %v)", kind, typ, exportTypesSeen(seen))
				}
			}
		})
	}
}

func exportTypesSeen(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
