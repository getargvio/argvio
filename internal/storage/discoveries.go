package storage

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// What kind of thing an install discovered.
const (
	DiscoveryFeature = "feature"
	DiscoveryFlag    = "flag"
)

// Log events and attributes discovery tracking is derived from. A feature is
// discovered by a cli.feature.activated event naming it; a flag by a
// cli.command.invoked event listing it in cli.flag.names.
const (
	eventFeatureActivated = "cli.feature.activated"
	eventCommandInvoked   = "cli.command.invoked"

	attrActivationFeature = "cli.activation.feature"
	attrFlagNames         = "cli.flag.names"
)

// DiscoveryEvent is one sighting of an install using a feature or flag.
type DiscoveryEvent struct {
	TenantID  uuid.UUID
	InstallID string
	Kind      string
	Name      string
	At        time.Time
}

// DiscoveryEventsFromLog extracts the discoveries a log row reveals: none for
// other events and for records without an install ID (anonymous-tier
// sessions), one for a feature activation, and one per distinct flag name for
// a command invocation.
func DiscoveryEventsFromLog(r LogRow) []DiscoveryEvent {
	if r.EventName != eventFeatureActivated && r.EventName != eventCommandInvoked {
		return nil
	}
	id, _ := r.LogAttributes[attrInstallID].(string)
	if id == "" {
		return nil
	}
	mk := func(kind, name string) DiscoveryEvent {
		return DiscoveryEvent{TenantID: r.TenantID, InstallID: id, Kind: kind, Name: name, At: r.Time}
	}

	if r.EventName == eventFeatureActivated {
		if name, _ := r.LogAttributes[attrActivationFeature].(string); name != "" {
			return []DiscoveryEvent{mk(DiscoveryFeature, name)}
		}
		return nil
	}

	var names []string
	switch v := r.LogAttributes[attrFlagNames].(type) {
	case []string:
		names = v
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				names = append(names, s)
			}
		}
	}
	var out []DiscoveryEvent
	seen := map[string]bool{}
	for _, n := range names {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, mk(DiscoveryFlag, n))
	}
	return out
}

type discoveryKey struct {
	tenant  uuid.UUID
	install string
	kind    string
	name    string
}

// The WHERE keeps replays and later sightings from rewriting the row: only an
// earlier first_at changes it, so late-arriving events can still move it back.
const upsertDiscoveries = `
INSERT INTO install_discoveries (tenant_id, install_id, kind, name, first_at)
SELECT u.tenant_id::uuid, u.install_id, u.kind, u.name, u.first_at
FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::timestamptz[])
     AS u(tenant_id, install_id, kind, name, first_at)
ON CONFLICT (tenant_id, install_id, kind, name) DO UPDATE SET first_at = EXCLUDED.first_at
WHERE EXCLUDED.first_at < install_discoveries.first_at`

// TrackDiscoveries records the earliest sighting of each (install, kind,
// name) in install_discoveries with one statement. Events for any number of
// installs and tenants may be mixed. Rows are inserted in a stable order so
// concurrent batches can't deadlock on each other's row locks.
func (w *Writer) TrackDiscoveries(ctx context.Context, events []DiscoveryEvent) error {
	if len(events) == 0 {
		return nil
	}
	first := map[discoveryKey]time.Time{}
	for _, e := range events {
		k := discoveryKey{e.TenantID, e.InstallID, e.Kind, e.Name}
		if cur, ok := first[k]; !ok || e.At.Before(cur) {
			first[k] = e.At
		}
	}
	keys := make([]discoveryKey, 0, len(first))
	for k := range first {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if c := compareUUID(a.tenant, b.tenant); c != 0 {
			return c < 0
		}
		if a.install != b.install {
			return a.install < b.install
		}
		if a.kind != b.kind {
			return a.kind < b.kind
		}
		return a.name < b.name
	})

	tenants := make([]string, len(keys))
	installs := make([]string, len(keys))
	kinds := make([]string, len(keys))
	names := make([]string, len(keys))
	at := make([]time.Time, len(keys))
	for i, k := range keys {
		tenants[i], installs[i], kinds[i], names[i], at[i] = k.tenant.String(), k.install, k.kind, k.name, first[k]
	}
	if _, err := w.pool.Exec(ctx, upsertDiscoveries, tenants, installs, kinds, names, at); err != nil {
		return fmt.Errorf("storage: track discoveries: %w", err)
	}
	return nil
}

// BackfillDiscoveries rebuilds install_discoveries from raw logs at or after
// since, replaying each (install, kind, name)'s earliest sighting through the
// same upsert as live ingest. Run it once after migrating to 0008; rerunning
// is safe. Discoveries older than log retention can't be recovered, so an
// install's first use of a feature may look later than it was. It returns how
// many discoveries it replayed.
func BackfillDiscoveries(ctx context.Context, pool *pgxpool.Pool, since time.Time) (int64, error) {
	rows, err := pool.Query(ctx, `
SELECT tenant_id, install_id, kind, name, min(time)
FROM (
	SELECT tenant_id, log_attributes ->> 'cli.install_id' AS install_id, 'feature' AS kind,
	       log_attributes ->> 'cli.activation.feature' AS name, time
	FROM logs
	WHERE time >= $1 AND event_name = 'cli.feature.activated'
	UNION ALL
	SELECT l.tenant_id, l.log_attributes ->> 'cli.install_id', 'flag', f.name, l.time
	FROM logs l,
	     jsonb_array_elements_text(CASE WHEN jsonb_typeof(l.log_attributes -> 'cli.flag.names') = 'array'
	                                    THEN l.log_attributes -> 'cli.flag.names' ELSE '[]'::jsonb END) AS f(name)
	WHERE l.time >= $1 AND l.event_name = 'cli.command.invoked'
) d
WHERE coalesce(install_id, '') <> '' AND coalesce(name, '') <> ''
GROUP BY tenant_id, install_id, kind, name`, since)
	if err != nil {
		return 0, fmt.Errorf("storage: backfill discoveries: %w", err)
	}
	defer rows.Close()

	w := NewWriter(pool)
	var total int64
	chunk := make([]DiscoveryEvent, 0, backfillChunk)
	flush := func() error {
		if err := w.TrackDiscoveries(ctx, chunk); err != nil {
			return err
		}
		total += int64(len(chunk))
		chunk = chunk[:0]
		return nil
	}
	for rows.Next() {
		var e DiscoveryEvent
		if err := rows.Scan(&e.TenantID, &e.InstallID, &e.Kind, &e.Name, &e.At); err != nil {
			return total, fmt.Errorf("storage: backfill discoveries: scan: %w", err)
		}
		chunk = append(chunk, e)
		if len(chunk) == backfillChunk {
			if err := flush(); err != nil {
				return total, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return total, fmt.Errorf("storage: backfill discoveries: %w", err)
	}
	if err := flush(); err != nil {
		return total, err
	}
	return total, nil
}
