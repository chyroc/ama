// Storing and reading the identity in the context.
//
// # Why context instead of keep rewriting request headers
//
// The earlier placeholder implementation **rewrote r.Header** so that the downstream
// feishu.ActorFrom read it from the header. That worked while there was only one placeholder
// identity, but with real authentication it has two problems:
//
//  1. **Request headers are something the caller controls**. The middleware writes and the
//     handler reads, with the whole chain in between: if any step fails to overwrite the
//     original value, a user can forge an X-AMA-Actor header and pose as someone else.
//     A context does not have this surface: only server code can write into it.
//  2. A header is a string; it cannot hold structured information such as "who this identity
//     is, where it came from, whether it is a machine".
//
// The comment in feishu/handler.go announced exactly this step: "once real authentication
// lands, this function reads from the context written by the auth middleware, and callers
// do not change."
//
// # Usage
//
//	// middleware:
//	ctx := auth.WithIdentity(r.Context(), identity)
//	next.ServeHTTP(w, r.WithContext(ctx))
//
//	// business layer:
//	email := auth.ActorFrom(ctx)          // only the Actor ID
//	identity, ok := auth.IdentityFrom(ctx) // the full identity

package auth

import "context"

// identityKey is the context key.
//
// Using an unexported custom type as the key is the standard practice for context: even if
// another package also uses the string "identity" as a key, they do not collide, and outside
// code cannot forge it.
type identityKey struct{}

// WithIdentity puts the identity into the context. **Only the auth middleware should call it.**
func WithIdentity(ctx context.Context, identity Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, identity)
}

// IdentityFrom returns the full identity. The second return value tells whether the request
// is authenticated.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	identity, ok := ctx.Value(identityKey{}).(Identity)
	if !ok || !identity.Valid() {
		return Identity{}, false
	}
	return identity, true
}

// ActorFrom returns the Actor ID (employee email). Returns an empty string when not authenticated.
//
// This is the entry point the business layer uses most: it only cares "who is acting", which
// is that email.
func ActorFrom(ctx context.Context) string {
	identity, ok := IdentityFrom(ctx)
	if !ok {
		return ""
	}
	return identity.Email
}
