import { useParams } from "react-router-dom";
import { useLibraryName } from "../browse/BackLink";
import MusicShell from "./MusicShell";
import ArtistList from "./ArtistList";

// The music library landing (/music/libraries/:libraryId): the music counterpart
// of the shared LibraryGridScreen. A music library browses Artists → Albums →
// Tracks, so this screen renders the Artist list inside the music shell. The
// library header (name) comes from the app-wide Libraries list.

export default function MusicLibraryScreen() {
  const { libraryId = "" } = useParams();
  const libraryName = useLibraryName(libraryId, "Library");

  return (
    <MusicShell testId="music-library-screen">
      <ArtistList libraryId={libraryId} libraryName={libraryName} />
    </MusicShell>
  );
}
