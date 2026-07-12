package scanner

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/marioquake/juicebox/internal/store"
)

// Music scanning (issue tv-music/03). A Music Library's identity authority is the
// embedded TAGS (amended ADR-0002), read through the EXISTING Prober/ffprobe
// seam — no new dependency. The scanner probes every audio file, maps its tags
// (path as fallback) to a music identity via MusicIdentityFromTags, and groups
// the results into Artist → Album → Track trees. Albums group by Album Artist
// (falling back to Artist) so a compilation stays one Album.
//
// Unlike Movie/TV, the grouping is NOT one-tree-per-folder: tags can place tracks
// from different folders under one Album, and a single folder can hold multiple
// albums. So the music path walks every audio file in the Library, accumulates by
// identity key, and emits one store.ArtistTree per Artist at the end.

// musicTrack is one probed audio file with its resolved music identity and the
// File subtree the scanner built (reusing the Movie edition logic).
type musicTrack struct {
	id   MusicIdentity
	tree store.TitleTree
	disc int
	trk  int
}

// scanMusicLibrary walks every root of a Music Library, probes each audio file,
// resolves its identity, and writes one ArtistTree per Artist. Files that yield
// no minimal identity (no artist/album/title from tags OR path) go to Unmatched —
// never auto-guessed. Returns the (#Tracks, #Files) found.
func (s *Service) scanMusicLibrary(ctx context.Context, sc *scanCtx, lib store.Library) (int, int, []store.UnmatchedFile, error) {
	dirs := make([]string, 0, len(lib.Roots))
	for _, root := range lib.Roots {
		dirs = append(dirs, root.Path)
	}
	return s.scanMusicDirs(ctx, sc, lib, dirs)
}

// scanMusicDirs is the music scan core over an explicit set of directories: it
// recursively walks each dir, probes every audio file, and writes one ArtistTree
// per Artist found. A full scan passes the Library's roots (scanMusicLibrary); a
// Targeted scan passes an entity's album folders (ADR-0030) — a folder is walked
// whole, so a track belonging to a sibling Album in the same folder is filed too.
func (s *Service) scanMusicDirs(ctx context.Context, sc *scanCtx, lib store.Library, dirs []string) (int, int, []store.UnmatchedFile, error) {
	// Discover every audio file under each dir, plus the album-folder → cover
	// artwork map (cover.jpg / folder.jpg honored before embedded art).
	var audioFiles []string
	albumArt := map[string]string{} // folder path → local cover image path
	for _, dir := range dirs {
		files, art, unresolved, err := discoverMusicRoot(dir)
		if err != nil {
			return 0, 0, nil, err
		}
		audioFiles = append(audioFiles, files...)
		// Subtrees that failed to read after retries: recorded so scanRoots can
		// skip the soft-delete for anything beneath them (a transient smbfs blip
		// must not mark real Tracks Missing — ADR-0008).
		sc.unresolved = append(sc.unresolved, unresolved...)
		for k, v := range art {
			if _, ok := albumArt[k]; !ok {
				albumArt[k] = v
			}
		}
	}
	sort.Strings(audioFiles)

	var unmatched []store.UnmatchedFile
	// Accumulate tracks per Artist identity (album_artist-derived), preserving
	// stable display fields from the first track seen for that artist/album.
	type artistAcc struct {
		artist store.Artist
		albums map[string]*store.AlbumTree // albumKey → tree
		order  []string                    // album insertion order (stable)
	}
	artists := map[string]*artistAcc{}
	var artistOrder []string
	filesFound := 0

	for _, path := range audioFiles {
		if err := ctx.Err(); err != nil {
			return 0, 0, nil, err
		}
		name := filepath.Base(path)
		if isJunk(name, fileSize(path)) {
			continue // sample/junk ignored entirely
		}
		sc.seen[path] = true // present on disk this walk (drives soft-delete)

		// Probe via the existing Prober to read tags + technical attributes. Tags ARE
		// the music identity authority (amended ADR-0002), so a Music scan always
		// reads them from ffprobe rather than reusing a stored snapshot — the
		// identity must come from the file's current tags. (The File's id/added_at
		// still stay stable across rescans via writeTitleSubtree's by-path reclaim.)
		sc.probes++
		media, err := s.prober.Probe(ctx, path)
		if err != nil {
			unmatched = append(unmatched, unmatchedFile(path, "could not probe audio file: "+err.Error()))
			continue
		}

		// A folder-anchored Match override on this track's ALBUM folder (the dir
		// holding it) overrules the tag/path Album grouping and persists across
		// rescans — the Music interpretation of the same folder-keyed override the
		// Movie/TV paths use (issue tv-music/04). It also rescues a file the tags +
		// path could not identify.
		var ov *AlbumOverride
		if o, has := sc.overrides[filepath.Dir(path)]; has {
			ov = &AlbumOverride{Album: o.Title, Year: o.Year, Key: o.IdentityKey}
		}
		id, ok := MusicIdentityFromTagsWithOverride(media.Tags, path, ov)
		if !ok {
			unmatched = append(unmatched, unmatchedFile(path, "no music identity from tags or path"))
			continue
		}

		tree := s.buildTrackTree(lib, id, path, media)
		filesFound++

		acc := artists[id.ArtistKey]
		if acc == nil {
			// Present a comma-inverted Album Artist ("Beatles, The") in natural
			// order; the tag-derived ArtistKey is untouched so the row still matches.
			name := uninvertTitle(id.AlbumArtist)
			acc = &artistAcc{
				artist: store.Artist{
					ID:          uuid.NewString(),
					LibraryID:   lib.ID,
					Name:        name,
					IdentityKey: id.ArtistKey,
					SortName:    sortTitle(name),
				},
				albums: map[string]*store.AlbumTree{},
			}
			artists[id.ArtistKey] = acc
			artistOrder = append(artistOrder, id.ArtistKey)
		}
		at := acc.albums[id.AlbumKey]
		if at == nil {
			albumTitle := uninvertTitle(id.Album)
			at = &store.AlbumTree{
				Title:       albumTitle,
				Year:        id.AlbumYear,
				IdentityKey: id.AlbumKey,
				SortTitle:   sortTitle(albumTitle),
				ArtworkPath: albumArt[filepath.Dir(path)],
			}
			acc.albums[id.AlbumKey] = at
			acc.order = append(acc.order, id.AlbumKey)
		}
		// A later track of the album may carry the year / artwork the first lacked.
		if at.Year == 0 && id.AlbumYear != 0 {
			at.Year = id.AlbumYear
		}
		if at.ArtworkPath == "" {
			if art := albumArt[filepath.Dir(path)]; art != "" {
				at.ArtworkPath = art
			}
		}
		at.Tracks = append(at.Tracks, store.TrackTree{
			TitleTree:   tree,
			DiscNumber:  id.Disc,
			TrackNumber: id.Track,
		})
	}

	// Emit one ArtistTree per Artist, albums sorted by (year, title), tracks by
	// (disc, track, title), and persist each.
	tracksFound := 0
	for _, ak := range artistOrder {
		acc := artists[ak]
		tree := store.ArtistTree{Artist: acc.artist}
		albumKeys := append([]string(nil), acc.order...)
		sort.SliceStable(albumKeys, func(i, j int) bool {
			a, b := acc.albums[albumKeys[i]], acc.albums[albumKeys[j]]
			if a.Year != b.Year {
				return a.Year < b.Year
			}
			return a.SortTitle < b.SortTitle
		})
		for _, alk := range albumKeys {
			at := acc.albums[alk]
			sort.SliceStable(at.Tracks, func(i, j int) bool {
				ti, tj := at.Tracks[i], at.Tracks[j]
				if ti.DiscNumber != tj.DiscNumber {
					return ti.DiscNumber < tj.DiscNumber
				}
				if ti.TrackNumber != tj.TrackNumber {
					return ti.TrackNumber < tj.TrackNumber
				}
				return ti.Title.SortTitle < tj.Title.SortTitle
			})
			tracksFound += len(at.Tracks)
			tree.Albums = append(tree.Albums, *at)
		}
		if err := s.store.UpsertArtistTree(tree); err != nil {
			return 0, 0, nil, err
		}
	}

	return tracksFound, filesFound, unmatched, nil
}

// buildTrackTree builds the store.TitleTree for one Track: a single Edition with
// the one audio File (and its Streams), keyed by the Track's stable identity. A
// Track has no quality Editions or parts — one file, one Edition — so the Movie
// edition machinery is not needed; the File/Stream construction mirrors buildFile.
func (s *Service) buildTrackTree(lib store.Library, id MusicIdentity, path string, media MediaInfo) store.TitleTree {
	editionID := uuid.NewString()
	f := buildFile(editionID, path, media)
	f.Mtime = fileMtime(path)
	f.Present = true
	if sz := fileSize(path); sz > 0 {
		f.SizeBytes = sz
	}

	return store.TitleTree{
		Title: store.Title{
			ID:          uuid.NewString(),
			LibraryID:   lib.ID,
			Kind:        "track",
			Title:       id.Title,
			IdentityKey: id.TrackKey,
			SortTitle:   sortTitle(id.Title),
			// A path-fallback Track (no tags) is a best-effort parse → needs-review
			// (naming-convention.md "Filed + needs-review").
			NeedsReview: !id.FromTags,
		},
		Editions: []store.Edition{{ID: editionID, Name: "", Files: []store.File{f}}},
	}
}

// readDirBackoffs is the retry schedule for a transient directory-read failure
// (see readDirResilient). Cumulative ~1s across the four waits — enough to ride
// out the common sub-second smbfs blip without stalling a scan when a subtree is
// genuinely unreadable (it then falls through to skip-and-record).
var readDirBackoffs = []time.Duration{
	50 * time.Millisecond, 150 * time.Millisecond, 300 * time.Millisecond, 500 * time.Millisecond,
}

// readDirResilient reads a directory, retrying a few times with backoff to ride
// out a transient failure. A network filesystem (an SMB mount on macOS) can
// return a spurious ENOENT/EIO for a directory that DOES exist — its parent just
// listed it — when it is opened under the burst of a deep walk: smbfs briefly
// negative-caches the failed lookup, so the same directory that fails mid-walk
// opens fine in isolation a moment later. A short retry rides out the common blip;
// a failure that outlasts the budget is returned so the caller skips (never
// aborts) that subtree.
func readDirResilient(dir string) ([]os.DirEntry, error) {
	for attempt := 0; ; attempt++ {
		entries, err := os.ReadDir(dir)
		if err == nil {
			return entries, nil
		}
		if attempt >= len(readDirBackoffs) {
			return nil, err
		}
		time.Sleep(readDirBackoffs[attempt])
	}
}

// discoverMusicRoot recursively walks a Music Library root, returning every audio
// file, a map of album-folder → local cover image (cover.jpg/folder.jpg), and the
// list of subdirectories that could not be read after retries (unresolved). A
// non-existent root is treated as empty (an unmounted volume); a real error on
// the root itself propagates. Unlike the Movie/TV discovery (one level), music
// walks the whole tree because tag-based identity does not depend on a fixed
// folder depth.
//
// A subdirectory that fails to read mid-walk (a transient smbfs ENOENT under
// load, per readDirResilient) is NOT fatal: it is skipped and appended to
// unresolved so the caller can suppress the destructive soft-delete for anything
// beneath it (ADR-0008) — a spurious read blip must never mark real content
// Missing, nor throw away the thousands of files the rest of the walk found. This
// replaces filepath.WalkDir, whose walkFn could only abort or skip, not retry.
func discoverMusicRoot(root string) (audio []string, art map[string]string, unresolved []string, err error) {
	art = map[string]string{}
	info, statErr := os.Stat(root)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return nil, art, nil, nil
		}
		return nil, nil, nil, statErr
	}
	if !info.IsDir() {
		return nil, art, nil, nil
	}

	var walk func(dir string)
	walk = func(dir string) {
		entries, rerr := readDirResilient(dir)
		if rerr != nil {
			unresolved = append(unresolved, dir)
			return
		}
		for _, e := range entries {
			path := filepath.Join(dir, e.Name())
			if e.IsDir() {
				walk(path)
				continue
			}
			name := e.Name()
			if isAudio(name) {
				audio = append(audio, path)
				continue
			}
			if role := musicArtworkRole(name); role != "" {
				if _, ok := art[dir]; !ok {
					art[dir] = path
				}
			}
		}
	}
	walk(root)
	return audio, art, unresolved, nil
}

// musicArtworkRole returns a non-empty role when name is a recognized local album
// cover (cover.jpg / folder.jpg), else "" (naming-convention.md "Local artwork":
// music uses cover.jpg/folder.jpg with embedded cover art as fallback).
func musicArtworkRole(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if !imageExtensions[ext] {
		return ""
	}
	base := strings.ToLower(strings.TrimSuffix(name, ext))
	if base == "cover" || base == "folder" {
		return "cover"
	}
	return ""
}
