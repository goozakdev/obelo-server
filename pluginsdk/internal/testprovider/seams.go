package testprovider

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk"
)

// The SDK's example of each one-call Extension point: a Web reference provider,
// a Lyric provider, a Marker provider and a password Sign-in provider that also
// answers lookup(subject). Each is the SHAPE the authoring guide shows — a
// struct holding a Host, one method per call, no net/http — compiled into
// pluginsdk/testdata/guest and driven through wazero by
// internal/plugins/sdk_guest_seams_test.go.

// The paths the Lyric and Marker providers ask the operator's source for.
const (
	LyricsPath  = "/lyrics"
	MarkersPath = "/markers"
)

// The paths the Online source provider asks the operator's source for.
const (
	ShelvesPath = "/shelves"
	ShelfPath   = "/shelf"
	FindPath    = "/find"
	StreamPath  = "/stream"
)

// sdk-sample:begin webref

// References links an item's IMDb id. It needs no Host: the call is a pure
// computation over the ids in the request, and a fetch made while it runs is
// refused.
type References struct{}

// Links answers one reference when the host holds an IMDb id for the item, keyed
// to that id, and nothing otherwise.
func (References) Links(_ context.Context, req pluginapi.WebReferencesRequest) (pluginapi.WebReferencesResponse, error) {
	id, ok := req.IDs["imdb"]
	if !ok || id == "" {
		return pluginapi.WebReferencesResponse{}, nil
	}
	return pluginapi.WebReferencesResponse{References: []pluginapi.WebReference{{
		Namespace: "imdb",
		ID:        id,
		Label:     "IMDb",
		URL:       "https://www.imdb.com/title/" + url.PathEscape(id) + "/",
	}}}, nil
}

// sdk-sample:end webref

// sdk-sample:begin lyrics

// Lyrics asks a lyrics source at the operator's URL for one track's words.
type Lyrics struct {
	host pluginsdk.Host
}

// NewLyrics builds the provider on a Host.
func NewLyrics(h pluginsdk.Host) *Lyrics { return &Lyrics{host: h} }

// Lyrics answers the source's timed lines as Synced, stating the length they
// were timed for, else its text as Plain. A source with nothing is an empty
// answer — a miss the host remembers — and an error is a failure it does not.
func (l *Lyrics) Lyrics(ctx context.Context, req pluginapi.LyricsRequest) (pluginapi.LyricsResponse, error) {
	q := url.Values{"artist": {req.Artist}, "title": {req.Title}}
	if req.Album != "" {
		q.Set("album", req.Album)
	}
	if req.DurationMs > 0 {
		q.Set("duration_ms", strconv.FormatInt(req.DurationMs, 10))
	}
	var out struct {
		Synced     []pluginapi.LyricLine `json:"synced"`
		Plain      string                `json:"plain"`
		DurationMs int64                 `json:"durationMs"`
	}
	err := pluginsdk.GetJSON(ctx, l.host, baseOf(l.host.Settings().URL)+LyricsPath, q, &out)
	var fe *pluginsdk.FetchError
	if errors.As(err, &fe) && fe.IsNotFound() {
		return pluginapi.LyricsResponse{}, nil
	}
	if err != nil {
		return pluginapi.LyricsResponse{}, err
	}
	switch {
	case len(out.Synced) > 0:
		return pluginapi.LyricsResponse{Kind: pluginapi.LyricsSynced, Lines: out.Synced, DurationMs: out.DurationMs}, nil
	case out.Plain != "":
		return pluginapi.LyricsResponse{Kind: pluginapi.LyricsPlain, Text: out.Plain}, nil
	}
	return pluginapi.LyricsResponse{}, nil
}

// sdk-sample:end lyrics

// sdk-sample:begin markers

// Markers asks a marker database at the operator's URL where a Movie's or an
// Episode's Intro, Recap, Credits and Preview are, by the item's IMDb id.
type Markers struct {
	host pluginsdk.Host
}

// NewMarkers builds the provider on a Host.
func NewMarkers(h pluginsdk.Host) *Markers { return &Markers{host: h} }

// Markers answers every span the source measured, each stating the length of
// the recording it was measured on, which the host checks against the File.
func (m *Markers) Markers(ctx context.Context, req pluginapi.MarkersRequest) (pluginapi.MarkersResponse, error) {
	id, ok := req.IDs["imdb"]
	if !ok || id == "" {
		return pluginapi.MarkersResponse{}, nil
	}
	var out struct {
		DurationMs int64 `json:"durationMs"`
		Markers    []struct {
			Kind    string `json:"kind"`
			StartMs int64  `json:"startMs"`
			EndMs   int64  `json:"endMs"`
		} `json:"markers"`
	}
	err := pluginsdk.GetJSON(ctx, m.host, baseOf(m.host.Settings().URL)+MarkersPath, url.Values{"imdb": {id}}, &out)
	var fe *pluginsdk.FetchError
	if errors.As(err, &fe) && fe.IsNotFound() {
		return pluginapi.MarkersResponse{}, nil
	}
	if err != nil {
		return pluginapi.MarkersResponse{}, err
	}
	var resp pluginapi.MarkersResponse
	for _, found := range out.Markers {
		resp.Markers = append(resp.Markers, pluginapi.MarkerCandidate{
			Kind:       found.Kind,
			StartMs:    found.StartMs,
			EndMs:      found.EndMs,
			DurationMs: out.DurationMs,
		})
	}
	return resp, nil
}

// sdk-sample:end markers

// sdk-sample:begin onlinesource

// Videos is an Online source provider over a video site's JSON API at the
// operator's URL. It maps the site's shelves, search and streams onto the
// contract's rows, search and variants; it never fetches a media URL — it only
// names it, and the host does the playing.
type Videos struct {
	host pluginsdk.Host
}

// NewVideos builds the provider on a Host.
func NewVideos(h pluginsdk.Host) *Videos { return &Videos{host: h} }

// video is how the site spells one video.
type video struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Thumb   string `json:"thumb"`
	Seconds int64  `json:"seconds"`
}

func items(vs []video) []pluginapi.OnlineItem {
	var out []pluginapi.OnlineItem
	for _, v := range vs {
		out = append(out, pluginapi.OnlineItem{ID: v.ID, Title: v.Title, ThumbnailURL: v.Thumb, DurationMs: v.Seconds * 1000})
	}
	return out
}

// Rows answers the site's shelves, each with its first page and the cursor for the
// next.
func (v *Videos) Rows(ctx context.Context, _ pluginapi.OnlineRowsRequest) (pluginapi.OnlineRowsResponse, error) {
	var out struct {
		Shelves []struct {
			ID     string  `json:"id"`
			Title  string  `json:"title"`
			Videos []video `json:"videos"`
			More   string  `json:"more"`
		} `json:"shelves"`
	}
	if err := pluginsdk.GetJSON(ctx, v.host, baseOf(v.host.Settings().URL)+ShelvesPath, nil, &out); err != nil {
		return pluginapi.OnlineRowsResponse{}, err
	}
	var resp pluginapi.OnlineRowsResponse
	for _, s := range out.Shelves {
		resp.Rows = append(resp.Rows, pluginapi.OnlineRow{ID: s.ID, Label: s.Title, Items: items(s.Videos), NextCursor: s.More})
	}
	return resp, nil
}

// Row answers the page of one shelf after the cursor the last answer named.
func (v *Videos) Row(ctx context.Context, req pluginapi.OnlineRowRequest) (pluginapi.OnlineRowResponse, error) {
	var out struct {
		Videos []video `json:"videos"`
		More   string  `json:"more"`
	}
	q := url.Values{"shelf": {req.RowID}, "after": {req.Cursor}}
	if err := pluginsdk.GetJSON(ctx, v.host, baseOf(v.host.Settings().URL)+ShelfPath, q, &out); err != nil {
		return pluginapi.OnlineRowResponse{}, err
	}
	return pluginapi.OnlineRowResponse{Items: items(out.Videos), NextCursor: out.More}, nil
}

// Search answers the videos matching the query, best first, in one page.
func (v *Videos) Search(ctx context.Context, req pluginapi.OnlineSearchRequest) (pluginapi.OnlineSearchResponse, error) {
	var out struct {
		Videos []video `json:"videos"`
	}
	if err := pluginsdk.GetJSON(ctx, v.host, baseOf(v.host.Settings().URL)+FindPath, url.Values{"q": {req.Query}}, &out); err != nil {
		return pluginapi.OnlineSearchResponse{}, err
	}
	return pluginapi.OnlineSearchResponse{Items: items(out.Videos)}, nil
}

// Resolve answers the one muxed file the site streams the video from. A video the
// site no longer has (404) is no variants, not a failure.
func (v *Videos) Resolve(ctx context.Context, req pluginapi.OnlineResolveRequest) (pluginapi.OnlineResolveResponse, error) {
	var out struct {
		File   string `json:"file"`
		Height int    `json:"height"`
	}
	err := pluginsdk.GetJSON(ctx, v.host, baseOf(v.host.Settings().URL)+StreamPath, url.Values{"id": {req.ItemID}}, &out)
	var fe *pluginsdk.FetchError
	if errors.As(err, &fe) && fe.IsNotFound() {
		return pluginapi.OnlineResolveResponse{}, nil
	}
	if err != nil {
		return pluginapi.OnlineResolveResponse{}, err
	}
	return pluginapi.OnlineResolveResponse{Variants: []pluginapi.OnlineVariant{{
		Kind:       pluginapi.OnlineVariantMuxed,
		URL:        out.File,
		Container:  "mp4",
		Codecs:     []string{"h264", "aac"},
		Resolution: strconv.Itoa(out.Height) + "p",
	}}}, nil
}

// sdk-sample:end onlinesource

// sdk-sample:begin password

// Directory checks a username and password against an HTTP directory at the
// URL its operator typed into the `directory` setting — a host its manifest
// allows — and answers lookup(subject) between sign-ins.
type Directory struct {
	host pluginsdk.Host
}

// NewDirectory builds the provider on a Host.
func NewDirectory(h pluginsdk.Host) *Directory { return &Directory{host: h} }

// person is who the directory says someone is.
type person struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Groups   []string `json:"groups"`
	Disabled bool     `json:"disabled"`
}

func (d *Directory) base() string {
	s, _ := d.host.Settings().Values["directory"].(string)
	return baseOf(s)
}

// CheckPassword answers the person the directory names for a right password. A
// wrong one is a rejection — Accepted false, no error — and an error is the
// directory failing.
func (d *Directory) CheckPassword(ctx context.Context, req pluginapi.SignInPasswordRequest) (pluginapi.SignInPasswordResponse, error) {
	body, err := json.Marshal(map[string]string{"username": req.Username, "password": req.Password})
	if err != nil {
		return pluginapi.SignInPasswordResponse{}, err
	}
	var who person
	err = pluginsdk.DoJSON(ctx, d.host, pluginapi.FetchRequest{
		Method:  "POST",
		URL:     d.base() + "/login",
		Headers: []pluginapi.FetchHeader{pluginsdk.Header("Content-Type", "application/json")},
		Body:    body,
	}, &who)
	var fe *pluginsdk.FetchError
	if errors.As(err, &fe) && (fe.Status == 401 || fe.Status == 403) {
		return pluginapi.SignInPasswordResponse{}, nil
	}
	if err != nil {
		return pluginapi.SignInPasswordResponse{}, err
	}
	if who.ID == "" || who.Disabled {
		return pluginapi.SignInPasswordResponse{}, nil
	}
	return pluginapi.SignInPasswordResponse{
		Accepted: true,
		Identity: &pluginapi.SignInIdentity{Subject: who.ID, Username: who.Name, Groups: who.Groups},
	}, nil
}

// Lookup answers whether the person the directory once named is still there, and
// their groups now. Only a directory that says it no longer knows them is gone:
// every other failure is an error, which changes nothing but when the host asks
// again.
func (d *Directory) Lookup(ctx context.Context, req pluginapi.SignInLookupRequest) (pluginapi.SignInLookupResponse, error) {
	var who person
	err := pluginsdk.GetJSON(ctx, d.host, d.base()+"/users/"+url.PathEscape(req.Subject), nil, &who)
	var fe *pluginsdk.FetchError
	if errors.As(err, &fe) && fe.IsNotFound() {
		return pluginapi.SignInLookupResponse{Status: pluginapi.SignInGone}, nil
	}
	if err != nil {
		return pluginapi.SignInLookupResponse{}, err
	}
	if who.Disabled {
		return pluginapi.SignInLookupResponse{Status: pluginapi.SignInDisabled}, nil
	}
	return pluginapi.SignInLookupResponse{
		Status:   pluginapi.SignInActive,
		Identity: &pluginapi.SignInIdentity{Subject: req.Subject, Username: who.Name, Groups: who.Groups},
	}, nil
}

// sdk-sample:end password

var (
	_ pluginapi.WebReferenceProvider = References{}
	_ pluginapi.LyricProvider        = (*Lyrics)(nil)
	_ pluginapi.MarkerProvider       = (*Markers)(nil)
	_ pluginapi.SignInProvider       = (*Directory)(nil)
	_ pluginapi.SignInLookupProvider = (*Directory)(nil)
)
