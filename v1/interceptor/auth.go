package interceptor

import (
	"context"
	"errors"

	"connectrpc.com/connect"
)

// Authenticator turns a request's headers into a principal.
//
// The framework deliberately has no opinion about what a principal is or
// where it comes from. JWTs, mTLS identities, session tokens and internal
// service accounts are all legitimate, and a framework that picked one
// would be a framework half the teams could not use. What it does have an
// opinion about is *where in the chain* this runs: below load shedding, so
// a flood does not get to make the service do expensive key fetches.
type Authenticator interface {
	// Authenticate returns a principal, or an error. Returning a
	// connect.Error with an explicit code lets the implementation
	// distinguish Unauthenticated (who are you?) from PermissionDenied
	// (I know who you are, and no).
	Authenticate(ctx context.Context, header Header) (any, error)
}

// Header is the subset of the request an Authenticator may read. It is an
// interface rather than the request itself so that an Authenticator cannot
// consume or mutate the message.
type Header interface {
	Get(key string) string
	Values(key string) []string
}

// AuthenticatorFunc adapts a function to the Authenticator interface.
type AuthenticatorFunc func(ctx context.Context, header Header) (any, error)

// Authenticate calls f.
func (f AuthenticatorFunc) Authenticate(ctx context.Context, h Header) (any, error) {
	return f(ctx, h)
}

type principalKey struct{}

// PrincipalFrom returns the authenticated principal, if any.
func PrincipalFrom(ctx context.Context) (any, bool) {
	p := ctx.Value(principalKey{})
	return p, p != nil
}

// Auth authenticates the request and puts the principal in the context.
//
// Procedures in `public` skip authentication entirely. The list is matched
// exactly against the Connect procedure, not by prefix: a prefix match on
// "/pkg.Service/Get" would also exempt "/pkg.Service/GetAdminSecrets", and
// an authentication bypass that comes from a string prefix is the kind of
// bug that gets a CVE.
func Auth(a Authenticator, public map[string]bool) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if a == nil {
				return next(ctx, req)
			}
			if public[req.Spec().Procedure] {
				return next(ctx, req)
			}

			principal, err := a.Authenticate(ctx, headerAdapter{req.Header()})
			if err != nil {
				// An authenticator that returns a bare error gets
				// Unauthenticated rather than the Unknown that would
				// otherwise leak out — and Unknown would be classified as a
				// server fault, so a wave of bad tokens would trip the
				// caller's breaker against a service that is working fine.
				if connect.CodeOf(err) == connect.CodeUnknown {
					return nil, connect.NewError(connect.CodeUnauthenticated, err)
				}
				return nil, err
			}
			if principal == nil {
				return nil, connect.NewError(connect.CodeUnauthenticated,
					errors.New("unauthenticated"))
			}

			return next(context.WithValue(ctx, principalKey{}, principal), req)
		}
	}
}

type headerAdapter struct {
	h interface {
		Get(string) string
		Values(string) []string
	}
}

func (a headerAdapter) Get(k string) string      { return a.h.Get(k) }
func (a headerAdapter) Values(k string) []string { return a.h.Values(k) }
