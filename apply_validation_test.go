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

// tamperShadow swaps the persisted shadow schema for a hostile value.
func tamperShadow(t *testing.T, planJSON []byte, newShadow string) []byte {
	t.Helper()
	old := `"shadow_schema": "_grizzle_shadow"`
	if !strings.Contains(string(planJSON), old) {
		// Field may be absent (omitted when equal to the recorded value);
		// fall back to a naive JSON string replacement of the empty value.
		return planJSON
	}
	return []byte(strings.Replace(string(planJSON), old, fmt.Sprintf("\"shadow_schema\": %q", newShadow), 1))
}

// TestPlan_ParseRejectsTamperedShadowSchema verifies plan loading rejects an
// artifact whose ShadowSchema points at a user schema: the shadow is dropped
// with CASCADE before compilation, so this would destroy the target.
func TestPlan_ParseRejectsTamperedShadowSchema(t *testing.T) {
	p := &grizzle.Plan{
		TargetSchema: "public",
		Steps:        []grizzle.Step{{Type: grizzle.ChangeAddColumn, Table: "users", SQL: `ALTER TABLE "public"."users" ADD COLUMN name TEXT;`}},
		SchemaSQL:    `CREATE TABLE users (id INT PRIMARY KEY, name TEXT);`,
		// Persist a shadow name so the field appears in the document and the
		// tamper below actually swaps it.
		ShadowSchema: "_grizzle_shadow",
	}
	data, err := p.ToJSON()
	if err != nil {
		t.Fatalf("serializing plan: %v", err)
	}

	tampered := tamperShadow(t, data, "public")
	if _, _, err := grizzle.ParsePlanJSON(tampered); err == nil {
		t.Fatalf("expected ParsePlanJSON to reject shadow_schema \"public\", got nil")
	}
}

// TestApply_RejectsTamperedPlanFields verifies Apply validates operational
// fields before touching the database.
func TestApply_RejectsTamperedPlanFields(t *testing.T) {
	db := testutil.TestDatabase(t)

	cases := []struct {
		name   string
		mutate func(p *grizzle.Plan)
	}{
		{"shadow=public", func(p *grizzle.Plan) { p.ShadowSchema = "public" }},
		{"shadow wrong prefix", func(p *grizzle.Plan) { p.ShadowSchema = "evil_shadow" }},
		{"shadow too long", func(p *grizzle.Plan) { p.ShadowSchema = "_grizzle_shadow_" + strings.Repeat("a", 60) }},
		{"shadow invalid ident", func(p *grizzle.Plan) { p.ShadowSchema = `_grizzle_shadow"; DROP SCHEMA public;` }},
		{"negative lock timeout", func(p *grizzle.Plan) { p.LockTimeout = -time.Second }},
		{"absurd statement timeout", func(p *grizzle.Plan) { p.StatementTimeout = 48 * time.Hour }},
		{"invalid lock namespace", func(p *grizzle.Plan) { p.LockNamespace = `grizzle"; DROP SCHEMA public;` }},
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

// TestApply_ReplanUsesPersistedLockID verifies the full CLI round trip:
// plan (with a custom LockID) -> plan.json -> apply. While a holder owns the
// custom advisory lock, Apply must block; losing the LockID in serialization
// would let it execute unprotected.
func TestApply_ReplanUsesPersistedLockID(t *testing.T) {
	db := testutil.TestDatabase(t)

	schema := fmt.Sprintf("test_replan_lock_%d", time.Now().UnixNano())
	if _, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema)); err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() { _, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema)) }()

	customLockID := int64(918273645111)
	p, err := grizzle.PlanDiff(context.Background(), db, grizzle.Options{
		Dialect:      grizzle.DialectPostgres,
		TargetSchema: schema,
		SchemaSQL:    "CREATE TABLE roundtrip (id BIGINT PRIMARY KEY);",
		LockID:       customLockID,
	})
	if err != nil {
		t.Fatalf("PlanDiff failed: %v", err)
	}

	// Serialize through the document envelope and back, as the CLI does.
	data, err := p.ToJSON()
	if err != nil {
		t.Fatalf("ToJSON failed: %v", err)
	}
	parsed, _, err := grizzle.ParsePlanJSON(data)
	if err != nil {
		t.Fatalf("ParsePlanJSON failed: %v", err)
	}
	if parsed.LockID != customLockID {
		t.Fatalf("envelope dropped LockID: got %d, want %d", parsed.LockID, customLockID)
	}

	// Holder owns the plan's advisory lock. The plan carries a custom LockID,
	// so Apply acquires the single-key form (pg_try_advisory_lock($1)); the
	// holder must hold the same lock key.
	holderConn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("failed acquiring holder conn: %v", err)
	}
	defer func() { _ = holderConn.Close() }()

	var dummy int
	if err := holderConn.QueryRowContext(context.Background(), "SELECT 1 FROM pg_advisory_lock($1);", customLockID).Scan(&dummy); err != nil {
		t.Fatalf("holder failed acquiring lock: %v", err)
	}

	applyDone := make(chan error, 1)
	go func() {
		applyDone <- grizzle.Apply(context.Background(), db, parsed, grizzle.ApplyOpts{})
	}()

	select {
	case err := <-applyDone:
		t.Fatalf("Apply ran while holder owned the plan's lock (LockID lost in replan?): err=%v", err)
	case <-time.After(500 * time.Millisecond):
		// Still blocked: correct.
	}

	var unlocked bool
	if err := holderConn.QueryRowContext(context.Background(), "SELECT pg_advisory_unlock($1);", customLockID).Scan(&unlocked); err != nil || !unlocked {
		t.Fatalf("holder unlock failed: err=%v unlocked=%v", err, unlocked)
	}
	if err := <-applyDone; err != nil {
		t.Fatalf("Apply failed after holder released: %v", err)
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
// errors.Is-detectable) while the schema changes remain applied. Uses a
// non-tx final group (CREATE INDEX CONCURRENTLY) so the history write runs
// in autocommit on the dedicated conn.
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

	// The migration itself must have been applied.
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
