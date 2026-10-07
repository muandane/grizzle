package diff_test

import (
	"testing"

	"github.com/muandane/grizzle/internal/diff"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
	"github.com/muandane/grizzle/internal/scope"
)

func commentFixture(liveComment, colLiveComment, dComment, colDComment string) (*schema.Schema, *schema.Schema) {
	mkTable := func(tableComment, colComment string) *schema.Table {
		return &schema.Table{
			Name:    "docs",
			Comment: tableComment,
			Columns: map[string]*schema.Column{
				"id": {Name: "id", DataType: "bigint", IsNullable: false, Position: 1, Comment: colComment},
			},
			PrimaryKey: &schema.PrimaryKey{Name: "pk", Columns: []string{"id"}},
		}
	}
	live := &schema.Schema{Name: "public", Tables: map[string]*schema.Table{"docs": mkTable(liveComment, colLiveComment)}}
	desired := &schema.Schema{Name: "public", Tables: map[string]*schema.Table{"docs": mkTable(dComment, colDComment)}}
	return live, desired
}

func TestDiff_CommentDrift(t *testing.T) {
	tests := []struct {
		name              string
		liveTable, wantTd string
		liveCol, wantCd   string
		wantSteps         int
		wantTableOld      string
	}{
		{
			name: "no comments no steps",
		},
		{
			name:      "table comment set",
			wantTd:    "primary docs",
			wantSteps: 1,
		},
		{
			name:         "column comment set",
			wantCd:       "row id",
			wantSteps:    1,
			wantTableOld: "",
		},
		{
			name:         "both set",
			wantTd:       "t comment",
			wantCd:       "c comment",
			wantSteps:    2,
			wantTableOld: "",
		},
		{
			name:         "comment replaced",
			liveTable:    "old",
			wantTd:       "new",
			liveCol:      "old col",
			wantCd:       "new col",
			wantSteps:    2,
			wantTableOld: "old",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			live, desired := commentFixture(tt.liveTable, tt.liveCol, tt.wantTd, tt.wantCd)
			changes, err := diff.Diff(live, desired, "public", "", scope.Filters{})
			if err != nil {
				t.Fatal(err)
			}
			if len(changes) != tt.wantSteps {
				t.Fatalf("want %d changes, got %d: %+v", tt.wantSteps, len(changes), changes)
			}
			for _, c := range changes {
				if c.Type != plan.ChangeCommentTable && c.Type != plan.ChangeCommentColumn {
					t.Fatalf("unexpected change type %s", c.Type)
				}
				if c.Destructive {
					t.Fatalf("comment change must not be destructive")
				}
			}
			// OldComment carries the live value for reversal.
			for _, c := range changes {
				if c.Type == plan.ChangeCommentTable && c.OldComment != tt.wantTableOld {
					t.Fatalf("table OldComment = %q, want %q", c.OldComment, tt.wantTableOld)
				}
			}
		})
	}
}

func TestDiff_CommentClear(t *testing.T) {
	live, desired := commentFixture("keep me", "col note", "", "")
	changes, err := diff.Diff(live, desired, "public", "", scope.Filters{})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 {
		t.Fatalf("want 2 clearing changes, got %d", len(changes))
	}
	for _, c := range changes {
		if c.TableData != nil && c.TableData.Comment != "" {
			t.Fatalf("table comment should be empty, got %q", c.TableData.Comment)
		}
		if c.Column != nil && c.Column.Comment != "" {
			t.Fatalf("column comment should be empty, got %q", c.Column.Comment)
		}
	}
}
