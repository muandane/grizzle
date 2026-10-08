package postgres_test

import (
	"testing"

	"github.com/muandane/grizzle/internal/dialect/postgres"
	"github.com/muandane/grizzle/internal/diff"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
)

func TestRender_CommentSQL(t *testing.T) {
	tests := []struct {
		name    string
		table   string
		column  string
		comment string
		want    string
	}{
		{
			name:    "table comment",
			table:   "docs",
			comment: "primary docs table",
			want:    `COMMENT ON TABLE "public"."docs" IS 'primary docs table';`,
		},
		{
			name:    "column comment",
			table:   "docs",
			column:  "id",
			comment: "row id",
			want:    `COMMENT ON COLUMN "public"."docs"."id" IS 'row id';`,
		},
		{
			name:    "quote escaping",
			table:   "docs",
			comment: "it's here",
			want:    `COMMENT ON TABLE "public"."docs" IS 'it''s here';`,
		},
		{
			name:  "clear",
			table: "docs",
			want:  `COMMENT ON TABLE "public"."docs" IS '';`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := postgres.GenerateCommentSQL("public", tt.table, tt.column, tt.comment)
			if got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestRender_ChangeCommentSteps(t *testing.T) {
	changes := []diff.Change{
		{
			Type:  plan.ChangeCommentTable,
			Table: "docs",
			TableData: &schema.Table{
				Name:    "docs",
				Comment: "hello",
			},
			OldComment: "old",
		},
		{
			Type:  plan.ChangeCommentColumn,
			Table: "docs",
			Column: &schema.Column{
				Name:    "id",
				Comment: "col note",
			},
		},
	}
	steps := postgres.RenderChanges("public", changes)
	if len(steps) != 2 {
		t.Fatalf("want 2 steps, got %d", len(steps))
	}
	got := map[plan.ChangeType]plan.Step{}
	for _, s := range steps {
		got[s.Type] = s
	}
	if tbl, ok := got[plan.ChangeCommentTable]; !ok || tbl.SQL != `COMMENT ON TABLE "public"."docs" IS 'hello';` {
		t.Fatalf("unexpected table comment step: %+v", tbl)
	} else if tbl.OldComment != "old" {
		t.Fatalf("OldComment not carried: %q", tbl.OldComment)
	}
	if col, ok := got[plan.ChangeCommentColumn]; !ok || col.SQL != `COMMENT ON COLUMN "public"."docs"."id" IS 'col note';` {
		t.Fatalf("unexpected column comment step: %+v", col)
	}
}
