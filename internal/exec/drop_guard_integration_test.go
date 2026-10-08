//go:build integration

package exec

import (
	"context"
	"fmt"
	"testing"

	"github.com/muandane/grizzle/internal/dialect/postgres"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/testutil"
)

// TestValidateCatalogDropSafety_RequiresLiveMarker proves a saved DROP_PUBLICATION
// is refused when the grizzle-managed marker is absent at apply time and
// allowed only while the exact marker is still present.
func TestValidateCatalogDropSafety_RequiresLiveMarker(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()

	const name = "grizzle_drop_guard_test"
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`DROP PUBLICATION IF EXISTS %s;`, name)); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`CREATE PUBLICATION %s;`, name)); err != nil {
		t.Fatalf("create publication: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), fmt.Sprintf(`DROP PUBLICATION IF EXISTS %s;`, name))
	})

	steps := []plan.Step{{Type: plan.ChangeDropPublication, Table: name, SQL: postgres.GenerateDropPublicationSQL(name)}}

	if err := validateCatalogDropSafety(ctx, db, steps); err == nil {
		t.Fatal("expected refusal: publication lacks grizzle-managed marker")
	}

	if _, err := db.ExecContext(ctx, postgres.GeneratePublicationCommentSQL(name)); err != nil {
		t.Fatalf("stamp marker: %v", err)
	}
	if err := validateCatalogDropSafety(ctx, db, steps); err != nil {
		t.Fatalf("expected drop allowed with exact marker, got: %v", err)
	}

	// Marker altered after planning must fail closed.
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`COMMENT ON PUBLICATION %s IS 'operator';`, name)); err != nil {
		t.Fatalf("alter marker: %v", err)
	}
	if err := validateCatalogDropSafety(ctx, db, steps); err == nil {
		t.Fatal("expected refusal after marker changed to a non-exact value")
	}
}
