package plugintest

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io/fs"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// ZipMember is one entry of a fixture archive, described down to the attributes a
// well-formed Plugin package never has, so a test can hand the server exactly the
// malformed package it means to refuse.
type ZipMember struct {
	Name string
	Body []byte
	// Mode, when non-zero, is the entry's file mode (fs.ModeSymlink for a link).
	Mode fs.FileMode
	// Store writes the entry uncompressed instead of deflated.
	Store bool
}

// Zip builds an archive from exactly the members given, in order, with no
// validation: the fixture for a package test is built here and never committed.
func Zip(t testing.TB, members ...ZipMember) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, m := range members {
		h := &zip.FileHeader{Name: m.Name, Method: zip.Deflate}
		if m.Store {
			h.Method = zip.Store
		}
		if m.Mode != 0 {
			h.SetMode(m.Mode)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatalf("plugintest: adding %q to a fixture zip: %v", m.Name, err)
		}
		if _, err := w.Write(m.Body); err != nil {
			t.Fatalf("plugintest: writing %q into a fixture zip: %v", m.Name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("plugintest: closing a fixture zip: %v", err)
	}
	return buf.Bytes()
}

// PackageZip is the well-formed Plugin package for these files: the manifest, the
// module under the name the manifest gives it, and the signature when there is
// one. It does not validate the manifest — a refusal test wants to send a bad one.
func PackageZip(t testing.TB, manifest, module, signature []byte) []byte {
	t.Helper()
	var m pluginapi.Manifest
	_ = json.Unmarshal(manifest, &m)
	modName := m.Module
	if modName == "" {
		modName = plugins.DefaultModuleFile
	}
	members := []ZipMember{{Name: plugins.ManifestFile, Body: manifest}, {Name: modName, Body: module}}
	if len(signature) > 0 {
		members = append(members, ZipMember{Name: pluginapi.SignatureFile, Body: signature})
	}
	return Zip(t, members...)
}
