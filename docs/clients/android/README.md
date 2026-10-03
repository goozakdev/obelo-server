# Android client

The Android client (phone and tablet first, Android TV later) lives in its own repo:
**`~/src/obelo-android`** (no remote yet; `applicationId` `dev.goozak.obelo`). It plays through
**libmpv**, like the Apple client, so the Apple TV handoff bundle in [`../appletv/`](../appletv/) —
the integration playbook and capability profile above all — applies to it largely unchanged.

What differs, as far as the server is concerned:

- `device.platform` is **`"android"`** (the field is free text; no server change needed).
- Its second player is **Google Cast** (Default Media Receiver), not AirPlay. It uses the same
  Stream token (`features.streamToken`, token in the URL path) and a narrow HLS capability profile.
- It pins this repo's docs at `docs/backend/` in its own tree, refreshed by `make refresh-backend-docs`.

The Android repo's `docs/spec.md` is the source of truth for its scope; this file is only a pointer.
