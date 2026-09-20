package enrich

import (
	"context"
	"errors"
	"strings"
)

// CandidateSearch is what the Edit-item and Fix-info pickers are answered with: a
// page of candidates, and whether the query was a PASTED REFERENCE the lead read
// and resolved rather than a term it searched for.
//
// ResolvedRef exists so the client never has to know which source leads a Library
// or what that source's ids look like (.scratch/bundled-plugins issue 12). The
// picker used to decide "is this a pasted id?" with a local regex per provider —
// TMDB ids are numbers, MusicBrainz ids are UUIDs — which is a question only the
// lead can answer once a Library can be led by an Installed plugin. The server asks
// the lead instead (its external-ref capability, else the host's own reader for the
// lead's namespace), and the picker auto-selects a resolved candidate.
type CandidateSearch struct {
	Candidates  []Candidate
	ResolvedRef bool
}

// FindTitleCandidates answers a single Title's picker: its kind, asked of its
// Library's lead, which first reads the query as a pasted id-or-URL. The service
// owns the lean existence+kind read, so store.ErrNotFound for an unknown Title
// flows to the handler as a 404 without a join-heavy detail fetch.
func (s *Service) FindTitleCandidates(ctx context.Context, titleID, query string, opts SearchOptions) (CandidateSearch, error) {
	t, err := s.store.TitleForEnrichmentByID(titleID)
	if err != nil {
		return CandidateSearch{}, err // ErrNotFound flows through
	}
	snap, err := s.snapshotFor(ctx, t.LibraryID)
	if err != nil {
		return CandidateSearch{}, err
	}
	return s.findIn(ctx, snap, t.Kind, query, opts)
}

// FindEntityCandidates is the browse-parent (Show/Artist/Album) analogue, deriving
// the kind from the entity type (ADR-0019). store.ErrNotFound for an unknown parent.
func (s *Service) FindEntityCandidates(ctx context.Context, entityType, entityID, query string, opts SearchOptions) (CandidateSearch, error) {
	snap, err := s.entitySnapshot(ctx, entityType, entityID)
	if err != nil {
		return CandidateSearch{}, err
	}
	return s.findIn(ctx, snap, entityKind(entityType), query, opts)
}

// FindCandidatesForKind is the Unmatched-file analogue, for a bare kind: no Title
// exists yet, so it reads and searches against the global snapshot exactly as
// SearchCandidates and PreviewExternalForKind do (issue 16 deviation 2 — its apply
// is fix-match, which stores a TMDB identity id).
func (s *Service) FindCandidatesForKind(ctx context.Context, kind, query string, opts SearchOptions) (CandidateSearch, error) {
	return s.findIn(ctx, s.snapshot(), kind, query, opts)
}

// looksLikeLink reports whether a query was plainly PASTED as a reference rather
// than typed as a term: a URL with a scheme, or a bare host/path like
// "musicbrainz.org/artist/…". It knows nothing about any source's id shapes — that
// is the lead's business (ADR-0057 decision 3) — only about what a human typing a
// title does not type. A term with a slash in it but no dot ("AC/DC") is a term.
func looksLikeLink(query string) bool {
	q := strings.TrimSpace(query)
	return strings.Contains(q, "://") || (strings.Contains(q, "/") && strings.Contains(q, "."))
}

// findIn asks the snapshot's lead to read query as a reference, then either
// previews the record it names or searches for query as a term.
//
//   - A query the lead cannot read (ErrExternalRefInvalid) is a search term.
//   - A query it reads as a reference is previewed exactly as the externalPreview
//     endpoints preview a paste, and its errors are the paste's errors.
//   - EXCEPT that an id naming no record (ErrNoMatch) falls back to searching for
//     the query, because a bare id is also a search term: "1917" and "2012" are
//     films as well as TMDB ids, and an Admin who typed one meaning the title
//     should get the film. A LINK never falls back — pasting a URL says plainly
//     that a reference was meant, so it keeps its "no record for that id" — and
//     neither does a fallback search that finds nothing, which answers with the
//     paste's error as well. So a stale bare id still reports itself as stale
//     rather than as an empty search.
//   - A reference of the WRONG kind (ErrExternalRefKindMismatch) or an unsupported
//     one (ErrExternalRefUnsupportedKind) is returned as that error. The Admin
//     plainly pasted a link, and "that's an album link, this is an artist" is the
//     answer they need — not a free-text search for a URL.
//
// Whenever the query was read as a reference, ResolvedRef is set — on an error too —
// so the caller reports a failure in the paste's terms (a stale id is "no record
// for that id", not "search failed").
//
// Only the FIRST page reads: a "show more" request (Offset > 0) is always the
// continuation of a search.
func (s *Service) findIn(ctx context.Context, snap providerSnapshot, kind, query string, opts SearchOptions) (CandidateSearch, error) {
	if strings.TrimSpace(query) != "" && opts.Offset == 0 {
		_, err := s.externalRef(ctx, snap, kind, query)
		switch {
		case err == nil:
			c, err := s.previewExternal(ctx, snap, kind, query)
			switch {
			case err == nil:
				return CandidateSearch{Candidates: []Candidate{c}, ResolvedRef: true}, nil
			case errors.Is(err, ErrNoMatch) && !looksLikeLink(query):
				// The id read fine and names nothing. A bare id is also a perfectly good
				// SEARCH TERM — "1917" and "2012" are films as well as TMDB ids — so the
				// term gets its search rather than a dead end. If that finds nothing
				// either, the paste's own answer is the better one and comes back below.
				if cands, serr := s.searchIn(ctx, snap, kind, query, opts); serr == nil && len(cands) > 0 {
					return CandidateSearch{Candidates: cands}, nil
				}
				return CandidateSearch{ResolvedRef: true}, err
			default:
				return CandidateSearch{ResolvedRef: true}, err
			}
		case errors.Is(err, ErrExternalRefKindMismatch), errors.Is(err, ErrExternalRefUnsupportedKind):
			return CandidateSearch{ResolvedRef: true}, err
		}
		// Anything else — not a reference, or a parser that failed — is a search term.
	}
	cands, err := s.searchIn(ctx, snap, kind, query, opts)
	return CandidateSearch{Candidates: cands}, err
}
