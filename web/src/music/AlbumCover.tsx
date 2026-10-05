import type { Album } from "../api/types";
import Poster from "../browse/Poster";
import { albumArtworkUrl } from "../browse/albumArt";

// An Album's cover: the album artwork endpoint when the Album reports a cover,
// otherwise the initials placeholder rendered directly (an Album id is not a Title
// id, so <Poster>'s title-keyed fallback request would only 404). `lazy` is off for
// the detail hero, which is always above the fold.
export default function AlbumCover({ album, lazy = true }: { album: Album; lazy?: boolean }) {
  if (!album.hasArtwork) return <Poster titleId={album.id} title={album.title} src={null} />;
  return (
    <img
      className="poster poster-img"
      data-testid="poster-img"
      src={albumArtworkUrl(album.id, album.artworkVersion)}
      alt={`${album.title} cover`}
      loading={lazy ? "lazy" : undefined}
    />
  );
}
