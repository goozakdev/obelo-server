import { ApiError } from "../api/errors";
import { errorMessage } from "../screens/errorMessage";

// The paste-time refusals of `POST /links` / `POST /links/{id}/rekey`, turned
// into the one sentence that names the operator's NEXT MOVE (ADR-0055 §3,
// linked-servers issue 06).
//
// These are separate codes on the wire precisely because the move differs in
// every case — "ask for the string again", "ask for a fresh one", "one of you
// needs an upgrade", "their machine is not answering" — and collapsing them into
// one "could not link" is the failure this module exists to prevent. The server's
// own message is short and correct but written for an API; what an Admin standing
// in front of a textarea needs is the instruction.
//
// Anything this does not recognise falls through to the server's message
// verbatim (`errorMessage`), which is always better than a house-written
// "something went wrong": a code added on the server later shows up here as its
// own sentence rather than as a lie.

/** The origins an `obelo-link:` string carries, decoded in the browser.
 *
 * This exists for exactly one sentence: `LINK_UNREACHABLE` says the server tried
 * every address in the invite and none answered, and the operator cannot act on
 * that without knowing WHICH addresses were tried — a friend who typed
 * `http://192.168.1.9:8080` when they meant their tailnet name learns it here and
 * nowhere else. The error carries no origins (it is one refusal for a walk over
 * several addresses), and the invite is in hand on this screen, so it is decoded
 * here.
 *
 * SHAPE ONLY, and never a decision: nothing in the returned strings is trusted or
 * dialed — the server did the dialing and has already refused. A string this
 * cannot read yields `[]` and the sentence simply omits the list, because a
 * malformed invite has its own error (`BAD_INVITE`) and this must not compete
 * with it. */
export function inviteOrigins(invite: string): string[] {
  const rest = invite.trim().replace(/^obelo-link:/, "");
  if (rest === invite.trim()) return [];
  try {
    // base64url, padding tolerated on the way in (the string travels through
    // chat clients and QR readers), exactly as the server's ParseInvite does.
    const b64 = rest.replace(/-/g, "+").replace(/_/g, "/").replace(/=+$/, "");
    const json = atob(b64.padEnd(Math.ceil(b64.length / 4) * 4, "="));
    const parsed: unknown = JSON.parse(json);
    if (!parsed || typeof parsed !== "object") return [];
    const origins = (parsed as { origins?: unknown }).origins;
    if (!Array.isArray(origins)) return [];
    return origins.filter((o): o is string => typeof o === "string" && o !== "");
  } catch {
    return [];
  }
}

/** Turn a thrown error from `createLink` / `rekeyLink` / `syncLink` into the
 * sentence the Linked servers page shows. `invite` is the string that was
 * submitted, used only to list the addresses in the unreachable case. */
export function linkErrorMessage(err: unknown, invite = ""): string {
  if (!(err instanceof ApiError)) return errorMessage(err);

  switch (err.code) {
    case "BAD_INVITE":
      return (
        "That is not an invite this server can read. Ask your friend to send the " +
        "whole obelo-link: string again — a paste that lost its first or last " +
        "character looks exactly like this."
      );

    case "INVITE_EXPIRED":
      return (
        "This invite has expired. Ask your friend to mint a fresh one: an invite " +
        "lasts 24 hours, and minting a new one is the normal way to replace it."
      );

    case "LINK_PROTOCOL": {
      // `upgrade` is the server's own verdict on which side has to move; the two
      // version numbers are shown after it as evidence, never as something the
      // reader has to compare themselves.
      const theirs = numberDetail(err, "theirs");
      const ours = numberDetail(err, "ours");
      const versions =
        theirs !== null && ours !== null
          ? ` (they speak link protocol ${theirs}, this server speaks ${ours}.)`
          : "";
      if (err.details?.upgrade === "ours") {
        return (
          "This server is too old to link with theirs — it is the one that needs " +
          `upgrading.${versions}`
        );
      }
      return (
        "Their server is too old to link with this one — they are the ones who " +
        `need to upgrade Obelo.${versions}`
      );
    }

    case "LINK_SERVER_MISMATCH":
      return (
        "That invite is from a different server than the one you are re-keying. " +
        "Ask this friend for a fresh invite, or paste that one under Link a server " +
        "to add their household as a new link."
      );

    case "LINK_REVOKED":
      return (
        "That server no longer accepts this server's credential — the remote user " +
        "or its device is gone over there. Ask them for a fresh invite and use " +
        "Re-key, which keeps the libraries and your watch state."
      );

    case "LINK_UNREACHABLE": {
      const origins = inviteOrigins(invite);
      const tried = origins.length
        ? ` Tried, in order: ${origins.join(", ")}.`
        : "";
      return (
        "Could not reach their server at any of the addresses in the invite." +
        tried +
        " Nothing about the invite is wrong — their machine did not answer. " +
        `The server reported: ${err.message}`
      );
    }

    default:
      return errorMessage(err);
  }
}

function numberDetail(err: ApiError, key: string): number | null {
  const v = err.details?.[key];
  return typeof v === "number" && Number.isFinite(v) ? v : null;
}
