package playback

import (
	"context"
	"errors"

	"github.com/goozakdev/obelo-server/internal/store"
)

// The one-hop relay, seen from the negotiation (ADR-0056 §5).
//
// A Title in a linked Library is a local row like any other — that is the whole
// point of the mirror — right up to the moment somebody presses play. Then the
// bytes are on the other household's disk, the tiering is theirs to decide under
// their governance and their `remote` User's ceiling (ADR-0054 §2), and this
// Server's job is to ask, to open a Session wrapping the one they opened, and to
// carry the bytes through untouched.
//
// THE BRANCH IS HERE AND NOWHERE ELSE. Negotiate asks the Relayer whether this
// Title's Library is a mirror, and every other part of playback — the session
// Manager, the reaper, progress, watch state — is unchanged, because a relay
// Session is an ordinary Session that happens to name a remote one. The seam is
// an interface so this package keeps knowing nothing about Links, tokens,
// dialers or HTTP; internal/link implements it.

// Relayer is the sharing Server, as the negotiation needs it. *link.Service
// satisfies it. Nil on every Server that holds no Links, which is the common
// case and costs one nil check per negotiation.
type Relayer interface {
	// RelaysLibrary reports whether a Library's Titles play through a relay — i.e.
	// whether it is a mirror of another Server's (ADR-0056 §1). It fails CLOSED to
	// false: a Server that cannot answer plays its own local Libraries as it always
	// did, and a mirrored Title with no relay simply cannot start (its Files carry
	// no path), which is the honest outcome either way.
	RelaysLibrary(libraryID string) bool
	// RelayNegotiate forwards one negotiation to the sharer under the Link's
	// credential and returns its answer.
	//
	// Its error is the sharer's, and the api layer renders each kind on its own
	// terms: a transport failure is 503 LINK_UNREACHABLE, a dead credential is 503
	// LINK_REVOKED, and any other refusal the sharer gave (SERVER_BUSY with its
	// suggestedMaxBitrate, STREAM_LIMIT, TRANSCODE_REQUIRED, a 404) passes through
	// verbatim — this Server has no better answer than the one the machine holding
	// the file just gave.
	RelayNegotiate(ctx context.Context, req RelayRequest) (RelayAnswer, error)
}

// RelayRequest is the negotiation as it goes over the wire: the client's
// Capability profile exactly as it arrived, the Constraints ALREADY CLAMPED by
// this household's own Playback ceiling (ADR-0054 §2 — the sharer then clamps
// again with the `remote` User's, and the stricter of the two binds), and the
// local ids of anything the client picked, which the relay maps through
// `remote_id`.
//
// It carries nothing about WHO is watching. That absence is the design (ADR-0054
// §3): the sharer sees one `remote` User and one session per relayed stream, and
// no household member's id, name or Device name crosses the wire.
type RelayRequest struct {
	// LibraryID is the mirror the Title sits in; it names the Link.
	LibraryID string
	// TitleID, EditionID and the two Stream ids are LOCAL ids; the relay maps each
	// through `remote_id`. BurnSubtitleID is NOT: a Subtitle track's id on a
	// mirrored Title is the sharer's own already — the feed carries no subtitle
	// rows (issue 05), so every id a client can name here arrived on the sharer's
	// own Decision — and it travels untranslated.
	TitleID        string
	EditionID      string
	AudioStreamID  string
	VideoStreamID  string
	BurnSubtitleID string

	Profile           DeviceProfile
	Constraints       Constraints
	StartPosition     int64
	RemuxSelectedOnly bool
}

// RelayAnswer is what the sharer said, plus the two ids this side needs to hold
// on to.
type RelayAnswer struct {
	// LinkID and RemoteSessionID are what the local Session wraps: the credential
	// to fetch under, and the session to end over there when this one ends.
	LinkID          string
	RemoteSessionID string
	// RemoteTitleID is the sharer's id for the Title, which the relay's media
	// routes need to validate a subtitle path against.
	RemoteTitleID string
	// RemoteEditionID is the Edition the sharer chose, so this side can find the
	// mirrored row and measure the Session against its duration.
	RemoteEditionID string
	Tier            Tier
	// Decision is the sharer's whole answer, decoded but not reshaped. It is
	// re-served to the client with the ids and URLs rewritten and NOTHING else
	// touched (see api's relay decision), which is what "the sharer's answers pass
	// through verbatim" means in practice: a field this build has never heard of
	// still reaches a client that has.
	Decision map[string]any
}

// Relayed marks a Decision the SHARER made, carried on the local Decision so the
// session Manager and the api layer can tell one from a Decision this Server made
// itself. A relay Decision never starts ffmpeg here and never counts against this
// Server's transcode cap — this household pays bandwidth, never CPU, for someone
// else's files (ADR-0056 §5).
type Relayed struct {
	LinkID          string
	RemoteSessionID string
	RemoteTitleID   string
	Decision        map[string]any
}

// SetRelay installs the relay seam. It mirrors Manager.SetObserver's
// post-construction style for the same reason: the link Service is built after
// the playback Service (it needs the Tailnet manager and the mirror), and
// threading it through NewService would make every unit test that builds a
// playback Service learn about Links.
func (s *Service) SetRelay(r Relayer) { s.relay = r }

// relayed reports whether this Title's Library plays through a relay.
func (s *Service) relayed(libraryID string) bool {
	return s.relay != nil && s.relay.RelaysLibrary(libraryID)
}

// negotiateRelay is Negotiate for a mirrored Title: ask the sharer, then open a
// local Session wrapping theirs.
//
// The Constraints handed to it are already clamped by this household's own
// ceiling, so the request that leaves is the strictest of what the client asked
// for and what this Server allows — and the sharer applies its own on top.
func (s *Service) negotiateRelay(req Request, detail store.TitleDetail) (Decision, Session, *Unsupported, *ServerBusy, error) {
	ans, err := s.relay.RelayNegotiate(context.Background(), RelayRequest{
		LibraryID:         detail.LibraryID,
		TitleID:           detail.ID,
		EditionID:         req.EditionID,
		AudioStreamID:     req.AudioStreamID,
		VideoStreamID:     req.VideoStreamID,
		BurnSubtitleID:    req.BurnSubtitleID,
		Profile:           req.Profile,
		Constraints:       req.Constraints,
		StartPosition:     req.StartPosition,
		RemuxSelectedOnly: req.RemuxSelectedOnly,
	})
	if err != nil {
		return Decision{}, Session{}, nil, nil, err
	}

	dec := Decision{
		Tier: ans.Tier,
		Relay: &Relayed{
			LinkID:          ans.LinkID,
			RemoteSessionID: ans.RemoteSessionID,
			RemoteTitleID:   ans.RemoteTitleID,
			Decision:        ans.Decision,
		},
	}
	// The mirrored Edition the sharer chose, so the Session is measured against the
	// same work the viewer is watching: the Watched threshold and the resume
	// position are both fractions of DurationMs, and a relay session with no
	// duration silently stops recording where anybody got to. A remote Edition this
	// side has not mirrored yet (a pull is behind) leaves the Decision's Edition
	// zero, which is a session that plays and records no resume — degraded, not
	// broken.
	if ed, ok := localEditionFor(detail, s.relayLocalEdition(ans.RemoteEditionID)); ok {
		dec.Edition = ed
		if files := ed.Parts(); len(files) > 0 {
			dec.File = files[0]
		}
	}

	sess, err := s.sessions.CreateGoverned(CreateInput{
		UserID:        req.UserID,
		DeviceID:      req.DeviceID,
		TitleID:       req.TitleID,
		StartPosition: req.StartPosition,
		// This household's own concurrent-stream ceiling still applies (ADR-0054 §2):
		// it is a statement about this User, and a stream that costs this Server only
		// bandwidth is still a stream they are taking. The sharer counts its own, over
		// there, against the `remote` User.
		MaxStreams: req.Scope.MaxStreams,
	}, dec)
	var limit *StreamLimitError
	if errors.As(err, &limit) {
		return Decision{}, Session{}, nil, nil, limit
	}
	if err != nil {
		return Decision{}, Session{}, nil, nil, err
	}
	return dec, sess, nil, nil, nil
}

// relayLocalEdition maps the sharer's Edition id onto this Server's, or "" when
// the mirror has not seen it. It is a method so the store lookup stays optional:
// a Service whose store cannot answer (a fake in a unit test) simply reports no
// Edition rather than failing the play.
func (s *Service) relayLocalEdition(remoteEditionID string) string {
	rs, ok := s.store.(RemoteIDStore)
	if !ok || remoteEditionID == "" {
		return ""
	}
	id, err := rs.LocalIDForRemote(store.ExportEdition, remoteEditionID)
	if err != nil {
		return ""
	}
	return id
}

// RemoteIDStore is the id translation the relay needs from the catalog store
// (store/relay.go). *store.DB satisfies it; it is type-asserted off the store
// the Service was built with, exactly as AudioMemoryStore is, so a fake store in
// a unit test runs with the translation disabled.
type RemoteIDStore interface {
	LocalIDForRemote(kind, remoteID string) (string, error)
}

// localEditionFor finds an Edition of the Title by local id.
func localEditionFor(detail store.TitleDetail, editionID string) (store.Edition, bool) {
	if editionID == "" {
		return store.Edition{}, false
	}
	for _, ed := range detail.Editions {
		if ed.ID == editionID {
			return ed, true
		}
	}
	return store.Edition{}, false
}
