package sqlite

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/yourorg/grizzle/internal/dialect"
	"github.com/yourorg/grizzle/internal/plan"
	"github.com/yourorg/grizzle/internal/schema"
	"github.com/yourorg/grizzle/internal/scope"
)

// GenerateSQLiteCreateTable constructs a standard SQLite CREATE TABLE statement.
func GenerateSQLiteCreateTable(tbl *schema.Table) string {
	cols := slices.Collect(maps.Values(tbl.Columns))
	slices.SortFunc(cols, func(a, b *schema.Column) int {
		return cmp.Compare(a.Position, b.Position)
	})

	var lines []string
	isSinglePK := tbl.PrimaryKey != nil && len(tbl.PrimaryKey.Columns) == 1

	for _, c := range cols {
		line := fmt.Sprintf("  %q %s", c.Name, c.DataType)
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

	if tbl.PrimaryKey != nil && len(tbl.PrimaryKey.Columns) > 1 {
		var quotedCols []string
		for _, col := range tbl.PrimaryKey.Columns {
			quotedCols = append(quotedCols, fmt.Sprintf("%q", col))
		}
		lines = append(lines, fmt.Sprintf("  PRIMARY KEY (%s)", strings.Join(quotedCols, ", ")))
	}

	for _, fk := range tbl.ForeignKeys {
		lines = append(lines, "  "+fk.Definition)
	}

	return fmt.Sprintf("CREATE TABLE %q (\n%s\n);", tbl.Name, strings.Join(lines, ",\n"))
}

// GenerateSQLiteRebuildPlan generates the standard 12-step atomic table replacement plan.
func GenerateSQLiteRebuildPlan(liveTable, desiredTable *schema.Table) (string, bool) {
	tempTable := "_grizzle_new_" + desiredTable.Name

	rebuiltTable := *desiredTable
	rebuiltTable.Name = tempTable
	createTempSQL := GenerateSQLiteCreateTable(&rebuiltTable)

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
		copyDataSQL = ""
	}

	dropOldSQL := fmt.Sprintf("DROP TABLE %q;", liveTable.Name)
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

// Diff compares live and desired schemas and produces a sequenced list of SQLite steps.
func Diff(live, desired *schema.Schema, filters scope.Filters) []plan.Step {
	var steps []plan.Step

	// 1. Tables
	for tblName, dTable := range desired.Tables {
		if !scope.IsTableManaged(tblName, filters) {
			continue
		}
		lTable, exists := live.Tables[tblName]
		if !exists {
			steps = append(steps, plan.Step{
				Type:        plan.ChangeCreateTable,
				Table:       tblName,
				SQL:         GenerateSQLiteCreateTable(dTable),
				Destructive: false,
			})
			for _, idx := range dTable.Indexes {
				def := strings.TrimSpace(idx.Definition)
				if !strings.HasSuffix(def, ";") {
					def += ";"
				}
				steps = append(steps, plan.Step{
					Type:        plan.ChangeCreateIndex,
					Table:       tblName,
					SQL:         def,
					Destructive: false,
				})
			}
			continue
		}

		// Existing table: Check for explicit rename mappings or rebuild
		needsRebuild := false
		isDestructive := false
		typeNarrowed := false
		isRenameCandidate := false
		var renameCandidateOldCol string

		var droppedCols []string
		for colName := range lTable.Columns {
			if _, inDesired := dTable.Columns[colName]; !inDesired {
				droppedCols = append(droppedCols, colName)
			}
		}
		var addedCols []string
		for colName := range dTable.Columns {
			if _, inLive := lTable.Columns[colName]; !inLive {
				addedCols = append(addedCols, colName)
			}
		}

		// Check if dropped columns match an explicit rename mapping
		var mappedRenames [][2]string
		for _, lColName := range droppedCols {
			var mappedNew string
			if target, ok := filters.Renames[fmt.Sprintf("%s.%s", tblName, lColName)]; ok {
				mappedNew = target
			} else if target, ok := filters.Renames[lColName]; ok {
				mappedNew = target
			}
			if mappedNew != "" && slices.Contains(addedCols, mappedNew) {
				mappedRenames = append(mappedRenames, [2]string{lColName, mappedNew})
			}
		}

		// If all dropped/added columns are accounted for by explicit renames
		pureRename := len(mappedRenames) > 0 && len(mappedRenames) == len(droppedCols) && len(mappedRenames) == len(addedCols)
		if pureRename && !filters.ExpandContract {
			for _, pair := range mappedRenames {
				steps = append(steps, plan.Step{
					Type:        plan.ChangeRenameColumn,
					Table:       tblName,
					SQL:         fmt.Sprintf("ALTER TABLE %q RENAME COLUMN %q TO %q;", tblName, pair[0], pair[1]),
					Destructive: false,
				})
			}
			for _, idx := range dTable.Indexes {
				def := strings.TrimSpace(idx.Definition)
				if !strings.HasSuffix(def, ";") {
					def += ";"
				}
				steps = append(steps, plan.Step{
					Type:        plan.ChangeCreateIndex,
					Table:       tblName,
					SQL:         def,
					Destructive: false,
				})
			}
			continue
		}

		var stagedExpandCols map[string]bool
		if filters.ExpandContract && len(mappedRenames) > 0 {
			stagedExpandCols = make(map[string]bool)
			mappedOld := make(map[string]bool)
			for _, pair := range mappedRenames {
				mappedOld[pair[0]] = true
				stagedExpandCols[pair[1]] = true
			}
			var newDropped []string
			for _, c := range droppedCols {
				if !mappedOld[c] {
					newDropped = append(newDropped, c)
				}
			}
			droppedCols = newDropped
		}

		if len(droppedCols) > 0 {
			// Check for ambiguous candidates
			for _, dColName := range droppedCols {
				lCol := lTable.Columns[dColName]
				for _, aColName := range addedCols {
					aCol := dTable.Columns[aColName]
					if schema.NormalizeType(lCol.DataType) == schema.NormalizeType(aCol.DataType) {
						isRenameCandidate = true
						renameCandidateOldCol = dColName
						break
					}
				}
				if isRenameCandidate {
					break
				}
			}
			needsRebuild = true
			isDestructive = true
		}

		if !needsRebuild {
			for colName, dCol := range dTable.Columns {
				lCol, inLive := lTable.Columns[colName]
				if inLive {
					if dCol.DataType != lCol.DataType || dCol.IsNullable != lCol.IsNullable || dCol.DefaultValue != lCol.DefaultValue {
						needsRebuild = true
						if schema.IsTypeNarrowing(lCol.DataType, dCol.DataType) {
							typeNarrowed = true
							isDestructive = true
						} else if !dCol.IsNullable && lCol.IsNullable {
							isDestructive = true
						}
						break
					}
				}
			}
		}

		if needsRebuild {
			rebuildSQL, destructive := GenerateSQLiteRebuildPlan(lTable, dTable)
			changeType := plan.ChangeAlterColumn
			if len(droppedCols) > 0 {
				changeType = plan.ChangeDropColumn
			}
			steps = append(steps, plan.Step{
				Type:              changeType,
				Table:             tblName,
				SQL:               rebuildSQL,
				Destructive:       isDestructive || destructive,
				TypeNarrowed:      typeNarrowed,
				IsTableRebuild:    true,
				IsRenameCandidate: isRenameCandidate,
				OldColumn:         renameCandidateOldCol,
			})

			for _, idx := range dTable.Indexes {
				def := strings.TrimSpace(idx.Definition)
				if !strings.HasSuffix(def, ";") {
					def += ";"
				}
				steps = append(steps, plan.Step{
					Type:        plan.ChangeCreateIndex,
					Table:       tblName,
					SQL:         def,
					Destructive: false,
				})
			}
		} else {
			for colName, dCol := range dTable.Columns {
				if _, inLive := lTable.Columns[colName]; !inLive {
					clause := fmt.Sprintf("%q %s", dCol.Name, dCol.DataType)
					isStaged := stagedExpandCols != nil && stagedExpandCols[colName]
					if !dCol.IsNullable && !isStaged {
						clause += " NOT NULL"
					}
					if dCol.DefaultValue != "" {
						clause += " DEFAULT " + dCol.DefaultValue
					}
					steps = append(steps, plan.Step{
						Type:             plan.ChangeAddColumn,
						Table:            tblName,
						SQL:              fmt.Sprintf("ALTER TABLE %q ADD COLUMN %s;", tblName, clause),
						Destructive:      false,
						ColumnNotNull:    !dCol.IsNullable && !isStaged,
						ColumnHasDefault: dCol.DefaultValue != "",
					})
				}
			}

			for idxName, dIdx := range dTable.Indexes {
				lIdx, inLive := lTable.Indexes[idxName]
				if !inLive {
					def := strings.TrimSpace(dIdx.Definition)
					if !strings.HasSuffix(def, ";") {
						def += ";"
					}
					steps = append(steps, plan.Step{
						Type:        plan.ChangeCreateIndex,
						Table:       tblName,
						SQL:         def,
						Destructive: false,
					})
				} else if dIdx.Definition != lIdx.Definition {
					steps = append(steps, plan.Step{
						Type:        plan.ChangeDropIndex,
						Table:       tblName,
						SQL:         fmt.Sprintf("DROP INDEX IF EXISTS %q;", idxName),
						Destructive: true,
					})
					def := strings.TrimSpace(dIdx.Definition)
					if !strings.HasSuffix(def, ";") {
						def += ";"
					}
					steps = append(steps, plan.Step{
						Type:        plan.ChangeCreateIndex,
						Table:       tblName,
						SQL:         def,
						Destructive: false,
					})
				}
			}

			for idxName := range lTable.Indexes {
				if _, inDesired := dTable.Indexes[idxName]; !inDesired {
					steps = append(steps, plan.Step{
						Type:        plan.ChangeDropIndex,
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
		if !scope.IsTableManaged(tblName, filters) {
			continue
		}
		if _, inDesired := desired.Tables[tblName]; !inDesired {
			steps = append(steps, plan.Step{
				Type:        plan.ChangeDropTable,
				Table:       tblName,
				SQL:         fmt.Sprintf("DROP TABLE %q;", tblName),
				Destructive: true,
			})
		}
	}

	return steps
}

// Render implements dialect.Dialect for single steps.
func Render(step plan.Step) ([]dialect.Stmt, error) {
	return []dialect.Stmt{{SQL: step.SQL, NonTx: false}}, nil
}
