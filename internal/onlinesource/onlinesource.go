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
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
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
}

// Service lists sources, serves their pages and plays their items.
type Service struct {
	reg    *pluginapi.Registry
	client *http.Client

	mu       sync.Mutex
	sessions map[string]*Session
	// thumbs maps source → item → the https thumbnail URL the source's last page
	// named. It is what lets the thumbnail route take an item id and no URL, so a
	// client can never aim the proxy at an address of its choosing. A restart
	// clears it, and the next page repopulates it.
	thumbs map[string]map[string]string
	onEnd  func(sessionID string)
	now    func() time.Time
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
		thumbs:   map[string]map[string]string{},
		now:      time.Now,
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

// Rows asks the source for its page and returns what survives the host's judgment.
// A Plugin that fails is ErrUnavailable.
func (s *Service) Rows(ctx context.Context, sourceID string) ([]Row, error) {
	p, _, err := s.provider(sourceID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	resp, err := p.Rows(ctx, pluginapi.OnlineRowsRequest{})
	if err != nil {
		log.Printf("obelo: online source %s: rows: %v", sourceID, err)
		return nil, ErrUnavailable
	}

	rows := make([]Row, 0, len(resp.Rows))
	thumbs := map[string]string{}
	for _, r := range resp.Rows {
		if !urlSafeID(r.ID) || strings.TrimSpace(r.Label) == "" {
			continue
		}
		row := Row{ID: r.ID, Label: r.Label, Items: []Item{}}
		for _, it := range r.Items {
			if !urlSafeID(it.ID) || strings.TrimSpace(it.Title) == "" || !isHTTPS(it.ThumbnailURL) {
				continue
			}
			row.Items = append(row.Items, Item{
				ID: it.ID, Title: it.Title, DurationMs: it.DurationMs,
				Description: it.Description, PublishedAt: it.PublishedAt,
			})
			thumbs[it.ID] = it.ThumbnailURL
		}
		rows = append(rows, row)
	}
	s.mu.Lock()
	for id, u := range thumbs {
		if s.thumbs[sourceID] == nil {
			s.thumbs[sourceID] = map[string]string{}
		}
		s.thumbs[sourceID][id] = u
	}
	s.mu.Unlock()
	return rows, nil
}

// Thumbnail fetches an item's thumbnail from its source through the guarded client
// and returns the bytes and their content type. Nothing is written to disk. An
// item the source's page never offered is ErrNoItem; anything that is not a small
// raster image is ErrUnavailable.
func (s *Service) Thumbnail(ctx context.Context, sourceID, itemID string) ([]byte, string, error) {
	if _, _, err := s.provider(sourceID); err != nil {
		return nil, "", err
	}
	s.mu.Lock()
	target := s.thumbs[sourceID][itemID]
	s.mu.Unlock()
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
	// Variant is what is relayed. Its URL is the upstream address and never leaves
	// the Server.
	Variant   pluginapi.OnlineVariant
	StartedAt time.Time
	LastSeen  time.Time
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
	p, _, err := s.provider(in.SourceID)
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
	var variants []pluginapi.OnlineVariant
	for _, v := range resp.Variants {
		if isHTTPS(v.URL) {
			variants = append(variants, v)
		}
	}
	if len(variants) == 0 {
		return Session{}, nil, ErrNoItem
	}
	chosen, unsup := playback.ChooseOnlineVariant(in.Profile, in.Constraints, variants)
	if unsup != nil {
		return Session{}, unsup, nil
	}
	now := s.now()
	sess := &Session{
		ID: uuid.NewString(), UserID: in.UserID, SourceID: in.SourceID, ItemID: in.ItemID,
		Variant: chosen, StartedAt: now, LastSeen: now,
	}
	s.mu.Lock()
	s.sessions[sess.ID] = sess
	s.mu.Unlock()
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
	_, ok := s.sessions[id]
	delete(s.sessions, id)
	s.mu.Unlock()
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
