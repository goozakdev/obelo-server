package v1

// The catalog index (.scratch/plugin-system issue 15) — the document an operator
// points their server at to DISCOVER plugins, and the one wire type in this
// package a Plugin never sees.
//
// # This project runs no catalog and vouches for no publisher (ADR-0001)
//
// There is no default index, no bundled URL and no fallback. A server browses a
// catalog only when an Admin has typed its address, and the address they typed is
// the whole of the trust decision: their own index on their own host, or a
// community one they chose to believe. The format is here rather than in the
// server's internals so that such a community CAN publish one — a JSON document
// this shape is readable by any server speaking v1, and writable by anything that
// can serve a file.
//
// # A catalog entry is a manifest URL, and deliberately nothing more
//
// Installing from an entry is the ordinary URL install: the entry's ManifestURL
// goes to POST /settings/plugins/from-url, the module is fetched from beside the
// manifest exactly as it is for an address an Admin pasted by hand, and the same
// refusals apply — including the one that is unique to this path, where the FIRST
// hop is address-checked because what comes back is code the server will execute.
// A catalog therefore adds a way to CHOOSE a URL and adds nothing whatever to the
// install path. An index that lists an address inside the server's own network is
// refused with the same sentence a pasted one gets.
//
// Everything else in an entry — the name, the version, what it provides, who
// publishes it — is DISPLAY. It is the index author's claim, shown to an operator
// so they can decide, and no part of it is believed by the loader: the manifest
// fetched from ManifestURL is the authority for every one of those facts, and a
// signature (signature.go) is the only thing that makes the publisher more than a
// string somebody typed.

// CatalogVersion is the index format this server reads. An index declaring a
// HIGHER version is still read — the format is additive like the rest of v1, so a
// later index's extra fields are ignored rather than fatal — and one declaring 0
// is treated as 1, because an index written by hand is the common case and a
// missing version should not cost an operator their catalog.
const CatalogVersion = 1

// CatalogIndex is the whole document served at the catalog URL.
//
// One flat list, no paging and no query interface. A household's catalog is tens
// of entries, not thousands; a format that needed a server behind it would be a
// format only a hosted service could publish, which is the opposite of the point.
type CatalogIndex struct {
	// Version is the index format. See CatalogVersion for how a mismatch is read.
	Version int `json:"version"`
	// Entries is what the catalog offers, in the order the index author wrote them.
	// The order is preserved end to end, because it is the only grouping the format
	// can express.
	Entries []CatalogEntry `json:"entries"`
}

// CatalogEntry is one plugin a catalog offers.
//
// Every field but ManifestURL is a CLAIM the index author makes and the server
// shows without believing: the manifest at ManifestURL is what decides the id,
// the name, the version and the Extension points, and it is re-read and
// re-validated at install exactly as it is for a pasted URL. An entry that lies
// about what it points at produces a plugin that is what the manifest said, under
// the name the manifest gave it.
type CatalogEntry struct {
	// ID is the plugin's manifest id, so a screen can tell an operator which
	// entries they already have installed without fetching every manifest.
	ID string `json:"id"`
	// Name is the human name to list.
	Name string `json:"name"`
	// Version is the author's own version string, opaque and never parsed.
	Version string `json:"version,omitempty"`
	// Publisher is who the index says publishes this plugin. It is the name a
	// SIGNATURE would carry (signature.go) and the name an Admin pins a key
	// against — which is the only way it becomes more than a word in a file.
	Publisher string `json:"publisher,omitempty"`
	// Provides is the Extension points this plugin fills, in the contract's own
	// tokens, for the one line of a listing that says what it is for.
	Provides []ExtensionPoint `json:"provides,omitempty"`
	// ManifestURL is the absolute http(s) URL of the plugin's manifest.json, with
	// its module published in the same directory. IT IS THE ENTRY: everything the
	// server does with an entry it does with this one field.
	ManifestURL string `json:"manifestUrl"`
	// SignatureURL is where the detached signature document lives, when it is not
	// beside the manifest under its conventional name. Absent means "beside the
	// manifest", which is where an author following the publishing layout puts it
	// and where the server looks anyway.
	SignatureURL string `json:"signatureUrl,omitempty"`
	// Description and DocsURL are the human-facing copy a listing shows, the same
	// two fields a Manifest carries for the settings screen.
	Description string `json:"description,omitempty"`
	DocsURL     string `json:"docsUrl,omitempty"`
}
