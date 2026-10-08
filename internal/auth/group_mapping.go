package auth

import (
	"errors"
	"sort"

	"github.com/goozakdev/obelo-server/internal/store"
)

// Group mapping (ADR-0063 decisions 4 and 5): an Admin's rule from a Sign-in
// provider's groups to a role and library grants, re-applied — synced — every
// time a provider vouches for an External identity: at each sign-in, and at each
// periodic re-check (internal/signin). This file is the one place the rule is
// applied, so a sign-in and a re-check cannot disagree about what it means.
//
// Two rules, and they are the whole of it:
//
//   - A User who holds a Local password is never governed by a mapping. They
//     sign in with the network and every provider down, and nothing a directory
//     says about their groups changes what they may do here.
//   - A provider with no mapping maps nothing: its Users keep whatever an Admin
//     gave them by hand. With a mapping, a User in none of its groups is a
//     Member granted nothing.
//
// A User holding identities from several mapped providers is governed by all of
// them at once: an Admin if any says so, and every Library any grants.

// mappingGoverns is whether a Group mapping may touch u at all.
func mappingGoverns(u store.User) bool {
	return u.PasswordHash == "" && u.Role != RoleRemote
}

// SyncGroupMapping re-applies the Group mappings to userID from the groups last
// recorded on each External identity they hold. It changes nothing for a User
// with a Local password, or one whose providers have no mapping.
func (s *Service) SyncGroupMapping(userID string) error {
	_, err := s.SyncGroupMappingChanged(userID)
	return err
}

// SyncGroupMappingChanged is SyncGroupMapping, answering whether the User's
// role or granted Libraries moved.
func (s *Service) SyncGroupMappingChanged(userID string) (bool, error) {
	user, err := s.store.UserByID(userID)
	if errors.Is(err, store.ErrNotFound) {
		return false, ErrUserNotFound
	}
	if err != nil {
		return false, err
	}
	if !mappingGoverns(user) {
		return false, nil
	}
	identities, err := s.store.ExternalIdentitiesByUser(userID)
	if err != nil {
		return false, err
	}
	governed, admin := false, false
	granted := map[string]bool{}
	for _, x := range identities {
		rules, err := s.store.GroupMapping(x.PluginID)
		if err != nil {
			return false, err
		}
		if len(rules) == 0 {
			continue
		}
		governed = true
		in := map[string]bool{}
		for _, g := range x.Groups {
			in[g] = true
		}
		for _, r := range rules {
			if !in[r.Group] {
				continue
			}
			if r.Role == RoleAdmin {
				admin = true
			}
			for _, lid := range r.LibraryIDs {
				granted[lid] = true
			}
		}
	}
	if !governed {
		return false, nil
	}
	role := RoleMember
	if admin {
		role = RoleAdmin
	}
	// The person has no Local password; CreateUser's own rule says whether the
	// role may be held without one, asked with the one fact only a mapping states.
	if !passwordRuleAdmits(role, "", true) {
		return false, ErrInvalidUser
	}
	libs := make([]string, 0, len(granted))
	for lid := range granted {
		libs = append(libs, lid)
	}
	sort.Strings(libs)
	changed, err := s.store.ApplyMappedAccess(userID, role, libs)
	if changed && err == nil && s.onAccessChange != nil {
		s.onAccessChange(userID)
	}
	return changed, err
}

// SetOnAccessChange installs the callback told the id of a User whose role or
// granted Libraries a Group mapping moved, so what is live for them can be
// re-judged (an Admin made a Member loses their Online source sessions).
func (s *Service) SetOnAccessChange(f func(userID string)) { s.onAccessChange = f }

// RevokeSessions ends every session userID holds, on every Device: what a
// provider saying the identity is gone or disabled costs (ADR-0063 decision 4),
// not merely the sessions opened through that identity.
func (s *Service) RevokeSessions(userID string) error {
	return s.store.DeleteSessionsForUser(userID)
}
