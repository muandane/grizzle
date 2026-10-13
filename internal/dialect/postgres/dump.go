package postgres

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/muandane/grizzle/internal/dialect"
	"github.com/muandane/grizzle/internal/schema"
)

// DumpSchema introspects the PostgreSQL schema and renders canonical DDL SQL.
func DumpSchema(ctx context.Context, dbtx dialect.DBTX, targetSchema string) (string, error) {
	if targetSchema == "" {
		targetSchema = "public"
	}
	s, err := Inspect(ctx, dbtx, targetSchema)
	if err != nil {
		return "", fmt.Errorf("postgres dump: introspect schema %q: %w", targetSchema, err)
	}
	return Dump(s), nil
}

// Dump transforms a PostgreSQL Schema IR into canonical DDL statements.
func Dump(s *schema.Schema) string {
	if s == nil {
		return ""
	}
	targetSchema := cmp.Or(s.Name, "public")

	var sections []string

	// 1. Extensions
	if len(s.Extensions) > 0 {
		exts := slices.Collect(maps.Values(s.Extensions))
		slices.SortFunc(exts, func(a, b *schema.Extension) int {
			return strings.Compare(a.Name, b.Name)
		})
		var extStmts []string
		for _, ext := range exts {
			extStmts = append(extStmts, GenerateCreateExtensionSQL(ext))
		}
		if len(extStmts) > 0 {
			sections = append(sections, strings.Join(extStmts, "\n"))
		}
	}

	// 2. Custom ENUM Types
	if len(s.Enums) > 0 {
		enums := slices.Collect(maps.Values(s.Enums))
		slices.SortFunc(enums, func(a, b *schema.Enum) int {
			return strings.Compare(a.Name, b.Name)
		})
		var enumStmts []string
		for _, e := range enums {
			enumStmts = append(enumStmts, GenerateCreateEnumSQL(targetSchema, e))
		}
		if len(enumStmts) > 0 {
			sections = append(sections, strings.Join(enumStmts, "\n"))
		}
	}

	// 3. Custom Range Types
	if len(s.RangeTypes) > 0 {
		rangeTypes := slices.Collect(maps.Values(s.RangeTypes))
		slices.SortFunc(rangeTypes, func(a, b *schema.RangeType) int {
			return strings.Compare(a.Name, b.Name)
		})
		var rangeStmts []string
		for _, rt := range rangeTypes {
			rangeStmts = append(rangeStmts, GenerateCreateRangeTypeSQL(targetSchema, rt))
		}
		if len(rangeStmts) > 0 {
			sections = append(sections, strings.Join(rangeStmts, "\n"))
		}
	}

	// 4. Custom Composite Types
	if len(s.CompositeTypes) > 0 {
		compTypes := slices.Collect(maps.Values(s.CompositeTypes))
		slices.SortFunc(compTypes, func(a, b *schema.CompositeType) int {
			return strings.Compare(a.Name, b.Name)
		})
		var compStmts []string
		for _, ct := range compTypes {
			compStmts = append(compStmts, GenerateCreateCompositeTypeSQL(targetSchema, ct))
		}
		if len(compStmts) > 0 {
			sections = append(sections, strings.Join(compStmts, "\n"))
		}
	}

	// 5. Domains
	if len(s.Domains) > 0 {
		domains := slices.Collect(maps.Values(s.Domains))
		slices.SortFunc(domains, func(a, b *schema.Domain) int {
			return strings.Compare(a.Name, b.Name)
		})
		var domStmts []string
		for _, d := range domains {
			domStmts = append(domStmts, GenerateCreateDomainSQL(targetSchema, d))
		}
		if len(domStmts) > 0 {
			sections = append(sections, strings.Join(domStmts, "\n"))
		}
	}

	// 6. Sequences
	if len(s.Sequences) > 0 {
		seqs := slices.Collect(maps.Values(s.Sequences))
		slices.SortFunc(seqs, func(a, b *schema.Sequence) int {
			return strings.Compare(a.Name, b.Name)
		})
		var seqStmts []string
		for _, seq := range seqs {
			seqStmts = append(seqStmts, GenerateCreateSequenceSQL(targetSchema, seq))
		}
		if len(seqStmts) > 0 {
			sections = append(sections, strings.Join(seqStmts, "\n"))
		}
	}

	// 7. Routines (Functions / Procedures)
	if len(s.Routines) > 0 {
		routines := slices.Collect(maps.Values(s.Routines))
		slices.SortFunc(routines, func(a, b *schema.Routine) int {
			return strings.Compare(a.Name, b.Name)
		})
		var routineStmts []string
		for _, r := range routines {
			routineStmts = append(routineStmts, GenerateCreateFunctionSQL(r))
		}
		if len(routineStmts) > 0 {
			sections = append(sections, strings.Join(routineStmts, "\n\n"))
		}
	}

	// 8. Tables
	tables := slices.Collect(maps.Values(s.Tables))
	slices.SortFunc(tables, func(a, b *schema.Table) int {
		return strings.Compare(a.Name, b.Name)
	})

	var tableStmts []string
	var fkStmts []string
	var checkStmts []string
	var indexStmts []string
	var rlsStmts []string
	var triggerStmts []string
	var commentStmts []string

	for _, tbl := range tables {
		tableStmts = append(tableStmts, GenerateCreateTableSQL(targetSchema, tbl))

		// CHECK constraints
		if len(tbl.Checks) > 0 {
			checks := slices.Collect(maps.Values(tbl.Checks))
			slices.SortFunc(checks, func(a, b *schema.CheckConstraint) int {
				return strings.Compare(a.Name, b.Name)
			})
			for _, chk := range checks {
				checkDef := chk.Definition
				if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(checkDef)), "CHECK") {
					checkDef = "CHECK (" + checkDef + ")"
				}
				checkStmts = append(checkStmts, fmt.Sprintf("ALTER TABLE %q.%q ADD CONSTRAINT %q %s;", targetSchema, tbl.Name, chk.Name, checkDef))
			}
		}

		// Foreign keys
		if len(tbl.ForeignKeys) > 0 {
			fks := slices.Collect(maps.Values(tbl.ForeignKeys))
			slices.SortFunc(fks, func(a, b *schema.ForeignKey) int {
				return strings.Compare(a.Name, b.Name)
			})
			for _, fk := range fks {
				fkStmts = append(fkStmts, fmt.Sprintf("ALTER TABLE %q.%q ADD CONSTRAINT %q %s;", targetSchema, tbl.Name, fk.Name, fk.Definition))
			}
		}

		// Indexes
		if len(tbl.Indexes) > 0 {
			indexes := slices.Collect(maps.Values(tbl.Indexes))
			slices.SortFunc(indexes, func(a, b *schema.Index) int {
				return strings.Compare(a.Name, b.Name)
			})
			for _, idx := range indexes {
				if tbl.PrimaryKey != nil && idx.Name == tbl.PrimaryKey.Name {
					continue
				}
				if idx.Definition != "" {
					indexStmts = append(indexStmts, GenerateCreateIndexSQL(idx.Definition, false))
				}
			}
		}

		// RLS & Policies
		if tbl.RLSEnabled {
			rlsStmts = append(rlsStmts, fmt.Sprintf("ALTER TABLE %q.%q ENABLE ROW LEVEL SECURITY;", targetSchema, tbl.Name))
		}
		if tbl.RLSForced {
			rlsStmts = append(rlsStmts, fmt.Sprintf("ALTER TABLE %q.%q FORCE ROW LEVEL SECURITY;", targetSchema, tbl.Name))
		}
		if len(tbl.Policies) > 0 {
			policies := slices.Collect(maps.Values(tbl.Policies))
			slices.SortFunc(policies, func(a, b *schema.Policy) int {
				return strings.Compare(a.Name, b.Name)
			})
			for _, pol := range policies {
				rlsStmts = append(rlsStmts, GenerateCreatePolicySQL(targetSchema, tbl.Name, pol))
			}
		}

		// Triggers
		if len(tbl.Triggers) > 0 {
			triggers := slices.Collect(maps.Values(tbl.Triggers))
			slices.SortFunc(triggers, func(a, b *schema.Trigger) int {
				return strings.Compare(a.Name, b.Name)
			})
			for _, trg := range triggers {
				triggerStmts = append(triggerStmts, GenerateCreateTriggerSQL(trg))
			}
		}

		// Comments
		if tbl.Comment != "" {
			commentStmts = append(commentStmts, GenerateCommentSQL(targetSchema, tbl.Name, "", tbl.Comment))
		}
		cols := slices.Collect(maps.Values(tbl.Columns))
		slices.SortFunc(cols, func(a, b *schema.Column) int {
			return cmp.Compare(a.Position, b.Position)
		})
		for _, col := range cols {
			if col.Comment != "" {
				commentStmts = append(commentStmts, GenerateCommentSQL(targetSchema, tbl.Name, col.Name, col.Comment))
			}
		}
	}

	if len(tableStmts) > 0 {
		sections = append(sections, strings.Join(tableStmts, "\n\n"))
	}
	if len(checkStmts) > 0 {
		sections = append(sections, strings.Join(checkStmts, "\n"))
	}
	if len(fkStmts) > 0 {
		sections = append(sections, strings.Join(fkStmts, "\n"))
	}
	if len(indexStmts) > 0 {
		sections = append(sections, strings.Join(indexStmts, "\n"))
	}
	if len(rlsStmts) > 0 {
		sections = append(sections, strings.Join(rlsStmts, "\n"))
	}

	// 9. Views & Materialized Views
	if len(s.Views) > 0 {
		views := slices.Collect(maps.Values(s.Views))
		slices.SortFunc(views, func(a, b *schema.View) int {
			return strings.Compare(a.Name, b.Name)
		})
		var viewStmts []string
		for _, v := range views {
			viewStmts = append(viewStmts, GenerateCreateViewSQL(targetSchema, v, false))
		}
		if len(viewStmts) > 0 {
			sections = append(sections, strings.Join(viewStmts, "\n\n"))
		}
	}

	// 10. Triggers
	if len(triggerStmts) > 0 {
		sections = append(sections, strings.Join(triggerStmts, "\n\n"))
	}

	// 11. Comments
	if len(commentStmts) > 0 {
		sections = append(sections, strings.Join(commentStmts, "\n"))
	}

	return strings.Join(sections, "\n\n") + "\n"
}
