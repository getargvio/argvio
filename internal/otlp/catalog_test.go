package otlp

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/getargvio/argvio/internal/storage"
)

func TestCatalogCacheReloadsAfterTTL(t *testing.T) {
	loads := 0
	c := newCatalogCache(time.Minute, func(context.Context, uuid.UUID) (map[storage.CatalogKey]storage.CatalogRule, error) {
		loads++
		return map[storage.CatalogKey]storage.CatalogRule{}, nil
	})
	tenant, other := uuid.New(), uuid.New()
	now := time.Now()
	for _, at := range []time.Time{now, now.Add(30 * time.Second)} {
		if _, err := c.rules(context.Background(), tenant, at); err != nil {
			t.Fatal(err)
		}
	}
	if loads != 1 {
		t.Errorf("loads within the TTL = %d, want 1", loads)
	}
	_, _ = c.rules(context.Background(), other, now)
	_, _ = c.rules(context.Background(), tenant, now.Add(2*time.Minute))
	if loads != 3 {
		t.Errorf("loads = %d, want one per tenant plus one after expiry", loads)
	}
}
