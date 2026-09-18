package v1

// The plugin-scoped key-value store, on the wire (ADR-0058 decision 5, third host
// function).
//
// A Plugin's own small durable namespace in SQLite (ADR-0007: state in SQLite,
// blobs on disk). It is for the things a source-shaped Plugin accumulates and
// would otherwise re-derive on every boot: a paging cursor, an etag, a token's
// expiry, a small response cache.
//
// It is NOT settings. Settings are the Admin's, arrive resolved from the host and
// are never written by a guest; this is the guest's own scratch space, which the
// host never reads. And it is NOT shared: every key is scoped by the Plugin's id,
// so two Plugins writing the same key never see each other's value, and
// uninstalling a Plugin drops the whole namespace.
//
// Keys are text and values are bytes, both size-capped by the host. A guest that
// exceeds a cap is REFUSED rather than silently truncated, for the reason an
// oversize fetch is: a truncated document is one a guest parses as complete.

// KVGetRequest asks for one key in this Plugin's own namespace.
type KVGetRequest struct {
	// Key is the entry to read. The Plugin id is NOT part of it and must not be:
	// the host adds the namespace, from the manifest on disk, so a guest cannot
	// name another Plugin's key by spelling one.
	Key string `json:"key"`
}

// KVGetResponse is what a read answers.
//
// Found is the whole point of the type: a key that was never written and a key
// holding zero bytes are different facts, and a guest caching "I asked and there
// was nothing" has to tell them apart. Error is a host-side failure (the store is
// unreachable, the key is longer than a key may be) and is not the same as an
// absent key.
type KVGetResponse struct {
	Found bool   `json:"found"`
	Value []byte `json:"value,omitempty"`
	Error string `json:"error,omitempty"`
}

// KVSetRequest writes one key in this Plugin's own namespace, replacing whatever
// was there.
type KVSetRequest struct {
	Key   string `json:"key"`
	Value []byte `json:"value,omitempty"`
}

// KVDeleteRequest removes one key from this Plugin's own namespace. Deleting a key
// that was never written is not an error — it is the state the caller asked for.
type KVDeleteRequest struct {
	Key string `json:"key"`
}

// KVWriteResponse is what a write or a delete answers. OK is false with an Error
// for a refusal (an oversize key or value) or a store failure; there is no outcome
// here because a write has no domain judgment to report.
type KVWriteResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}
