package exec

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/muandane/grizzle/internal/dialect/postgres"
	"github.com/muandane/grizzle/internal/dialect/sqlite"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
)

// CompileSchemaSQLite compiles schemaSQL in an isolated in-memory SQLite
// shadow database and returns the desired schema IR without touching any
// persistent database. When schemas lists attached names, empty :memory:
// databases are ATTACHed so multi-schema DDL compiles without live files.
func CompileSchemaSQLite(ctx context.Context, schemaSQL string, schemas ...string) (*schema.Schema, error) {
	if len(schemas) == 0 {
		schemas = []string{"main"}
	}
	m, err := sqlite.CompileInShadowSchemas(ctx, schemaSQL, schemas)
	if err != nil {
		return nil, err
	}
	if len(schemas) == 1 {
		s := m[schemas[0]]
		if s == nil {
			return nil, fmt.Errorf("sqlite: shadow schema %q missing after compile", schemas[0])
		}
		s.SourceSQL = schemaSQL
		s.Dialect = "sqlite"
		return s, nil
	}
	// Merge attached schemas into one IR; table keys are schema-qualified to
	// avoid collisions across ATTACH databases.
	merged := &schema.Schema{
		Name:           schemas[0],
		Tables:         make(map[string]*schema.Table),
		Enums:          make(map[string]*schema.Enum),
		RangeTypes:     make(map[string]*schema.RangeType),
		CompositeTypes: make(map[string]*schema.CompositeType),
		Sequences:      make(map[string]*schema.Sequence),
		Unmanaged:      make(map[string]*schema.UnmanagedObject),
		Dialect:        "sqlite",
		SourceSQL:      schemaSQL,
	}
	for _, sch := range schemas {
		s := m[sch]
		if s == nil {
			continue
		}
		for name, tbl := range s.Tables {
			key := name
			if sch != "main" {
				key = sch + "." + name
			}
			merged.Tables[key] = tbl
		}
		for k, u := range s.Unmanaged {
			merged.Unmanaged[sch+":"+k] = u
		}
	}
	return merged, nil
}

// CompileSchemaPostgres compiles schemaSQL in the shadow schema within a
// single always-rolled-back transaction and returns the desired schema IR.
// The shadow schema is created and dropped as part of the transaction, so no
// durable state is modified.
//
// When cfg.DevDB is non-nil, the shadow compilation transaction runs against
// cfg.DevDB instead of db. This lets callers compile against an external
// database when the target database user lacks CREATE SCHEMA privilege.
func CompileSchemaPostgres(ctx context.Context, db *sql.DB, cfg PostgresExecConfig) (*schema.Schema, error) {
	groups, err := splitSchemaSQL(cfg.SchemaSQL)
	if err != nil {
		return nil, err
	}

	primarySchema := cfg.primarySchema()
	shadowSchema := cfg.ShadowSchema
	if shadowSchema == "" {
		shadowSchema = uniqueShadowName("_grizzle_shadow")
	}

	// Route the shadow transaction to DevDB when configured.
	shadowDB := db
	if cfg.DevDB != nil {
		shadowDB = cfg.DevDB
	}

	tx, err := shadowDB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("grizzle: failed to begin shadow transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := postgres.SetupShadowSchema(ctx, tx, shadowSchema); err != nil {
		return nil, wrapShadowSetupError(err, cfg.DevDB != nil)
	}
	if err := postgres.RunShadowDDL(ctx, tx, shadowSchema, primarySchema, groups.ShadowSQL); err != nil {
		return nil, err
	}

	s, err := inspectPostgresSchema(ctx, tx, shadowSchema, cfg.Tracer)
	if err != nil {
		return nil, fmt.Errorf("grizzle: inspecting shadow schema: %w", err)
	}
	s.Name = primarySchema
	for _, t := range s.Tables {
		t.Schema = primarySchema
	}
	for _, r := range s.RangeTypes {
		r.Schema = primarySchema
	}
	for _, c := range s.CompositeTypes {
		c.Schema = primarySchema
	}
	for _, seq := range s.Sequences {
		seq.Schema = primarySchema
		delete(s.Unmanaged, "sequence:"+seq.Name)
	}
	// Desired extensions come from statement parsing, not the shadow catalog:
	// best-effort installs roll back with the tx and may have failed on
	// privileges, but the declared set is what the user wrote.
	s.Extensions = schema.ParseExtensions(cfg.SchemaSQL)

	// Shadow-installed functions (e.g. extension functions re-created via
	// WITH SCHEMA rewriting) qualify expressions with the ephemeral shadow
	// schema name, which is unique per run. Left in place they break rendered
	// target DDL and cause permanent drift.
	unmapShadowExprs(s, []string{shadowSchema}, primarySchema)

	s.SourceSQL = cfg.SchemaSQL
	s.Dialect = "postgres"
	return s, nil
}

// wrapShadowSetupError wraps a shadow schema setup error with an actionable
// message. When hasDevDB is false and the error looks like a privilege failure
// (PostgreSQL SQLSTATE 42501: insufficient_privilege), the message explicitly
// names --dev-url / Options.DevDatabaseURL as the fix.
func wrapShadowSetupError(err error, hasDevDB bool) error {
	if err == nil {
		return nil
	}
	if !hasDevDB && isPrivilegeError(err) {
		return fmt.Errorf("%w: target database user lacks CREATE SCHEMA privilege required for shadow compilation; "+
			"supply an external database via --dev-url / Options.DevDatabaseURL, "+
			"or GRANT CREATE ON DATABASE to the migration user: %w",
			plan.ErrCompilationFailed, err)
	}
	return err
}

// isPrivilegeError reports whether err is a PostgreSQL insufficient_privilege
// (SQLSTATE 42501) or contains equivalent text.
func isPrivilegeError(err error) bool {
	if err == nil {
		return false
	}
	type sqlStater interface{ SQLState() string }
	if s, ok := err.(sqlStater); ok && s.SQLState() == "42501" { //nolint:errorlint // intentional interface check on concrete err
		return true
	}
	// Fallback: match pgx PgError via error unwrapping.
	errStr := err.Error()
	return strings.Contains(errStr, "42501") ||
		strings.Contains(errStr, "insufficient_privilege") ||
		strings.Contains(errStr, "permission denied to create schema")
}
