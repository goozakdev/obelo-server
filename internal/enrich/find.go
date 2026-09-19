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

// findIn asks the snapshot's lead to read query as a reference, then either
// previews the record it names or searches for query as a term.
//
//   - A query the lead cannot read (ErrExternalRefInvalid) is a search term.
//   - A query it reads as a reference is previewed exactly as the externalPreview
//     endpoints preview a paste, and its errors are the paste's errors: an id with
//     no record is ErrNoMatch, and so on.
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
			if err != nil {
				return CandidateSearch{ResolvedRef: true}, err
			}
			return CandidateSearch{Candidates: []Candidate{c}, ResolvedRef: true}, nil
		case errors.Is(err, ErrExternalRefKindMismatch), errors.Is(err, ErrExternalRefUnsupportedKind):
			return CandidateSearch{ResolvedRef: true}, err
		}
		// Anything else — not a reference, or a parser that failed — is a search term.
	}
	cands, err := s.searchIn(ctx, snap, kind, query, opts)
	return CandidateSearch{Candidates: cands}, err
}
