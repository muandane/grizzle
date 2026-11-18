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

// Metrics defines the interface for recording Grizzle operational metrics.
type Metrics interface {
	RecordMigrationDuration(ctx context.Context, duration time.Duration, dialect string, status string)
	RecordDriftDetected(ctx context.Context, dialect string, schema string)
	RecordLockWait(ctx context.Context, duration time.Duration, dialect string)
}

// PostgresExecConfig specifies the execution options for PostgreSQL synchronization.
type PostgresExecConfig struct {
	TargetSchema         string
	TargetSchemas        []string
	ShadowSchema         string
	ShadowSchemas        []string
	SchemaSQL            string
	RolesSQL             string
	CatalogSQL           string
	LockNamespace        string
	LockID               int64
	Filters              scope.Filters
	Policy               plan.DropPolicy
	AcceptHazards        []plan.HazardCode
	AllowResumePartial   bool
	ExpectedHash         string
	NonConcurrentIndexes bool
	LockTimeout          time.Duration
	StatementTimeout     time.Duration
	MaxRetries           int
	RandFloat            func() float64
	Logger               *slog.Logger
	Tracer               Tracer
	Metrics              Metrics
	DryRun               bool
	Backfill             BackfillFunc
	BeforeSync           SyncHook
	AfterSync            SyncHook
	BeforeStep           StepHook
	AfterStep            StepHook
	// FailOnContention gates DDL execution on PostgreSQL: if concurrent sessions hold conflicting
	// locks on target tables, retry with backoff up to LockTimeout; if contention persists, abort
	// with ErrLockContention instead of proceeding. When false (default), lock contention is advisory
	// and logs a warning before proceeding.
	FailOnContention bool
	// DryRunLockTimeout bounds lock waits for dry-run verification
	// (defaults to DefaultDryRunLockTimeout of 2s).
	DryRunLockTimeout time.Duration
	// ExecuteHooksInDryRun allows BeforeStep/AfterStep hooks to run inside the
	// dry-run sandbox. Defaults to false to avoid external side effects.
	ExecuteHooksInDryRun bool
	// DevDB is an optional external database connection used exclusively for
	// shadow-schema compilation (the always-rolled-back transaction). When
	// non-nil, shadow transactions open against DevDB instead of the target
	// database. Live catalog introspection always uses the target db passed to
	// the calling function. DevDB is never written to outside of the rolled-back
	// shadow transaction.
	DevDB *sql.DB
	// ShadowDBTX is an optional DBTX used by DiffPostgres to inspect the shadow
	// schema. If nil, dbtx is used for both live and shadow inspection.
	ShadowDBTX dialect.DBTX
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
	if _, err := splitSchemaSQL(cfg.SchemaSQL); err != nil {
		return nil, err
	}
	if cfg.Tracer != nil {
		var diffSpan Span
		ctx, diffSpan = cfg.Tracer.Start(ctx, "grizzle.diff_plan")
		defer diffSpan.End()
	}
	var serverVersion int
	if row := dbtx.QueryRowContext(ctx, "SELECT current_setting('server_version_num')::integer;"); row != nil {
		if err := row.Scan(&serverVersion); err != nil {
			return nil, fmt.Errorf("%w: server version: %w", plan.ErrInspectionFailed, err)
		}
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

		shadowDBTX := dbtx
		if cfg.ShadowDBTX != nil {
			shadowDBTX = cfg.ShadowDBTX
		}
		desired, err := inspectPostgresSchema(ctx, shadowDBTX, shadowSchema, cfg.Tracer)
		if err != nil {
			return nil, fmt.Errorf("%w: shadow schema: %w", plan.ErrInspectionFailed, err)
		}
		// Desired extensions are declared via CREATE EXTENSION statements,
		// which do not persist in the rolled-back shadow tx. Parse them
		// straight from SchemaSQL (authoritative desired set).
		desired.Extensions = schema.ParseExtensions(cfg.SchemaSQL)
		maskShadowExtensions(live, []string{shadowSchema})
		unmapShadowExprs(desired, []string{shadowSchema}, targetSchema)

		changes, err := diff.Diff(live, desired, targetSchema, shadowSchema, cfg.Filters)
		if err != nil {
			return nil, err
		}
		steps, err := postgres.RenderChangesWithOpts(targetSchema, changes, renderOpts)
		if err != nil {
			return nil, err
		}
		rolesSteps, err := diffRolesSteps(ctx, dbtx, cfg, serverVersion)
		if err != nil {
			return nil, err
		}
		catalogSteps, err := diffCatalogSteps(ctx, dbtx, cfg, serverVersion)
		if err != nil {
			return nil, err
		}
		return append(steps, append(rolesSteps, catalogSteps...)...), nil
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

	shadowDBTX := dbtx
	if cfg.ShadowDBTX != nil {
		shadowDBTX = cfg.ShadowDBTX
	}
	desiredMap, err := inspectPostgresSchemas(ctx, shadowDBTX, shadowSchemas, cfg.Tracer)
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
	for target, shadow := range shadowMap {
		if desired := desiredMap[shadow]; desired != nil {
			unmapShadowExprs(desired, shadowNames, target)
		}
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

	steps, err := postgres.RenderChangesWithOpts(targetSchemas[0], allChanges, renderOpts)
	if err != nil {
		return nil, err
	}
	rolesSteps, err := diffRolesSteps(ctx, dbtx, cfg, serverVersion)
	if err != nil {
		return nil, err
	}
	catalogSteps, err := diffCatalogSteps(ctx, dbtx, cfg, serverVersion)
	if err != nil {
		return nil, err
	}
	return append(steps, append(rolesSteps, catalogSteps...)...), nil
}

func inspectPostgresSchema(ctx context.Context, dbtx dialect.DBTX, schema string, tracer Tracer, searchPath ...string) (*schema.Schema, error) {
	if tracer != nil {
		var span Span
		ctx, span = tracer.Start(ctx, "grizzle.inspect_schema")
		span.SetAttribute("schema", schema)
		defer span.End()
	}
	return postgres.Inspect(ctx, dbtx, schema, searchPath...)
}

func inspectPostgresSchemas(ctx context.Context, dbtx dialect.DBTX, schemas []string, tracer Tracer, searchPath ...string) (map[string]*schema.Schema, error) {
	if tracer != nil {
		var span Span
		ctx, span = tracer.Start(ctx, "grizzle.inspect_schema")
		span.SetAttribute("schemas", strings.Join(schemas, ","))
		defer span.End()
	}
	return postgres.InspectSchemas(ctx, dbtx, schemas, searchPath...)
}

func execStepWithTracing(ctx context.Context, execer dialect.DBTX, s plan.Step, isNonTx bool, tracer Tracer, roleIR map[string]*schema.Role, subIR map[string]*schema.Subscription) error {
	var span Span
	if tracer != nil {
		_, span = tracer.Start(ctx, "grizzle.exec_step")
		span.SetAttribute("step.type", string(s.Type))
		span.SetAttribute("step.table", s.Table)
		// Always attribute the redacted Step.SQL — never the materialized secret form.
		span.SetAttribute("step.sql", s.SQL)
		span.SetAttribute("step.non_tx", isNonTx)
	}
	sql, err := materializeRoleStepSQL(s, roleIR)
	if err != nil {
		if span != nil {
			span.RecordError(err)
			span.End()
		}
		return err
	}
	sql, err = materializeSubscriptionStepSQL(plan.Step{Type: s.Type, Table: s.Table, SQL: sql}, subIR)
	if err != nil {
		if span != nil {
			span.RecordError(err)
			span.End()
		}
		return err
	}
	_, err = execer.ExecContext(ctx, sql)
	if span != nil {
		if err != nil {
			span.RecordError(err)
		}
		span.End()
	}
	return err
}

func roleIRFromConfig(cfg PostgresExecConfig) map[string]*schema.Role {
	desired, err := desiredRolesSpec(cfg)
	if err != nil || desired == nil {
		return nil
	}
	return desiredRoleIR(desired)
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
func SyncPostgres(ctx context.Context, db *sql.DB, cfg PostgresExecConfig) (err error) {
	start := time.Now()
	var totalCommitted int
	defer func() {
		if cfg.Metrics != nil {
			status := "applied"
			if err != nil {
				status = "failed"
			} else if totalCommitted == 0 {
				status = "noop"
			}
			cfg.Metrics.RecordMigrationDuration(ctx, time.Since(start), "postgres", status)
		}
	}()

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
		committed, syncErr := syncPostgresOnce(ctx, db, cfg, budget)
		if syncErr == nil {
			totalCommitted = committed
			if cfg.Backfill != nil && cfg.Filters.ExpandContract {
				if err := RunBackfill(ctx, db, cfg.TargetSchema, cfg.Filters.Renames, nil, cfg.Backfill, cfg.Logger); err != nil {
					return err
				}
			}
			return nil
		}
		err = syncErr

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

	var acquiredSchemas []string
	var lockAcquired bool
	defer releasePostgresConn(ctx, conn, func(relCtx context.Context) error {
		if !lockAcquired {
			return nil
		}
		if cfg.LockID != 0 {
			return postgres.ReleaseSessionAdvisoryLock(relCtx, conn, cfg.LockID)
		}
		lockNs := cmp.Or(cfg.LockNamespace, "grizzle")
		return postgres.ReleaseSchemaLocks(relCtx, conn, lockNs, acquiredSchemas)
	})

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
	if cfg.LockID != 0 {
		err = postgres.AcquireSessionAdvisoryLock(lockCtx, conn, cfg.LockID)
	} else {
		lockNs := cmp.Or(cfg.LockNamespace, "grizzle")
		acquiredSchemas, err = postgres.AcquireSchemaLocks(lockCtx, conn, lockNs, targetSchemas)
	}
	cancelLock()
	lockWaitDuration := time.Since(lockStart)
	budget.recordWait(lockWaitDuration)
	if cfg.Metrics != nil {
		cfg.Metrics.RecordLockWait(ctx, lockWaitDuration, "postgres")
	}
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
		if errors.Is(err, plan.ErrTransactionPooling) {
			return 0, err
		}
		return 0, fmt.Errorf("%w: %w", plan.ErrLockAcquisition, err)
	}
	lockAcquired = true

	if logger != nil {
		if cfg.LockID != 0 {
			logger.DebugContext(ctx, "grizzle: acquired session advisory lock", "lock_id", cfg.LockID)
		} else {
			logger.DebugContext(ctx, "grizzle: acquired session advisory locks", "schemas", acquiredSchemas, "namespace", cfg.LockNamespace)
		}
	}

	// 1b. Gating check: fail closed if latest migration is in partial or failed state
	latest, err := history.GetLatest(ctx, conn, "postgres", primarySchema)
	if err != nil {
		return 0, fmt.Errorf("grizzle: checking migration history: %w", err)
	}
	if latest != nil && (latest.Status == "partial" || latest.Status == "failed") {
		if !cfg.AllowResumePartial {
			if latest.Error != "" {
				return 0, fmt.Errorf("%w: previous migration ended in %q status (plan %s, step %d): %s; resolve failure with 'grizzle repair' or resume with WithAllowResumePartial / Options.AllowResumePartial",
					plan.ErrPriorFailureBlocked, latest.Status, latest.PlanHash, latest.FailedStep, latest.Error)
			}
			return 0, fmt.Errorf("%w: previous migration ended in %q status (plan %s, step %d); resolve failure with 'grizzle repair' or resume with WithAllowResumePartial / Options.AllowResumePartial",
				plan.ErrPriorFailureBlocked, latest.Status, latest.PlanHash, latest.FailedStep)
		}
		if logger != nil {
			logger.WarnContext(ctx, "grizzle: resuming prior partial/failed migration",
				"prior_status", latest.Status,
				"prior_plan", latest.PlanHash,
				"prior_step", latest.FailedStep,
			)
		}
	}

	// 2. Setup shadow schema and diff schemas inside an isolated transaction
	shadowStart := time.Now()
	var shadowTx *sql.Tx
	if cfg.DevDB != nil {
		var err error
		shadowTx, err = cfg.DevDB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		if err != nil {
			return 0, fmt.Errorf("grizzle: failed to begin shadow tx on dev database: %w", err)
		}
	} else {
		var err error
		shadowTx, err = conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		if err != nil {
			return 0, fmt.Errorf("grizzle: failed to begin shadow tx: %w", err)
		}
	}
	defer func() { _ = shadowTx.Rollback() }()

	_ = ApplyTxTimeouts(ctx, shadowTx, cfg.LockTimeout, cfg.StatementTimeout)

	schemaGroups, err := splitSchemaSQL(cfg.SchemaSQL)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", plan.ErrCompilationFailed, err)
	}

	var shadowSchemas []string
	if !isMulti {
		shadowSchema := cfg.ShadowSchema
		if shadowSchema == "" {
			shadowSchema = "_grizzle_shadow"
		}
		shadowSchemas = []string{shadowSchema}

		if err := postgres.SetupShadowSchema(ctx, shadowTx, shadowSchema); err != nil {
			return 0, wrapShadowSetupError(err, cfg.DevDB != nil)
		}

		if err := postgres.RunShadowDDL(ctx, shadowTx, shadowSchema, primarySchema, schemaGroups.ShadowSQL); err != nil {
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
			return 0, wrapShadowSetupError(err, cfg.DevDB != nil)
		}

		if err := postgres.RunMultiShadowDDL(ctx, shadowTx, shadowMap, targetSchemas, schemaGroups.ShadowSQL); err != nil {
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
	diffCfg := cfg
	diffCfg.ShadowDBTX = shadowTx
	steps, err := DiffPostgres(ctx, conn, diffCfg)
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
		CatalogSQL:     cfg.CatalogSQL,
	}

	// 4b. Verify expected plan hash if provided (aborts with ErrPlanDrift on mismatch)
	if cfg.ExpectedHash != "" && p.Hash() != cfg.ExpectedHash {
		if cfg.Metrics != nil {
			cfg.Metrics.RecordDriftDetected(ctx, "postgres", primarySchema)
		}
		if logger != nil {
			logger.WarnContext(ctx, "grizzle: plan drift detected post-lock", "expected", cfg.ExpectedHash, "actual", p.Hash())
		}
		return 0, fmt.Errorf("%w: expected hash %q, actual post-lock hash %q", plan.ErrPlanDrift, cfg.ExpectedHash, p.Hash())
	}

	if len(steps) == 0 {
		if logger != nil {
			logger.InfoContext(ctx, "grizzle: schema is already in sync", "duration", time.Since(start))
		}
		recordNoOpSyncIfStale(ctx, conn, "postgres", primarySchema, cfg.SchemaSQL, p, latest, cfg.DryRun, time.Since(start))
		if cfg.DevDB != nil {
			_ = postgres.DropShadowSchemas(ctx, cfg.DevDB, shadowSchemas)
		} else {
			_ = postgres.DropShadowSchemas(ctx, conn, shadowSchemas)
		}
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

	// 6b. Lock contention pre-flight: check for conflicting holders on target tables before DDL execution
	lockTables := CollectTablesNeedingStrongLocks(primarySchema, steps)
	if err := postgres.PreflightLockContention(ctx, conn, lockTables, cfg.LockTimeout, cfg.FailOnContention, logger); err != nil {
		return 0, err
	}

	// 7. Apply DDL statements split into transactional and non-transactional groups
	groups := GroupSteps(steps)
	roleIR := roleIRFromConfig(cfg)
	subIR := subscriptionIRFromConfig(cfg)
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
		// Write using detached context not cancelled by the failure
		histCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		// Primary: use the dedicated connection already held, avoiding pool starvation (MaxOpenConns=1)
		if rErr := history.RecordProgress(histCtx, conn, "postgres", primarySchema, p, status, failedStep, execErr, time.Since(start)); rErr != nil {
			if logger != nil {
				logger.WarnContext(histCtx, "grizzle: failed recording failure history on conn, trying fallback", "status", status, "error", rErr)
			}
			// Fallback: only if conn was broken, try pool with a short timeout
			fbCtx, fbCancel := context.WithTimeout(context.Background(), 1*time.Second)
			defer fbCancel()
			if histConn, err := db.Conn(fbCtx); err == nil {
				defer func() { _ = histConn.Close() }()
				_ = history.RecordProgress(fbCtx, histConn, "postgres", primarySchema, p, status, failedStep, execErr, time.Since(start))
			}
		}
	}

	for groupIdx, group := range groups {
		isLastGroup := groupIdx == len(groups)-1
		if group.NonTx {
			// Long-running non-transactional DDL (CREATE INDEX CONCURRENTLY etc.)
			// must not be killed by statement_timeout: disable it for this group
			// and restore the configured session timeout on every exit path.
			if restoreStmtTimeout, dErr := DisableStatementTimeout(ctx, conn, cfg.StatementTimeout); dErr == nil {
				defer restoreStmtTimeout()
			}
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
				if err := execStepWithTracing(ctx, conn, s, true, cfg.Tracer, roleIR, subIR); err != nil {
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
			tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
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
				// VALIDATE CONSTRAINT scans the whole table under SHARE UPDATE EXCLUSIVE;
				// exempt it from the statement timeout, then restore it so later steps
				// in this same transaction remain protected.
				validateExempt := s.Type == plan.ChangeValidateConstraint && cfg.StatementTimeout > 0
				if validateExempt {
					_, _ = tx.ExecContext(ctx, "SET LOCAL statement_timeout = 0;")
				}
				if err := execStepWithTracing(ctx, tx, s, false, cfg.Tracer, roleIR, subIR); err != nil {
					_ = tx.Rollback()
					if logger != nil {
						logger.ErrorContext(ctx, "grizzle: failed executing step in tx", "step_index", stepIdx, "sql", s.SQL, "error", err)
					}
					recordFailureHistory(stepIdx, err, false)
					return committedSteps, wrapStepExecError(s, err, false)
				}
				if validateExempt {
					_, _ = tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%dms';", cfg.StatementTimeout.Milliseconds()))
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

	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("grizzle: failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var shadowTx *sql.Tx
	if cfg.DevDB != nil {
		var err error
		shadowTx, err = cfg.DevDB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		if err != nil {
			return nil, fmt.Errorf("grizzle: failed to begin shadow tx on dev database: %w", err)
		}
		defer func() { _ = shadowTx.Rollback() }()
	} else {
		shadowTx = tx
	}

	_ = ApplyTxTimeouts(ctx, shadowTx, cfg.LockTimeout, cfg.StatementTimeout)

	schemaGroups, err := splitSchemaSQL(cfg.SchemaSQL)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", plan.ErrCompilationFailed, err)
	}

	var shadowSchemas []string
	if !isMulti {
		shadowSchema := cfg.ShadowSchema
		if shadowSchema == "" {
			shadowSchema = "_grizzle_shadow"
		}
		shadowSchemas = []string{shadowSchema}

		if err := postgres.SetupShadowSchema(ctx, shadowTx, shadowSchema); err != nil {
			return nil, wrapShadowSetupError(err, cfg.DevDB != nil)
		}
		defer func() { _ = postgres.DropShadowSchema(context.Background(), shadowTx, shadowSchema) }()

		if err := postgres.RunShadowDDL(ctx, shadowTx, shadowSchema, primarySchema, schemaGroups.ShadowSQL); err != nil {
			return nil, fmt.Errorf("%w: %w", plan.ErrCompilationFailed, err)
		}
	} else {
		shadowMap := postgres.ComputeShadowSchemas(cfg.ShadowSchema, targetSchemas)
		for _, shadow := range shadowMap {
			shadowSchemas = append(shadowSchemas, shadow)
		}

		if err := postgres.SetupShadowSchemas(ctx, shadowTx, shadowSchemas); err != nil {
			return nil, wrapShadowSetupError(err, cfg.DevDB != nil)
		}
		defer func() { _ = postgres.DropShadowSchemas(context.Background(), shadowTx, shadowSchemas) }()

		if err := postgres.RunMultiShadowDDL(ctx, shadowTx, shadowMap, targetSchemas, schemaGroups.ShadowSQL); err != nil {
			return nil, fmt.Errorf("%w: %w", plan.ErrCompilationFailed, err)
		}
	}

	diffCfg := cfg
	diffCfg.ShadowDBTX = shadowTx
	steps, err := DiffPostgres(ctx, tx, diffCfg)
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
		CatalogSQL:           cfg.CatalogSQL,
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

// recordNoOpSyncIfStale writes an advisory history record with the current schema hash
// when a synchronization detected no steps and the stored schema hash is absent or stale.
func recordNoOpSyncIfStale(ctx context.Context, dbtx dialect.DBTX, dialectName, schemaName, schemaSQL string, p *plan.Plan, latest *history.Record, dryRun bool, duration time.Duration) {
	if dryRun {
		return
	}
	desiredHash := schema.HashSchemaSQL(schemaSQL)
	if desiredHash == "" {
		return
	}
	if latest == nil || latest.SchemaHash != desiredHash {
		_ = history.RecordPlan(ctx, dbtx, dialectName, schemaName, p, duration)
	}
}

// ApplyPostgres applies a precomputed plan to PostgreSQL using a dedicated connection,
// session-level advisory locking, step grouping (NonTx vs Tx), timeouts, and retry logic.
func ApplyPostgres(ctx context.Context, db *sql.DB, p *plan.Plan, cfg PostgresExecConfig) (err error) {
	start := time.Now()
	var totalCommitted int
	defer func() {
		if cfg.Metrics != nil && p != nil {
			status := "applied"
			if err != nil {
				status = "failed"
			} else if totalCommitted == 0 || len(p.Steps) == 0 {
				status = "noop"
			}
			cfg.Metrics.RecordMigrationDuration(ctx, time.Since(start), "postgres", status)
		}
	}()

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
		committed, applyErr := applyPostgresOnce(ctx, db, p, cfg, budget)
		if applyErr == nil {
			totalCommitted = committed
			if cfg.Backfill != nil && p.ExpandContract {
				if err := RunBackfill(ctx, db, cfg.TargetSchema, p.Renames, nil, cfg.Backfill, cfg.Logger); err != nil {
					return err
				}
			}
			return nil
		}
		err = applyErr

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

	var acquiredSchemas []string
	var lockAcquired bool
	defer releasePostgresConn(ctx, conn, func(relCtx context.Context) error {
		if !lockAcquired {
			return nil
		}
		if cfg.LockID != 0 {
			return postgres.ReleaseSessionAdvisoryLock(relCtx, conn, cfg.LockID)
		}
		lockNs := cmp.Or(cfg.LockNamespace, "grizzle")
		return postgres.ReleaseSchemaLocks(relCtx, conn, lockNs, acquiredSchemas)
	})

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
	if cfg.LockID != 0 {
		err = postgres.AcquireSessionAdvisoryLock(lockCtx, conn, cfg.LockID)
	} else {
		lockNs := cmp.Or(cfg.LockNamespace, "grizzle")
		acquiredSchemas, err = postgres.AcquireSchemaLocks(lockCtx, conn, lockNs, targetSchemas)
	}
	cancelLock()
	lockWaitDuration := time.Since(lockStart)
	budget.recordWait(lockWaitDuration)
	if cfg.Metrics != nil {
		cfg.Metrics.RecordLockWait(ctx, lockWaitDuration, "postgres")
	}
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
		if errors.Is(err, plan.ErrTransactionPooling) {
			return 0, err
		}
		return 0, fmt.Errorf("%w: %w", plan.ErrLockAcquisition, err)
	}
	lockAcquired = true

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
	var droppedRoles []string
	for _, step := range p.Steps {
		if step.Type == plan.ChangeDropRole {
			droppedRoles = append(droppedRoles, step.Table)
		}
	}
	if err := validateRoleDropSafety(ctx, conn, droppedRoles, targetSchemas, p.Steps); err != nil {
		return 0, err
	}
	if err := validateCatalogDropSafety(ctx, conn, p.Steps); err != nil {
		return 0, err
	}

	// Apply DDL statements split into transactional and non-transactional groups
	groups := GroupSteps(p.Steps)
	roleIR := roleIRFromConfig(cfg)
	if len(roleIR) == 0 && (p.RolesSQL != "" || p.SchemaSQL != "") {
		// Direct apply may carry RolesSQL on the plan when cfg omits it.
		roleIR = roleIRFromConfig(PostgresExecConfig{
			SchemaSQL:    p.SchemaSQL,
			RolesSQL:     p.RolesSQL,
			TargetSchema: primarySchema,
		})
	}
	subIR := subscriptionIRFromConfig(cfg)
	if len(subIR) == 0 && (p.CatalogSQL != "" || p.SchemaSQL != "") {
		subIR = subscriptionIRFromConfig(PostgresExecConfig{
			SchemaSQL:    p.SchemaSQL,
			CatalogSQL:   p.CatalogSQL,
			TargetSchema: primarySchema,
		})
	}
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

		// Primary: use the dedicated connection already held, avoiding pool starvation (MaxOpenConns=1)
		if rErr := history.RecordProgress(histCtx, conn, "postgres", primarySchema, p, status, failedStep, execErr, time.Since(start)); rErr != nil {
			if logger != nil {
				logger.WarnContext(histCtx, "grizzle: failed recording failure history on conn, trying fallback", "status", status, "error", rErr)
			}
			// Fallback: only if conn was broken, try pool with a short timeout
			fbCtx, fbCancel := context.WithTimeout(context.Background(), 1*time.Second)
			defer fbCancel()
			if histConn, err := db.Conn(fbCtx); err == nil {
				defer func() { _ = histConn.Close() }()
				_ = history.RecordProgress(fbCtx, histConn, "postgres", primarySchema, p, status, failedStep, execErr, time.Since(start))
			}
		}
	}

	// Lock contention pre-flight: check for conflicting holders on target tables before DDL execution
	lockTables := CollectTablesNeedingStrongLocks(primarySchema, p.Steps)
	if err := postgres.PreflightLockContention(ctx, conn, lockTables, cfg.LockTimeout, cfg.FailOnContention, logger); err != nil {
		return 0, err
	}

	for groupIdx, group := range groups {
		isLastGroup := groupIdx == len(groups)-1
		if group.NonTx {
			// Long-running non-transactional DDL (CREATE INDEX CONCURRENTLY etc.)
			// must not be killed by statement_timeout: disable it for this group
			// and restore the configured session timeout on every exit path.
			if restoreStmtTimeout, dErr := DisableStatementTimeout(ctx, conn, cfg.StatementTimeout); dErr == nil {
				defer restoreStmtTimeout()
			}
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
				if err := execStepWithTracing(ctx, conn, s, true, cfg.Tracer, roleIR, subIR); err != nil {
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
			tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
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
				// VALIDATE CONSTRAINT scans the whole table under SHARE UPDATE EXCLUSIVE;
				// exempt it from the statement timeout, then restore it so later steps
				// in this same transaction remain protected.
				validateExempt := s.Type == plan.ChangeValidateConstraint && cfg.StatementTimeout > 0
				if validateExempt {
					_, _ = tx.ExecContext(ctx, "SET LOCAL statement_timeout = 0;")
				}
				if err := execStepWithTracing(ctx, tx, s, false, cfg.Tracer, roleIR, subIR); err != nil {
					_ = tx.Rollback()
					if logger != nil {
						logger.ErrorContext(ctx, "grizzle: failed executing step in tx", "step_index", stepIdx, "sql", s.SQL, "error", err)
					}
					recordFailureHistory(stepIdx, err, false)
					return committedSteps, wrapStepExecError(s, err, false)
				}
				if validateExempt {
					_, _ = tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%dms';", cfg.StatementTimeout.Milliseconds()))
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
