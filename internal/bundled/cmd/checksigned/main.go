// Command checksigned is the release guard for Bundled plugin signatures
// (ADR-0069, issue 02): when the Obelo release public key is being compiled in, every
// module this build embeds must carry a signature that verifies under it. A release
// built with the key but unsigned modules would otherwise boot, refuse to install
// every Bundled plugin, and ship a server that enriches nothing.
//
// The key is read from OBELO_RELEASE_PUBLIC_KEY, the same value `make` and the
// Dockerfile hand to -ldflags. Unset or empty is a dev build and the guard passes
// without checking anything. Exit 1 on any module that fails.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/goozakdev/obelo-server/internal/bundled"
	"github.com/goozakdev/obelo-server/internal/plugins/signing"
)

func main() {
	os.Exit(check(os.Getenv("OBELO_RELEASE_PUBLIC_KEY"), bundled.Present(), os.Stderr))
}

func check(pubB64 string, ids []string, stderr io.Writer) int {
	if pubB64 == "" {
		fmt.Println("ok: no Obelo release key is compiled in; Bundled plugins ship unsigned")
		return 0
	}
	pub, err := signing.ParsePublicKey(pubB64)
	if err != nil {
		fmt.Fprintf(stderr, "ERROR: OBELO_RELEASE_PUBLIC_KEY is not an ed25519 public key: %v\n", err)
		return 1
	}
	if len(ids) == 0 {
		fmt.Fprintln(stderr, "ERROR: no Bundled module is embedded, so nothing could be checked — run make plugins")
		return 1
	}
	failed := 0
	for _, id := range ids {
		if err := bundled.VerifySigned(id, pub); err != nil {
			fmt.Fprintf(stderr, "ERROR: the embedded Bundled plugin %s: %v\n", id, err)
			failed++
		}
	}
	if failed > 0 {
		fmt.Fprintf(stderr, "ERROR: %d of %d embedded Bundled plugin(s) fail; a release with the key compiled in must sign every one\n", failed, len(ids))
		return 1
	}
	fmt.Printf("ok: %d embedded Bundled plugin(s) verify under the Obelo release key\n", len(ids))
	return 0
}
