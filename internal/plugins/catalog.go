package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/goozakdev/obelo-server/internal/useragent"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The optional catalog (.scratch/plugin-system issue 15): a JSON index an
// operator pointed this server at, so they can choose a plugin from a list
// instead of pasting an address they found somewhere.
//
// # It is off, and off is the shipped state
//
// There is no default index, no bundled address and no fallback (ADR-0001). The
// catalog URL is empty until an Admin types one, and while it is empty this file
// makes no request at all and the Plugins screen shows no Browse tab.
//
// # An unreachable catalog is a NOTE, never an error
//
// This is the load-bearing behaviour and it is easy to get wrong. The Plugins
// screen's other two paths — upload a file, paste a URL — have nothing to do with
// the catalog, and an index that is down, slow, moved or malformed must not take
// them away. So every failure here comes back as a sentence for the operator
// beside an EMPTY entry list, and the API answers 200 with it: the screen says
// "this catalog could not be read right now" and everything else on it keeps
// working. A 5xx would turn somebody else's outage into this server's.
//
// # The index is data; the plugin is code
//
// Fetching the index goes through the ordinary safefetch client, whose documented
// posture is that the FIRST hop is not address-checked — an operator serving their
// own index from a box on their own LAN is exactly the case this product exists
// for. Installing an entry does NOT relax anything: it is Manager.InstallFromURL
// with the entry's manifest URL, so the first hop IS checked there, and an entry
// resolving into this server's own network is refused with the same sentence a
// pasted address gets. Choosing from a list and executing what you chose are
// different acts and only the second is about code.

// CatalogResult is one attempt to read the operator's index: what was asked for,
// what came back, and — when nothing did — what to tell them.
//
// Note it carries both Entries and Note, and that either may be empty
// independently. A catalog that is reachable and genuinely empty is not an error
// and must not read as one.
type CatalogResult struct {
	// URL is the configured index, "" when none is set. A caller renders the Browse
	// tab exactly when this is non-empty — not when Entries is.
	URL string
	// Entries is what the index offered, in its own order. Empty when the fetch
	// failed, and then Note says why.
	Entries []pluginapi.CatalogEntry
	// Note is the quiet sentence for the operator when the catalog could not be
	// read. Empty means it was read.
	Note string
}

// Catalog reads the configured index.
//
// It NEVER returns an error for anything the catalog did — a store failure is an
// error because that is this server's own database, and everything else is a Note.
func (m *Manager) Catalog(ctx context.Context) (CatalogResult, error) {
	raw, err := m.catalogURL()
	if err != nil {
		return CatalogResult{}, err
	}
	target := strings.TrimSpace(raw)
	if target == "" {
		return CatalogResult{}, nil
	}
	out := CatalogResult{URL: target, Entries: []pluginapi.CatalogEntry{}}

	u, err := url.Parse(target)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		out.Note = "The catalog address is not an absolute http:// or https:// URL."
		return out, nil
	}

	ctx, cancel := context.WithTimeout(ctx, CatalogFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		out.Note = "The catalog address is not one this server can request."
		return out, nil
	}
	req.Header.Set("User-Agent", useragent.Default)
	req.Header.Set("Accept", "application/json")

	resp, err := m.client.Do(req)
	if err != nil {
		out.Note = fmt.Sprintf("The catalog at %s could not be reached right now.", target)
		return out, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		out.Note = fmt.Sprintf("The catalog at %s answered %d.", target, resp.StatusCode)
		return out, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxCatalogBytes+1))
	if err != nil {
		out.Note = fmt.Sprintf("The catalog at %s could not be read right now.", target)
		return out, nil
	}
	if int64(len(body)) > MaxCatalogBytes {
		out.Note = fmt.Sprintf("The catalog at %s is larger than %d bytes, which is not an index.",
			target, int64(MaxCatalogBytes))
		return out, nil
	}
	var index pluginapi.CatalogIndex
	if err := json.Unmarshal(body, &index); err != nil {
		out.Note = fmt.Sprintf("The catalog at %s did not answer with a plugin index.", target)
		return out, nil
	}
	out.Entries = usableEntries(index.Entries)
	if len(out.Entries) == 0 && len(index.Entries) > 0 {
		out.Note = "Every entry in this catalog is missing a manifest URL, so there is nothing to install from it."
	}
	return out, nil
}

// usableEntries drops the entries a server could do nothing with — one with no
// manifest URL is a row an operator could look at and not install — and leaves
// every other claim alone, because every other claim is display and the manifest
// is the authority for all of them.
//
// It does NOT filter by address, and that is deliberate: an entry pointing
// somewhere this server will not fetch from is still listed, and the refusal
// arrives when it is chosen, naming the reason. Hiding it would leave an operator
// comparing their catalog against their screen and finding a plugin missing with
// nothing said about why.
func usableEntries(in []pluginapi.CatalogEntry) []pluginapi.CatalogEntry {
	out := make([]pluginapi.CatalogEntry, 0, len(in))
	for _, e := range in {
		if strings.TrimSpace(e.ManifestURL) == "" {
			continue
		}
		if strings.TrimSpace(e.Name) == "" {
			e.Name = e.ID
		}
		out = append(out, e)
	}
	return out
}

// catalogURL is the configured index, or "" — which is both "never set" and
// "cleared", because they mean the same thing.
func (m *Manager) catalogURL() (string, error) {
	if m == nil || m.store == nil {
		return "", nil
	}
	return m.store.PluginCatalogURL()
}

// SetCatalogURL points this server at an index, or clears it with "".
//
// The URL is validated as an absolute http(s) address and NOT fetched: an
// operator saving the address of a catalog that happens to be down must not be
// told their address is wrong. Whether it answers is the next GET's business, and
// that one reports it as a note.
func (m *Manager) SetCatalogURL(rawURL string) error {
	if m.store == nil {
		return nil
	}
	target := strings.TrimSpace(rawURL)
	if target != "" {
		u, err := url.Parse(target)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return refuse(ReasonSource,
				"a catalog address must be an absolute http:// or https:// URL pointing at a plugin index")
		}
	}
	if err := m.store.SetPluginCatalogURL(target); err != nil {
		return err
	}
	if target == "" {
		m.logf("obelo: the plugin catalog was cleared by an admin")
	} else {
		m.logf("obelo: the plugin catalog is now %s", target)
	}
	return nil
}

// --- Pinned publishers ---------------------------------------------------------

// Publishers is the keys an Admin has pinned. An EMPTY list is the shipped policy
// — nothing is verified — rather than an absence of one, and the API says so on
// the screen rather than showing an empty table with no explanation.
func (m *Manager) Publishers() ([]Publisher, error) {
	pinned, err := m.pinnedPublishers()
	if err != nil {
		return nil, err
	}
	out := make([]Publisher, 0, len(pinned))
	for _, p := range pinned {
		out = append(out, Publisher{
			Publisher: p.Publisher,
			PublicKey: p.PublicKey,
			KeyID:     p.KeyID,
			AddedAt:   p.AddedAt,
		})
	}
	return out, nil
}

// Publisher is one pinned key as the API returns it. The public key is returned
// IN FULL and deliberately unmasked: it is public, and an operator has to be able
// to compare what they pinned against what a publisher advertises.
type Publisher struct {
	Publisher string `json:"publisher"`
	PublicKey string `json:"publicKey"`
	KeyID     string `json:"keyId,omitempty"`
	AddedAt   string `json:"addedAt,omitempty"`
}
