package plugins

import (
	"archive/zip"
	"bytes"
	"fmt"
	"image"
	_ "image/png" // registers the PNG decoder image.DecodeConfig reads the icon header with
	"io"
	"io/fs"
	"sort"
	"strings"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// A Plugin package is the one file an Admin hands this server to install a Plugin:
// a zip holding, at its root and nothing else, the manifest, the module the
// manifest names, and optionally the detached signature and a tile icon.
//
//	manifest.json
//	plugin.wasm        (or whatever the manifest's `module` says)
//	plugin.sig.json    (optional)
//	icon.png           (optional; an Online source's tile image, ADR-0068)
//
// # Why the layout is this strict
//
// Unpacking an archive an untrusted person made is where the two well-known
// archive attacks live: a member called "../../obelo.db" that escapes the
// directory it is extracted into, and a few kilobytes that decompress into
// gigabytes. Neither is defended against by being clever. The package is read
// ENTIRELY IN MEMORY and nothing in it is ever written under the name it carries —
// the members go to fixed names in a directory this server chose — and every
// member is read through a cap on the DECOMPRESSED bytes, never trusting the size
// the archive's own header claims. Anything that is not one of the expected
// files is refused, by name, with a sentence saying what was found and what was
// expected: a wrapping folder, a __MACOSX directory, a directory entry, a symlink,
// a duplicate, an unknown file, an encrypted member or an exotic compression method.
//
// Bytes before or after the archive (a self-extracting stub, trailing junk) are
// tolerated, because Go's archive/zip reads from the central directory at the end
// of the file; every member is still checked as above.
//
// A single wrapping folder is NOT tolerated. Being lenient about it would mean two
// package shapes to describe and test, and `pluginsign pack` produces the right one.
//
// The signature covers the manifest and module bytes (ADR-0058), and the icon when
// the package carries one (ADR-0068), not the archive, so where it travels adds
// nothing to the trust decision; that is decided by the keys an Admin pinned.
//
// The icon is checked at unpack, so a bad one is refused wherever a package is read
// (upload, URL, `pluginsign pack`): a real PNG, square, at most MaxIconBytes and at
// most maxIconSide on a side. It is served to Users from this server's own origin
// and never from anywhere else.

// MaxPackageBytes caps a Plugin package, uploaded or fetched: the members'
// own caps plus a megabyte for the zip framing. Larger is refused, never truncated.
const MaxPackageBytes = MaxManifestBytes + MaxModuleBytes + MaxSignatureBytes + (1 << 20)

// IconFile is the optional tile image a package may carry beside the manifest.
const IconFile = "icon.png"

// MaxIconBytes caps the icon once unpacked: 64 KiB, the cap ADR-0068 sets. The
// archive's 1 MiB of framing allowance already has room for it.
const MaxIconBytes = 64 << 10

// maxIconSide bounds a tile icon's width and height. A few kilobytes of PNG can
// declare a canvas of billions of pixels that a browser would try to allocate; a
// tile is drawn at a few dozen.
const maxIconSide = 1024

// Package is the files a Plugin package unpacks to.
type Package struct {
	Manifest  []byte
	Module    []byte
	Signature []byte // nil when the package carried none
	Icon      []byte // nil when the package carried none
}

// UnpackPackage checks a Plugin package's layout and returns its contents. Every
// refusal is a *Refusal: ReasonPackage for a malformed archive, and the manifest's
// own reasons for a manifest that is not acceptable.
func UnpackPackage(archive []byte) (Package, error) {
	if int64(len(archive)) > MaxPackageBytes {
		return Package{}, refuse(ReasonPackage,
			"the plugin package is %d bytes, and this server will not accept one larger than %d",
			len(archive), int64(MaxPackageBytes))
	}
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return Package{}, refuse(ReasonPackage,
			"this is not a plugin package: it is not a readable .zip file (%v)", err)
	}

	byName := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		if err := checkPackageMember(f, byName); err != nil {
			return Package{}, err
		}
		byName[f.Name] = f
	}

	mf, ok := byName[ManifestFile]
	if !ok {
		return Package{}, refuse(ReasonPackage,
			"the plugin package has no %s at its root; found %s", ManifestFile, memberList(byName))
	}
	manifestRaw, err := readMember(mf, MaxManifestBytes, "manifest")
	if err != nil {
		return Package{}, err
	}
	man, err := decodeManifest(manifestRaw)
	if err != nil {
		return Package{}, err
	}

	modName := moduleFile(man)
	expected := map[string]bool{ManifestFile: true, modName: true, pluginapi.SignatureFile: true, IconFile: true}
	for name := range byName {
		if !expected[name] {
			return Package{}, refuse(ReasonPackage,
				"the plugin package holds %q, which is not part of a plugin; a package holds only %s, %s and optionally %s and %s",
				name, ManifestFile, modName, pluginapi.SignatureFile, IconFile)
		}
	}
	modf, ok := byName[modName]
	if !ok {
		return Package{}, refuse(ReasonPackage,
			"the plugin package has no module: the manifest names %s and the package holds %s",
			modName, memberList(byName))
	}
	module, err := readMember(modf, MaxModuleBytes, "module")
	if err != nil {
		return Package{}, err
	}
	pkg := Package{Manifest: manifestRaw, Module: module}
	if sf, ok := byName[pluginapi.SignatureFile]; ok {
		sig, err := readMember(sf, MaxSignatureBytes, "signature")
		if err != nil {
			return Package{}, err
		}
		if len(sig) > 0 {
			pkg.Signature = sig
		}
	}
	if icf, ok := byName[IconFile]; ok {
		icon, err := readMember(icf, MaxIconBytes, "icon")
		if err != nil {
			return Package{}, err
		}
		if err := checkIcon(icon); err != nil {
			return Package{}, err
		}
		pkg.Icon = icon
	}
	return pkg, nil
}

// checkIcon refuses an icon that is empty, is not a PNG, or is not square. The
// size cap was applied as it was read. Only the header is decoded: the bytes are
// served as they came, never re-encoded.
func checkIcon(icon []byte) error {
	if len(icon) == 0 {
		return refuse(ReasonPackage, "the plugin package's %s is empty; it must be a square PNG of at most %d KiB", IconFile, MaxIconBytes>>10)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(icon))
	if err != nil || format != "png" {
		return refuse(ReasonPackage, "the plugin package's %s is not a PNG; the tile icon must be a square PNG of at most %d KiB", IconFile, MaxIconBytes>>10)
	}
	if cfg.Width != cfg.Height {
		return refuse(ReasonPackage, "the plugin package's %s is %d by %d pixels; the tile icon must be square", IconFile, cfg.Width, cfg.Height)
	}
	if cfg.Width > maxIconSide {
		return refuse(ReasonPackage, "the plugin package's %s is %d pixels on a side; the tile icon may be at most %d", IconFile, cfg.Width, maxIconSide)
	}
	return nil
}

// PackPackage builds a Plugin package from a manifest, its module and an optional
// signature. It refuses whatever UnpackPackage would refuse, by checking its own
// output with it, so a package this makes always installs. The output is
// deterministic: the same inputs give the same bytes.
func PackPackage(manifestRaw, module, signatureRaw []byte) ([]byte, error) {
	return PackPackageWithIcon(manifestRaw, module, signatureRaw, nil)
}

// PackPackageWithIcon is PackPackage with an optional icon.png; a nil icon gives
// exactly the package PackPackage does.
func PackPackageWithIcon(manifestRaw, module, signatureRaw, icon []byte) ([]byte, error) {
	man, err := decodeManifest(manifestRaw)
	if err != nil {
		return nil, err
	}
	if len(module) == 0 {
		return nil, refuse(ReasonModule, "the module %s is empty", moduleFile(man))
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	members := []struct {
		name string
		body []byte
	}{{ManifestFile, manifestRaw}, {moduleFile(man), module}}
	if len(signatureRaw) > 0 {
		members = append(members, struct {
			name string
			body []byte
		}{pluginapi.SignatureFile, signatureRaw})
	}
	if icon != nil {
		members = append(members, struct {
			name string
			body []byte
		}{IconFile, icon})
	}
	for _, m := range members {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: m.name, Method: zip.Deflate})
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(m.body); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	if _, err := UnpackPackage(buf.Bytes()); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// checkPackageMember refuses one archive entry that has no place in a package,
// judged by name and attributes only — nothing is decompressed here.
func checkPackageMember(f *zip.File, seen map[string]*zip.File) error {
	name := f.Name
	switch {
	case f.Mode()&fs.ModeSymlink != 0:
		return refuse(ReasonPackage,
			"the plugin package holds %q, which is a symbolic link; a package holds only regular files", name)
	case strings.HasPrefix(name, "/") || strings.Contains(name, `\`) ||
		name == ".." || strings.HasPrefix(name, "../") || strings.Contains(name, "/../") || strings.HasSuffix(name, "/.."):
		return refuse(ReasonPackage,
			"the plugin package holds %q, which points outside the package; every file must sit at the package root", name)
	case strings.HasPrefix(name, "__MACOSX/") || name == "__MACOSX":
		return refuse(ReasonPackage,
			"the plugin package holds %q, a macOS resource-fork folder; repackage it with `pluginsign pack`, or zip with `zip -X` from inside the plugin folder", name)
	case strings.HasSuffix(name, "/") || f.Mode().IsDir():
		return refuse(ReasonPackage,
			"the plugin package holds a folder, %q; the files must sit at the package root, not inside a folder", name)
	case strings.Contains(name, "/"):
		return refuse(ReasonPackage,
			"the plugin package holds %q inside a folder; the files must sit at the package root, not inside a folder", name)
	}
	if _, dup := seen[name]; dup {
		return refuse(ReasonPackage, "the plugin package holds %q twice", name)
	}
	if f.Flags&0x1 != 0 {
		return refuse(ReasonPackage, "the plugin package member %q is encrypted; a package is not password protected", name)
	}
	if f.Method != zip.Store && f.Method != zip.Deflate {
		return refuse(ReasonPackage,
			"the plugin package member %q uses compression method %d; only stored and deflate are accepted", name, f.Method)
	}
	return nil
}

// readMember reads one member through a cap on the bytes it DECOMPRESSES to. The
// size in the archive's header is an author's claim and is never consulted.
func readMember(f *zip.File, limit int64, what string) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, refuse(ReasonPackage, "the plugin package's %s (%s) could not be opened: %v", what, f.Name, err)
	}
	defer rc.Close()
	body, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, refuse(ReasonPackage, "the plugin package's %s (%s) could not be read: %v", what, f.Name, err)
	}
	if int64(len(body)) > limit {
		return nil, refuse(ReasonPackage,
			"the plugin package's %s (%s) is larger than the %d bytes this server accepts once unpacked", what, f.Name, limit)
	}
	return body, nil
}

// memberList names what a package held, for the sentences that say what was
// expected against what was found.
func memberList(byName map[string]*zip.File) string {
	if len(byName) == 0 {
		return "nothing"
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, fmt.Sprintf("%q", n))
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
