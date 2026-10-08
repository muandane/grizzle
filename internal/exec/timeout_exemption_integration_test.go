//go:build integration

package exec_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/muandane/grizzle/internal/dialect/postgres"
	"github.com/muandane/grizzle/internal/exec"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/testutil"
)

// timeoutSample records the statement_timeout GUC observed by a step hook.
type timeoutSample struct {
	phase   string // "sync" or "apply"
	stepIdx int
	typ     plan.ChangeType
	sql     string
	isNonTx bool
	hook    string // "before" or "after"
	value   string // SHOW statement_timeout
}

func recordTimeoutSamples(phase string, samples *[]timeoutSample) (before, after exec.StepHook) {
	collect := func(hook string) exec.StepHook {
		return func(h exec.HookContext) error {
			var value string
			if err := h.DBTX.QueryRowContext(h.Context, "SHOW statement_timeout;").Scan(&value); err != nil {
				return fmt.Errorf("SHOW statement_timeout: %w", err)
			}
			*samples = append(*samples, timeoutSample{
				phase:   phase,
				stepIdx: h.Index,
				typ:     h.Step.Type,
				sql:     h.Step.SQL,
				isNonTx: h.IsNonTx,
				hook:    hook,
				value:   value,
			})
			return nil
		}
	}
	return collect("before"), collect("after")
}

func findSample(samples []timeoutSample, phase string, typ plan.ChangeType, hook string, sqlContains string) *timeoutSample {
	for i := range samples {
		s := &samples[i]
		if s.phase == phase && s.typ == typ && s.hook == hook && strings.Contains(s.sql, sqlContains) {
			return s
		}
	}
	return nil
}

// TestTimeoutExemption_NonTxAndValidate verifies statement_timeout handling in
// both execution paths:
//
//  1. NonTx steps (CREATE INDEX CONCURRENTLY) execute with statement_timeout
//     disabled ("0"), since a killed CIC leaves an INVALID index behind.
//  2. VALIDATE CONSTRAINT runs exempted from the timeout and the tx-local
//     timeout is RESTORED afterward, so later steps in the same transaction
//     remain protected (AfterStep of the validate step must show the configured
//     timeout, not "0").
//  3. Both SyncPostgres and PlanDiffPostgres+ApplyPostgres behave identically.
func TestTimeoutExemption_NonTxAndValidate(t *testing.T) {
	db := testutil.TestDatabase(t)
	ctx := context.Background()

	schema := fmt.Sprintf("test_tmout_ex_%d", time.Now().UnixNano())
	if _, err := db.Exec(fmt.Sprintf("CREATE SCHEMA %s;", schema)); err != nil {
		t.Fatalf("failed creating schema: %v", err)
	}
	defer func() { _, _ = db.Exec(fmt.Sprintf("DROP SCHEMA %s CASCADE;", schema)) }()

	// Initial table with rows: forces staged ADD ... NOT VALID + VALIDATE.
	initSQL := fmt.Sprintf(`
		CREATE TABLE %s.products (
			id INT PRIMARY KEY,
			price_cents INT NOT NULL,
			name TEXT NOT NULL
		);
		INSERT INTO %s.products (id, price_cents, name)
		VALUES (1, 100, 'widget'), (2, 250, 'gadget'), (3, 300, 'gizmo');
	`, schema, schema)
	if _, err := db.Exec(initSQL); err != nil {
		t.Fatalf("failed creating initial table: %v", err)
	}

	const wantTimeout = "2s" // SHOW rendering of StatementTimeout = 2s

	newCfg := func(desiredSQL string, samples *[]timeoutSample, phase string) exec.PostgresExecConfig {
		before, after := recordTimeoutSamples(phase, samples)
		return exec.PostgresExecConfig{
			TargetSchema: schema,
			ShadowSchema: fmt.Sprintf("_shadow_%s", schema),
			SchemaSQL:    desiredSQL,
			LockID:       postgres.GenerateLockID(schema),
			Policy:       plan.DropPolicy{},
			LockTimeout:  5 * time.Second,

			StatementTimeout: 2 * time.Second,
			BeforeStep:       before,
			AfterStep:        after,
		}
	}

	// ---- Phase A: SyncPostgres — adds CHECK + index on existing table ----
	var syncSamples []timeoutSample
	syncV2 := `
		CREATE TABLE products (
			id INT PRIMARY KEY,
			price_cents INT NOT NULL,
			name TEXT NOT NULL,
			CONSTRAINT check_positive_price CHECK (price_cents > 0)
		);
		CREATE INDEX idx_products_name ON products(name);
	`
	if err := exec.SyncPostgres(ctx, db, newCfg(syncV2, &syncSamples, "sync")); err != nil {
		t.Fatalf("SyncPostgres failed: %v", err)
	}

	assertTimeoutFlow(t, "sync", syncSamples, wantTimeout)

	// ---- Phase B: PlanDiffPostgres + ApplyPostgres — adds another CHECK + index ----
	var applySamples []timeoutSample
	applyV3 := `
		CREATE TABLE products (
			id INT PRIMARY KEY,
			price_cents INT NOT NULL,
			name TEXT NOT NULL,
			CONSTRAINT check_positive_price CHECK (price_cents > 0),
			CONSTRAINT check_name_len CHECK (char_length(name) <= 100)
		);
		CREATE INDEX idx_products_name ON products(name);
		CREATE INDEX idx_products_price ON products(price_cents);
	`
	planCfg := newCfg(applyV3, &applySamples, "plan")
	p, err := exec.PlanDiffPostgres(ctx, db, planCfg)
	if err != nil {
		t.Fatalf("PlanDiffPostgres failed: %v", err)
	}
	applyCfg := newCfg(applyV3, &applySamples, "apply")
	if err := exec.ApplyPostgres(ctx, db, p, applyCfg); err != nil {
		t.Fatalf("ApplyPostgres failed: %v", err)
	}

	assertTimeoutFlow(t, "apply", applySamples, wantTimeout)

	// ---- Idempotency: third sync is a no-op ----
	if err := exec.SyncPostgres(ctx, db, newCfg(applyV3, &syncSamples, "sync3")); err != nil {
		t.Fatalf("expected idempotent no-op sync, got: %v", err)
	}
}

func assertTimeoutFlow(t *testing.T, phase string, samples []timeoutSample, wantTimeout string) {
	t.Helper()

	// 1. CIC ran with statement_timeout disabled.
	cic := findSample(samples, phase, plan.ChangeCreateIndex, "after", "CREATE INDEX")
	if cic == nil {
		t.Fatalf("[%s] no AfterStep sample for CREATE INDEX step; samples: %+v", phase, samples)
	}
	if !cic.isNonTx {
		t.Errorf("[%s] expected CREATE INDEX step to be NonTx", phase)
	}
	if cic.value != "0" {
		t.Errorf("[%s] CIC AfterStep statement_timeout = %q, want \"0\" (disabled)", phase, cic.value)
	}

	// 2. VALIDATE ran exempted; tx-local timeout restored right after.
	valBefore := findSample(samples, phase, plan.ChangeValidateConstraint, "before", "VALIDATE CONSTRAINT")
	valAfter := findSample(samples, phase, plan.ChangeValidateConstraint, "after", "VALIDATE CONSTRAINT")
	if valBefore == nil || valAfter == nil {
		t.Fatalf("[%s] missing VALIDATE CONSTRAINT hook samples (before=%v after=%v); samples: %+v", phase, valBefore, valAfter, samples)
	}
	if valBefore.value != wantTimeout {
		t.Errorf("[%s] VALIDATE BeforeStep statement_timeout = %q, want %q", phase, valBefore.value, wantTimeout)
	}
	if valAfter.value != wantTimeout {
		t.Errorf("[%s] VALIDATE AfterStep statement_timeout = %q, want %q (timeout must be restored after the scan)", phase, valAfter.value, wantTimeout)
	}

	// 3. A plain tx step (ADD ... NOT VALID) kept the configured timeout.
	notValid := findSample(samples, phase, plan.ChangeAddCheck, "after", "NOT VALID")
	if notValid == nil {
		t.Fatalf("[%s] no AfterStep sample for ADD CONSTRAINT NOT VALID step; samples: %+v", phase, samples)
	}
	if notValid.value != wantTimeout {
		t.Errorf("[%s] NOT VALID AfterStep statement_timeout = %q, want %q", phase, notValid.value, wantTimeout)
	}
}
