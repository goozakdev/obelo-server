// Package signin is the host half of the Sign-in provider Extension point
// (ADR-0063): which password-flow Sign-in providers a login asks, in what order,
// and what an answer must carry before the Server will act on it.
//
// The judgment is the HOST's. A Plugin says "accepted, subject S"; this package
// decides whether that is an answer at all — accepted, with a subject, from a
// provider that declared the password flow — and hands internal/auth only the
// answers that are. What an accepted answer resolves to (the User holding the
// External identity, or a new Member) is internal/auth's, beside the rest of
// sign-in.
//
// A provider that fails, or answers something incomplete, counts as a rejection
// and the next one is asked: a failing Plugin is never a reason to sign anyone
// in, and never a reason to lock anyone out of the providers after it.
package signin

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/goozakdev/obelo-server/internal/auth"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// OrderStore persists the Admin's order. *store.DB satisfies it.
type OrderStore interface {
	SignInProviderOrder() ([]string, error)
	SetSignInProviderOrder(ids []string) error
}

// Source is the ordered set of password-flow Sign-in providers this server has,
// read fresh from the registry on every login so an install, an uninstall or an
// enable switch is seen at once.
type Source struct {
	reg   *pluginapi.Registry
	order OrderStore
}

// NewSource returns a Source over reg, ordered by what order holds.
func NewSource(reg *pluginapi.Registry, order OrderStore) *Source {
	return &Source{reg: reg, order: order}
}

// Provider is one password-flow Sign-in provider as the Admin order screen lists
// it.
type Provider struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ErrInvalidOrder is what SetOrder answers for an order it will not save: one
// naming an id that is not a password-flow Sign-in provider registered on this
// server, or naming one twice.
var ErrInvalidOrder = errors.New("signin: invalid sign-in provider order")

// Providers lists every registered password-flow Sign-in provider in the order a
// login asks them: the Admin's order first, then any provider the Admin has not
// placed, in registration order.
func (s *Source) Providers() []Provider {
	regs := s.ordered()
	out := make([]Provider, 0, len(regs))
	for _, r := range regs {
		out = append(out, Provider{ID: r.Descriptor.Slug, Name: r.Descriptor.Name})
	}
	return out
}

// SetOrder saves the Admin's order. Every id must be a registered password-flow
// Sign-in provider, named once; a provider left out keeps no Admin-set place and
// is asked after the ones named.
func (s *Source) SetOrder(ids []string) error {
	known := map[string]bool{}
	for _, r := range s.passwordRegistrations() {
		known[r.Descriptor.Slug] = true
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if !known[id] {
			return fmt.Errorf("%w: %q is not a password sign-in provider on this server", ErrInvalidOrder, id)
		}
		if seen[id] {
			return fmt.Errorf("%w: %q is named twice", ErrInvalidOrder, id)
		}
		seen[id] = true
	}
	return s.order.SetSignInProviderOrder(ids)
}

// PasswordProviders satisfies auth.SignInProviders: the providers a login asks
// after the Local password, first-asked first. A provider whose factory refuses
// is skipped here, which is the same as it rejecting.
func (s *Source) PasswordProviders() []auth.PasswordProvider {
	var out []auth.PasswordProvider
	for _, r := range s.ordered() {
		p, err := r.New(pluginapi.Settings{Enabled: true})
		if err != nil {
			log.Printf("obelo: sign-in provider %s: %v", r.Descriptor.Slug, err)
			continue
		}
		out = append(out, &judged{id: r.Descriptor.Slug, p: p})
	}
	return out
}

var _ auth.SignInProviders = (*Source)(nil)

// passwordRegistrations is every registered Sign-in provider that declared the
// password flow, in registration order. One that did not is never handed a
// password.
func (s *Source) passwordRegistrations() []pluginapi.SignInProviderRegistration {
	var out []pluginapi.SignInProviderRegistration
	for _, r := range s.reg.SignInProviders() {
		if r.Descriptor.HasCapability(pluginapi.CapabilityPasswordSignIn) {
			out = append(out, r)
		}
	}
	return out
}

// ordered is passwordRegistrations in the Admin's order. An unreadable order is
// logged and read as "no order": registration order still signs people in.
func (s *Source) ordered() []pluginapi.SignInProviderRegistration {
	regs := s.passwordRegistrations()
	if s.order == nil {
		return regs
	}
	ids, err := s.order.SignInProviderOrder()
	if err != nil {
		log.Printf("obelo: the sign-in provider order could not be read, so registration order is used: %v", err)
		return regs
	}
	out := make([]pluginapi.SignInProviderRegistration, 0, len(regs))
	placed := map[string]bool{}
	for _, id := range ids {
		for _, r := range regs {
			if r.Descriptor.Slug == id && !placed[id] {
				out = append(out, r)
				placed[id] = true
			}
		}
	}
	for _, r := range regs {
		if !placed[r.Descriptor.Slug] {
			out = append(out, r)
		}
	}
	return out
}

// judged is one provider behind the host's judgment on its answers.
type judged struct {
	id string
	p  pluginapi.SignInProvider
}

func (j *judged) ID() string { return j.id }

// CheckPassword asks the provider and keeps only an answer the Server can act on.
func (j *judged) CheckPassword(ctx context.Context, username, password string) (auth.ExternalAnswer, bool) {
	resp, err := j.p.CheckPassword(ctx, pluginapi.SignInPasswordRequest{Username: username, Password: password})
	if err != nil {
		log.Printf("obelo: sign-in provider %s: %v", j.id, err)
		return auth.ExternalAnswer{}, false
	}
	return Judge(resp, username)
}

// Judge is the host's whole judgment on one password-flow answer. It is an
// answer the Server signs in on only when it is accepted AND names a subject.
// The username defaults to the one typed at the form when the provider reports
// none, and the groups are trimmed, de-duplicated and stripped of blanks — they
// are stored, and a Group mapping later reads them, so they are kept tidy here
// rather than there.
func Judge(resp pluginapi.SignInPasswordResponse, typed string) (auth.ExternalAnswer, bool) {
	if !resp.Accepted || resp.Identity == nil {
		return auth.ExternalAnswer{}, false
	}
	subject := strings.TrimSpace(resp.Identity.Subject)
	if subject == "" {
		return auth.ExternalAnswer{}, false
	}
	username := strings.TrimSpace(resp.Identity.Username)
	if username == "" {
		username = strings.TrimSpace(typed)
	}
	if username == "" {
		return auth.ExternalAnswer{}, false
	}
	var groups []string
	seen := map[string]bool{}
	for _, g := range resp.Identity.Groups {
		g = strings.TrimSpace(g)
		if g == "" || seen[g] {
			continue
		}
		seen[g] = true
		groups = append(groups, g)
	}
	return auth.ExternalAnswer{Subject: subject, Username: username, Groups: groups}, true
}
