package signin

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/goozakdev/obelo-server/internal/auth"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// TestJudgeKeepsOnlyAnAnswerTheServerCanActOn is the host's judgment on one
// password-flow answer, case by case.
func TestJudgeKeepsOnlyAnAnswerTheServerCanActOn(t *testing.T) {
	for _, tc := range []struct {
		name string
		resp pluginapi.SignInPasswordResponse
		want auth.ExternalAnswer
		ok   bool
	}{
		{"rejected", pluginapi.SignInPasswordResponse{}, auth.ExternalAnswer{}, false},
		{"accepted with no identity", pluginapi.SignInPasswordResponse{Accepted: true}, auth.ExternalAnswer{}, false},
		{"accepted with a blank subject", pluginapi.SignInPasswordResponse{Accepted: true,
			Identity: &pluginapi.SignInIdentity{Subject: "  ", Username: "ada"}}, auth.ExternalAnswer{}, false},
		{"an identity that was not accepted", pluginapi.SignInPasswordResponse{
			Identity: &pluginapi.SignInIdentity{Subject: "s", Username: "ada"}}, auth.ExternalAnswer{}, false},
		{"accepted, name defaults to the one typed, groups tidied", pluginapi.SignInPasswordResponse{Accepted: true,
			Identity: &pluginapi.SignInIdentity{Subject: " s-1 ", Groups: []string{"b", " a ", "", "b"}}},
			auth.ExternalAnswer{Subject: "s-1", Username: "typed", Groups: []string{"b", "a"}}, true},
		{"accepted with the provider's own name", pluginapi.SignInPasswordResponse{Accepted: true,
			Identity: &pluginapi.SignInIdentity{Subject: "s-1", Username: "ada.l"}},
			auth.ExternalAnswer{Subject: "s-1", Username: "ada.l"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Judge(tc.resp, "typed")
			if ok != tc.ok || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Judge = %+v, %v; want %+v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

type stubProvider struct {
	answer pluginapi.SignInPasswordResponse
}

func (s stubProvider) CheckPassword(context.Context, pluginapi.SignInPasswordRequest) (pluginapi.SignInPasswordResponse, error) {
	return s.answer, nil
}

type memOrder struct{ ids []string }

func (m *memOrder) SignInProviderOrder() ([]string, error) { return m.ids, nil }
func (m *memOrder) SetSignInProviderOrder(ids []string) error {
	m.ids = append([]string(nil), ids...)
	return nil
}

func register(reg *pluginapi.Registry, slug string, caps ...pluginapi.Capability) {
	reg.RegisterSignInProvider(pluginapi.SignInProviderRegistration{
		Descriptor: pluginapi.Descriptor{Slug: slug, Name: slug, Capabilities: caps},
		New: func(pluginapi.Settings) (pluginapi.SignInProvider, error) {
			return stubProvider{}, nil
		},
	})
}

// TestTheOrderIsTheAdminsThenRegistrationAndOnlyPasswordProvidersAreAsked: a
// provider that did not declare the password flow is never listed or handed a
// password; the Admin's order leads; an unplaced provider follows in
// registration order; an order naming a stranger or naming one twice is refused.
func TestTheOrderIsTheAdminsThenRegistrationAndOnlyPasswordProvidersAreAsked(t *testing.T) {
	reg := pluginapi.NewRegistry()
	register(reg, "a", pluginapi.CapabilityPasswordSignIn)
	register(reg, "redirect-only")
	register(reg, "b", pluginapi.CapabilityPasswordSignIn)
	register(reg, "c", pluginapi.CapabilityPasswordSignIn)
	order := &memOrder{}
	src := NewSource(reg, order)

	if err := src.SetOrder([]string{"c", "a"}); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, p := range src.PasswordProviders() {
		ids = append(ids, p.ID())
	}
	if want := []string{"c", "a", "b"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("asked in order %v, want %v", ids, want)
	}
	for _, bad := range [][]string{{"redirect-only"}, {"nope"}, {"a", "a"}} {
		if err := src.SetOrder(bad); !errors.Is(err, ErrInvalidOrder) {
			t.Fatalf("SetOrder(%v) err = %v, want ErrInvalidOrder", bad, err)
		}
	}
	if !reflect.DeepEqual(order.ids, []string{"c", "a"}) {
		t.Fatalf("a refused order was saved: %v", order.ids)
	}
}
