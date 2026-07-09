package api

import (
	"os"
	"path/filepath"
	"time"
)

// sessionFile is an opened on-disk media File ready for http.ServeContent: the
// open handle (an io.ReadSeeker), its base name (content-type sniffing), and its
// modtime (caching / If-Modified-Since). The caller owns closing file.
type sessionFile struct {
	file    *os.File
	name    string
	modTime time.Time
}

// openSessionFile opens the File at path for progressive streaming. It returns an
// error when the path is missing or unreadable (the stream handler turns that
// into a 404 — the media negotiated fine but is gone now).
func openSessionFile(path string) (sessionFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return sessionFile{}, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return sessionFile{}, err
	}
	return sessionFile{
		file:    f,
		name:    filepath.Base(path),
		modTime: info.ModTime(),
	}, nil
}
