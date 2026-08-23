// Command keytool is the OFFLINE maintainer half of the metadata key-rotation
// channel (ADR-0032): it takes a fresh set of default provider keys and the
// base64 AES-256-GCM app encryption key (kAppEncKey) and emits the exact
// versioned envelope the running server fetches and decrypts —
//
//	{ "v": 1, "minAppVersion": "0.x",
//	  "payload": "<base64( nonce ‖ AES-256-GCM(kAppEncKey, {"tmdb","fanart"}) )>" }
//
// It is the "encrypt" step of the revoke-and-replace runbook: revoke the leaked
// key on the provider dashboard → `keytool` to seal the replacement → publish the
// envelope to the Cloudflare Worker's KV store (`wrangler kv key put`) → installs
// pick it up on their next poll, with no release. See
// docs/runbooks/metadata-key-rotation.md.
//
// The encryption itself is rotation.Encrypt — the SAME implementation the client's
// Decrypt inverts — so the two ends of the envelope can never drift out of sync,
// and every run uses a fresh random nonce (Encrypt's guarantee). This tool never
// touches the network and never writes a secret anywhere but the output envelope:
// the plaintext provider keys and kAppEncKey live only on the maintainer's machine.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/goozakdev/obelo-server/internal/rotation"
)

// options are the resolved inputs for one keytool run.
type options struct {
	tmdb          string // plaintext default TMDB key ("" ships none)
	fanart        string // plaintext default fanart.tv key ("" ships none)
	encKeyB64     string // base64 AES-256-GCM key (kAppEncKey) that seals the payload
	minAppVersion string // envelope minAppVersion gate (default "0.x" = any build)
	out           string // output path; "" or "-" writes to stdout
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "keytool:", err)
		os.Exit(1)
	}
}

// run parses args and writes the sealed envelope, split out from main so it is
// testable end-to-end (the round-trip test drives it and feeds the output to the
// rotation client). stdout receives the envelope only when no -o file is given, so
// piping keytool straight into `wrangler kv key put ... --path=-` stays clean.
func run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("keytool", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, usage)
		fs.PrintDefaults()
	}

	var opt options
	fs.StringVar(&opt.tmdb, "tmdb", os.Getenv("OBELO_BOOTSTRAP_TMDB_KEY"), "plaintext default TMDB key (default $OBELO_BOOTSTRAP_TMDB_KEY)")
	fs.StringVar(&opt.fanart, "fanart", os.Getenv("OBELO_BOOTSTRAP_FANART_KEY"), "plaintext default fanart.tv key (default $OBELO_BOOTSTRAP_FANART_KEY)")
	fs.StringVar(&opt.encKeyB64, "enc-key", os.Getenv("OBELO_APP_ENC_KEY"), "base64 AES-256-GCM app encryption key (default $OBELO_APP_ENC_KEY — prefer the env var so the key stays out of shell history)")
	fs.StringVar(&opt.minAppVersion, "min-app-version", "0.x", "envelope minAppVersion: only builds >= this adopt the payload (\"0.x\" = any)")
	fs.StringVar(&opt.out, "o", "-", "output file for the envelope JSON (\"-\" = stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	env, err := buildEnvelope(opt)
	if err != nil {
		return err
	}

	// Pretty-print so a maintainer can eyeball the envelope before publishing.
	blob, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding envelope: %w", err)
	}
	blob = append(blob, '\n')

	if opt.out == "" || opt.out == "-" {
		_, err = stdout.Write(blob)
		return err
	}
	if err := os.WriteFile(opt.out, blob, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", opt.out, err)
	}
	fmt.Fprintf(stderr, "wrote sealed envelope to %s\n", opt.out)
	return nil
}

// buildEnvelope validates the inputs and seals the provider keys into the versioned
// envelope the client expects. It is the whole contract of the tool, factored out so
// the round-trip test can seal here and open with the real rotation client. A missing
// enc key or an all-empty key set is a usage error (publishing an empty payload would
// silently wipe every install's default keys), caught here rather than shipped.
func buildEnvelope(opt options) (rotation.Envelope, error) {
	if opt.encKeyB64 == "" {
		return rotation.Envelope{}, fmt.Errorf("no app encryption key: pass -enc-key or set $OBELO_APP_ENC_KEY (the base64 kAppEncKey baked into official builds)")
	}
	if opt.tmdb == "" && opt.fanart == "" {
		return rotation.Envelope{}, fmt.Errorf("no provider keys: pass at least one of -tmdb / -fanart (an empty payload would strip every install's default keys)")
	}

	// Encrypt validates the key is a 32-byte AES-256 key and uses a fresh random
	// nonce per call, so re-running keytool on the same inputs yields a new payload.
	payload, err := rotation.Encrypt(opt.encKeyB64, rotation.Keys{TMDB: opt.tmdb, Fanart: opt.fanart})
	if err != nil {
		return rotation.Envelope{}, err
	}
	return rotation.Envelope{
		V:             rotation.SupportedVersion,
		MinAppVersion: opt.minAppVersion,
		Payload:       payload,
	}, nil
}

const usage = `keytool — seal default metadata provider keys into a rotation envelope (ADR-0032).

Emits the versioned JSON envelope the Obelo server fetches and decrypts, so a
leaked default TMDB/fanart.tv key can be revoked and replaced WITHOUT cutting a
release. See docs/runbooks/metadata-key-rotation.md for the full runbook.

Usage:
  keytool -tmdb <key> -fanart <key> [-enc-key <b64>] [-min-app-version 0.x] [-o file]

Prefer supplying the encryption key (and provider keys) via the OBELO_* env vars
so no secret lands in your shell history. Publish the output to the Worker's KV:
  keytool -o envelope.json && wrangler kv key put --binding=KEYS_KV envelope --path=envelope.json

Flags:
`
