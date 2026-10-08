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

func TestParseCatalogSQL_PreservesEscapedQuotedIdentifiers(t *testing.T) {
	sql := `CREATE PUBLICATION "pub""name" FOR TABLE "semi;colon", "weird.schema"."table""name";
CREATE EVENT TRIGGER "trig""name" ON ddl_command_end
	WHEN TAG IN ('O''TABLE') EXECUTE FUNCTION "fn""name"();`
	if err := ValidateCatalogSQL(sql); err != nil {
		t.Fatalf("quoted catalog identifiers should validate: %v", err)
	}
	spec := ParseCatalogSQL(sql)
	if len(spec.Publications) != 1 || spec.Publications[0].Name != `pub"name` {
		t.Fatalf("publication identifier was not decoded correctly: %+v", spec.Publications)
	}
	if got := spec.Publications[0].Tables; len(got) != 2 || got[0] != `"semi;colon"` ||
		got[1] != `"weird.schema"."table""name"` {
		t.Fatalf("publication membership was not preserved: %v", got)
	}
	if len(spec.EventTriggers) != 1 {
		t.Fatalf("event trigger was not parsed: %+v", spec.EventTriggers)
	}
	trigger := spec.EventTriggers[0]
	if trigger.Name != `trig"name` || trigger.Function != `fn"name` ||
		len(trigger.Tags) != 1 || trigger.Tags[0] != "O'TABLE" {
		t.Fatalf("event-trigger identifiers/tags were not decoded: %+v", trigger)
	}
}

func TestParseCatalogSQL_AlterAndDropForms(t *testing.T) {
	spec := ParseCatalogSQL(`
		CREATE PUBLICATION docs_pub FOR TABLE docs;
		ALTER PUBLICATION docs_pub ADD TABLE audit;
		ALTER PUBLICATION docs_pub SET (publish = 'insert');
		CREATE EVENT TRIGGER audit ON ddl_command_end EXECUTE FUNCTION log_ddl();
		ALTER EVENT TRIGGER audit DISABLE;
	`)
	if len(spec.Publications) != 1 || len(spec.Publications[0].Tables) != 2 {
		t.Fatalf("publication ALTER statements should update desired IR: %+v", spec.Publications)
	}
	if spec.Publications[0].PublishInsert != true || spec.Publications[0].PublishUpdate ||
		spec.Publications[0].PublishDelete || spec.Publications[0].PublishTruncate {
		t.Fatalf("publication publish options not applied: %+v", spec.Publications[0])
	}
	if len(spec.EventTriggers) != 1 || spec.EventTriggers[0].Enabled {
		t.Fatalf("event-trigger ALTER should update desired enabled state: %+v", spec.EventTriggers)
	}

	dropped := ParseCatalogSQL(`
		CREATE PUBLICATION docs_pub FOR TABLE docs;
		DROP PUBLICATION docs_pub;
		CREATE EVENT TRIGGER audit ON ddl_command_end EXECUTE FUNCTION log_ddl();
		DROP EVENT TRIGGER audit;
	`)
	if len(dropped.Publications) != 0 || len(dropped.EventTriggers) != 0 {
		t.Fatalf("DROP statements should remove declarations from desired IR: %+v", dropped)
	}
}

func TestMergeCatalogSpecs_OverlaysThenAppliesOperations(t *testing.T) {
	schemaSpec := ParseCatalogSQL(`CREATE PUBLICATION docs_pub FOR ALL TABLES;`)
	sideSpec := ParseCatalogSQL(`
		CREATE PUBLICATION docs_pub FOR TABLE docs;
		ALTER PUBLICATION docs_pub ADD TABLE audit;
	`)
	if err := ValidateCatalogSpecMerge(schemaSpec, sideSpec); err != nil {
		t.Fatalf("overlay with side-channel CREATE and ALTER should validate: %v", err)
	}
	merged := MergeCatalogSpecs(schemaSpec, sideSpec)
	if len(merged.Publications) != 1 || merged.Publications[0].AllTables ||
		len(merged.Publications[0].Tables) != 2 {
		t.Fatalf("side-channel CREATE must overlay before its ALTER: %+v", merged.Publications)
	}

	sideAlter := ParseCatalogSQL(`ALTER PUBLICATION docs_pub SET TABLE audit;`)
	if err := ValidateCatalogSpecMerge(schemaSpec, sideAlter); err != nil {
		t.Fatalf("side-channel ALTER should apply to SchemaSQL base: %v", err)
	}
	merged = MergeCatalogSpecs(schemaSpec, sideAlter)
	if len(merged.Publications) != 1 || len(merged.Publications[0].Tables) != 1 ||
		merged.Publications[0].Tables[0] != "audit" {
		t.Fatalf("side-channel ALTER should apply after SchemaSQL base: %+v", merged.Publications)
	}

	schemaAlter := ParseCatalogSQL(`ALTER PUBLICATION docs_pub SET TABLE audit;`)
	sideCreate := ParseCatalogSQL(`CREATE PUBLICATION docs_pub FOR TABLE docs;`)
	if err := ValidateCatalogSpecMerge(schemaAlter, sideCreate); err != nil {
		t.Fatalf("SchemaSQL ALTER should be satisfied by CatalogSQL CREATE: %v", err)
	}
	merged = MergeCatalogSpecs(schemaAlter, sideCreate)
	if len(merged.Publications) != 1 || len(merged.Publications[0].Tables) != 1 ||
		merged.Publications[0].Tables[0] != "audit" {
		t.Fatalf("SchemaSQL ALTER should apply after side-channel CREATE: %+v", merged.Publications)
	}

	schemaAlter = ParseCatalogSQL(`ALTER PUBLICATION docs_pub ADD TABLE schema_audit;`)
	sideCreate = ParseCatalogSQL(`
		CREATE PUBLICATION docs_pub FOR TABLE audit;
		ALTER PUBLICATION docs_pub ADD TABLE side_audit;
	`)
	if err := ValidateCatalogSpecMerge(schemaAlter, sideCreate); err != nil {
		t.Fatalf("cross-file ALTER operations should validate: %v", err)
	}
	merged = MergeCatalogSpecs(schemaAlter, sideCreate)
	if len(merged.Publications) != 1 || len(merged.Publications[0].Tables) != 3 {
		t.Fatalf("both cross-file ALTER operations should apply to the side-channel base: %+v", merged.Publications)
	}

	if err := ValidateCatalogSpecMerge(nil, ParseCatalogSQL(`ALTER PUBLICATION missing ADD TABLE docs;`)); err == nil {
		t.Fatal("ALTER without a declaration must fail validation")
	}
}

func TestMergeCatalogSpecsForTarget_QualifiesPublicationOperations(t *testing.T) {
	schemaSpec := ParseCatalogSQL(`
		CREATE PUBLICATION docs_pub FOR TABLE docs;
	`)
	sideSpec := ParseCatalogSQL(`
		ALTER PUBLICATION docs_pub ADD TABLE audit;
		ALTER PUBLICATION docs_pub DROP TABLE docs;
	`)
	if err := ValidateCatalogSpecMergeForTarget(schemaSpec, sideSpec, "billing"); err != nil {
		t.Fatalf("target-aware catalog overlay should validate: %v", err)
	}
	merged := MergeCatalogSpecsForTarget(schemaSpec, sideSpec, "billing")
	if len(merged.Publications) != 1 || len(merged.Publications[0].Tables) != 1 ||
		merged.Publications[0].Tables[0] != "billing.audit" {
		t.Fatalf("publication operations must resolve against target schema before replay: %+v", merged.Publications)
	}
}

func TestValidateCatalogSpecMerge_RejectsMixedAllTablesState(t *testing.T) {
	spec := &CatalogSpec{
		Publications: []*Publication{{
			Name:      "docs_pub",
			AllTables: true,
			Tables:    []string{"public.docs"},
		}},
	}
	if err := ValidateCatalogSpecMerge(spec, nil); err == nil {
		t.Fatal("FOR ALL TABLES plus explicit membership must be rejected")
	}
}

func TestMergeCatalogSpecs_ExplicitDropDoesNotSweep(t *testing.T) {
	merged := MergeCatalogSpecs(nil, ParseCatalogSQL(`DROP PUBLICATION docs_pub;`))
	if !merged.ExplicitDropsOnly() || !merged.PublicationExplicitlyDropped("docs_pub") {
		t.Fatalf("drop-only catalog input must remain an explicit drop: %+v", merged)
	}
}

func TestMergeCatalogSpecs_OperationOnlyPatchSuppressesImplicitDrops(t *testing.T) {
	schemaSpec := ParseCatalogSQL(`CREATE PUBLICATION docs_pub FOR TABLE docs;`)
	sideSpec := ParseCatalogSQL(`ALTER PUBLICATION docs_pub ADD TABLE audit;`)
	merged := MergeCatalogSpecs(schemaSpec, sideSpec)
	if !merged.SuppressImplicitDrops() {
		t.Fatal("operation-only side-channel patch must not imply unrelated managed drops")
	}
	if len(merged.Publications) != 1 || len(merged.Publications[0].Tables) != 2 {
		t.Fatalf("operation-only patch should retain and alter its base declaration: %+v", merged.Publications)
	}
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
		"CREATE TABLE docs (id int);",
		"GRANT SELECT ON TABLE docs TO app_read;",
		"CREATE EVENT TRIGGER t ON ddl_command_start WHEN VALUE IN ('x') EXECUTE FUNCTION f();",
		"CREATE PUBLICATION p SET (publish = 'insert');",
		"CREATE PUBLICATION p FOR VIEW docs;",
		"CREATE PUBLICATION p WITH (publish = 'insert, vacuum);",
		"CREATE PUBLICATION p WITH (publish = 'insert, insert');",
		"CREATE PUBLICATION p FOR TABLE docs,;",
		"CREATE PUBLICATION p FOR TABLE ONLY docs;",
		"CREATE PUBLICATION p FOR TABLE docs.*;",
		"CREATE PUBLICATION p FOR TABLES IN SCHEMA public.docs;",
		`CREATE PUBLICATION "" FOR ALL TABLES;`,
		`CREATE PUBLICATION p FOR TABLE "";`,
		"ALTER PUBLICATION p;",
		"ALTER PUBLICATION p RENAME TO renamed;",
		"ALTER PUBLICATION p ADD TABLE docs,;",
		"ALTER PUBLICATION p ADD TABLE docs -- trailing comment\n;",
		"ALTER PUBLICATION p ADD TABLES IN SCHEMA public,;",
		"ALTER EVENT TRIGGER t ENABLE ALWAYS;",
		"CREATE EVENT TRIGGER t ON ddl_command_end WHEN TAG IN ('x') trailing EXECUTE FUNCTION f();",
		"CREATE EVENT TRIGGER t ON unsupported_event EXECUTE FUNCTION f();",
		"CREATE EVENT TRIGGER t ON ddl_command_end EXECUTE FUNCTION f() trailing;",
		"CREATE PUBLICATION p FOR TABLE docs -- trailing comment\n;",
		"DROP PUBLICATION;",
		"DROP EVENT TRIGGER;",
		`CREATE EVENT TRIGGER "" ON ddl_command_end EXECUTE FUNCTION f();`,
		`CREATE EVENT TRIGGER t ON ddl_command_end EXECUTE FUNCTION ""();`,
	}
	for _, sql := range invalid {
		if err := ValidateCatalogSQL(sql); err == nil {
			t.Errorf("ValidateCatalogSQL(%q) = nil, want error", sql)
		}
	}
}

func TestValidateCatalogSQL_RejectsAllTablesMembershipMutations(t *testing.T) {
	schemaSpec := ParseCatalogSQL(`CREATE PUBLICATION p FOR ALL TABLES;`)
	for _, sql := range []string{
		`ALTER PUBLICATION p ADD TABLE docs;`,
		`ALTER PUBLICATION p DROP TABLE docs;`,
		`ALTER PUBLICATION p ADD TABLES IN SCHEMA analytics;`,
		`ALTER PUBLICATION p DROP TABLES IN SCHEMA analytics;`,
	} {
		t.Run(sql, func(t *testing.T) {
			if err := ValidateCatalogSpecMerge(schemaSpec, ParseCatalogSQL(sql)); err == nil {
				t.Fatalf("membership mutation after FOR ALL TABLES should fail: %q", sql)
			}
		})
	}
}

func TestCatalogIdentifierIdentityKeepsQuotedCase(t *testing.T) {
	spec := ParseCatalogSQL(`
		CREATE PUBLICATION pub;
		CREATE PUBLICATION "Pub";
	`)
	if len(spec.Publications) != 2 {
		t.Fatalf("quoted and unquoted names must remain distinct: %+v", spec.Publications)
	}
	if spec.Publications[0].Name != "pub" || spec.Publications[1].Name != "Pub" {
		t.Fatalf("unexpected publication names: %+v", spec.Publications)
	}
}

func TestValidateCatalogSQL_UnifiedForms(t *testing.T) {
	valid := `
		CREATE PUBLICATION p FOR TABLE docs;
		ALTER PUBLICATION p ADD TABLE public.audit;
		DROP PUBLICATION old_p;
		CREATE EVENT TRIGGER t ON ddl_command_end EXECUTE FUNCTION f();
		ALTER EVENT TRIGGER t DISABLE;
		DROP EVENT TRIGGER old_t;
	`
	if err := ValidateCatalogSQL(valid); err != nil {
		t.Fatalf("unified catalog statements must pass: %v", err)
	}
}

func TestMergeCatalogSpecs_SideChannelWinsDuplicateName(t *testing.T) {
	schemaSpec := ParseCatalogSQL(`CREATE PUBLICATION p FOR ALL TABLES;`)
	sideSpec := ParseCatalogSQL(`CREATE PUBLICATION p FOR TABLE docs;`)
	merged := MergeCatalogSpecs(schemaSpec, sideSpec)
	if len(merged.Publications) != 1 {
		t.Fatalf("unexpected merged catalog spec: %+v", merged)
	}
	if merged.Publications[0].Name != "p" || len(merged.Publications[0].Tables) != 1 || merged.Publications[0].AllTables {
		t.Fatalf("side-channel publication should replace duplicate: %+v", merged.Publications[0])
	}

	merged = MergeCatalogSpecs(schemaSpec, ParseCatalogSQL(`DROP PUBLICATION p;`))
	if len(merged.Publications) != 0 {
		t.Fatalf("side-channel DROP should override duplicate SchemaSQL publication: %+v", merged.Publications)
	}
}

func TestMergeCatalogSpecs_PreservesCreateAlterDropOrder(t *testing.T) {
	spec := ParseCatalogSQL(`
		CREATE PUBLICATION p FOR TABLE docs;
		ALTER PUBLICATION p ADD TABLE audit;
		DROP PUBLICATION p;
	`)
	if err := ValidateCatalogSpecMerge(spec, nil); err != nil {
		t.Fatalf("CREATE/ALTER/DROP sequence should validate: %v", err)
	}
	merged := MergeCatalogSpecs(spec, nil)
	if len(merged.Publications) != 0 || !merged.PublicationExplicitlyDropped("p") {
		t.Fatalf("DROP must remain after ALTER and remove the declaration: %+v", merged)
	}
}

func TestMergeCatalogSpecs_SideDropCannotBeResurrected(t *testing.T) {
	schemaSpec := ParseCatalogSQL(`CREATE PUBLICATION p FOR TABLE docs;`)
	sideSpec := ParseCatalogSQL(`DROP PUBLICATION p;`)
	if err := ValidateCatalogSpecMerge(schemaSpec, sideSpec); err != nil {
		t.Fatalf("side-channel DROP over a SchemaSQL CREATE should validate: %v", err)
	}
	merged := MergeCatalogSpecs(schemaSpec, sideSpec)
	if len(merged.Publications) != 0 || !merged.PublicationExplicitlyDropped("p") {
		t.Fatalf("side-channel DROP must be authoritative: %+v", merged)
	}
}

func TestMergeCatalogSpecs_PreservesSideCreateDropCreateOrder(t *testing.T) {
	sideSpec := ParseCatalogSQL(`
		DROP PUBLICATION p;
		CREATE PUBLICATION p FOR TABLE recreated;
	`)
	if err := ValidateCatalogSpecMerge(nil, sideSpec); err != nil {
		t.Fatalf("DROP followed by CREATE should validate: %v", err)
	}
	merged := MergeCatalogSpecs(nil, sideSpec)
	if len(merged.Publications) != 1 || len(merged.Publications[0].Tables) != 1 ||
		merged.Publications[0].Tables[0] != "recreated" {
		t.Fatalf("side-channel operation order must be preserved: %+v", merged.Publications)
	}
}

func TestValidateCatalogSQL_RejectsOverlengthIdentifiers(t *testing.T) {
	longName := strings.Repeat("p", 64)
	if err := ValidateCatalogSQL(`CREATE PUBLICATION "` + longName + `";`); err == nil {
		t.Fatal("publication identifiers over PostgreSQL's 63-byte limit must be rejected")
	}
}

func TestParseCatalogSQL_QualifiedEventFunctionIdentity(t *testing.T) {
	spec := ParseCatalogSQL(`CREATE EVENT TRIGGER t ON ddl_command_end EXECUTE FUNCTION "audit.schema"."Fn"();`)
	if len(spec.EventTriggers) != 1 {
		t.Fatalf("expected one event trigger: %+v", spec.EventTriggers)
	}
	if got, want := spec.EventTriggers[0].Function, `"audit.schema"."Fn"`; got != want {
		t.Fatalf("event function identity = %q, want %q", got, want)
	}
}
