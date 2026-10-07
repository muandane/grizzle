package exec

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/muandane/grizzle/internal/dialect"
	"github.com/muandane/grizzle/internal/dialect/postgres"
	"github.com/muandane/grizzle/internal/dialect/sqlite"
	"github.com/muandane/grizzle/internal/diff"
	"github.com/muandane/grizzle/internal/history"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
	"github.com/muandane/grizzle/internal/scope"
)

// Tracer defines the interface for tracing Grizzle lifecycle events.
type Tracer interface {
	Start(ctx context.Context, spanName string) (context.Context, Span)
}

// Span represents an active trace span recorded by a Tracer.
type Span interface {
	End()
	RecordError(err error)
	SetAttribute(key string, value any)
}

// PostgresExecConfig specifies the execution options for PostgreSQL synchronization.
type PostgresExecConfig struct {
	TargetSchema         string
	TargetSchemas        []string
	ShadowSchema         string
	ShadowSchemas        []string
	SchemaSQL            string
	RolesSQL             string
	LockNamespace        string
	LockID               int64
	Filters              scope.Filters
	Policy               plan.DropPolicy
	AcceptHazards        []plan.HazardCode
	ExpectedHash         string
	NonConcurrentIndexes bool
	LockTimeout          time.Duration
	StatementTimeout     time.Duration
	MaxRetries           int
	RandFloat            func() float64
	Logger               *slog.Logger
	Tracer               Tracer
	DryRun               bool
	Backfill             BackfillFunc
	BeforeSync           SyncHook
	AfterSync            SyncHook
	BeforeStep           StepHook
	AfterStep            StepHook
	// DryRunLockTimeout bounds lock waits for dry-run verification
	// (defaults to DefaultDryRunLockTimeout of 2s).
	DryRunLockTimeout time.Duration
	// ExecuteHooksInDryRun allows BeforeStep/AfterStep hooks to run inside the
	// dry-run sandbox. Defaults to false to avoid external side effects.
	ExecuteHooksInDryRun bool
}

func (cfg PostgresExecConfig) targetSchemas() []string {
	if len(cfg.TargetSchemas) > 0 {
		return cfg.TargetSchemas
	}
	if cfg.TargetSchema != "" {
		return []string{cfg.TargetSchema}
	}
	return []string{"public"}
}

func (cfg PostgresExecConfig) primarySchema() string {
	return cfg.targetSchemas()[0]
}

func searchPathSQL(schemas []string) string {
	var parts []string
	for _, s := range schemas {
		if err := postgres.ValidateIdentifier(s); err == nil {
			parts = append(parts, fmt.Sprintf("%q", s))
		}
	}
	parts = append(parts, "public")
	return fmt.Sprintf("SET search_path TO %s;", strings.Join(parts, ", "))
}

func localSearchPathSQL(schemas []string) string {
	var parts []string
	for _, s := range schemas {
		if err := postgres.ValidateIdentifier(s); err == nil {
			parts = append(parts, fmt.Sprintf("%q", s))
		}
	}
	parts = append(parts, "public")
	return fmt.Sprintf("SET LOCAL search_path TO %s;", strings.Join(parts, ", "))
}

// DiffPostgres computes the diff and renders the sequenced migration steps for PostgreSQL.
func DiffPostgres(ctx context.Context, dbtx dialect.DBTX, cfg PostgresExecConfig) ([]plan.Step, error) {
	if cfg.Tracer != nil {
		var diffSpan Span
		ctx, diffSpan = cfg.Tracer.Start(ctx, "grizzle.diff_plan")
		defer diffSpan.End()
	}
	var serverVersion int
	if row := dbtx.QueryRowContext(ctx, "SELECT current_setting('server_version_num')::integer;"); row != nil {
		_ = row.Scan(&serverVersion)
	}
	renderOpts := postgres.RenderOpts{
		NonConcurrentIndexes: cfg.NonConcurrentIndexes,
		ServerVersion:        serverVersion,
	}

	targetSchemas := cfg.targetSchemas()
	if len(targetSchemas) <= 1 {
		targetSchema := cfg.primarySchema()
		shadowSchema := cfg.ShadowSchema
		if shadowSchema == "" {
			shadowSchema = "_grizzle_shadow"
		}

		live, err := inspectPostgresSchema(ctx, dbtx, targetSchema, cfg.Tracer)
		if err != nil {
			return nil, fmt.Errorf("%w: live schema: %w", plan.ErrInspectionFailed, err)
		}

		desired, err := inspectPostgresSchema(ctx, dbtx, shadowSchema, cfg.Tracer)
		if err != nil {
			return nil, fmt.Errorf("%w: shadow schema: %w", plan.ErrInspectionFailed, err)
		}
		// Desired extensions are declared via CREATE EXTENSION statements,
		// which do not persist in the rolled-back shadow tx. Parse them
		// straight from SchemaSQL (authoritative desired set).
		desired.Extensions = schema.ParseExtensions(cfg.SchemaSQL)
		maskShadowExtensions(live, []string{shadowSchema})
		unmapShadowExprs(desired, []string{shadowSchema})

		changes, err := diff.Diff(live, desired, targetSchema, shadowSchema, cfg.Filters)
		if err != nil {
			return nil, err
		}
		steps := postgres.RenderChangesWithOpts(targetSchema, changes, renderOpts)
		rolesSteps, err := diffRolesSteps(ctx, dbtx, cfg)
		if err != nil {
			return nil, err
		}
		return append(steps, rolesSteps...), nil
	}

	// Multi-schema diffing
	shadowMap := postgres.ComputeShadowSchemas(cfg.ShadowSchema, targetSchemas)
	shadowToTarget := make(map[string]string, len(shadowMap))
	var shadowSchemas []string
	for target, shadow := range shadowMap {
		shadowToTarget[shadow] = target
		shadowSchemas = append(shadowSchemas, shadow)
	}

	liveMap, err := inspectPostgresSchemas(ctx, dbtx, targetSchemas, cfg.Tracer)
	if err != nil {
		return nil, fmt.Errorf("%w: live schemas: %w", plan.ErrInspectionFailed, err)
	}

	desiredMap, err := inspectPostgresSchemas(ctx, dbtx, shadowSchemas, cfg.Tracer)
	if err != nil {
		return nil, fmt.Errorf("%w: shadow schemas: %w", plan.ErrInspectionFailed, err)
	}
	// Extensions are database-wide: inject the parsed desired set into the
	// primary schema only so multi-schema diffs do not duplicate steps.
	if primary := desiredMap[shadowMap[cfg.primarySchema()]]; primary != nil {
		primary.Extensions = schema.ParseExtensions(cfg.SchemaSQL)
	}
	shadowNames := slices.Collect(maps.Values(shadowMap))
	for _, live := range liveMap {
		maskShadowExtensions(live, shadowNames)
	}
	for _, desired := range desiredMap {
		unmapShadowExprs(desired, shadowNames)
	}

	var allChanges []diff.Change
	for _, target := range targetSchemas {
		shadow := shadowMap[target]
		live := liveMap[target]
		desired := desiredMap[shadow]
		changes, err := diff.DiffWithMappings(live, desired, target, shadow, cfg.Filters, shadowToTarget)
		if err != nil {
			return nil, err
		}
		allChanges = append(allChanges, changes...)
	}

	steps := postgres.RenderChangesWithOpts(targetSchemas[0], allChanges, renderOpts)
	rolesSteps, err := diffRolesSteps(ctx, dbtx, cfg)
	if err != nil {
		return nil, err
	}
	return append(steps, rolesSteps...), nil
}

// maskShadowExtensions removes shadow-installed extensions from a live
// schema snapshot. The shadow compile best-effort installs CREATE EXTENSION
// into the shadow namespace inside the rolled-back compile tx; pg_extension
// is database-wide, so without masking the diff would treat those ephemeral
// installs as already-satisfied live state.
func maskShadowExtensions(live *schema.Schema, shadowSchemas []string) {
	if live == nil || len(live.Extensions) == 0 || len(shadowSchemas) == 0 {
		return
	}
	shadowSet := make(map[string]bool, len(shadowSchemas))
	for _, s := range shadowSchemas {
		shadowSet[s] = true
	}
	for name, ext := range live.Extensions {
		if ext != nil && shadowSet[ext.Schema] {
			delete(live.Extensions, name)
		}
	}
}

// unmapShadowExprs strips ephemeral shadow-schema qualifications from column
// default and generated expressions. Functions re-installed into the shadow
// schema (e.g. extension functions captured via WITH SCHEMA rewriting) leave
// `_grizzle_shadow_<id>.fn(...)` in pg_get_expr output; the shadow schema is
// unique per run and never exists in the target, so left in place they break
// rendered DDL and cause permanent drift. Stripping lets the reference
// resolve through the apply-time search_path.
func unmapShadowExprs(s *schema.Schema, shadowSchemas []string) {
	if s == nil || len(shadowSchemas) == 0 {
		return
	}
	for _, t := range s.Tables {
		for _, c := range t.Columns {
			for _, shadow := range shadowSchemas {
				if shadow == "" {
					continue
				}
				c.DefaultValue = strings.ReplaceAll(c.DefaultValue, shadow+".", "")
				c.DefaultValue = strings.ReplaceAll(c.DefaultValue, `"`+shadow+`".`, "")
				if c.Generated != nil {
					c.Generated.Expr = strings.ReplaceAll(c.Generated.Expr, shadow+".", "")
					c.Generated.Expr = strings.ReplaceAll(c.Generated.Expr, `"`+shadow+`".`, "")
				}
			}
		}
	}
}

func inspectPostgresSchema(ctx context.Context, dbtx dialect.DBTX, schema string, tracer Tracer) (*schema.Schema, error) {
	if tracer != nil {
		var span Span
		ctx, span = tracer.Start(ctx, "grizzle.inspect_schema")
		span.SetAttribute("schema", schema)
		defer span.End()
	}
	return postgres.Inspect(ctx, dbtx, schema)
}

func inspectPostgresSchemas(ctx context.Context, dbtx dialect.DBTX, schemas []string, tracer Tracer) (map[string]*schema.Schema, error) {
	if tracer != nil {
		var span Span
		ctx, span = tracer.Start(ctx, "grizzle.inspect_schema")
		span.SetAttribute("schemas", strings.Join(schemas, ","))
		defer span.End()
	}
	return postgres.InspectSchemas(ctx, dbtx, schemas)
}

func execStepWithTracing(ctx context.Context, execer dialect.DBTX, s plan.Step, isNonTx bool, tracer Tracer) error {
	var span Span
	if tracer != nil {
		_, span = tracer.Start(ctx, "grizzle.exec_step")
		span.SetAttribute("step.type", string(s.Type))
		span.SetAttribute("step.table", s.Table)
		span.SetAttribute("step.sql", s.SQL)
		span.SetAttribute("step.non_tx", isNonTx)
	}
	_, err := execer.ExecContext(ctx, s.SQL)
	if span != nil {
		if err != nil {
			span.RecordError(err)
		}
		span.End()
	}
	return err
}

// StepGroup partitions contiguous steps into transactional and non-transactional execution batches.
type StepGroup struct {
	NonTx bool
	Steps []plan.Step
}

// GroupSteps partitions migration steps into contiguous batches based on transactional requirement.
// Non-transactional steps run standalone. VALIDATE CONSTRAINT steps run in separate transactions
// after ADD ... NOT VALID steps commit, releasing ACCESS EXCLUSIVE table locks.
func GroupSteps(steps []plan.Step) []StepGroup {
	var groups []StepGroup
	for _, s := range steps {
		isNewGroup := len(groups) == 0 ||
			groups[len(groups)-1].NonTx != s.NonTx ||
			s.Type == plan.ChangeValidateConstraint ||
			(len(groups) > 0 && len(groups[len(groups)-1].Steps) > 0 && groups[len(groups)-1].Steps[len(groups[len(groups)-1].Steps)-1].Type == plan.ChangeValidateConstraint)

		if isNewGroup {
			groups = append(groups, StepGroup{NonTx: s.NonTx, Steps: []plan.Step{s}})
		} else {
			groups[len(groups)-1].Steps = append(groups[len(groups)-1].Steps, s)
		}
	}
	return groups
}

// lockBudget bounds the TOTAL advisory-lock acquisition wait across retry
// attempts, including backoff. Only lock waiting consumes the budget:
// preamble work (hooks, schema setup, session timeout statements) and DDL
// execution are not lock waits, so each attempt's acquisition timer is armed
// only when acquisition begins. A slow BeforeSync hook or a retryable DDL
// lock failure therefore cannot starve a later attempt's acquisition window.
type lockBudgetTracker struct {
	budget time.Duration
	spent  time.Duration
}

// remaining returns the budget left for the current attempt's acquisition.
func (t *lockBudgetTracker) remaining() time.Duration {
	return max(t.budget-t.spent, 0)
}

// recordWait charges a measured acquisition wait (successful or not) to the
// budget.
func (t *lockBudgetTracker) recordWait(d time.Duration) {
	t.spent += d
}

// reserve charges a pending backoff interval to the budget, reporting whether
// the budget could accommodate it; false means the campaign must stop.
func (t *lockBudgetTracker) reserve(d time.Duration) bool {
	if t.spent+d >= t.budget {
		return false
	}
	t.spent += d
	return true
}

// SyncPostgres synchronizes PostgreSQL with session-level advisory locking, timeouts, and retry logic.
func SyncPostgres(ctx context.Context, db *sql.DB, cfg PostgresExecConfig) error {
	maxRetries := max(cfg.MaxRetries, 0)

	// LockTimeout bounds the TOTAL advisory-lock acquisition wait across
	// retries, including backoff. It is deliberately NOT overwritten here:
	// the same value feeds ApplySessionTimeouts, which sets the DDL
	// lock_timeout on every attempt.
	lockBudget := cfg.LockTimeout
	if lockBudget <= 0 {
		lockBudget = DefaultLockTimeout
	}
	budget := &lockBudgetTracker{budget: lockBudget}

	attempt := 0
	for {
		committed, err := syncPostgresOnce(ctx, db, cfg, budget)
		if err == nil {
			if cfg.Backfill != nil && cfg.Filters.ExpandContract {
				if err := RunBackfill(ctx, db, cfg.TargetSchema, cfg.Filters.Renames, nil, cfg.Backfill, cfg.Logger); err != nil {
					return err
				}
			}
			return nil
		}

		// Only retry before any step has committed. After partial progress: abort, require re-plan.
		if committed > 0 || !IsRetryable(err) || attempt >= maxRetries {
			return err
		}

		attempt++
		backoff := ComputeBackoff(attempt, cfg.RandFloat)

		// The budget covers total lock waiting: stop if waiting the backoff
		// would exhaust it.
		if !budget.reserve(backoff) {
			return err
		}

		if cfg.Logger != nil {
			cfg.Logger.WarnContext(ctx, "grizzle: lock timeout encountered before progress, retrying migration",
				"attempt", attempt,
				"max_retries", maxRetries,
				"backoff_ms", backoff.Milliseconds(),
				"error", err,
			)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled during lock retry: %w", ctx.Err())
		case <-time.After(backoff):
		}
	}
}

func syncPostgresOnce(ctx context.Context, db *sql.DB, cfg PostgresExecConfig, budget *lockBudgetTracker) (int, error) {
	start := time.Now()
	logger := cfg.Logger
	targetSchemas := cfg.targetSchemas()
	primarySchema := cfg.primarySchema()
	isMulti := len(targetSchemas) > 1

	// Ephemeral per-run shadow prefix: never trust a persisted artifact name.
	// uniqueShadowName keeps concurrent Sync/PlanDiff callers collision-free
	// within PostgreSQL's 63-byte identifier limit.
	userShadow := cmp.Or(cfg.ShadowSchema, "_grizzle_shadow")
	cfg.ShadowSchema = uniqueShadowName(userShadow)

	if logger != nil {
		logger.InfoContext(ctx, "grizzle: starting schema synchronization", "target_schemas", targetSchemas)
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("grizzle: failed to acquire connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// BeforeSync runs once before any migration steps or locks are executed.
	if err := callBeforeSync(cfg.BeforeSync, ctx, conn); err != nil {
		hookErr := fmt.Errorf("grizzle: before_sync hook: %w", err)
		if logger != nil {
			logger.ErrorContext(ctx, "grizzle: hook failed", "error", hookErr)
		}
		return 0, hookErr
	}

	for _, s := range targetSchemas {
		if err := postgres.ValidateIdentifier(s); err == nil && s != "public" {
			_, _ = conn.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %q;", s))
		}
	}
	_, _ = conn.ExecContext(ctx, searchPathSQL(targetSchemas))

	// Apply session-level timeouts
	_ = ApplySessionTimeouts(ctx, conn, cfg.LockTimeout, cfg.StatementTimeout)

	// 1. Acquire session-level advisory lock on dedicated connection. The
	// timer is armed HERE, not at wrapper start: the preamble above (hooks,
	// schema setup, session timeouts) is not a lock wait and must not consume
	// the acquisition budget. DDL lock_timeout still keeps cfg.LockTimeout
	// via ApplySessionTimeouts.
	lockCtx, cancelLock := context.WithTimeout(ctx, budget.remaining())
	lockStart := time.Now()
	var lockSpan Span
	if cfg.Tracer != nil {
		_, lockSpan = cfg.Tracer.Start(ctx, "grizzle.acquire_lock")
	}
	var acquiredSchemas []string
	if cfg.LockID != 0 {
		err = postgres.AcquireSessionAdvisoryLock(lockCtx, conn, cfg.LockID)
	} else {
		lockNs := cmp.Or(cfg.LockNamespace, "grizzle")
		acquiredSchemas, err = postgres.AcquireSchemaLocks(lockCtx, conn, lockNs, targetSchemas)
	}
	cancelLock()
	budget.recordWait(time.Since(lockStart))
	if lockSpan != nil {
		if err != nil {
			lockSpan.RecordError(err)
		}
		lockSpan.End()
	}
	if err != nil {
		if logger != nil {
			logger.ErrorContext(ctx, "grizzle: lock acquisition failed", "schemas", targetSchemas, "lock_id", cfg.LockID, "error", err)
		}
		return 0, fmt.Errorf("%w: %w", plan.ErrLockAcquisition, err)
	}
	defer releasePostgresConn(ctx, conn, func(relCtx context.Context) error {
		if cfg.LockID != 0 {
			return postgres.ReleaseSessionAdvisoryLock(relCtx, conn, cfg.LockID)
		}
		lockNs := cmp.Or(cfg.LockNamespace, "grizzle")
		return postgres.ReleaseSchemaLocks(relCtx, conn, lockNs, acquiredSchemas)
	})

	if logger != nil {
		if cfg.LockID != 0 {
			logger.DebugContext(ctx, "grizzle: acquired session advisory lock", "lock_id", cfg.LockID)
		} else {
			logger.DebugContext(ctx, "grizzle: acquired session advisory locks", "schemas", acquiredSchemas, "namespace", cfg.LockNamespace)
		}
	}

	// 2. Setup shadow schema and diff schemas inside an isolated transaction
	shadowStart := time.Now()
	shadowTx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("grizzle: failed to begin shadow tx: %w", err)
	}
	defer func() { _ = shadowTx.Rollback() }()

	_ = ApplyTxTimeouts(ctx, shadowTx, cfg.LockTimeout, cfg.StatementTimeout)

	var shadowSchemas []string
	if !isMulti {
		shadowSchema := cfg.ShadowSchema
		if shadowSchema == "" {
			shadowSchema = "_grizzle_shadow"
		}
		shadowSchemas = []string{shadowSchema}

		if err := postgres.SetupShadowSchema(ctx, shadowTx, shadowSchema); err != nil {
			return 0, err
		}

		if err := postgres.RunShadowDDL(ctx, shadowTx, shadowSchema, primarySchema, cfg.SchemaSQL); err != nil {
			if logger != nil {
				logger.ErrorContext(ctx, "grizzle: shadow compilation failed", "error", err)
			}
			return 0, fmt.Errorf("%w: %w", plan.ErrCompilationFailed, err)
		}
	} else {
		shadowMap := postgres.ComputeShadowSchemas(cfg.ShadowSchema, targetSchemas)
		for _, shadow := range shadowMap {
			shadowSchemas = append(shadowSchemas, shadow)
		}

		if err := postgres.SetupShadowSchemas(ctx, shadowTx, shadowSchemas); err != nil {
			return 0, err
		}

		if err := postgres.RunMultiShadowDDL(ctx, shadowTx, shadowMap, targetSchemas, cfg.SchemaSQL); err != nil {
			if logger != nil {
				logger.ErrorContext(ctx, "grizzle: shadow compilation failed", "error", err)
			}
			return 0, fmt.Errorf("%w: %w", plan.ErrCompilationFailed, err)
		}
	}
	if logger != nil {
		logger.DebugContext(ctx, "grizzle: shadow compilation succeeded", "duration", time.Since(shadowStart))
	}

	// 3. Diff schemas post-lock
	steps, err := DiffPostgres(ctx, shadowTx, cfg)
	if err != nil {
		return 0, err
	}
	_ = shadowTx.Rollback()

	p := &plan.Plan{
		TargetSchema:   primarySchema,
		TargetSchemas:  targetSchemas,
		Steps:          steps,
		Policy:         cfg.Policy,
		IncludeTables:  cfg.Filters.Includes,
		ExcludeTables:  cfg.Filters.Excludes,
		Renames:        cfg.Filters.Renames,
		ExpandContract: cfg.Filters.ExpandContract,
		SchemaSQL:      cfg.SchemaSQL,
		RolesSQL:       cfg.RolesSQL,
	}

	// 4b. Verify expected plan hash if provided (aborts with ErrPlanDrift on mismatch)
	if cfg.ExpectedHash != "" && p.Hash() != cfg.ExpectedHash {
		if logger != nil {
			logger.WarnContext(ctx, "grizzle: plan drift detected post-lock", "expected", cfg.ExpectedHash, "actual", p.Hash())
		}
		return 0, fmt.Errorf("%w: expected hash %q, actual post-lock hash %q", plan.ErrPlanDrift, cfg.ExpectedHash, p.Hash())
	}

	if len(steps) == 0 {
		if logger != nil {
			logger.InfoContext(ctx, "grizzle: schema is already in sync", "duration", time.Since(start))
		}
		_ = postgres.DropShadowSchemas(ctx, conn, shadowSchemas)
		return 0, nil
	}

	if logger != nil {
		logger.InfoContext(ctx, "grizzle: computed migration plan", "steps_count", len(steps))
	}

	// 5. Enforce safety policy (policy check hard-blocks drops regardless of AcceptHazards)
	if err := p.ValidatePolicy(); err != nil {
		if logger != nil {
			if vErr, ok := errors.AsType[*plan.DestructiveViolationError](err); ok {
				logger.WarnContext(ctx, "grizzle: migration blocked by safety policy", "violations_count", len(vErr.Violations))
			} else {
				logger.WarnContext(ctx, "grizzle: migration blocked by safety policy", "error", err)
			}
		}
		return 0, err
	}

	// 5b. Enforce hazard gating (fails on critical hazards unless explicitly accepted)
	if err := p.ValidateHazards(cfg.AcceptHazards); err != nil {
		if logger != nil {
			logger.WarnContext(ctx, "grizzle: migration blocked by unaccepted critical hazards", "error", err)
		}
		return 0, err
	}

	if cfg.DryRun {
		if logger != nil {
			logger.InfoContext(ctx, "grizzle: dry-run mode, skipping statement execution")
		}
		_ = postgres.DropShadowSchemas(ctx, conn, shadowSchemas)
		return 0, nil
	}

	// 6. Cleanup shadow schema before live execution
	if err := postgres.DropShadowSchemas(ctx, conn, shadowSchemas); err != nil {
		return 0, err
	}

	// 7. Apply DDL statements split into transactional and non-transactional groups
	groups := GroupSteps(steps)
	stepIdx := 0
	committedSteps := 0
	// histErr captures a failed success-path history write: reported as a
	// typed non-fatal error after the migration completes (plan.ErrHistoryRecord).
	var histErr error

	recordFailureHistory := func(failedStep int, execErr error, isNonTx bool) {
		status := "failed"
		if committedSteps > 0 || isNonTx {
			status = "partial"
		}
		// Write using fresh connection and detached context not cancelled by the failure
		histCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		histConn, err := db.Conn(histCtx)
		if err == nil {
			defer func() { _ = histConn.Close() }()
			if rErr := history.RecordProgress(histCtx, histConn, "postgres", primarySchema, p, status, failedStep, execErr, time.Since(start)); rErr != nil && logger != nil {
				logger.WarnContext(histCtx, "grizzle: failed recording failure history", "status", status, "error", rErr)
			}
		}
	}

	for groupIdx, group := range groups {
		isLastGroup := groupIdx == len(groups)-1
		if group.NonTx {
			// Non-transactional steps (e.g. CREATE INDEX CONCURRENTLY) executed directly on dedicated conn
			for _, s := range group.Steps {
				stepIdx++
				stepStart := time.Now()
				if err := callBeforeStep(cfg.BeforeStep, HookContext{Context: ctx, DBTX: conn, Step: s, Index: stepIdx, Total: len(steps), IsNonTx: true}); err != nil {
					hookErr := fmt.Errorf("before_step hook: %w", err)
					if logger != nil {
						logger.ErrorContext(ctx, "grizzle: hook failed", "step_index", stepIdx, "error", hookErr)
					}
					// No DDL ran yet; status follows committedSteps only (not isNonTx).
					recordFailureHistory(stepIdx, hookErr, false)
					return committedSteps, hookErr
				}
				if err := execStepWithTracing(ctx, conn, s, true, cfg.Tracer); err != nil {
					if logger != nil {
						logger.ErrorContext(ctx, "grizzle: failed executing non-tx step", "step_index", stepIdx, "sql", s.SQL, "error", err)
					}
					recordFailureHistory(stepIdx, err, true)
					return committedSteps + 1, wrapStepExecError(s, err, true)
				}
				committedSteps++
				if err := callAfterStep(cfg.AfterStep, HookContext{Context: ctx, DBTX: conn, Step: s, Index: stepIdx, Total: len(steps), IsNonTx: true}); err != nil {
					hookErr := fmt.Errorf("after_step hook: %w", err)
					if logger != nil {
						logger.ErrorContext(ctx, "grizzle: hook failed", "step_index", stepIdx, "error", hookErr)
					}
					recordFailureHistory(stepIdx, hookErr, true)
					return committedSteps, hookErr
				}
				if logger != nil {
					logger.DebugContext(ctx, "grizzle: executed non-tx step", "step_index", stepIdx, "type", s.Type, "table", s.Table, "duration", time.Since(stepStart))
				}
			}
			if isLastGroup {
				if err := history.RecordPlan(ctx, conn, "postgres", primarySchema, p, time.Since(start)); err != nil {
					histErr = fmt.Errorf("%w: %w", plan.ErrHistoryRecord, err)
					if logger != nil {
						logger.ErrorContext(ctx, "grizzle: failed recording history on conn", "error", err)
					}
				}
			}
		} else {
			// Transactional group executed in transaction on dedicated conn
			tx, err := conn.BeginTx(ctx, nil)
			if err != nil {
				return committedSteps, fmt.Errorf("grizzle: failed to begin step transaction: %w", err)
			}
			_, _ = tx.ExecContext(ctx, localSearchPathSQL(targetSchemas))
			_ = ApplyTxTimeouts(ctx, tx, cfg.LockTimeout, cfg.StatementTimeout)

			for _, s := range group.Steps {
				stepIdx++
				stepStart := time.Now()
				if err := callBeforeStep(cfg.BeforeStep, HookContext{Context: ctx, DBTX: tx, Step: s, Index: stepIdx, Total: len(steps), IsNonTx: false}); err != nil {
					_ = tx.Rollback()
					hookErr := fmt.Errorf("before_step hook: %w", err)
					if logger != nil {
						logger.ErrorContext(ctx, "grizzle: hook failed", "step_index", stepIdx, "error", hookErr)
					}
					recordFailureHistory(stepIdx, hookErr, false)
					return committedSteps, hookErr
				}
				if err := execStepWithTracing(ctx, tx, s, false, cfg.Tracer); err != nil {
					_ = tx.Rollback()
					if logger != nil {
						logger.ErrorContext(ctx, "grizzle: failed executing step in tx", "step_index", stepIdx, "sql", s.SQL, "error", err)
					}
					recordFailureHistory(stepIdx, err, false)
					return committedSteps, wrapStepExecError(s, err, false)
				}
				if err := callAfterStep(cfg.AfterStep, HookContext{Context: ctx, DBTX: tx, Step: s, Index: stepIdx, Total: len(steps), IsNonTx: false}); err != nil {
					_ = tx.Rollback()
					hookErr := fmt.Errorf("after_step hook: %w", err)
					if logger != nil {
						logger.ErrorContext(ctx, "grizzle: hook failed", "step_index", stepIdx, "error", hookErr)
					}
					recordFailureHistory(stepIdx, hookErr, false)
					return committedSteps, hookErr
				}
				if logger != nil {
					logger.DebugContext(ctx, "grizzle: executed step in tx", "step_index", stepIdx, "type", s.Type, "table", s.Table, "duration", time.Since(stepStart))
				}
			}

			if err := tx.Commit(); err != nil {
				recordFailureHistory(stepIdx, err, false)
				return committedSteps, fmt.Errorf("grizzle: failed committing step transaction: %w", err)
			}
			committedSteps += len(group.Steps)
			// Model B: history is advisory — recorded after commit on the
			// dedicated connection so a history failure can never roll back
			// applied DDL.
			if isLastGroup {
				if err := history.RecordPlan(ctx, conn, "postgres", primarySchema, p, time.Since(start)); err != nil {
					histErr = fmt.Errorf("%w: %w", plan.ErrHistoryRecord, err)
					if logger != nil {
						logger.ErrorContext(ctx, "grizzle: failed recording history post-commit", "error", err)
					}
				}
			}
		}
	}

	if err := callAfterSync(cfg.AfterSync, ctx, conn); err != nil {
		hookErr := fmt.Errorf("%w: %w", plan.ErrAfterSyncFailed, err)
		if logger != nil {
			logger.ErrorContext(ctx, "grizzle: hook failed", "error", hookErr)
		}
		return committedSteps, hookErr
	}

	if logger != nil {
		logger.InfoContext(ctx, "grizzle: synchronization finished successfully", "steps_applied", len(steps), "total_duration", time.Since(start))
	}

	if histErr != nil {
		return committedSteps, histErr
	}
	return committedSteps, nil
}

// PlanDiffPostgres generates the plan for PostgreSQL without applying statements.
func PlanDiffPostgres(ctx context.Context, db *sql.DB, cfg PostgresExecConfig) (*plan.Plan, error) {
	targetSchemas := cfg.targetSchemas()
	primarySchema := cfg.primarySchema()
	isMulti := len(targetSchemas) > 1

	// Per-call unique shadow name: PlanDiff is a read-only drift check and
	// must not block behind (or interfere with) a concurrently running Sync
	// that holds the default shadow schema name. The compile transaction is
	// always rolled back, so the unique schema cannot leak.
	userShadowSchema := cmp.Or(cfg.ShadowSchema, "_grizzle_shadow")
	cfg.ShadowSchema = uniqueShadowName(userShadowSchema)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("grizzle: failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_ = ApplyTxTimeouts(ctx, tx, cfg.LockTimeout, cfg.StatementTimeout)

	var shadowSchemas []string
	if !isMulti {
		shadowSchema := cfg.ShadowSchema
		if shadowSchema == "" {
			shadowSchema = "_grizzle_shadow"
		}
		shadowSchemas = []string{shadowSchema}

		if err := postgres.SetupShadowSchema(ctx, tx, shadowSchema); err != nil {
			return nil, err
		}
		defer func() { _ = postgres.DropShadowSchema(context.Background(), tx, shadowSchema) }()

		if err := postgres.RunShadowDDL(ctx, tx, shadowSchema, primarySchema, cfg.SchemaSQL); err != nil {
			return nil, fmt.Errorf("%w: %w", plan.ErrCompilationFailed, err)
		}
	} else {
		shadowMap := postgres.ComputeShadowSchemas(cfg.ShadowSchema, targetSchemas)
		for _, shadow := range shadowMap {
			shadowSchemas = append(shadowSchemas, shadow)
		}

		if err := postgres.SetupShadowSchemas(ctx, tx, shadowSchemas); err != nil {
			return nil, err
		}
		defer func() { _ = postgres.DropShadowSchemas(context.Background(), tx, shadowSchemas) }()

		if err := postgres.RunMultiShadowDDL(ctx, tx, shadowMap, targetSchemas, cfg.SchemaSQL); err != nil {
			return nil, fmt.Errorf("%w: %w", plan.ErrCompilationFailed, err)
		}
	}

	steps, err := DiffPostgres(ctx, tx, cfg)
	if err != nil {
		return nil, err
	}

	return &plan.Plan{
		TargetSchema:         primarySchema,
		TargetSchemas:        targetSchemas,
		Steps:                steps,
		Policy:               cfg.Policy,
		IncludeTables:        cfg.Filters.Includes,
		ExcludeTables:        cfg.Filters.Excludes,
		Renames:              cfg.Filters.Renames,
		ExpandContract:       cfg.Filters.ExpandContract,
		SchemaSQL:            cfg.SchemaSQL,
		RolesSQL:             cfg.RolesSQL,
		LockTimeout:          cfg.LockTimeout,
		StatementTimeout:     cfg.StatementTimeout,
		NonConcurrentIndexes: cfg.NonConcurrentIndexes,
	}, nil
}

// planShadowSuffix is a process-local counter ensuring concurrent PlanDiff
// calls never share a shadow schema name.
var planShadowSuffix atomic.Uint64

// uniqueShadowName derives a per-call shadow schema name that is unique
// across concurrent callers while staying inside PostgreSQL's 63-byte
// identifier limit (NAMEDATALEN). A longer name would be silently truncated
// by the server, making setup, introspection, and drop disagree on the
// schema name. The suffix is a short hash of pid, timestamp, and a process
// counter, so concurrent calls collide with probability ~2^-32 per pair.
// The result stays a valid SQL identifier.
func uniqueShadowName(base string) string {
	h := fnv.New32a()
	seed := uint64(time.Now().UnixNano()) ^ uint64(os.Getpid())<<32 ^ planShadowSuffix.Add(1)
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], seed)
	_, _ = h.Write(buf[:])
	if len(base) > 54 { // reserve room for the 9-char suffix within 63 bytes
		base = base[:54]
	}
	return fmt.Sprintf("%s_%08x", base, h.Sum32())
}

// SQLiteExecConfig specifies the execution options for SQLite synchronization.
type SQLiteExecConfig struct {
	SchemaSQL            string
	Filters              scope.Filters
	Policy               plan.DropPolicy
	AcceptHazards        []plan.HazardCode
	ExpectedHash         string
	Logger               *slog.Logger
	Tracer               Tracer
	DryRun               bool
	Backfill             BackfillFunc
	BeforeSync           SyncHook
	AfterSync            SyncHook
	BeforeStep           StepHook
	AfterStep            StepHook
	DryRunLockTimeout    time.Duration
	ExecuteHooksInDryRun bool

	RebuildThreshold int
	RebuildBatchSize int
}

// SyncSQLite synchronizes SQLite in a single transaction with foreign keys handling.
func SyncSQLite(ctx context.Context, db *sql.DB, cfg SQLiteExecConfig) error {
	start := time.Now()
	logger := cfg.Logger
	if logger != nil {
		logger.InfoContext(ctx, "sqlite: starting SQLite schema synchronization")
	}

	// 1. Compile in shadow database
	desired, err := sqlite.CompileInShadow(ctx, cfg.SchemaSQL)
	if err != nil {
		if logger != nil {
			logger.ErrorContext(ctx, "sqlite: shadow compilation failed", "error", err)
		}
		return fmt.Errorf("%w: %w", plan.ErrCompilationFailed, err)
	}

	// 2. Introspect live schema
	live, err := sqlite.Inspect(ctx, db)
	if err != nil {
		return fmt.Errorf("%w: %w", plan.ErrInspectionFailed, err)
	}

	// 3. Diff schemas
	steps := sqlite.Diff(live, desired, cfg.Filters)

	p := &plan.Plan{
		TargetSchema:   "main",
		Steps:          steps,
		Policy:         cfg.Policy,
		IncludeTables:  cfg.Filters.Includes,
		ExcludeTables:  cfg.Filters.Excludes,
		Renames:        cfg.Filters.Renames,
		ExpandContract: cfg.Filters.ExpandContract,
		SchemaSQL:      cfg.SchemaSQL,
	}

	// 3b. Verify expected plan hash if provided (aborts with ErrPlanDrift on mismatch)
	if cfg.ExpectedHash != "" && p.Hash() != cfg.ExpectedHash {
		if logger != nil {
			logger.WarnContext(ctx, "sqlite: plan drift detected post-lock", "expected", cfg.ExpectedHash, "actual", p.Hash())
		}
		return fmt.Errorf("%w: expected hash %q, actual post-lock hash %q", plan.ErrPlanDrift, cfg.ExpectedHash, p.Hash())
	}

	if len(steps) == 0 {
		if logger != nil {
			logger.InfoContext(ctx, "sqlite: schema is already in sync", "duration", time.Since(start))
		}
		return nil
	}

	if logger != nil {
		logger.InfoContext(ctx, "sqlite: computed migration plan", "steps_count", len(steps))
	}

	// 4. Enforce safety policy (policy check hard-blocks drops regardless of AcceptHazards)
	if err := p.ValidatePolicy(); err != nil {
		if logger != nil {
			if vErr, ok := errors.AsType[*plan.DestructiveViolationError](err); ok {
				logger.WarnContext(ctx, "sqlite: migration blocked by safety policy", "violations_count", len(vErr.Violations))
			} else {
				logger.WarnContext(ctx, "sqlite: migration blocked by safety policy", "error", err)
			}
		}
		return err
	}

	// 4b. Enforce hazard gating (fails on critical hazards unless explicitly accepted)
	if err := p.ValidateHazards(cfg.AcceptHazards); err != nil {
		if logger != nil {
			logger.WarnContext(ctx, "sqlite: migration blocked by unaccepted critical hazards", "error", err)
		}
		return err
	}

	if cfg.DryRun {
		return nil
	}

	// BeforeSync runs once before any migration steps are executed.
	// SQLite passes the *sql.DB handle because the entire sync is a single transaction.
	if err := callBeforeSync(cfg.BeforeSync, ctx, db); err != nil {
		hookErr := fmt.Errorf("sqlite: before_sync hook: %w", err)
		if logger != nil {
			logger.ErrorContext(ctx, "grizzle: hook failed", "error", hookErr)
		}
		return hookErr
	}

	// 5. Execute in a transaction pinned to a single connection with foreign
	// keys disabled (per-connection PRAGMA; a no-op inside a transaction).
	// histErr captures a failed success-path history write; surfaced as a
	// typed non-fatal error after the transaction commits.
	var histErr error
	err = runSQLiteWithForeignKeysOff(ctx, db, func(tx *sql.Tx, conn *sql.Conn) error {
		for i, s := range steps {
			stepStart := time.Now()
			sqlToExec := strings.TrimSpace(s.SQL)
			if sqlToExec == "" {
				continue
			}
			if err := callBeforeStep(cfg.BeforeStep, HookContext{Context: ctx, DBTX: tx, Step: s, Index: i + 1, Total: len(steps), IsNonTx: false}); err != nil {
				_ = tx.Rollback()
				hookErr := fmt.Errorf("before_step hook: %w", err)
				if logger != nil {
					logger.ErrorContext(ctx, "grizzle: hook failed", "step_index", i+1, "error", hookErr)
				}
				histCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if rErr := history.RecordProgress(histCtx, conn, "sqlite", "", p, "failed", i+1, hookErr, time.Since(start)); rErr != nil && logger != nil {
					logger.WarnContext(histCtx, "sqlite: failed recording failure history", "error", rErr)
				}
				return hookErr
			}
			if err := executeSQLiteStep(ctx, tx, s, cfg); err != nil {
				_ = tx.Rollback()
				if logger != nil {
					logger.ErrorContext(ctx, "sqlite: failed executing step", "step_index", i+1, "sql", sqlToExec, "error", err)
				}
				histCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if rErr := history.RecordProgress(histCtx, conn, "sqlite", "", p, "failed", i+1, err, time.Since(start)); rErr != nil && logger != nil {
					logger.WarnContext(histCtx, "sqlite: failed recording failure history", "error", rErr)
				}
				return fmt.Errorf("%w: failed executing [%s]: %w", plan.ErrExecutionFailed, sqlToExec, err)
			}
			if err := callAfterStep(cfg.AfterStep, HookContext{Context: ctx, DBTX: tx, Step: s, Index: i + 1, Total: len(steps), IsNonTx: false}); err != nil {
				_ = tx.Rollback()
				hookErr := fmt.Errorf("after_step hook: %w", err)
				if logger != nil {
					logger.ErrorContext(ctx, "grizzle: hook failed", "step_index", i+1, "error", hookErr)
				}
				histCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if rErr := history.RecordProgress(histCtx, conn, "sqlite", "", p, "failed", i+1, hookErr, time.Since(start)); rErr != nil && logger != nil {
					logger.WarnContext(histCtx, "sqlite: failed recording failure history", "error", rErr)
				}
				return hookErr
			}
			if logger != nil {
				logger.DebugContext(ctx, "sqlite: executed step", "step_index", i+1, "type", s.Type, "table", s.Table, "duration", time.Since(stepStart))
			}
		}

		finalFKRows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check;")
		if err != nil {
			return fmt.Errorf("sqlite: foreign key check failed: %w", err)
		}
		var fkViolations []string
		for finalFKRows.Next() {
			var vTbl, vParent string
			var vRowID, vFKID int64
			if err := finalFKRows.Scan(&vTbl, &vRowID, &vParent, &vFKID); err == nil {
				fkViolations = append(fkViolations, fmt.Sprintf("table %q row %d -> %q", vTbl, vRowID, vParent))
			}
		}
		_ = finalFKRows.Close()
		if len(fkViolations) > 0 {
			return fmt.Errorf("sqlite: foreign key constraint violation: %s", strings.Join(fkViolations, "; "))
		}
		// Model B: do not record history inside the migration transaction.
		return nil
	})
	if err != nil {
		return err
	}
	// Model B: history is advisory — recorded after DDL commit.
	if err := history.RecordPlan(ctx, db, "sqlite", "", p, time.Since(start)); err != nil {
		histErr = fmt.Errorf("%w: %w", plan.ErrHistoryRecord, err)
		if logger != nil {
			logger.ErrorContext(ctx, "sqlite: failed recording history post-commit", "error", err)
		}
	}
	if histErr != nil {
		return histErr
	}

	if cfg.Backfill != nil && cfg.Filters.ExpandContract {
		if err := RunBackfill(ctx, db, "main", cfg.Filters.Renames, steps, cfg.Backfill, cfg.Logger); err != nil {
			return err
		}
	}

	if err := callAfterSync(cfg.AfterSync, ctx, db); err != nil {
		hookErr := fmt.Errorf("%w: %w", plan.ErrAfterSyncFailed, err)
		if logger != nil {
			logger.ErrorContext(ctx, "grizzle: hook failed", "error", hookErr)
		}
		return hookErr
	}

	if logger != nil {
		logger.InfoContext(ctx, "sqlite: synchronization finished successfully", "steps_applied", len(steps), "total_duration", time.Since(start))
	}

	return nil
}

// PlanDiffSQLite generates the migration plan for SQLite without applying it.
func PlanDiffSQLite(ctx context.Context, db *sql.DB, cfg SQLiteExecConfig) (*plan.Plan, error) {
	desired, err := sqlite.CompileInShadow(ctx, cfg.SchemaSQL)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", plan.ErrCompilationFailed, err)
	}

	live, err := sqlite.Inspect(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", plan.ErrInspectionFailed, err)
	}

	steps := sqlite.Diff(live, desired, cfg.Filters)

	return &plan.Plan{
		TargetSchema:   "main",
		Steps:          steps,
		Policy:         cfg.Policy,
		IncludeTables:  cfg.Filters.Includes,
		ExcludeTables:  cfg.Filters.Excludes,
		Renames:        cfg.Filters.Renames,
		ExpandContract: cfg.Filters.ExpandContract,
		SchemaSQL:      cfg.SchemaSQL,
	}, nil
}

// ApplyPostgres applies a precomputed plan to PostgreSQL using a dedicated connection,
// session-level advisory locking, step grouping (NonTx vs Tx), timeouts, and retry logic.
func ApplyPostgres(ctx context.Context, db *sql.DB, p *plan.Plan, cfg PostgresExecConfig) error {
	if p == nil {
		return fmt.Errorf("grizzle: plan cannot be nil")
	}

	// 1. Enforce safety policy (policy check hard-blocks drops regardless of AcceptHazards)
	if err := p.ValidatePolicy(); err != nil {
		if cfg.Logger != nil {
			if vErr, ok := errors.AsType[*plan.DestructiveViolationError](err); ok {
				cfg.Logger.WarnContext(ctx, "grizzle: migration blocked by safety policy", "violations_count", len(vErr.Violations))
			} else {
				cfg.Logger.WarnContext(ctx, "grizzle: migration blocked by safety policy", "error", err)
			}
		}
		return err
	}

	// 1b. Enforce hazard gating (fails on critical hazards unless explicitly accepted)
	if err := p.ValidateHazards(cfg.AcceptHazards); err != nil {
		if cfg.Logger != nil {
			cfg.Logger.WarnContext(ctx, "grizzle: migration blocked by unaccepted critical hazards", "error", err)
		}
		return err
	}

	if cfg.DryRun || len(p.Steps) == 0 {
		return nil
	}

	maxRetries := max(cfg.MaxRetries, 0)

	// LockTimeout bounds the TOTAL advisory-lock acquisition wait across
	// retries, including backoff (see SyncPostgres). cfg.LockTimeout itself
	// is never overwritten: it still feeds ApplySessionTimeouts for the DDL
	// lock_timeout on every attempt.
	lockBudget := cfg.LockTimeout
	if lockBudget <= 0 {
		lockBudget = DefaultLockTimeout
	}
	budget := &lockBudgetTracker{budget: lockBudget}

	attempt := 0

	for {
		committed, err := applyPostgresOnce(ctx, db, p, cfg, budget)
		if err == nil {
			if cfg.Backfill != nil && p.ExpandContract {
				if err := RunBackfill(ctx, db, cfg.TargetSchema, p.Renames, nil, cfg.Backfill, cfg.Logger); err != nil {
					return err
				}
			}
			return nil
		}

		// Only retry before any step has committed. After partial progress: abort, require re-plan.
		if committed > 0 || !IsRetryable(err) || attempt >= maxRetries {
			return err
		}

		attempt++
		backoff := ComputeBackoff(attempt, cfg.RandFloat)

		// The budget covers total lock waiting: stop if waiting the backoff
		// would exhaust it.
		if !budget.reserve(backoff) {
			return err
		}

		if cfg.Logger != nil {
			cfg.Logger.WarnContext(ctx, "grizzle: lock timeout encountered before progress, retrying plan apply",
				"attempt", attempt,
				"max_retries", maxRetries,
				"backoff_ms", backoff.Milliseconds(),
				"error", err,
			)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled during lock retry: %w", ctx.Err())
		case <-time.After(backoff):
		}
	}
}

func applyPostgresOnce(ctx context.Context, db *sql.DB, p *plan.Plan, cfg PostgresExecConfig, budget *lockBudgetTracker) (int, error) {
	start := time.Now()
	logger := cfg.Logger
	targetSchemas := cfg.targetSchemas()
	primarySchema := cfg.primarySchema()

	if logger != nil {
		logger.InfoContext(ctx, "grizzle: starting plan execution", "target_schemas", targetSchemas, "steps_count", len(p.Steps))
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("grizzle: failed to acquire connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// BeforeSync runs once before any migration steps or locks are executed.
	if err := callBeforeSync(cfg.BeforeSync, ctx, conn); err != nil {
		hookErr := fmt.Errorf("grizzle: before_sync hook: %w", err)
		if logger != nil {
			logger.ErrorContext(ctx, "grizzle: hook failed", "error", hookErr)
		}
		return 0, hookErr
	}

	for _, s := range targetSchemas {
		if err := postgres.ValidateIdentifier(s); err == nil && s != "public" {
			_, _ = conn.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %q;", s))
		}
	}
	_, _ = conn.ExecContext(ctx, searchPathSQL(targetSchemas))

	// Apply session-level timeouts
	_ = ApplySessionTimeouts(ctx, conn, cfg.LockTimeout, cfg.StatementTimeout)

	// Acquire session-level advisory lock on dedicated connection. The timer
	// is armed HERE, not at wrapper start: the preamble above (hooks, schema
	// setup, session timeouts) is not a lock wait and must not consume the
	// acquisition budget. DDL lock_timeout still keeps cfg.LockTimeout via
	// ApplySessionTimeouts.
	lockCtx, cancelLock := context.WithTimeout(ctx, budget.remaining())
	lockStart := time.Now()
	var lockSpan Span
	if cfg.Tracer != nil {
		_, lockSpan = cfg.Tracer.Start(ctx, "grizzle.acquire_lock")
	}
	var acquiredSchemas []string
	if cfg.LockID != 0 {
		err = postgres.AcquireSessionAdvisoryLock(lockCtx, conn, cfg.LockID)
	} else {
		lockNs := cmp.Or(cfg.LockNamespace, "grizzle")
		acquiredSchemas, err = postgres.AcquireSchemaLocks(lockCtx, conn, lockNs, targetSchemas)
	}
	cancelLock()
	budget.recordWait(time.Since(lockStart))
	if lockSpan != nil {
		if err != nil {
			lockSpan.RecordError(err)
		}
		lockSpan.End()
	}
	if err != nil {
		if logger != nil {
			logger.ErrorContext(ctx, "grizzle: lock acquisition failed", "schemas", targetSchemas, "lock_id", cfg.LockID, "error", err)
		}
		return 0, fmt.Errorf("%w: %w", plan.ErrLockAcquisition, err)
	}
	defer releasePostgresConn(ctx, conn, func(relCtx context.Context) error {
		if cfg.LockID != 0 {
			return postgres.ReleaseSessionAdvisoryLock(relCtx, conn, cfg.LockID)
		}
		lockNs := cmp.Or(cfg.LockNamespace, "grizzle")
		return postgres.ReleaseSchemaLocks(relCtx, conn, lockNs, acquiredSchemas)
	})

	if logger != nil {
		if cfg.LockID != 0 {
			logger.DebugContext(ctx, "grizzle: acquired session advisory lock", "lock_id", cfg.LockID)
		} else {
			logger.DebugContext(ctx, "grizzle: acquired session advisory locks", "schemas", acquiredSchemas, "namespace", cfg.LockNamespace)
		}
	}

	// Idempotency: if this plan has already been applied by another process under the lock, skip execution.
	if history.IsApplied(ctx, conn, "postgres", primarySchema, p.Hash()) {
		if logger != nil {
			logger.InfoContext(ctx, "grizzle: plan already applied by another process, skipping", "plan_hash", p.Hash())
		}
		return 0, nil
	}

	// Apply DDL statements split into transactional and non-transactional groups
	groups := GroupSteps(p.Steps)
	stepIdx := 0
	committedSteps := 0
	// histErr captures a failed success-path history write: reported as a
	// typed non-fatal error after the migration completes (plan.ErrHistoryRecord).
	var histErr error

	recordFailureHistory := func(failedStep int, execErr error, isNonTx bool) {
		status := "failed"
		if committedSteps > 0 || isNonTx {
			status = "partial"
		}
		histCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		histConn, err := db.Conn(histCtx)
		if err == nil {
			defer func() { _ = histConn.Close() }()
			if rErr := history.RecordProgress(histCtx, histConn, "postgres", primarySchema, p, status, failedStep, execErr, time.Since(start)); rErr != nil && logger != nil {
				logger.WarnContext(histCtx, "grizzle: failed recording failure history", "status", status, "error", rErr)
			}
		}
	}

	for groupIdx, group := range groups {
		isLastGroup := groupIdx == len(groups)-1
		if group.NonTx {
			for _, s := range group.Steps {
				stepIdx++
				stepStart := time.Now()
				if err := callBeforeStep(cfg.BeforeStep, HookContext{Context: ctx, DBTX: conn, Step: s, Index: stepIdx, Total: len(p.Steps), IsNonTx: true}); err != nil {
					hookErr := fmt.Errorf("before_step hook: %w", err)
					if logger != nil {
						logger.ErrorContext(ctx, "grizzle: hook failed", "step_index", stepIdx, "error", hookErr)
					}
					// No DDL ran yet; status follows committedSteps only (not isNonTx).
					recordFailureHistory(stepIdx, hookErr, false)
					return committedSteps, hookErr
				}
				if err := execStepWithTracing(ctx, conn, s, true, cfg.Tracer); err != nil {
					if logger != nil {
						logger.ErrorContext(ctx, "grizzle: failed executing non-tx step", "step_index", stepIdx, "sql", s.SQL, "error", err)
					}
					recordFailureHistory(stepIdx, err, true)
					return committedSteps + 1, wrapStepExecError(s, err, true)
				}
				committedSteps++
				if err := callAfterStep(cfg.AfterStep, HookContext{Context: ctx, DBTX: conn, Step: s, Index: stepIdx, Total: len(p.Steps), IsNonTx: true}); err != nil {
					hookErr := fmt.Errorf("after_step hook: %w", err)
					if logger != nil {
						logger.ErrorContext(ctx, "grizzle: hook failed", "step_index", stepIdx, "error", hookErr)
					}
					recordFailureHistory(stepIdx, hookErr, true)
					return committedSteps, hookErr
				}
				if logger != nil {
					logger.DebugContext(ctx, "grizzle: executed non-tx step", "step_index", stepIdx, "type", s.Type, "table", s.Table, "duration", time.Since(stepStart))
				}
			}
			if isLastGroup {
				if err := history.RecordPlan(ctx, conn, "postgres", primarySchema, p, time.Since(start)); err != nil {
					histErr = fmt.Errorf("%w: %w", plan.ErrHistoryRecord, err)
					if logger != nil {
						logger.ErrorContext(ctx, "grizzle: failed recording history on conn", "error", err)
					}
				}
			}
		} else {
			tx, err := conn.BeginTx(ctx, nil)
			if err != nil {
				return committedSteps, fmt.Errorf("grizzle: failed to begin step transaction: %w", err)
			}
			_, _ = tx.ExecContext(ctx, localSearchPathSQL(targetSchemas))
			_ = ApplyTxTimeouts(ctx, tx, cfg.LockTimeout, cfg.StatementTimeout)

			for _, s := range group.Steps {
				stepIdx++
				stepStart := time.Now()
				if err := callBeforeStep(cfg.BeforeStep, HookContext{Context: ctx, DBTX: tx, Step: s, Index: stepIdx, Total: len(p.Steps), IsNonTx: false}); err != nil {
					_ = tx.Rollback()
					hookErr := fmt.Errorf("before_step hook: %w", err)
					if logger != nil {
						logger.ErrorContext(ctx, "grizzle: hook failed", "step_index", stepIdx, "error", hookErr)
					}
					recordFailureHistory(stepIdx, hookErr, false)
					return committedSteps, hookErr
				}
				if err := execStepWithTracing(ctx, tx, s, false, cfg.Tracer); err != nil {
					_ = tx.Rollback()
					if logger != nil {
						logger.ErrorContext(ctx, "grizzle: failed executing step in tx", "step_index", stepIdx, "sql", s.SQL, "error", err)
					}
					recordFailureHistory(stepIdx, err, false)
					return committedSteps, wrapStepExecError(s, err, false)
				}
				if err := callAfterStep(cfg.AfterStep, HookContext{Context: ctx, DBTX: tx, Step: s, Index: stepIdx, Total: len(p.Steps), IsNonTx: false}); err != nil {
					_ = tx.Rollback()
					hookErr := fmt.Errorf("after_step hook: %w", err)
					if logger != nil {
						logger.ErrorContext(ctx, "grizzle: hook failed", "step_index", stepIdx, "error", hookErr)
					}
					recordFailureHistory(stepIdx, hookErr, false)
					return committedSteps, hookErr
				}
				if logger != nil {
					logger.DebugContext(ctx, "grizzle: executed step in tx", "step_index", stepIdx, "type", s.Type, "table", s.Table, "duration", time.Since(stepStart))
				}
			}

			if err := tx.Commit(); err != nil {
				recordFailureHistory(stepIdx, err, false)
				return committedSteps, fmt.Errorf("grizzle: failed committing step transaction: %w", err)
			}
			committedSteps += len(group.Steps)
			// Model B: history is advisory — recorded after commit.
			if isLastGroup {
				if err := history.RecordPlan(ctx, conn, "postgres", primarySchema, p, time.Since(start)); err != nil {
					histErr = fmt.Errorf("%w: %w", plan.ErrHistoryRecord, err)
					if logger != nil {
						logger.ErrorContext(ctx, "grizzle: failed recording history post-commit", "error", err)
					}
				}
			}
		}
	}

	if err := callAfterSync(cfg.AfterSync, ctx, conn); err != nil {
		hookErr := fmt.Errorf("%w: %w", plan.ErrAfterSyncFailed, err)
		if logger != nil {
			logger.ErrorContext(ctx, "grizzle: hook failed", "error", hookErr)
		}
		return committedSteps, hookErr
	}

	if logger != nil {
		logger.InfoContext(ctx, "grizzle: plan execution finished successfully", "steps_applied", len(p.Steps), "total_duration", time.Since(start))
	}

	if histErr != nil {
		return committedSteps, histErr
	}
	return committedSteps, nil
}

// ApplySQLite applies a precomputed plan to SQLite in a single transaction with foreign keys handling.
func ApplySQLite(ctx context.Context, db *sql.DB, p *plan.Plan, cfg SQLiteExecConfig) error {
	if p == nil {
		return fmt.Errorf("sqlite: plan cannot be nil")
	}

	if err := p.ValidatePolicy(); err != nil {
		if cfg.Logger != nil {
			if vErr, ok := errors.AsType[*plan.DestructiveViolationError](err); ok {
				cfg.Logger.WarnContext(ctx, "sqlite: migration blocked by safety policy", "violations_count", len(vErr.Violations))
			} else {
				cfg.Logger.WarnContext(ctx, "sqlite: migration blocked by safety policy", "error", err)
			}
		}
		return err
	}

	if err := p.ValidateHazards(cfg.AcceptHazards); err != nil {
		if cfg.Logger != nil {
			cfg.Logger.WarnContext(ctx, "sqlite: migration blocked by unaccepted critical hazards", "error", err)
		}
		return err
	}

	if cfg.DryRun || len(p.Steps) == 0 {
		return nil
	}

	start := time.Now()
	logger := cfg.Logger
	if logger != nil {
		logger.InfoContext(ctx, "sqlite: starting SQLite plan application")
	}

	// BeforeSync runs once before any migration steps are executed.
	if err := callBeforeSync(cfg.BeforeSync, ctx, db); err != nil {
		hookErr := fmt.Errorf("sqlite: before_sync hook: %w", err)
		if logger != nil {
			logger.ErrorContext(ctx, "grizzle: hook failed", "error", hookErr)
		}
		return hookErr
	}

	// Execute in a transaction pinned to a single connection with foreign
	// keys disabled (per-connection PRAGMA; a no-op inside a transaction).
	// histErr captures a failed success-path history write; surfaced as a
	// typed non-fatal error after the transaction commits.
	var histErr error
	err := runSQLiteWithForeignKeysOff(ctx, db, func(tx *sql.Tx, conn *sql.Conn) error {
		// Idempotency: if this plan has already been applied by another process, skip execution.
		if history.IsApplied(ctx, tx, "sqlite", "", p.Hash()) {
			if logger != nil {
				logger.InfoContext(ctx, "sqlite: plan already applied by another process, skipping", "plan_hash", p.Hash())
			}
			return nil
		}

		for i, s := range p.Steps {
			stepStart := time.Now()
			sqlToExec := strings.TrimSpace(s.SQL)
			if sqlToExec == "" {
				continue
			}
			if err := callBeforeStep(cfg.BeforeStep, HookContext{Context: ctx, DBTX: tx, Step: s, Index: i + 1, Total: len(p.Steps), IsNonTx: false}); err != nil {
				_ = tx.Rollback()
				hookErr := fmt.Errorf("before_step hook: %w", err)
				if logger != nil {
					logger.ErrorContext(ctx, "grizzle: hook failed", "step_index", i+1, "error", hookErr)
				}
				histCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if rErr := history.RecordProgress(histCtx, conn, "sqlite", "", p, "failed", i+1, hookErr, time.Since(start)); rErr != nil && logger != nil {
					logger.WarnContext(histCtx, "sqlite: failed recording failure history", "error", rErr)
				}
				return hookErr
			}
			if err := executeSQLiteStep(ctx, tx, s, cfg); err != nil {
				_ = tx.Rollback()
				if logger != nil {
					logger.ErrorContext(ctx, "sqlite: failed executing step", "step_index", i+1, "sql", sqlToExec, "error", err)
				}
				histCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if rErr := history.RecordProgress(histCtx, conn, "sqlite", "", p, "failed", i+1, err, time.Since(start)); rErr != nil && logger != nil {
					logger.WarnContext(histCtx, "sqlite: failed recording failure history", "error", rErr)
				}
				return fmt.Errorf("%w: failed executing [%s]: %w", plan.ErrExecutionFailed, sqlToExec, err)
			}
			if err := callAfterStep(cfg.AfterStep, HookContext{Context: ctx, DBTX: tx, Step: s, Index: i + 1, Total: len(p.Steps), IsNonTx: false}); err != nil {
				_ = tx.Rollback()
				hookErr := fmt.Errorf("after_step hook: %w", err)
				if logger != nil {
					logger.ErrorContext(ctx, "grizzle: hook failed", "step_index", i+1, "error", hookErr)
				}
				histCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if rErr := history.RecordProgress(histCtx, conn, "sqlite", "", p, "failed", i+1, hookErr, time.Since(start)); rErr != nil && logger != nil {
					logger.WarnContext(histCtx, "sqlite: failed recording failure history", "error", rErr)
				}
				return hookErr
			}
			if logger != nil {
				logger.DebugContext(ctx, "sqlite: executed step", "step_index", i+1, "type", s.Type, "table", s.Table, "duration", time.Since(stepStart))
			}
		}

		finalFKRows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check;")
		if err != nil {
			return fmt.Errorf("sqlite: foreign key check failed: %w", err)
		}
		var fkViolations []string
		for finalFKRows.Next() {
			var vTbl, vParent string
			var vRowID, vFKID int64
			if err := finalFKRows.Scan(&vTbl, &vRowID, &vParent, &vFKID); err == nil {
				fkViolations = append(fkViolations, fmt.Sprintf("table %q row %d -> %q", vTbl, vRowID, vParent))
			}
		}
		_ = finalFKRows.Close()
		if len(fkViolations) > 0 {
			return fmt.Errorf("sqlite: foreign key constraint violation: %s", strings.Join(fkViolations, "; "))
		}
		// Model B: do not record history inside the migration transaction.
		return nil
	})
	if err != nil {
		return err
	}
	// Model B: history is advisory — recorded after DDL commit.
	if err := history.RecordPlan(ctx, db, "sqlite", "", p, time.Since(start)); err != nil {
		histErr = fmt.Errorf("%w: %w", plan.ErrHistoryRecord, err)
		if logger != nil {
			logger.ErrorContext(ctx, "sqlite: failed recording history post-commit", "error", err)
		}
	}
	if histErr != nil {
		return histErr
	}

	if cfg.Backfill != nil && p.ExpandContract {
		if err := RunBackfill(ctx, db, "main", p.Renames, p.Steps, cfg.Backfill, cfg.Logger); err != nil {
			return err
		}
	}

	if err := callAfterSync(cfg.AfterSync, ctx, db); err != nil {
		hookErr := fmt.Errorf("%w: %w", plan.ErrAfterSyncFailed, err)
		if logger != nil {
			logger.ErrorContext(ctx, "grizzle: hook failed", "error", hookErr)
		}
		return hookErr
	}

	if logger != nil {
		logger.InfoContext(ctx, "sqlite: plan application finished successfully", "steps_applied", len(p.Steps), "total_duration", time.Since(start))
	}

	return nil
}

func executeSQLiteStep(ctx context.Context, tx *sql.Tx, s plan.Step, cfg SQLiteExecConfig) error {
	var span Span
	if cfg.Tracer != nil {
		_, span = cfg.Tracer.Start(ctx, "grizzle.exec_step")
		span.SetAttribute("step.type", string(s.Type))
		span.SetAttribute("step.table", s.Table)
		span.SetAttribute("step.sql", s.SQL)
		span.SetAttribute("step.non_tx", false)
	}
	err := executeSQLiteStepInner(ctx, tx, s, cfg)
	if span != nil {
		if err != nil {
			span.RecordError(err)
		}
		span.End()
	}
	return err
}

func executeSQLiteStepInner(ctx context.Context, tx *sql.Tx, s plan.Step, cfg SQLiteExecConfig) error {
	if !s.IsTableRebuild {
		sqlToExec := strings.TrimSpace(s.SQL)
		if sqlToExec == "" {
			return nil
		}
		_, err := tx.ExecContext(ctx, sqlToExec)
		return err
	}

	// Table rebuild step: wrap in SAVEPOINT grizzle_rebuild
	if _, err := tx.ExecContext(ctx, "SAVEPOINT grizzle_rebuild;"); err != nil {
		return fmt.Errorf("creating savepoint: %w", err)
	}

	threshold := cfg.RebuildThreshold
	if threshold == 0 {
		threshold = 100000
	}
	batchSize := cfg.RebuildBatchSize
	if batchSize <= 0 {
		batchSize = 10000
	}

	var rowCount int64
	_ = tx.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %q;", s.Table)).Scan(&rowCount)

	tempTable := "_grizzle_new_" + s.Table
	copyDataPrefix := fmt.Sprintf("INSERT INTO %q", tempTable)
	idx := strings.Index(s.SQL, copyDataPrefix)

	// WITHOUT ROWID tables have no usable rowid for keyset batching.
	hasRowID := true
	var probeDDL string
	if qErr := tx.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name=?;`, s.Table).Scan(&probeDDL); qErr == nil {
		if strings.Contains(strings.ToUpper(probeDDL), "WITHOUT ROWID") {
			hasRowID = false
			if cfg.Logger != nil {
				cfg.Logger.DebugContext(ctx, "sqlite: skipping rowid keyset copy for WITHOUT ROWID table", "table", s.Table)
			}
		}
	}

	needsChunked := threshold > 0 && rowCount > int64(threshold) && idx != -1 && hasRowID

	if needsChunked {
		beforeCopy := strings.TrimSpace(s.SQL[:idx])
		rest := s.SQL[idx:]
		semiIdx := strings.Index(rest, ";")
		if semiIdx == -1 {
			_, _ = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT grizzle_rebuild;")
			_, _ = tx.ExecContext(ctx, "RELEASE SAVEPOINT grizzle_rebuild;")
			return fmt.Errorf("malformed copy statement in rebuild SQL")
		}
		copyStmt := rest[:semiIdx+1]
		afterCopy := strings.TrimSpace(rest[semiIdx+1:])

		if beforeCopy != "" {
			if _, err := tx.ExecContext(ctx, beforeCopy); err != nil {
				_, _ = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT grizzle_rebuild;")
				_, _ = tx.ExecContext(ctx, "RELEASE SAVEPOINT grizzle_rebuild;")
				return fmt.Errorf("executing create temp table: %w", err)
			}
		}

		colStart := strings.Index(copyStmt, "(")
		colEnd := strings.Index(copyStmt, ")")
		if colStart != -1 && colEnd != -1 && colEnd > colStart {
			colList := strings.TrimSpace(copyStmt[colStart+1 : colEnd])
			if colList != "" {
				if err := copyDataKeysetChunks(ctx, tx, tempTable, colList, s.Table, batchSize, cfg.Logger); err != nil {
					_, _ = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT grizzle_rebuild;")
					_, _ = tx.ExecContext(ctx, "RELEASE SAVEPOINT grizzle_rebuild;")
					return err
				}
			}
		}

		if afterCopy != "" {
			if _, err := tx.ExecContext(ctx, afterCopy); err != nil {
				_, _ = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT grizzle_rebuild;")
				_, _ = tx.ExecContext(ctx, "RELEASE SAVEPOINT grizzle_rebuild;")
				return fmt.Errorf("executing post-copy rebuild statements: %w", err)
			}
		}
	} else {
		if _, err := tx.ExecContext(ctx, s.SQL); err != nil {
			_, _ = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT grizzle_rebuild;")
			_, _ = tx.ExecContext(ctx, "RELEASE SAVEPOINT grizzle_rebuild;")
			return fmt.Errorf("executing rebuild SQL: %w", err)
		}
	}

	// Validate foreign keys under the savepoint
	fkRows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check;")
	if err != nil {
		_, _ = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT grizzle_rebuild;")
		_, _ = tx.ExecContext(ctx, "RELEASE SAVEPOINT grizzle_rebuild;")
		return fmt.Errorf("foreign key check query failed: %w", err)
	}
	var fkViolations []string
	for fkRows.Next() {
		var vTbl, vParent string
		var vRowID, vFKID int64
		if err := fkRows.Scan(&vTbl, &vRowID, &vParent, &vFKID); err == nil {
			fkViolations = append(fkViolations, fmt.Sprintf("table %q row %d -> %q", vTbl, vRowID, vParent))
		}
	}
	_ = fkRows.Close()

	if len(fkViolations) > 0 {
		_, _ = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT grizzle_rebuild;")
		_, _ = tx.ExecContext(ctx, "RELEASE SAVEPOINT grizzle_rebuild;")
		return fmt.Errorf("foreign key constraint violation detected after rebuilding table %q: %s", s.Table, strings.Join(fkViolations, ", "))
	}

	if _, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT grizzle_rebuild;"); err != nil {
		return fmt.Errorf("releasing savepoint grizzle_rebuild: %w", err)
	}

	return nil
}

func copyDataKeysetChunks(ctx context.Context, tx *sql.Tx, tempTable, colList, liveTable string, batchSize int, logger *slog.Logger) error {
	var lastRowID int64
	var hasStarted bool
	var batchNum int

	for {
		batchNum++
		var query string
		var args []any
		if !hasStarted {
			query = fmt.Sprintf("INSERT INTO %q (%s) SELECT %s FROM %q ORDER BY rowid ASC LIMIT ?;", tempTable, colList, colList, liveTable)
			args = []any{batchSize}
		} else {
			query = fmt.Sprintf("INSERT INTO %q (%s) SELECT %s FROM %q WHERE rowid > ? ORDER BY rowid ASC LIMIT ?;", tempTable, colList, colList, liveTable)
			args = []any{lastRowID, batchSize}
		}

		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("sqlite: chunked keyset copy failed: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("sqlite: failed checking rows affected in keyset copy: %w", err)
		}
		if affected == 0 {
			break
		}
		if logger != nil {
			logger.DebugContext(ctx, "sqlite: copied keyset batch", "table", liveTable, "batch", batchNum, "rows", affected)
		}

		var maxRowID int64
		var maxQuery string
		var maxArgs []any
		if !hasStarted {
			maxQuery = fmt.Sprintf("SELECT MAX(rowid) FROM (SELECT rowid FROM %q ORDER BY rowid ASC LIMIT ?);", liveTable)
			maxArgs = []any{batchSize}
		} else {
			maxQuery = fmt.Sprintf("SELECT MAX(rowid) FROM (SELECT rowid FROM %q WHERE rowid > ? ORDER BY rowid ASC LIMIT ?);", liveTable)
			maxArgs = []any{lastRowID, batchSize}
		}

		if err := tx.QueryRowContext(ctx, maxQuery, maxArgs...).Scan(&maxRowID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				break
			}
			return fmt.Errorf("sqlite: failed querying max rowid in keyset copy: %w", err)
		}
		lastRowID = maxRowID
		hasStarted = true
	}
	return nil
}

func wrapStepExecError(s plan.Step, err error, isNonTx bool) error {
	prefix := ""
	if isNonTx {
		prefix = "non-tx "
	}
	isFK := s.Type == plan.ChangeAddFK || s.Type == plan.ChangeValidateConstraint || s.RefTable != "" ||
		strings.Contains(strings.ToLower(err.Error()), "foreign key") ||
		strings.Contains(strings.ToLower(err.Error()), "fk")
	if isFK {
		return fmt.Errorf("%w: table %q foreign key constraint: failed executing %s[%s]: %w", plan.ErrExecutionFailed, s.Table, prefix, s.SQL, err)
	}
	return fmt.Errorf("%w: failed executing %s[%s]: %w", plan.ErrExecutionFailed, prefix, s.SQL, err)
}
