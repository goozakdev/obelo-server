package api_test

import (
	"net/http"
	"testing"
)

// TestRowPagingFollowsTheGrantGate: GET /onlineSources/{id}/rows/{rowId} answers a
// granted Member and an Admin, and a 404 to an ungranted Member and a Remote-role
// caller; and a granted Member gets 403 from the Admin-only /transcoding that lists
// the live Online sessions.
func TestRowPagingFollowsTheGrantGate(t *testing.T) {
	t.Parallel()
	srv, admin, _, src, media := onlineAccessServerFull(t, nil)
	src.rows = func() []map[string]any {
		return []map[string]any{{"id": "recent", "label": "Recent", "nextCursor": "c1", "items": []map[string]any{pagedItem(media, "a")}}}
	}
	src.row = func(string, string) map[string]any {
		return map[string]any{"items": []map[string]any{pagedItem(media, "b")}}
	}
	grantedID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	srv.CreateUser(admin, "other", "memberpass123", "member")
	mustGrantSources(t, srv, admin, grantedID, onlineSlug)
	remoteID := createRemoteUser(t, srv, admin, "peer")
	srv.Exec(`INSERT INTO user_online_source_access (user_id, source_id) VALUES (?, ?)`, remoteID, onlineSlug)

	path := onlineBase + "/" + onlineSlug + "/rows/recent?cursor=c1"
	granted, other := srv.LoginAs("kid", "memberpass123"), srv.LoginAs("other", "memberpass123")
	remote := srv.IssueTokenForUser(remoteID, "peer-server-id")
	for name, want := range map[string]struct {
		token  string
		status int
	}{"admin": {admin, 200}, "granted": {granted, 200}, "ungranted": {other, 404}, "remote": {remote, 404}} {
		if st, body := srv.AuthGET(path, want.token, nil); st != want.status {
			t.Errorf("%s: GET row page = %d, want %d; body: %s", name, st, want.status, body)
		}
	}
	if st, _ := srv.AuthGET("/api/v1/transcoding", granted, nil); st != http.StatusForbidden {
		t.Errorf("a granted Member's GET /transcoding = %d, want 403", st)
	}
}
