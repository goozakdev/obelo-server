package enrich

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

func TestStoredParentRecordBlankNamespaceReadsAsNoRecord(t *testing.T) {
	rec := storedParentRecord(store.EntityEnrichment{
		ExternalID: "123", Namespace: "",
	})
	if rec != (parentRecord{}) {
		t.Fatalf("storedParentRecord = %+v, want the zero value (no record)", rec)
	}
}

// TestStoredParentRecordBlankIDReadsAsNoRecord: the reverse shape, a namespace
// beside a blank id, reads as no record too.
func TestStoredParentRecordBlankIDReadsAsNoRecord(t *testing.T) {
	rec := storedParentRecord(store.EntityEnrichment{
		ExternalID: "", Namespace: "musicbrainz",
	})
	if rec != (parentRecord{}) {
		t.Fatalf("storedParentRecord = %+v, want the zero value (no record)", rec)
	}
}

// TestStoredParentRecordReadsAnAlbumsIDAndNamespace: a music entity with both
// an id and a namespace reads as that record.
func TestStoredParentRecordReadsAnAlbumsIDAndNamespace(t *testing.T) {
	rec := storedParentRecord(store.EntityEnrichment{
		ExternalID: "rg-1", Namespace: "musicbrainz",
	})
	want := parentRecord{ID: "rg-1", Namespace: "musicbrainz"}
	if rec != want {
		t.Fatalf("storedParentRecord = %+v, want %+v", rec, want)
	}
}
