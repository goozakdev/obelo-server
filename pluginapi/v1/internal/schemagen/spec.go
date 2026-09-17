package schemagen

import (
	"reflect"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// schemaDoc is the document's own description: the three things an author has to
// know before reading a single $def.
const schemaDoc = "The wire contract every Obelo Plugin implements (ADR-0057). " +
	"Generated from the Go structs in pluginapi/v1 by pluginapi/v1/internal/schemagen; " +
	"regenerate with `go generate ./pluginapi/v1`. " +
	"Within v1 this schema changes ADDITIVELY ONLY: fields and enum values may be added, " +
	"never removed, renamed or retyped, and no field becomes required after the fact. " +
	"additionalProperties is deliberately left OPEN on every type, because that is what " +
	"makes additive evolution safe: a v1.0 guest must ignore a field a later v1.x host sends, " +
	"and a host must ignore a field a guest sends back. " +
	"A pointer field in Go is an OMITTED key here, never null — absent and zero are different " +
	"instructions wherever the contract says so."

// contractEnums lists every closed set the contract package declares constants
// for. Adding a value here is additive; removing one is not.
func contractEnums() []enumSpec {
	toStrings := func(vs []pluginapi.Outcome) []string {
		out := make([]string, len(vs))
		for i, v := range vs {
			out[i] = string(v)
		}
		return out
	}
	return []enumSpec{
		{
			name:   "ExtensionPoint",
			doc:    "Which of the closed set of seams a registration fills. The set grows by ADR, not by a Plugin asking.",
			goType: reflect.TypeOf(pluginapi.ExtensionPoint("")),
			values: []string{
				string(pluginapi.ExtensionMetadataProvider),
				string(pluginapi.ExtensionSubtitleProvider),
				string(pluginapi.ExtensionEventSink),
			},
		},
		{
			name:   "Capability",
			doc:    "An OPTIONAL operation a Plugin declares it implements. The host consults the declaration before calling, so an undeclared operation costs no call and answers 'unavailable'.",
			goType: reflect.TypeOf(pluginapi.Capability("")),
			values: []string{
				string(pluginapi.CapabilitySearch),
				string(pluginapi.CapabilityArtworkCandidates),
				string(pluginapi.CapabilityAlbumTracklist),
				string(pluginapi.CapabilityExternalRef),
				string(pluginapi.CapabilityEpisodeList),
			},
		},
		{
			name:   "Outcome",
			doc:    "What happened, as a value rather than an error identity. A Go/transport error alongside a response means the Plugin or the network failed, which the host retries; everything the host would otherwise learn from an error sentinel is one of these.",
			goType: reflect.TypeOf(pluginapi.Outcome("")),
			values: toStrings(pluginapi.AllOutcomes()),
		},
		{
			name:   "Role",
			doc:    "A Plugin's default position in the provider chain: the authoritative source that leads, or the fill-only supplement that adds only what the leader left empty.",
			goType: reflect.TypeOf(pluginapi.Role("")),
			values: []string{
				string(pluginapi.RoleAuthoritative),
				string(pluginapi.RoleSupplement),
			},
		},
		{
			name:   "Class",
			doc:    "The capability distinction a Library's Authoritative provider pointer is constrained against (ADR-0027): only a 'full' provider may lead a Library's Enrichment.",
			goType: reflect.TypeOf(pluginapi.Class("")),
			values: []string{
				string(pluginapi.ClassFull),
				string(pluginapi.ClassArtworkOnly),
			},
		},
		{
			name: "MediaKind",
			doc:  "A coarse media-kind group a Plugin declares it serves. These are the Enrichment kinds the settings screen groups by, not the finer entity kinds (movie/show/artist/album/track) that appear as plain strings elsewhere.",
			values: []string{
				pluginapi.KindVideo,
				pluginapi.KindMusic,
			},
		},
		{
			name:   "EventType",
			doc:    "One of the curated terminal events an Event sink may be told about. The set is closed and grows by decision: a translator that cannot derive an event honestly is a reason not to have it.",
			values: pluginapi.AllEventTypes(),
		},
	}
}

// fieldOverrides are the few fields whose schema is not what reflection alone
// would produce: the plain `string` fields that carry a declared enum, and the
// one pointer whose absent/zero distinction is load-bearing.
func fieldOverrides() map[string]any {
	return map[string]any{
		"Settings.rateLimitMillis": obj().
			set("type", "integer").
			set("minimum", 0).
			set("description", "Minimum interval between two requests to this source's host, in milliseconds. "+
				"ABSENT means 'use your own default pacing'; 0 means 'do not throttle at all', which an operator sets "+
				"for a self-hosted mirror with no rate policy. The two are different instructions and neither is the "+
				"other's default, so a guest must distinguish a missing key from a present 0 and must never substitute "+
				"one for the other."),
		"Settings.events": obj().
			set("type", "array").
			set("items", ref("EventType")).
			set("description", "An Event sink's subscribed event types. The HOST filters on this, so a sink is never handed an event it did not ask for. Empty for every provider."),
		"Descriptor.kinds": obj().
			set("type", "array").
			set("items", ref("MediaKind")).
			set("description", "The coarse media-kind groups this Plugin serves. Empty for an Extension point that is not kind-scoped."),
		"ManifestProvides.kinds": obj().
			set("type", "array").
			set("items", ref("MediaKind")).
			set("description", "The coarse media-kind groups this entry serves. Empty and ignored for an Event sink, which is not kind-scoped."),
		"SinkEvent.type": ref("EventType"),
	}
}

// contractTypes lists every wire struct, in the order the three Extension points
// declare them: the shared vocabulary, then metadata, then subtitles, then the
// Event sink. Every struct that can appear in a document is here; a struct the
// reflector meets and cannot find in this list is an error, so a new wire type
// arrives with a description rather than anonymously.
//
// Registry is NOT here and never will be: it is host-side composition (the set of
// Plugins one running server has), not something that crosses the contract.
// Neither are the factory function types or the Go interfaces, for the same
// reason — they are the in-process calling convention, and the calls they name are
// request/response pairs of the types below.
func contractTypes() []typeSpec {
	return []typeSpec{
		// ---- the shared vocabulary -------------------------------------------------
		{
			value: pluginapi.Settings{},
			doc: "The fixed shape an Admin configures for every Plugin at every Extension point: " +
				"an enabled toggle, one secret, one URL, an optional second URL, an Event sink's subscribed " +
				"events, and the two host-resolved knobs (language, rate limit) the Built-ins forced. " +
				"The host resolves every field before building the Plugin. " +
				"NOTE for a manifest reader: url2 need not come from the SAME registration as url — " +
				"TMDB's image host is TMDB's own setting, while MusicBrainz's is Cover Art Archive's, " +
				"a separate registration with its own settings row that the host resolves into MusicBrainz's url2.",
		},
		{
			value: pluginapi.Descriptor{},
			doc: "The static self-description a Plugin registers with: the facts that live in code rather " +
				"than the database, which the settings API renders and the builder composes from. It carries no " +
				"factory — how to BUILD a Plugin sits beside the descriptor, not in it, so everything here " +
				"round-trips. A registration may legitimately have no factory at all (Cover Art Archive is the " +
				"Built-in case: it has no client of its own, only settings the host folds into another Plugin's url2), " +
				"so an install flow must not require a module for one.",
		},
		{
			value: pluginapi.Page{},
			doc: "The contract's only paging shape: a limit and an offset, never a cursor, so 'show more' " +
				"works against any source without a stateful protocol. Zero means the Plugin's own default. " +
				"It is EMBEDDED in every paging request, so limit and offset appear as flat keys on that " +
				"request rather than under a 'page' object.",
		},

		// ---- Metadata provider -----------------------------------------------------
		{
			value: pluginapi.MediaRef{},
			doc: "The locally-parsed identity a Metadata provider is asked about, plus whatever external ids " +
				"the host already holds. When it carries an id the Plugin recognizes, the Plugin RESOLVES BY ID " +
				"and does not search — an id is the identification.",
		},
		{
			value: pluginapi.AlbumHint{},
			doc:   "One local album offered as corroboration for its artist, so a source can identify an artist through its discography rather than its name. Evidence, not a pin.",
		},
		{
			value: pluginapi.ArtworkRef{},
			doc:   "One remote image a record carries for an Artwork role (poster, background, logo, cover). A Plugin returns a URL and never bytes: the host downloads through its own guarded fetcher.",
		},
		{
			value: pluginapi.Credit{},
			doc:   "One cast or crew member, normalized. personRef is the source-namespaced stable person id that keys the headshot in the host's cache, so one actor's photo is stored once across every Title they appear in.",
		},
		{
			value: pluginapi.MetadataRecord{},
			doc: "One source's descriptive answer about a MediaRef. It carries no identity — the host applies " +
				"name as a display-only override where its own rules allow. fromSearch is the one field an author " +
				"must get right: it states HOW the answer was found (a relevance-ranked search rather than an id " +
				"resolution), never whether it is believed. The HOST judges.",
		},
		{value: pluginapi.LookupRequest{}, doc: "Asks one source to resolve one ref to one record."},
		{
			value: pluginapi.LookupResponse{},
			doc:   "What a lookup answers. 'matched' carries the record; 'no-match' is the normal 'this source has nothing for that'. A Plugin does NOT answer 'rejected' for its own top hit — acceptance is the host's judgment, and the way to hand a search hit over is a record with fromSearch set.",
		},
		{value: pluginapi.SearchRequest{}, doc: "The Edit-item picker's free-text query, scoped to one fine entity kind. artist and release are optional narrowing axes for a music search; a kind with no such axis ignores them."},
		{
			value: pluginapi.SearchCandidate{},
			doc:   "One result an Admin may pick to correct an item's record: enough to tell two same-named works apart. tracklist is a rough PREVIEW for an album candidate, not the album-tracklist call's answer. An empty releaseId applied to an album CLEARS whatever edition it had.",
		},
		{value: pluginapi.SearchResponse{}, doc: "What a search answers. 'matched' with an empty list is a query that found nothing, which is NOT 'unavailable' — that is 'this kind cannot be searched right now'."},
		{value: pluginapi.ArtworkCandidatesRequest{}, doc: "Asks for the images a source offers for one Artwork role on the record ref points at. ref must carry the resolved external id: a role has no candidates without a record."},
		{value: pluginapi.ArtworkCandidate{}, doc: "One selectable image for a role. width and height are the source's own dimensions (0 when it reports none). As everywhere in this contract, an image is a URL the host fetches, never bytes a Plugin supplies."},
		{value: pluginapi.ArtworkCandidatesResponse{}, doc: "What the artwork picker's query answers. 'matched' with an empty list is 'this record has no image for that role'; 'unavailable' is 'this source owns no listable set for this kind', which degrades to the upload path rather than an error."},
		{value: pluginapi.SeriesSeasonsRequest{}, doc: "Asks which seasons a series has, by the source's own id. Behind the episode-list capability."},
		{value: pluginapi.SeasonSummary{}, doc: "One season of a series, for the season chooser."},
		{value: pluginapi.SeriesSeasonsResponse{}, doc: "Lists a series' seasons in season order."},
		{value: pluginapi.SeasonEpisodesRequest{}, doc: "Asks for one season's episodes, by the source's series id. Behind the episode-list capability."},
		{value: pluginapi.EpisodeCandidate{}, doc: "One episode an Admin may point a file at when the on-disk numbering does not line up with the source's."},
		{value: pluginapi.SeasonEpisodesResponse{}, doc: "Lists one season's episodes in episode order."},
		{value: pluginapi.TrackCandidate{}, doc: "One entry of an album's tracklist: where it sits, what it is called, and the id of the recording behind it. Display and positional-map data only, never identity. externalId may be empty when the source named no recording; the entry still CLAIMS its position."},
		{
			value: pluginapi.TracklistRequest{},
			doc: "Names the album whose tracklist is wanted. releaseId is used ONLY when its parent release-group " +
				"is releaseGroupId. releaseIdChosen says WHOSE assertion releaseId is — a human's or a file's — which " +
				"changes nothing about how the release is fetched and everything about what happens when it does not " +
				"apply: a file's release falls through to fit-selection silently, a human's is reported as no-match so " +
				"the host can re-ask without the pin and know what it finally got is not the edition the human asserted.",
		},
		{value: pluginapi.TracklistResponse{}, doc: "What a tracklist answers. 'matched' ALWAYS carries at least one track, and 'no-match' is 'this album has no tracklist'. That invariant is what makes reusing 'no-match' here lossless rather than an eighth Outcome value."},
		{value: pluginapi.ReleaseEditionsRequest{}, doc: "Asks which editions an album has, by the album's own id. Behind the album-tracklist capability, which covers both album calls."},
		{value: pluginapi.ReleaseEdition{}, doc: "One edition of an album, described with the five facts an Admin needs to tell two editions apart. trackCount does the work: the edition whose count equals the local album's is the one that will line up. An edition is a DECORATION refinement, never an album's identity."},
		{value: pluginapi.ReleaseEditionsResponse{}, doc: "Lists an album's editions. An empty list with 'matched' is 'this album has exactly no editions to choose from'; 'unavailable' is 'no listable edition set here', which only hides a control."},
		{value: pluginapi.ExternalRefRequest{}, doc: "A string an Admin pasted into the 'paste an id when search isn't enough' box, plus the fine kind of the item they pasted it on. pasted is raw: untrimmed, unvalidated, possibly nonsense."},
		{
			value: pluginapi.ExternalRefResponse{},
			doc: "What parsing a pasted reference answers. The three refusals are distinct because the host renders " +
				"three different sentences: 'ref-invalid' (not an id or URL I know), 'ref-kind-mismatch' (a real entity " +
				"of the wrong kind — gotKind and wantKind travel with it so the message can name both), and " +
				"'ref-unsupported-kind' (one of my URLs, for a kind this server does not pin at all).",
		},

		// ---- Subtitle provider -----------------------------------------------------
		{value: pluginapi.SubtitleRef{}, doc: "Everything a Subtitle provider may key a search by, gathered by the host. A Plugin tries whichever signals are present in the match order — movieHash (release-exact), then imdbId, then a title/year query — and says which one produced each candidate."},
		{value: pluginapi.SubtitleSearchRequest{}, doc: "Asks for the candidates a source offers for one Title in one language. language is a normalized ISO-639-1 code; the host normalizes before asking."},
		{value: pluginapi.SubtitleCandidate{}, doc: "One subtitle a source offers: enough to show a viewer a choice and to ask for those exact bytes later. id is the opaque provider handle echoed back in a download request."},
		{value: pluginapi.SubtitleSearchResponse{}, doc: "What a subtitle search answers. 'no-match' (or an empty list) is the normal 'nothing for this release in this language'."},
		{value: pluginapi.SubtitleDownloadRequest{}, doc: "Asks for one candidate's bytes, whole. maxBytes is the CALLER's cap: a Plugin must refuse rather than return more, and the host checks the answer anyway."},
		{value: pluginapi.SubtitleDownloadResponse{}, doc: "Carries the subtitle file itself, base64-encoded and complete — there is no streaming in this contract. 'no-match' means the candidate has vanished from the source since the search."},

		// ---- Event sink ------------------------------------------------------------
		{value: pluginapi.EventEntity{}, doc: "A reference to one thing on this server: its id, the name an operator would recognize it by, and its kind. The ONLY way an entity crosses this contract — never a catalog row, never a path."},
		{
			value: pluginapi.EventActor{},
			doc: "Who a playback event belongs to. A session relayed over a Link names the LINK and NEVER the " +
				"person watching on the other side, so userId and linkId are mutually exclusive: at most one is " +
				"present. BOTH ABSENT IS LEGAL and means the host could not attribute the session — unattributed is " +
				"a smaller lie than misattributed. name is the operator-facing label (a User's username, or a Link's " +
				"name), never a name from the other household.",
			constrain: notBoth("userId", "linkId"),
		},
		{value: pluginapi.EventScan{}, doc: "The terminal counts of a Scanner pass. Present as a whole block or absent: a pass that found nothing is a real, reportable outcome, so all-zero must not read as 'no scan here'."},
		{value: pluginapi.EventEnrich{}, doc: "The terminal counts of an Enrichment pass. The split between failed and retrying is the whole reason an operator would automate on this event: a transient failure will be tried again, and raising an alarm about it is noise."},
		{
			value: pluginapi.SinkEvent{},
			doc: "One curated event, whole. id is stable and unique and is carried unchanged through every " +
				"delivery attempt, which is what makes the sink call idempotent by construction: a sink that has seen " +
				"an id may discard it. at is RFC 3339 in UTC. Every field but id, type and at is optional and omitted " +
				"when the event has nothing to say there, so a receiving script reads one document shape per type " +
				"rather than a union. The scan and enrich blocks are MUTUALLY EXCLUSIVE — a scan and an enrichment " +
				"pass are separate events even when one follows the other. A relayed playback session carries no " +
				"device block at all, because the device on the sharing side IS the other household's Server.",
			constrain: notBoth("scan", "enrich"),
		},

		// ---- the Installed plugin (ADR-0058) ---------------------------------------
		{
			value: pluginapi.Manifest{},
			doc: "The document an author ships beside a WebAssembly module (`manifest.json`): the Installed " +
				"half of a Descriptor, plus the two things a Built-in never declares because the compiler knew " +
				"them — which contract major it was built against, and which hosts it may reach. Every field is a " +
				"CLAIM the host enforces from its own side: id must not collide with a Plugin the server already " +
				"has, apiVersion must equal the host's, and network.hosts is checked host-side against this file " +
				"on every fetch. A manifest the host cannot read, or that names a contract major it does not " +
				"speak, is refused at load with a message naming which side to upgrade — and the server still " +
				"boots, because a Plugin may never stop one.",
		},
		{
			value: pluginapi.ManifestProvides{},
			doc: "One Extension point this Plugin fills. The host copies these facts onto the Descriptor it " +
				"registers, so an Installed Plugin reaches the registry as the same kind of thing a Built-in does. " +
				"role and class are a claim about where the Plugin belongs in the provider chain, not a grant: " +
				"the host's Enrichment policy decides whether to honour it.",
		},
		{
			value: pluginapi.ManifestNetwork{},
			doc: "The outbound allowlist, and the most load-bearing claim in the document — which is why it is " +
				"the one the host trusts least. Hosts are bare names with no scheme, port, path or wildcard " +
				"('api.example.test'), matched exactly and case-insensitively against the URL's host with its port " +
				"removed. A guest cannot widen this at call time and is never told why a refusal happened beyond " +
				"'refused'. An absent or empty list means a Plugin that makes no outbound requests at all.",
		},
		{
			value: pluginapi.ManifestSettings{},
			doc: "What a manifest may declare about the FIXED settings shape (enabled, secret, url, url2, " +
				"events): which parts must be filled before the Plugin can be turned on, and what the defaults " +
				"are. It is not a schema and does not change the shape. An Event sink leaves the default URLs " +
				"empty — there is no sensible default target for somebody else's receiver.",
		},
		{
			value: pluginapi.SinkDeliverRequest{},
			doc: "What the host hands an Installed Event sink for one event: the curated event, and the " +
				"Settings the Admin saved. The settings travel WITH the call and are never installed into the " +
				"guest, so a secret lives only for the duration of a delivery and an instance rebuilt after a trap " +
				"starts with nobody's credential. The host has already filtered on settings.events, so a guest is " +
				"never handed an event it did not ask for.",
		},
		{
			value: pluginapi.SinkDeliverResponse{},
			doc: "What an Installed Event sink answers. It carries no outcome for the reason the Go sink " +
				"interface returns only an error: a sink has no domain judgment to report, and everything that can " +
				"go wrong is transport, which the host counts and then forgets. delivered=false with an empty " +
				"error is still a failure; filling error gives the operator something to read.",
		},
		{
			value: pluginapi.FetchRequest{},
			doc: "What a guest asks the host to send. It is the ONLY way out: a Plugin has no sockets, and the " +
				"HOST performs the request through the same guarded fetcher every outbound call in this server " +
				"uses. The host sets its own user agent and content length; anything else the Plugin needs goes in " +
				"headers. An empty method means GET.",
		},
		{
			value: pluginapi.FetchHeader{},
			doc: "One header, as a name/value pair rather than a map entry, because a header may legitimately " +
				"repeat and because the order an author wrote them in is the order they are sent.",
		},
		{
			value: pluginapi.FetchResponse{},
			doc: "What came back, or why nothing did. Exactly one of three things is true: refused is set (the " +
				"host would not make this request — allowlist, private address, redirect policy or size), error is " +
				"set (it was attempted and the network or the target failed), or neither is and status is what the " +
				"target answered. A non-2xx status is NOT an error here: it is an answer, and what to do about it " +
				"is the Plugin's business. The text of refused is host-authored and must not be branched on — ANY " +
				"non-empty value means this server will refuse the same request again.",
		},
	}
}
