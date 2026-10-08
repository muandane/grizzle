//go:build integration

package exec_test

import (
	"context"
	"testing"

	"github.com/muandane/grizzle/internal/dialect/postgres"
	"github.com/muandane/grizzle/internal/testutil"
)

// TestSubscriptions_InspectSkipsWithoutLogicalWAL verifies live catalog
// inspection for subscriptions/slots succeeds. Creating subscriptions needs a
// reachable publisher; that path is unit-covered. When wal_level is not
// logical, slot creation is skipped gracefully.
func TestSubscriptions_InspectSkipsWithoutLogicalWAL(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()

	live, err := postgres.InspectLiveCatalog(ctx, db)
	if err != nil {
		t.Fatalf("InspectLiveCatalog: %v", err)
	}
	if live.Subscriptions == nil || live.ReplicationSlots == nil {
		t.Fatal("expected subscription/slot maps to be initialized")
	}

	var walLevel string
	if err := db.QueryRowContext(ctx, `SHOW wal_level`).Scan(&walLevel); err != nil {
		t.Fatalf("SHOW wal_level: %v", err)
	}
	if walLevel != "logical" {
		t.Skipf("wal_level=%q; skipping logical slot create (need logical)", walLevel)
	}
	t.Log("wal_level=logical; subscription create still requires a publisher — unit-covered")
}
