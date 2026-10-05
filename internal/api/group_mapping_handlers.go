package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/goozakdev/obelo-server/internal/auth"
	"github.com/goozakdev/obelo-server/internal/signin"
	"github.com/goozakdev/obelo-server/internal/store"
)

// Group mapping (ADR-0063 decisions 4 and 5), Admin scope. Per Sign-in
// provider:
//
//	GET  /settings/sign-in-providers/{id}/group-mapping
//	     → { "rules": [ { "group", "role", "libraryIds" } ], "recheck",
//	         "intervalHours", "defaultInterval", "failing"? }
//	PUT  /settings/sign-in-providers/{id}/group-mapping
//	     { "rules": [...], "intervalHours": n } → the same (0 or absent: the
//	     24-hour default)
//	POST /settings/sign-in-providers/{id}/resync
//	     → { "checked", "remapped", "failed", "revoked" }
//
// recheck is whether the provider can be asked between sign-ins (it declared
// lookup or refresh); failing is present from its first failed re-check until it
// next answers. A mapping governs only Users without a Local password.

// GroupMappingStore persists the Admin's mappings. *store.DB satisfies it.
type GroupMappingStore interface {
	GroupMapping(pluginID string) ([]store.GroupMappingRule, error)
	// SetGroupMappingAndInterval writes the rules and the re-check interval in
	// one transaction, so a PUT is all-or-nothing.
	SetGroupMappingAndInterval(pluginID string, rules []store.GroupMappingRule, interval time.Duration) error
}

// maxRecheckIntervalHours bounds the Admin's interval: a month.
const maxRecheckIntervalHours = 24 * 31

type groupMappingRuleJSON struct {
	Group      string   `json:"group"`
	Role       string   `json:"role"`
	LibraryIDs []string `json:"libraryIds"`
}

type groupMappingJSON struct {
	Rules           []groupMappingRuleJSON  `json:"rules"`
	Recheck         bool                    `json:"recheck"`
	IntervalHours   int                     `json:"intervalHours"`
	DefaultInterval bool                    `json:"defaultInterval"`
	Failing         *signin.ProviderFailure `json:"failing,omitempty"`
}

type groupMappingRequest struct {
	Rules         []groupMappingRuleJSON `json:"rules"`
	IntervalHours int                    `json:"intervalHours"`
}

func handleSignInProviderSubtree(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.SignInRecheck == nil || deps.GroupMappings == nil {
			writeError(w, http.StatusServiceUnavailable, codeServiceUnavailable,
				"sign-in providers are not available on this server", nil)
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, "/settings/sign-in-providers/")
		id, what, ok := strings.Cut(rest, "/")
		if !ok || id == "" || strings.Contains(what, "/") || !deps.SignInRecheck.Known(id) {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		switch what {
		case "group-mapping":
			handleGroupMapping(deps, id)(w, r)
		case "resync":
			requireMethod(http.MethodPost, handleResyncNow(deps, id))(w, r)
		default:
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
		}
	}
}

func handleGroupMapping(deps Deps, id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
		case http.MethodPut:
			var req groupMappingRequest
			if !decodeJSON(w, r, &req) {
				return
			}
			rules, msg := validGroupMapping(req)
			if msg != "" {
				writeError(w, http.StatusBadRequest, codeBadRequest, msg, nil)
				return
			}
			if err := deps.GroupMappings.SetGroupMappingAndInterval(id, rules, time.Duration(req.IntervalHours)*time.Hour); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					writeError(w, http.StatusBadRequest, codeBadRequest, "a rule grants a library that does not exist", nil)
					return
				}
				writeError(w, http.StatusInternalServerError, codeInternal, "the group mapping could not be saved", nil)
				return
			}
		default:
			w.Header().Set("Allow", "GET, PUT")
			writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "method not allowed", nil)
			return
		}
		view, err := groupMappingView(deps, id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "the group mapping could not be read", nil)
			return
		}
		writeJSON(w, http.StatusOK, view)
	}
}

// validGroupMapping checks an Admin's mapping, answering the rules to store or
// the sentence saying what is wrong.
func validGroupMapping(req groupMappingRequest) ([]store.GroupMappingRule, string) {
	if req.IntervalHours < 0 || req.IntervalHours > maxRecheckIntervalHours {
		return nil, "intervalHours must be between 1 and 744, or 0 for the 24-hour default"
	}
	seen := map[string]bool{}
	rules := make([]store.GroupMappingRule, 0, len(req.Rules))
	for _, r := range req.Rules {
		group := strings.TrimSpace(r.Group)
		if group == "" {
			return nil, "every rule names a group"
		}
		if seen[group] {
			return nil, "a group is mapped once"
		}
		seen[group] = true
		if r.Role != auth.RoleAdmin && r.Role != auth.RoleMember {
			return nil, "a rule's role is admin or member"
		}
		libs := r.LibraryIDs
		if libs == nil {
			libs = []string{}
		}
		rules = append(rules, store.GroupMappingRule{Group: group, Role: r.Role, LibraryIDs: libs})
	}
	return rules, ""
}

func groupMappingView(deps Deps, id string) (groupMappingJSON, error) {
	rules, err := deps.GroupMappings.GroupMapping(id)
	if err != nil {
		return groupMappingJSON{}, err
	}
	interval, err := deps.SignInRecheck.Interval(id)
	if err != nil {
		return groupMappingJSON{}, err
	}
	view := groupMappingJSON{
		Rules:           make([]groupMappingRuleJSON, 0, len(rules)),
		Recheck:         deps.SignInRecheck.CanRecheck(id),
		IntervalHours:   int(interval / time.Hour),
		DefaultInterval: interval == signin.DefaultRecheckInterval,
	}
	for _, r := range rules {
		view.Rules = append(view.Rules, groupMappingRuleJSON{Group: r.Group, Role: r.Role, LibraryIDs: r.LibraryIDs})
	}
	if f, ok := deps.SignInRecheck.Failures()[id]; ok {
		view.Failing = &f
	}
	return view, nil
}

func handleResyncNow(deps Deps, id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		res, err := deps.SignInRecheck.ResyncNow(r.Context(), id)
		if errors.Is(err, signin.ErrUnknownSignInProvider) {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "the re-sync did not finish", nil)
			return
		}
		writeJSON(w, http.StatusOK, res)
	}
}
