package telemetry

import (
	"errors"
	"fmt"
	"slices"

	"github.com/davidgeorgehope/ducktel/internal/keys"
)

// ErrForbidden marks a request for a tenant the caller's key is not bound to.
var ErrForbidden = errors.New("forbidden")

type forbiddenError struct{ tenant, key string }

func (e *forbiddenError) Error() string {
	return fmt.Sprintf("tenant %q is not allowed for key %q", e.tenant, e.key)
}
func (e *forbiddenError) Unwrap() error { return ErrForbidden }

// IsForbidden reports whether err is a tenant the caller may not read.
func IsForbidden(err error) bool { return errors.Is(err, ErrForbidden) }

// ResolveTenant applies a principal's tenant binding to the tenant a request
// asked for. This is the one place the read side enforces keys, shared by the
// MCP tools and the REST API:
//
//   - shared (legacy) token: the request is passed through unchanged, so the
//     usual "tenant is required" check still applies;
//   - key bound to one tenant: an empty request defaults to it;
//   - any other tenant than the key allows: ErrForbidden.
func ResolveTenant(p *keys.Principal, requested string) (string, error) {
	if p == nil || p.Legacy {
		return requested, nil
	}
	if requested == "" {
		if t, ok := p.SingleTenant(); ok {
			return t, nil
		}
		return "", nil // validateTenant reports it
	}
	if !p.AllowsTenant(requested) {
		return "", &forbiddenError{tenant: requested, key: p.Name}
	}
	return requested, nil
}

// KeyTenants returns the tenant list for a key bound to specific tenants,
// straight from the key: there is nothing to discover, so no data is scanned,
// and a tenant that has not sent anything yet is listed all the same. ok is
// false for a shared token or a "*" key, which still need ListTenants to see
// tenants that no key names (senders on the shared token, older data).
func KeyTenants(p *keys.Principal) (rows []map[string]any, ok bool) {
	if p == nil || p.AllTenants() {
		return nil, false
	}
	rows = make([]map[string]any, 0, len(p.Tenants))
	for _, t := range slices.Sorted(slices.Values(p.Tenants)) {
		rows = append(rows, map[string]any{"tenant": t})
	}
	return rows, true
}
