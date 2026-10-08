// Command pluginsign is the plugin author's half of manifest signing
// (.scratch/plugin-system issue 15): it makes a publisher key pair, signs a
// manifest and a module with it, and verifies the result.
//
//	pluginsign keygen  -out publisher.key
//	pluginsign sign    -key publisher.key -publisher "Example Publisher" \
//	                   -manifest manifest.json -module plugin.wasm [-icon icon.png] \
//	                   -out plugin.sig.json
//	pluginsign verify  -sig plugin.sig.json -pub <base64> \
//	                   -manifest manifest.json -module plugin.wasm
//	pluginsign pack    -manifest manifest.json -module plugin.wasm [-icon icon.png] \
//	                   [-signature plugin.sig.json] [-out <id>-<version>.zip]
//	pluginsign verify  -package plugin.zip -pub <base64>
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
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/plugins/signing"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

const usage = `pluginsign — sign an Obelo plugin, and check one.

  pluginsign keygen [-out FILE]
      Make an ed25519 publisher key pair. The PRIVATE key is written to FILE
      (mode 0600) or to stdout; the PUBLIC key and its key id are printed to
      stderr. The public key is what an operator pins on their server; the
      private key never leaves your machine and nothing here ever uploads it.

  pluginsign sign -key FILE -publisher NAME -manifest FILE -module FILE [-icon FILE] [-out FILE]
      Write the detached signature document (` + pluginapi.SignatureFile + `) covering
      those exact bytes. Put it in the package with pack -signature FILE.
      Re-sign whenever ANY covered file changes: the signature covers the
      manifest and the module, and an icon when there is one, and a manifest
      edited by one byte is a different manifest.

  pluginsign pack -manifest FILE -module FILE [-icon FILE] [-signature FILE] [-out FILE]
      Build the Plugin package a server installs: a .zip holding the manifest,
      the module and, when present, the icon and the signature, at its root.
      The default output is <id>-<version>.zip. It refuses anything a server
      would refuse to unpack.

  The icon (a square PNG of at most 64 KiB, an Online source's tile image) is
  ` + plugins.IconFile + ` beside the manifest when -icon is not given; sign, pack and
  verify all use it when it exists, so they cover the same bytes. A source with no
  icon file simply has none.

  pluginsign verify -sig FILE -pub BASE64|-key FILE -manifest FILE -module FILE [-icon FILE]
  pluginsign verify -package FILE -pub BASE64|-key FILE
      Check a signature the way a server checks it -- against loose files, or
      against the signature inside a package. Exit 0 if it verifies.

`

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "pluginsign:", err)
		var ue usageError
		if errors.As(err, &ue) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

// usageError is a command line that cannot mean what it says, as opposed to a
// check that ran and failed; main exits 2 for it.
type usageError string

func (e usageError) Error() string { return string(e) }

// run is main split out so the round-trip test can drive the real command and
// feed its output through the server's install path — which is the only way "the
// signing command's output verifies in the server" gets asserted rather than
// asserted about.
func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return fmt.Errorf("say what to do: keygen, sign, pack or verify")
	}
	switch args[0] {
	case "keygen":
		return runKeygen(args[1:], stdout, stderr)
	case "sign":
		return runSign(args[1:], stdout, stderr)
	case "pack":
		return runPack(args[1:], stdout, stderr)
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
	iconPath := fs.String("icon", "", "the tile icon to cover (default: icon.png beside the manifest, when it exists)")
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
	icon, err := readIcon(*iconPath, *manifestPath)
	if err != nil {
		return err
	}
	sig, err := signing.SignWithIcon(priv, *publisher, manifest, module, icon)
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
	fmt.Fprintf(stderr, "signed %s and %s%s as %q (key id %s)\n",
		*manifestPath, *modulePath, iconNote(icon), sig.Publisher, sig.KeyID)
	fmt.Fprintf(stderr, "put this file in the package with: pluginsign pack -signature <this file> (it travels as %s)\n", pluginapi.SignatureFile)
	return nil
}

// --- pack ---------------------------------------------------------------------

func runPack(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("pluginsign pack", flag.ContinueOnError)
	fs.SetOutput(stderr)
	manifestPath := fs.String("manifest", "manifest.json", "the plugin's manifest.json")
	modulePath := fs.String("module", "plugin.wasm", "the plugin's WebAssembly module")
	iconPath := fs.String("icon", "", "the tile icon to include (default: icon.png beside the manifest, when it exists)")
	sigPath := fs.String("signature", "", "the detached signature to include (optional)")
	out := fs.String("out", "", `file to write the package to (default <id>-<version>.zip, "-" = stdout)`)
	if err := fs.Parse(args); err != nil {
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
	var sig []byte
	if *sigPath != "" {
		if sig, err = os.ReadFile(*sigPath); err != nil {
			return fmt.Errorf("reading the signature: %w", err)
		}
	}
	icon, err := readIcon(*iconPath, *manifestPath)
	if err != nil {
		return err
	}
	// PackPackageWithIcon checks its own output with the routine a server unpacks
	// with, so a package this writes is one a server will take.
	archive, err := plugins.PackPackageWithIcon(manifest, module, sig, icon)
	if err != nil {
		return err
	}
	target := *out
	if target == "" {
		var man pluginapi.Manifest
		if err := json.Unmarshal(manifest, &man); err != nil {
			return err
		}
		target = man.ID + ".zip"
		if man.Version != "" {
			target = man.ID + "-" + man.Version + ".zip"
		}
	}
	if err := writeOut(target, archive, 0o644, stdout); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "packed %s and %s%s into %s (%d bytes)\n", *manifestPath, *modulePath, iconNote(icon), target, len(archive))
	return nil
}

// --- verify -------------------------------------------------------------------

func runVerify(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("pluginsign verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sigPath := fs.String("sig", pluginapi.SignatureFile, "the detached signature document")
	packagePath := fs.String("package", "", "a plugin package (.zip) to check, instead of -sig/-manifest/-module")
	pubB64 := fs.String("pub", "", "the base64 ed25519 PUBLIC key to check against")
	keyPath := fs.String("key", "", "a private key file to derive the public key from, instead of -pub")
	manifestPath := fs.String("manifest", "manifest.json", "the plugin's manifest.json")
	modulePath := fs.String("module", "plugin.wasm", "the plugin's WebAssembly module")
	iconPath := fs.String("icon", "", "the tile icon the signature covers (default: icon.png beside the manifest, when it exists)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// -package checks the signature inside the archive, so a loose-file flag beside
	// it would be silently ignored. Whether it was given, not what it was given, is
	// what selects that mode: `-package=` must not fall back to the loose files.
	var packageGiven bool
	var conflicts []string
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "package":
			packageGiven = true
		case "sig", "manifest", "module", "icon":
			conflicts = append(conflicts, "-"+f.Name)
		}
	})
	if packageGiven {
		if len(conflicts) > 0 {
			return usageError("-package cannot be combined with " + strings.Join(conflicts, ", ") +
				": it checks the signature inside the package")
		}
		if *packagePath == "" {
			return usageError("-package needs a path")
		}
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

	var sigRaw, manifest, module, icon []byte
	var err error
	if packageGiven {
		archive, err := os.ReadFile(*packagePath)
		if err != nil {
			return fmt.Errorf("reading the package: %w", err)
		}
		pkg, err := plugins.UnpackPackage(archive)
		if err != nil {
			return err
		}
		if len(pkg.Signature) == 0 {
			return fmt.Errorf("the package holds no %s, so there is nothing to verify", pluginapi.SignatureFile)
		}
		sigRaw, manifest, module, icon = pkg.Signature, pkg.Manifest, pkg.Module, pkg.Icon
	} else {
		if sigRaw, err = os.ReadFile(*sigPath); err != nil {
			return fmt.Errorf("reading the signature: %w", err)
		}
		if manifest, err = os.ReadFile(*manifestPath); err != nil {
			return fmt.Errorf("reading the manifest: %w", err)
		}
		if module, err = os.ReadFile(*modulePath); err != nil {
			return fmt.Errorf("reading the module: %w", err)
		}
		if icon, err = readIcon(*iconPath, *manifestPath); err != nil {
			return err
		}
	}
	sig, err := signing.Parse(sigRaw)
	if err != nil {
		return err
	}
	if err := signing.VerifyWithIcon(sig, pub, manifest, module, icon); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "ok: signed by %q (key id %s)\n", sig.Publisher, sig.KeyID)
	return nil
}

// --- small helpers -------------------------------------------------------------

// readIcon is the tile icon sign, pack and verify agree on: the file named by -icon,
// else icon.png beside the manifest when there is one, else none. An icon named
// explicitly that cannot be read is an error; a missing default is not.
func readIcon(iconPath, manifestPath string) ([]byte, error) {
	if iconPath != "" {
		icon, err := os.ReadFile(iconPath)
		if err != nil {
			return nil, fmt.Errorf("reading the icon: %w", err)
		}
		return icon, nil
	}
	icon, err := os.ReadFile(filepath.Join(filepath.Dir(manifestPath), plugins.IconFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the icon: %w", err)
	}
	return icon, nil
}

// iconNote is the part of a progress line that says an icon was involved.
func iconNote(icon []byte) string {
	if icon == nil {
		return ""
	}
	return " and " + plugins.IconFile
}

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
