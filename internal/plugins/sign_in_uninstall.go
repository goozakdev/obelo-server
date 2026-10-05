package plugins

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/goozakdev/obelo-server/internal/store"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// Uninstalling a Sign-in provider (ADR-0063 decision 10) is the one uninstall
// that deletes Users: every External identity the provider issued goes, and so
// does every User it leaves with no sign-in path at all. Nobody is deleted
// without an Admin having seen them listed by name first. The preview says who;
// the confirm carries those ids back, and the store refuses — changing nothing —
// unless they are exactly who the uninstall would delete by then.

// signInUninstallStore is the part of the store an uninstall of a Sign-in
// provider needs. A store without it uninstalls every Plugin the ordinary way.
type signInUninstallStore interface {
	SignInCasualties(pluginID string, otherProviders []string) ([]store.SignInCasualty, error)
	DeleteSignInPlugin(pluginID string, otherProviders, confirmed []string) error
}

// UninstallPreview is what uninstalling a Plugin would do to Users.
type UninstallPreview struct {
	// SignInProvider is true when the Plugin is a Sign-in provider, whose
	// uninstall deletes its External identities.
	SignInProvider bool `json:"signInProvider"`
	// UsersToDelete is every User the uninstall would delete, by name.
	UsersToDelete []store.SignInCasualty `json:"usersToDelete"`
}

// UninstallPreview answers who uninstalling id would delete. It changes
// nothing.
func (m *Manager) UninstallPreview(ctx context.Context, id string) (UninstallPreview, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.mustBeInstalled(id); err != nil {
		return UninstallPreview{}, err
	}
	out := UninstallPreview{UsersToDelete: []store.SignInCasualty{}}
	others, signIn, err := m.signInProviders(ctx, id)
	if err != nil {
		return UninstallPreview{}, err
	}
	st, ok := m.store.(signInUninstallStore)
	if !signIn || !ok {
		return out, nil
	}
	users, err := st.SignInCasualties(id, others)
	if err != nil {
		return UninstallPreview{}, err
	}
	out.SignInProvider, out.UsersToDelete = true, users
	return out, nil
}

// UninstallConfirming uninstalls id. For a Sign-in provider, confirmed is the
// ids of the Users the Admin was shown the uninstall would delete; unless they
// are exactly who it would delete, the answer is
// *store.UnconfirmedUninstallError and nothing changes. Any other Plugin
// ignores confirmed and is uninstalled as it always was.
func (m *Manager) UninstallConfirming(ctx context.Context, id string, confirmed []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.mustBeInstalled(id); err != nil {
		return err
	}
	others, signIn, err := m.signInProviders(ctx, id)
	if err != nil {
		return err
	}
	st, ok := m.store.(signInUninstallStore)
	if !signIn || !ok {
		return m.uninstall(ctx, id)
	}
	return m.uninstallSignIn(ctx, id, others, confirmed, st)
}

// uninstallSignIn is uninstall for a Sign-in provider. The files go aside and
// the registry is rebuilt without it, as for any Plugin; then one transaction
// deletes its Users, its identities and its rows. If the rebuild or that
// transaction fails — the confirmation no longer matching included — the files
// come back and the registry is rebuilt with it again, so the Plugin is exactly
// as installed as it was. Caller holds mu.
func (m *Manager) uninstallSignIn(ctx context.Context, id string, others, confirmed []string, st signInUninstallStore) error {
	// Checked before anything moves, so a stale confirmation is refused without
	// the provider leaving the registry even for a moment. The transaction checks
	// again, because the list can change between here and there.
	users, err := st.SignInCasualties(id, others)
	if err != nil {
		return err
	}
	if !store.CasualtiesConfirmed(users, confirmed) {
		return &store.UnconfirmedUninstallError{Users: users}
	}
	dir := m.pluginDir(id)
	trash := ""
	if _, err := os.Stat(dir); err == nil {
		trash = filepath.Join(m.dir, fmt.Sprintf(".uninstall-%s-%d", id, m.staging.Add(1)))
		if err := os.Rename(dir, trash); err != nil {
			return fmt.Errorf("plugins: removing %s: %w", id, err)
		}
	}
	// Puts the Plugin back exactly as installed, after the uninstall failed.
	putBack := func() {
		if trash != "" {
			if rerr := os.Rename(trash, dir); rerr != nil {
				m.logf("obelo: plugin %s could not be put back after a failed uninstall: %v", id, rerr)
			}
		}
		if rerr := m.rebuild(ctx); rerr != nil {
			m.logf("obelo: plugin %s: the plugins were not re-read after a failed uninstall: %v", id, rerr)
		}
	}
	// A registry that may still hold the provider is no uninstall to delete Users
	// for: the rebuild failing aborts it before the transaction, deleting nothing.
	if err := m.rebuild(ctx); err != nil {
		putBack()
		return err
	}
	bundledRow := m.rowOrigin(id) == OriginBundled
	if err := st.DeleteSignInPlugin(id, others, confirmed); err != nil {
		putBack()
		return err
	}
	if bundledRow {
		m.declineIfUninstalled(id)
	}
	if trash != "" {
		if err := os.RemoveAll(trash); err != nil {
			m.logf("obelo: plugin %s was uninstalled but its files could not be deleted: %v", id, err)
		}
	}
	m.logf("obelo: plugin %s was uninstalled; %d users with no other sign-in path were deleted", id, len(users))
	return nil
}

// signInProviders reports whether id is a Sign-in provider, and the ids of
// every OTHER installed one — enabled or not, and whether or not the host has
// stopped calling it: an identity at any of them is still a way in.
func (m *Manager) signInProviders(ctx context.Context, id string) (others []string, signIn bool, err error) {
	list, err := m.list(ctx, "", false)
	if err != nil {
		return nil, false, err
	}
	for _, item := range list {
		if item.State == StateDeclined || !providesSignIn(item.Provides) {
			continue
		}
		if item.ID == id {
			signIn = true
		} else {
			others = append(others, item.ID)
		}
	}
	return others, signIn, nil
}

func providesSignIn(provides []string) bool {
	for _, p := range provides {
		if p == string(pluginapi.ExtensionSignInProvider) {
			return true
		}
	}
	return false
}
