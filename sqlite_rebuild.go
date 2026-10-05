package grizzle

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// generateSQLiteCreateTable constructs a standard SQLite CREATE TABLE statement.
func generateSQLiteCreateTable(tbl *TableIR) string {
	cols := slices.Collect(maps.Values(tbl.Columns))
	slices.SortFunc(cols, func(a, b *ColumnIR) int {
		return cmp.Compare(a.Position, b.Position)
	})

	var lines []string
	isSinglePK := tbl.PrimaryKey != nil && len(tbl.PrimaryKey.Columns) == 1

	for _, c := range cols {
		line := fmt.Sprintf("  %q %s", c.Name, c.DataType)
		// Single-column integer PK in SQLite is autoincrement primary key
		if isSinglePK && c.Name == tbl.PrimaryKey.Columns[0] && strings.EqualFold(c.DataType, "INTEGER") {
			line += " PRIMARY KEY AUTOINCREMENT"
		} else {
			if !c.IsNullable {
				line += " NOT NULL"
			}
			if c.DefaultValue != "" {
				line += " DEFAULT " + c.DefaultValue
			}
		}
		lines = append(lines, line)
	}

	// Composite primary key
	if tbl.PrimaryKey != nil && len(tbl.PrimaryKey.Columns) > 1 {
		var quotedCols []string
		for _, col := range tbl.PrimaryKey.Columns {
			quotedCols = append(quotedCols, fmt.Sprintf("%q", col))
		}
		lines = append(lines, fmt.Sprintf("  PRIMARY KEY (%s)", strings.Join(quotedCols, ", ")))
	}

	// Foreign keys
	for _, fk := range tbl.ForeignKeys {
		lines = append(lines, "  "+fk.Definition)
	}

	return fmt.Sprintf("CREATE TABLE %q (\n%s\n);", tbl.Name, strings.Join(lines, ",\n"))
}

// generateSQLiteRebuildPlan generates the standard 12-step atomic table replacement plan.
func generateSQLiteRebuildPlan(liveTable, desiredTable *TableIR) (string, bool) {
	tempTable := "_grizzle_new_" + desiredTable.Name

	// 1. Create temporary new table
	rebuiltTable := *desiredTable
	rebuiltTable.Name = tempTable
	createTempSQL := generateSQLiteCreateTable(&rebuiltTable)

	// 2. Determine common columns to copy
	var commonCols []string
	var droppedCols []string
	for colName := range liveTable.Columns {
		if _, exists := desiredTable.Columns[colName]; exists {
			commonCols = append(commonCols, fmt.Sprintf("%q", colName))
		} else {
			droppedCols = append(droppedCols, colName)
		}
	}
	slices.Sort(commonCols)

	colList := strings.Join(commonCols, ", ")
	copyDataSQL := fmt.Sprintf("INSERT INTO %q (%s) SELECT %s FROM %q;", tempTable, colList, colList, liveTable.Name)
	if len(commonCols) == 0 {
		copyDataSQL = "" // No data to copy
	}

	// 3. Drop old table
	dropOldSQL := fmt.Sprintf("DROP TABLE %q;", liveTable.Name)

	// 4. Rename temp table to final name
	renameSQL := fmt.Sprintf("ALTER TABLE %q RENAME TO %q;", tempTable, liveTable.Name)

	var statements []string
	statements = append(statements, createTempSQL)
	if copyDataSQL != "" {
		statements = append(statements, copyDataSQL)
	}
	statements = append(statements, dropOldSQL, renameSQL)

	isDestructive := len(droppedCols) > 0
	return strings.Join(statements, "\n"), isDestructive
}

// diffSQLiteSchemas compares live and desired schemas and produces a sequenced list of SQLite steps.
func diffSQLiteSchemas(live, desired *SchemaIR) []Step {
	var steps []Step

	// 1. Tables
	for tblName, dTable := range desired.Tables {
		lTable, exists := live.Tables[tblName]
		if !exists {
			// New table
			steps = append(steps, Step{
				Type:        ChangeCreateTable,
				Table:       tblName,
				SQL:         generateSQLiteCreateTable(dTable),
				Destructive: false,
			})
			for _, idx := range dTable.Indexes {
				def := strings.TrimSpace(idx.Definition)
				if !strings.HasSuffix(def, ";") {
					def += ";"
				}
				steps = append(steps, Step{
					Type:        ChangeCreateIndex,
					Table:       tblName,
					SQL:         def,
					Destructive: false,
				})
			}
			continue
		}

		// Existing table: Check if rebuild is necessary
		needsRebuild := false
		isDestructive := false

		// Check dropped columns
		for colName := range lTable.Columns {
			if _, inDesired := dTable.Columns[colName]; !inDesired {
				needsRebuild = true
				isDestructive = true
				break
			}
		}

		// Check column alterations
		if !needsRebuild {
			for colName, dCol := range dTable.Columns {
				lCol, inLive := lTable.Columns[colName]
				if inLive {
					if dCol.DataType != lCol.DataType || dCol.IsNullable != lCol.IsNullable || dCol.DefaultValue != lCol.DefaultValue {
						needsRebuild = true
						if dCol.DataType != lCol.DataType || (!dCol.IsNullable && lCol.IsNullable) {
							isDestructive = true
						}
						break
					}
				}
			}
		}

		if needsRebuild {
			// Execute 12-step table rebuild
			rebuildSQL, destructive := generateSQLiteRebuildPlan(lTable, dTable)
			changeType := ChangeAlterColumn
			if isDestructive || destructive {
				changeType = ChangeDropColumn
			}
			steps = append(steps, Step{
				Type:        changeType,
				Table:       tblName,
				SQL:         rebuildSQL,
				Destructive: isDestructive || destructive,
			})

			// Re-create all desired indexes
			for _, idx := range dTable.Indexes {
				def := strings.TrimSpace(idx.Definition)
				if !strings.HasSuffix(def, ";") {
					def += ";"
				}
				steps = append(steps, Step{
					Type:        ChangeCreateIndex,
					Table:       tblName,
					SQL:         def,
					Destructive: false,
				})
			}
		} else {
			// Check if columns were simply added (no rebuild needed)
			for colName, dCol := range dTable.Columns {
				if _, inLive := lTable.Columns[colName]; !inLive {
					clause := fmt.Sprintf("%q %s", dCol.Name, dCol.DataType)
					if !dCol.IsNullable {
						clause += " NOT NULL"
					}
					if dCol.DefaultValue != "" {
						clause += " DEFAULT " + dCol.DefaultValue
					}
					steps = append(steps, Step{
						Type:        ChangeAddColumn,
						Table:       tblName,
						SQL:         fmt.Sprintf("ALTER TABLE %q ADD COLUMN %s;", tblName, clause),
						Destructive: false,
					})
				}
			}

			// Check indexes
			for idxName, dIdx := range dTable.Indexes {
				lIdx, inLive := lTable.Indexes[idxName]
				if !inLive {
					def := strings.TrimSpace(dIdx.Definition)
					if !strings.HasSuffix(def, ";") {
						def += ";"
					}
					steps = append(steps, Step{
						Type:        ChangeCreateIndex,
						Table:       tblName,
						SQL:         def,
						Destructive: false,
					})
				} else if dIdx.Definition != lIdx.Definition {
					steps = append(steps, Step{
						Type:        ChangeDropIndex,
						Table:       tblName,
						SQL:         fmt.Sprintf("DROP INDEX IF EXISTS %q;", idxName),
						Destructive: true,
					})
					def := strings.TrimSpace(dIdx.Definition)
					if !strings.HasSuffix(def, ";") {
						def += ";"
					}
					steps = append(steps, Step{
						Type:        ChangeCreateIndex,
						Table:       tblName,
						SQL:         def,
						Destructive: false,
					})
				}
			}

			for idxName := range lTable.Indexes {
				if _, inDesired := dTable.Indexes[idxName]; !inDesired {
					steps = append(steps, Step{
						Type:        ChangeDropIndex,
						Table:       tblName,
						SQL:         fmt.Sprintf("DROP INDEX IF EXISTS %q;", idxName),
						Destructive: true,
					})
				}
			}
		}
	}

	// 2. Dropped tables
	for tblName := range live.Tables {
		if _, inDesired := desired.Tables[tblName]; !inDesired {
			steps = append(steps, Step{
				Type:        ChangeDropTable,
				Table:       tblName,
				SQL:         fmt.Sprintf("DROP TABLE %q;", tblName),
				Destructive: true,
			})
		}
	}

	return steps
}
