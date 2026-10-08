// Package onlinesource is the host half of the Online source provider Extension
// point (ADR-0068): it lists the enabled sources, asks one for its rows, proxies
// an item's thumbnail, and plays an item by relaying a muxed variant's bytes.
//
// Everything here is live and in memory. An Online item has no Edition, no File and
// no identity the catalog knows, so nothing in this package touches the store: no
// progress, no watch state, no history, and nothing written to disk. Its Session is
// its own type, deliberately NOT a playback.Session — reusing the Title session
// would make "records no progress" a thing every future change to it must remember
// to preserve, instead of a thing this type cannot do.
//
// The judgment is the HOST's, and a Plugin is never asked whether its own answer is
// acceptable: an id outside a URL-safe set, an empty title or a thumbnail or media
// URL that is not https is dropped, and the upstream URL of a variant is kept in the
// Session and never leaves the Server. A client is handed a stream token.
package onlinesource

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/goozakdev/obelo-server/internal/playback"
	"github.com/goozakdev/obelo-server/internal/safefetch"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

var (
	// ErrNoSource: no enabled, working source has this id.
	ErrNoSource = errors.New("onlinesource: no such source")
	// ErrNoItem: the source has no such item, or never offered it.
	ErrNoItem = errors.New("onlinesource: no such item")
	// ErrUnavailable: the source (or the host serving its bytes) failed or refused.
	ErrUnavailable = errors.New("onlinesource: the source is not responding")
	// ErrNoSession: no live session has this id.
	ErrNoSession = errors.New("onlinesource: no such session")
)

const (
	// callTimeout bounds one rows() or resolve() call, the wait for the Plugin's
	// call slot included.
	callTimeout = 30 * time.Second
	// thumbnailMaxBytes bounds a proxied thumbnail; a source's poster is never a
	// megabytes-long file, and the bytes are held in memory for one response.
	thumbnailMaxBytes = 4 << 20
	// thumbnailTimeout bounds the whole thumbnail fetch.
	thumbnailTimeout = 20 * time.Second
	// maxIDLen bounds a row or item id.
	maxIDLen = 256
	// The caps on what a Plugin may hand a User. Over-length text is truncated to
	// the cap (counted in characters) and the entry kept; over-many rows or items
	// are cut to the cap.
	maxRows         = 50
	maxItemsPerRow  = 100
	maxPageItems    = 100
	maxLabelLen     = 100
	maxTitleLen     = 200
	maxDescLen      = 2000
	maxCursorLen    = 2048
	cacheTTL        = 5 * time.Minute
	maxCacheEntries = 512
	// maxThumbsPerSource bounds the item → thumbnail URL map of one source.
	// Four of the largest answers a Plugin may give, so one answer never evicts itself.
	maxThumbsPerSource = 4 * maxRows * maxItemsPerRow
)

// Source is one tile: an enabled source, by the name its Plugin gave it. HasIcon
// is true when its package carried an icon.png the Server can serve as the tile
// image; without one the client draws a generic tile showing Name.
type Source struct {
	ID      string
	Name    string
	HasIcon bool
}

// iconProvider is what a Plugin that carries a tile icon offers beyond the
// pluginapi contract. The icon lives in the Plugin's package, not in anything it
// answers, so a Built-in or a package without one simply does not implement it.
type iconProvider interface{ Icon() []byte }

// Item is an Online item as a client is shown it. The thumbnail is not here: the
// api layer points a client at the Server's own proxy for it.
type Item struct {
	ID          string
	Title       string
	DurationMs  int64
	Description string
	PublishedAt string
}

// Row is one labelled shelf of a source's page.
type Row struct {
	ID    string
	Label string
	Items []Item
	// NextCursor asks Row for the page after Items; empty when there is none.
	NextCursor string
}

// Page is one more page of a row.
type Page struct {
	Items      []Item
	NextCursor string
}

// Service lists sources, serves their pages and plays their items.
type Service struct {
	reg    *pluginapi.Registry
	client *http.Client

	mu       sync.Mutex
	sessions map[string]*Session
	// thumbs maps source → item → the https thumbnail URL the source's last pages
	// named. It is what lets the thumbnail route take an item id and no URL, so a
	// client can never aim the proxy at an address of its choosing. It outlives the
	// rows cache (a page left open still loads its images) and is held to
	// maxThumbsPerSource per source by evicting the least recently used; a restart
	// clears it, and the next page repopulates it.
	//
	// The same entry carries the item's title: what lets a session be labelled with
	// the title of the item it plays (the sessions page), though resolve() is asked
	// for an id alone. It shares the thumbnails' bound and refresh.
	thumbs map[string]*thumbLRU
	// cache holds rows() and row() answers for cacheTTL, keyed per source.
	cache map[cacheKey]cacheEntry
	onEnd func(sessionID string)
	now   func() time.Time

	// The first-URL judgment on a variant's media (check.go).
	transcoder  *Transcoder
	mediaAllow  func(sourceID, host string) bool
	lookup      func(ctx context.Context, host string) ([]net.IP, error)
	exemptAddrs map[string]bool
}

// New returns a Service over the sources registered in reg. client carries the
// upstream fetches (thumbnails and relayed media) and is wrapped in the https and
// private-address redirect policy; nil means a client with the default transport
// and no overall timeout, since a relayed stream outlives any fixed one.
func New(reg *pluginapi.Registry, client *http.Client) *Service {
	if client == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.ResponseHeaderTimeout = 30 * time.Second
		client = &http.Client{Transport: tr}
	}
	guarded := safefetch.Guard(client)
	guarded.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		// safefetch accepts a plain-http hop; an Online source's bytes never ride
		// one (ADR-0068 decision 3).
		if req.URL.Scheme != "https" {
			return safefetch.ErrRedirectBlocked
		}
		return safefetch.CheckRedirect(req, via)
	}
	return &Service{
		reg: reg, client: guarded,
		sessions: map[string]*Session{},
		thumbs:   map[string]*thumbLRU{},
		cache:    map[cacheKey]cacheEntry{},
		now:      time.Now,
		lookup:   lookupIPs,
	}
}

// SetOnEnd installs the callback fired with a session's id whenever it ends, by
// the client, the idle reaper or shutdown. The composition root uses it to revoke
// the session's stream tokens, as it does for a Title's.
func (s *Service) SetOnEnd(f func(sessionID string)) { s.onEnd = f }

// Sources lists the enabled sources in registration order, each as the tile its
// Plugin's name makes. It reads the registry and calls no Plugin: a source whose
// Plugin was switched off is not registered, and one that crashed refuses to be
// built, so neither has a tile.
func (s *Service) Sources() []Source {
	var out []Source
	for _, r := range s.reg.OnlineSourceProviders() {
		p, err := r.New(settingsFor(r))
		if err != nil {
			continue
		}
		name := r.Descriptor.Name
		if name == "" {
			name = r.Descriptor.Slug
		}
		out = append(out, Source{ID: r.Descriptor.Slug, Name: name, HasIcon: iconOf(p) != nil})
	}
	return out
}

// iconOf is the provider's tile icon, or nil when it has none.
func iconOf(p pluginapi.OnlineSourceProvider) []byte {
	if ip, ok := p.(iconProvider); ok {
		return ip.Icon()
	}
	return nil
}

// Icon returns the source's tile icon (PNG bytes). A source that is not enabled and
// working, or has no icon, is ErrNoSource: both are one 404 to a client. It calls no
// Plugin, only reads the file the install stored.
func (s *Service) Icon(sourceID string) ([]byte, error) {
	p, _, err := s.provider(sourceID)
	if err != nil {
		return nil, err
	}
	icon := iconOf(p)
	if icon == nil {
		return nil, ErrNoSource
	}
	return icon, nil
}

func settingsFor(r pluginapi.OnlineSourceProviderRegistration) pluginapi.Settings {
	return pluginapi.Settings{Enabled: true, URL: r.Descriptor.DefaultURL}
}

// provider builds the source's Plugin, or ErrNoSource for one that is not enabled
// and working.
func (s *Service) provider(sourceID string) (pluginapi.OnlineSourceProvider, pluginapi.OnlineSourceProviderRegistration, error) {
	r, ok := s.reg.OnlineSourceProvider(sourceID)
	if !ok {
		return nil, r, ErrNoSource
	}
	p, err := r.New(settingsFor(r))
	if err != nil {
		return nil, r, ErrNoSource
	}
	return p, r, nil
}

// cacheKey names one cached answer: a source's rows(), or one row() page. The
// source is always part of it, so one source's rows can never be served as
// another's.
type cacheKey struct {
	source, row, cursor string
	list                bool
}

type cacheEntry struct {
	rows    []Row
	page    Page
	expires time.Time
	// thumbs are the thumbnail URLs the answer's items named, so serving it from
	// the cache can refresh them.
	thumbs map[string]itemRef
}

// itemRef is what the host remembers of an item a page named: the thumbnail address
// the proxy fetches and the title a session of it is labelled with.
type itemRef struct {
	url, title string
}

// thumbEntry is one remembered item.
type thumbEntry struct {
	id string
	itemRef
}

// thumbLRU is one source's item → thumbnail URL map in least-recently-used order:
// the front is the most recently named or fetched.
type thumbLRU struct {
	items map[string]*list.Element
	order *list.List
}

// cached is the unexpired answer under key, if any.
func (s *Service) cached(key cacheKey) (cacheEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.cache[key]
	if !ok || !s.now().Before(e.expires) {
		return cacheEntry{}, false
	}
	return e, true
}

// remember keeps an answer for cacheTTL, along with the thumbnails its items named.
// Expired answers and thumbnails are pruned here, and the cache is held to
// maxCacheEntries by evicting the entry nearest to expiry.
func (s *Service) remember(key cacheKey, e cacheEntry, thumbs map[string]itemRef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	e.expires = now.Add(cacheTTL)
	for k, old := range s.cache {
		if !now.Before(old.expires) {
			delete(s.cache, k)
		}
	}
	if _, replacing := s.cache[key]; !replacing && len(s.cache) >= maxCacheEntries {
		var oldest cacheKey
		var oldestAt time.Time
		for k, old := range s.cache {
			if oldestAt.IsZero() || old.expires.Before(oldestAt) {
				oldest, oldestAt = k, old.expires
			}
		}
		delete(s.cache, oldest)
	}
	e.thumbs = thumbs
	s.cache[key] = e
	s.touchThumbsLocked(key.source, thumbs)
}

// touchThumbs registers or refreshes the thumbnails of an answer served from the
// cache, so a page that is still cached can always load its images.
func (s *Service) touchThumbs(source string, thumbs map[string]itemRef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touchThumbsLocked(source, thumbs)
}

// touchThumbsLocked moves every thumbnail of an answer to the front of the source's
// LRU, then evicts from the back. The cap is several whole answers, and the
// answer's own entries are at the front, so it can never evict itself.
func (s *Service) touchThumbsLocked(source string, thumbs map[string]itemRef) {
	l := s.thumbs[source]
	if l == nil {
		l = &thumbLRU{items: map[string]*list.Element{}, order: list.New()}
		s.thumbs[source] = l
	}
	for id, ref := range thumbs {
		if el, ok := l.items[id]; ok {
			el.Value = thumbEntry{id: id, itemRef: ref}
			l.order.MoveToFront(el)
		} else {
			l.items[id] = l.order.PushFront(thumbEntry{id: id, itemRef: ref})
		}
	}
	for l.order.Len() > maxThumbsPerSource {
		back := l.order.Back()
		delete(l.items, back.Value.(thumbEntry).id)
		l.order.Remove(back)
	}
}

// itemRefOf is what a source's page named for an item, marking it recently used.
func (s *Service) itemRefOf(source, item string) itemRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.thumbs[source]
	if l == nil {
		return itemRef{}
	}
	el, ok := l.items[item]
	if !ok {
		return itemRef{}
	}
	l.order.MoveToFront(el)
	return el.Value.(thumbEntry).itemRef
}

// thumbURL is the thumbnail address a source's page named for an item.
func (s *Service) thumbURL(source, item string) string { return s.itemRefOf(source, item).url }

// ClearCache forgets the cached rows of one source, or of every source for "", so
// the next page is asked of the Plugin afresh. The Plugin Manager calls it when a
// source's Admin settings are saved or its Plugin is installed, upgraded, enabled
// or removed. Thumbnail entries stay: an open page still needs them.
func (s *Service) ClearCache(sourceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.cache {
		if sourceID == "" || k.source == sourceID {
			delete(s.cache, k)
		}
	}
}

// Rows asks the source for its page and returns what survives the host's judgment.
// The answer is cached for five minutes per source and shared across Users. A
// Plugin that fails is ErrUnavailable.
func (s *Service) Rows(ctx context.Context, sourceID string) ([]Row, error) {
	p, _, err := s.provider(sourceID)
	if err != nil {
		return nil, err
	}
	key := cacheKey{source: sourceID, list: true}
	if e, ok := s.cached(key); ok {
		s.touchThumbs(sourceID, e.thumbs)
		return e.rows, nil
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	resp, err := p.Rows(ctx, pluginapi.OnlineRowsRequest{})
	if err != nil {
		log.Printf("obelo: online source %s: rows: %v", sourceID, err)
		return nil, ErrUnavailable
	}

	rows := make([]Row, 0, min(len(resp.Rows), maxRows))
	thumbs := map[string]itemRef{}
	seen := map[string]bool{}
	for _, r := range resp.Rows {
		if len(rows) == maxRows {
			break
		}
		if !urlSafeID(r.ID) || seen[r.ID] || strings.TrimSpace(r.Label) == "" {
			continue
		}
		seen[r.ID] = true
		rows = append(rows, Row{
			ID: r.ID, Label: truncate(r.Label, maxLabelLen),
			Items: cleanItems(r.Items, maxItemsPerRow, thumbs), NextCursor: cleanCursor(r.NextCursor),
		})
	}
	s.remember(key, cacheEntry{rows: rows}, thumbs)
	return rows, nil
}

// Row returns the page of one row after cursor, the token an earlier answer named.
// A row id or cursor the host would not have handed out is ErrNoItem, before any
// Plugin call. The answer is cached like Rows', per source, row and cursor.
func (s *Service) Row(ctx context.Context, sourceID, rowID, cursor string) (Page, error) {
	p, _, err := s.provider(sourceID)
	if err != nil {
		return Page{}, err
	}
	if !urlSafeID(rowID) || cursor == "" || len(cursor) > maxCursorLen {
		return Page{}, ErrNoItem
	}
	key := cacheKey{source: sourceID, row: rowID, cursor: cursor}
	if e, ok := s.cached(key); ok {
		s.touchThumbs(sourceID, e.thumbs)
		return e.page, nil
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	resp, err := p.Row(ctx, pluginapi.OnlineRowRequest{RowID: rowID, Cursor: cursor})
	if err != nil {
		log.Printf("obelo: online source %s: row %s: %v", sourceID, rowID, err)
		return Page{}, ErrUnavailable
	}
	thumbs := map[string]itemRef{}
	page := Page{Items: cleanItems(resp.Items, maxPageItems, thumbs), NextCursor: cleanCursor(resp.NextCursor)}
	s.remember(key, cacheEntry{page: page}, thumbs)
	return page, nil
}

// cleanItems is the host's judgment on a Plugin's items. Malformed ones are dropped
// (an id outside the URL-safe set or repeated in the row, no title, a thumbnail
// that is not https, a negative duration, a publish date that is not RFC 3339), the
// rest are kept up to limit with over-length text truncated, and the thumbnail URL
// of each kept item is recorded in thumbs. A very long duration is not malformed.
func cleanItems(in []pluginapi.OnlineItem, limit int, thumbs map[string]itemRef) []Item {
	out := make([]Item, 0, min(len(in), limit))
	seen := map[string]bool{}
	for _, it := range in {
		if len(out) == limit {
			break
		}
		if !urlSafeID(it.ID) || seen[it.ID] || strings.TrimSpace(it.Title) == "" ||
			!isHTTPS(it.ThumbnailURL) || it.DurationMs < 0 {
			continue
		}
		if it.PublishedAt != "" {
			if _, err := time.Parse(time.RFC3339, it.PublishedAt); err != nil {
				continue
			}
		}
		seen[it.ID] = true
		out = append(out, Item{
			ID: it.ID, Title: truncate(it.Title, maxTitleLen), DurationMs: it.DurationMs,
			Description: truncate(it.Description, maxDescLen), PublishedAt: it.PublishedAt,
		})
		thumbs[it.ID] = itemRef{url: it.ThumbnailURL, title: truncate(it.Title, maxTitleLen)}
	}
	return out
}

// cleanCursor passes a Plugin's cursor on, or ends the paging for one the host
// would not hand back.
func cleanCursor(c string) string {
	if len(c) > maxCursorLen {
		return ""
	}
	return c
}

// truncate cuts s to at most n characters.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// Thumbnail fetches an item's thumbnail from its source through the guarded client
// and returns the bytes and their content type. Nothing is written to disk. An
// item the source's page never offered is ErrNoItem; anything that is not a small
// raster image is ErrUnavailable.
func (s *Service) Thumbnail(ctx context.Context, sourceID, itemID string) ([]byte, string, error) {
	if _, _, err := s.provider(sourceID); err != nil {
		return nil, "", err
	}
	target := s.thumbURL(sourceID, itemID)
	if target == "" {
		return nil, "", ErrNoItem
	}
	ctx, cancel := context.WithTimeout(ctx, thumbnailTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, "", ErrUnavailable
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, "", ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", ErrUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, thumbnailMaxBytes+1))
	if err != nil || len(body) > thumbnailMaxBytes {
		return nil, "", ErrUnavailable
	}
	// The type is what the bytes are, not what the source says they are: an
	// answer that is not an image (HTML, SVG, which can carry script) is refused.
	ct := http.DetectContentType(body)
	if !strings.HasPrefix(ct, "image/") {
		return nil, "", ErrUnavailable
	}
	return body, ct, nil
}

// Session is one live play of an Online item. It has no Edition, no File and no
// progress: only who is watching, what, and where the bytes come from.
type Session struct {
	ID       string
	UserID   string
	SourceID string
	ItemID   string
	// SourceName and ItemTitle are what the sessions page calls the play: the
	// source's name and the item's title as its page last showed it (the id when it
	// never did).
	SourceName string
	ItemTitle  string
	// Variant is what is played. Its URLs are the upstream addresses and never leave
	// the Server.
	Variant pluginapi.OnlineVariant
	// Transcoded is true when ffmpeg encodes the variant into HLS (C), false when its
	// bytes are relayed (B).
	Transcoded bool
	StartedAt  time.Time
	LastSeen   time.Time

	// run is the ffmpeg encode of a transcoded session, nil for a relay.
	run *run
}

// PlayInput is one request to play an item.
type PlayInput struct {
	UserID, SourceID, ItemID string
	Profile                  playback.DeviceProfile
	// Constraints are already clamped to the User's Playback ceiling.
	Constraints playback.Constraints
}

// Play resolves the item once, chooses a variant the client can play as-is under
// the constraints and opens a session for it. A variant the client cannot play, or
// one over the ceiling, comes back as *playback.Unsupported: the same "a transcode
// would be required" outcome a Title reports.
func (s *Service) Play(ctx context.Context, in PlayInput) (Session, *playback.Unsupported, error) {
	if !urlSafeID(in.ItemID) {
		return Session{}, nil, ErrNoItem
	}
	p, reg, err := s.provider(in.SourceID)
	if err != nil {
		return Session{}, nil, err
	}
	rctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	resp, err := p.Resolve(rctx, pluginapi.OnlineResolveRequest{
		ItemID: in.ItemID,
		Hints:  pluginapi.OnlineHints{MaxHeight: playback.ResolutionHeight(in.Constraints.MaxResolution)},
	})
	if err != nil {
		log.Printf("obelo: online source %s: resolve %s: %v", in.SourceID, in.ItemID, err)
		return Session{}, nil, ErrUnavailable
	}
	if len(resp.Variants) == 0 {
		return Session{}, nil, ErrNoItem
	}
	// The host's judgment on the first URL of every variant, for both paths: one the
	// Plugin could not have meant for the Server to fetch is not a variant.
	var variants []pluginapi.OnlineVariant
	for _, v := range resp.Variants {
		if err := s.checkVariant(ctx, in.SourceID, v); err != nil {
			log.Printf("obelo: online source %s: item %s: variant dropped: %v", in.SourceID, in.ItemID, err)
			continue
		}
		variants = append(variants, v)
	}
	if len(variants) == 0 {
		return Session{}, nil, ErrUnavailable
	}
	plan, unsup := playback.PlanOnline(in.Profile, in.Constraints, variants, s.transcoder != nil)
	if unsup != nil {
		return Session{}, unsup, nil
	}
	id := uuid.NewString()
	var r *run
	if plan.Transcode {
		if r, err = s.startEncode(id, plan.Variant, plan); err != nil {
			return Session{}, nil, err
		}
	}
	now := s.now()
	sourceName := reg.Descriptor.Name
	if sourceName == "" {
		sourceName = reg.Descriptor.Slug
	}
	itemTitle := s.itemRefOf(in.SourceID, in.ItemID).title
	if itemTitle == "" {
		itemTitle = in.ItemID
	}
	sess := &Session{
		ID: id, UserID: in.UserID, SourceID: in.SourceID, ItemID: in.ItemID,
		SourceName: sourceName, ItemTitle: itemTitle,
		Variant: plan.Variant, Transcoded: plan.Transcode, StartedAt: now, LastSeen: now, run: r,
	}
	s.mu.Lock()
	s.sessions[sess.ID] = sess
	s.mu.Unlock()
	if r != nil {
		go s.watch(sess.ID, r)
	}
	return *sess, nil, nil
}

// Session returns a live session by id.
func (s *Service) Session(id string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return Session{}, false
	}
	return *sess, true
}

// Sessions lists the live sessions, oldest first.
func (s *Service) Sessions() []Session {
	s.mu.Lock()
	out := make([]Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		out = append(out, *sess)
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if !out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].StartedAt.Before(out[j].StartedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Touch marks a session as just used, which keeps the idle reaper off it.
func (s *Service) Touch(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[id]; ok {
		sess.LastSeen = s.now()
	}
}

// End ends a session, reporting whether there was one.
func (s *Service) End(id string) bool {
	s.mu.Lock()
	sess, ok := s.sessions[id]
	delete(s.sessions, id)
	s.mu.Unlock()
	if ok && sess.run != nil {
		sess.run.stop()
	}
	if ok && s.onEnd != nil {
		s.onEnd(id)
	}
	return ok
}

// Count is how many sessions are live.
func (s *Service) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// Reap ends every session idle for longer than idle and returns how many.
func (s *Service) Reap(idle time.Duration) int {
	cutoff := s.now().Add(-idle)
	var stale []string
	s.mu.Lock()
	for id, sess := range s.sessions {
		if sess.LastSeen.Before(cutoff) {
			stale = append(stale, id)
		}
	}
	s.mu.Unlock()
	for _, id := range stale {
		s.End(id)
	}
	return len(stale)
}

// EndAll ends every session, for shutdown.
func (s *Service) EndAll() int {
	s.mu.Lock()
	ids := make([]string, 0, len(s.sessions))
	for id := range s.sessions {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.End(id)
	}
	return len(ids)
}

// relayRequestHeaders are the only headers a Plugin may have the relay send: the
// ones a media host asks for to serve a browser-like request. Anything else a
// Plugin names (Authorization, Cookie, Host) is not forwarded.
var relayRequestHeaders = map[string]bool{
	"referer": true, "user-agent": true, "origin": true, "accept-language": true,
}

// OpenMedia opens the session's upstream bytes for a client request, forwarding its
// Range. The fetch goes through the guarded client, so every redirect hop is
// checked to be https and not to lead to a private or loopback address. The caller
// owns closing the body.
//
// Only a 200, 206 or 416 is passed on; any other answer from the media host is
// ErrUnavailable, so a client never sees an upstream error page.
func (s *Service) OpenMedia(ctx context.Context, sess Session, rangeHeader, ifRange string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sess.Variant.URL, nil)
	if err != nil {
		return nil, ErrUnavailable
	}
	for k, v := range sess.Variant.Headers {
		if relayRequestHeaders[strings.ToLower(k)] {
			req.Header.Set(k, v)
		}
	}
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
		if ifRange != "" {
			req.Header.Set("If-Range", ifRange)
		}
	}
	resp, err := s.client.Do(req)
	if err != nil {
		log.Printf("obelo: online source %s: relaying %s: %v", sess.SourceID, sess.ItemID, redactURLError(err))
		return nil, ErrUnavailable
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent, http.StatusRequestedRangeNotSatisfiable:
		return resp, nil
	}
	resp.Body.Close()
	return nil, fmt.Errorf("%w: media host answered %d", ErrUnavailable, resp.StatusCode)
}

// redactURLError drops the URL a *url.Error carries, so the upstream address (which
// may hold a Plugin's token) is not written to the log.
func redactURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// urlSafeID reports whether id is one the host will put in a URL path: letters,
// digits and . _ ~ -, not empty, not "." or "..", and not absurdly long.
func urlSafeID(id string) bool {
	if id == "" || id == "." || id == ".." || len(id) > maxIDLen {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '.', c == '_', c == '~', c == '-':
		default:
			return false
		}
	}
	return true
}

// isHTTPS reports whether raw is an absolute https URL with a host.
func isHTTPS(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != ""
}
