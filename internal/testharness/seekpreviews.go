package testharness

import (
	"os"
	"path/filepath"
	"strconv"

	"github.com/goozakdev/obelo-server/internal/store"
)

// SeedSeekPreviews records a Seek preview set for a File and writes its sheets under
// <data>/seekpreviews/<fileID>/<n>.jpg, as generation would. set.FileID is filled in
// and set.SheetCount is len(sheets) when left zero; an empty set.Mtime takes the File
// row's current mtime and size, so the set reads as fresh. A direct-DB seam: generation
// needs a real ffmpeg decode, and delivery tests care about what is served, not how
// the sheets were made.
func (s *Server) SeedSeekPreviews(fileID string, set store.SeekPreviewSet, sheets [][]byte) {
	s.t.Helper()
	set.FileID = fileID
	if set.Mtime == "" {
		if err := s.app.DB.QueryRow(`SELECT mtime, size_bytes FROM files WHERE id = ?`, fileID).Scan(&set.Mtime, &set.SizeBytes); err != nil {
			s.t.Fatalf("testharness: reading file %s mtime: %v", fileID, err)
		}
	}
	if set.SheetCount == 0 {
		set.SheetCount = len(sheets)
	}
	dir := filepath.Join(s.DataDir, "seekpreviews", fileID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.t.Fatalf("testharness: seek preview dir: %v", err)
	}
	for i, b := range sheets {
		if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(i)+".jpg"), b, 0o644); err != nil {
			s.t.Fatalf("testharness: writing seek preview sheet %d: %v", i, err)
		}
	}
	if err := s.app.DB.SaveSeekPreview(set); err != nil {
		s.t.Fatalf("testharness: saving seek preview record: %v", err)
	}
}

// SetSeekPreviewsEnabled flips a Library's generation toggle.
func (s *Server) SetSeekPreviewsEnabled(libraryID string, on bool) {
	s.t.Helper()
	if err := s.app.DB.SetSeekPreviewsEnabled(libraryID, on); err != nil {
		s.t.Fatalf("testharness: seek preview toggle: %v", err)
	}
}
