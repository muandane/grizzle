package grizzle

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/muandane/grizzle/internal/exec"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
	"github.com/muandane/grizzle/internal/scope"
)

// Dialect specifies the target SQL database dialect.
type Dialect string

const (
	// DialectAuto instructs Grizzle to auto-detect the engine from the driver type.
	DialectAuto Dialect = ""
	// DialectPostgres specifies PostgreSQL (13+).
	DialectPostgres Dialect = "postgres"
	// DialectSQLite specifies SQLite (3.35+).
	DialectSQLite Dialect = "sqlite"
)

// Options configures the schema synchronization process.
type Options struct {
	// Dialect explicitly defines the database engine (DialectPostgres or DialectSQLite).
	Dialect Dialect

	// SchemaSQL contains the complete desired state. PostgreSQL role and
	// catalog statements may share this file; they bypass shadow compilation
	// and are merged with the optional side-channel files.
	SchemaSQL string

	// TargetSchema is the schema to manage (defaults to "public" for Postgres, "main" for SQLite).
	// Deprecated: Use TargetSchemas for multi-schema support.
	TargetSchema string

	// TargetSchemas specifies the database schemas to manage (defaults to [TargetSchema] or ["public"] for Postgres).
	// For SQLite, entries other than "main" are ATTACH DATABASE schema names listed in SQLiteAttach.
	TargetSchemas []string

	// SQLiteAttach maps ATTACH DATABASE schema names to filesystem paths.
	// Every TargetSchemas entry other than "main" must appear here; "main" is
	// the primary database already open and must not be listed. Unknown keys,
	// empty paths, and duplicate names are rejected. PostgreSQL ignores this field.
	SQLiteAttach map[string]string

	// ShadowSchema is the temporary schema name used for validation (defaults to "_grizzle_shadow").
	ShadowSchema string

	// AllowDrop permits all destructive operations when set to true.
	// Defaults to false for zero data loss.
	AllowDrop bool

	// AllowRevoke permits privilege revocation when a grant in RolesSQL is
	// removed (access loss). Has no granular override; REVOKE_PRIVILEGE
	// (CRITICAL) still requires AcceptHazards.
	AllowRevoke bool

	// AllowDropRole permits dropping a managed role that left RolesSQL.
	// Has no granular override; DROP_ROLE (CRITICAL) still requires
	// AcceptHazards.
	AllowDropRole bool

	// AllowDropPublication permits dropping a managed publication that
	// left CatalogSQL. Has no granular override; DROP_PUBLICATION
	// (CRITICAL) still requires AcceptHazards.
	AllowDropPublication bool

	// AllowDropEventTrigger permits dropping a managed event trigger that
	// left CatalogSQL. Has no granular override; DROP_EVENT_TRIGGER
	// (CRITICAL) still requires AcceptHazards.
	AllowDropEventTrigger bool

	// AllowDropSubscription permits dropping a managed subscription that
	// left CatalogSQL. Has no granular override; DROP_SUBSCRIPTION
	// (CRITICAL) still requires AcceptHazards. Remote slots are kept
	// (DISABLE + SET slot_name = NONE before DROP).
	AllowDropSubscription bool

	// AllowDropReplicationSlot permits dropping a standalone logical
	// replication slot via explicit SELECT pg_drop_replication_slot.
	// Live-only slots are never swept. DROP_REPLICATION_SLOT (CRITICAL)
	// still requires AcceptHazards. Active slots (active_pid IS NOT NULL)
	// are refused.
	AllowDropReplicationSlot bool

	// Granular drop overrides (nil inherits from AllowDrop):
	AllowDropTable     *bool
	AllowDropColumn    *bool
	AllowDropIndex     *bool
	AllowDropFK        *bool
	AllowDropCheck     *bool
	AllowDropExtension *bool
	AllowDropFunction  *bool
	AllowDropPolicy    *bool
	AllowDropTrigger   *bool
	AllowDropView      *bool
	AllowDropDomain    *bool

	// ExcludeTables defines table names or glob patterns (e.g. "spatial_ref_sys", "asynq_*")
	// that Grizzle will never manage, alter, or drop.
	ExcludeTables []string

	// IncludeTables limits Grizzle's management scope to only the specified tables or patterns.
	IncludeTables []string

	// StrictScope requires IncludeTables to be non-empty. When true and IncludeTables is empty,
	// operations fail immediately with ErrStrictScope.
	// Strongly recommended for production environments to avoid accidental alterations or drops
	// of untracked tables.
	StrictScope bool

	// AcceptHazards lists critical hazard codes that are explicitly approved to execute.
	// Critical hazards not present in this list will cause Apply to fail with ErrHazardBlocked.
	AcceptHazards []plan.HazardCode

	// NonConcurrentIndexes opts out of emitting CONCURRENTLY for PostgreSQL index creation/drops.
	// When true, index operations are created inside the transaction.
	NonConcurrentIndexes bool

	// LockNamespace specifies the application namespace string used for PostgreSQL advisory locking (defaults to "grizzle").
	LockNamespace string

	// LockID is an optional explicit 64-bit integer used for the PostgreSQL advisory lock (pg_advisory_xact_lock).
	// If 0, Grizzle derives 2-int per-schema advisory locks using (hash32(LockNamespace), hash32(schema)).
	LockID int64

	// LockTimeout specifies the maximum time to wait when acquiring locks (defaults to 5s).
	LockTimeout time.Duration

	// StatementTimeout specifies the maximum execution time for any single DDL statement (defaults to 5m).
	StatementTimeout time.Duration

	// MaxRetries specifies the number of retry attempts upon lock timeout (SQLSTATE 55P03, defaults to 3).
	MaxRetries int

	// RandFloat provides an optional random source func returning in [0.0, 1.0) for deterministic jitter in tests.
	RandFloat func() float64

	// Renames maps old column names to new column names (e.g. "users.old_col": "new_col" or "old_col": "new_col")
	// to explicitly disambiguate column renames instead of treating them as DROP + ADD.
	//
	// Experimental: the rename mapping format and RENAME_AMBIGUOUS semantics
	// may change before the 1.0 release.
	Renames map[string]string

	// ExpandContract enables staged expand-and-contract zero-downtime migrations (ZDM).
	// In expand mode, renamed or modified columns are added alongside existing columns,
	// delaying destructive drops to a later, separately approved plan.
	//
	// Experimental: staged expand-and-contract plans are under active development;
	// the contract phase and hazard surface may change before the 1.0 release.
	ExpandContract bool

	// Backfill hook function run outside the DDL lock window in batches during staged expand migration.
	//
	// Experimental: backfill batching semantics (batch size, ordering, error
	// handling) may change before the 1.0 release. The CLI installs a hook via
	// --backfill / --backfill-file when --expand-contract is set; a library
	// Backfill still wins when both are configured by the caller.
	Backfill BackfillFunc

	// BeforeSync runs once before any migration steps or locks are executed.
	// If it returns an error, the migration aborts before any DDL runs and no
	// history record is written.
	BeforeSync SyncHook

	// AfterSync runs once after all migration steps and history recording succeed.
	// If it returns an error, the DDL has already committed; the error is
	// wrapped with ErrAfterSyncFailed.
	AfterSync SyncHook

	// BeforeStep executes immediately prior to executing each plan step.
	// If it returns an error, the pending step is not executed; in a
	// transactional group the transaction is rolled back and history records
	// the failure with error "before_step hook: ...".
	BeforeStep StepHook

	// AfterStep executes immediately following the successful execution of each
	// plan step. If it returns an error in a transactional group, the
	// transaction is rolled back (reverting the step); in a non-transactional
	// group the step has already committed and history records "partial".
	AfterStep StepHook

	// DryRun returns the planned SQL statements without executing them on the live database.
	DryRun bool

	// DryRunLockTimeout bounds how long live dry-run verification waits on
	// table locks before failing fast (defaults to 2s).
	DryRunLockTimeout time.Duration

	// ExecuteHooksInDryRun allows BeforeStep/AfterStep hooks to run during
	// live dry-run verification. Defaults to false to avoid accidental
	// external side effects (webhooks, message publishing, etc.).
	ExecuteHooksInDryRun bool

	// SeedSQL contains idempotent data-seed SQL executed after a successful
	// sync (Sync DDL → AfterSync → Seed). The seed runs in a single
	// transaction and is skipped when the same seed (by content hash) was
	// already applied, unless SeedForce is set.
	SeedSQL string

	// SeedForce re-runs the seed even when the same seed hash was already
	// applied.
	SeedForce bool

	// RolesSQL optionally supplies desired roles and privilege grants
	// alongside SchemaSQL. Duplicate role/grant identities in this explicit
	// side-channel override entries extracted from SchemaSQL. Role statements
	// are never shadow-compiled, and schema DDL is not valid here. Managed
	// attributes include LOGIN/NOLOGIN, PASSWORD, VALID UNTIL, CONNECTION
	// LIMIT, INHERIT, CREATEDB, CREATEROLE, and ALTER ROLE SET/RESET.
	// SUPERUSER/REPLICATION/BYPASSRLS are refused. Passwords are compared by
	// server-side hash equality and never written into Step.SQL or plan JSON.
	// PostgreSQL only — ignored on SQLite.
	RolesSQL string

	// CatalogSQL optionally supplies desired cluster-catalog objects alongside
	// SchemaSQL. Duplicate publication/event-trigger names in this explicit
	// side-channel override entries extracted from SchemaSQL. Catalog
	// statements are never shadow-compiled or run inside the shadow
	// transaction, and schema DDL is not valid here. Drops are narrow — only
	// marker-stamped (grizzle-managed) objects are considered. Event triggers
	// reference functions that must exist in SchemaSQL or as live objects.
	// PostgreSQL only — rejected on SQLite.
	CatalogSQL string

	// SQLiteRebuildThreshold defines the row count threshold above which SQLite table rebuilds
	// chunk data copying by keyset to prevent journal memory exhaustion.
	// Defaults to 100000. Set to 0 to disable batching.
	SQLiteRebuildThreshold int

	// SQLiteRebuildBatchSize defines the chunk size when copying data in batches during SQLite table rebuilds.
	// Defaults to 10000.
	SQLiteRebuildBatchSize int

	// Logger accepts a structured logger (*slog.Logger) for migration events.
	Logger *slog.Logger

	// Tracer specifies an optional tracer (OpenTelemetry or custom) for observing migrations.
	Tracer Tracer
}

// Tracer defines the interface for tracing Grizzle lifecycle events.
type Tracer = exec.Tracer

// Span represents an active trace span recorded by a Tracer.
type Span = exec.Span

// BackfillFunc defines the hook function signature for batch backfilling columns outside the DDL lock window.
type BackfillFunc func(ctx context.Context, tx *sql.Tx, table, oldCol, newCol string) error

// HookContext provides invocation context and database access for a step hook.
// DBTX is bound to the executor of the pending step: a *sql.Tx for
// transactional groups or a *sql.Conn for non-transactional steps.
type HookContext = exec.HookContext

// StepHook executes custom imperative code immediately before or after each
// plan step. Hooks must be idempotent: if a later step fails and the migration
// is retried or resumed after partial execution, hooks may be invoked again.
type StepHook = exec.StepHook

// SyncHook executes custom imperative code once before or after the entire
// synchronization. It receives a dedicated connection (not a transaction)
// because the execution may contain non-transactional statements.
type SyncHook = exec.SyncHook

// Option represents a functional option for configuring Options.
type Option func(*Options)

// WithLogger sets the structured logger.
func WithLogger(l *slog.Logger) Option {
	return func(o *Options) {
		o.Logger = l
	}
}

// WithTracer sets the tracer.
func WithTracer(t Tracer) Option {
	return func(o *Options) {
		o.Tracer = t
	}
}

// WithDialect sets the database dialect.
func WithDialect(d Dialect) Option {
	return func(o *Options) {
		o.Dialect = d
	}
}

// WithTargetSchema sets the target schema.
// Deprecated: Use WithTargetSchemas for multi-schema support.
func WithTargetSchema(schema string) Option {
	return func(o *Options) {
		o.TargetSchema = schema
		o.TargetSchemas = []string{schema}
	}
}

// WithTargetSchemas sets the target schemas to manage.
func WithTargetSchemas(schemas ...string) Option {
	return func(o *Options) {
		o.TargetSchemas = schemas
		if len(schemas) > 0 {
			o.TargetSchema = schemas[0]
		}
	}
}

// WithSQLiteAttach sets the SQLite ATTACH DATABASE map (schema name → path).
func WithSQLiteAttach(attach map[string]string) Option {
	return func(o *Options) {
		o.SQLiteAttach = attach
	}
}

// WithAllowDrop sets the general drop permission.
func WithAllowDrop(allow bool) Option {
	return func(o *Options) {
		o.AllowDrop = allow
	}
}

// WithExcludeTables sets excluded tables and glob patterns.
func WithExcludeTables(tables ...string) Option {
	return func(o *Options) {
		o.ExcludeTables = tables
	}
}

// WithIncludeTables sets included tables.
func WithIncludeTables(tables ...string) Option {
	return func(o *Options) {
		o.IncludeTables = tables
	}
}

// WithAcceptHazards configures explicitly accepted critical hazard codes.
func WithAcceptHazards(hazards ...plan.HazardCode) Option {
	return func(o *Options) {
		o.AcceptHazards = append(o.AcceptHazards, hazards...)
	}
}

// WithNonConcurrentIndexes controls whether PostgreSQL index creation should run inside the transaction.
func WithNonConcurrentIndexes(disabled bool) Option {
	return func(o *Options) {
		o.NonConcurrentIndexes = disabled
	}
}

// WithLockNamespace sets the application namespace string for PostgreSQL advisory locks.
func WithLockNamespace(ns string) Option {
	return func(o *Options) {
		o.LockNamespace = ns
	}
}

// WithLockTimeout sets the maximum duration to wait for acquiring locks.
func WithLockTimeout(d time.Duration) Option {
	return func(o *Options) {
		o.LockTimeout = d
	}
}

// WithStatementTimeout sets the maximum duration for any single migration DDL statement.
func WithStatementTimeout(d time.Duration) Option {
	return func(o *Options) {
		o.StatementTimeout = d
	}
}

// WithMaxRetries sets the maximum retry attempts upon lock timeout conflict (SQLSTATE 55P03).
func WithMaxRetries(n int) Option {
	return func(o *Options) {
		o.MaxRetries = n
	}
}

// WithRandFloat sets a custom random float function for deterministic backoff jitter in tests.
func WithRandFloat(fn func() float64) Option {
	return func(o *Options) {
		o.RandFloat = fn
	}
}

// WithStrictScope enables strict scoping mode requiring non-empty IncludeTables.
func WithStrictScope(strict bool) Option {
	return func(o *Options) {
		o.StrictScope = strict
	}
}

// WithSQLiteRebuildBatching sets the threshold and batch size for chunked keyset copying during SQLite rebuilds.
func WithSQLiteRebuildBatching(threshold, batchSize int) Option {
	return func(o *Options) {
		o.SQLiteRebuildThreshold = threshold
		o.SQLiteRebuildBatchSize = batchSize
	}
}

// Validate is the canonical pure validation entry point for Options.
// It checks SchemaSQL, strict scope, dialect value, target/shadow identifiers,
// timeout and retry bounds, SQLite rebuild parameters, and multi-schema SQLite
// rejection. Dialect-specific checks are skipped while Dialect is DialectAuto
// (prepareOptions detects the dialect then re-validates). Defaults are applied
// by prepareOptions after Validate succeeds — Validate never mutates Options.
func (o *Options) Validate() error {
	if strings.TrimSpace(o.SchemaSQL) == "" {
		return ErrEmptySchema
	}
	if o.StrictScope && len(o.IncludeTables) == 0 {
		return ErrStrictScope
	}
	switch o.Dialect {
	case DialectAuto, DialectPostgres, DialectSQLite:
	default:
		return fmt.Errorf("grizzle: unsupported dialect %q", o.Dialect)
	}
	if o.LockTimeout < 0 {
		return fmt.Errorf("%w: LockTimeout must be non-negative", ErrInvalidOptions)
	}
	if o.StatementTimeout < 0 {
		return fmt.Errorf("%w: StatementTimeout must be non-negative", ErrInvalidOptions)
	}
	if o.MaxRetries < 0 {
		return fmt.Errorf("%w: MaxRetries must be non-negative", ErrInvalidOptions)
	}
	if o.SQLiteRebuildThreshold < 0 {
		return fmt.Errorf("%w: SQLiteRebuildThreshold must be non-negative", ErrInvalidOptions)
	}
	if o.SQLiteRebuildBatchSize < 0 {
		return fmt.Errorf("%w: SQLiteRebuildBatchSize must be non-negative", ErrInvalidOptions)
	}

	switch o.Dialect {
	case DialectSQLite:
		if err := validateSQLiteAttach(o); err != nil {
			return err
		}
		if strings.TrimSpace(o.RolesSQL) != "" {
			return fmt.Errorf("%w: RolesSQL requires PostgreSQL; roles are not managed on SQLite", ErrInvalidOptions)
		}
		if strings.TrimSpace(o.CatalogSQL) != "" {
			return fmt.Errorf("%w: CatalogSQL requires PostgreSQL; publications and event triggers are not managed on SQLite", ErrInvalidOptions)
		}
	case DialectPostgres:
		idents := append([]string{}, o.TargetSchemas...)
		if o.TargetSchema != "" {
			idents = append(idents, o.TargetSchema)
		}
		if o.LockNamespace != "" {
			idents = append(idents, o.LockNamespace)
		}
		for _, id := range idents {
			if err := validateOptionIdent(id); err != nil {
				return err
			}
		}
		if o.ShadowSchema != "" && o.ShadowSchema != "_grizzle_shadow" {
			if len(o.ShadowSchema) > 63 {
				return fmt.Errorf("%w: shadow schema %q exceeds 63 bytes (PostgreSQL identifier limit)", ErrInvalidOptions, o.ShadowSchema)
			}
			if err := validateOptionIdent(o.ShadowSchema); err != nil {
				return fmt.Errorf("%w: shadow schema %q is not a valid SQL identifier", ErrInvalidOptions, o.ShadowSchema)
			}
		}
		groups := schema.ExtractStatements(o.SchemaSQL)
		if err := schema.ValidateRolesSQL(groups.RolesSQL); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidOptions, err)
		}
		if err := schema.ValidateCatalogSQL(groups.CatalogSQL); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidOptions, err)
		}
		if strings.TrimSpace(o.RolesSQL) != "" {
			if err := schema.ValidateRolesSQL(o.RolesSQL); err != nil {
				return fmt.Errorf("%w: %w", ErrInvalidOptions, err)
			}
		}
		if strings.TrimSpace(o.CatalogSQL) != "" {
			if err := schema.ValidateCatalogSQL(o.CatalogSQL); err != nil {
				return fmt.Errorf("%w: %w", ErrInvalidOptions, err)
			}
		}
		if err := schema.ValidateCatalogSpecMerge(
			schema.ParseCatalogSQL(groups.CatalogSQL),
			schema.ParseCatalogSQL(o.CatalogSQL),
		); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidOptions, err)
		}
	}
	return nil
}

var optionIdentRegex = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func validateOptionIdent(ident string) error {
	if ident == "" {
		return nil
	}
	if len(ident) > 63 {
		return fmt.Errorf("%w: identifier %q exceeds 63 bytes", ErrInvalidOptions, ident)
	}
	if !optionIdentRegex.MatchString(ident) {
		return fmt.Errorf("%w: identifier %q is not a valid SQL identifier", ErrInvalidOptions, ident)
	}
	return nil
}

// validateSQLiteAttach enforces TargetSchemas ↔ SQLiteAttach consistency for SQLite.
func validateSQLiteAttach(o *Options) error {
	targets := o.TargetSchemas
	if len(targets) == 0 && o.TargetSchema != "" {
		targets = []string{o.TargetSchema}
	}

	seenTarget := make(map[string]bool, len(targets))
	for _, name := range targets {
		if name == "" {
			return fmt.Errorf("%w: TargetSchemas entry must not be empty", ErrInvalidOptions)
		}
		if err := validateOptionIdent(name); err != nil {
			return err
		}
		if seenTarget[name] {
			return fmt.Errorf("%w: duplicate TargetSchemas entry %q", ErrInvalidOptions, name)
		}
		seenTarget[name] = true
	}

	seenAttach := make(map[string]bool, len(o.SQLiteAttach))
	for name, path := range o.SQLiteAttach {
		if name == "" {
			return fmt.Errorf("%w: SQLiteAttach key must not be empty", ErrInvalidOptions)
		}
		if name == "main" {
			return fmt.Errorf("%w: SQLiteAttach must not include %q (primary database is already open)", ErrInvalidOptions, name)
		}
		if err := validateOptionIdent(name); err != nil {
			return fmt.Errorf("%w: SQLiteAttach key: %w", ErrInvalidOptions, err)
		}
		if seenAttach[name] {
			return fmt.Errorf("%w: duplicate SQLiteAttach key %q", ErrInvalidOptions, name)
		}
		seenAttach[name] = true
		if strings.TrimSpace(path) == "" {
			return fmt.Errorf("%w: SQLiteAttach[%q] path must not be empty", ErrInvalidOptions, name)
		}
		if !seenTarget[name] {
			return fmt.Errorf("%w: SQLiteAttach key %q is not listed in TargetSchemas", ErrInvalidOptions, name)
		}
	}

	for name := range seenTarget {
		if name == "main" {
			continue
		}
		if _, ok := o.SQLiteAttach[name]; !ok {
			return fmt.Errorf("%w: TargetSchemas entry %q requires SQLiteAttach[%q] filesystem path", ErrInvalidOptions, name, name)
		}
	}
	return nil
}

// resolveDropPolicy extracts the effective fine-grained drop policy from Options.
func resolveDropPolicy(opts Options) plan.DropPolicy {
	allowTable := opts.AllowDrop
	if opts.AllowDropTable != nil {
		allowTable = *opts.AllowDropTable
	}

	allowColumn := opts.AllowDrop
	if opts.AllowDropColumn != nil {
		allowColumn = *opts.AllowDropColumn
	}

	allowIndex := opts.AllowDrop
	if opts.AllowDropIndex != nil {
		allowIndex = *opts.AllowDropIndex
	}

	allowFK := opts.AllowDrop
	if opts.AllowDropFK != nil {
		allowFK = *opts.AllowDropFK
	}

	allowCheck := opts.AllowDrop
	if opts.AllowDropCheck != nil {
		allowCheck = *opts.AllowDropCheck
	}

	allowExtension := opts.AllowDrop
	if opts.AllowDropExtension != nil {
		allowExtension = *opts.AllowDropExtension
	}

	allowFunction := opts.AllowDrop
	if opts.AllowDropFunction != nil {
		allowFunction = *opts.AllowDropFunction
	}

	allowPolicy := opts.AllowDrop
	if opts.AllowDropPolicy != nil {
		allowPolicy = *opts.AllowDropPolicy
	}

	allowTrigger := opts.AllowDrop
	if opts.AllowDropTrigger != nil {
		allowTrigger = *opts.AllowDropTrigger
	}

	allowView := opts.AllowDrop
	if opts.AllowDropView != nil {
		allowView = *opts.AllowDropView
	}

	allowDomain := opts.AllowDrop
	if opts.AllowDropDomain != nil {
		allowDomain = *opts.AllowDropDomain
	}

	// Privilege-loss gates follow the same blanket-override rule as every
	// other drop gate: they can also be set directly on Options so a plan
	// artifact's recorded policy round-trips through optionsFromPlan.
	// CRITICAL hazards still apply.
	allowRevoke := opts.AllowDrop || opts.AllowRevoke
	allowDropRole := opts.AllowDrop || opts.AllowDropRole
	allowDropPublication := opts.AllowDrop || opts.AllowDropPublication
	allowDropEventTrigger := opts.AllowDrop || opts.AllowDropEventTrigger
	allowDropSubscription := opts.AllowDrop || opts.AllowDropSubscription
	allowDropReplicationSlot := opts.AllowDrop || opts.AllowDropReplicationSlot

	return plan.DropPolicy{
		AllowTable:               allowTable,
		AllowColumn:              allowColumn,
		AllowIndex:               allowIndex,
		AllowFK:                  allowFK,
		AllowCheck:               allowCheck,
		AllowExtension:           allowExtension,
		AllowFunction:            allowFunction,
		AllowPolicy:              allowPolicy,
		AllowTrigger:             allowTrigger,
		AllowView:                allowView,
		AllowDomain:              allowDomain,
		AllowRevoke:              allowRevoke,
		AllowDropRole:            allowDropRole,
		AllowDropPublication:     allowDropPublication,
		AllowDropEventTrigger:    allowDropEventTrigger,
		AllowDropSubscription:    allowDropSubscription,
		AllowDropReplicationSlot: allowDropReplicationSlot,
	}
}

// WithRenames sets the explicit column rename mapping.
//
// Experimental: the rename mapping format and RENAME_AMBIGUOUS semantics
// may change before the 1.0 release.
func WithRenames(renames map[string]string) Option {
	return func(o *Options) {
		o.Renames = renames
	}
}

// WithExpandContract enables or disables staged expand-and-contract zero-downtime migrations.
//
// Experimental: staged expand-and-contract plans are under active development;
// the contract phase and hazard surface may change before the 1.0 release.
func WithExpandContract(expand bool) Option {
	return func(o *Options) {
		o.ExpandContract = expand
	}
}

// WithBackfill configures the batch backfill hook function for staged expand migrations.
//
// Experimental: backfill batching semantics (batch size, ordering, error
// handling) may change before the 1.0 release. There is no CLI equivalent;
// backfill is library-only.
func WithBackfill(fn BackfillFunc) Option {
	return func(o *Options) {
		o.Backfill = fn
	}
}

// WithBeforeSync registers a hook that runs once before any migration steps or
// locks are executed.
func WithBeforeSync(fn SyncHook) Option {
	return func(o *Options) {
		o.BeforeSync = fn
	}
}

// WithAfterSync registers a hook that runs once after all migration steps and
// history recording succeed.
func WithAfterSync(fn SyncHook) Option {
	return func(o *Options) {
		o.AfterSync = fn
	}
}

// WithBeforeStep registers a hook that runs immediately prior to each plan step.
func WithBeforeStep(fn StepHook) Option {
	return func(o *Options) {
		o.BeforeStep = fn
	}
}

// WithAfterStep registers a hook that runs immediately after each successful
// plan step.
func WithAfterStep(fn StepHook) Option {
	return func(o *Options) {
		o.AfterStep = fn
	}
}

// WithDryRun enables dry-run mode: the planned SQL is validated against the
// live database without executing it.
func WithDryRun() Option {
	return func(o *Options) {
		o.DryRun = true
	}
}

// WithDryRunLockTimeout sets the lock wait bound for live dry-run verification.
func WithDryRunLockTimeout(d time.Duration) Option {
	return func(o *Options) {
		o.DryRunLockTimeout = d
	}
}

// WithExecuteHooksInDryRun allows BeforeStep/AfterStep hooks to run during
// live dry-run verification.
func WithExecuteHooksInDryRun() Option {
	return func(o *Options) {
		o.ExecuteHooksInDryRun = true
	}
}

// WithSeedSQL attaches idempotent seed SQL executed after a successful sync.
func WithSeedSQL(seedSQL string) Option {
	return func(o *Options) {
		o.SeedSQL = seedSQL
	}
}

// WithSeedForce re-runs the seed even when the same seed hash was already
// applied.
func WithSeedForce(force bool) Option {
	return func(o *Options) {
		o.SeedForce = force
	}
}

func toScopeFilters(opts Options) scope.Filters {
	return scope.Filters{
		Includes:       opts.IncludeTables,
		Excludes:       opts.ExcludeTables,
		Strict:         opts.StrictScope,
		Renames:        opts.Renames,
		ExpandContract: opts.ExpandContract,
	}
}
