// Command pluginsign is the plugin author's half of manifest signing
// (.scratch/plugin-system issue 15): it makes a publisher key pair, signs a
// manifest and a module with it, and verifies the result.
//
//	pluginsign keygen  -out publisher.key
//	pluginsign sign    -key publisher.key -publisher "Example Publisher" \
//	                   -manifest manifest.json -module plugin.wasm -out plugin.sig.json
//	pluginsign verify  -sig plugin.sig.json -pub <base64> \
//	                   -manifest manifest.json -module plugin.wasm
//
// # It is NOT cmd/keytool, and that is the point
//
// `keytool` seals the maintainer's default provider keys into an AES-GCM envelope
// for key rotation (ADR-0032): symmetric encryption of a secret, for one channel,
// with one recipient. Signing a plugin is asymmetric authentication of a public
// artifact by an author this project has never met. Different primitive, different
// key material, different threat. Reusing `keytool` would have made one key file
// mean two incompatible things, so this is a new and separate command and
// `cmd/keytool` is untouched.
//
// # The verify here is the server's verify
//
// Every byte of the signing and checking logic lives in internal/plugins/signing,
// which is also what the server calls when a plugin is installed. That is what
// makes "the tool's output verifies in the server" a property and not a hope: this
// file is argument parsing and file I/O over it, and there is no second opinion
// anywhere about what the signed message is.
//
// # What it never does
//
// It makes no network request, contacts no registry and publishes nothing. A
// signature is trusted by a server only because an operator pinned the matching
// public key on it by hand (ADR-0001).
package main

import (
	"crypto/ed25519"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/goozakdev/obelo-server/internal/plugins/signing"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

const usage = `pluginsign — sign an Obelo plugin, and check one.

  pluginsign keygen [-out FILE]
      Make an ed25519 publisher key pair. The PRIVATE key is written to FILE
      (mode 0600) or to stdout; the PUBLIC key and its key id are printed to
      stderr. The public key is what an operator pins on their server; the
      private key never leaves your machine and nothing here ever uploads it.

  pluginsign sign -key FILE -publisher NAME -manifest FILE -module FILE [-out FILE]
      Write the detached signature document (` + pluginapi.SignatureFile + `) covering
      those exact bytes. Publish it in the same directory as the manifest and
      the module. Re-sign whenever EITHER file changes: the signature covers
      both, and a manifest edited by one byte is a different manifest.

  pluginsign verify -sig FILE -pub BASE64|-key FILE -manifest FILE -module FILE
      Check a signature the way a server checks it. Exit 0 if it verifies.

`

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "pluginsign:", err)
		os.Exit(1)
	}
}

// run is main split out so the round-trip test can drive the real command and
// feed its output through the server's install path — which is the only way "the
// signing command's output verifies in the server" gets asserted rather than
// asserted about.
func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return fmt.Errorf("say what to do: keygen, sign or verify")
	}
	switch args[0] {
	case "keygen":
		return runKeygen(args[1:], stdout, stderr)
	case "sign":
		return runSign(args[1:], stdout, stderr)
	case "verify":
		return runVerify(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stderr, usage)
		return nil
	default:
		fmt.Fprint(stderr, usage)
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

// --- keygen -------------------------------------------------------------------

func runKeygen(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("pluginsign keygen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "-", `file to write the PRIVATE key to ("-" = stdout)`)
	if err := fs.Parse(args); err != nil {
		return err
	}

	pub, priv, err := signing.GenerateKey()
	if err != nil {
		return err
	}
	// The private key is written with 0600 and NOTHING ELSE is written to the same
	// place: an operator who redirects stdout to a file must not find the public
	// key and a paragraph of prose in their key file.
	if err := writeOut(*out, []byte(signing.EncodeKey(priv)+"\n"), 0o600, stdout); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "public key: %s\n", signing.EncodeKey(pub))
	fmt.Fprintf(stderr, "key id:     %s\n", signing.KeyID(pub))
	fmt.Fprintf(stderr, "\nPin the PUBLIC key on a server under the publisher name you will sign with.\n"+
		"Keep the private key: it is the only thing that makes a signature yours.\n")
	return nil
}

// --- sign ---------------------------------------------------------------------

func runSign(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("pluginsign sign", flag.ContinueOnError)
	fs.SetOutput(stderr)
	keyPath := fs.String("key", "", "file holding the base64 ed25519 PRIVATE key (from keygen)")
	publisher := fs.String("publisher", "", "the publisher name a server pins your key against")
	manifestPath := fs.String("manifest", "manifest.json", "the plugin's manifest.json")
	modulePath := fs.String("module", "plugin.wasm", "the plugin's WebAssembly module")
	out := fs.String("out", pluginapi.SignatureFile, `file to write the signature to ("-" = stdout)`)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *keyPath == "" {
		return fmt.Errorf("-key is required: sign with the private key keygen made")
	}
	priv, err := readPrivateKey(*keyPath)
	if err != nil {
		return err
	}
	manifest, err := os.ReadFile(*manifestPath)
	if err != nil {
		return fmt.Errorf("reading the manifest: %w", err)
	}
	module, err := os.ReadFile(*modulePath)
	if err != nil {
		return fmt.Errorf("reading the module: %w", err)
	}
	sig, err := signing.Sign(priv, *publisher, manifest, module)
	if err != nil {
		return err
	}
	doc, err := signing.Encode(sig)
	if err != nil {
		return err
	}
	if err := writeOut(*out, doc, 0o644, stdout); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "signed %s and %s as %q (key id %s)\n",
		*manifestPath, *modulePath, sig.Publisher, sig.KeyID)
	fmt.Fprintf(stderr, "publish this file beside the manifest as %s\n", pluginapi.SignatureFile)
	return nil
}

// --- verify -------------------------------------------------------------------

func runVerify(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("pluginsign verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sigPath := fs.String("sig", pluginapi.SignatureFile, "the detached signature document")
	pubB64 := fs.String("pub", "", "the base64 ed25519 PUBLIC key to check against")
	keyPath := fs.String("key", "", "a private key file to derive the public key from, instead of -pub")
	manifestPath := fs.String("manifest", "manifest.json", "the plugin's manifest.json")
	modulePath := fs.String("module", "plugin.wasm", "the plugin's WebAssembly module")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var pub ed25519.PublicKey
	switch {
	case *pubB64 != "":
		parsed, err := signing.ParsePublicKey(*pubB64)
		if err != nil {
			return err
		}
		pub = parsed
	case *keyPath != "":
		priv, err := readPrivateKey(*keyPath)
		if err != nil {
			return err
		}
		pub = priv.Public().(ed25519.PublicKey)
	default:
		return fmt.Errorf("give -pub (the public key an operator would pin) or -key (a private key to derive it from)")
	}

	sigRaw, err := os.ReadFile(*sigPath)
	if err != nil {
		return fmt.Errorf("reading the signature: %w", err)
	}
	sig, err := signing.Parse(sigRaw)
	if err != nil {
		return err
	}
	manifest, err := os.ReadFile(*manifestPath)
	if err != nil {
		return fmt.Errorf("reading the manifest: %w", err)
	}
	module, err := os.ReadFile(*modulePath)
	if err != nil {
		return fmt.Errorf("reading the module: %w", err)
	}
	if err := signing.Verify(sig, pub, manifest, module); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "ok: signed by %q (key id %s)\n", sig.Publisher, sig.KeyID)
	return nil
}

// --- small helpers -------------------------------------------------------------

func readPrivateKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the private key: %w", err)
	}
	priv, err := signing.ParsePrivateKey(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return priv, nil
}

// writeOut sends bytes to a file or to stdout. A file is created with the mode
// given and never widened — a private key written 0600 stays 0600 even when the
// file was already there with looser bits, because the alternative is a key file
// that is world-readable because it once was.
func writeOut(path string, body []byte, mode os.FileMode, stdout io.Writer) error {
	if path == "" || path == "-" {
		_, err := stdout.Write(body)
		return err
	}
	if err := os.WriteFile(path, body, mode); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("setting the mode of %s: %w", path, err)
	}
	return nil
}
