package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// DashboardCard is one card of a CLI's Product dashboard: a metric of its
// catalog shown as a card type, with free-form display options.
type DashboardCard struct {
	Metric   string          `json:"metric"`
	CardType string          `json:"card_type"`
	Options  json.RawMessage `json:"options,omitempty"`
}

// ReplaceDashboardCards makes cards, in order, the saved layout of one
// tenant's CLI. An empty list removes it, so the default layout applies.
func (w *Writer) ReplaceDashboardCards(ctx context.Context, tenantID uuid.UUID, service string, cards []DashboardCard, actor string, at time.Time) error {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("storage: replace dashboard cards: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM dashboard_cards WHERE tenant_id = $1 AND service_name = $2`, tenantID, service); err != nil {
		return fmt.Errorf("storage: clear dashboard cards: %w", err)
	}
	for i, c := range cards {
		opts := []byte(c.Options)
		if len(opts) == 0 {
			opts = []byte("{}")
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO dashboard_cards (tenant_id, service_name, position, metric, card_type, options, updated_at, updated_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, tenantID, service, i, c.Metric, c.CardType, opts, at, actor); err != nil {
			return fmt.Errorf("storage: insert dashboard card: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("storage: replace dashboard cards: %w", err)
	}
	return nil
}

// DashboardCards returns the saved layout of one tenant's CLI, in order;
// saved is false when there is none.
func (q *QueryBuilder) DashboardCards(ctx context.Context, scope TenantScope, service string) (cards []DashboardCard, saved bool, err error) {
	rows, err := q.pool.Query(ctx, `
SELECT metric, card_type, options FROM dashboard_cards
WHERE tenant_id = $1 AND service_name = $2 ORDER BY position`, scope.tenantID, service)
	if err != nil {
		return nil, false, fmt.Errorf("storage: dashboard cards: %w", err)
	}
	defer rows.Close()
	cards = []DashboardCard{}
	for rows.Next() {
		var c DashboardCard
		var opts []byte
		if err := rows.Scan(&c.Metric, &c.CardType, &opts); err != nil {
			return nil, false, fmt.Errorf("storage: scan dashboard cards: %w", err)
		}
		c.Options = opts
		cards = append(cards, c)
	}
	return cards, len(cards) > 0, rows.Err()
}
