# Runbook: share libraries between two Obelo servers

**When:** you and someone you know each run an Obelo, and one of you wants to let the other browse
and play a few libraries — without handing out a login on your server, without your watch state
becoming theirs, and without either household learning a second address. Governing decisions:
[ADR-0054](../adr/0054-a-linked-server-is-a-user-with-the-remote-role.md) (the `remote` role and the
Playback ceiling), [ADR-0055](../adr/0055-linking-is-a-one-time-invite-redeemed-server-to-server.md)
(the invite and the two dialers),
[ADR-0056](../adr/0056-a-linked-library-is-a-read-only-mirror-played-through-a-one-hop-relay.md)
(the mirror and the relay).

**Precondition:** two Obelo servers, an Admin account on each, and **one of them reachable from the
other**. That last one is the only hard part, and §4 is the whole of it. There is no directory, no
account service and no relay to make this discoverable, and there never will be
([ADR-0001](../adr/0001-fully-self-hosted-no-vendor-dependency.md)) — linking is a deliberately
manual act between two people who already know each other. The apps need no update: linked libraries
arrive as libraries.

**Two sides, and they are not symmetric.** The **sharer** creates a user for the other machine and
sends one string. The **receiver** pastes it, and from then on their server holds the credential,
mirrors the catalog and carries the bytes. Sharing does not travel: a library you received from a
friend can never be shared onward to a third household. If you want to share both ways, do this
runbook twice, once in each direction.

**What this does not change:** on the sharing side, nothing about your own household — your users,
your watch state, your libraries and your transcode budget are all exactly as they were, and the
new user can only ever read and play what you grant it. On the receiving side, the Scanner, the
enrichment pass and every editor skip a linked library entirely; nothing about your own libraries
moves. Neither server learns anything about the other's people.

**Check both builds first.** On each server:

```sh
curl -s http://192.168.1.50:8080/api/v1/server | grep -o '"serverLinking":[a-z]*\|"linkedLibraries":[a-z]*'
```

Both flags on both machines. If either is missing, that server predates linking and needs an
upgrade — which is also what it will be told at paste time, naming which side.

---

## The sharing side

### 1. Create a user for their server

**Settings → Users → Add user.** Set the role to **Linked server** and give it a name you will
recognise in your own session list — "Kate's Obelo", not "guest". There is no password field, and
that is not an omission: this role has no password at all, cannot be given one, cannot log in with
one, and cannot ever be turned into a person's account.

It is created with **no libraries granted**. That is deliberate — "all libraries by default" is a
friendly default inside a household and a disclosure of your whole collection to a machine in
someone else's.

### 2. Grant the libraries you mean

**Settings → Users → (the new row) → Edit → Libraries.** Tick exactly the libraries you are
sharing. This is the same replace-set the rest of your household uses, and you can change it
whenever you like — a library you revoke disappears from their server at the next sync, and a
library you add appears the same way.

**Libraries you received from someone else are not listed here**, and the dialog says why. The
owner of those files decided who sees them; a hop later that decision would be made by somebody
they never met, and paid for in transcode CPU by them.

### 3. Set the ceilings

Two independent caps, in the same dialog:

- **Rating ceiling** — the same one you use for a child's account. Titles above it are hidden from
  that server entirely.
- **Playback ceiling** — new, and this is the one that matters here. Three fields:
  - **Max resolution**: `720p`, `1080p` or `2160p` (blank = uncapped). A 4K-only file under a
    1080p cap **transcodes down on your machine**, under your own governance. It is not hidden —
    a quality cap changes how something plays, never whether it exists.
  - **Max bitrate**: entered in Mbps, sent in bits/sec. Blank or zero = uncapped.
  - **Max streams**: how many of their household may be watching at once. This is your one lever
    over *load* from a household you cannot see into, and it exists precisely so you do not need a
    second one: you will never see who over there is watching, only that your server is serving
    N streams to "Kate's Obelo".

Everything transcoding costs is yours to pay, so set max streams before you set anything else.

### 4. Decide how their server will reach yours

**This is the whole of the difficulty.** One machine — theirs — has to be able to open a connection
to yours. Their phones and televisions do not; they only ever talk to their own server, which is
what makes this possible at all.

Three ways, and you may list more than one in the invite. They are tried in the order you type them
and the one that answers is remembered.

**(a) A public HTTPS address, with a port-forward.** If you already reach your own server from
outside the house, you already have this — use the same origin. If you do not, set it up first:
[Let's Encrypt and a forwarded port](./https-with-lets-encrypt.md), or
[your own certificate](./https-with-your-own-certificate.md) if you have one. The origin goes in the
invite as `https://media.example.org`, scheme and host, no path and no trailing slash.

**(b) A reverse proxy.** If nginx, Caddy or a container ingress already terminates TLS in front of
Obelo, its public origin is the address — nothing special is needed, and Obelo cannot tell this
apart from (a). Make sure `OBELO_TRUSTED_PROXIES` names the proxy (see the README) so rate limits
stay per-client.

**(c) A Tailscale machine-share — no port-forward on either side.** This is the one most households
should use if both operators are willing to make a Tailscale account. You share **your Obelo
machine** into their tailnet from the Tailscale console; their server then dials it directly over
WireGuard through its own tailnet node. Nothing is exposed to the internet, neither of you opens a
router page, and there is no bandwidth cap.

Both servers need the tailnet feature connected first — follow
[remote access with Tailscale](./remote-access-with-tailscale.md) on each, at least as far as its
step 3. Then, in the **Tailscale admin console** on *your* account:

1. **Machines → your Obelo server → ⋯ → Share…**
2. Enter the other operator's Tailscale account email and send the invite link.
3. They accept it in their own console. Your machine now appears in their machine list as a shared
   node and resolves on their tailnet as `obelo.<your-tailnet>.ts.net` — it keeps *your* tailnet in
   its name, which is exactly how their server knows to dial it through the tailnet rather than the
   open internet.
4. **Machines → your Obelo server → ⋯ → Disable key expiry.** Do not skip this. See below.
5. Copy that `obelo.<your-tailnet>.ts.net` name; the origin you will type in step 5 is
   `https://obelo.<your-tailnet>.ts.net` (or `http://…` if you have not enabled tailnet HTTPS —
   Obelo's own Remote access panel shows you which is true).

A shared machine is **quarantined by Tailscale**: it can be reached by them and cannot initiate
anything back at their network. There is no API for machine sharing; it is a console action, which
is why this is a runbook step and not a button in Obelo.

> **Node key expiry is the one scheduled failure on this path.** Tailscale expires a machine's
> membership after 180 days by default. When yours lapses, your server silently stops being
> reachable over the tailnet — and on **their** side it shows up only as **unreachable**, with no
> hint that a date arrived on a machine in your house. Nothing local breaks and nothing logs an
> error over there. Step 4 above removes it permanently; Obelo will also warn you in its own
> settings panel and log as the date approaches.

### 5. Generate the invite and send it

**Settings → Users → (the linked-server row) → Edit → Link.**

Type the addresses from step 4, one per row, **in the order you want them tried** — "Add another
address" for a second. If your tailnet node is connected, the MagicDNS row is pre-filled for you;
the free-text row is for the public origin. Obelo cannot know its own public address and never
guesses one, which is why you are typing this.

Press **Generate invite.** You get one string:

```
obelo-link:eyJ2IjoxLCJpZCI6IjkwZjBhYTYyLTQ3NjktNDc4OC1iNjNhLWQ5ZTNjYjRhZDUxYyIsIm5hbWUiOi…
```

and a QR of the same string. Send either one — iMessage, Signal, email, a photo of the screen.
Three things to know:

- **It is single-use and lasts 24 hours.** Anyone holding it can link **once**, within a day, and
  the code is dead the moment it is spent. That is why it is safe in a chat in a way a password
  never is.
- **Generating another one replaces it.** If you press the button twice, the first string stops
  working — so do not generate a fresh one while your friend is halfway through pasting the old.
- **The string is not a link to click.** There is no page to open; the action it triggers belongs
  on *their* server, which the string cannot name. It gets pasted into Obelo, or scanned by the
  Obelo app.

Nothing has been shared yet. The grants and the ceilings above are what will apply the moment they
redeem it.

---

## The receiving side

### 6. Paste the string

**Settings → Linked servers → Link a server.** Paste the whole `obelo-link:` string into the box
and submit. (In the iPhone or iPad app, once that ships, **Settings → Link a server** opens the
camera and scans the QR instead — you must be signed in as an Admin of *your own* server, and the
phone stores nothing from the code; it is a courier.)

Within a few seconds the panel names the sharing server and lists the libraries that arrived. The
whole thing happens in that one request: your server reads the addresses, tries them in order,
checks that the other machine speaks linking and speaks the same version of it, spends the code for
a credential, creates a library per granted library, and pulls the whole catalog once.

If it refuses, the sentence names the next move:

| What it says | What to do |
| --- | --- |
| That is not a valid invite | You pasted something else, or the string got mangled in transit. Ask for it again. |
| This invite has expired | More than 24 hours old. Ask them to generate a fresh one. |
| The other server would not accept this invite | The code was already spent (or replaced by a newer one). Ask for a fresh one. |
| Their server needs an upgrade / yours does | The two servers speak different link protocol versions. The sentence says which side. |
| Could not reach that server at any of these addresses | None of the addresses answered. The addresses are listed; go back to §4 with them. |

### 7. Grant the new libraries to your household

A linked library is a library. **Settings → Users → (a person) → Edit → Libraries** now lists it
alongside your own, and it obeys that person's rating ceiling exactly as your own libraries do. The
two households' ceilings stack — the stricter side wins — because each server enforces its own and
neither knows about the other. This is what lets you say "the kids get his cartoons library and not
the other one" with a dialog you already know.

From there the mirrored titles are ordinary titles: they appear in Home rows, in search, in
Collections and Playlists, and they carry **your household's** watch state. Nothing about what you
watch crosses back.

### 8. Read the state

Each row on **Settings → Linked servers** carries one of three states, the address currently in use
(with the alternatives beside it), and when it last synced.

| State | Means | What you do |
| --- | --- | --- |
| **connected** | The last call to them succeeded. | Nothing. |
| **unreachable** | Their machine is not answering — it is off, rebooting, or its address moved. **Nothing is deleted.** The libraries stay, badged unavailable and greyed; their titles stay in Continue Watching; a play says the sharing server could not be reached. Your server keeps retrying on a backoff. | Wait, or press **Sync now**. If it persists, ask them: on the tailnet path, a lapsed node key looks exactly like this. |
| **revoked** | They deleted the linked-server user, or its device. Your credential is dead and **no amount of retrying will fix it** — so nothing retries. The libraries stay. | Ask for a fresh invite and press **Re-key**. |

### 9. The three row actions

- **Sync now** — one immediate sweep: reconcile which libraries you are granted, then pull each of
  them forward. It is synchronous and reports what happened, because you pressed it after plugging
  something back in. It is also the *only* thing that dials a revoked link — the automatic loop
  never does, but a human asking is not a retry.
- **Re-key** — paste a fresh invite from **the same server** into the same box. The link, its
  libraries, their local ids and your household's watch state are all kept; only the credential and
  the addresses are replaced. An invite from a *different* server is refused rather than silently
  repointed. This is the fix for **revoked**, and also the fix for a friend whose address changed.
- **Unlink** — the only destructive action, and the only thing that ever deletes what came over a
  link. It removes: the link, **every library it provided, every mirrored movie/show/episode/
  artist/album/track behind them, and your household's watch state for all of it.** It also tells
  the other server, so your entry disappears from their users page instead of lingering as a ghost
  with a last-seen — but it succeeds either way, because a friend's server being switched off must
  not be able to keep you linked to them. The confirmation names the libraries; read it.

### 10. Staying fresh

After the first pull, the mirror refreshes two ways: immediately, when the sharing server says one
of your libraries changed (your server holds an event subscription to theirs), and on a timer as
the fallback.

```
OBELO_LINK_SYNC_INTERVAL=1h     # the default
OBELO_LINK_SYNC_INTERVAL=0      # background refresh off entirely
```

`0` turns off the timer, the retry backoff **and** the subscription — one knob for one question,
"does this server keep its friends' libraries fresh on its own". Sync now, and the pull that
follows a link or a re-key, still work. Every link still starts *unreachable* at boot until
something succeeds, because that is a statement about this boot and not about the schedule.

---

## Two things not to do

**Do not try to re-share a library you received.** It is refused at three surfaces — the grant, the
export, and the user dialog, which does not list it — and this is not a limitation to work around.
The person whose disk those files are on decided who sees them, and they would be the one paying
the transcode for a household they have never heard of.

**Do not put a friend's address in `OBELO_TRUSTED_PROXIES`.** It is not a proxy. See the README's
reverse-proxy section for what that setting actually does and why widening it is expensive.

## Troubleshooting

**Everything paste-time worked, but no libraries arrived.** They granted the linked-server user
nothing. Step 2, on their side.

**A film plays but stutters, and the LAN is fine.** If you are on the tailnet path, your two
machines may have failed to connect directly and fallen back to a DERP relay, which is throttled and
shared. Tailscale's console shows the connection type per machine. See the tailnet runbook's
troubleshooting; nothing in Obelo can see or report this.

**"Stream limit reached" on a title from their library.** Their server refused, not yours: the
linked-server user is at its **max streams** cap (§3). Someone else in your household is already
watching something of theirs. The refusal passes through verbatim, counts and all.

**Something of theirs plays at 1080p when the file is 4K.** Working as intended — that is their
Playback ceiling, applied on their machine, transcoding down under their governance. Your server
never re-encodes anything of theirs; it only carries the bytes.

**They renamed a film and it did not change here.** It will at the next sync. If you are impatient,
**Sync now**. If it still does not, the link is not `connected` — read the state.

**It worked for months and then went unreachable.** If you linked over a tailnet machine-share, this
is almost certainly the sharer's node key expiring after 180 days (§4). It cannot be seen from this
side; ask them to check their Tailscale console and disable key expiry on the shared machine.
