package otlp

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/getargvio/argvio/internal/storage"
)

// catalogCache keeps each tenant's metric catalog rules for a TTL, so ingest
// reads metric_definitions at most once per TTL per tenant and process.
type catalogCache struct {
	ttl  time.Duration
	load func(ctx context.Context, tenantID uuid.UUID) (map[storage.CatalogKey]storage.CatalogRule, error)

	mu      sync.Mutex
	entries map[uuid.UUID]catalogEntry
}

type catalogEntry struct {
	rules   map[storage.CatalogKey]storage.CatalogRule
	expires time.Time
}

func newCatalogCache(ttl time.Duration, load func(ctx context.Context, tenantID uuid.UUID) (map[storage.CatalogKey]storage.CatalogRule, error)) *catalogCache {
	if ttl <= 0 {
		ttl = time.Minute
	}
	return &catalogCache{ttl: ttl, load: load, entries: map[uuid.UUID]catalogEntry{}}
}

func (c *catalogCache) rules(ctx context.Context, tenantID uuid.UUID, now time.Time) (map[storage.CatalogKey]storage.CatalogRule, error) {
	c.mu.Lock()
	e, ok := c.entries[tenantID]
	c.mu.Unlock()
	if ok && now.Before(e.expires) {
		return e.rules, nil
	}
	rules, err := c.load(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.entries[tenantID] = catalogEntry{rules: rules, expires: now.Add(c.ttl)}
	c.mu.Unlock()
	return rules, nil
}

// vendorMetric describes a vendor metric that the catalog governs.
type vendorMetric struct {
	kind, nameAttr, labelAttr string
}

var vendorMetrics = map[string]vendorMetric{
	metricCounter: {"counter", attrCounterName, attrCounterLabel},
	metricMeasure: {"duration", attrMeasureName, attrMeasureLabel},
	metricValue:   {"value", attrValueName, attrValueLabel},
}

// attrServiceName is the OTel resource attribute naming the sending CLI.
const attrServiceName = "service.name"

// loadCatalog fetches the tenant's catalog rules for this request, when the
// pipeline has a catalog cache.
func (p *Pipeline) loadCatalog(ctx context.Context) error {
	if p.Catalogs == nil || p.catalog != nil {
		return nil
	}
	rules, err := p.Catalogs.rules(ctx, p.tenantUUID(), p.now())
	if err != nil {
		return fmt.Errorf("otlp: load metric catalog: %w", err)
	}
	p.catalog = rules
	return nil
}

// checkCatalog applies the sending CLI's catalog to one vendor data point,
// after allowlist evaluation and before its dimensions are folded. kept is
// rewritten in place. It returns why the point must be rejected, or "":
//   - a declared metric sent as another kind, or below its min_tier, is
//     rejected;
//   - dimensions (and a plain label, the dimension "label") the definition
//     doesn't declare are dropped and logged by key;
//   - with strict_metrics, an undeclared metric is rejected; otherwise it is
//     accepted unchecked.
func (p *Pipeline) checkCatalog(metric string, kept map[string]any, tier string) string {
	vm, ok := vendorMetrics[metric]
	if !ok {
		return ""
	}
	name, _ := kept[vm.nameAttr].(string)
	service, _ := kept[attrServiceName].(string)
	rule, declared := p.catalog[storage.CatalogKey{Service: service, Name: name}]
	if !declared {
		if p.Tenant.Config.StrictMetrics {
			return fmt.Sprintf("%s %q is not declared in the catalog of %q (strict_metrics)", metric, name, service)
		}
		return ""
	}
	if rule.Kind != vm.kind {
		return fmt.Sprintf("%q is declared as a %s, sent as %s", name, rule.Kind, metric)
	}
	if rule.MinTier != "" {
		want, okWant := p.Schema.TierRank(rule.MinTier)
		got, okGot := p.Schema.TierRank(tier)
		if okWant && okGot && got < want {
			return fmt.Sprintf("%q is declared for tier %s and above, sent at %s", name, rule.MinTier, tier)
		}
	}
	allowed := make(map[string]bool, len(rule.Dims))
	for _, d := range rule.Dims {
		allowed[d] = true
	}
	var dropped []string
	for k := range kept {
		if key, ok := strings.CutPrefix(k, attrDimPrefix); ok && !allowed[key] {
			delete(kept, k)
			dropped = append(dropped, k)
		}
	}
	if _, ok := kept[vm.labelAttr]; ok && !allowed[storage.DimLabelKey] {
		delete(kept, vm.labelAttr)
		dropped = append(dropped, vm.labelAttr)
	}
	if len(dropped) > 0 {
		p.Log.Info("otlp: dimensions not declared in the metric catalog dropped",
			"tenant_id", p.tenantUUID(), "service", service, "metric", name, "dropped_keys", dropped)
	}
	return ""
}
