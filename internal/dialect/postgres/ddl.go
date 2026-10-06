package postgres

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/muandane/grizzle/internal/dialect"
	"github.com/muandane/grizzle/internal/diff"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
)

// IsSerialColumn checks whether a column is an implicit serial sequence column.
func IsSerialColumn(tableName string, col *schema.Column) (string, bool) {
	if strings.HasPrefix(col.DefaultValue, "nextval(") &&
		(strings.Contains(col.DefaultValue, tableName+"_"+col.Name+"_seq") ||
			strings.Contains(col.DefaultValue, col.Name+"_seq")) {
		switch col.DataType {
		case "bigint":
			return "bigserial", true
		case "integer":
			return "serial", true
		case "smallint":
			return "smallserial", true
		}
	}
	return "", false
}

// GenerateCreateEnumSQL constructs a CREATE TYPE ... AS ENUM statement.
func GenerateCreateEnumSQL(targetSchema string, e *schema.Enum) string {
	var quotedVals []string
	for _, v := range e.Values {
		quotedVals = append(quotedVals, fmt.Sprintf("'%s'", strings.ReplaceAll(v, "'", "''")))
	}
	return fmt.Sprintf("CREATE TYPE %q.%q AS ENUM (%s);", targetSchema, e.Name, strings.Join(quotedVals, ", "))
}

// GenerateAddEnumValueSQL constructs an ALTER TYPE ... ADD VALUE statement.
func GenerateAddEnumValueSQL(targetSchema, enumName, val string) string {
	return fmt.Sprintf("ALTER TYPE %q.%q ADD VALUE '%s';", targetSchema, enumName, strings.ReplaceAll(val, "'", "''"))
}

// GenerateCreateTableSQL constructs a CREATE TABLE statement including columns and primary key.
func GenerateCreateTableSQL(targetSchema string, tbl *schema.Table) string {
	if tbl.IsPartition() {
		bounds := strings.TrimSpace(tbl.PartitionOf.Bounds)
		if !strings.HasPrefix(strings.ToUpper(bounds), "FOR VALUES") && strings.ToUpper(bounds) != "DEFAULT" {
			bounds = "FOR VALUES " + bounds
		}
		return fmt.Sprintf("CREATE TABLE %q.%q PARTITION OF %q.%q %s;", targetSchema, tbl.Name, targetSchema, tbl.PartitionOf.Parent, bounds)
	}

	cols := slices.Collect(maps.Values(tbl.Columns))
	slices.SortFunc(cols, func(a, b *schema.Column) int {
		return cmp.Compare(a.Position, b.Position)
	})

	var lines []string
	for _, c := range cols {
		var line string
		if serialType, isSerial := IsSerialColumn(tbl.Name, c); isSerial {
			line = fmt.Sprintf("  %q %s", c.Name, serialType)
		} else if c.IsIdentity {
			line = fmt.Sprintf("  %q %s GENERATED %s AS IDENTITY", c.Name, c.DataType, c.IdentityType)
		} else if c.Generated != nil {
			stored := "STORED"
			if !c.Generated.Stored {
				stored = "VIRTUAL"
			}
			line = fmt.Sprintf("  %q %s GENERATED ALWAYS AS (%s) %s", c.Name, c.DataType, c.Generated.Expr, stored)
		} else {
			line = fmt.Sprintf("  %q %s", c.Name, c.DataType)
			if !c.IsNullable {
				line += " NOT NULL"
			}
			if c.DefaultValue != "" {
				line += " DEFAULT " + c.DefaultValue
			}
		}
		lines = append(lines, line)
	}

	if tbl.PrimaryKey != nil && len(tbl.PrimaryKey.Columns) > 0 {
		var quotedCols []string
		for _, col := range tbl.PrimaryKey.Columns {
			quotedCols = append(quotedCols, fmt.Sprintf("%q", col))
		}
		pkLine := fmt.Sprintf("  CONSTRAINT %q PRIMARY KEY (%s)", tbl.PrimaryKey.Name, strings.Join(quotedCols, ", "))
		lines = append(lines, pkLine)
	}

	partitionClause := ""
	if tbl.IsPartitioned() {
		partDef := strings.TrimSpace(tbl.PartitionKey.Def)
		strat := string(tbl.PartitionKey.Strategy)
		if !strings.HasPrefix(strings.ToUpper(partDef), strat) {
			partDef = strat + " " + partDef
		}
		partitionClause = " PARTITION BY " + partDef
	}

	return fmt.Sprintf("CREATE TABLE %q.%q (\n%s\n)%s;", targetSchema, tbl.Name, strings.Join(lines, ",\n"), partitionClause)
}

// GenerateAddColumnSQL constructs an ALTER TABLE ... ADD COLUMN statement.
func GenerateAddColumnSQL(targetSchema, tableName string, col *schema.Column) string {
	var clause string
	if serialType, isSerial := IsSerialColumn(tableName, col); isSerial {
		clause = fmt.Sprintf("%q %s", col.Name, serialType)
	} else if col.IsIdentity {
		clause = fmt.Sprintf("%q %s GENERATED %s AS IDENTITY", col.Name, col.DataType, col.IdentityType)
	} else if col.Generated != nil {
		stored := "STORED"
		if !col.Generated.Stored {
			stored = "VIRTUAL"
		}
		clause = fmt.Sprintf("%q %s GENERATED ALWAYS AS (%s) %s", col.Name, col.DataType, col.Generated.Expr, stored)
	} else {
		clause = fmt.Sprintf("%q %s", col.Name, col.DataType)
		if !col.IsNullable {
			clause += " NOT NULL"
		}
		if col.DefaultValue != "" {
			clause += " DEFAULT " + col.DefaultValue
		}
	}
	return fmt.Sprintf("ALTER TABLE %q.%q ADD COLUMN %s;", targetSchema, tableName, clause)
}

// GenerateAlterColumnSQL constructs an ALTER TABLE ... ALTER COLUMN statement for modified columns.
func GenerateAlterColumnSQL(targetSchema, tableName string, live, desired *schema.Column) string {
	if desired.Generated != nil || live.Generated != nil {
		stored := "STORED"
		expr := ""
		if desired.Generated != nil {
			expr = desired.Generated.Expr
			if !desired.Generated.Stored {
				stored = "VIRTUAL"
			}
		}
		if expr != "" {
			return fmt.Sprintf("ALTER TABLE %q.%q DROP COLUMN %q; ALTER TABLE %q.%q ADD COLUMN %q %s GENERATED ALWAYS AS (%s) %s;",
				targetSchema, tableName, desired.Name,
				targetSchema, tableName, desired.Name, desired.DataType, expr, stored)
		}
		clause := fmt.Sprintf("%q %s", desired.Name, desired.DataType)
		if !desired.IsNullable {
			clause += " NOT NULL"
		}
		if desired.DefaultValue != "" {
			clause += " DEFAULT " + desired.DefaultValue
		}
		return fmt.Sprintf("ALTER TABLE %q.%q DROP COLUMN %q; ALTER TABLE %q.%q ADD COLUMN %s;",
			targetSchema, tableName, desired.Name,
			targetSchema, tableName, clause)
	}

	var actions []string

	if desired.DataType != live.DataType {
		actions = append(actions, fmt.Sprintf("ALTER COLUMN %q TYPE %s", desired.Name, desired.DataType))
	}

	if desired.IsNullable != live.IsNullable {
		if desired.IsNullable {
			actions = append(actions, fmt.Sprintf("ALTER COLUMN %q DROP NOT NULL", desired.Name))
		} else {
			actions = append(actions, fmt.Sprintf("ALTER COLUMN %q SET NOT NULL", desired.Name))
		}
	}

	if desired.DefaultValue != live.DefaultValue {
		if desired.DefaultValue == "" {
			actions = append(actions, fmt.Sprintf("ALTER COLUMN %q DROP DEFAULT", desired.Name))
		} else {
			actions = append(actions, fmt.Sprintf("ALTER COLUMN %q SET DEFAULT %s", desired.Name, desired.DefaultValue))
		}
	}

	return fmt.Sprintf("ALTER TABLE %q.%q %s;", targetSchema, tableName, strings.Join(actions, ", "))
}

// GenerateCreateIndexSQL constructs a CREATE [UNIQUE] INDEX statement with terminating semicolon.
func GenerateCreateIndexSQL(normalizedIndexDef string, concurrently bool) string {
	def := strings.TrimSpace(normalizedIndexDef)
	if !strings.HasSuffix(def, ";") {
		def += ";"
	}
	if !concurrently {
		return def
	}

	upper := strings.ToUpper(def)
	if strings.Contains(upper, " INDEX CONCURRENTLY ") {
		return def
	}
	if strings.HasPrefix(upper, "CREATE UNIQUE INDEX ") {
		return "CREATE UNIQUE INDEX CONCURRENTLY " + def[len("CREATE UNIQUE INDEX "):]
	}
	if strings.HasPrefix(upper, "CREATE INDEX ") {
		return "CREATE INDEX CONCURRENTLY " + def[len("CREATE INDEX "):]
	}
	return def
}

// GenerateDropIndexSQL constructs a DROP INDEX statement with optional CONCURRENTLY.
func GenerateDropIndexSQL(targetSchema, indexName string, concurrently bool) string {
	if concurrently {
		return fmt.Sprintf("DROP INDEX CONCURRENTLY IF EXISTS %q.%q;", targetSchema, indexName)
	}
	return fmt.Sprintf("DROP INDEX IF EXISTS %q.%q;", targetSchema, indexName)
}

// GenerateAddFKSQL constructs an ALTER TABLE ... ADD CONSTRAINT statement with NOT VALID for safe zero-lock addition.
func GenerateAddFKSQL(targetSchema, tableName, fkName, normalizedFKDef string) string {
	return fmt.Sprintf("ALTER TABLE %q.%q ADD CONSTRAINT %q %s NOT VALID;", targetSchema, tableName, fkName, normalizedFKDef)
}

// GenerateValidateFKSQL constructs an ALTER TABLE ... VALIDATE CONSTRAINT statement.
func GenerateValidateFKSQL(targetSchema, tableName, fkName string) string {
	return fmt.Sprintf("ALTER TABLE %q.%q VALIDATE CONSTRAINT %q;", targetSchema, tableName, fkName)
}

// GenerateDropFKSQL constructs an ALTER TABLE ... DROP CONSTRAINT statement.
func GenerateDropFKSQL(targetSchema, tableName, fkName string) string {
	return fmt.Sprintf("ALTER TABLE %q.%q DROP CONSTRAINT IF EXISTS %q;", targetSchema, tableName, fkName)
}

// GenerateAttachPartitionSQL constructs an ALTER TABLE ... ATTACH PARTITION statement.
func GenerateAttachPartitionSQL(targetSchema, parentTable, childTable, bounds string) string {
	b := strings.TrimSpace(bounds)
	if !strings.HasPrefix(strings.ToUpper(b), "FOR VALUES") && strings.ToUpper(b) != "DEFAULT" {
		b = "FOR VALUES " + b
	}
	return fmt.Sprintf("ALTER TABLE %q.%q ATTACH PARTITION %q.%q %s;", targetSchema, parentTable, targetSchema, childTable, b)
}

// GenerateDetachPartitionSQL constructs an ALTER TABLE ... DETACH PARTITION statement.
func GenerateDetachPartitionSQL(targetSchema, parentTable, childTable string) string {
	return fmt.Sprintf("ALTER TABLE %q.%q DETACH PARTITION %q.%q;", targetSchema, parentTable, targetSchema, childTable)
}

func foreignKeyRefTable(fk *schema.ForeignKey, defaultSchema string) string {
	if fk == nil {
		return ""
	}
	if fk.RefTable != "" {
		if fk.RefSchema != "" {
			return fk.RefSchema + "." + fk.RefTable
		}
		if defaultSchema != "" {
			return defaultSchema + "." + fk.RefTable
		}
		return fk.RefTable
	}
	upper := strings.ToUpper(fk.Definition)
	idx := strings.Index(upper, "REFERENCES ")
	if idx != -1 {
		rest := strings.TrimSpace(fk.Definition[idx+len("REFERENCES "):])
		parenIdx := strings.Index(rest, "(")
		spaceIdx := strings.Index(rest, " ")
		end := len(rest)
		if parenIdx != -1 && (spaceIdx == -1 || parenIdx < spaceIdx) {
			end = parenIdx
		} else if spaceIdx != -1 {
			end = spaceIdx
		}
		ref := strings.Trim(strings.TrimSpace(rest[:end]), `"'`)
		if !strings.Contains(ref, ".") && defaultSchema != "" {
			ref = defaultSchema + "." + ref
		}
		return ref
	}
	return ""
}

// RenderChange converts a pure diff.Change into an executable plan.Step with PostgreSQL DDL.
func RenderChange(targetSchema string, c diff.Change, nonConcurrent ...bool) plan.Step {
	isNonConcurrent := len(nonConcurrent) > 0 && nonConcurrent[0]
	effectiveSchema := cmp.Or(c.Schema, targetSchema)

	step := plan.Step{
		Type:              c.Type,
		Table:             c.Table,
		Schema:            c.Schema,
		Destructive:        c.Destructive,
		ColumnNotNull:      c.ColumnNotNull,
		ColumnHasDefault:   c.ColumnHasDefault,
		TypeNarrowed:       c.TypeNarrowed,
		IsRenameCandidate:  c.IsRenameCandidate,
		IsGeneratedRewrite: c.GeneratedChanged,
		UnmanagedDeps:      c.UnmanagedDeps,
	}
	if c.Column != nil {
		step.Column = c.Column.Name
	}
	if c.OldColumn != nil {
		step.OldColumn = c.OldColumn.Name
	}

	switch c.Type {
	case plan.ChangeCreateEnum:
		step.SQL = GenerateCreateEnumSQL(effectiveSchema, c.Enum)
	case plan.ChangeAlterEnum:
		step.SQL = GenerateAddEnumValueSQL(effectiveSchema, c.Table, c.EnumValue)
	case plan.ChangeCreateTable:
		step.SQL = GenerateCreateTableSQL(effectiveSchema, c.TableData)
		if c.TableData != nil {
			if c.TableData.IsPartition() {
				step.ParentTable = c.TableData.PartitionOf.Parent
				step.PartitionBounds = c.TableData.PartitionOf.Bounds
			}
			for _, fk := range c.TableData.ForeignKeys {
				if ref := foreignKeyRefTable(fk, effectiveSchema); ref != "" {
					step.DependsOn = append(step.DependsOn, ref)
				}
			}
			if len(step.DependsOn) > 0 {
				slices.Sort(step.DependsOn)
				step.DependsOn = slices.Compact(step.DependsOn)
			}
		}
	case plan.ChangeAttachPartition:
		step.SQL = GenerateAttachPartitionSQL(effectiveSchema, c.ParentTable, c.Table, c.PartitionBounds)
		step.ParentTable = c.ParentTable
		step.PartitionBounds = c.PartitionBounds
	case plan.ChangeDetachPartition:
		step.SQL = GenerateDetachPartitionSQL(effectiveSchema, c.ParentTable, c.Table)
		step.ParentTable = c.ParentTable
	case plan.ChangeAddColumn:
		step.SQL = GenerateAddColumnSQL(effectiveSchema, c.Table, c.Column)
	case plan.ChangeAlterColumn:
		step.SQL = GenerateAlterColumnSQL(effectiveSchema, c.Table, c.OldColumn, c.Column)
	case plan.ChangeRenameColumn:
		step.SQL = fmt.Sprintf("ALTER TABLE %q.%q RENAME COLUMN %q TO %q;", effectiveSchema, c.Table, c.OldColumn.Name, c.Column.Name)
	case plan.ChangeDropColumn:
		step.SQL = fmt.Sprintf("ALTER TABLE %q.%q DROP COLUMN %q CASCADE;", effectiveSchema, c.Table, c.Column.Name)
	case plan.ChangeCreateIndex:
		step.SQL = GenerateCreateIndexSQL(c.Index.Definition, !isNonConcurrent)
		step.NonTx = !isNonConcurrent
	case plan.ChangeDropIndex:
		step.SQL = GenerateDropIndexSQL(effectiveSchema, c.Index.Name, !isNonConcurrent)
		step.NonTx = !isNonConcurrent
	case plan.ChangeAddFK:
		step.SQL = GenerateAddFKSQL(effectiveSchema, c.Table, c.ForeignKey.Name, c.ForeignKey.Definition)
		step.RefTable = foreignKeyRefTable(c.ForeignKey, effectiveSchema)
	case plan.ChangeValidateConstraint:
		step.SQL = GenerateValidateFKSQL(effectiveSchema, c.Table, c.ForeignKey.Name)
		step.RefTable = foreignKeyRefTable(c.ForeignKey, effectiveSchema)
	case plan.ChangeDropFK:
		step.SQL = GenerateDropFKSQL(effectiveSchema, c.Table, c.ForeignKey.Name)
		step.RefTable = foreignKeyRefTable(c.ForeignKey, effectiveSchema)
	case plan.ChangeDropTable:
		step.SQL = fmt.Sprintf("DROP TABLE %q.%q CASCADE;", effectiveSchema, c.Table)
		if c.TableData != nil {
			for _, fk := range c.TableData.ForeignKeys {
				if ref := foreignKeyRefTable(fk, effectiveSchema); ref != "" {
					step.DependsOn = append(step.DependsOn, ref)
				}
			}
			if len(step.DependsOn) > 0 {
				slices.Sort(step.DependsOn)
				step.DependsOn = slices.Compact(step.DependsOn)
			}
		}
	}

	return step
}

// RenderChanges converts a list of pure changes into sequenced, topologically ordered plan steps.
func RenderChanges(targetSchema string, changes []diff.Change, nonConcurrent ...bool) []plan.Step {
	isNonConcurrent := len(nonConcurrent) > 0 && nonConcurrent[0]
	var steps []plan.Step

	for _, c := range changes {
		step := RenderChange(targetSchema, c, isNonConcurrent)
		steps = append(steps, step)

		if c.Type == plan.ChangeAddFK {
			effectiveSchema := cmp.Or(c.Schema, targetSchema)
			validateStep := plan.Step{
				Type:     plan.ChangeValidateConstraint,
				Schema:   c.Schema,
				Table:    c.Table,
				SQL:      GenerateValidateFKSQL(effectiveSchema, c.Table, c.ForeignKey.Name),
				RefTable: step.RefTable,
			}
			steps = append(steps, validateStep)
		}
	}

	plan.SortSteps(steps)
	return steps
}

// Render implements dialect.Dialect for single steps.
func Render(step plan.Step) ([]dialect.Stmt, error) {
	return []dialect.Stmt{{SQL: step.SQL, NonTx: step.NonTx}}, nil
}
