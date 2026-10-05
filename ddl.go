package grizzle

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// isSerialColumn checks whether a column is an implicit serial sequence column.
func isSerialColumn(tableName string, col *ColumnIR) (string, bool) {
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

// generateCreateTableSQL constructs a CREATE TABLE statement including columns and primary key.
func generateCreateTableSQL(targetSchema string, tbl *TableIR) string {
	// Collect and sort columns by original position using modern slices and maps packages
	cols := slices.Collect(maps.Values(tbl.Columns))
	slices.SortFunc(cols, func(a, b *ColumnIR) int {
		return cmp.Compare(a.Position, b.Position)
	})

	var lines []string
	for _, c := range cols {
		var line string
		if serialType, isSerial := isSerialColumn(tbl.Name, c); isSerial {
			line = fmt.Sprintf("  %q %s", c.Name, serialType)
		} else if c.IsIdentity {
			line = fmt.Sprintf("  %q %s GENERATED %s AS IDENTITY", c.Name, c.DataType, c.IdentityType)
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

	// Add primary key if present
	if tbl.PrimaryKey != nil && len(tbl.PrimaryKey.Columns) > 0 {
		var quotedCols []string
		for _, col := range tbl.PrimaryKey.Columns {
			quotedCols = append(quotedCols, fmt.Sprintf("%q", col))
		}
		pkLine := fmt.Sprintf("  CONSTRAINT %q PRIMARY KEY (%s)", tbl.PrimaryKey.Name, strings.Join(quotedCols, ", "))
		lines = append(lines, pkLine)
	}

	return fmt.Sprintf("CREATE TABLE %q.%q (\n%s\n);", targetSchema, tbl.Name, strings.Join(lines, ",\n"))
}

// generateAddColumnSQL constructs an ALTER TABLE ... ADD COLUMN statement.
func generateAddColumnSQL(targetSchema, tableName string, col *ColumnIR) string {
	var clause string
	if serialType, isSerial := isSerialColumn(tableName, col); isSerial {
		clause = fmt.Sprintf("%q %s", col.Name, serialType)
	} else if col.IsIdentity {
		clause = fmt.Sprintf("%q %s GENERATED %s AS IDENTITY", col.Name, col.DataType, col.IdentityType)
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

// generateAlterColumnSQL constructs an ALTER TABLE ... ALTER COLUMN statement for modified columns.
func generateAlterColumnSQL(targetSchema, tableName string, live, desired *ColumnIR) string {
	var actions []string

	// Type modification
	if desired.DataType != live.DataType {
		actions = append(actions, fmt.Sprintf("ALTER COLUMN %q TYPE %s", desired.Name, desired.DataType))
	}

	// Nullability modification
	if desired.IsNullable != live.IsNullable {
		if desired.IsNullable {
			actions = append(actions, fmt.Sprintf("ALTER COLUMN %q DROP NOT NULL", desired.Name))
		} else {
			actions = append(actions, fmt.Sprintf("ALTER COLUMN %q SET NOT NULL", desired.Name))
		}
	}

	// Default value modification
	if desired.DefaultValue != live.DefaultValue {
		if desired.DefaultValue == "" {
			actions = append(actions, fmt.Sprintf("ALTER COLUMN %q DROP DEFAULT", desired.Name))
		} else {
			actions = append(actions, fmt.Sprintf("ALTER COLUMN %q SET DEFAULT %s", desired.Name, desired.DefaultValue))
		}
	}

	return fmt.Sprintf("ALTER TABLE %q.%q %s;", targetSchema, tableName, strings.Join(actions, ", "))
}

// sortSteps applies topological ordering so dependent DDL operations execute in the correct sequence.
func sortSteps(steps []Step) {
	priority := map[ChangeType]int{
		ChangeCreateTable: 10,
		ChangeAddColumn:   20,
		ChangeAlterColumn: 30,
		ChangeDropColumn:  40,
		ChangeDropTable:   50,
	}

	slices.SortStableFunc(steps, func(a, b Step) int {
		return cmp.Compare(priority[a.Type], priority[b.Type])
	})
}
