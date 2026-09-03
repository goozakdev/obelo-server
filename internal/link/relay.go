package link

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goozakdev/obelo-server/internal/playback"
	"github.com/goozakdev/obelo-server/internal/store"
)

// The one-hop relay (ADR-0056 §5): the home side of playing somebody else's file.
//
// Three things happen here and nothing else does.
//
//  1. A negotiation is FORWARDED. The client's Capability profile goes to the
//     sharer under the Link's credential, the sharer tiers it under its own
//     governance and its `remote` User's Playback ceiling, and its answer comes
//     back to be re-served with the ids and URLs rewritten. This Server decides
//     nothing about quality and re-encodes nothing.
//  2. Bytes are CARRIED. Every media artifact the answer named is fetched under
//     the same credential and streamed straight through, Range and all.
//  3. Artwork is FETCHED ONCE and cached, because a poster is asked for on every
//     grid paint and the sharer should be asked once.
//
// What never crosses the wire is who is watching (ADR-0054 §3). The requests
// below carry the Link's bearer, a Title id and a Capability profile — no User
// id, no username, no Device name — and that absence is asserted by a test.

// RelayStore is the catalog read the relay needs: which Libraries are mirrors,
// and the translation between this Server's ids and the sharer's (store/relay.go).
// *store.DB satisfies it.
//
// It is type-asserted off the mirror rather than being its own Option, in the
// same optional-capability style playback uses for its memory stores: a Service
// whose mirror cannot answer these simply relays nothing, which is exactly what a
// Server with no mirror should do.
type RelayStore interface {
	LibraryByID(id string) (store.Library, error)
	LibraryOfTitle(id string) (string, error)
	LibraryOfEntity(entityType, id string) (string, error)
	RemoteIDOf(kind, id string) (string, error)
	MirrorStamp(kind, id string) (string, error)
}

// ErrNotRelayed is an entity that does not live in a mirror — the caller asked
// the relay about one of this Server's own Titles. It is never rendered: every
// caller uses it to fall back to the local path.
var ErrNotRelayed = errors.New("link: this entity is not mirrored from another server")

// ErrArtworkAbsent is the sharer answering 404 for an image. The mirrored entity
// simply has no artwork over there, which is a 404 here too — not a link fault.
var ErrArtworkAbsent = errors.New("link: the sharing server has no artwork for this entity")

// RemoteRefusal is a refusal the SHARER gave that this Server has no better
// answer to, carried whole so the api layer can re-serve it verbatim: a
// SERVER_BUSY with its suggestedMaxBitrate (ADR-0009, retried at a lower quality
// by a client that never learns which machine was busy), a STREAM_LIMIT with its
// counts (ADR-0054 §2), a TRANSCODE_REQUIRED with its reason, a 404.
//
// Passing it through rather than collapsing it to "the other server said no" is
// the whole of ADR-0056 §5's "answers pass through verbatim": the client's next
// move — retry at half the bitrate, stop another stream, give up — is decided by
// this envelope, and the home Server knows nothing that would improve it.
type RemoteRefusal struct {
	Status  int
	Code    string
	Message string
	Details map[string]any
}

func (e *RemoteRefusal) Error() string {
	return fmt.Sprintf("link: the sharing server refused playback (%d %s)", e.Status, e.Code)
}

// maxRelayPlaylistBytes bounds a playlist read into memory. Media SEGMENTS are
// streamed and never buffered; a playlist is rewritten, so it is read whole, and
// 4 MiB is orders of magnitude past the longest VOD playlist a feature-length
// file produces.
const maxRelayPlaylistBytes = 4 << 20

// maxRelayArtworkBytes mirrors the artwork cache's own 16 MiB cap (ADR-0026), so
// a peer cannot fill this disk through the poster route.
const maxRelayArtworkBytes = 16 << 20

// relayStore returns the catalog reads, or nil on a Service with no mirror.
func (s *Service) relay() RelayStore { return s.relayCatalog }

// RelaysLibrary reports whether a Library's Titles play through this relay
// (playback.Relayer). It fails CLOSED — an unreadable answer is "no" — because
// saying yes about a local Library would send a play to a Server that does not
// have the file.
func (s *Service) RelaysLibrary(libraryID string) bool {
	rs := s.relay()
	if rs == nil || libraryID == "" {
		return false
	}
	lib, err := rs.LibraryByID(libraryID)
	return err == nil && lib.Linked() && lib.LinkID != ""
}

// linkForLibrary resolves the Link a mirrored Library came over.
func (s *Service) linkForLibrary(libraryID string) (store.Link, error) {
	rs := s.relay()
	if rs == nil {
		return store.Link{}, ErrNotRelayed
	}
	lib, err := rs.LibraryByID(libraryID)
	if err != nil {
		return store.Link{}, ErrNotRelayed
	}
	if !lib.Linked() || lib.LinkID == "" {
		return store.Link{}, ErrNotRelayed
	}
	return s.store.LinkByID(lib.LinkID)
}

// RelayNegotiate forwards one negotiation to the sharer (playback.Relayer).
func (s *Service) RelayNegotiate(ctx context.Context, req playback.RelayRequest) (playback.RelayAnswer, error) {
	rs := s.relay()
	if rs == nil {
		return playback.RelayAnswer{}, ErrNotRelayed
	}
	l, err := s.linkForLibrary(req.LibraryID)
	if err != nil {
		return playback.RelayAnswer{}, err
	}
	// A Link already known dead is not dialed. The credential cannot come back on
	// its own — only a fresh invite re-keys it (ADR-0056 §6) — so asking again
	// costs a round trip to be told the same thing, in front of somebody who
	// pressed play.
	if l.State == store.LinkStateRevoked || l.Token == "" {
		return playback.RelayAnswer{}, ErrCredentialDead
	}

	remoteTitle, err := rs.RemoteIDOf(store.ExportTitle, req.TitleID)
	if err != nil {
		return playback.RelayAnswer{}, ErrNotRelayed
	}
	body := relayNegotiationBody(req, relayRemoteID(rs, store.ExportEdition, req.EditionID),
		relayRemoteID(rs, store.ExportStream, req.AudioStreamID),
		relayRemoteID(rs, store.ExportStream, req.VideoStreamID))

	path := apiPrefix + "/titles/" + url.PathEscape(remoteTitle) + "/playback"
	answer, err := s.postNegotiation(ctx, l, path, body)
	s.recordRelay(l, err)
	if err != nil {
		return playback.RelayAnswer{}, err
	}
	answer.LinkID = l.ID
	answer.RemoteTitleID = remoteTitle
	return answer, nil
}

// relayRemoteID maps one optional local id onto the sharer's, dropping it when
// this side cannot resolve it. An unresolvable id is dropped rather than sent
// through as-is: the sharer would answer 404 for an id it never issued, which
// reads to the viewer as "this title is gone" when the truth is "the pick you
// made no longer exists here".
func relayRemoteID(rs RelayStore, kind, localID string) string {
	if localID == "" {
		return ""
	}
	remote, err := rs.RemoteIDOf(kind, localID)
	if err != nil {
		return ""
	}
	return remote
}

// relayNegotiationBody is the negotiation request as the sharer's
// POST /titles/{id}/playback expects it (api-contract §3.6).
//
// It is written out longhand — the encoder to the api package's decoder — for
// export.go's reason: this is the one place this Server decides what it tells
// another household about a play, and the answer must be readable in one screen.
// Everything here came from the client's own Capability profile; NOTHING here
// identifies the person watching (ADR-0054 §3).
func relayNegotiationBody(req playback.RelayRequest, editionID, audioID, videoID string) map[string]any {
	codecs := make([]map[string]any, 0, len(req.Profile.VideoCodecs))
	for _, c := range req.Profile.VideoCodecs {
		codecs = append(codecs, map[string]any{
			"codec":         c.Codec,
			"maxLevel":      c.MaxLevel,
			"maxResolution": c.MaxResolution,
			"hdr":           c.HDR,
		})
	}
	return map[string]any{
		"deviceProfile": map[string]any{
			"containers":          req.Profile.Containers,
			"videoCodecs":         codecs,
			"audioCodecs":         req.Profile.AudioCodecs,
			"maxAudioChannels":    req.Profile.MaxAudioChannels,
			"textSubtitleFormats": req.Profile.TextSubtitleFormats,
			"hevcInMpegts":        req.Profile.HevcInMpegTS,
		},
		"constraints": map[string]any{
			"maxBitrate":            req.Constraints.MaxBitrate,
			"maxResolution":         req.Constraints.MaxResolution,
			"preferredAudioLang":    req.Constraints.PreferredAudioLang,
			"preferredSubtitleLang": req.Constraints.PreferredSubtitleLang,
		},
		"startPosition":     req.StartPosition,
		"editionId":         editionID,
		"burnSubtitleId":    req.BurnSubtitleID,
		"audioStreamId":     audioID,
		"videoStreamId":     videoID,
		"remuxSelectedOnly": req.RemuxSelectedOnly,
	}
}

// postNegotiation spends one negotiation against whichever address answers.
//
// The origin walk is the sweep's (originsFor): a household that rebooted its
// router between the last sync and this play should not be told its friend is
// gone. An address that answers is remembered, so the media fetches that follow
// go straight there.
func (s *Service) postNegotiation(ctx context.Context, l store.Link, path string, body map[string]any) (playback.RelayAnswer, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return playback.RelayAnswer{}, err
	}
	client := s.client()
	var lastErr error
	for _, origin := range originsFor(l) {
		answer, err := s.negotiateAt(ctx, client, l, origin, path, raw)
		switch {
		case err == nil:
			if origin != l.ActiveOrigin {
				if serr := s.store.SetLinkActiveOrigin(l.ID, origin); serr != nil {
					log.Printf("obelo: link: recording the address of %q: %v", l.ServerName, serr)
				}
			}
			return answer, nil
		case errors.Is(err, ErrCredentialDead):
			return playback.RelayAnswer{}, err
		default:
			var refusal *RemoteRefusal
			if errors.As(err, &refusal) {
				// The sharer answered — this is its decision, not an unreachable address.
				return playback.RelayAnswer{}, err
			}
			lastErr = fmt.Errorf("%s: %w", origin, err)
		}
	}
	if lastErr != nil {
		return playback.RelayAnswer{}, fmt.Errorf("%w (%v)", ErrUnreachable, lastErr)
	}
	return playback.RelayAnswer{}, ErrUnreachable
}

func (s *Service) negotiateAt(ctx context.Context, client *http.Client, l store.Link, origin, path string, raw []byte) (playback.RelayAnswer, error) {
	ctx, cancel := context.WithTimeout(ctx, s.callTimeout())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+path, bytes.NewReader(raw))
	if err != nil {
		return playback.RelayAnswer{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+l.Token)

	resp, err := client.Do(req)
	if err != nil {
		return playback.RelayAnswer{}, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer closeBody(resp)

	if resp.StatusCode == http.StatusUnauthorized {
		return playback.RelayAnswer{}, ErrCredentialDead
	}
	if resp.StatusCode != http.StatusOK {
		return playback.RelayAnswer{}, relayRefusal(resp)
	}
	var decision map[string]any
	if err := decodeBody(resp, &decision); err != nil {
		return playback.RelayAnswer{}, fmt.Errorf("%w: %v", ErrNotObelo, err)
	}
	sessionID, _ := decision["sessionId"].(string)
	if sessionID == "" {
		return playback.RelayAnswer{}, fmt.Errorf("%w: it answered a decision with no session", ErrNotObelo)
	}
	tier, _ := decision["tier"].(string)
	answer := playback.RelayAnswer{
		RemoteSessionID: sessionID,
		Tier:            playback.Tier(tier),
		Decision:        decision,
	}
	if ed, ok := decision["edition"].(map[string]any); ok {
		answer.RemoteEditionID, _ = ed["id"].(string)
	}
	return answer, nil
}

// relayRefusal reads the sharer's error envelope into a RemoteRefusal. A body
// that is not an envelope still yields one, carrying the status — the client
// gets a truthful refusal either way.
func relayRefusal(resp *http.Response) error {
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	_ = decodeBody(resp, &env)
	return &RemoteRefusal{
		Status:  resp.StatusCode,
		Code:    env.Error.Code,
		Message: env.Error.Message,
		Details: env.Error.Details,
	}
}

// recordRelay feeds a relay call's outcome into the ADR-0056 §6 state machine —
// "the last export OR RELAY call succeeded" — through the sweep's own writer, so
// a friend whose server went down mid-film has their Libraries badged unavailable
// without waiting for the next hourly sweep.
//
// A SUCCESS is the one difference from Service.record: it moves an unreachable
// Link back to connected but does NOT stamp last_synced_at, because a play proves
// the sharer is reachable and says nothing at all about when this Server last
// pulled its catalog. Stamping it there would make "last sync" mean "last
// anything", and the admin page would report a mirror as fresh because somebody
// watched an old episode.
func (s *Service) recordRelay(l store.Link, err error) {
	if err == nil {
		if l.State == store.LinkStateConnected {
			return
		}
		if serr := s.store.SetLinkState(l.ID, store.LinkStateConnected, ""); serr != nil {
			log.Printf("obelo: link: recording the state of %q: %v", l.ServerName, serr)
			return
		}
		log.Printf("obelo: link: %q answered a play; it is reachable again", l.ServerName)
		s.publishLinkState(l.ID)
		return
	}
	// A refusal is the sharer at its healthiest: it answered. Only transport
	// failures and a dead credential say anything about the Link.
	var refusal *RemoteRefusal
	if errors.As(err, &refusal) {
		s.recordRelay(l, nil)
		return
	}
	s.record(l, err)
}

// --- carrying the bytes -------------------------------------------------------

// RelayFetch performs one media fetch against the sharer for a live relay session
// and hands back the LIVE response: the caller copies the status, the headers
// that matter and the body straight to its own client, and closes it. Nothing is
// buffered to disk, and nothing is buffered in memory except the playlists the
// caller chooses to rewrite.
//
// path is the sharer's own API path tail (e.g. "sessions/{id}/hls/000.ts"), which
// the caller has already validated as belonging to this session. header carries
// the client's conditional/range headers forward, because a seek on a relayed
// direct play is a byte range like any other.
func (s *Service) RelayFetch(ctx context.Context, linkID, method, path string, header http.Header) (*http.Response, error) {
	l, err := s.store.LinkByID(linkID)
	if err != nil {
		return nil, err
	}
	if l.Token == "" {
		return nil, ErrCredentialDead
	}
	origin := l.ActiveOrigin
	if origin == "" {
		if origins := originsFor(l); len(origins) > 0 {
			origin = origins[0]
		}
	}
	if origin == "" {
		return nil, ErrUnreachable
	}
	if method != http.MethodHead {
		method = http.MethodGet
	}

	req, err := http.NewRequestWithContext(ctx, method, origin+apiPrefix+"/"+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+l.Token)
	for _, h := range relayRequestHeaders {
		if v := header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}

	// The STREAM client, not the request/response one: a segment or a progressive
	// range can legitimately take longer than a whole-call timeout meant for JSON,
	// and cutting a film off after twenty seconds would be a bug that only shows up
	// on a slow link. The connect/handshake/response-header bounds still apply.
	resp, err := s.dialer.StreamClient().Do(req)
	if err != nil {
		s.recordRelay(l, fmt.Errorf("%w: %v", ErrUnreachable, err))
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		closeBody(resp)
		s.recordRelay(l, ErrCredentialDead)
		return nil, ErrCredentialDead
	}
	s.recordRelay(l, nil)
	return resp, nil
}

// relayRequestHeaders are the client request headers carried to the sharer. It is
// an allowlist, not a copy: everything a browser or a television sends that is
// not on this list — cookies, user agents, anything naming the viewer — stops
// here (ADR-0054 §3).
var relayRequestHeaders = []string{"Range", "If-Range", "If-None-Match", "If-Modified-Since"}

// RelayEndSession ends the sharer's session when the local one ends (a clean
// DELETE, or the idle reaper). Best effort and logged: this side has already let
// go, and the sharer's own reaper is the backstop.
func (s *Service) RelayEndSession(ctx context.Context, linkID, remoteSessionID string) error {
	if linkID == "" || remoteSessionID == "" {
		return nil
	}
	l, err := s.store.LinkByID(linkID)
	if err != nil {
		return err
	}
	if l.ActiveOrigin == "" || l.Token == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, s.callTimeout())
	defer cancel()
	return s.call(ctx, s.client(), http.MethodDelete,
		l.ActiveOrigin+apiPrefix+"/sessions/"+url.PathEscape(remoteSessionID), l.Token)
}

// --- artwork ------------------------------------------------------------------

// RelayArtwork returns the on-disk path of a mirrored entity's artwork, fetching
// it from the sharer on the first request and caching it (ADR-0056 §5).
//
// The cache is the app's own identity-keyed artwork cache (ADR-0007/ADR-0026),
// under a name built from the Link and the SHARER's id for the entity, so it is
// stable across a re-pull (which mints no new remote ids) and is removed with the
// data directory like every other cached image. A cached file older than the last
// change this Server applied to the row is re-fetched, which is how a poster the
// friend replaced arrives here.
func (s *Service) RelayArtwork(ctx context.Context, kind, entityID, role string) (string, error) {
	rs := s.relay()
	if rs == nil || s.artworkDir == "" || entityID == "" || role == "" {
		return "", ErrNotRelayed
	}
	libraryID, err := relayLibraryOf(rs, kind, entityID)
	if err != nil {
		return "", ErrNotRelayed
	}
	l, err := s.linkForLibrary(libraryID)
	if err != nil {
		return "", err
	}
	remoteID, err := rs.RemoteIDOf(kind, entityID)
	if err != nil {
		return "", ErrNotRelayed
	}

	base := "linked-" + l.ID + "-" + remoteID + "-" + role
	if cached, ok := cachedArtwork(s.artworkDir, base, relayStamp(rs, kind, entityID)); ok {
		return cached, nil
	}
	if l.State == store.LinkStateRevoked || l.Token == "" {
		return "", ErrCredentialDead
	}

	path, err := relayArtworkPath(kind, remoteID, role)
	if err != nil {
		return "", err
	}
	resp, err := s.RelayFetch(ctx, l.ID, http.MethodGet, path, http.Header{})
	if err != nil {
		return "", err
	}
	defer closeBody(resp)
	if resp.StatusCode == http.StatusNotFound {
		return "", ErrArtworkAbsent
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: %s answered %d for artwork", ErrUnreachable, l.ServerName, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRelayArtworkBytes))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	return writeCachedArtwork(s.artworkDir, base, resp.Header.Get("Content-Type"), data)
}

// relayLibraryOf resolves the Library behind a mirrored entity id.
func relayLibraryOf(rs RelayStore, kind, entityID string) (string, error) {
	switch kind {
	case store.ExportTitle, store.ExportEpisode, store.ExportTrack:
		return rs.LibraryOfTitle(entityID)
	default:
		return rs.LibraryOfEntity(kind, entityID)
	}
}

// relayArtworkPath is the sharer's own artwork route for an entity kind. The
// Album's is the odd one out — a single cover, no role in the path — exactly as
// the contract has it.
func relayArtworkPath(kind, remoteID, role string) (string, error) {
	id := url.PathEscape(remoteID)
	r := url.PathEscape(role)
	switch kind {
	case store.ExportTitle, store.ExportEpisode, store.ExportTrack:
		return "titles/" + id + "/artwork/" + r, nil
	case store.EntityShow:
		return "shows/" + id + "/artwork/" + r, nil
	case store.EntitySeason:
		return "seasons/" + id + "/artwork/" + r, nil
	case store.EntityArtist:
		return "artists/" + id + "/artwork/" + r, nil
	case store.EntityAlbum:
		return "albums/" + id + "/artwork", nil
	default:
		return "", ErrNotRelayed
	}
}

// relayStamp is the mirrored row's last-applied instant, or the zero time when
// the store cannot say — which makes a cached file valid forever rather than
// re-fetched forever.
func relayStamp(rs RelayStore, kind, entityID string) time.Time {
	raw, err := rs.MirrorStamp(kind, entityID)
	if err != nil || raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		if t, err = time.Parse("2006-01-02T15:04:05.000Z07:00", raw); err != nil {
			return time.Time{}
		}
	}
	return t
}

// relayArtworkExtensions are the extensions a cached relayed image can wear. The
// set is the artwork cache's own (ADR-0026 accepts JPEG/PNG/WebP), and the lookup
// probes them rather than recording the extension anywhere: the name is derived
// from ids that never change, so three stats answer "have I already fetched
// this?" with no state to keep in step.
var relayArtworkExtensions = []string{".jpg", ".png", ".webp"}

// cachedArtwork returns the cached file for a base name when one exists and is at
// least as new as the row it belongs to.
func cachedArtwork(dir, base string, stamp time.Time) (string, bool) {
	for _, ext := range relayArtworkExtensions {
		path := filepath.Join(dir, base+ext)
		info, err := os.Stat(path)
		if err != nil || info.Size() == 0 {
			continue
		}
		if !stamp.IsZero() && info.ModTime().Before(stamp) {
			// The mirror has applied a change to this entity since these bytes were
			// fetched — the poster may be one of the things that changed.
			return "", false
		}
		return path, true
	}
	return "", false
}

// writeCachedArtwork stores the fetched bytes and returns the file's path. It
// overwrites in place (the name is deterministic), so a re-fetch leaves no
// second copy behind.
func writeCachedArtwork(dir, base, contentType string, data []byte) (string, error) {
	if len(data) == 0 {
		return "", ErrArtworkAbsent
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, base+relayArtworkExtension(contentType))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return path, nil
}

// relayArtworkExtension maps a content type onto the cache-file extension. An
// unknown or missing type falls back to .jpg, which is what the overwhelming
// majority of posters are and what the serve layer sniffs correctly anyway.
func relayArtworkExtension(contentType string) string {
	switch {
	case strings.Contains(contentType, "png"):
		return ".png"
	case strings.Contains(contentType, "webp"):
		return ".webp"
	default:
		return ".jpg"
	}
}
