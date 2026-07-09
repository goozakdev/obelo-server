import { useParams } from "react-router-dom";
import { apiClient } from "../api/client";
import { useAsync } from "../browse/useAsync";
import MusicShell from "./MusicShell";
import ArtistList from "./ArtistList";

// The music library landing (/music/libraries/:libraryId): the music counterpart
// of the shared LibraryGridScreen. A music library browses Artists → Albums →
// Tracks, so this screen renders the Artist list inside the music shell. The
// library header (name) comes from a one-shot GET /libraries/{id} (the same fetch
// LibraryGridScreen used before music split out).

export default function MusicLibraryScreen() {
  const { libraryId = "" } = useParams();
  const lib = useAsync(
    (signal) => apiClient.getLibrary(libraryId, signal),
    [libraryId],
  );

  const libraryName = lib.status === "ready" ? lib.data.name : "Library";

  return (
    <MusicShell testId="music-library-screen">
      <ArtistList libraryId={libraryId} libraryName={libraryName} />
    </MusicShell>
  );
}
