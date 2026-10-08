package exec

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/muandane/grizzle/internal/dialect/postgres"
	"github.com/muandane/grizzle/internal/dialect/sqlite"
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
		return s, nil
	}
	// Merge attached schemas into one IR; table keys are schema-qualified to
	// avoid collisions across ATTACH databases.
	merged := &schema.Schema{
		Name:      schemas[0],
		Tables:    make(map[string]*schema.Table),
		Enums:     make(map[string]*schema.Enum),
		Unmanaged: make(map[string]*schema.UnmanagedObject),
		SourceSQL: schemaSQL,
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
func CompileSchemaPostgres(ctx context.Context, db *sql.DB, cfg PostgresExecConfig) (*schema.Schema, error) {
	groups, err := splitSchemaSQL(cfg.SchemaSQL)
	if err != nil {
		return nil, err
	}

	primarySchema := cfg.primarySchema()
	shadowSchema := cfg.ShadowSchema
	if shadowSchema == "" {
		shadowSchema = "_grizzle_shadow"
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("grizzle: failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := postgres.SetupShadowSchema(ctx, tx, shadowSchema); err != nil {
		return nil, err
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
	// Desired extensions come from statement parsing, not the shadow catalog:
	// best-effort installs roll back with the tx and may have failed on
	// privileges, but the declared set is what the user wrote.
	s.Extensions = schema.ParseExtensions(cfg.SchemaSQL)

	// Shadow-installed functions (e.g. extension functions re-created via
	// WITH SCHEMA rewriting) qualify expressions with the ephemeral shadow
	// schema name, which is unique per run. Left in place they break rendered
	// target DDL and cause permanent drift.
	unmapShadowExprs(s, []string{shadowSchema})

	s.SourceSQL = cfg.SchemaSQL
	return s, nil
}
