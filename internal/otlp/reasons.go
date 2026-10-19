package otlp

import (
	"context"

	"github.com/getargvio/argvio/internal/storage"
)

// attrErrorReason is the vendor-assigned error reason (allowlist
// cli.error.reason). Its format is enforced by the schema's pattern; its
// cardinality per tenant is enforced here.
const attrErrorReason = "cli.error.reason"

// defaultMaxErrorReasons stands in for an unset Bounds.MaxErrorReasonsPerTenant
// (config validation requires it, but a zero Bounds must not fold everything).
const defaultMaxErrorReasons = 100

// capErrorReasons rewrites the cli.error.reason of every record in attrs that
// its tenant's vocabulary has no room for to storage.ErrorReasonOther, before
// the records are stored. The attribute maps are modified in place. Unlike
// install tracking this runs before the write, so an error is returned: the
// export fails before anything is stored and the client retries it.
func (p *Pipeline) capErrorReasons(ctx context.Context, attrs []storage.Attrs) error {
	counts := map[string]int64{}
	for _, a := range attrs {
		if r, ok := a[attrErrorReason].(string); ok {
			counts[r]++
		}
	}
	if len(counts) == 0 {
		return nil
	}
	limit := p.Bounds.MaxErrorReasonsPerTenant
	if limit <= 0 {
		limit = defaultMaxErrorReasons
	}
	admitted, folded, err := p.Writer.AdmitErrorReasons(ctx, p.tenantUUID(), counts, limit, p.now())
	if err != nil {
		return err
	}
	if folded > 0 {
		p.Log.Warn("otlp: error reasons stored as \"other\": tenant reached max_error_reasons_per_tenant",
			"tenant_id", p.tenantUUID(), "folded_records", folded, "max_error_reasons_per_tenant", limit)
	}
	for _, a := range attrs {
		if r, ok := a[attrErrorReason].(string); ok && !admitted[r] {
			a[attrErrorReason] = storage.ErrorReasonOther
		}
	}
	return nil
}
