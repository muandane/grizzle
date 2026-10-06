package grizzle_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/muandane/grizzle"
	"github.com/muandane/grizzle/internal/dialect/postgres"
	"github.com/muandane/grizzle/internal/diff"
	"github.com/muandane/grizzle/internal/plan"
)

func getPostgresDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	connStr := os.Getenv("DATABASE_URL")
	if connStr == "" {
		connStr = os.Getenv("POSTGRES_DSN")
	}
	if connStr == "" {
		connStr = "postgres://127.0.0.1:5432/grizzle_test?sslmode=disable"
	}
	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("failed to open postgres: %v", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		t.Skipf("skipping postgres partition test, db unavailable: %v", err)
	}
	return db, connStr
}

func TestPartition_PostgresIntrospection(t *testing.T) {
	db, _ := getPostgresDB(t)
	defer func() { _ = db.Close() }()

	schemaPrefix := fmt.Sprintf("test_part_intro_%d", time.Now().UnixNano())
	_, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schemaPrefix))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schemaPrefix))
	}()

	//nolint:gosec // G201: test constructs setup DDL with randomized schema prefix
	setupSQL := fmt.Sprintf(`
		SET search_path TO %q;
		CREATE TABLE measurements (
			city_id INT NOT NULL,
			log_date DATE NOT NULL,
			peak_temp INT
		) PARTITION BY RANGE (log_date);

		CREATE TABLE measurements_y2026m01 PARTITION OF measurements
			FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
	`, schemaPrefix)

	if _, err := db.Exec(setupSQL); err != nil {
		t.Fatalf("failed setup: %v", err)
	}

	s, err := postgres.Inspect(context.Background(), db, schemaPrefix)
	if err != nil {
		t.Fatalf("inspect failed: %v", err)
	}

	tbl, exists := s.Tables["measurements"]
	if !exists {
		t.Fatalf("measurements table not found")
	}
	if tbl.PartitionKey == nil {
		t.Fatalf("expected PartitionKey on measurements, got nil")
	}
	if tbl.PartitionKey.Strategy != grizzle.PartitionStrategyRange && tbl.PartitionKey.Strategy != "RANGE" {
		t.Errorf("expected RANGE strategy, got: %s", tbl.PartitionKey.Strategy)
	}

	child, exists := s.Tables["measurements_y2026m01"]
	if !exists {
		t.Fatalf("measurements_y2026m01 child table not found")
	}
	if child.PartitionOf == nil {
		t.Fatalf("expected PartitionOf on measurements_y2026m01, got nil")
	}
	if child.PartitionOf.Parent != "measurements" {
		t.Errorf("expected parent measurements, got: %s", child.PartitionOf.Parent)
	}
}

func TestPartition_DeclarativeCreationAndIdempotency(t *testing.T) {
	db, _ := getPostgresDB(t)
	defer func() { _ = db.Close() }()

	schemaPrefix := fmt.Sprintf("test_part_idemp_%d", time.Now().UnixNano())
	_, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schemaPrefix))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schemaPrefix))
	}()

	desiredSQL := `
		CREATE TABLE measurements (
			city_id INT NOT NULL,
			log_date DATE NOT NULL,
			peak_temp INT,
			units_sold INT
		) PARTITION BY RANGE (log_date);

		CREATE TABLE measurements_y2026m01 PARTITION OF measurements
			FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');

		CREATE TABLE measurements_y2026m02 PARTITION OF measurements
			FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');
	`

	opts := grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schemaPrefix,
		SchemaSQL:    desiredSQL,
	}

	// 1. Initial Apply / Sync
	if err := grizzle.Sync(context.Background(), db, opts); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}

	// 2. Re-introspect and check idempotency (diff == empty)
	planDiff, err := grizzle.PlanDiff(context.Background(), db, opts)
	if err != nil {
		t.Fatalf("PlanDiff failed: %v", err)
	}
	if len(planDiff.Steps) != 0 {
		t.Fatalf("expected 0 steps on re-diff, got %d: %+v", len(planDiff.Steps), planDiff.Steps)
	}
}

func TestPartition_ListAndHashPartitions(t *testing.T) {
	db, _ := getPostgresDB(t)
	defer func() { _ = db.Close() }()

	schemaPrefix := fmt.Sprintf("test_part_list_hash_%d", time.Now().UnixNano())
	_, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schemaPrefix))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schemaPrefix))
	}()

	desiredSQL := `
		CREATE TABLE sales (
			id INT NOT NULL,
			country TEXT NOT NULL,
			amount NUMERIC
		) PARTITION BY LIST (country);

		CREATE TABLE sales_na PARTITION OF sales
			FOR VALUES IN ('US', 'CA', 'MX');

		CREATE TABLE sales_eu PARTITION OF sales
			FOR VALUES IN ('FR', 'DE', 'UK');

		CREATE TABLE events (
			id BIGINT NOT NULL,
			payload TEXT
		) PARTITION BY HASH (id);

		CREATE TABLE events_p0 PARTITION OF events
			FOR VALUES WITH (MODULUS 2, REMAINDER 0);

		CREATE TABLE events_p1 PARTITION OF events
			FOR VALUES WITH (MODULUS 2, REMAINDER 1);
	`

	opts := grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schemaPrefix,
		SchemaSQL:    desiredSQL,
	}

	if err := grizzle.Sync(context.Background(), db, opts); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}

	planDiff, err := grizzle.PlanDiff(context.Background(), db, opts)
	if err != nil {
		t.Fatalf("PlanDiff failed: %v", err)
	}
	if len(planDiff.Steps) != 0 {
		t.Fatalf("expected 0 steps on re-diff, got %d: %+v", len(planDiff.Steps), planDiff.Steps)
	}
}

func TestPartition_AttachScanHazard(t *testing.T) {
	db, _ := getPostgresDB(t)
	defer func() { _ = db.Close() }()

	schemaPrefix := fmt.Sprintf("test_part_attach_%d", time.Now().UnixNano())
	_, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schemaPrefix))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schemaPrefix))
	}()

	// 1. Initial live database has parent partitioned table and standalone measurements_extra table
	//nolint:gosec // G201: test constructs setup DDL with randomized schema prefix
	setupSQL := fmt.Sprintf(`
		SET search_path TO %q;
		CREATE TABLE measurements (
			city_id INT NOT NULL,
			log_date DATE NOT NULL
		) PARTITION BY RANGE (log_date);

		CREATE TABLE measurements_extra (
			city_id INT NOT NULL,
			log_date DATE NOT NULL
		);
	`, schemaPrefix)
	if _, err := db.Exec(setupSQL); err != nil {
		t.Fatalf("failed setup: %v", err)
	}

	// 2. Desired schema defines measurements_extra as attached partition of measurements
	desiredSQL := `
		CREATE TABLE measurements (
			city_id INT NOT NULL,
			log_date DATE NOT NULL
		) PARTITION BY RANGE (log_date);

		CREATE TABLE measurements_extra PARTITION OF measurements
			FOR VALUES FROM ('2026-05-01') TO ('2026-06-01');
	`

	opts := grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schemaPrefix,
		SchemaSQL:    desiredSQL,
	}

	p, err := grizzle.PlanDiff(context.Background(), db, opts)
	if err != nil {
		t.Fatalf("PlanDiff failed: %v", err)
	}

	// Check step emitted is ATTACH_PARTITION
	var foundAttach bool
	for _, s := range p.Steps {
		if s.Type == grizzle.ChangeAttachPartition && s.Table == "measurements_extra" && s.ParentTable == "measurements" {
			foundAttach = true
			break
		}
	}
	if !foundAttach {
		t.Fatalf("expected ChangeAttachPartition step, got steps: %+v", p.Steps)
	}

	// Check hazard emitted is PARTITION_ATTACH_SCAN with WARNING level
	hazards := p.Hazards()
	var foundHazard bool
	for _, h := range hazards {
		if h.Code == grizzle.HazardPartitionAttachScan && h.Level == grizzle.HazardLevelWarning {
			foundHazard = true
			break
		}
	}
	if !foundHazard {
		t.Fatalf("expected HazardPartitionAttachScan with WARNING level, got hazards: %+v", hazards)
	}

	// 3. Applying the plan attaches the partition successfully
	if err := grizzle.Sync(context.Background(), db, opts); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}

	// 4. Verify idempotent
	p2, err := grizzle.PlanDiff(context.Background(), db, opts)
	if err != nil {
		t.Fatalf("re-diff failed: %v", err)
	}
	if len(p2.Steps) != 0 {
		t.Fatalf("expected 0 steps after attach, got %d: %+v", len(p2.Steps), p2.Steps)
	}
}

func TestPartition_Detach(t *testing.T) {
	db, _ := getPostgresDB(t)
	defer func() { _ = db.Close() }()

	schemaPrefix := fmt.Sprintf("test_part_detach_%d", time.Now().UnixNano())
	_, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schemaPrefix))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schemaPrefix))
	}()

	// 1. Initial live database has attached partition
	//nolint:gosec // G201: test constructs setup DDL with randomized schema prefix
	setupSQL := fmt.Sprintf(`
		SET search_path TO %q;
		CREATE TABLE logs (
			id INT NOT NULL,
			created_at DATE NOT NULL
		) PARTITION BY RANGE (created_at);

		CREATE TABLE logs_2026 PARTITION OF logs
			FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
	`, schemaPrefix)
	if _, err := db.Exec(setupSQL); err != nil {
		t.Fatalf("failed setup: %v", err)
	}

	// 2. Desired schema defines logs_2026 as standalone table
	desiredSQL := `
		CREATE TABLE logs (
			id INT NOT NULL,
			created_at DATE NOT NULL
		) PARTITION BY RANGE (created_at);

		CREATE TABLE logs_2026 (
			id INT NOT NULL,
			created_at DATE NOT NULL
		);
	`

	opts := grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schemaPrefix,
		SchemaSQL:    desiredSQL,
	}

	p, err := grizzle.PlanDiff(context.Background(), db, opts)
	if err != nil {
		t.Fatalf("PlanDiff failed: %v", err)
	}

	// Check step emitted is DETACH_PARTITION
	var foundDetach bool
	for _, s := range p.Steps {
		if s.Type == grizzle.ChangeDetachPartition && s.Table == "logs_2026" && s.ParentTable == "logs" {
			foundDetach = true
			break
		}
	}
	if !foundDetach {
		t.Fatalf("expected ChangeDetachPartition step, got steps: %+v", p.Steps)
	}

	// 3. Applying the plan detaches the partition
	if err := grizzle.Sync(context.Background(), db, opts); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}

	// 4. Verify idempotent
	p2, err := grizzle.PlanDiff(context.Background(), db, opts)
	if err != nil {
		t.Fatalf("re-diff failed: %v", err)
	}
	if len(p2.Steps) != 0 {
		t.Fatalf("expected 0 steps after detach, got %d: %+v", len(p2.Steps), p2.Steps)
	}
}

func TestPartition_RejectInPlaceConversion(t *testing.T) {
	db, _ := getPostgresDB(t)
	defer func() { _ = db.Close() }()

	schemaPrefix := fmt.Sprintf("test_part_reject_%d", time.Now().UnixNano())
	_, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schemaPrefix))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schemaPrefix))
	}()

	// 1. Initial live table is a regular table
	//nolint:gosec // G201: test constructs setup DDL with randomized schema prefix
	setupSQL := fmt.Sprintf(`
		SET search_path TO %q;
		CREATE TABLE orders (
			id INT PRIMARY KEY,
			order_date DATE NOT NULL
		);
	`, schemaPrefix)
	if _, err := db.Exec(setupSQL); err != nil {
		t.Fatalf("failed setup: %v", err)
	}

	// 2. Desired schema defines orders as partitioned table
	desiredSQL := `
		CREATE TABLE orders (
			id INT NOT NULL,
			order_date DATE NOT NULL
		) PARTITION BY RANGE (order_date);
	`

	opts := grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schemaPrefix,
		SchemaSQL:    desiredSQL,
	}

	_, err = grizzle.PlanDiff(context.Background(), db, opts)
	if err == nil {
		t.Fatalf("expected PlanDiff to fail with ErrPartitionConversion, got nil")
	}
	if !errors.Is(err, grizzle.ErrPartitionConversion) {
		t.Fatalf("expected ErrPartitionConversion, got: %v", err)
	}

	// 3. Test reverse: live table is partitioned table, desired is regular table
	schemaPrefixRev := fmt.Sprintf("test_part_reject_rev_%d", time.Now().UnixNano())
	_, err = db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schemaPrefixRev))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schemaPrefixRev))
	}()

	//nolint:gosec // G201: test constructs setup DDL with randomized schema prefix
	setupSQLRev := fmt.Sprintf(`
		SET search_path TO %q;
		CREATE TABLE orders (
			id INT NOT NULL,
			order_date DATE NOT NULL
		) PARTITION BY RANGE (order_date);
	`, schemaPrefixRev)
	if _, err := db.Exec(setupSQLRev); err != nil {
		t.Fatalf("failed setup: %v", err)
	}

	desiredSQLRev := `
		CREATE TABLE orders (
			id INT PRIMARY KEY,
			order_date DATE NOT NULL
		);
	`

	optsRev := grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schemaPrefixRev,
		SchemaSQL:    desiredSQLRev,
	}

	_, err = grizzle.PlanDiff(context.Background(), db, optsRev)
	if err == nil {
		t.Fatalf("expected reverse PlanDiff to fail with ErrPartitionConversion, got nil")
	}
	if !errors.Is(err, grizzle.ErrPartitionConversion) {
		t.Fatalf("expected ErrPartitionConversion on reverse conversion, got: %v", err)
	}
}

func TestPartition_StructuralValidation_PKUniqueRules(t *testing.T) {
	db, _ := getPostgresDB(t)
	defer func() { _ = db.Close() }()

	schemaPrefix := fmt.Sprintf("test_part_struct_%d", time.Now().UnixNano())
	_, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schemaPrefix))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schemaPrefix))
	}()

	// 1. Partitioned table where PRIMARY KEY omits partition key column -> fails
	desiredSQLMissingPartCol := `
		CREATE TABLE measurements (
			id BIGINT NOT NULL,
			log_date DATE NOT NULL,
			PRIMARY KEY (id)
		) PARTITION BY RANGE (log_date);
	`

	ctx := context.Background()
	_, err = grizzle.PlanDiff(ctx, db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schemaPrefix,
		SchemaSQL:    desiredSQLMissingPartCol,
	})
	if err == nil {
		t.Fatalf("expected PlanDiff to fail when PK omits partition key, got nil")
	}
	if !errors.Is(err, grizzle.ErrPartitionKeyNotInUnique) {
		t.Fatalf("expected ErrPartitionKeyNotInUnique, got: %v", err)
	}

	// 2. Partitioned table where UNIQUE INDEX omits partition key column -> fails
	desiredSQLMissingInUnique := `
		CREATE TABLE measurements (
			id BIGINT NOT NULL,
			log_date DATE NOT NULL,
			tenant_id TEXT NOT NULL,
			PRIMARY KEY (id, log_date)
		) PARTITION BY RANGE (log_date);

		CREATE UNIQUE INDEX idx_measurements_tenant ON measurements (tenant_id);
	`
	_, err = grizzle.PlanDiff(ctx, db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schemaPrefix,
		SchemaSQL:    desiredSQLMissingInUnique,
	})
	if err == nil {
		t.Fatalf("expected PlanDiff to fail when unique index omits partition key, got nil")
	}
	if !errors.Is(err, grizzle.ErrPartitionKeyNotInUnique) {
		t.Fatalf("expected ErrPartitionKeyNotInUnique, got: %v", err)
	}

	// 3. Partitioned table where PK and UNIQUE INDEX include partition key column -> succeeds
	desiredSQLValid := `
		CREATE TABLE measurements (
			id BIGINT NOT NULL,
			log_date DATE NOT NULL,
			tenant_id TEXT NOT NULL,
			PRIMARY KEY (id, log_date)
		) PARTITION BY RANGE (log_date);

		CREATE UNIQUE INDEX idx_measurements_tenant ON measurements (tenant_id, log_date);
	`
	_, err = grizzle.PlanDiff(ctx, db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schemaPrefix,
		SchemaSQL:    desiredSQLValid,
	})
	if err != nil {
		t.Fatalf("expected valid partition table to succeed, got: %v", err)
	}
}

func TestPartition_DetachConcurrently_RenderVariants(t *testing.T) {
	t.Parallel()

	change := diff.Change{
		Type:        plan.ChangeDetachPartition,
		Schema:      "public",
		Table:       "measurements_2026",
		ParentTable: "measurements",
	}

	// 1. PG 14+ without default partition: renders CONCURRENTLY as NonTx
	step14 := postgres.RenderChangeWithOpts("public", change, postgres.RenderOpts{
		ServerVersion: 140000,
	})
	if !step14.NonTx {
		t.Errorf("expected NonTx=true for PG14+ DETACH CONCURRENTLY")
	}
	if step14.SQL != `ALTER TABLE "public"."measurements" DETACH PARTITION "public"."measurements_2026" CONCURRENTLY;` {
		t.Errorf("unexpected SQL for PG14+: %s", step14.SQL)
	}

	// 2. PG 13 (< 14): renders plain DETACH as in-tx (NonTx=false)
	step13 := postgres.RenderChangeWithOpts("public", change, postgres.RenderOpts{
		ServerVersion: 130000,
	})
	if step13.NonTx {
		t.Errorf("expected NonTx=false for PG13 DETACH")
	}
	if step13.SQL != `ALTER TABLE "public"."measurements" DETACH PARTITION "public"."measurements_2026";` {
		t.Errorf("unexpected SQL for PG13: %s", step13.SQL)
	}

	// 3. NonConcurrentIndexes=true: falls back to plain DETACH as in-tx
	stepNonConcurrent := postgres.RenderChangeWithOpts("public", change, postgres.RenderOpts{
		ServerVersion:        140000,
		NonConcurrentIndexes: true,
	})
	if stepNonConcurrent.NonTx {
		t.Errorf("expected NonTx=false when NonConcurrentIndexes=true")
	}
	if stepNonConcurrent.SQL != `ALTER TABLE "public"."measurements" DETACH PARTITION "public"."measurements_2026";` {
		t.Errorf("unexpected SQL for NonConcurrentIndexes: %s", stepNonConcurrent.SQL)
	}

	// 4. ParentHasDefault=true: falls back to plain DETACH as in-tx
	changeWithDefault := change
	changeWithDefault.ParentHasDefault = true
	stepDefault := postgres.RenderChangeWithOpts("public", changeWithDefault, postgres.RenderOpts{
		ServerVersion: 140000,
	})
	if stepDefault.NonTx {
		t.Errorf("expected NonTx=false when ParentHasDefault=true")
	}
	if stepDefault.SQL != `ALTER TABLE "public"."measurements" DETACH PARTITION "public"."measurements_2026";` {
		t.Errorf("unexpected SQL for ParentHasDefault: %s", stepDefault.SQL)
	}

	// 5. IsPendingDetach=true: renders FINALIZE step as in-tx with hazard
	changePending := change
	changePending.IsPendingDetach = true
	stepPending := postgres.RenderChangeWithOpts("public", changePending, postgres.RenderOpts{
		ServerVersion: 140000,
	})
	if stepPending.NonTx {
		t.Errorf("expected NonTx=false for FINALIZE step")
	}
	if stepPending.SQL != `ALTER TABLE "public"."measurements" DETACH PARTITION "public"."measurements_2026" FINALIZE;` {
		t.Errorf("unexpected SQL for IsPendingDetach: %s", stepPending.SQL)
	}

	p := &grizzle.Plan{Steps: []grizzle.Step{stepPending}}
	hazards := p.Hazards()
	var foundPendingHazard bool
	for _, h := range hazards {
		if h.Code == grizzle.HazardPartitionPendingDetach && h.Level == grizzle.HazardLevelWarning {
			foundPendingHazard = true
		}
	}
	if !foundPendingHazard {
		t.Errorf("expected HazardPartitionPendingDetach, got hazards: %+v", hazards)
	}
}

func TestPartition_ThreeLevelNesting_Postgres(t *testing.T) {
	db, _ := getPostgresDB(t)
	defer func() { _ = db.Close() }()

	schemaPrefix := fmt.Sprintf("test_part_3level_%d", time.Now().UnixNano())
	_, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schemaPrefix))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schemaPrefix))
	}()

	// Level 1: orders partitioned by RANGE (created_at)
	// Level 2: orders_2026 partitioned by LIST (region)
	// Level 3: orders_2026_eu (leaf partition)
	threeLevelSQL := `
		CREATE TABLE orders (
			id BIGINT NOT NULL,
			created_at DATE NOT NULL,
			region TEXT NOT NULL,
			amount NUMERIC(10,2) NOT NULL,
			PRIMARY KEY (id, created_at, region)
		) PARTITION BY RANGE (created_at);

		CREATE TABLE orders_2026 PARTITION OF orders
			FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')
			PARTITION BY LIST (region);

		CREATE TABLE orders_2026_eu PARTITION OF orders_2026
			FOR VALUES IN ('EU');
	`

	opts := grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schemaPrefix,
		SchemaSQL:    threeLevelSQL,
	}

	ctx := context.Background()

	// 1. Initial Sync
	if err := grizzle.Sync(ctx, db, opts); err != nil {
		t.Fatalf("initial Sync failed: %v", err)
	}

	// 2. Second Sync must be completely idempotent (zero diff)
	plan2, err := grizzle.PlanDiff(ctx, db, opts)
	if err != nil {
		t.Fatalf("PlanDiff failed: %v", err)
	}
	if len(plan2.Steps) > 0 {
		t.Fatalf("expected 0 steps on second Sync for 3-level nesting, got %d: %+v", len(plan2.Steps), plan2.Steps)
	}
}

func TestPartition_DefaultPartition_AttachScanConflict(t *testing.T) {
	db, _ := getPostgresDB(t)
	defer func() { _ = db.Close() }()

	schemaPrefix := fmt.Sprintf("test_part_def_scan_%d", time.Now().UnixNano())
	_, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schemaPrefix))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schemaPrefix))
	}()

	ctx := context.Background()

	// 1. Create partitioned table with a DEFAULT partition and insert row in default partition
	initialSQL := fmt.Sprintf(`
		SET search_path TO %q;
		CREATE TABLE events (
			id BIGINT NOT NULL,
			event_date DATE NOT NULL,
			PRIMARY KEY (id, event_date)
		) PARTITION BY RANGE (event_date);

		CREATE TABLE events_default PARTITION OF events DEFAULT;

		CREATE TABLE events_2026_06 (
			id BIGINT NOT NULL,
			event_date DATE NOT NULL,
			PRIMARY KEY (id, event_date)
		);

		INSERT INTO events (id, event_date) VALUES (1, '2026-06-15');
	`, schemaPrefix)

	if _, err := db.Exec(initialSQL); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	// 2. Desired schema defines a new partition events_2026_06 matching the existing row
	desiredSQL := `
		CREATE TABLE events (
			id BIGINT NOT NULL,
			event_date DATE NOT NULL,
			PRIMARY KEY (id, event_date)
		) PARTITION BY RANGE (event_date);

		CREATE TABLE events_default PARTITION OF events DEFAULT;

		CREATE TABLE events_2026_06 PARTITION OF events
			FOR VALUES FROM ('2026-06-01') TO ('2026-07-01');
	`

	opts := grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schemaPrefix,
		SchemaSQL:    desiredSQL,
	}

	// 3. PlanDiff: must detect HazardPartitionAttachScan
	p, err := grizzle.PlanDiff(ctx, db, opts)
	if err != nil {
		t.Fatalf("PlanDiff failed: %v", err)
	}

	var hasAttachScanHazard bool
	for _, h := range p.Hazards() {
		if h.Code == grizzle.HazardPartitionAttachScan {
			hasAttachScanHazard = true
			break
		}
	}
	if !hasAttachScanHazard {
		t.Errorf("expected HazardPartitionAttachScan in plan hazards, got: %+v", p.Hazards())
	}

	// 4. Sync must fail because row in events_default conflicts with the new partition range
	err = grizzle.Sync(ctx, db, opts)
	if err == nil {
		t.Fatalf("expected Sync to fail due to conflicting rows in default partition, got nil")
	}
	if !errors.Is(err, plan.ErrExecutionFailed) {
		t.Errorf("expected ErrExecutionFailed, got: %v", err)
	}
}

func TestPartition_WrappedFKError_Context(t *testing.T) {
	db, _ := getPostgresDB(t)
	defer func() { _ = db.Close() }()

	schemaPrefix := fmt.Sprintf("test_part_fk_err_%d", time.Now().UnixNano())
	_, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schemaPrefix))
	if err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() {
		_, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schemaPrefix))
	}()

	ctx := context.Background()

	// Setup: parent orders table and items table with invalid foreign key data
	setupSQL := fmt.Sprintf(`
		SET search_path TO %q;
		CREATE TABLE orders (
			id BIGINT NOT NULL,
			created_at DATE NOT NULL,
			PRIMARY KEY (id, created_at)
		) PARTITION BY RANGE (created_at);

		CREATE TABLE items (
			id BIGINT PRIMARY KEY,
			order_id BIGINT NOT NULL,
			created_at DATE NOT NULL
		);

		INSERT INTO items (id, order_id, created_at) VALUES (1, 9999, '2026-01-01');
	`, schemaPrefix)
	if _, err := db.Exec(setupSQL); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	desiredSQL := `
		CREATE TABLE orders (
			id BIGINT NOT NULL,
			created_at DATE NOT NULL,
			PRIMARY KEY (id, created_at)
		) PARTITION BY RANGE (created_at);

		CREATE TABLE items (
			id BIGINT PRIMARY KEY,
			order_id BIGINT NOT NULL,
			created_at DATE NOT NULL,
			CONSTRAINT fk_items_orders FOREIGN KEY (order_id, created_at) REFERENCES orders (id, created_at)
		);
	`

	opts := grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schemaPrefix,
		SchemaSQL:    desiredSQL,
	}

	err = grizzle.Sync(ctx, db, opts)
	if err == nil {
		t.Fatalf("expected Sync to fail on invalid FK data, got nil")
	}
	// Verify error wraps table and foreign key constraint context
	errMsg := err.Error()
	if !errors.Is(err, plan.ErrExecutionFailed) {
		t.Errorf("expected ErrExecutionFailed, got: %v", err)
	}
	if !strings.Contains(errMsg, "items") || !strings.Contains(errMsg, "foreign key constraint") {
		t.Errorf("expected error message to contain table and foreign key context, got: %s", errMsg)
	}
}
