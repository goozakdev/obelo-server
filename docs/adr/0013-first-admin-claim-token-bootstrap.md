# First Admin bootstrap via claim token in logs

On first start with an empty database (zero users), the server generates a one-time claim token and prints it to stdout / container logs. The setup wizard that creates the first Admin requires this token. Once the first Admin exists, setup permanently closes — the zero-users state cannot be re-entered without wiping the data directory.

## Why
Everything is authenticated-only, so the first Admin must be bootstrapped somehow. An open setup page would let anyone who reaches an internet-exposed fresh server hijack it. Requiring a token only readable from the host logs is secure-by-default even if the port is exposed immediately.

## Considered and rejected
- **Open setup wizard** — vulnerable to first-hit hijack on an exposed server.
- **Env-var admin credentials** — plaintext in compose config, awkward rotation.
