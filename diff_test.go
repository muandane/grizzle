package grizzle

import (
	"testing"
)

func TestDiffSchemas_CreateTable(t *testing.T) {
	live := &SchemaIR{
		Name:   "public",
		Tables: make(map[string]*TableIR),
	}

	desired := &SchemaIR{
		Name: "_grizzle_shadow",
		Tables: map[string]*TableIR{
			"users": {
				Name: "users",
				Columns: map[string]*ColumnIR{
					"id": {
						Name:       "id",
						DataType:   "bigint",
						IsNullable: false,
						Position:   1,
					},
					"email": {
						Name:       "email",
						DataType:   "varchar(255)",
						IsNullable: false,
						Position:   2,
					},
				},
				PrimaryKey: &PrimaryKeyIR{
					Name:    "users_pkey",
					Columns: []string{"id"},
				},
			},
		},
	}

	steps := diffSchemas(live, desired, "public")
	if len(steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(steps))
	}

	if steps[0].Type != ChangeCreateTable {
		t.Errorf("expected ChangeCreateTable, got %s", steps[0].Type)
	}
	if steps[0].Destructive {
		t.Errorf("expected create table to be non-destructive")
	}
}

func TestDiffSchemas_AddColumn(t *testing.T) {
	live := &SchemaIR{
		Name: "public",
		Tables: map[string]*TableIR{
			"users": {
				Name: "users",
				Columns: map[string]*ColumnIR{
					"id": {Name: "id", DataType: "bigint", IsNullable: false, Position: 1},
				},
			},
		},
	}

	desired := &SchemaIR{
		Name: "_grizzle_shadow",
		Tables: map[string]*TableIR{
			"users": {
				Name: "users",
				Columns: map[string]*ColumnIR{
					"id":   {Name: "id", DataType: "bigint", IsNullable: false, Position: 1},
					"role": {Name: "role", DataType: "varchar(50)", IsNullable: false, DefaultValue: "'member'", Position: 2},
				},
			},
		},
	}

	steps := diffSchemas(live, desired, "public")
	if len(steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(steps))
	}

	if steps[0].Type != ChangeAddColumn {
		t.Errorf("expected ChangeAddColumn, got %s", steps[0].Type)
	}
	if steps[0].Destructive {
		t.Errorf("expected add column to be non-destructive")
	}
}

func TestDiffSchemas_DropColumn_Destructive(t *testing.T) {
	live := &SchemaIR{
		Name: "public",
		Tables: map[string]*TableIR{
			"users": {
				Name: "users",
				Columns: map[string]*ColumnIR{
					"id":   {Name: "id", DataType: "bigint", Position: 1},
					"temp": {Name: "temp", DataType: "text", Position: 2},
				},
			},
		},
	}

	desired := &SchemaIR{
		Name: "_grizzle_shadow",
		Tables: map[string]*TableIR{
			"users": {
				Name: "users",
				Columns: map[string]*ColumnIR{
					"id": {Name: "id", DataType: "bigint", Position: 1},
				},
			},
		},
	}

	steps := diffSchemas(live, desired, "public")
	if len(steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(steps))
	}

	if steps[0].Type != ChangeDropColumn {
		t.Errorf("expected ChangeDropColumn, got %s", steps[0].Type)
	}
	if !steps[0].Destructive {
		t.Errorf("expected drop column to be marked destructive")
	}
}
