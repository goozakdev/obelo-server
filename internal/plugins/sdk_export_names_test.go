package plugins

import (
	"context"
	"sort"
	"testing"

	"github.com/tetratelabs/wazero"

	"github.com/goozakdev/obelo-server/pluginsdk"
)

// The SDK cannot drift from the host (.scratch/bundled-plugins issue 03).
//
// pluginsdk is its own Go module and may not import the server, so the export
// names its //go:wasmexport dispatchers use are COPIED from this package rather
// than shared with it. A copy is a thing that drifts, and the way this one would
// drift is the worst kind: rename an export here, and every plugin built with the
// SDK keeps compiling, keeps loading, and answers "the module does not export
// that call" at run time — on the maintainer's server, after a release.
//
// So the copy is held to the original from THIS side, where the unexported
// constants are visible. These tests are the reason the SDK's constants are
// exported at all.
//
// It is an internal test (package plugins, not plugins_test) for exactly that
// reason.

// TestTheSDKsMetadataExportNamesAreTheHosts compares the two lists element for
// element, in contract order. Adding a ninth Metadata provider call breaks this
// until the SDK has one too.
func TestTheSDKsMetadataExportNamesAreTheHosts(t *testing.T) {
	host := []string{
		exportMetadataLookup,
		exportMetadataSearch,
		exportMetadataArtworkCandidates,
		exportMetadataSeriesSeasons,
		exportMetadataSeasonEpisodes,
		exportMetadataAlbumTracklist,
		exportMetadataReleaseEditions,
		exportMetadataExternalRef,
	}
	assertSameList(t, "the Metadata provider exports", host, pluginsdk.MetadataExports())
}

// TestTheSDKsSubtitleExportNamesAreTheHosts is the same for the Subtitle provider
// seam, whose two exports carry the obelo_ prefix the metadata ones do not — a
// spelling nobody would guess and exactly the kind a copy gets wrong.
func TestTheSDKsSubtitleExportNamesAreTheHosts(t *testing.T) {
	host := []string{exportSubtitleSearch, exportSubtitleDownload}
	assertSameList(t, "the Subtitle provider exports", host, pluginsdk.SubtitleExports())
}

// TestTheSDKsABIExportNamesAreTheHosts covers the plumbing every module provides
// whatever seam it fills, plus the Event sink's one call and the host module's
// namespace.
func TestTheSDKsABIExportNamesAreTheHosts(t *testing.T) {
	assertSameList(t, "the ABI exports",
		[]string{exportAlloc, exportFree, exportLastError}, pluginsdk.ABIExports())

	if pluginsdk.ExportDeliver != exportDeliver {
		t.Errorf("the SDK calls the Event sink's export %q; the host looks up %q",
			pluginsdk.ExportDeliver, exportDeliver)
	}
	if pluginsdk.HostModule != hostModule {
		t.Errorf("the SDK imports its host functions from %q; the host registers them under %q",
			pluginsdk.HostModule, hostModule)
	}
}

// TestTheSDKsHostFunctionNamesAreTheHosts asks the RUNTIME rather than a second
// list of constants: it instantiates the real host module and compares what it
// actually exports with what the SDK declares //go:wasmimport for.
//
// That direction matters more than it looks. A host function the SDK names and
// the host does not register is a module that fails to INSTANTIATE — every call,
// for every plugin built with the SDK, with a message about a missing import and
// nothing about which side is wrong.
func TestTheSDKsHostFunctionNamesAreTheHosts(t *testing.T) {
	ctx := context.Background()
	rt := wazero.NewRuntime(ctx)
	defer func() { _ = rt.Close(ctx) }()

	h := &hostFuncs{}
	if err := h.instantiate(ctx, rt); err != nil {
		t.Fatalf("instantiating the host module: %v", err)
	}
	mod := rt.Module(hostModule)
	if mod == nil {
		t.Fatalf("the runtime has no module named %q after instantiating it", hostModule)
	}
	var registered []string
	for name := range mod.ExportedFunctionDefinitions() {
		registered = append(registered, name)
	}
	sort.Strings(registered)

	declared := pluginsdk.HostFuncs()
	sorted := append([]string(nil), declared...)
	sort.Strings(sorted)

	assertSameList(t, "the host functions", registered, sorted)
}

// assertSameList compares two lists element for element and says which side is
// missing what, because "not equal" on two string slices is the least useful
// failure a test can print.
func assertSameList(t *testing.T, what string, host, sdk []string) {
	t.Helper()
	if len(host) != len(sdk) {
		t.Fatalf("%s: the host has %d (%v), the SDK has %d (%v). "+
			"The SDK copies these names from this package; copy the new one across.",
			what, len(host), host, len(sdk), sdk)
	}
	for i := range host {
		if host[i] != sdk[i] {
			t.Errorf("%s: entry %d is %q on the host and %q in the SDK", what, i, host[i], sdk[i])
		}
	}
}
