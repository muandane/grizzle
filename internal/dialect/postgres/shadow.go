package postgres

import (
	"context"
	"fmt"
	"regexp"

	"github.com/yourorg/grizzle/internal/dialect"
)

var validIdentRegex = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// ValidateIdentifier ensures schema names contain only safe SQL identifier characters.
func ValidateIdentifier(ident string) error {
	if !validIdentRegex.MatchString(ident) {
		return fmt.Errorf("invalid SQL identifier: %q", ident)
	}
	return nil
}

// SetupShadowSchema creates a clean, isolated temporary schema for DDL compilation.
func SetupShadowSchema(ctx context.Context, dbtx dialect.DBTX, shadowSchema string) error {
	if err := ValidateIdentifier(shadowSchema); err != nil {
		return err
	}

	cleanSQL := fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE; CREATE SCHEMA %q;", shadowSchema, shadowSchema)
	if _, err := dbtx.ExecContext(ctx, cleanSQL); err != nil {
		return fmt.Errorf("failed creating shadow schema %q: %w", shadowSchema, err)
	}
	return nil
}

// RunShadowDDL sets search_path to the shadow schema, runs the user's schemaSQL, and restores search_path.
func RunShadowDDL(ctx context.Context, dbtx dialect.DBTX, shadowSchema, targetSchema, schemaSQL string) error {
	if err := ValidateIdentifier(shadowSchema); err != nil {
		return err
	}
	if err := ValidateIdentifier(targetSchema); err != nil {
		return err
	}

	setPathSQL := fmt.Sprintf("SET LOCAL search_path TO %q, public;", shadowSchema)
	if _, err := dbtx.ExecContext(ctx, setPathSQL); err != nil {
		return fmt.Errorf("failed setting search_path to %q: %w", shadowSchema, err)
	}

	if _, err := dbtx.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("schema compilation in shadow schema failed: %w", err)
	}

	restorePathSQL := fmt.Sprintf("SET LOCAL search_path TO %q, public;", targetSchema)
	if _, err := dbtx.ExecContext(ctx, restorePathSQL); err != nil {
		return fmt.Errorf("failed restoring search_path to %q: %w", targetSchema, err)
	}

	return nil
}

// DropShadowSchema safely destroys the temporary shadow schema.
func DropShadowSchema(ctx context.Context, dbtx dialect.DBTX, shadowSchema string) error {
	if err := ValidateIdentifier(shadowSchema); err != nil {
		return err
	}
	dropSQL := fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE;", shadowSchema)
	if _, err := dbtx.ExecContext(ctx, dropSQL); err != nil {
		return fmt.Errorf("failed dropping shadow schema %q: %w", shadowSchema, err)
	}
	return nil
}
