import { describe, it, expect } from "vitest";
import { ApiError, NetworkError } from "../api/errors";
import { inviteOrigins, linkErrorMessage } from "./linkErrors";

// The paste-time refusals, code by code. This is the suite that keeps the six
// codes from collapsing into one "could not link": each assertion is about the
// operator's NEXT MOVE being present and being DIFFERENT from its neighbours'.

/** Build an `obelo-link:` string the way the sharer does — base64url of the JSON
 * payload, unpadded — so the decoder is exercised against the real shape rather
 * than a hand-written fixture that happens to suit it. */
function invite(payload: Record<string, unknown>): string {
  const json = JSON.stringify(payload);
  const b64 = btoa(json).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  return `obelo-link:${b64}`;
}

const INVITE = invite({
  v: 1,
  id: "srv-1",
  name: "Sam's server",
  origins: ["https://sam.example.net", "http://obelo.tail1a2b.ts.net"],
  code: "abc123",
  exp: "2026-09-04T00:00:00Z",
});

function apiError(
  code: string,
  status = 400,
  message = "server said so",
  details?: Record<string, unknown>,
): ApiError {
  return new ApiError(status, code, message, details);
}

describe("inviteOrigins", () => {
  it("reads the addresses out of a real invite string, in order", () => {
    expect(inviteOrigins(INVITE)).toEqual([
      "https://sam.example.net",
      "http://obelo.tail1a2b.ts.net",
    ]);
  });

  it("tolerates surrounding whitespace and base64 padding, as the server does", () => {
    const padded = `  ${INVITE}==  `;
    expect(inviteOrigins(padded)).toEqual([
      "https://sam.example.net",
      "http://obelo.tail1a2b.ts.net",
    ]);
  });

  it("yields nothing — never throws — for a string it cannot read", () => {
    // Each of these has its own error (BAD_INVITE); this decoder must not compete
    // with it, so its only failure mode is silence.
    expect(inviteOrigins("")).toEqual([]);
    expect(inviteOrigins("hello")).toEqual([]);
    expect(inviteOrigins("obelo-link:not-base64!!")).toEqual([]);
    expect(inviteOrigins(`obelo-link:${btoa("[1,2,3]")}`)).toEqual([]);
    expect(inviteOrigins(`obelo-link:${btoa('{"origins":"nope"}')}`)).toEqual([]);
  });
});

describe("linkErrorMessage — one code, one next move", () => {
  it("BAD_INVITE says to ask for the string again", () => {
    const msg = linkErrorMessage(apiError("BAD_INVITE"), "rubbish");
    expect(msg).toMatch(/obelo-link:/);
    expect(msg).toMatch(/send the whole/i);
  });

  it("INVITE_EXPIRED says to ask for a FRESH one, not to re-copy this one", () => {
    const msg = linkErrorMessage(apiError("INVITE_EXPIRED", 410), INVITE);
    expect(msg).toMatch(/expired/i);
    expect(msg).toMatch(/fresh one/i);
    expect(msg).toMatch(/24 hours/);
  });

  it("LINK_PROTOCOL names THEIR side when the server says `theirs`", () => {
    const msg = linkErrorMessage(
      apiError("LINK_PROTOCOL", 409, "different versions", {
        theirs: 1,
        ours: 2,
        upgrade: "theirs",
      }),
      INVITE,
    );
    expect(msg).toMatch(/their server is too old/i);
    expect(msg).toMatch(/they are the ones who need to upgrade/i);
    expect(msg).toContain("they speak link protocol 1, this server speaks 2");
  });

  it("LINK_PROTOCOL names OUR side when the server says `ours`", () => {
    const msg = linkErrorMessage(
      apiError("LINK_PROTOCOL", 409, "different versions", {
        theirs: 3,
        ours: 2,
        upgrade: "ours",
      }),
      INVITE,
    );
    expect(msg).toMatch(/this server is too old/i);
    expect(msg).toMatch(/it is the one that needs upgrading/i);
    // The two sentences must not be interchangeable: the whole PRD success
    // criterion is a message naming WHICH side to upgrade.
    expect(msg).not.toMatch(/they are the ones/i);
  });

  it("LINK_UNREACHABLE LISTS THE ADDRESSES TRIED and quotes the server's reason", () => {
    const msg = linkErrorMessage(
      apiError("LINK_UNREACHABLE", 503, "dial tcp 10.0.0.4:443: connection refused"),
      INVITE,
    );
    expect(msg).toContain("https://sam.example.net");
    expect(msg).toContain("http://obelo.tail1a2b.ts.net");
    expect(msg).toContain("dial tcp 10.0.0.4:443: connection refused");
    // Nothing about the invite was wrong — saying so is what stops the operator
    // re-asking their friend for a string that is perfectly fine.
    expect(msg).toMatch(/nothing about the invite is wrong/i);
  });

  it("LINK_UNREACHABLE still reads without a decodable invite", () => {
    const msg = linkErrorMessage(apiError("LINK_UNREACHABLE", 503, "timeout"), "");
    expect(msg).toMatch(/could not reach their server/i);
    expect(msg).not.toMatch(/tried, in order/i);
  });

  it("LINK_SERVER_MISMATCH explains it is a DIFFERENT household's invite", () => {
    const msg = linkErrorMessage(apiError("LINK_SERVER_MISMATCH", 409), INVITE);
    expect(msg).toMatch(/different server/i);
    expect(msg).toMatch(/link a server/i);
  });

  it("LINK_REVOKED points at Re-key and promises the watch state survives", () => {
    const msg = linkErrorMessage(apiError("LINK_REVOKED", 409), INVITE);
    expect(msg).toMatch(/no longer accepts/i);
    expect(msg).toMatch(/re-key/i);
    expect(msg).toMatch(/watch state/i);
  });

  it("falls through to the server's own message for a code it does not know", () => {
    expect(linkErrorMessage(apiError("SOMETHING_NEW", 500, "the server's words"))).toBe(
      "the server's words",
    );
  });

  it("reports an unreachable OWN server as such, not as a link failure", () => {
    expect(linkErrorMessage(new NetworkError("boom"))).toMatch(/could not reach the server/i);
  });
});
