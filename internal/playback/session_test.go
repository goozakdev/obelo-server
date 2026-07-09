package playback

import (
	"testing"

	"github.com/marioquake/juicebox/internal/store"
)

func TestSessionManagerLifecycle(t *testing.T) {
	m := NewManager()
	dec := Decision{
		Tier:    TierDirectPlay,
		Edition: store.Edition{ID: "e1"},
		File:    store.File{ID: "f1", Path: "/movies/x.mp4"},
	}
	s := m.Create(CreateInput{UserID: "u1", DeviceID: "d1", TitleID: "t1", StartPosition: 5000}, dec)

	if s.ID == "" {
		t.Fatal("created session has empty id")
	}
	if s.FilePath != "/movies/x.mp4" || s.EditionID != "e1" || s.FileID != "f1" {
		t.Errorf("session record = %+v, missing decision fields", s)
	}
	if s.StartPosition != 5000 || s.UserID != "u1" || s.DeviceID != "d1" {
		t.Errorf("session record = %+v, missing input fields", s)
	}
	if m.Count() != 1 {
		t.Errorf("count = %d, want 1", m.Count())
	}

	got, ok := m.Get(s.ID)
	if !ok || got.ID != s.ID {
		t.Fatalf("Get(%q) = %+v, %v; want the session", s.ID, got, ok)
	}

	if !m.End(s.ID) {
		t.Error("End returned false for an existing session")
	}
	if _, ok := m.Get(s.ID); ok {
		t.Error("session still present after End")
	}
	if m.End(s.ID) {
		t.Error("second End returned true; want false for an already-ended session")
	}
	if m.Count() != 0 {
		t.Errorf("count = %d after End, want 0", m.Count())
	}
}

func TestSessionManagerUnknownGet(t *testing.T) {
	m := NewManager()
	if _, ok := m.Get("nope"); ok {
		t.Error("Get of unknown id returned ok=true")
	}
	if m.End("nope") {
		t.Error("End of unknown id returned true")
	}
}
