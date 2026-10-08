package postgres_test

import (
	"testing"

	"github.com/muandane/grizzle/internal/dialect/postgres"
	"github.com/muandane/grizzle/internal/schema"
)

func TestGenerateCreateTriggerSQL(t *testing.T) {
	tests := []struct {
		name string
		trg  *schema.Trigger
		want string
	}{
		{
			name: "appends terminator",
			trg:  &schema.Trigger{Name: "trg_guard", Definition: "CREATE TRIGGER trg_guard BEFORE INSERT ON public.docs FOR EACH ROW EXECUTE FUNCTION trg_noop()"},
			want: "CREATE TRIGGER trg_guard BEFORE INSERT ON public.docs FOR EACH ROW EXECUTE FUNCTION trg_noop();",
		},
		{name: "nil trigger", trg: nil, want: ""},
		{name: "empty definition", trg: &schema.Trigger{Name: "t"}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := postgres.GenerateCreateTriggerSQL(tt.trg); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGenerateDropTriggerSQL(t *testing.T) {
	if got := postgres.GenerateDropTriggerSQL("public", "docs", "trg_guard"); got != `DROP TRIGGER IF EXISTS "trg_guard" ON "public"."docs";` {
		t.Fatalf("unexpected: %q", got)
	}
}
