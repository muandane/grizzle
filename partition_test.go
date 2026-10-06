package grizzle_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/muandane/grizzle"
	"github.com/muandane/grizzle/internal/dialect/postgres"
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
