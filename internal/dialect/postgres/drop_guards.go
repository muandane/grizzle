package postgres

import (
	"context"
	"fmt"

	"github.com/muandane/grizzle/internal/dialect"
	"github.com/muandane/grizzle/internal/schema"
)

// Managed catalog object kinds accepted by ObjectHasManagedMarker.
const (
	ManagedKindPublication  = "publication"
	ManagedKindEventTrigger = "event trigger"
	ManagedKindSubscription = "subscription"
)

// ObjectHasManagedMarker reports whether the named catalog object still
// carries the exact grizzle-managed COMMENT immediately before a destructive
// drop. Plans are computed earlier; the marker may have been removed since,
// so apply re-verifies it under the advisory lock.
func ObjectHasManagedMarker(ctx context.Context, dbtx dialect.DBTX, kind, name string) (bool, error) {
	var query, marker string
	switch kind {
	case ManagedKindPublication:
		query = `SELECT EXISTS (
			SELECT 1 FROM pg_publication p
			WHERE p.pubname = $1
			  AND obj_description(p.oid, 'pg_publication') = $2
		);`
		marker = schema.PublicationManagedComment
	case ManagedKindEventTrigger:
		query = `SELECT EXISTS (
			SELECT 1 FROM pg_event_trigger e
			WHERE e.evtname = $1
			  AND obj_description(e.oid, 'pg_event_trigger') = $2
		);`
		marker = schema.EventTriggerManagedComment
	case ManagedKindSubscription:
		query = `SELECT EXISTS (
			SELECT 1 FROM pg_subscription s
			WHERE s.subname = $1
			  AND s.subdbid = (SELECT oid FROM pg_database WHERE datname = current_database())
			  AND obj_description(s.oid, 'pg_subscription') = $2
		);`
		marker = schema.SubscriptionManagedComment
	default:
		return false, fmt.Errorf("unknown managed catalog object kind %q", kind)
	}
	var managed bool
	if err := dbtx.QueryRowContext(ctx, query, name, marker).Scan(&managed); err != nil {
		return false, fmt.Errorf("checking managed marker for %s %q: %w", kind, name, err)
	}
	return managed, nil
}

// ReplicationSlotIsActive reports whether a logical slot in the current
// database is held by a backend (active_pid IS NOT NULL). An absent slot
// reports false; the DROP itself then fails with PostgreSQL's own error.
func ReplicationSlotIsActive(ctx context.Context, dbtx dialect.DBTX, name string) (bool, error) {
	var active bool
	err := dbtx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_replication_slots
			WHERE slot_name = $1
			  AND database = current_database()
			  AND slot_type = 'logical'
			  AND active_pid IS NOT NULL
		);`, name).Scan(&active)
	if err != nil {
		return false, fmt.Errorf("checking active state for replication slot %q: %w", name, err)
	}
	return active, nil
}
