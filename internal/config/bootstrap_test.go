package config

import (
	"encoding/base64"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// setBootstrap installs obfuscated (base64) bootstrap values for the duration of a
// test and restores the originals afterward, mimicking what an official build's
// `-ldflags -X` injection produces. Empty inputs inject empty vars (a from-source
// build). It manipulates the unexported package vars directly — the reason this
// test lives in `package config`, not `config_test`.
func setBootstrap(t *testing.T, tmdb, fanart, enc string) {
	t.Helper()
	origTMDB, origFanart, origEnc := bootstrapTMDBKey, bootstrapFanartKey, kAppEncKey
	t.Cleanup(func() {
		bootstrapTMDBKey, bootstrapFanartKey, kAppEncKey = origTMDB, origFanart, origEnc
	})
	obf := func(s string) string {
		if s == "" {
			return ""
		}
		return base64.StdEncoding.EncodeToString([]byte(s))
	}
	bootstrapTMDBKey, bootstrapFanartKey, kAppEncKey = obf(tmdb), obf(fanart), enc
}

// TestResolveTMDBKeyPrecedence exercises the default-credential chain (ADR-0032)
// at every operator×rotation×bootstrap combination: operator BYOK wins, else the
// cached rotation key, else the bundled bootstrap key, else none. The rotation
// layer sits strictly between operator and bootstrap (issue 03).
func TestResolveTMDBKeyPrecedence(t *testing.T) {
	cases := []struct {
		name       string
		operator   string
		rotation   string
		bootstrap  string
		wantKey    string
		wantSource CredentialSource
	}{
		{"operator over all", "op-key", "rot-key", "boot-key", "op-key", CredentialOperator},
		{"rotation over bootstrap", "", "rot-key", "boot-key", "rot-key", CredentialRotation},
		{"rotation only", "", "rot-key", "", "rot-key", CredentialRotation},
		{"bootstrap when no rotation", "", "", "boot-key", "boot-key", CredentialBootstrap},
		{"none", "", "", "", "", CredentialNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setBootstrap(t, tc.bootstrap, "", "")
			cfg := Config{TMDBAPIKey: tc.operator}
			key, src := cfg.ResolveTMDBKey(RotationKeys{TMDB: tc.rotation})
			if key != tc.wantKey || src != tc.wantSource {
				t.Fatalf("ResolveTMDBKey() = (%q, %v), want (%q, %v)", key, src, tc.wantKey, tc.wantSource)
			}
		})
	}
}

// TestResolveFanartTVKeyPrecedence mirrors the TMDB test for the fanart.tv key.
func TestResolveFanartTVKeyPrecedence(t *testing.T) {
	cases := []struct {
		name       string
		operator   string
		rotation   string
		bootstrap  string
		wantKey    string
		wantSource CredentialSource
	}{
		{"operator over all", "op-key", "rot-key", "boot-key", "op-key", CredentialOperator},
		{"rotation over bootstrap", "", "rot-key", "boot-key", "rot-key", CredentialRotation},
		{"rotation only", "", "rot-key", "", "rot-key", CredentialRotation},
		{"bootstrap when no rotation", "", "", "boot-key", "boot-key", CredentialBootstrap},
		{"none", "", "", "", "", CredentialNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setBootstrap(t, "", tc.bootstrap, "")
			cfg := Config{FanartTVAPIKey: tc.operator}
			key, src := cfg.ResolveFanartTVKey(RotationKeys{Fanart: tc.rotation})
			if key != tc.wantKey || src != tc.wantSource {
				t.Fatalf("ResolveFanartTVKey() = (%q, %v), want (%q, %v)", key, src, tc.wantKey, tc.wantSource)
			}
		})
	}
}

// TestDeobfuscate: a build injects base64, so BootstrapTMDBKey/BootstrapFanartKey
// round-trip it back to the plaintext key; an empty injection (from-source build)
// yields "", and a malformed (non-base64) injection degrades to "" rather than
// shipping a broken credential.
func TestDeobfuscate(t *testing.T) {
	if got := deobfuscate(""); got != "" {
		t.Errorf("deobfuscate(empty) = %q, want empty", got)
	}
	if got := deobfuscate(base64.StdEncoding.EncodeToString([]byte("abc123"))); got != "abc123" {
		t.Errorf("deobfuscate(base64 round-trip) = %q, want abc123", got)
	}
	if got := deobfuscate("!!! not base64 !!!"); got != "" {
		t.Errorf("deobfuscate(malformed) = %q, want empty (degrade to no key)", got)
	}
}

// TestAppEncKeyVerbatim: the AES key is already base64 and is returned as-is (the
// rotation client in issue 03 decodes it), unlike the double-obfuscated provider
// keys.
func TestAppEncKeyVerbatim(t *testing.T) {
	setBootstrap(t, "", "", "enc-key-base64")
	if got := AppEncKey(); got != "enc-key-base64" {
		t.Errorf("AppEncKey() = %q, want verbatim enc-key-base64", got)
	}
}

// TestBootstrapVarsEmptyInSource is the credential-free-repo gate (ADR-0032): it
// parses bootstrap.go and asserts the three injected vars are declared with NO
// initializer (or an empty string literal), so a plaintext key can never be
// committed to the open-source repo. `make check-credentials-free` is the fast
// grep equivalent; this AST check is the authoritative one run by `go test`.
func TestBootstrapVarsEmptyInSource(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "bootstrap.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing bootstrap.go: %v", err)
	}
	want := map[string]bool{"bootstrapTMDBKey": false, "bootstrapFanartKey": false, "kAppEncKey": false}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if _, tracked := want[name.Name]; !tracked {
					continue
				}
				want[name.Name] = true
				// No initializer at all is the expected form.
				if i >= len(vs.Values) {
					continue
				}
				// An initializer is only acceptable if it is the empty string literal.
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING || (lit.Value != `""` && lit.Value != "``") {
					t.Errorf("var %s has a non-empty initializer %s in source — credentials must never be committed (ADR-0032)", name.Name, exprText(fset, vs.Values[i]))
				}
			}
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("expected to find bundled-credential var %q declared in bootstrap.go", name)
		}
	}
}

// exprText renders an AST expression back to source for a readable failure.
func exprText(fset *token.FileSet, e ast.Expr) string {
	if lit, ok := e.(*ast.BasicLit); ok {
		return lit.Value
	}
	return fset.Position(e.Pos()).String()
}
