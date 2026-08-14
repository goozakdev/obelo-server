import { useCallback, useEffect, useState } from "react";
import { apiClient } from "../api/client";
import { appEvents } from "../events/enrichEvents";
import { errorMessage } from "../screens/errorMessage";
import { formatDate } from "../time";
import type { TailnetSettingsView, UpdateTailnetInput } from "../api/types";
import ConfirmDialog from "./ConfirmDialog";

// The Remote access admin screen (ADR-0043, tailscale/04): the whole lifecycle of
// this Server's Tailnet node — connect, the login link, the address, key expiry,
// disconnect and forget — in one place, behind RequireAdmin and still
// server-enforced (the /settings API is Admin scope regardless of the client
// gate). AdminProvidersScreen is the model for the fetch/save/error shape.
//
// THE SCREEN IS A STATE MACHINE'S FACE. Six states an operator actually sits in:
//
//   off              what this is, what it costs, and a Connect button
//   connecting       asked to come up, no link yet — say so, do not spin silently
//   waitingForLogin  the login.tailscale.com link, which IS the content
//   connected        the address, copyable, as the primary object on the screen
//   expired          a fresh link, framed as re-authorize rather than as a fault
//   failed           the server's message, VERBATIM
//
// Two of those carry the product weight. The login link arrives SECONDS AFTER
// Connect, asynchronously, over the `tailscaleState` SSE nudge — an operator
// staring at an unchanged screen concludes the button did not work and clicks it
// again, so the link must land with no reload. And the "every device needs
// Tailscale" cost belongs in the OFF state, where it can still change somebody's
// mind, not in the connected state where it is too late to be useful.
//
// The event is a nudge and the GET is the truth (the event carries no payload on
// purpose): every transition refetches.

/** How close to the key-expiry date the standing warning appears. ADR-0043 puts
 * it at ~14 days: far enough out that an operator who only touches the server at
 * weekends still sees it before remote access silently stops, close enough that
 * it is not permanent furniture on a node with 180 days left. */
export const EXPIRY_WARNING_DAYS = 14;

const MS_PER_DAY = 86_400_000;

/** What the screen renders, derived from the one payload. A discriminated union
 * rather than a pile of booleans, because every branch below must be exhaustive:
 * a state that renders nothing is an operator staring at a blank panel. */
export type TailnetPhase =
  | { kind: "off" }
  | { kind: "connecting" }
  | { kind: "waitingForLogin"; loginURL: string }
  | { kind: "expired"; loginURL: string; expiredAt: string }
  | { kind: "connected"; expiresInDays: number | null }
  | { kind: "failed"; message: string };

/** Map a settings payload onto the phase the screen renders.
 *
 * `expired` IS a server state (`keyExpired`), and this function deliberately does
 * not second-guess it. It was briefly derived here instead — comparing `keyExpiry`
 * against now — because the server had no way to say it; the server now asserts it
 * on positive evidence and checks it BEFORE its ordinary state mapping, so a
 * lapsed key never arrives as `needsLogin`. The derivation was removed rather than
 * kept alongside: two sources of truth for one distinction eventually disagree,
 * and the one that disagrees on a clock skew is this one.
 *
 * There is no version skew to defend against — the SPA is embedded in the binary
 * that serves it (ADR-0012), so a screen that understands `keyExpired` is only
 * ever served by a server that emits it.
 *
 * An unrecognized state falls through to `off`: the default posture is the
 * feature being off, and a Connect button is the one control that is never wrong.
 */
export function tailnetPhase(
  view: TailnetSettingsView,
  now: number = Date.now(),
): TailnetPhase {
  const s = view.status;
  const expiry = s.keyExpiry ? Date.parse(s.keyExpiry) : NaN;
  const hasExpiry = Number.isFinite(expiry);
  switch (s.state) {
    case "error":
      return {
        kind: "failed",
        // Empty would be a panel that says a thing went wrong and refuses to say
        // what; the server always sends one, and this is the floor.
        message: s.lastError || "The Tailnet node could not start.",
      };
    case "running":
      return {
        kind: "connected",
        expiresInDays: hasExpiry ? Math.ceil((expiry - now) / MS_PER_DAY) : null,
      };
    case "keyExpired":
      return {
        kind: "expired",
        loginURL: s.loginURL ?? "",
        expiredAt: s.keyExpiry ?? "",
      };
    case "needsLogin":
      return { kind: "waitingForLogin", loginURL: s.loginURL ?? "" };
    case "starting":
      return { kind: "connecting" };
    default:
      return { kind: "off" };
  }
}

/** The address a client is told to use: the MagicDNS name with the scheme this
 * node ACTUALLY SERVES on the Tailnet.
 *
 * The scheme comes from `status.httpsBound` — an observed bind — and never from
 * `httpsEnabled`, which is only the operator's request. It read the setting once,
 * and that was the bug: tailnet HTTPS needs MagicDNS and HTTPS certificates
 * enabled in the Tailscale console, which this server can neither set nor detect
 * ahead of time, so ticking the box without doing the console half produced a
 * saved setting, a confident `https://obelo.tail1a2b.ts.net`, and an address that
 * refuses connections. Everything underneath was behaving correctly — `:80` kept
 * serving, the failure was logged, the retry was armed — and the one surface the
 * operator looks at said the opposite, which sends them off to debug their client.
 *
 * Tailnet `:80` is bound whenever the node is running and has no prerequisites at
 * all, so the `http://` form is always the true answer when `:443` is not up. */
export function tailnetAddress(view: TailnetSettingsView): string {
  const fqdn = view.status.fqdn ?? "";
  if (!fqdn) return "";
  return `${view.status.httpsBound ? "https" : "http"}://${fqdn}`;
}

/** What the panel beside the HTTPS toggle says. Three cases, deliberately
 * distinct: `off` (nothing to report), `bound` (asked for and serving), and
 * `unbound` (asked for and NOT serving — the case the operator has to be told
 * about, carrying the server's own message).
 *
 * `off` covers two situations that read identically on screen because they say
 * the same thing — nothing: HTTPS was not asked for, and the NODE IS NOT UP. The
 * second matters. A stopped node is not trying to bind anything, so an amber
 * "HTTPS is not serving" beside it would be a failure the server has not had —
 * the mirror image of the bug this panel exists to fix, and on the one screen
 * that should know better. The node's own state is the panel above; this one has
 * nothing to add to it.
 *
 * It is keyed on the SAVED view rather than the draft: this describes what the
 * server is doing, and a half-typed form must not make the panel claim anything. */
export type TailnetHTTPSState =
  | { kind: "off" }
  | { kind: "bound" }
  | { kind: "unbound"; message: string };

export function tailnetHTTPSState(view: TailnetSettingsView): TailnetHTTPSState {
  if (!view.httpsEnabled || view.status.state !== "running") return { kind: "off" };
  if (view.status.httpsBound) return { kind: "bound" };
  return { kind: "unbound", message: view.status.httpsError ?? "" };
}

/** The editable settings, held as a draft so an in-flight SSE refetch cannot
 * rewrite a field the operator is halfway through typing. */
interface SettingsDraft {
  hostname: string;
  controlURL: string;
  httpsEnabled: boolean;
}

function draftFrom(view: TailnetSettingsView): SettingsDraft {
  return {
    hostname: view.hostname,
    controlURL: view.controlURL,
    httpsEnabled: view.httpsEnabled,
  };
}

export default function AdminRemoteAccessScreen() {
  const [view, setView] = useState<TailnetSettingsView | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [draft, setDraft] = useState<SettingsDraft | null>(null);
  // Which imperative verb is in flight (null = none), so the whole action bar
  // disables together and a double-click cannot connect twice.
  const [busy, setBusy] = useState<null | "connect" | "disconnect" | "forget">(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  const [confirmForget, setConfirmForget] = useState(false);
  const [copied, setCopied] = useState(false);

  // Adopt a fresh payload. The draft is seeded ONCE (and re-seeded after a save):
  // a `tailscaleState` nudge can arrive at any moment, and adopting the server's
  // hostname on every refetch would delete what the operator is typing.
  const adopt = useCallback((next: TailnetSettingsView) => {
    setView(next);
    setDraft((cur) => cur ?? draftFrom(next));
  }, []);

  const load = useCallback(
    async (signal?: AbortSignal) => {
      try {
        const next = await apiClient.getTailnet(signal);
        if (signal?.aborted) return;
        adopt(next);
        setLoadError(null);
      } catch (err) {
        if (signal?.aborted) return;
        if (err instanceof DOMException && err.name === "AbortError") return;
        setLoadError(errorMessage(err));
      }
    },
    [adopt],
  );

  useEffect(() => {
    const ctrl = new AbortController();
    void load(ctrl.signal);
    return () => ctrl.abort();
  }, [load]);

  // The live half. The event carries nothing: it says "look again", and the GET
  // says what is true. This is what puts the login link on screen without a
  // reload, and what clears it the moment the operator approves the machine.
  useEffect(() => {
    return appEvents.subscribe((type) => {
      if (type !== "tailscaleState") return;
      void load();
    });
  }, [load]);

  async function runVerb(
    which: "connect" | "disconnect" | "forget",
    call: () => Promise<TailnetSettingsView>,
  ) {
    setBusy(which);
    setActionError(null);
    try {
      adopt(await call());
      setConfirmForget(false);
    } catch (err) {
      setActionError(errorMessage(err));
    } finally {
      setBusy(null);
    }
  }

  function buildPayload(v: TailnetSettingsView, d: SettingsDraft): UpdateTailnetInput {
    const payload: UpdateTailnetInput = {};
    if (d.hostname.trim() !== v.hostname) payload.hostname = d.hostname.trim();
    if (d.controlURL.trim() !== v.controlURL) payload.controlURL = d.controlURL.trim();
    if (d.httpsEnabled !== v.httpsEnabled) payload.httpsEnabled = d.httpsEnabled;
    return payload;
  }

  async function onSave() {
    if (!view || !draft || saving) return;
    setSaving(true);
    setSaveError(null);
    setSaved(false);
    try {
      const next = await apiClient.updateTailnet(buildPayload(view, draft));
      setView(next);
      setDraft(draftFrom(next));
      setSaved(true);
    } catch (err) {
      setSaveError(errorMessage(err));
    } finally {
      setSaving(false);
    }
  }

  async function onCopy(address: string) {
    try {
      await navigator.clipboard?.writeText(address);
      setCopied(true);
    } catch {
      // A browser that refuses clipboard access is not an error worth a panel:
      // the address is on screen in full and can be selected by hand.
      setCopied(false);
    }
  }

  if (loadError && !view) {
    return (
      <section className="admin-remote-access" data-testid="admin-remote-access">
        <h2 className="section-title">Remote access</h2>
        <p
          className="status status-error"
          data-testid="remote-access-load-error"
          role="alert"
        >
          <span className="dot dot-error" aria-hidden="true" />
          {loadError}{" "}
          <button
            className="nav-link"
            type="button"
            data-testid="remote-access-retry"
            onClick={() => void load()}
          >
            Retry
          </button>
        </p>
      </section>
    );
  }

  if (!view || !draft) {
    return (
      <section className="admin-remote-access" data-testid="admin-remote-access">
        <h2 className="section-title">Remote access</h2>
        <p className="status status-loading" data-testid="remote-access-loading">
          Loading remote access&hellip;
        </p>
      </section>
    );
  }

  const phase = tailnetPhase(view);
  const hostnameChanged = draft.hostname.trim() !== view.hostname;

  return (
    <section className="admin-remote-access" data-testid="admin-remote-access">
      <h2 className="section-title">Remote access</h2>

      <div
        className="tailnet-panel card"
        data-testid="remote-access-state"
        data-state={phase.kind}
      >
        {phase.kind === "off" && <OffState />}
        {phase.kind === "connecting" && <ConnectingState />}
        {phase.kind === "waitingForLogin" && (
          <WaitingForLoginState loginURL={phase.loginURL} />
        )}
        {phase.kind === "expired" && (
          <ExpiredState loginURL={phase.loginURL} expiredAt={phase.expiredAt} />
        )}
        {phase.kind === "connected" && (
          <ConnectedState
            view={view}
            expiresInDays={phase.expiresInDays}
            copied={copied}
            onCopy={(addr) => void onCopy(addr)}
          />
        )}
        {phase.kind === "failed" && <FailedState message={phase.message} />}

        <div className="tailnet-actions" data-testid="remote-access-actions">
          {(!view.enabled || phase.kind === "failed") && (
            <button
              className="auth-submit"
              type="button"
              data-testid="tailnet-connect"
              onClick={() => void runVerb("connect", () => apiClient.connectTailnet())}
              disabled={busy !== null}
            >
              {busy === "connect"
                ? "Connecting…"
                : phase.kind === "failed"
                  ? "Try again"
                  : "Connect"}
            </button>
          )}
          {view.enabled && (
            // No confirmation, deliberately: disconnecting is reversible and
            // instant to undo (the node keeps its identity), and a dialog on a
            // reversible action teaches people to dismiss dialogs.
            <button
              className="nav-link"
              type="button"
              data-testid="tailnet-disconnect"
              onClick={() =>
                void runVerb("disconnect", () => apiClient.disconnectTailnet())
              }
              disabled={busy !== null}
            >
              {busy === "disconnect" ? "Disconnecting…" : "Disconnect"}
            </button>
          )}
          <button
            className="nav-link nav-link-danger"
            type="button"
            data-testid="tailnet-forget"
            onClick={() => setConfirmForget(true)}
            disabled={busy !== null}
          >
            Forget this Tailnet
          </button>
        </div>

        {actionError && !confirmForget && (
          <p
            className="status status-error"
            data-testid="remote-access-action-error"
            role="alert"
          >
            <span className="dot dot-error" aria-hidden="true" />
            {actionError}
          </p>
        )}
      </div>

      <div className="tailnet-settings card" data-testid="remote-access-settings">
        <h3 className="card-title">Settings</h3>

        <div className="field">
          <label className="field-label" htmlFor="tailnet-hostname-input">
            Hostname
          </label>
          <input
            id="tailnet-hostname-input"
            className="field-input"
            data-testid="tailnet-hostname-input"
            type="text"
            value={draft.hostname}
            placeholder="obelo"
            onChange={(e) => {
              const v = e.target.value;
              setDraft((d) => (d ? { ...d, hostname: v } : d));
              setSaved(false);
            }}
            disabled={saving}
          />
          <span className="field-hint">
            The first part of this server&rsquo;s MagicDNS name — letters, digits and
            hyphens only.
          </span>
          {/* The warning is the point of this field, so it fires at the moment of
              editing rather than after the save that already broke things. */}
          {hostnameChanged && (
            <p
              className="tailnet-warning"
              data-testid="tailnet-hostname-warning"
              role="alert"
            >
              Changing the hostname changes this server&rsquo;s address. Every client
              that stored the old one will stop reaching it until you update it by
              hand, and the old Tailnet node stays listed in your Tailscale console
              for you to delete.
            </p>
          )}
        </div>

        <div className="field">
          <label className="field-label" htmlFor="tailnet-controlurl-input">
            Control URL
          </label>
          <input
            id="tailnet-controlurl-input"
            className="field-input"
            data-testid="tailnet-controlurl-input"
            type="text"
            value={draft.controlURL}
            placeholder="https://headscale.example.net"
            onChange={(e) => {
              const v = e.target.value;
              setDraft((d) => (d ? { ...d, controlURL: v } : d));
              setSaved(false);
            }}
            disabled={saving}
          />
          <span className="field-hint" data-testid="tailnet-controlurl-hint">
            Leave this empty to use Tailscale&rsquo;s own coordination server. If you
            would rather not depend on a vendor for it, point this at your own
            Headscale and everything here works the same way.
          </span>
        </div>

        <div className="field">
          <label className="tailnet-toggle">
            <input
              type="checkbox"
              data-testid="tailnet-https-input"
              checked={draft.httpsEnabled}
              onChange={(e) => {
                const v = e.target.checked;
                setDraft((d) => (d ? { ...d, httpsEnabled: v } : d));
                setSaved(false);
              }}
              disabled={saving}
            />{" "}
            Serve HTTPS on the Tailnet
          </label>
          <span className="field-hint">
            Needs MagicDNS and HTTPS Certificates enabled in the Tailscale console —
            neither can be turned on from here. If a certificate cannot be issued,
            plain HTTP on the Tailnet keeps working and the server log says why.
          </span>
          {/* What actually happened, as opposed to what was asked for. Keyed on
              the saved view, so a half-edited draft cannot make it claim anything. */}
          <HTTPSStatus view={view} />
        </div>

        <div className="tailnet-save-bar">
          <button
            className="auth-submit"
            type="button"
            data-testid="tailnet-save"
            onClick={() => void onSave()}
            disabled={saving}
          >
            {saving ? "Saving…" : "Save settings"}
          </button>
          {saved && (
            <span className="status status-ok" data-testid="tailnet-save-success" role="status">
              Settings saved.
            </span>
          )}
          {saveError && (
            <p className="status status-error" data-testid="tailnet-save-error" role="alert">
              <span className="dot dot-error" aria-hidden="true" />
              {saveError}
            </p>
          )}
        </div>
      </div>

      {confirmForget && (
        <ConfirmDialog
          title="Forget this Tailnet?"
          message={
            "This wipes the server's Tailnet identity, so the next connect needs a fresh login. " +
            "The now-dead node stays listed in your Tailscale console and only you can delete it — " +
            "nothing Obelo does can reach it. To stop remote access for now without any of that, use Disconnect."
          }
          confirmLabel="Forget"
          busyLabel="Forgetting…"
          busy={busy === "forget"}
          error={actionError}
          onConfirm={() => void runVerb("forget", () => apiClient.forgetTailnet())}
          onCancel={() => {
            if (busy === null) setConfirmForget(false);
          }}
        />
      )}
    </section>
  );
}

// --- The six states ---------------------------------------------------------

function OffState() {
  return (
    <>
      <h3 className="tailnet-headline">Remote access is off</h3>
      <p className="tailnet-body">
        Join this server to your Tailnet and it becomes reachable from anywhere at
        one stable name — no port to forward, no domain to buy, no dynamic DNS, and
        nothing exposed to the internet.
      </p>
      <p className="tailnet-body tailnet-cost" data-testid="tailnet-cost">
        <strong>Every device you watch on must join the Tailnet too.</strong>{" "}
        Tailscale has to be installed and signed in on each phone, tablet, Apple TV
        and laptop that will reach this server. That is the whole cost of this path,
        and it is worth deciding on now rather than after you have set it up.
      </p>
    </>
  );
}

function ConnectingState() {
  return (
    <>
      <h3 className="tailnet-headline">Connecting&hellip;</h3>
      <p className="tailnet-body">
        Contacting the coordination server. A login link will appear here within a
        few seconds — leave this page open, it updates itself.
      </p>
    </>
  );
}

function WaitingForLoginState({ loginURL }: { loginURL: string }) {
  return (
    <>
      <h3 className="tailnet-headline">Waiting for you to log in</h3>
      <p className="tailnet-body">
        Open this link and approve this server in your Tailscale account. Nothing
        happens until you do.
      </p>
      {loginURL ? (
        <a
          className="tailnet-login-link"
          data-testid="tailnet-login-link"
          href={loginURL}
          target="_blank"
          rel="noreferrer"
        >
          {loginURL}
        </a>
      ) : (
        <p className="tailnet-body" data-testid="tailnet-login-pending">
          The link is on its way and will appear here on its own — no need to reload.
        </p>
      )}
    </>
  );
}

function ExpiredState({
  loginURL,
  expiredAt,
}: {
  loginURL: string;
  expiredAt: string;
}) {
  return (
    <>
      <h3 className="tailnet-headline">Re-authorize this server</h3>
      <p className="tailnet-body" data-testid="tailnet-expired-note">
        This Tailnet node&rsquo;s membership lapsed on {formatDate(expiredAt)}, so it
        is no longer reachable from away. Nothing is broken and nothing on your LAN
        changed — Tailscale simply expires a machine&rsquo;s membership after a while.
        Open the link to sign it back in.
      </p>
      {loginURL ? (
        <a
          className="tailnet-login-link"
          data-testid="tailnet-login-link"
          href={loginURL}
          target="_blank"
          rel="noreferrer"
        >
          {loginURL}
        </a>
      ) : (
        <p className="tailnet-body" data-testid="tailnet-login-pending">
          A fresh link is on its way and will appear here on its own.
        </p>
      )}
      <p className="tailnet-body tailnet-hint">
        To stop this happening again, turn key expiry off for this node in the
        Tailscale console: Machines &rarr; this server &rarr; Disable key expiry.
        That switch lives there, not here.
      </p>
    </>
  );
}

function ConnectedState({
  view,
  expiresInDays,
  copied,
  onCopy,
}: {
  view: TailnetSettingsView;
  expiresInDays: number | null;
  copied: boolean;
  onCopy: (address: string) => void;
}) {
  const address = tailnetAddress(view);
  const addresses = view.status.addresses ?? [];
  const warn = expiresInDays !== null && expiresInDays <= EXPIRY_WARNING_DAYS;
  return (
    <>
      <span className="tailnet-label">Reachable at</span>
      {/* The single thing the operator came here for, so it is the largest thing
          on the screen and it can be copied without selecting it by hand. */}
      <div className="tailnet-address-row">
        <span className="tailnet-address" data-testid="tailnet-address">
          {address}
        </span>
        <button
          className="nav-link"
          type="button"
          data-testid="tailnet-copy"
          aria-label={`Copy ${address}`}
          onClick={() => onCopy(address)}
        >
          {copied ? "Copied" : "Copy"}
        </button>
      </div>
      <p className="tailnet-body">
        Use this address in any Obelo client on a device that has joined the Tailnet.
        It is the same address at home and away.
      </p>
      {addresses.length > 0 && (
        <p className="tailnet-body tailnet-hint" data-testid="tailnet-addresses">
          If MagicDNS is off in your Tailscale console, these Tailnet addresses reach
          it too: {addresses.join(", ")}
        </p>
      )}

      {warn ? (
        <p className="tailnet-warning" data-testid="tailnet-expiry-warning" role="alert">
          This node&rsquo;s membership expires on {formatDate(view.status.keyExpiry)}
          {expiresInDays >= 0
            ? ` (${expiresInDays} ${expiresInDays === 1 ? "day" : "days"} from now)`
            : ""}
          . When it lapses, remote access stops and nothing local changes. The fix
          lives in the Tailscale console: Machines &rarr; this server &rarr; Disable
          key expiry.
        </p>
      ) : (
        <p className="tailnet-body tailnet-hint" data-testid="tailnet-expiry">
          {view.status.keyExpiry
            ? `Membership expires ${formatDate(view.status.keyExpiry)}. Turning key expiry off for this node in the Tailscale console removes the date entirely.`
            : "Membership does not expire — key expiry is off for this node."}
        </p>
      )}
    </>
  );
}

/** The HTTPS opt-in's OUTCOME, beside the toggle that asked for it.
 *
 * Three cases, rendered distinctly, because collapsing them is the bug this
 * exists to fix:
 *
 *   off       nothing beyond the toggle itself — there is nothing to report
 *   bound     asked for, and serving: the address above is the https:// one
 *   unbound   asked for and NOT serving, which is the case the operator has to
 *             be told about and could not previously see anywhere
 *
 * The unbound case is amber rather than red on purpose. Nothing has failed for
 * the household: the node is up, the Tailnet address works over http, and one
 * optional extra has not come up — which is a caution the operator has time to
 * act on, not an outage. It says so in the same breath as the failure, because an
 * operator who reads "no HTTPS" and stops reading will go looking for a problem
 * that is not there. */
function HTTPSStatus({ view }: { view: TailnetSettingsView }) {
  const state = tailnetHTTPSState(view);
  if (state.kind === "off") return null;

  const address = tailnetAddress(view);
  if (state.kind === "bound") {
    return (
      <p className="status status-ok" data-testid="tailnet-https-bound" role="status">
        <span className="dot dot-ok" aria-hidden="true" />
        HTTPS is serving on the Tailnet{address ? ` at ${address}` : ""}.
      </p>
    );
  }
  return (
    <div
      className="tailnet-warning tailnet-https-panel"
      data-testid="tailnet-https-unbound"
      role="alert"
    >
      <p>
        <strong>HTTPS on the Tailnet is not serving.</strong>{" "}
        {address
          ? `The address above, ${address}, is unaffected and is still the one to use.`
          : "Plain HTTP on the Tailnet is unaffected and is still the address to use."}{" "}
        Your LAN was never involved.
      </p>
      {/* VERBATIM. The server's paragraph names both Tailscale console settings,
          which is the entire fix and lives somewhere this UI cannot reach; a
          friendlier rewrite would drop exactly that. Same rule as lastError. */}
      {state.message ? (
        <p className="tailnet-https-detail" data-testid="tailnet-https-error">
          {state.message}
        </p>
      ) : (
        <p className="tailnet-https-detail" data-testid="tailnet-https-pending">
          The server has not reported a reason yet. It keeps trying on its own, and
          this updates itself — no reload and no restart.
        </p>
      )}
    </div>
  );
}

function FailedState({ message }: { message: string }) {
  return (
    <>
      <h3 className="tailnet-headline">The Tailnet node could not start</h3>
      {/* VERBATIM. The server's message names console settings, control URLs and
          builds; a friendlier paraphrase would lose the only actionable part. */}
      <p className="tailnet-error" data-testid="tailnet-error" role="alert">
        {message}
      </p>
      <p className="tailnet-body tailnet-hint">
        Your LAN is unaffected — the server is serving it normally, and only remote
        access over the Tailnet is off.
      </p>
    </>
  );
}
