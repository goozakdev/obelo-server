package enrich

import (
	"context"
	"errors"
	"strings"

	"github.com/goozakdev/obelo-server/internal/store"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// External-id namespaces as the enrichment pass uses them (ADR-0060).
//
// Three rules live here, because every path that resolves or writes a record has
// to agree on them:
//
//   - WHICH provider an item resolves through (pinnedProviderFor): the Library's
//     lead, unless the item's record is a DECISION in some other Authoritative
//     namespace (decision 6).
//   - WHICH namespace a written id belongs to (stampNamespace): the one the host
//     knows it asked, never one a column is named after (decision 5).
//   - WHICH ids a lookup is handed (titleExternalIDs, withExternalIDs): every id the
//     host holds, keyed by namespace, with the five named fields filled from the same
//     map as v1 mirrors (decision 7).

// stampNamespace is the namespace of an id a provider just resolved: the record's
// own Source when it names one — a source's namespace is its plugin id, and a
// record's Source names its ExternalID's namespace (ADR-0060 decision 1) — else the
// slug of the provider the host asked, which is the fact the host always has.
func stampNamespace(source, asked string) string {
	if ns := strings.TrimSpace(source); ns != "" {
		return ns
	}
	return asked
}

// isAuthoritativeNamespace reports whether ns names a registered provider that could
// lead the kind — a Full provider serving its coarse media kind, which is exactly
// the set a Library's Authoritative-provider pointer may name (ADR-0027). A pin to
// any other namespace (`imdb`, which no plugin owns; a Supplement's; an uninstalled
// plugin's) is no pin at all: the lead handles the item, as it always did.
func isAuthoritativeNamespace(cat Catalog, ns, kind string) bool {
	if ns == "" {
		return false
	}
	e, ok := cat.Entry(ns)
	return ok && e.Class == ClassFull && e.Serves(kindGroupFor(kind))
}

// decisionNamespace is the namespace of a Title's record when that record is a
// DECISION (ADR-0060 decision 6), and "" when it is not:
//
//   - chosen or cascaded (ADR-0046's RecordOrigin.Locked()): an Admin's Fix info,
//     Wrong item or Episode pin, or a parent's Cascade — the record row named by
//     RecordNamespace; or
//   - asserted by the FOLDER: a `{tmdb-…}` / `{imdb-…}` token (ADR-0002), read from
//     the identity columns raw, `tmdb` before `imdb`.
//
// A record a pass resolved on its own (store.OriginDerived) is not a decision, whatever its
// namespace, and reads "" here.
func decisionNamespace(t store.Title) string {
	if t.EnrichmentIDOrigin.Locked() && t.RecordNamespace != "" &&
		strings.TrimSpace(t.RecordID(t.RecordNamespace)) != "" {
		return t.RecordNamespace
	}
	for _, ns := range []string{pluginapi.NamespaceTMDB, pluginapi.NamespaceIMDB} {
		if strings.TrimSpace(t.IdentityID(ns)) != "" {
			return ns
		}
	}
	return ""
}

// pinnedProviderFor reports the registry slug of the provider a Title is PINNED to,
// and whether it is pinned at all (ADR-0060 decision 6). A Title is pinned only when
// its record is a decision (decisionNamespace) AND that record's namespace names a
// registered Authoritative provider (isAuthoritativeNamespace) — the namespace IS the
// provider's slug, because a source's namespace is its plugin id (decision 1).
//
// Everything else is unpinned and resolves via the Library's current lead. That
// includes the case this rule exists for: a record a pass resolved on its own in a
// namespace the Library no longer leads with. The next pass re-resolves it via the
// lead, and processLeaf REPLACES the old record (TitleEnrichment.ReplaceRecord) —
// repointing a Library means what it says. It also includes an `imdb` folder token,
// which no Authoritative provider owns: the lead handles it, as it always has.
//
// This replaces a rule keyed on column NAMES: a video Title with any id in the
// column named after TMDB pinned to TMDB, whoever had chosen it, and a Track
// reported its Library's music lead because its column named no source. Both
// guesses are gone with the columns (ADR-0060's recorded behaviour change for
// video; music's pin was a no-op and is now a real one).
//
// The caller decides what a pin to a provider other than the lead means — resolve
// through that provider, or, when a policy change made it unreachable, ORPHAN the
// item to the attention list (issue 06's rule, unchanged).
func pinnedProviderFor(t store.Title, cat Catalog) (string, bool) {
	ns := decisionNamespace(t)
	if !isAuthoritativeNamespace(cat, ns, t.Kind) {
		return "", false
	}
	return ns, true
}

// replacesRecord reports whether a pass's answer in namespace ns REPLACES the
// Title's current record rather than filling beside it — ADR-0060 decision 6's
// deliberate exception to ADR-0045's fill-only rule. It does exactly when the
// current record is nobody's decision (store.OriginDerived), lives in another namespace, and
// the pass actually resolved an id to put in its place. A chosen or cascaded record
// is never replaced here (it either pinned the pass to its own provider, or its
// provider is not registered and it stands untouched beside the lead's answer).
func replacesRecord(t store.Title, ns, id string) bool {
	if strings.TrimSpace(id) == "" || t.EnrichmentIDOrigin.Locked() {
		return false
	}
	cur := t.RecordNamespace
	return cur != "" && cur != ns && strings.TrimSpace(t.RecordID(cur)) != ""
}

// titleExternalIDs is every id the host holds for a Title, keyed by namespace
// (ADR-0060 decision 7): the ids the folder asserts, then the record rows, which
// outrank an identity id in the same namespace (ADR-0045's precedence, the one
// recordIDExpr spells in SQL), then — for music — the recording MBID in its
// precedence (trackRecordID: the record row, else the file's tag). Nil when the
// Title holds none.
func titleExternalIDs(t store.Title) map[string]string {
	ids := map[string]string{}
	for ns, id := range t.IdentityIDs {
		putID(ids, ns, id)
	}
	for ns, id := range t.RecordIDs {
		putID(ids, ns, id)
	}
	putID(ids, pluginapi.NamespaceMusicBrainz, trackRecordID(t))
	if len(ids) == 0 {
		return nil
	}
	return ids
}

// putID sets ids[ns] to a trimmed id, ignoring a blank namespace or id.
func putID(ids map[string]string, ns, id string) {
	ns, id = strings.TrimSpace(ns), strings.TrimSpace(id)
	if ns == "" || id == "" {
		return
	}
	ids[ns] = id
}

// withExternalIDs makes ids the lookup reference's id carrier and fills the five
// named fields FROM IT, so the two can never disagree (issue 13: the map wins on the
// wire anyway, and an in-process provider still reads the named fields). This is how
// TheTVDBID and AniDBID reach a guest at all. ids is not retained; the ref gets its
// own copy.
func withExternalIDs(ref TitleRef, ids map[string]string) TitleRef {
	var own map[string]string
	for ns, id := range ids {
		if own == nil {
			own = make(map[string]string, len(ids))
		}
		putID(own, ns, id)
	}
	if len(own) == 0 {
		own = nil
	}
	ref.ExternalIDs = own
	ref.TMDBID = own[pluginapi.NamespaceTMDB]
	ref.IMDBID = own[pluginapi.NamespaceIMDB]
	ref.MusicbrainzID = own[pluginapi.NamespaceMusicBrainz]
	ref.TheTVDBID = own[pluginapi.NamespaceTheTVDB]
	ref.AniDBID = own[pluginapi.NamespaceAniDB]
	return ref
}

// withExternalID is withExternalIDs adding (or replacing) ONE namespaced id on top
// of whatever the ref already carries. A blank id leaves the ref as it was.
func withExternalID(ref TitleRef, ns, id string) TitleRef {
	if strings.TrimSpace(ns) == "" || strings.TrimSpace(id) == "" {
		return ref
	}
	ids := make(map[string]string, len(ref.ExternalIDs)+1)
	for k, v := range ref.ExternalIDs {
		ids[k] = v
	}
	putID(ids, ns, id)
	return withExternalIDs(ref, ids)
}

// idsIn is the one-entry map a parent's lookup ref starts from, nil for a blank id.
func idsIn(ns, id string) map[string]string {
	ids := map[string]string{}
	putID(ids, ns, id)
	if len(ids) == 0 {
		return nil
	}
	return ids
}

// parentRecord is a browse parent's record: its id and the namespace it belongs to
// (entity_enrichment.external_id + external_id_namespace, ADR-0060 decision 3). It
// is what a parent hands its children, so an Episode resolves under its Show's id
// in the Show's namespace and not in whichever namespace the lead happens to read.
type parentRecord struct {
	ID        string
	Namespace string
}

// storedParentRecord reads a parent's record off its enrichment row. A row that
// has an id but no namespace reads as its kind's default lead.
func storedParentRecord(entityType string, e store.EntityEnrichment) parentRecord {
	id := strings.TrimSpace(e.ExternalID)
	if id == "" {
		return parentRecord{}
	}
	ns := strings.TrimSpace(e.Namespace)
	if ns == "" {
		ns = defaultParentNamespace(entityType)
	}
	return parentRecord{ID: id, Namespace: ns}
}

// showAssertedRecord is the record a Show's FOLDER asserts: its `{tmdb-…}` token,
// else its `{imdb-…}` one (ADR-0002), the same order decisionNamespace reads a
// Title's in. It is a decision (ADR-0060 decision 6), so it pins the Show to its
// namespace's provider whenever that is a registered Authoritative one — which
// `imdb` never is, so an IMDb token leaves the Show with the lead, as a Title's does.
func showAssertedRecord(sh store.Show) parentRecord {
	if id := strings.TrimSpace(sh.TMDBID); id != "" {
		return parentRecord{ID: id, Namespace: pluginapi.NamespaceTMDB}
	}
	if id := strings.TrimSpace(sh.IMDBID); id != "" {
		return parentRecord{ID: id, Namespace: pluginapi.NamespaceIMDB}
	}
	return parentRecord{}
}

// showPin is the Show's record when that record is a DECISION (ADR-0060 decision 6),
// in enrichParent's own precedence — a chosen or cascaded record, else the folder's
// token — and empty when the Show's record is one a pass resolved on its own. It is
// what a pinned Show's CHILDREN follow: a Season and an Episode have no decision of
// their own about which source knows their Show, so they resolve through the same
// provider the Show does. Were they left with the lead, a TMDB-pinned Show in an
// AniDB-led Library would hand AniDB a TMDB series id for every one of them.
func (s *Service) showPin(sh store.Show) parentRecord {
	if e, err := s.store.EntityEnrichmentByID(store.EntityShow, sh.ID); err == nil && e.ExternalIDOrigin.Locked() {
		if rec := storedParentRecord(store.EntityShow, e); rec.ID != "" {
			return rec
		}
	}
	return showAssertedRecord(sh)
}

// episodeShowPin is showPin for an Episode reached on its own (a single-Title
// re-enrich), which arrives with nothing but its id. Anything that is not an
// Episode, or whose Show cannot be read, follows no Show.
func (s *Service) episodeShowPin(t store.Title) parentRecord {
	if t.Kind != "episode" {
		return parentRecord{}
	}
	ec, err := s.store.EpisodeContextForTitle(t.ID)
	if err != nil {
		return parentRecord{}
	}
	sh, err := s.store.ShowByID(ec.ShowID)
	if err != nil {
		return parentRecord{}
	}
	return s.showPin(sh)
}

// assertedParentRecord is showAssertedRecord for a parent named by type and id: a
// Show's folder token, and nothing for every other parent, whose ids no folder
// asserts (an Artist's or Album's tag MBID is decoration, ADR-0049). A failed read
// asserts nothing, which leaves the parent with its stored record or the lead.
func (s *Service) assertedParentRecord(entityType, entityID string) parentRecord {
	if entityType != store.EntityShow {
		return parentRecord{}
	}
	sh, err := s.store.ShowByID(entityID)
	if err != nil {
		return parentRecord{}
	}
	return showAssertedRecord(sh)
}

// parentNamespace is the namespace of a parent's stored record, for a child to
// inherit (ADR-0060 decision 5: an Episode pin and a Cascade inherit the parent's
// namespace). A parent with no record, or a read that fails, lends its kind's
// default lead — a bookkeeping read must not be able to fail a Cascade.
func (s *Service) parentNamespace(entityType, entityID string) string {
	if e, err := s.store.EntityEnrichmentByID(entityType, entityID); err == nil {
		if rec := storedParentRecord(entityType, e); rec.Namespace != "" {
			return rec.Namespace
		}
	}
	return defaultParentNamespace(entityType)
}

// defaultParentNamespace is the namespace a parent's id is read in when nothing
// recorded one: its kind's default lead (ADR-0060 decision 4).
func defaultParentNamespace(entityType string) string {
	switch entityType {
	case store.EntityArtist, store.EntityAlbum:
		return pluginapi.NamespaceMusicBrainz
	default:
		return pluginapi.NamespaceTMDB
	}
}

// leadNamespace is the namespace of the Library's CURRENT lead for an entity kind —
// the lead's slug, since a source's namespace is its plugin id. It is what an
// Admin's pick means when the caller names no namespace (ADR-0060 decision 5), so an
// older client that sends no `source` keeps working.
func (s *Service) leadNamespace(ctx context.Context, libraryID, kind string) (string, error) {
	snap, err := s.snapshotFor(ctx, libraryID)
	if err != nil {
		return "", err
	}
	return snap.config.authoritativeSlugFor(kind), nil
}

// LeadNamespace is leadNamespace for the API layer: the namespace an Enrichment
// override on an item of `kind` in this Library means when the request names none.
func (s *Service) LeadNamespace(ctx context.Context, libraryID, kind string) (string, error) {
	return s.leadNamespace(ctx, libraryID, kind)
}

// ErrUnknownNamespace is an Admin's pick naming a namespace no registered
// Authoritative provider of the item's kind claims (ADR-0060 decision 5). The API
// answers it with a 400 rather than pinning an id nothing can ever look up.
var ErrUnknownNamespace = errors.New("enrich: no authoritative provider claims that namespace")

// CheckTitleNamespace reports whether ns is a namespace an Enrichment override on
// the Title may name: nil when a registered Full provider serving the Title's kind
// claims it (the set a Library's Authoritative-provider pointer may name) or when it
// is the Library's current lead's, else ErrUnknownNamespace. store.ErrNotFound for
// an unknown Title. An empty ns is the caller's "no source", which is always fine:
// it means the lead.
func (s *Service) CheckTitleNamespace(ctx context.Context, titleID, ns string) error {
	t, err := s.store.TitleForEnrichmentByID(titleID)
	if err != nil {
		return err
	}
	return s.checkNamespace(ctx, t.LibraryID, t.Kind, ns)
}

// CheckEntityNamespace is CheckTitleNamespace for a browse parent. store.ErrNotFound
// for an unknown parent.
func (s *Service) CheckEntityNamespace(ctx context.Context, entityType, entityID, ns string) error {
	libraryID, err := s.store.LibraryOfEntity(entityType, entityID)
	if err != nil {
		return err
	}
	return s.checkNamespace(ctx, libraryID, entityKind(entityType), ns)
}

// checkNamespace is the shared rule. The lead is accepted by name as well as through
// the catalog because a fixed provider (a test's, or the global path) carries no
// catalog, and its lead is still what every candidate it returns is stamped with.
func (s *Service) checkNamespace(ctx context.Context, libraryID, kind, ns string) error {
	ns = strings.TrimSpace(ns)
	if ns == "" {
		return nil
	}
	snap, err := s.snapshotFor(ctx, libraryID)
	if err != nil {
		return err
	}
	if ns == snap.config.authoritativeSlugFor(kind) || isAuthoritativeNamespace(snap.catalog, ns, kind) {
		return nil
	}
	return ErrUnknownNamespace
}
