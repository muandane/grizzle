package sqlite

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/muandane/grizzle/internal/dialect"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
	"github.com/muandane/grizzle/internal/scope"
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
			if c.Autoincrement {
				line += " PRIMARY KEY AUTOINCREMENT"
			} else {
				line += " PRIMARY KEY"
			}
		} else if c.Generated != nil {
			stored := "STORED"
			if !c.Generated.Stored {
				stored = "VIRTUAL"
			}
			line += fmt.Sprintf(" GENERATED ALWAYS AS (%s) %s", c.Generated.Expr, stored)
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

	// CHECK constraints (named and inline) are emitted as table-level
	// CONSTRAINT lines — semantically identical to inline forms in SQLite and
	// symmetric with what the inspector parses back out of the stored DDL.
	checkNames := slices.Collect(maps.Keys(tbl.Checks))
	slices.Sort(checkNames)
	for _, name := range checkNames {
		lines = append(lines, fmt.Sprintf("  CONSTRAINT %q %s", name, tbl.Checks[name].Definition))
	}

	return fmt.Sprintf("CREATE TABLE %q (\n%s\n);", tbl.Name, strings.Join(lines, ",\n"))
}

// GenerateSQLiteRebuildPlan generates the standard 12-step atomic table replacement plan,
// preserving and rebinding referencing views and triggers across table recreation.
func GenerateSQLiteRebuildPlan(liveTable, desiredTable *schema.Table, refViews, refTriggers []*schema.UnmanagedObject) (string, bool) {
	tempTable := "_grizzle_new_" + desiredTable.Name

	rebuiltTable := *desiredTable
	rebuiltTable.Name = tempTable
	createTempSQL := GenerateSQLiteCreateTable(&rebuiltTable)

	var commonCols []string
	var droppedCols []string
	for colName := range liveTable.Columns {
		if dCol, exists := desiredTable.Columns[colName]; exists {
			// Generated columns cannot be inserted into in SQLite
			if dCol.Generated == nil {
				commonCols = append(commonCols, fmt.Sprintf("%q", colName))
			}
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

	// 1. Drop referencing views before dropping old table so SQLite ALTER TABLE ... RENAME does not fail on broken views
	for _, v := range refViews {
		statements = append(statements, fmt.Sprintf("DROP VIEW IF EXISTS %q;", v.Name))
	}

	// 2. Drop old table and rename temp table
	statements = append(statements, dropOldSQL, renameSQL)

	// 3. Rebind referencing views to recreated table
	for _, v := range refViews {
		def := strings.TrimSpace(v.SQL)
		if !strings.HasSuffix(def, ";") {
			def += ";"
		}
		statements = append(statements, def)
	}

	// 4. Rebind triggers to recreated table
	for _, trg := range refTriggers {
		def := strings.TrimSpace(trg.SQL)
		if !strings.HasSuffix(def, ";") {
			def += ";"
		}
		statements = append(statements, def)
	}

	isDestructive := len(droppedCols) > 0
	return strings.Join(statements, "\n"), isDestructive
}

// checksDelta compares live and desired CHECK constraints. It returns the
// names of added constraints (desired but not live) and removed constraints
// (live but not desired). Constraints present on both sides with identical
// definitions are unchanged; differing definitions count as removed+added.
func checksDelta(live, desired map[string]*schema.CheckConstraint) (added, removed []string) {
	for name, dChk := range desired {
		lChk, inLive := live[name]
		if !inLive || lChk.Definition != dChk.Definition {
			added = append(added, name)
		}
	}
	for name, lChk := range live {
		dChk, inDesired := desired[name]
		if !inDesired || lChk.Definition != dChk.Definition {
			removed = append(removed, name)
		}
	}
	slices.Sort(added)
	slices.Sort(removed)
	return added, removed
}

// Diff compares live and desired schemas and produces a sequenced list of SQLite steps.
func Diff(live, desired *schema.Schema, filters scope.Filters) []plan.Step {
	var steps []plan.Step

	// 1. Tables
	tableNames := slices.Collect(maps.Keys(desired.Tables))
	slices.Sort(tableNames)
	for _, tblName := range tableNames {
		dTable := desired.Tables[tblName]
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

		// CHECK constraint drift forces a rebuild: SQLite has no ALTER for
		// CHECK (no NOT VALID/VALIDATE path). Removals and narrowing of
		// validation are destructive and gated like Postgres DROP_CHECK.
		checksAdded, checksRemoved := checksDelta(lTable.Checks, dTable.Checks)
		checksChanged := len(checksAdded) > 0 || len(checksRemoved) > 0
		if checksChanged && !needsRebuild {
			needsRebuild = true
			if len(checksRemoved) > 0 {
				isDestructive = true
			}
		}

		isGeneratedRewrite := false
		if !needsRebuild {
			for colName, dCol := range dTable.Columns {
				lCol, inLive := lTable.Columns[colName]
				if inLive {
					genChanged := false
					if (lCol.Generated == nil) != (dCol.Generated == nil) {
						genChanged = true
					} else if lCol.Generated != nil && dCol.Generated != nil {
						if schema.NormalizeGeneratedExpr(lCol.Generated.Expr) != schema.NormalizeGeneratedExpr(dCol.Generated.Expr) ||
							lCol.Generated.Stored != dCol.Generated.Stored {
							genChanged = true
						}
					}
					if dCol.DataType != lCol.DataType || dCol.IsNullable != lCol.IsNullable || dCol.DefaultValue != lCol.DefaultValue || genChanged {
						needsRebuild = true
						if genChanged {
							isGeneratedRewrite = true
						}
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
			var refViews []*schema.UnmanagedObject
			var refTriggers []*schema.UnmanagedObject
			if live.Unmanaged != nil {
				for _, obj := range live.Unmanaged {
					if obj.Kind == schema.UnmanagedTrigger && obj.Table == tblName {
						refTriggers = append(refTriggers, obj)
					} else if obj.Kind == schema.UnmanagedView {
						for _, ref := range obj.DependsOn {
							if ref.Table == tblName {
								refViews = append(refViews, obj)
								break
							}
						}
					}
				}
			}
			slices.SortFunc(refViews, func(a, b *schema.UnmanagedObject) int { return cmp.Compare(a.Name, b.Name) })
			slices.SortFunc(refTriggers, func(a, b *schema.UnmanagedObject) int { return cmp.Compare(a.Name, b.Name) })

			rebuildSQL, destructive := GenerateSQLiteRebuildPlan(lTable, dTable, refViews, refTriggers)
			changeType := plan.ChangeAlterColumn
			if len(droppedCols) > 0 {
				changeType = plan.ChangeDropColumn
			} else if checksChanged && len(checksRemoved) > 0 {
				// CHECK-only drift with removals: gate the rebuild behind the
				// check drop policy so it is rejected by default.
				changeType = plan.ChangeDropCheck
			} else if checksChanged {
				changeType = plan.ChangeAddCheck
			}
			steps = append(steps, plan.Step{
				Type:               changeType,
				Table:              tblName,
				SQL:                rebuildSQL,
				Destructive:        isDestructive || destructive,
				TypeNarrowed:       typeNarrowed,
				IsTableRebuild:     true,
				IsRenameCandidate:  isRenameCandidate,
				IsGeneratedRewrite: isGeneratedRewrite,
				OldColumn:          renameCandidateOldCol,
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
					if dCol.Generated != nil {
						stored := "STORED"
						if !dCol.Generated.Stored {
							stored = "VIRTUAL"
						}
						clause += fmt.Sprintf(" GENERATED ALWAYS AS (%s) %s", dCol.Generated.Expr, stored)
					} else {
						isStaged := stagedExpandCols != nil && stagedExpandCols[colName]
						if !dCol.IsNullable && !isStaged {
							clause += " NOT NULL"
						}
						if dCol.DefaultValue != "" {
							clause += " DEFAULT " + dCol.DefaultValue
						}
					}
					steps = append(steps, plan.Step{
						Type:             plan.ChangeAddColumn,
						Table:            tblName,
						SQL:              fmt.Sprintf("ALTER TABLE %q ADD COLUMN %s;", tblName, clause),
						Destructive:      false,
						ColumnNotNull:    dCol.Generated == nil && !dCol.IsNullable && (stagedExpandCols == nil || !stagedExpandCols[colName]),
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
	liveTableNames := slices.Collect(maps.Keys(live.Tables))
	slices.Sort(liveTableNames)
	for _, tblName := range liveTableNames {
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
