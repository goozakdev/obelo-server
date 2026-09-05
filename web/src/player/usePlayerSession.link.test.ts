import { describe, it, expect } from "vitest";
import { ApiError } from "../api/errors";
import { linkOutageMessage } from "./usePlayerSession";

// The two refusals a Title in a LINKED Library can come back with (ADR-0056 §6,
// linked-servers issues 09 and 10), and the sentences they map to.
//
// They are separate codes because the next move differs — one fixes itself, the
// other needs a person — so the test that matters is that the two sentences are
// not interchangeable, and that neither reads as a fault of this household's own
// server, which is the thing a bare "playback failed" would imply.

describe("linkOutageMessage", () => {
  it("LINK_UNREACHABLE says the friend's server is down and to try again", () => {
    const msg = linkOutageMessage(new ApiError(503, "LINK_UNREACHABLE", "no answer"));
    expect(msg).toMatch(/friend's server/i);
    expect(msg).toMatch(/can't be reached/i);
    expect(msg).toMatch(/try again/i);
    // Nothing here has been lost, and a viewer who thinks otherwise goes looking
    // for a problem on this side that does not exist.
    expect(msg).toMatch(/nothing here is lost/i);
  });

  it("LINK_REVOKED says access was withdrawn and names the admin's fix", () => {
    const msg = linkOutageMessage(new ApiError(503, "LINK_REVOKED", "credential dead"));
    expect(msg).toMatch(/friend's server/i);
    expect(msg).toMatch(/withdrawn access/i);
    expect(msg).toMatch(/fresh invite/i);
    expect(msg).toMatch(/linked servers/i);
    // It will NEVER come back on its own, so it must not invite a retry.
    expect(msg).not.toMatch(/try again/i);
  });

  it("is null for everything else, so ordinary errors keep their own message", () => {
    expect(linkOutageMessage(new ApiError(503, "SERVER_BUSY", "busy"))).toBeNull();
    expect(linkOutageMessage(new ApiError(501, "TRANSCODE_REQUIRED", "nope"))).toBeNull();
    expect(linkOutageMessage(new ApiError(404, "NOT_FOUND", "gone"))).toBeNull();
  });
});
