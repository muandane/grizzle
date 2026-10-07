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
// persistent database.
func CompileSchemaSQLite(ctx context.Context, schemaSQL string) (*schema.Schema, error) {
	s, err := sqlite.CompileInShadow(ctx, schemaSQL)
	if err != nil {
		return nil, err
	}
	s.SourceSQL = schemaSQL
	return s, nil
}

// CompileSchemaPostgres compiles schemaSQL in the shadow schema within a
// single always-rolled-back transaction and returns the desired schema IR.
// The shadow schema is created and dropped as part of the transaction, so no
// durable state is modified.
func CompileSchemaPostgres(ctx context.Context, db *sql.DB, cfg PostgresExecConfig) (*schema.Schema, error) {
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
	if err := postgres.RunShadowDDL(ctx, tx, shadowSchema, primarySchema, cfg.SchemaSQL); err != nil {
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
	s.SourceSQL = cfg.SchemaSQL
	return s, nil
}
