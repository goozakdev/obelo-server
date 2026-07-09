// Command checkbundle fails loudly (non-zero exit) when the embedded SPA bundle
// is the committed placeholder rather than a real Vite build. Wire it into a
// release/CI gate (`make check-bundle`) so a binary that would serve the
// placeholder page never ships unnoticed (ADR-0012 build-order guard).
package main

import (
	"fmt"
	"os"

	"github.com/marioquake/juicebox/internal/webui"
)

func main() {
	if webui.IsPlaceholder() {
		fmt.Fprintln(os.Stderr,
			"checkbundle: embedded SPA is the PLACEHOLDER bundle. "+
				"Run `make web` (or `npm run build` in web/) before building the release binary.")
		os.Exit(1)
	}
	fmt.Println("checkbundle: real SPA bundle embedded.")
}
