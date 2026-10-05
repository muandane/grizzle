package grizzle

import (
	"testing"
)

func TestDiffSchemas_CreateTable(t *testing.T) {
	live := &SchemaIR{
		Name:   "public",
		Tables: make(map[string]*TableIR),
		Enums:  make(map[string]*EnumIR),
	}

	desired := &SchemaIR{
		Name:  "_grizzle_shadow",
		Enums: make(map[string]*EnumIR),
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

	steps := diffSchemas(live, desired, "public", "_grizzle_shadow", tableFilters{})
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
		Name:  "public",
		Enums: make(map[string]*EnumIR),
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
		Name:  "_grizzle_shadow",
		Enums: make(map[string]*EnumIR),
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

	steps := diffSchemas(live, desired, "public", "_grizzle_shadow", tableFilters{})
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
		Name:  "public",
		Enums: make(map[string]*EnumIR),
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
		Name:  "_grizzle_shadow",
		Enums: make(map[string]*EnumIR),
		Tables: map[string]*TableIR{
			"users": {
				Name: "users",
				Columns: map[string]*ColumnIR{
					"id": {Name: "id", DataType: "bigint", Position: 1},
				},
			},
		},
	}

	steps := diffSchemas(live, desired, "public", "_grizzle_shadow", tableFilters{})
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

func TestDiffSchemas_Enums(t *testing.T) {
	live := &SchemaIR{
		Name:   "public",
		Tables: make(map[string]*TableIR),
		Enums: map[string]*EnumIR{
			"status": {Name: "status", Values: []string{"active", "inactive"}},
		},
	}

	desired := &SchemaIR{
		Name:   "_grizzle_shadow",
		Tables: make(map[string]*TableIR),
		Enums: map[string]*EnumIR{
			"status": {Name: "status", Values: []string{"active", "inactive", "archived"}},
			"role":   {Name: "role", Values: []string{"admin", "member"}},
		},
	}

	steps := diffSchemas(live, desired, "public", "_grizzle_shadow", tableFilters{})
	if len(steps) != 2 {
		t.Fatalf("expected 2 steps (1 create enum, 1 alter enum), got %d", len(steps))
	}

	// Step 1: Create new role enum
	if steps[0].Type != ChangeCreateEnum || steps[0].Table != "role" {
		t.Errorf("expected ChangeCreateEnum for role, got %s on %s", steps[0].Type, steps[0].Table)
	}

	// Step 2: Alter status enum to add archived
	if steps[1].Type != ChangeAlterEnum || steps[1].Table != "status" {
		t.Errorf("expected ChangeAlterEnum for status, got %s on %s", steps[1].Type, steps[1].Table)
	}
}

func TestDiffSchemas_IndexesAndForeignKeys(t *testing.T) {
	live := &SchemaIR{
		Name:  "public",
		Enums: make(map[string]*EnumIR),
		Tables: map[string]*TableIR{
			"users": {
				Name:        "users",
				Columns:     map[string]*ColumnIR{"id": {Name: "id", DataType: "bigint", Position: 1}},
				Indexes:     map[string]*IndexIR{"old_idx": {Name: "old_idx", TableName: "users", Definition: "CREATE INDEX old_idx ON public.users (id)"}},
				ForeignKeys: make(map[string]*ForeignKeyIR),
			},
		},
	}

	desired := &SchemaIR{
		Name:  "_grizzle_shadow",
		Enums: make(map[string]*EnumIR),
		Tables: map[string]*TableIR{
			"users": {
				Name:    "users",
				Columns: map[string]*ColumnIR{"id": {Name: "id", DataType: "bigint", Position: 1}},
				Indexes: map[string]*IndexIR{
					"idx_users_id": {Name: "idx_users_id", TableName: "users", Definition: "CREATE INDEX idx_users_id ON _grizzle_shadow.users (id)"},
				},
				ForeignKeys: map[string]*ForeignKeyIR{
					"fk_users_org": {Name: "fk_users_org", TableName: "users", Definition: "FOREIGN KEY (org_id) REFERENCES _grizzle_shadow.orgs(id)"},
				},
			},
		},
	}

	steps := diffSchemas(live, desired, "public", "_grizzle_shadow", tableFilters{})

	// Expect:
	// 1. Drop old_idx (Priority 20)
	// 2. Create idx_users_id (Priority 70)
	// 3. Add fk_users_org (Priority 80)
	if len(steps) != 3 {
		t.Fatalf("expected 3 steps, got %d", len(steps))
	}

	if steps[0].Type != ChangeDropIndex || steps[0].SQL != `DROP INDEX IF EXISTS "public"."old_idx";` {
		t.Errorf("expected ChangeDropIndex, got %+v", steps[0])
	}
	if steps[1].Type != ChangeCreateIndex {
		t.Errorf("expected ChangeCreateIndex, got %+v", steps[1])
	}
	if steps[2].Type != ChangeAddFK {
		t.Errorf("expected ChangeAddFK, got %+v", steps[2])
	}
}
