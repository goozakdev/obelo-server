# Watch state keyed to parsed Title identity, not path or file

Watch state is keyed to the parsed Title identity (title+year / embedded id for video; album-artist+album+track for music) and is per-Title, not per-Edition or per-File.

Consequences for the scenarios that would otherwise lose history:
- Renaming a file to fix its name (missing file + new file to the scanner) re-resolves to the same Title → history survives.
- Replacing a 1080p rip with a 4K one is a new Edition under the same Title → history survives.
- Moving the whole library to a new drive changes only paths, not identities → history survives.

Match overrides remain keyed to the folder path (their physical anchor). Renaming/moving a folder drops its override — acceptable because renaming a folder is the user re-asserting identity — and orphaned overrides are surfaced in the Admin attention list, not silently lost.

## Why
Convention-as-authority ([ADR-0002](./0002-naming-convention-is-identity-authority.md)) is only livable if reorganizing files doesn't wipe watch history. Keying to identity rather than path/file is what makes routine library maintenance safe.

## Consequences
- Two distinct works that parse to the same identity would wrongly share watch state; the embedded-id / Match-override disambiguation exists precisely to prevent this.
- Identity parsing must be stable across scanner versions, or an upgrade could silently re-key history; identity-affecting parser changes need migration care.
