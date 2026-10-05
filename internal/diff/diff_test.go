package diff_test

import (
	"testing"

	"github.com/muandane/grizzle/internal/diff"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
	"github.com/muandane/grizzle/internal/scope"
)

func TestDiff_PureUnit(t *testing.T) {
	filters := scope.Filters{}

	// 1. Create table
	liveEmpty := &schema.Schema{Tables: make(map[string]*schema.Table), Enums: make(map[string]*schema.Enum)}
	desiredTable := &schema.Schema{
		Tables: map[string]*schema.Table{
			"users": {
				Name: "users",
				Columns: map[string]*schema.Column{
					"id": {Name: "id", DataType: "bigint", IsNullable: false},
				},
				Indexes:     make(map[string]*schema.Index),
				ForeignKeys: make(map[string]*schema.ForeignKey),
			},
		},
		Enums: make(map[string]*schema.Enum),
	}

	changes := diff.Diff(liveEmpty, desiredTable, "shadow", "public", filters)
	if len(changes) == 0 || changes[0].Type != plan.ChangeCreateTable {
		t.Fatalf("expected ChangeCreateTable, got: %+v", changes)
	}

	// 2. Drop table
	changesDrop := diff.Diff(desiredTable, liveEmpty, "shadow", "public", filters)
	if len(changesDrop) == 0 || changesDrop[0].Type != plan.ChangeDropTable {
		t.Fatalf("expected ChangeDropTable, got: %+v", changesDrop)
	}

	// 3. Add & Drop column
	liveWithCol := &schema.Schema{
		Tables: map[string]*schema.Table{
			"users": {
				Name: "users",
				Columns: map[string]*schema.Column{
					"id":    {Name: "id", DataType: "bigint", IsNullable: false},
					"email": {Name: "email", DataType: "varchar(255)", IsNullable: false},
				},
				Indexes:     make(map[string]*schema.Index),
				ForeignKeys: make(map[string]*schema.ForeignKey),
			},
		},
		Enums: make(map[string]*schema.Enum),
	}

	// Adding email column
	changesAddCol := diff.Diff(desiredTable, liveWithCol, "shadow", "public", filters)
	var foundAddCol bool
	for _, c := range changesAddCol {
		if c.Type == plan.ChangeAddColumn && c.Column.Name == "email" {
			foundAddCol = true
			if !c.ColumnNotNull {
				t.Errorf("expected ColumnNotNull to be true")
			}
		}
	}
	if !foundAddCol {
		t.Fatalf("expected ChangeAddColumn for email, got: %+v", changesAddCol)
	}

	// Dropping email column
	changesDropCol := diff.Diff(liveWithCol, desiredTable, "shadow", "public", filters)
	var foundDropCol bool
	for _, c := range changesDropCol {
		if c.Type == plan.ChangeDropColumn && c.OldColumn.Name == "email" {
			foundDropCol = true
			if !c.Destructive {
				t.Errorf("expected drop column to be destructive")
			}
		}
	}
	if !foundDropCol {
		t.Fatalf("expected ChangeDropColumn for email, got: %+v", changesDropCol)
	}

	// 4. Alter column (type change)
	liveAltered := &schema.Schema{
		Tables: map[string]*schema.Table{
			"users": {
				Name: "users",
				Columns: map[string]*schema.Column{
					"id": {Name: "id", DataType: "integer", IsNullable: false}, // narrowed from bigint
				},
				Indexes:     make(map[string]*schema.Index),
				ForeignKeys: make(map[string]*schema.ForeignKey),
			},
		},
		Enums: make(map[string]*schema.Enum),
	}
	changesAlter := diff.Diff(desiredTable, liveAltered, "shadow", "public", filters)
	var foundAlter bool
	for _, c := range changesAlter {
		if c.Type == plan.ChangeAlterColumn {
			foundAlter = true
			if !c.TypeNarrowed {
				t.Errorf("expected TypeNarrowed to be true when converting bigint to integer")
			}
		}
	}
	if !foundAlter {
		t.Fatalf("expected ChangeAlterColumn, got: %+v", changesAlter)
	}

	// 5. Enums (Create and Alter)
	liveEnum := &schema.Schema{
		Tables: make(map[string]*schema.Table),
		Enums: map[string]*schema.Enum{
			"status": {Name: "status", Values: []string{"active"}},
		},
	}
	desiredEnum := &schema.Schema{
		Tables: make(map[string]*schema.Table),
		Enums: map[string]*schema.Enum{
			"status": {Name: "status", Values: []string{"active", "archived"}},
			"role":   {Name: "role", Values: []string{"admin", "user"}},
		},
	}
	changesEnum := diff.Diff(liveEnum, desiredEnum, "shadow", "public", filters)
	var foundCreateRole, foundAlterStatus bool
	for _, c := range changesEnum {
		if c.Type == plan.ChangeCreateEnum && c.Enum.Name == "role" {
			foundCreateRole = true
		}
		if c.Type == plan.ChangeAlterEnum && c.Enum.Name == "status" && c.EnumValue == "archived" {
			foundAlterStatus = true
		}
	}
	if !foundCreateRole || !foundAlterStatus {
		t.Fatalf("expected enum changes, got: %+v", changesEnum)
	}

	// 6. Indexes (Add, Drop, Invalid repair)
	liveIdx := &schema.Schema{
		Tables: map[string]*schema.Table{
			"users": {
				Name: "users",
				Columns: map[string]*schema.Column{
					"id": {Name: "id", DataType: "bigint", IsNullable: false},
				},
				Indexes: map[string]*schema.Index{
					"idx_users_id": {Name: "idx_users_id", Definition: "CREATE INDEX idx_users_id ON users (id)", IsValid: false},
				},
				ForeignKeys: make(map[string]*schema.ForeignKey),
			},
		},
		Enums: make(map[string]*schema.Enum),
	}
	desiredIdx := &schema.Schema{
		Tables: map[string]*schema.Table{
			"users": {
				Name: "users",
				Columns: map[string]*schema.Column{
					"id": {Name: "id", DataType: "bigint", IsNullable: false},
				},
				Indexes: map[string]*schema.Index{
					"idx_users_id": {Name: "idx_users_id", Definition: "CREATE INDEX idx_users_id ON users (id)", IsValid: true},
				},
				ForeignKeys: make(map[string]*schema.ForeignKey),
			},
		},
		Enums: make(map[string]*schema.Enum),
	}
	changesIdx := diff.Diff(liveIdx, desiredIdx, "shadow", "public", filters)
	var foundDropIdx, foundAddIdx bool
	for _, c := range changesIdx {
		if c.Type == plan.ChangeDropIndex {
			foundDropIdx = true
		}
		if c.Type == plan.ChangeCreateIndex {
			foundAddIdx = true
		}
	}
	if !foundDropIdx || !foundAddIdx {
		t.Fatalf("expected invalid index recovery (DROP + CREATE), got: %+v", changesIdx)
	}
}
