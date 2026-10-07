package exec

import (
	"fmt"

	"github.com/muandane/grizzle/internal/schema"
)

func splitSchemaSQL(schemaSQL string) (schema.StatementGroups, error) {
	groups := schema.ExtractStatements(schemaSQL)
	if err := schema.ValidateRolesSQL(groups.RolesSQL); err != nil {
		return schema.StatementGroups{}, fmt.Errorf("validating extracted role statements: %w", err)
	}
	if err := schema.ValidateCatalogSQL(groups.CatalogSQL); err != nil {
		return schema.StatementGroups{}, fmt.Errorf("validating extracted catalog statements: %w", err)
	}
	return groups, nil
}
