package enrich

import (
	"context"
	"errors"
	"reflect"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The music half of the contract edge. What is worth a unit test here is exactly
// what a type assertion used to decide and a capability now does: that an album's
// tracklist and edition list cross the wire whole, that ErrNoTracklist survives as
// itself through an Outcome that is spelled "no-match", that a kind mismatch keeps
// the two kinds that make its message useful, and that an undeclared capability
// costs no call and degrades to the answer the failed assertion used to give.

// cannedMusicSource is a MetadataProvider in this package's own vocabulary that
// also implements all three optional MUSIC seams — what MusicBrainz is — used to
// drive the Plugin side of the adapter.
type cannedMusicSource struct {
	cannedProvider

	tracks     []TrackCandidate
	tracksErr  error
	editions   []ReleaseEdition
	editionErr error
	ref        ExternalRef
	refErr     error

	lastTracklist TracklistRequest
	lastRGID      string
	lastKind      string
	lastPasted    string
}

func (c *cannedMusicSource) AlbumTracklist(_ context.Context, req TracklistRequest) ([]TrackCandidate, error) {
	c.lastTracklist = req
	return c.tracks, c.tracksErr
}

func (c *cannedMusicSource) ReleaseGroupEditions(_ context.Context, rgID string) ([]ReleaseEdition, error) {
	c.lastRGID = rgID
	return c.editions, c.editionErr
}

func (c *cannedMusicSource) ParseExternalRef(_ context.Context, kind, pasted string) (ExternalRef, error) {
	c.lastKind, c.lastPasted = kind, pasted
	return c.ref, c.refErr
}

// musicCapable is a Descriptor declaring what an authoritative music source does.
func musicCapable() pluginapi.Descriptor {
	return pluginapi.Descriptor{
		Slug:  "fakemusic",
		Name:  "Fake Music Source",
		Kinds: []string{KindMusic},
		Role:  RoleAuthoritative,
		Class: ClassFull,
		Capabilities: []pluginapi.Capability{
			pluginapi.CapabilitySearch,
			pluginapi.CapabilityArtworkCandidates,
			pluginapi.CapabilityAlbumTracklist,
			pluginapi.CapabilityExternalRef,
		},
	}
}

// TestAnAlbumTracklistSurvivesTheRoundTrip drives a source out through the Plugin
// side and back in through the host side — the path BuildProvider composes — and
// asserts the whole request and the whole answer cross.
//
// ReleaseIDChosen is the field that matters most. It is not a hint: ADR-0052 grants
// position-alone mapping on the single fact that a HUMAN asserted this edition, and
// a tracklist cannot carry that fact home (every release's tracklist looks the
// same), so a request that lost it would either withhold a licence the operator
// earned or grant one nobody did.
func TestAnAlbumTracklistSurvivesTheRoundTrip(t *testing.T) {
	want := []TrackCandidate{
		{Disc: 1, Position: 1, Title: "Airbag", ExternalID: "rec-1"},
		// An entry the source named no recording for still CLAIMS its position, which
		// is what the host's rule 3 counts (ADR-0050).
		{Disc: 1, Position: 2, Title: "Paranoid Android"},
	}
	source := &cannedMusicSource{tracks: want}
	provider := ProviderFromPlugin(musicCapable(), pluginFromProvider(source))

	lister, ok := provider.(AlbumTracklister)
	if !ok {
		t.Fatal("the host side must always satisfy AlbumTracklister, so the chains' assertions still hold")
	}
	req := TracklistRequest{
		ReleaseGroupID:  "rg-1",
		ReleaseID:       "rel-1",
		ReleaseIDChosen: true,
		LocalTrackCount: 12,
	}
	got, err := lister.AlbumTracklist(context.Background(), req)
	if err != nil {
		t.Fatalf("album tracklist: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tracklist lost data across the contract.\n got: %+v\nwant: %+v", got, want)
	}
	if source.lastTracklist != req {
		t.Errorf("request did not cross intact: got %+v, want %+v", source.lastTracklist, req)
	}
}

// TestNoTracklistCrossesAsNoMatchAndComesBackItself: the contract grew no eighth
// Outcome for "this album has no tracklist" because it did not need one — the call
// guarantees a matched answer is never empty, so no-match can only mean that. What
// must NOT happen is the other collapse: a transport failure reading as a settled
// nothing, which would turn an outage into a diagnosis (ADR-0048/0049).
func TestNoTracklistCrossesAsNoMatchAndComesBackItself(t *testing.T) {
	source := &cannedMusicSource{tracksErr: ErrNoTracklist}
	plugin := pluginFromProvider(source)

	wire, ok := plugin.(pluginapi.AlbumTracklister)
	if !ok {
		t.Fatal("the Plugin side must always expose the album-tracklist calls")
	}
	resp, err := wire.AlbumTracklist(context.Background(), pluginapi.TracklistRequest{ReleaseGroupID: "rg-1"})
	if err != nil {
		t.Fatalf("a settled 'no tracklist' must not travel as a Go error: %v", err)
	}
	if resp.Outcome != pluginapi.OutcomeNoMatch {
		t.Errorf("outcome = %q, want no-match", resp.Outcome)
	}

	provider := ProviderFromPlugin(musicCapable(), plugin).(AlbumTracklister)
	if _, err := provider.AlbumTracklist(context.Background(), TracklistRequest{ReleaseGroupID: "rg-1"}); !errors.Is(err, ErrNoTracklist) {
		t.Errorf("host-side error = %v, want ErrNoTracklist", err)
	}

	// A real failure — including the bare ErrNoMatch a 404 on the release browse
	// produces — is NOT an outcome for this call and travels as itself, so the pass
	// retries the album instead of recording that it has no tracklist.
	shed := errors.New("musicbrainz: status 503")
	for _, transient := range []error{shed, ErrNoMatch} {
		source.tracksErr = transient
		if _, err := wire.AlbumTracklist(context.Background(), pluginapi.TracklistRequest{ReleaseGroupID: "rg-1"}); !errors.Is(err, transient) {
			t.Errorf("transport failure %v was swallowed into an outcome (got %v)", transient, err)
		}
		if _, err := provider.AlbumTracklist(context.Background(), TracklistRequest{ReleaseGroupID: "rg-1"}); errors.Is(err, ErrNoTracklist) {
			t.Errorf("transport failure %v reached the host as a settled ErrNoTracklist", transient)
		}
	}

	// An empty list with a matched outcome is forbidden by the call; the host
	// normalizes it rather than handing a caller an album with no tracks and no error.
	source.tracksErr, source.tracks = nil, nil
	if _, err := provider.AlbumTracklist(context.Background(), TracklistRequest{ReleaseGroupID: "rg-1"}); !errors.Is(err, ErrNoTracklist) {
		t.Errorf("an empty matched tracklist = %v, want ErrNoTracklist", err)
	}
}

// TestAnAlbumsEditionsCrossTheContract: the edition list's absent-answers are the
// OPPOSITE way round from the tracklist's, and that difference is the host's, not
// the Plugin's. An album with no editions is a real answer the picker renders; a
// source that lists none at all is the picker's "not now" (ErrSearchUnavailable),
// which degrades to the pasted-URL escape hatch instead of an error page.
func TestAnAlbumsEditionsCrossTheContract(t *testing.T) {
	want := []ReleaseEdition{{
		ReleaseID: "rel-1", Date: "1997-06-16", Country: "GB",
		Format: "CD", TrackCount: 12, Disambiguation: "deluxe edition",
	}}
	source := &cannedMusicSource{editions: want}
	provider := ProviderFromPlugin(musicCapable(), pluginFromProvider(source)).(AlbumEditionLister)

	got, err := provider.ReleaseGroupEditions(context.Background(), "rg-1")
	if err != nil {
		t.Fatalf("editions: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("editions lost data across the contract.\n got: %+v\nwant: %+v", got, want)
	}
	if source.lastRGID != "rg-1" {
		t.Errorf("release-group id = %q, want rg-1", source.lastRGID)
	}

	// No editions: an empty list and a nil error, not a refusal.
	source.editions = nil
	if got, err := provider.ReleaseGroupEditions(context.Background(), "rg-1"); err != nil || len(got) != 0 {
		t.Errorf("no editions = (%+v, %v), want an empty list and no error", got, err)
	}

	// A source that cannot list editions at all: unavailable, through the Plugin side.
	plain := ProviderFromPlugin(musicCapable(), pluginFromProvider(&cannedProvider{})).(AlbumEditionLister)
	if _, err := plain.ReleaseGroupEditions(context.Background(), "rg-1"); !errors.Is(err, ErrSearchUnavailable) {
		t.Errorf("a source with no edition list = %v, want ErrSearchUnavailable", err)
	}
}

// TestUndeclaredMusicCapabilitiesCostNoCall: the host consults the DECLARATION
// before the Plugin (ADR-0057 decision 3), and each call's undeclared answer is the
// one its failed type assertion used to give — no tracklist for the tracklist, "not
// now" for the edition list and for reading a paste.
func TestUndeclaredMusicCapabilitiesCostNoCall(t *testing.T) {
	source := &cannedMusicSource{
		tracks:   []TrackCandidate{{Position: 1, Title: "Airbag"}},
		editions: []ReleaseEdition{{ReleaseID: "rel-1"}},
		ref:      ExternalRef{ExternalID: "mb-1"},
	}
	// Declares nothing optional, which is every artwork-only supplement.
	desc := pluginapi.Descriptor{Slug: "silent", Kinds: []string{KindMusic}}
	provider := ProviderFromPlugin(desc, pluginFromProvider(source))

	if _, err := provider.(AlbumTracklister).AlbumTracklist(context.Background(),
		TracklistRequest{ReleaseGroupID: "rg-1"}); !errors.Is(err, ErrNoTracklist) {
		t.Errorf("undeclared album-tracklist = %v, want ErrNoTracklist", err)
	}
	if _, err := provider.(AlbumEditionLister).ReleaseGroupEditions(context.Background(), "rg-1"); !errors.Is(err, ErrSearchUnavailable) {
		t.Errorf("undeclared edition list = %v, want ErrSearchUnavailable", err)
	}
	if _, err := provider.(ExternalRefParser).ParseExternalRef(context.Background(), "album", "mb-1"); !errors.Is(err, ErrSearchUnavailable) {
		t.Errorf("undeclared external-ref = %v, want ErrSearchUnavailable", err)
	}
	if source.lastTracklist.ReleaseGroupID != "" || source.lastRGID != "" || source.lastPasted != "" {
		t.Error("the Plugin was asked for operations it never declared; an undeclared capability must cost no call")
	}
}

// TestAPastedRefKeepsItsGotAndWantAcrossTheContract: ref-kind-mismatch is ONE
// Outcome, and the sentence the Admin reads names the kind pasted and the kind
// wanted. Those two kinds live on the response, not in the enum, and the adapter
// rebuilds the typed error from them — otherwise the wrong-kind 400 degrades to the
// generic "that id is for a different kind of record", which is what the Admin
// already knew.
func TestAPastedRefKeepsItsGotAndWantAcrossTheContract(t *testing.T) {
	source := &cannedMusicSource{refErr: &ExternalRefKindMismatchError{Got: "artist", Want: "track"}}
	provider := ProviderFromPlugin(musicCapable(), pluginFromProvider(source)).(ExternalRefParser)

	_, err := provider.ParseExternalRef(context.Background(), "track", "https://musicbrainz.org/artist/x")
	if !errors.Is(err, ErrExternalRefKindMismatch) {
		t.Fatalf("error = %v, want a kind mismatch", err)
	}
	var mismatch *ExternalRefKindMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatal("the kinds did not survive the contract; the API would fall back to the generic message")
	}
	if mismatch.Got != "artist" || mismatch.Want != "track" {
		t.Errorf("kinds = %+v, want got=artist want=track", mismatch)
	}

	// The other two refusals keep their own identity, because they are three
	// different sentences with three different fixes.
	for _, tc := range []struct {
		name string
		from error
		want error
	}{
		{"unreadable", ErrExternalRefInvalid, ErrExternalRefInvalid},
		{"an entity kind nothing pins", ErrExternalRefUnsupportedKind, ErrExternalRefUnsupportedKind},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source.refErr = tc.from
			if _, err := provider.ParseExternalRef(context.Background(), "album", "x"); !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}

	// And a read that succeeds carries BOTH ids: the album to pin and the edition
	// the paste named (ADR-0052).
	source.refErr = nil
	source.ref = ExternalRef{ExternalID: "rg-1", ReleaseID: "rel-1"}
	got, err := provider.ParseExternalRef(context.Background(), "album", "https://musicbrainz.org/release/rel-1")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// ...stamped with the answering Plugin's id as its namespace (ADR-0060 decision 5).
	if got != (ExternalRef{ExternalID: "rg-1", ReleaseID: "rel-1", Namespace: "fakemusic"}) {
		t.Errorf("ref = %+v, want both the release-group and the release, in the plugin's namespace", got)
	}
	if source.lastKind != "album" || source.lastPasted != "https://musicbrainz.org/release/rel-1" {
		t.Errorf("the request did not cross intact: kind=%q pasted=%q", source.lastKind, source.lastPasted)
	}
}

// TestTheMusicBrainzPluginReadsAPaste IS GONE (.scratch/bundled-plugins: issue 06).
// It drove the real MusicBrainz Built-in through its registration over the pastes
// that produce the two distinct 400s. There is no Built-in to drive: the source is
// a WebAssembly module, its paste vocabulary is asserted natively in
// plugins/musicbrainz/musicbrainz (TestParseExternalRef, over the same pastes), and
// what CROSSES the contract is asserted by TestAPastedRefKeepsItsGotAndWantAcrossTheContract
// above, which is the part this package owns.

// TestTheHostReadsAPasteOnlyWhenNoPluginCan: the provider is asked first and its
// answer stands — including its refusals — and the host's own parse is the answer
// for a kind whose provider does not read pastes (an undeclared capability, or the
// fixed provider a test injects).
func TestTheHostReadsAPasteOnlyWhenNoPluginCan(t *testing.T) {
	const id = "b1392450-e666-3926-a536-22c65f834433"
	ctx := context.Background()

	// A fixed provider that reads no pastes: the host reads it, exactly as before the
	// capability existed. This is also why every existing black-box suite is unmoved.
	plain := NewService(nil, &cannedProvider{}, nil, Enablement{Music: true}, "", 0)
	got, err := plain.externalRef(ctx, plain.snapshot(), "track", id)
	if err != nil || got.ExternalID != id {
		t.Errorf("host parse = (%+v, %v), want the bare mbid trusted", got, err)
	}

	// A provider that DOES read pastes speaks for its source: its refusal is the
	// answer, not an invitation for the host to have a second go.
	source := &cannedMusicSource{refErr: ErrExternalRefInvalid}
	svc := NewService(nil, ProviderFromPlugin(musicCapable(), pluginFromProvider(source)), nil,
		Enablement{Music: true}, "", 0)
	if _, err := svc.externalRef(ctx, svc.snapshot(), "track", id); !errors.Is(err, ErrExternalRefInvalid) {
		t.Errorf("provider refusal = %v, want it to stand as ErrExternalRefInvalid", err)
	}
	if source.lastPasted != id {
		t.Error("the provider was not asked at all")
	}
}

// TestAnAlbumCandidateKeepsItsTracklistAndEdition: both fields are album-only and
// both were left off the wire while only video crossed it. Dropping them is not a
// compile error anywhere — the picker's track preview would simply go empty and a
// pasted edition would silently clear (ADR-0052) — so they are asserted here.
func TestAnAlbumCandidateKeepsItsTracklistAndEdition(t *testing.T) {
	want := Candidate{
		ExternalID: "rg-1",
		Title:      "OK Computer",
		Year:       1997,
		Kind:       "album",
		TypeLabel:  "Album",
		Tracklist:  []TrackCandidate{{Disc: 1, Position: 1, Title: "Airbag", ExternalID: "rec-1"}},
		ReleaseID:  "rel-1",
	}
	source := &searchingSource{cands: []Candidate{want}}
	provider := ProviderFromPlugin(musicCapable(), pluginFromProvider(source))
	// The host stamps the namespace from the Plugin it asked (ADR-0060 decision 5).
	want.Source = "fakemusic"

	got, err := provider.Search(context.Background(), "album", "OK Computer", SearchOptions{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Errorf("candidate lost data across the contract.\n got: %+v\nwant: %+v", got, want)
	}
}

// searchingSource is a source whose Search answers, for the candidate round trip.
type searchingSource struct {
	cannedProvider
	cands []Candidate
}

func (s *searchingSource) Search(context.Context, string, string, SearchOptions) ([]Candidate, error) {
	return s.cands, nil
}

// TestTheMusicBrainzPluginIsBuiltFromItsSettings IS ALSO GONE, for the same reason
// and with the same replacement in two halves: what the HOST resolves into a music
// lead's Settings — the operator's pacing, both hosts, the language — is asserted in
// builder_test.go against a settings-recording registration, and what the PLUGIN
// does with them is asserted in plugins/musicbrainz/musicbrainz (the pacer suite,
// and TestSettingsAreReadPerCall). Neither half can be asserted by reaching through
// the contract at a guest, which is what the deleted test did.
