package telemetry

import (
	"errors"
	"fmt"

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

// FilterTenants keeps only the ListTenants rows the principal may read, so a
// key's dashboard picker never shows another tenant's name. A key bound to
// one tenant always sees that tenant, even before it has sent any traces.
func FilterTenants(p *keys.Principal, rows []map[string]any) []map[string]any {
	if p == nil || p.AllTenants() {
		return rows
	}
	out := make([]map[string]any, 0, len(p.Tenants))
	seen := map[string]bool{}
	for _, r := range rows {
		t, _ := r["tenant"].(string)
		if p.AllowsTenant(t) {
			out = append(out, r)
			seen[t] = true
		}
	}
	for _, t := range p.Tenants {
		if !seen[t] {
			out = append(out, map[string]any{"tenant": t})
		}
	}
	return out
}
