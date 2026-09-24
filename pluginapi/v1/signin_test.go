package v1

import "testing"

// signInWireCases pins the documents of the Sign-in provider Extension point's
// password flow: the request, an identity, both answers, and the envelope an
// Installed plugin is handed.
func signInWireCases() []wireCase {
	req := SignInPasswordRequest{Username: "alice", Password: "correct horse"}
	id := SignInIdentity{Subject: "u-1001", Username: "alice", Groups: []string{"family", "media"}}
	return []wireCase{
		{
			name:   "SignInPasswordRequest",
			value:  req,
			golden: `{"username":"alice","password":"correct horse"}`,
		},
		{
			name:   "SignInIdentity",
			value:  id,
			golden: `{"subject":"u-1001","username":"alice","groups":["family","media"]}`,
		},
		{
			name:  "SignInPasswordResponse",
			value: SignInPasswordResponse{Accepted: true, Identity: &id},
			golden: `{"accepted":true,"identity":{"subject":"u-1001","username":"alice",` +
				`"groups":["family","media"]}}`,
		},
		{
			name:   "SignInPasswordResponse rejected",
			value:  SignInPasswordResponse{},
			golden: `{"accepted":false}`,
		},
		{
			name:  "SignInPasswordCall",
			value: SignInPasswordCall{Request: req, Settings: Settings{Enabled: true}},
			golden: `{"request":{"username":"alice","password":"correct horse"},` +
				`"settings":{"enabled":true}}`,
		},
	}
}

// TestRegistryHoldsSignInProviders: a Sign-in provider registers into the same
// Registry value as every other seam, reads back in registration order under its
// own slug namespace, and a malformed registration panics at the composition
// root.
func TestRegistryHoldsSignInProviders(t *testing.T) {
	stub := func(Settings) (SignInProvider, error) { return nil, nil }
	reg := NewRegistry()
	reg.RegisterSignInProvider(SignInProviderRegistration{
		Descriptor: Descriptor{Slug: "directory", Name: "Directory"}, New: stub,
	})
	reg.RegisterSignInProvider(SignInProviderRegistration{
		Descriptor: Descriptor{Slug: "second-directory"}, New: stub,
	})

	got := reg.SignInProviders()
	if len(got) != 2 || got[0].Descriptor.Slug != "directory" || got[1].Descriptor.Slug != "second-directory" {
		t.Fatalf("SignInProviders() = %+v, want directory then second-directory", got)
	}
	if ep := got[0].Descriptor.ExtensionPoint; ep != ExtensionSignInProvider {
		t.Fatalf("extension point = %q, want %q", ep, ExtensionSignInProvider)
	}
	got[0].Descriptor.Slug = "mutated"
	if _, ok := reg.SignInProvider("directory"); !ok {
		t.Fatal("SignInProviders() handed out the registry's own backing array")
	}
	if _, ok := reg.WebReferenceProvider("directory"); ok {
		t.Fatal("WebReferenceProvider(directory) found a sign-in provider")
	}
	var nilReg *Registry
	if nilReg.SignInProviders() != nil {
		t.Fatal("a nil registry listed sign-in providers")
	}

	for _, tc := range []struct {
		name string
		reg  SignInProviderRegistration
	}{
		{"duplicate slug", SignInProviderRegistration{Descriptor: Descriptor{Slug: "directory"}, New: stub}},
		{"no slug", SignInProviderRegistration{New: stub}},
		{"no factory", SignInProviderRegistration{Descriptor: Descriptor{Slug: "other"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("registration was accepted; want a panic at the composition root")
				}
			}()
			reg.RegisterSignInProvider(tc.reg)
		})
	}
}
