package catalog

import (
	"encoding/base64"
	"encoding/json"

	"github.com/marioquake/juicebox/internal/store"
)

// The cursor is an opaque, stable position within a sorted listing
// (api-contract.md: cursor pagination, not OFFSET). We encode the sort key of
// the last row plus its id and base64url the JSON — opaque to clients, and
// stable as the catalog mutates between pages because the next page seeks past
// the (sortKey, id) tuple rather than skipping a row count.

type cursorPayload struct {
	K string `json:"k"` // sort key (sort_title or added_at of the last row)
	I string `json:"i"` // id of the last row (tie-break)
}

func encodeCursor(c store.TitleCursor) string {
	b, _ := json.Marshal(cursorPayload{K: c.SortKey, I: c.ID})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (store.TitleCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return store.TitleCursor{}, err
	}
	var p cursorPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return store.TitleCursor{}, err
	}
	return store.TitleCursor{SortKey: p.K, ID: p.I}, nil
}
