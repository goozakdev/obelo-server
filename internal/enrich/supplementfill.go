package enrich

// fillFromSupplement is the ONE fill-only rule both chains compose a Supplement's
// answer by (ADR-0002, ADR-0061). It returns meta with sup's fields taken only
// where meta left them empty:
//
//   - Overview is taken when meta has none, or when meta's is SYNTHESIZED (a
//     placeholder the lead composed from structured facts — MusicBrainz's "English
//     rock band from Oxford") and sup's is not. That is the whole of the rule that
//     used to be TheAudioDB's by name: a real biography replaces a placeholder, and
//     never real prose. A synthesized answer still fills an empty Overview, and
//     stays marked synthesized so a later Supplement's real text replaces it in turn.
//   - Name is a display-only override (never identity) taken only when meta left it
//     empty — and never on a record found by SEARCH, where Name is the candidate's own
//     title the host is about to judge it by (ADR-0050); a Supplement's title there
//     would be judged in the candidate's place.
//   - ContentRating and Genres fill only when empty; Artwork merges by role, so a
//     role meta already carries keeps meta's image (mergeArtwork).
//
// Identity (ExternalID, Source), the match facts (Matched, FromSearch) and every
// other field are meta's, untouched.
func fillFromSupplement(meta, sup TitleMetadata) TitleMetadata {
	if meta.Name == "" && !meta.FromSearch {
		meta.Name = sup.Name
	}
	if sup.Overview != "" && (meta.Overview == "" || (meta.OverviewSynthesized && !sup.OverviewSynthesized)) {
		meta.Overview = sup.Overview
		meta.OverviewSynthesized = sup.OverviewSynthesized
	}
	if meta.ContentRating == "" {
		meta.ContentRating = sup.ContentRating
	}
	if len(meta.Genres) == 0 {
		meta.Genres = sup.Genres
	}
	meta.Artwork = mergeArtwork(meta.Artwork, sup.Artwork)
	return meta
}

// mergeArtwork returns base extended with any of add's refs whose role base does
// not already carry — fill-only, so an existing image for a role wins.
func mergeArtwork(base, add []ArtworkRef) []ArtworkRef {
	have := make(map[string]bool, len(base))
	for _, a := range base {
		have[a.Role] = true
	}
	for _, a := range add {
		if a.URL == "" || have[a.Role] {
			continue
		}
		base = append(base, a)
		have[a.Role] = true
	}
	return base
}
