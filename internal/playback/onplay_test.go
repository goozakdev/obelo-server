package playback

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/access"
	"github.com/goozakdev/obelo-server/internal/store"
)

// TestStartingASessionTellsOnPlayTheFile: Seek preview generation moves a File
// someone starts playing to the front of its queue (ADR-0070 §3).
func TestStartingASessionTellsOnPlayTheFile(t *testing.T) {
	f := mp4File(1080, 6_000_000)
	st := &markerStore{ceilingStore: ceilingStore{detail: titleWith(store.Edition{ID: "e1", Files: []store.File{f}})}}
	svc := NewService(st, nil, "", Governance{})
	var played []string
	svc.SetOnPlay(func(fileID string) { played = append(played, fileID) })
	mustNegotiate(t, svc, Request{
		UserID: "u1", TitleID: "t1", Profile: uhdProfile(),
		Constraints: Constraints{MaxResolution: "2160p", MaxBitrate: 100_000_000},
		Scope:       access.Scope{AllLibraries: true},
	})
	if len(played) != 1 || played[0] != f.ID {
		t.Fatalf("onPlay saw %v, want [%s]", played, f.ID)
	}
}
