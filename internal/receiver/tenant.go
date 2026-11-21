package receiver

import (
	"fmt"
	"log"
	"net/http"
	"slices"

	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	resourcev1 "go.opentelemetry.io/proto/otlp/resource/v1"

	"github.com/davidgeorgehope/ducktel/internal/httpauth"
)

// TenantAttr is the resource attribute that carries the tenant id, the same
// one the query side hard-filters on.
const TenantAttr = "tenant.id"

// stampTenant sets tenant.id on a resource from the request's key when the key
// is bound to exactly one tenant. The sender's own value, if any, is replaced:
// the key decides the tenant, so a sender can neither forget it nor write into
// someone else's. Shared tokens and keys for all tenants leave the resource as
// sent (the pre-keys behaviour). A key bound to several tenants also leaves it
// as sent, but only when its tenant.id is one of the key's tenants: otherwise
// it returns an error and the caller rejects the whole request, so such a key
// can write into exactly its own tenants and no others. It returns the
// resource to use, creating one when the request had none.
func (r *Receiver) stampTenant(req *http.Request, res *resourcev1.Resource) (*resourcev1.Resource, error) {
	p := httpauth.PrincipalFrom(req.Context())
	tenant, ok := p.SingleTenant()
	if !ok {
		if p.AllTenants() {
			return res, nil
		}
		sent := tenantOfResource(res)
		if !slices.Contains(p.Tenants, sent) {
			return nil, fmt.Errorf("key %q may write tenants %v; resource has %s=%q", p.Name, p.Tenants, TenantAttr, sent)
		}
		return res, nil
	}
	if res == nil {
		res = &resourcev1.Resource{}
	}
	for _, kv := range res.Attributes {
		if kv.GetKey() != TenantAttr {
			continue
		}
		if sent := kv.GetValue().GetStringValue(); sent != tenant {
			r.reportOverwrite(p.Name, sent)
		}
		kv.Value = stringValue(tenant)
		return res, nil
	}
	res.Attributes = append(res.Attributes, &commonv1.KeyValue{Key: TenantAttr, Value: stringValue(tenant)})
	return res, nil
}

// tenantOfResource returns the resource's tenant.id, or "" when it has none.
func tenantOfResource(res *resourcev1.Resource) string {
	for _, kv := range res.GetAttributes() {
		if kv.GetKey() == TenantAttr {
			return kv.GetValue().GetStringValue()
		}
	}
	return ""
}

// reportOverwrite logs once per (key, sent value) that a sender's own
// tenant.id was replaced, so a misconfigured sender is visible without
// flooding the log on every batch.
func (r *Receiver) reportOverwrite(key, sent string) {
	if _, seen := r.overwrites.LoadOrStore(key+"\x00"+sent, true); seen {
		return
	}
	log.Printf("ingest: key=%s sent %s=%q; replaced with the key's tenant", key, TenantAttr, sent)
}

func stringValue(s string) *commonv1.AnyValue {
	return &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: s}}
}
