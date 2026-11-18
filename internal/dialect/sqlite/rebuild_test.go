package sqlite_test

import (
	"slices"
	"testing"

	"github.com/muandane/grizzle/internal/dialect/sqlite"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
	"github.com/muandane/grizzle/internal/scope"
)

func TestSQLite_RebuildChunkedParentheses(t *testing.T) {
	live := &schema.Schema{
		Name: "main",
		Tables: map[string]*schema.Table{
			"items": {
				Name: "items",
				Columns: map[string]*schema.Column{
					"id": {
						Name:     "id",
						DataType: "INTEGER",
						Position: 1,
					},
					"val": {
						Name:         "val",
						DataType:     "TEXT",
						DefaultValue: "(COALESCE('default', ''))",
						Position:     2,
					},
					"old_col": {
						Name:     "old_col",
						DataType: "TEXT",
						Position: 3,
					},
				},
				PrimaryKey: &schema.PrimaryKey{
					Columns: []string{"id"},
				},
			},
		},
	}

	desired := &schema.Schema{
		Name: "main",
		Tables: map[string]*schema.Table{
			"items": {
				Name: "items",
				Columns: map[string]*schema.Column{
					"id": {
						Name:     "id",
						DataType: "INTEGER",
						Position: 1,
					},
					"val": {
						Name:         "val",
						DataType:     "TEXT",
						DefaultValue: "(COALESCE('default', ''))",
						Position:     2,
					},
				},
				PrimaryKey: &schema.PrimaryKey{
					Columns: []string{"id"},
				},
			},
		},
	}

	steps := sqlite.Diff(live, desired, scope.Filters{})
	if len(steps) == 0 {
		t.Fatal("expected at least one step for table rebuild")
	}

	var rebuildStep *plan.Step
	for i := range steps {
		if steps[i].IsTableRebuild {
			rebuildStep = &steps[i]
			break
		}
	}

	if rebuildStep == nil {
		t.Fatal("expected IsTableRebuild step")
	}

	expectedCols := []string{"id", "val"}
	if !slices.Equal(rebuildStep.Columns, expectedCols) {
		t.Fatalf("rebuildStep.Columns = %v, want %v", rebuildStep.Columns, expectedCols)
	}
}
