# Runbook: serve HTTPS from a certificate you made yourself

**When:** you want TLS without a public CA — a development machine, or a
household that wants the LAN encrypted and cannot get a certificate for
`192.168.1.50` or a `.local` name (no CA will issue one). Governing decision:
[ADR-0041](../adr/0041-native-tls-optional-alongside-plain-http.md),
`OBELO_TLS_MODE=files`.

**Precondition:** `openssl`, and a fixed address or name for the machine running
Obelo. Nothing external — no domain, no port-forward, no internet.

**What this does not change:** the plain-HTTP listener on `OBELO_LISTEN_ADDR`
keeps serving and mDNS discovery goes on working. Both listeners serve the same
API and a session created on one works on the other.

**Read this first, because it is the whole reason this runbook exists.** A
certificate that is otherwise perfect will be refused by every Apple client if it
lacks **`extendedKeyUsage=serverAuth`**. Apple has required that extension on TLS
server certificates since iOS 13, and the refusal names neither the extension nor
the fix — it comes back as *"certificate is not permitted for this usage"*, which
reads like a hostname or expiry problem and is neither. The first hand-rolled
certificate in this project omitted it and cost a debugging session. Step 1 puts
it in.

If you want a certificate from a real CA instead, that is
[the Let's Encrypt runbook](./https-with-lets-encrypt.md) and it is less work
than this one.

## 1. Write the certificate config

Extensions cannot be passed as flags to `openssl req`, so they go in a file. Put
this next to where the certificate will live:

```ini
[req]
distinguished_name = dn
x509_extensions    = v3
prompt             = no

[dn]
CN = obelo.lan

[v3]
basicConstraints = critical,CA:TRUE
keyUsage         = critical,digitalSignature,keyEncipherment,keyCertSign
extendedKeyUsage = serverAuth
subjectAltName   = DNS:obelo.lan,DNS:localhost,IP:192.168.1.50,IP:127.0.0.1
```

Two lines need your values:

- **`CN`** — a name for the machine. It is cosmetic; modern clients read only the
  SAN below.
- **`subjectAltName`** — **every** name and address a client might reach this
  server by. A name that is not in this list is refused even after the
  certificate is trusted, because the name check is separate from the trust
  check. Include the LAN IP, any `.local` or LAN DNS name, and `127.0.0.1` if you
  ever test on the host. Adding one later means reissuing.

`CA:TRUE` is deliberate: it lets this one certificate act as its own trust
anchor, so a client that has trusted it keeps working when you reissue the leaf
under it. A `CA:FALSE` leaf works equally well today and has to be re-trusted on
every device at every renewal.

## 2. Generate the pair

```sh
openssl req -x509 -newkey rsa:2048 -nodes \
  -keyout obelo-key.pem -out obelo-cert.pem \
  -days 397 -config obelo-cert.cnf
```

**397 days, not longer.** Apple rejects a server certificate whose validity
exceeds 398 days outright, regardless of trust. A 10-year certificate is refused
on every Apple device in the house and the message will not tell you why.

Keep the `.cnf`. Reissuing without it means reconstructing the extensions from
memory, which is how the `serverAuth` line gets lost.

## 3. Lock down the key

```sh
chmod 600 obelo-key.pem
```

Obelo warns at boot if the key is group- or world-readable
(`warnIfKeyIsExposed`, `cmd/obelo/tls.go`). It is a warning rather than a refusal
because a wrong mode is not a reason to take somebody's media server down — but
anyone who can read that file can impersonate this server to every client that
trusts it.

## 4. Point the server at it

```
OBELO_TLS_MODE=files
OBELO_TLS_CERT=/absolute/path/to/obelo-cert.pem
OBELO_TLS_KEY=/absolute/path/to/obelo-key.pem
```

`OBELO_TLS_LISTEN_ADDR` defaults to `:8443`. Absolute paths: the server resolves
them against its working directory, which is rarely where you generated them.

Restart, and expect the server to start. **In `files` mode a bad certificate
stops the boot**, with an error naming the path — deliberately the opposite of
`acme` mode, which boots anyway. A wrong path is a typo you can fix in ten
seconds, and starting regardless would leave you on plain HTTP believing you had
TLS. The pair is loaded at startup (`tls.LoadX509KeyPair`, `internal/config`),
so a key that belongs to a different certificate is caught then rather than on
the first handshake.

## Verifying it took

Check the chain the server actually serves, and confirm the extension that
matters is present:

```sh
openssl s_client -connect 192.168.1.50:8443 </dev/null 2>/dev/null \
  | openssl x509 -noout -subject -dates -ext extendedKeyUsage,subjectAltName
```

You want `TLS Web Server Authentication` in the EKU and your address in the SAN.
A self-signed certificate makes `openssl` report `verify error: self-signed
certificate` — that is expected here and is not the failure you are looking for.

## 5. Make clients trust it

Nothing trusts this certificate yet. **The route differs by client, and the
differences are real rather than cosmetic.**

**The Obelo app on iPhone, iPad, or Apple TV** asks you directly. Connect to the
server, the app refuses the certificate, and it shows you the certificate's
SHA-256 fingerprint with a button to accept it. Compare that fingerprint against
the server's before pressing it — that comparison is the entire security of the
arrangement:

```sh
openssl x509 -in obelo-cert.pem -noout -fingerprint -sha256
```

Accepting stores it for that host and covers **both** the API and media playback.
This is the Apple client's ADR-0019.

**A browser, or anything else on the machine**, needs the certificate installed
in the OS or browser trust store. On Apple platforms that means installing a
profile *and* separately enabling it under Settings → General → About →
Certificate Trust Settings; the second step is easy to miss and nothing works
until it is done.

**Installing the OS profile does not substitute for the app's own prompt.** The
Obelo app's media playback verifies against a certificate list it ships itself
and never consults the system trust store, so a device with the profile installed
will sign in, browse, and load artwork perfectly while every film fails to start.
That was measured on an iPhone against this server on 2026-08-11. If you want
both, do both — they answer different questions.

## Afterwards

- **Renewal is manual, and it is on a clock.** 397 days from generation, repeat
  steps 2–4. `files` mode re-reads the files while running (re-`stat`ed on the
  handshake path, rate-limited to every few seconds), so a reissue takes effect
  without a restart, and a half-written file leaves the previous certificate
  serving rather than taking the listener down.
- **Reissuing under the same `CA:TRUE` certificate does not re-prompt** clients
  that already accepted it. Replacing the anchor itself does, with a new
  fingerprint to compare — which is the warning working, not a bug.
- **AirPlay video will not work against this server**, from any client, ever. An
  AirPlay receiver fetches the stream itself and validates against its own trust
  store, which no phone can add a certificate to. The Apple client hides the
  affordance rather than offering one that fails (its ADR-0023). Picture-in-
  Picture is hidden for the same reason. Direct playback is unaffected. If you
  need AirPlay, you need a publicly-trusted certificate — the Let's Encrypt
  runbook, plus split-horizon DNS so the public name resolves to the LAN address.
- **The plain-HTTP listener is still there** and still serves everything. Nothing
  in this runbook closes it, and mDNS still advertises it — a client that
  discovers this server on the LAN gets plain HTTP and never sees the
  certificate at all.
- **Adding a name or address later** means editing `subjectAltName`, reissuing,
  and re-trusting on every device if you replaced the anchor. Listing every name
  you might plausibly use in step 1 is cheaper than doing this twice.
