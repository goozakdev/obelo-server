package api_test

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Black-box tests for row paging, the caps, the malformed-entry drops and the
// in-memory cache of an Online source (ADR-0068, issue 04), through the same
// Installed Plugin the other Online source tests use.

func pagedItem(media *onlineMedia, id string) map[string]any {
	return map[string]any{"id": id, "title": "Title " + id, "thumbnailUrl": media.srv.URL + "/thumb/" + id + ".png", "durationMs": 1000}
}

type onlinePageResp struct {
	Items []struct {
		ID         string `json:"id"`
		DurationMs int64  `json:"durationMs"`
	} `json:"items"`
	NextCursor *string `json:"nextCursor"`
}

func (p onlinePageResp) ids() string {
	var out []string
	for _, it := range p.Items {
		out = append(out, it.ID)
	}
	return strings.Join(out, ",")
}

// TestOnlineAnswersAreCappedAndMalformedEntriesDroppedWithoutAnError: over-length
// text is truncated and the entry kept, rows and items over the caps are cut, and
// every kind of malformed entry (including a duration that is not a number) is
// dropped while its valid siblings survive and the request still answers 200.
func TestOnlineAnswersAreCappedAndMalformedEntriesDroppedWithoutAnError(t *testing.T) {
	t.Parallel()
	srv, admin, src, media := onlineServer(t)
	with := func(id string, kv ...any) map[string]any {
		it := pagedItem(media, id)
		for i := 0; i < len(kv); i += 2 {
			it[kv[i].(string)] = kv[i+1]
		}
		return it
	}
	long := strings.Repeat("é", 5000)
	src.rows = func() []map[string]any {
		items := []map[string]any{
			with("ok1", "title", long, "description", long),
			with("httpthumb", "thumbnailUrl", "http://insecure.example/x.png"),
			with("negative", "durationMs", -5),
			with("nonnumeric", "durationMs", "soon"),
			with("baddate", "publishedAt", "last tuesday"),
			with("bad/id"),
			with("", "id", ""),
			with("notitle", "title", ""),
			with("ok1"), // duplicate id within the row
			with("fortyeight", "durationMs", 48*3600*1000),
		}
		for i := 0; i < 150; i++ {
			items = append(items, pagedItem(media, fmt.Sprintf("fill%d", i)))
		}
		rows := []map[string]any{
			{"id": "good", "label": long, "items": items},
			{"id": "bad/row", "label": "Dropped", "items": []map[string]any{pagedItem(media, "x")}},
		}
		for i := 0; i < 60; i++ {
			rows = append(rows, map[string]any{"id": fmt.Sprintf("r%d", i), "label": "Row", "items": []map[string]any{}})
		}
		return rows
	}
	src.row = func(string, string) map[string]any {
		items := []map[string]any{
			with("negative", "durationMs", -5),
			with("nonnumeric", "durationMs", "soon"),
			with("httpthumb", "thumbnailUrl", "http://insecure.example/x.png"),
			with("bad id"),
			pagedItem(media, "p1"),
			with("fortyeight", "durationMs", 48*3600*1000),
		}
		for i := 0; i < 150; i++ {
			items = append(items, pagedItem(media, fmt.Sprintf("fill%d", i)))
		}
		return map[string]any{"items": items}
	}

	var page struct {
		Rows []struct {
			ID    string `json:"id"`
			Label string `json:"label"`
			Items []struct {
				ID          string `json:"id"`
				Title       string `json:"title"`
				Description string `json:"description"`
				DurationMs  int64  `json:"durationMs"`
			} `json:"items"`
		} `json:"rows"`
	}
	if status, body := srv.AuthGET(onlineBase+"/"+onlineSlug+"/rows", admin, &page); status != http.StatusOK {
		t.Fatalf("GET rows = %d, want 200 despite bad siblings; body: %.300s", status, body)
	}
	if len(page.Rows) != 50 {
		t.Fatalf("rows = %d, want the cap 50 (the unsafe row id dropped)", len(page.Rows))
	}
	for _, r := range page.Rows {
		if r.ID == "bad/row" {
			t.Fatal("a row whose id is outside the URL-safe set was served")
		}
	}
	good := page.Rows[0]
	if good.ID != "good" || len([]rune(good.Label)) != 100 {
		t.Fatalf("row = %q with a %d-character label, want its label truncated to 100 and the row kept", good.ID, len([]rune(good.Label)))
	}
	if len(good.Items) != 100 {
		t.Fatalf("items in the row = %d, want the cap 100", len(good.Items))
	}
	first := good.Items[0]
	if first.ID != "ok1" || len([]rune(first.Title)) != 200 || len([]rune(first.Description)) != 2000 {
		t.Fatalf("first item = %q, title %d, description %d characters; want ok1 with both truncated to 200 and 2000 and kept",
			first.ID, len([]rune(first.Title)), len([]rune(first.Description)))
	}
	var got []string
	for _, it := range good.Items[:3] {
		got = append(got, it.ID)
	}
	if want := "ok1,fortyeight,fill0"; strings.Join(got, ",") != want {
		t.Fatalf("leading item ids = %v, want %q (every malformed entry dropped, the 48 hour item kept)", got, want)
	}
	if good.Items[1].DurationMs != 48*3600*1000 {
		t.Fatalf("a 48 hour duration came back as %d", good.Items[1].DurationMs)
	}

	var next onlinePageResp
	if status, body := srv.AuthGET(onlineBase+"/"+onlineSlug+"/rows/good?cursor=c1", admin, &next); status != http.StatusOK {
		t.Fatalf("GET row = %d, want 200 despite bad siblings; body: %.300s", status, body)
	}
	if len(next.Items) != 100 || !strings.HasPrefix(next.ids(), "p1,fortyeight,fill0") {
		t.Fatalf("page = %d items %.40q, want the cap 100 starting p1,fortyeight,fill0", len(next.Items), next.ids())
	}
}

// TestARowPagesForwardByTheOpaqueCursorAndTheLastPageHasNone: the cursor in a rows
// answer is handed back to row(), whose answer names the next, and nextCursor is
// absent (not null) on the last page.
func TestARowPagesForwardByTheOpaqueCursorAndTheLastPageHasNone(t *testing.T) {
	t.Parallel()
	srv, admin, src, media := onlineServer(t)
	src.rows = func() []map[string]any {
		return []map[string]any{{"id": "recent", "label": "Recent", "nextCursor": "tok/1?a b", "items": []map[string]any{pagedItem(media, "a")}}}
	}
	src.row = func(rowID, cursor string) map[string]any {
		switch cursor {
		case "tok/1?a b":
			return map[string]any{"items": []map[string]any{pagedItem(media, "b")}, "nextCursor": "tok2"}
		case "tok2":
			return map[string]any{"items": []map[string]any{pagedItem(media, "c")}}
		}
		return map[string]any{"items": []map[string]any{}}
	}
	var rows onlineRowsResp
	if status, body := srv.AuthGET(onlineBase+"/"+onlineSlug+"/rows", admin, &rows); status != http.StatusOK {
		t.Fatalf("GET rows = %d; body: %s", status, body)
	}
	cur := rows.Rows[0].NextCursor
	if cur == nil || *cur != "tok/1?a b" {
		t.Fatalf("rows nextCursor = %v, want the Plugin's token", cur)
	}

	var ids []string
	for i := 0; cur != nil; i++ {
		if i > 5 {
			t.Fatal("paging never ended")
		}
		var page onlinePageResp
		path := onlineBase + "/" + onlineSlug + "/rows/recent?cursor=" + urlQueryEscape(*cur)
		status, body := srv.AuthGET(path, admin, &page)
		if status != http.StatusOK {
			t.Fatalf("GET %s = %d; body: %s", path, status, body)
		}
		ids = append(ids, page.ids())
		cur = page.NextCursor
		if cur == nil && bytes.Contains(body, []byte("nextCursor")) {
			t.Fatalf("the last page carries a nextCursor field: %s", body)
		}
	}
	if strings.Join(ids, "|") != "b|c" {
		t.Fatalf("pages = %v, want b then c", ids)
	}
	src.mu.Lock()
	calls := fmt.Sprint(src.rowCalls)
	src.mu.Unlock()
	if want := "[map[cursor:tok/1?a b rowId:recent] map[cursor:tok2 rowId:recent]]"; calls != want {
		t.Fatalf("the Plugin's row() calls = %s, want %s", calls, want)
	}

	for name, path := range map[string]string{
		"no cursor":     onlineBase + "/" + onlineSlug + "/rows/recent",
		"unsafe row id": onlineBase + "/" + onlineSlug + "/rows/a%20b?cursor=x",
		"unknown":       onlineBase + "/nosuch/rows/recent?cursor=x",
	} {
		if status, _ := srv.AuthGET(path, admin, nil); status != http.StatusNotFound {
			t.Errorf("%s: GET %s = %d, want 404", name, path, status)
		}
	}
}

func urlQueryEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// TestTwoUsersLoadingTheSameRowCauseOnePluginCall: the answers are cached in
// memory and shared across Users.
func TestTwoUsersLoadingTheSameRowCauseOnePluginCall(t *testing.T) {
	t.Parallel()
	srv, admin, src, media := onlineServer(t)
	src.rows = func() []map[string]any {
		return []map[string]any{{"id": "recent", "label": "Recent", "nextCursor": "c1", "items": []map[string]any{pagedItem(media, "a")}}}
	}
	src.row = func(string, string) map[string]any {
		return map[string]any{"items": []map[string]any{pagedItem(media, "b")}}
	}
	srv.CreateUser(admin, "second", "secondpass123", "admin")
	second := srv.LoginAs("second", "secondpass123")
	for _, token := range []string{admin, second} {
		if st, body := srv.AuthGET(onlineBase+"/"+onlineSlug+"/rows", token, nil); st != http.StatusOK {
			t.Fatalf("GET rows = %d; body: %s", st, body)
		}
		if st, body := srv.AuthGET(onlineBase+"/"+onlineSlug+"/rows/recent?cursor=c1", token, nil); st != http.StatusOK {
			t.Fatalf("GET row = %d; body: %s", st, body)
		}
	}
	if n := src.calls(); n != 2 {
		t.Fatalf("two Users caused %d Plugin calls, want 2 (one rows(), one row())", n)
	}
}

// TestNoRowItemOrThumbnailBytesAreKeptInTheDatabaseOrDataDir: after the rows, a row
// page and a thumbnail have all been served, no file under the data directory (the
// database and its journal included) holds any of what the source answered.
func TestNoRowItemOrThumbnailBytesAreKeptInTheDatabaseOrDataDir(t *testing.T) {
	t.Parallel()
	srv, admin, src, media := onlineServer(t)
	src.rows = func() []map[string]any {
		it := pagedItem(media, "v1")
		it["title"] = "ZQ-UNIQUE-TITLE-7731"
		it["description"] = "ZQ-UNIQUE-DESCRIPTION-7731"
		return []map[string]any{{"id": "recent", "label": "ZQ-UNIQUE-LABEL-7731", "nextCursor": "ZQ-UNIQUE-CURSOR-7731", "items": []map[string]any{it}}}
	}
	src.row = func(string, string) map[string]any {
		it := pagedItem(media, "v2")
		it["title"] = "ZQ-UNIQUE-PAGE-TITLE-7731"
		return map[string]any{"items": []map[string]any{it}}
	}
	if st, body := srv.AuthGET(onlineBase+"/"+onlineSlug+"/rows", admin, nil); st != http.StatusOK {
		t.Fatalf("GET rows = %d; body: %s", st, body)
	}
	if st, body := srv.AuthGET(onlineBase+"/"+onlineSlug+"/rows/recent?cursor=ZQ-UNIQUE-CURSOR-7731", admin, nil); st != http.StatusOK {
		t.Fatalf("GET row = %d; body: %s", st, body)
	}
	resp, thumb := getBytes(t, srv, onlineBase+"/"+onlineSlug+"/items/v1/thumbnail", map[string]string{"Authorization": "Bearer " + admin})
	if resp.StatusCode != http.StatusOK || !bytes.Equal(thumb, onlineThumbPNG) {
		t.Fatalf("thumbnail = %d (%d bytes), want the media host's PNG", resp.StatusCode, len(thumb))
	}

	needles := [][]byte{
		[]byte("ZQ-UNIQUE"), []byte("media.example"), []byte(media.srv.URL),
		onlineThumbPNG[:64], bytes.Repeat([]byte{0x42}, 64),
	}
	var files, dbFiles int
	err := filepath.Walk(srv.DataDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		files++
		if strings.Contains(p, ".db") || strings.Contains(p, ".sqlite") {
			dbFiles++
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("reading %s: %v", p, err)
			return nil
		}
		for _, n := range needles {
			if bytes.Contains(b, n) {
				t.Errorf("%s holds %q, which only the source's answers carry", p, n[:min(len(n), 24)])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if dbFiles == 0 {
		t.Fatalf("no database file found among the %d files under %s, so the walk proves nothing", files, srv.DataDir)
	}
}

// TestTheRowEndpointIs404ForAnUngrantedMemberAndARemoteCaller: the page endpoint of
// this slice follows the same access rule as the rest of an Online source, and the
// Plugin is never called for a refused caller. The Admin control keeps the test
// honest: the same path answers 200 for the caller who may see the source.
func TestTheRowEndpointIs404ForAnUngrantedMemberAndARemoteCaller(t *testing.T) {
	t.Parallel()
	srv, admin, src, media := onlineServer(t)
	src.row = func(string, string) map[string]any {
		return map[string]any{"items": []map[string]any{pagedItem(media, "b")}}
	}
	path := onlineBase + "/" + onlineSlug + "/rows/recent?cursor=c1"
	if st, body := srv.AuthGET(path, admin, nil); st != http.StatusOK {
		t.Fatalf("Admin: GET %s = %d, want 200; body: %s", path, st, body)
	}
	calls := src.calls()

	srv.CreateUser(admin, "kid", "memberpass123", "member")
	member := srv.LoginAs("kid", "memberpass123")
	var remoteOut struct {
		ID string `json:"id"`
	}
	if st, body := srv.JSON(http.MethodPost, "/api/v1/users", admin,
		map[string]any{"username": "peer", "role": "remote"}, &remoteOut); st != http.StatusCreated {
		t.Fatalf("creating the remote User = %d; body: %s", st, body)
	}
	remote := srv.IssueTokenForUser(remoteOut.ID, "peer-server-id")
	for name, token := range map[string]string{"member": member, "remote": remote} {
		if st, _ := srv.AuthGET(path, token, nil); st != http.StatusNotFound {
			t.Errorf("%s: GET %s = %d, want 404", name, path, st)
		}
	}
	if st, _ := srv.AuthGET(path, "", nil); st != http.StatusUnauthorized {
		t.Errorf("no token: GET %s = %d, want 401", path, st)
	}
	if n := src.calls(); n != calls {
		t.Fatalf("a refused caller caused %d Plugin calls", n-calls)
	}
}

// TestASettingsSaveOrAPluginRebuildClearsASourcesCachedRows: the 5 minute rows cache
// must not keep serving what a source answered under settings or a Plugin build
// that are no longer in force.
func TestASettingsSaveOrAPluginRebuildClearsASourcesCachedRows(t *testing.T) {
	t.Parallel()
	srv, admin, src, _ := onlineServer(t)
	rowsPath := onlineBase + "/" + onlineSlug + "/rows"
	load := func(want int, why string) {
		t.Helper()
		if st, body := srv.AuthGET(rowsPath, admin, nil); st != http.StatusOK {
			t.Fatalf("GET rows = %d; body: %s", st, body)
		}
		if n := src.calls(); n != want {
			t.Fatalf("%s: %d Plugin calls, want %d", why, n, want)
		}
	}
	load(1, "first load")
	load(1, "second load, cached")

	if st, body := saveDeclaredSettings(t, srv, admin, onlineSlug, map[string]any{"region": "eu"}); st != http.StatusOK {
		t.Fatalf("saving settings = %d; body: %s", st, body)
	}
	load(2, "after a settings save")
	load(2, "cached again")

	for _, verb := range []string{"disable", "enable"} {
		if st, body := srv.JSON(http.MethodPost, "/api/v1/settings/plugins/"+onlineSlug+"/"+verb, admin, nil, nil); st != http.StatusOK {
			t.Fatalf("%s = %d; body: %s", verb, st, body)
		}
	}
	load(3, "after the Plugin was rebuilt")
}
