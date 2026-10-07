package schema

import (
	"strings"
	"testing"
)

func TestParseCatalogSQL_Publications(t *testing.T) {
	tests := []struct {
		name    string
		sql     string
		want    *Publication
		wantLen int
	}{
		{
			name:    "all tables",
			sql:     "CREATE PUBLICATION events_pub FOR ALL TABLES;",
			wantLen: 1,
			want: &Publication{
				Name: "events_pub", AllTables: true,
				PublishInsert: true, PublishUpdate: true, PublishDelete: true, PublishTruncate: true,
			},
		},
		{
			name:    "table list",
			sql:     `CREATE PUBLICATION docs_pub FOR TABLE public.docs, public.comments;`,
			wantLen: 1,
			want: &Publication{
				Name: "docs_pub", Tables: []string{"public.docs", "public.comments"},
				PublishInsert: true, PublishUpdate: true, PublishDelete: true, PublishTruncate: true,
			},
		},
		{
			name:    "schema list",
			sql:     "CREATE PUBLICATION analytics_pub FOR TABLES IN SCHEMA analytics, reporting;",
			wantLen: 1,
			want: &Publication{
				Name: "analytics_pub", Schemas: []string{"analytics", "reporting"},
				PublishInsert: true, PublishUpdate: true, PublishDelete: true, PublishTruncate: true,
			},
		},
		{
			name:    "publish flags narrow",
			sql:     "CREATE PUBLICATION ins_only WITH (publish = 'insert');",
			wantLen: 1,
			want: &Publication{
				Name:          "ins_only",
				PublishInsert: true, PublishUpdate: false, PublishDelete: false, PublishTruncate: false,
			},
		},
		{
			name:    "publish flags subset",
			sql:     `CREATE PUBLICATION upd_del WITH (publish = 'insert, update, delete');`,
			wantLen: 1,
			want: &Publication{
				Name:          "upd_del",
				PublishInsert: true, PublishUpdate: true, PublishDelete: true, PublishTruncate: false,
			},
		},
		{
			name:    "quoted name and membership combo",
			sql:     `CREATE PUBLICATION "mixed.pub" FOR TABLE docs WITH (publish = 'insert, truncate');`,
			wantLen: 1,
			want: &Publication{
				Name: "mixed.pub", Tables: []string{"docs"},
				PublishInsert: true, PublishUpdate: false, PublishDelete: false, PublishTruncate: true,
			},
		},
		{
			name:    "no membership",
			sql:     "CREATE PUBLICATION empty_pub;",
			wantLen: 1,
			want: &Publication{
				Name:          "empty_pub",
				PublishInsert: true, PublishUpdate: true, PublishDelete: true, PublishTruncate: true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := ParseCatalogSQL(tt.sql)
			if len(spec.Publications) != tt.wantLen {
				t.Fatalf("got %d publications, want %d", len(spec.Publications), tt.wantLen)
			}
			if tt.want == nil {
				return
			}
			got := spec.Publications[0]
			if got.Name != tt.want.Name {
				t.Errorf("Name = %q, want %q", got.Name, tt.want.Name)
			}
			if got.AllTables != tt.want.AllTables {
				t.Errorf("AllTables = %v, want %v", got.AllTables, tt.want.AllTables)
			}
			if got.PublishInsert != tt.want.PublishInsert || got.PublishUpdate != tt.want.PublishUpdate ||
				got.PublishDelete != tt.want.PublishDelete || got.PublishTruncate != tt.want.PublishTruncate {
				t.Errorf("publish flags = (%v,%v,%v,%v), want (%v,%v,%v,%v)",
					got.PublishInsert, got.PublishUpdate, got.PublishDelete, got.PublishTruncate,
					tt.want.PublishInsert, tt.want.PublishUpdate, tt.want.PublishDelete, tt.want.PublishTruncate)
			}
			if len(got.Tables) != len(tt.want.Tables) {
				t.Errorf("Tables = %v, want %v", got.Tables, tt.want.Tables)
			} else {
				for i := range got.Tables {
					if !strings.EqualFold(got.Tables[i], tt.want.Tables[i]) {
						t.Errorf("Tables[%d] = %q, want %q", i, got.Tables[i], tt.want.Tables[i])
					}
				}
			}
			if len(got.Schemas) != len(tt.want.Schemas) {
				t.Errorf("Schemas = %v, want %v", got.Schemas, tt.want.Schemas)
			}
		})
	}
}

func TestParseCatalogSQL_EventTriggers(t *testing.T) {
	t.Run("basic", func(t *testing.T) {
		spec := ParseCatalogSQL(`CREATE EVENT TRIGGER audit_ddl ON ddl_command_end EXECUTE FUNCTION log_ddl();`)
		if len(spec.EventTriggers) != 1 {
			t.Fatalf("got %d event triggers, want 1", len(spec.EventTriggers))
		}
		e := spec.EventTriggers[0]
		if e.Name != "audit_ddl" || e.Event != "DDL_COMMAND_END" || e.Function != "log_ddl" {
			t.Errorf("unexpected parse: %+v", e)
		}
		if !e.Enabled {
			t.Error("parsed event trigger should be desired-enabled")
		}
		if len(e.Tags) != 0 {
			t.Errorf("Tags = %v, want empty", e.Tags)
		}
	})

	t.Run("with tag filter", func(t *testing.T) {
		spec := ParseCatalogSQL(`CREATE EVENT TRIGGER block_tables ON ddl_command_start WHEN TAG IN ('CREATE TABLE', 'ALTER TABLE') EXECUTE FUNCTION refuse_ddl();`)
		if len(spec.EventTriggers) != 1 {
			t.Fatalf("got %d event triggers, want 1", len(spec.EventTriggers))
		}
		e := spec.EventTriggers[0]
		if len(e.Tags) != 2 || e.Tags[0] != "CREATE TABLE" || e.Tags[1] != "ALTER TABLE" {
			t.Errorf("Tags = %v, want [CREATE TABLE ALTER TABLE]", e.Tags)
		}
	})

	t.Run("quoted name", func(t *testing.T) {
		spec := ParseCatalogSQL(`CREATE EVENT TRIGGER "trig.name" ON sql_drop EXECUTE FUNCTION on_drop();`)
		if len(spec.EventTriggers) != 1 || spec.EventTriggers[0].Name != "trig.name" {
			t.Errorf("unexpected parse: %+v", spec.EventTriggers)
		}
	})

	t.Run("mixed statements", func(t *testing.T) {
		sql := `
-- comment line
CREATE PUBLICATION pub_a FOR ALL TABLES;
CREATE EVENT TRIGGER trig_a ON ddl_command_end EXECUTE FUNCTION fn_a();
CREATE PUBLICATION pub_b FOR TABLE public.docs;
`
		spec := ParseCatalogSQL(sql)
		if len(spec.Publications) != 2 {
			t.Errorf("got %d publications, want 2", len(spec.Publications))
		}
		if len(spec.EventTriggers) != 1 {
			t.Errorf("got %d event triggers, want 1", len(spec.EventTriggers))
		}
	})
}

func TestValidateCatalogSQL(t *testing.T) {
	valid := []string{
		"",
		"-- empty desired catalog state\n",
		"CREATE PUBLICATION p FOR ALL TABLES;",
		"CREATE EVENT TRIGGER t ON ddl_command_end EXECUTE FUNCTION f();",
		"CREATE PUBLICATION p FOR TABLE public.docs;\nCREATE EVENT TRIGGER t ON sql_drop EXECUTE FUNCTION f();",
	}
	for _, sql := range valid {
		if err := ValidateCatalogSQL(sql); err != nil {
			t.Errorf("ValidateCatalogSQL(%q) = %v, want nil", sql, err)
		}
	}

	invalid := []string{
		"ALTER PUBLICATION p ADD TABLE public.docs;",
		"DROP PUBLICATION p;",
		"DROP EVENT TRIGGER t;",
		"CREATE TABLE docs (id int);",
		"GRANT SELECT ON TABLE docs TO app_read;",
		"CREATE EVENT TRIGGER t ON ddl_command_start WHEN VALUE IN ('x') EXECUTE FUNCTION f();",
		"CREATE PUBLICATION p SET (publish = 'insert');",
	}
	for _, sql := range invalid {
		if err := ValidateCatalogSQL(sql); err == nil {
			t.Errorf("ValidateCatalogSQL(%q) = nil, want error", sql)
		}
	}
}
