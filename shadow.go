package grizzle

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
)

var validIdentRegex = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// validateIdentifier ensures schema names contain only valid safe SQL identifier characters.
func validateIdentifier(ident string) error {
	if !validIdentRegex.MatchString(ident) {
		return fmt.Errorf("invalid SQL identifier: %q", ident)
	}
	return nil
}

// setupShadowSchema creates a clean, isolated temporary schema for DDL compilation.
func setupShadowSchema(ctx context.Context, tx *sql.Tx, shadowSchema string) error {
	if err := validateIdentifier(shadowSchema); err != nil {
		return err
	}

	cleanSQL := fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE; CREATE SCHEMA %q;", shadowSchema, shadowSchema)
	if _, err := tx.ExecContext(ctx, cleanSQL); err != nil {
		return fmt.Errorf("failed creating shadow schema %q: %w", shadowSchema, err)
	}
	return nil
}

// runShadowDDL sets the search_path to the shadow schema, runs the user's schemaSQL, and resets the search_path.
func runShadowDDL(ctx context.Context, tx *sql.Tx, shadowSchema, targetSchema, schemaSQL string) error {
	if err := validateIdentifier(shadowSchema); err != nil {
		return err
	}
	if err := validateIdentifier(targetSchema); err != nil {
		return err
	}

	// Route all unqualified CREATE TABLE / INDEX statements to the shadow schema
	setPathSQL := fmt.Sprintf("SET LOCAL search_path TO %q, public;", shadowSchema)
	if _, err := tx.ExecContext(ctx, setPathSQL); err != nil {
		return fmt.Errorf("failed setting search_path to %q: %w", shadowSchema, err)
	}

	// Execute user's schema.sql
	if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("%w: %v", ErrCompilationFailed, err)
	}

	// Restore search_path back to targetSchema
	restorePathSQL := fmt.Sprintf("SET LOCAL search_path TO %q, public;", targetSchema)
	if _, err := tx.ExecContext(ctx, restorePathSQL); err != nil {
		return fmt.Errorf("failed restoring search_path to %q: %w", targetSchema, err)
	}

	return nil
}

// dropShadowSchema safely destroys the temporary shadow schema.
func dropShadowSchema(ctx context.Context, tx *sql.Tx, shadowSchema string) error {
	if err := validateIdentifier(shadowSchema); err != nil {
		return err
	}
	dropSQL := fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE;", shadowSchema)
	if _, err := tx.ExecContext(ctx, dropSQL); err != nil {
		return fmt.Errorf("failed dropping shadow schema %q: %w", shadowSchema, err)
	}
	return nil
}
