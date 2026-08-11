package api

import (
	"context"

	"github.com/marioquake/obelo-server/internal/access"
	"github.com/marioquake/obelo-server/internal/store"
)

// identity is what the bearer-auth middleware attaches to an authenticated
// request: the resolved User, Device, and the raw token (so handlers like
// logout can revoke exactly the token that authorized the call).
type identity struct {
	User   store.User
	Device store.Device
	Token  string
}

// ctxKey is an unexported type for context keys defined in this package, so
// keys never collide with those from other packages.
type ctxKey int

const (
	identityKey ctxKey = iota
	scopeKey
	originKey
)

// withIdentity returns a copy of ctx carrying the authenticated identity.
func withIdentity(ctx context.Context, id identity) context.Context {
	return context.WithValue(ctx, identityKey, id)
}

// identityFrom extracts the authenticated identity attached by the middleware.
// The bool is false for unauthenticated requests; authenticated handlers run
// behind the middleware, so they can rely on it being true.
func identityFrom(ctx context.Context) (identity, bool) {
	id, ok := ctx.Value(identityKey).(identity)
	return id, ok
}

// withScope returns a copy of ctx carrying the caller's resolved access Scope.
func withScope(ctx context.Context, s access.Scope) context.Context {
	return context.WithValue(ctx, scopeKey, s)
}

// scopeFrom extracts the access Scope attached by requireScope. The bool is
// false when no scope was resolved (a read handler reached without the
// requireScope wrapper) — such a handler fails closed rather than serving with
// the zero-value (deny-all) Scope.
func scopeFrom(ctx context.Context) (access.Scope, bool) {
	s, ok := ctx.Value(scopeKey).(access.Scope)
	return s, ok
}

// withRequestOrigin returns a copy of ctx carrying the resolved client address
// and scheme (forwarded.go), attached by the resolveRequestOrigin middleware
// that wraps the whole API.
func withRequestOrigin(ctx context.Context, o requestOrigin) context.Context {
	return context.WithValue(ctx, originKey, o)
}

// requestOriginFrom extracts the resolved origin. The bool is false only for a
// request that never met the middleware — a handler called directly by a narrow
// unit test — and the callers (clientIP, requestIsHTTPS) then re-derive it with
// an EMPTY allowlist, which is the same answer the middleware would give a server
// whose operator configured no trusted proxy. Missing context therefore means
// "trust nothing", never "trust anything".
func requestOriginFrom(ctx context.Context) (requestOrigin, bool) {
	o, ok := ctx.Value(originKey).(requestOrigin)
	return o, ok
}
