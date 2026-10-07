package grizzle_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/muandane/grizzle"
	"github.com/muandane/grizzle/internal/testutil"
)

// TestApply_RejectsTamperedTimeouts verifies Apply validates operational
// timeout fields before touching the database.
func TestApply_RejectsTamperedTimeouts(t *testing.T) {
	db := testutil.TestDatabase(t)

	cases := []struct {
		name   string
		mutate func(p *grizzle.Plan)
	}{
		{"negative lock timeout", func(p *grizzle.Plan) { p.LockTimeout = -time.Second }},
		{"absurd statement timeout", func(p *grizzle.Plan) { p.StatementTimeout = 48 * time.Hour }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &grizzle.Plan{
				TargetSchema: "public",
				Steps:        []grizzle.Step{{Type: grizzle.ChangeAddColumn, Table: "users", SQL: `ALTER TABLE "public"."users" ADD COLUMN name TEXT;`}},
				SchemaSQL:    `CREATE TABLE users (id INT PRIMARY KEY, name TEXT);`,
			}
			tc.mutate(p)

			err := grizzle.Apply(context.Background(), db, p, grizzle.ApplyOpts{})
			if err == nil {
				t.Fatalf("expected Apply to reject tampered plan, got nil")
			}
			if !errors.Is(err, grizzle.ErrInvalidOptions) {
				t.Errorf("expected ErrInvalidOptions, got: %v", err)
			}
		})
	}
}

// TestApply_ArtifactCannotOverrideLockIdentity verifies that plan.json cannot
// carry a LockID: after round-trip serialization the apply path uses
// namespace-derived locks (LockID=0), not an artifact-supplied value.
func TestApply_ArtifactCannotOverrideLockIdentity(t *testing.T) {
	db := testutil.TestDatabase(t)

	schema := fmt.Sprintf("test_replan_lock_%d", time.Now().UnixNano())
	if _, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema)); err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() { _, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema)) }()

	p, err := grizzle.PlanDiff(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    "CREATE TABLE roundtrip (id BIGINT PRIMARY KEY);",
	})
	if err != nil {
		t.Fatalf("PlanDiff failed: %v", err)
	}

	data, err := p.ToJSON()
	if err != nil {
		t.Fatalf("ToJSON failed: %v", err)
	}
	if strings.Contains(string(data), `"lock_id"`) || strings.Contains(string(data), `"shadow_schema"`) {
		t.Fatalf("artifact must not persist lock_id or shadow_schema: %s", data)
	}
	parsed, hash, err := grizzle.ParsePlanJSON(data)
	if err != nil {
		t.Fatalf("ParsePlanJSON failed: %v", err)
	}
	if hash != p.Hash() {
		t.Fatalf("envelope hash mismatch")
	}

	// Holding an arbitrary single-key advisory lock must NOT block Apply,
	// because Apply no longer trusts artifact LockIDs — it uses
	// namespace+schema 2-int locks.
	holderConn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("failed acquiring holder conn: %v", err)
	}
	defer func() { _ = holderConn.Close() }()

	arbitraryLockID := int64(918273645111)
	var dummy int
	if err := holderConn.QueryRowContext(context.Background(), "SELECT 1 FROM pg_advisory_lock($1);", arbitraryLockID).Scan(&dummy); err != nil {
		t.Fatalf("holder failed acquiring lock: %v", err)
	}
	defer func() {
		var unlocked bool
		_ = holderConn.QueryRowContext(context.Background(), "SELECT pg_advisory_unlock($1);", arbitraryLockID).Scan(&unlocked)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := grizzle.Apply(ctx, db, parsed, grizzle.ApplyOpts{ExpectedHash: hash}); err != nil {
		t.Fatalf("Apply should succeed with namespace locks despite unrelated LockID holder: %v", err)
	}
}

// TestApply_ExpectedHashRejectsPolicyTamper verifies mutating Policy after
// approval fails ExpectedHash verification before execution.
func TestApply_ExpectedHashRejectsPolicyTamper(t *testing.T) {
	db := testutil.TestDatabase(t)

	p := &grizzle.Plan{
		TargetSchema: "public",
		Policy:       grizzle.DropPolicy{},
		Steps:        []grizzle.Step{{Type: grizzle.ChangeAddColumn, Table: "users", SQL: `ALTER TABLE "public"."users" ADD COLUMN name TEXT;`}},
		SchemaSQL:    `CREATE TABLE users (id INT PRIMARY KEY, name TEXT);`,
	}
	approved := p.Hash()
	p.Policy.AllowTable = true

	err := grizzle.Apply(context.Background(), db, p, grizzle.ApplyOpts{ExpectedHash: approved})
	if err == nil {
		t.Fatal("expected ErrPlanDrift for policy tamper")
	}
	if !errors.Is(err, grizzle.ErrPlanDrift) {
		t.Fatalf("expected ErrPlanDrift, got: %v", err)
	}
}

// TestSync_ShadowSchemaTooLong_Rejected verifies prepareOptions errors on a
// shadow schema name PostgreSQL would truncate instead of silently using it.
func TestSync_ShadowSchemaTooLong_Rejected(t *testing.T) {
	db := testutil.TestDatabase(t)

	cases := []struct {
		name   string
		shadow string
	}{
		{"too long", strings.Repeat("s", 64)},
		{"not an identifier", `shadow"; DROP SCHEMA public; --`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := grizzle.Sync(context.Background(), db, grizzle.Options{
				Dialect:      grizzle.DialectPostgres,
				TargetSchema: "public",
				SchemaSQL:    "CREATE TABLE shadow_len_probe (id BIGINT PRIMARY KEY);",
				ShadowSchema: tc.shadow,
			})
			if err == nil {
				t.Fatalf("expected rejection for shadow schema %q, got nil", tc.shadow)
			}
			if !errors.Is(err, grizzle.ErrInvalidOptions) {
				t.Errorf("expected ErrInvalidOptions, got: %v", err)
			}
		})
	}
}

// TestSync_HistoryWriteFailure_TypedNonFatal verifies a successful migration
// whose history INSERT fails surfaces plan.ErrHistoryRecord (non-fatal,
// errors.Is-detectable) while the schema changes remain applied.
func TestSync_HistoryWriteFailure_TypedNonFatal(t *testing.T) {
	db := testutil.TestDatabase(t)

	schema := fmt.Sprintf("test_hist_fail_%d", time.Now().UnixNano())
	if _, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema)); err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() { _, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema)) }()

	// Poison grizzle_history: the status CHECK rejects every INSERT.
	histTable := fmt.Sprintf(`CREATE TABLE %s.grizzle_history (
		id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
		plan_hash VARCHAR(64),
		status TEXT NOT NULL CHECK (status <> 'applied'),
		failed_step INT,
		error TEXT,
		applied_at TIMESTAMPTZ,
		duration_ms INT,
		applied_by TEXT,
		steps JSONB,
		seed_hash VARCHAR(64)
	);`, schema)
	if _, err := db.Exec(histTable); err != nil {
		t.Fatalf("failed creating poisoned history table: %v", err)
	}

	err := grizzle.Sync(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL: `
			CREATE TABLE widgets (id BIGINT PRIMARY KEY, sku TEXT);
			CREATE INDEX idx_widgets_sku ON widgets(sku);
		`,
	})
	if err == nil {
		t.Fatalf("expected typed non-fatal history error, got nil")
	}
	if !errors.Is(err, grizzle.ErrHistoryRecord) {
		t.Errorf("expected ErrHistoryRecord, got: %v", err)
	}

	var exists bool
	if err := db.QueryRow(`SELECT EXISTS (
		SELECT 1 FROM pg_indexes WHERE schemaname = $1 AND indexname = 'idx_widgets_sku'
	);`, schema).Scan(&exists); err != nil {
		t.Fatalf("querying index: %v", err)
	}
	if !exists {
		t.Errorf("index was not applied despite non-fatal history error")
	}
}

// TestSync_HistoryWriteFailure_TransactionalDDL verifies Model B: when the
// final group is transactional, history is recorded after commit. A poisoned
// history table must leave DDL committed and return ErrHistoryRecord.
func TestSync_HistoryWriteFailure_TransactionalDDL(t *testing.T) {
	db := testutil.TestDatabase(t)

	schema := fmt.Sprintf("test_hist_tx_%d", time.Now().UnixNano())
	if _, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema)); err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() { _, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema)) }()

	histTable := fmt.Sprintf(`CREATE TABLE %s.grizzle_history (
		id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
		plan_hash VARCHAR(64),
		status TEXT NOT NULL CHECK (status <> 'applied'),
		failed_step INT,
		error TEXT,
		applied_at TIMESTAMPTZ,
		duration_ms INT,
		applied_by TEXT,
		steps JSONB,
		seed_hash VARCHAR(64)
	);`, schema)
	if _, err := db.Exec(histTable); err != nil {
		t.Fatalf("failed creating poisoned history table: %v", err)
	}

	err := grizzle.Sync(context.Background(), db, grizzle.Options{
		Dialect:              grizzle.DialectPostgres,
		TargetSchema:         schema,
		NonConcurrentIndexes: true, // force transactional CREATE INDEX
		SchemaSQL: `
			CREATE TABLE widgets (id BIGINT PRIMARY KEY, sku TEXT);
			CREATE INDEX idx_widgets_sku ON widgets(sku);
		`,
	})
	if !errors.Is(err, grizzle.ErrHistoryRecord) {
		t.Fatalf("expected ErrHistoryRecord, got: %v", err)
	}

	var exists bool
	if err := db.QueryRow(`SELECT EXISTS (
		SELECT 1 FROM information_schema.tables WHERE table_schema = $1 AND table_name = 'widgets'
	);`, schema).Scan(&exists); err != nil {
		t.Fatalf("querying table: %v", err)
	}
	if !exists {
		t.Fatal("transactional DDL must remain committed after history failure")
	}
}
