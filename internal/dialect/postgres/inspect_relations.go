package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/muandane/grizzle/internal/dialect"
	"github.com/muandane/grizzle/internal/schema"
)

// polCmdName converts pg_policy.polcmd to its SQL keyword.
func polCmdName(cmd string) string {
	switch cmd {
	case "r":
		return "SELECT"
	case "a":
		return "INSERT"
	case "w":
		return "UPDATE"
	case "d":
		return "DELETE"
	default:
		return "ALL"
	}
}

// inspectTablesAndColumns reads columns and table comments for base tables and partitioned tables.
func inspectTablesAndColumns(ctx context.Context, dbtx dialect.DBTX, schemaName string, s *schema.Schema) error {
	colQuery := `
		SELECT
			c.relname AS table_name,
			a.attname AS column_name,
			format_type(a.atttypid, a.atttypmod) AS formatted_type,
			NOT a.attnotnull AS is_nullable,
			COALESCE(pg_get_expr(d.adbin, d.adrelid), '') AS column_default,
			a.attnum AS ordinal_position,
			a.attidentity AS identity_type,
			a.attgenerated AS generated_type,
			COALESCE(col_description(c.oid, a.attnum), '') AS column_comment
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_attrdef d ON d.adrelid = c.oid AND d.adnum = a.attnum
		WHERE n.nspname = $1
		  AND c.relkind IN ('r', 'p') -- Base tables and partitioned tables
		  AND a.attnum > 0            -- Filter out system columns (tableoid, ctid, etc.)
		  AND NOT a.attisdropped      -- Filter out dropped columns
		ORDER BY c.relname, a.attnum;
	`

	rows, err := dbtx.QueryContext(ctx, colQuery, schemaName)
	if err != nil {
		return fmt.Errorf("inspecting columns in schema %q: %w", schemaName, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			tableName     string
			colName       string
			rawType       string
			isNullable    bool
			rawDefault    string
			position      int
			identityType  string
			generatedType string
			colComment    string
		)

		if err := rows.Scan(&tableName, &colName, &rawType, &isNullable, &rawDefault, &position, &identityType, &generatedType, &colComment); err != nil {
			return fmt.Errorf("scanning column data in schema %q: %w", schemaName, err)
		}

		tbl, exists := s.Tables[tableName]
		if !exists {
			tbl = &schema.Table{
				Schema:      schemaName,
				Name:        tableName,
				Columns:     make(map[string]*schema.Column),
				Indexes:     make(map[string]*schema.Index),
				ForeignKeys: make(map[string]*schema.ForeignKey),
				Checks:      make(map[string]*schema.CheckConstraint),
			}
			s.Tables[tableName] = tbl
		}

		cleanedType := strings.TrimPrefix(rawType, schemaName+".")
		cleanedType = strings.TrimPrefix(cleanedType, `"`+schemaName+`".`)

		col := &schema.Column{
			Name:         colName,
			DataType:     schema.NormalizeType(cleanedType),
			IsNullable:   isNullable,
			DefaultValue: schema.NormalizeDefault(rawDefault),
			Position:     position,
			Comment:      colComment,
		}

		switch strings.TrimSpace(generatedType) {
		case "s":
			col.Generated = &schema.GeneratedColumn{
				Expr:   schema.NormalizeGeneratedExpr(rawDefault),
				Stored: true,
			}
			col.DefaultValue = ""
		case "v":
			col.Generated = &schema.GeneratedColumn{
				Expr:   schema.NormalizeGeneratedExpr(rawDefault),
				Stored: false,
			}
			col.DefaultValue = ""
		}

		switch identityType {
		case "a":
			col.IsIdentity = true
			col.IdentityType = "ALWAYS"
		case "d":
			col.IsIdentity = true
			col.IdentityType = "BY DEFAULT"
		}

		tbl.Columns[colName] = col
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_ = rows.Close()

	// Table comments (obj_description on pg_class).
	tableCommentQuery := `
		SELECT c.relname, COALESCE(obj_description(c.oid, 'pg_class'), '')
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1
		  AND c.relkind IN ('r', 'p')
		ORDER BY c.relname;
	`
	commentRows, err := dbtx.QueryContext(ctx, tableCommentQuery, schemaName)
	if err != nil {
		return fmt.Errorf("inspecting table comments in schema %q: %w", schemaName, err)
	}
	for commentRows.Next() {
		var tblName, comment string
		if err := commentRows.Scan(&tblName, &comment); err != nil {
			_ = commentRows.Close()
			return fmt.Errorf("scanning table comment in schema %q: %w", schemaName, err)
		}
		tbl, exists := s.Tables[tblName]
		if !exists {
			continue
		}
		tbl.Comment = comment
	}
	if err := commentRows.Err(); err != nil {
		return err
	}
	_ = commentRows.Close()

	return nil
}

// inspectPartitions inspects partitioned table definitions and attached partitions.
func inspectPartitions(ctx context.Context, dbtx dialect.DBTX, schemaName string, s *schema.Schema) error {
	partQuery := `
		SELECT
			c.relname AS table_name,
			p.partstrat AS strategy,
			pg_get_partkeydef(c.oid) AS partition_key
		FROM pg_partitioned_table p
		JOIN pg_class c ON c.oid = p.partrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1
		ORDER BY c.relname;
	`
	partRows, err := dbtx.QueryContext(ctx, partQuery, schemaName)
	if err != nil {
		return fmt.Errorf("inspecting partitioned tables in schema %q: %w", schemaName, err)
	}
	defer func() { _ = partRows.Close() }()

	for partRows.Next() {
		var (
			tableName string
			strat     string
			partKey   string
		)
		if err := partRows.Scan(&tableName, &strat, &partKey); err != nil {
			return fmt.Errorf("scanning partitioned table data in schema %q: %w", schemaName, err)
		}
		tbl, exists := s.Tables[tableName]
		if !exists {
			tbl = &schema.Table{
				Schema:      schemaName,
				Name:        tableName,
				Columns:     make(map[string]*schema.Column),
				Indexes:     make(map[string]*schema.Index),
				ForeignKeys: make(map[string]*schema.ForeignKey),
				Checks:      make(map[string]*schema.CheckConstraint),
			}
			s.Tables[tableName] = tbl
		}
		var pStrat schema.PartitionStrategy
		switch strat {
		case "r":
			pStrat = schema.PartitionStrategyRange
		case "l":
			pStrat = schema.PartitionStrategyList
		case "h":
			pStrat = schema.PartitionStrategyHash
		default:
			pStrat = schema.PartitionStrategy(strings.ToUpper(strat))
		}
		tbl.PartitionKey = &schema.PartitionKey{
			Strategy: pStrat,
			Def:      partKey,
		}
	}
	if err := partRows.Err(); err != nil {
		return err
	}
	_ = partRows.Close()

	// Attached Partitions
	var hasDetachPendingCol bool
	_ = dbtx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = 'pg_inherits'::regclass AND attname = 'inhdetachpending' AND NOT attisdropped);").Scan(&hasDetachPendingCol)

	detachPendingExpr := "false"
	if hasDetachPendingCol {
		detachPendingExpr = "COALESCE(i.inhdetachpending, false)"
	}

	inhQuery := fmt.Sprintf(`
		SELECT
			c.relname AS child_table,
			p.relname AS parent_table,
			COALESCE(pg_get_expr(c.relpartbound, c.oid), '') AS partition_bounds,
			%s AS is_detach_pending
		FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid
		JOIN pg_class p ON p.oid = i.inhparent
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1
		  AND c.relkind IN ('r', 'p')
		  AND p.relkind IN ('r', 'p')
		  AND c.relispartition
		ORDER BY c.relname;
	`, detachPendingExpr)
	inhRows, err := dbtx.QueryContext(ctx, inhQuery, schemaName)
	if err != nil {
		return fmt.Errorf("inspecting partition inheritance in schema %q: %w", schemaName, err)
	}
	defer func() { _ = inhRows.Close() }()

	for inhRows.Next() {
		var (
			childTable      string
			parentTable     string
			bounds          string
			isDetachPending bool
		)
		if err := inhRows.Scan(&childTable, &parentTable, &bounds, &isDetachPending); err != nil {
			return fmt.Errorf("scanning partition bounds in schema %q: %w", schemaName, err)
		}
		tbl, exists := s.Tables[childTable]
		if !exists {
			tbl = &schema.Table{
				Schema:      schemaName,
				Name:        childTable,
				Columns:     make(map[string]*schema.Column),
				Indexes:     make(map[string]*schema.Index),
				ForeignKeys: make(map[string]*schema.ForeignKey),
				Checks:      make(map[string]*schema.CheckConstraint),
			}
			s.Tables[childTable] = tbl
		}
		tbl.PartitionOf = &schema.PartitionOf{
			Parent:          parentTable,
			Bounds:          bounds,
			IsDetachPending: isDetachPending,
		}
	}
	if err := inhRows.Err(); err != nil {
		return err
	}
	_ = inhRows.Close()

	return nil
}

// inspectRLS inspects row-level security flags and policies on tables.
func inspectRLS(ctx context.Context, dbtx dialect.DBTX, schemaName string, s *schema.Schema) error {
	rlsQuery := `
		SELECT
			c.relname AS table_name,
			c.relrowsecurity AS rls_enabled,
			c.relforcerowsecurity AS rls_forced
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1
		  AND c.relkind IN ('r', 'p')
		ORDER BY c.relname;
	`
	rlsRows, err := dbtx.QueryContext(ctx, rlsQuery, schemaName)
	if err != nil {
		return fmt.Errorf("inspecting RLS flags in schema %q: %w", schemaName, err)
	}
	defer func() { _ = rlsRows.Close() }()
	for rlsRows.Next() {
		var tblName string
		var rlsEnabled, rlsForced bool
		if err := rlsRows.Scan(&tblName, &rlsEnabled, &rlsForced); err != nil {
			return fmt.Errorf("scanning RLS flags in schema %q: %w", schemaName, err)
		}
		if tbl, exists := s.Tables[tblName]; exists {
			tbl.RLSEnabled = rlsEnabled
			tbl.RLSForced = rlsForced
		}
	}
	if err := rlsRows.Err(); err != nil {
		return err
	}
	_ = rlsRows.Close()

	policyQuery := `
		SELECT
			c.relname AS table_name,
			p.polname AS policy_name,
			p.polcmd AS cmd,
			p.polpermissive AS permissive,
			COALESCE(pg_get_expr(p.polqual, p.polrelid), '') AS using_expr,
			COALESCE(pg_get_expr(p.polwithcheck, p.polrelid), '') AS with_check_expr,
			COALESCE((
				SELECT string_agg(CASE WHEN r = 0 THEN 'public' ELSE COALESCE(a.rolname, r::text) END, ',' ORDER BY r)
				FROM unnest(p.polroles) AS r
				LEFT JOIN pg_authid a ON a.oid = r
			), '') AS roles
		FROM pg_policy p
		JOIN pg_class c ON c.oid = p.polrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1
		ORDER BY c.relname, p.polname;
	`
	policyRows, err := dbtx.QueryContext(ctx, policyQuery, schemaName)
	if err != nil {
		return fmt.Errorf("inspecting policies in schema %q: %w", schemaName, err)
	}
	defer func() { _ = policyRows.Close() }()
	for policyRows.Next() {
		var (
			tblName    string
			policyName string
			cmd        string
			permissive bool
			usingExpr  string
			withCheck  string
			rolesCSV   string
		)
		if err := policyRows.Scan(&tblName, &policyName, &cmd, &permissive, &usingExpr, &withCheck, &rolesCSV); err != nil {
			return fmt.Errorf("scanning policy in schema %q: %w", schemaName, err)
		}
		tbl, exists := s.Tables[tblName]
		if !exists {
			continue
		}
		if tbl.Policies == nil {
			tbl.Policies = make(map[string]*schema.Policy)
		}
		var roles []string
		if rolesCSV != "" {
			for r := range strings.SplitSeq(rolesCSV, ",") {
				if r = strings.TrimSpace(r); r != "" {
					roles = append(roles, r)
				}
			}
		}
		tbl.Policies[policyName] = &schema.Policy{
			Name:       policyName,
			Cmd:        polCmdName(cmd),
			Roles:      roles,
			Using:      usingExpr,
			WithCheck:  withCheck,
			Permissive: permissive,
		}
	}
	if err := policyRows.Err(); err != nil {
		return err
	}
	_ = policyRows.Close()

	return nil
}

// inspectTriggers inspects user-defined triggers on tables.
func inspectTriggers(ctx context.Context, dbtx dialect.DBTX, schemaName string, s *schema.Schema) error {
	triggerQuery := `
		SELECT
			c.relname AS table_name,
			t.tgname AS trigger_name,
			pg_get_triggerdef(t.oid) AS definition
		FROM pg_trigger t
		JOIN pg_class c ON c.oid = t.tgrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1
		  AND NOT t.tgisinternal
		ORDER BY c.relname, t.tgname;
	`
	triggerRows, err := dbtx.QueryContext(ctx, triggerQuery, schemaName)
	if err != nil {
		return fmt.Errorf("inspecting triggers in schema %q: %w", schemaName, err)
	}
	defer func() { _ = triggerRows.Close() }()
	for triggerRows.Next() {
		var tblName, triggerName, definition string
		if err := triggerRows.Scan(&tblName, &triggerName, &definition); err != nil {
			return fmt.Errorf("scanning trigger in schema %q: %w", schemaName, err)
		}
		tbl, exists := s.Tables[tblName]
		if !exists {
			continue
		}
		if tbl.Triggers == nil {
			tbl.Triggers = make(map[string]*schema.Trigger)
		}
		tbl.Triggers[triggerName] = &schema.Trigger{
			Name:       triggerName,
			Definition: definition,
		}
	}
	if err := triggerRows.Err(); err != nil {
		return err
	}
	_ = triggerRows.Close()

	return nil
}

// inspectConstraintsAndIndexes inspects primary keys, secondary indexes, foreign keys, and check constraints.
func inspectConstraintsAndIndexes(ctx context.Context, dbtx dialect.DBTX, schemaName string, s *schema.Schema) error {
	// Primary Keys
	pkQuery := `
		SELECT
			c.relname AS table_name,
			con.conname AS constraint_name,
			COALESCE(string_agg(a.attname, ',' ORDER BY u.pos), '') AS pk_columns
		FROM pg_constraint con
		JOIN pg_class c ON c.oid = con.conrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		CROSS JOIN LATERAL unnest(con.conkey) WITH ORDINALITY AS u(attnum, pos)
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = u.attnum
		WHERE n.nspname = $1
		  AND con.contype = 'p'
		GROUP BY c.relname, con.conname;
	`
	pkRows, err := dbtx.QueryContext(ctx, pkQuery, schemaName)
	if err != nil {
		return fmt.Errorf("inspecting primary keys in schema %q: %w", schemaName, err)
	}
	defer func() { _ = pkRows.Close() }()

	for pkRows.Next() {
		var (
			tableName  string
			pkName     string
			colsJoined string
		)
		if err := pkRows.Scan(&tableName, &pkName, &colsJoined); err != nil {
			return fmt.Errorf("scanning primary key in schema %q: %w", schemaName, err)
		}

		if tbl, exists := s.Tables[tableName]; exists && colsJoined != "" {
			var pkCols []string
			for c := range strings.SplitSeq(colsJoined, ",") {
				pkCols = append(pkCols, strings.TrimSpace(c))
			}
			tbl.PrimaryKey = &schema.PrimaryKey{
				Name:    pkName,
				Columns: pkCols,
			}
		}
	}
	if err := pkRows.Err(); err != nil {
		return err
	}
	_ = pkRows.Close()

	// Indexes (excluding primary keys)
	idxQuery := `
		SELECT
			t.relname AS table_name,
			i.relname AS index_name,
			ix.indisunique AS is_unique,
			ix.indisvalid AS is_valid,
			COALESCE(pg_get_expr(ix.indpred, ix.indrelid), '') AS predicate,
			pg_get_indexdef(ix.indexrelid) AS index_def
		FROM pg_index ix
		JOIN pg_class t ON t.oid = ix.indrelid
		JOIN pg_class i ON i.oid = ix.indexrelid
		JOIN pg_namespace n ON n.oid = t.relnamespace
		WHERE n.nspname = $1
		  AND NOT ix.indisprimary
		ORDER BY t.relname, i.relname;
	`
	idxRows, err := dbtx.QueryContext(ctx, idxQuery, schemaName)
	if err != nil {
		return fmt.Errorf("inspecting indexes in schema %q: %w", schemaName, err)
	}
	defer func() { _ = idxRows.Close() }()

	for idxRows.Next() {
		var (
			tableName string
			indexName string
			isUnique  bool
			isValid   bool
			predicate string
			indexDef  string
		)
		if err := idxRows.Scan(&tableName, &indexName, &isUnique, &isValid, &predicate, &indexDef); err != nil {
			return fmt.Errorf("scanning index in schema %q: %w", schemaName, err)
		}

		if tbl, exists := s.Tables[tableName]; exists {
			tbl.Indexes[indexName] = &schema.Index{
				Name:       indexName,
				TableName:  tableName,
				IsUnique:   isUnique,
				Definition: indexDef,
				IsValid:    isValid,
				Predicate:  predicate,
			}
		}
	}
	if err := idxRows.Err(); err != nil {
		return err
	}
	_ = idxRows.Close()

	// Foreign Keys
	fkQuery := `
		SELECT
			c.relname AS table_name,
			con.conname AS constraint_name,
			pg_get_constraintdef(con.oid) AS constraint_def,
			con.convalidated AS is_valid,
			COALESCE(ref_ns.nspname, '') AS ref_schema,
			COALESCE(ref_c.relname, '') AS ref_table
		FROM pg_constraint con
		JOIN pg_class c ON c.oid = con.conrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_class ref_c ON ref_c.oid = con.confrelid
		LEFT JOIN pg_namespace ref_ns ON ref_ns.oid = ref_c.relnamespace
		WHERE n.nspname = $1
		  AND con.contype = 'f'
		ORDER BY c.relname, con.conname;
	`
	fkRows, err := dbtx.QueryContext(ctx, fkQuery, schemaName)
	if err != nil {
		return fmt.Errorf("inspecting foreign keys in schema %q: %w", schemaName, err)
	}
	defer func() { _ = fkRows.Close() }()

	for fkRows.Next() {
		var (
			tableName string
			fkName    string
			fkDef     string
			isValid   bool
			refSchema string
			refTable  string
		)
		if err := fkRows.Scan(&tableName, &fkName, &fkDef, &isValid, &refSchema, &refTable); err != nil {
			return fmt.Errorf("scanning foreign key in schema %q: %w", schemaName, err)
		}

		if tbl, exists := s.Tables[tableName]; exists {
			if refSchema != "" && refSchema != schemaName {
				fkDef = qualifyCrossSchemaFK(fkDef, refSchema, refTable)
			}
			tbl.ForeignKeys[fkName] = &schema.ForeignKey{
				Name:       fkName,
				TableName:  tableName,
				RefSchema:  refSchema,
				RefTable:   refTable,
				Definition: fkDef,
				IsValid:    isValid,
			}
		}
	}
	if err := fkRows.Err(); err != nil {
		return err
	}
	_ = fkRows.Close()

	// Check Constraints (table constraints only)
	checkQuery := `
		SELECT
			c.relname AS table_name,
			con.conname AS constraint_name,
			pg_get_constraintdef(con.oid) AS constraint_def,
			con.convalidated AS is_valid
		FROM pg_constraint con
		JOIN pg_class c ON c.oid = con.conrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1
		  AND con.contype = 'c'
		  AND con.conislocal
		  AND c.relkind IN ('r', 'p')
		ORDER BY c.relname, con.conname;
	`
	checkRows, err := dbtx.QueryContext(ctx, checkQuery, schemaName)
	if err != nil {
		return fmt.Errorf("inspecting check constraints in schema %q: %w", schemaName, err)
	}
	defer func() { _ = checkRows.Close() }()

	for checkRows.Next() {
		var (
			tableName string
			checkName string
			checkDef  string
			isValid   bool
		)
		if err := checkRows.Scan(&tableName, &checkName, &checkDef, &isValid); err != nil {
			return fmt.Errorf("scanning check constraint in schema %q: %w", schemaName, err)
		}

		if tbl, exists := s.Tables[tableName]; exists {
			tbl.Checks[checkName] = &schema.CheckConstraint{
				Name:       checkName,
				TableName:  tableName,
				Definition: checkDef,
				IsValid:    isValid,
			}
		}
	}
	if err := checkRows.Err(); err != nil {
		return err
	}
	_ = checkRows.Close()

	return nil
}

// qualifyCrossSchemaFK rewrites the "REFERENCES <table>(...)" clause in a
// constraint definition to "REFERENCES <schema>.<table>(...)" when the
// referenced table resides in a different schema.
func qualifyCrossSchemaFK(fkDef, refSchema, refTable string) string {
	targetBare := fmt.Sprintf("REFERENCES %s(", refTable)
	targetQuoted := fmt.Sprintf("REFERENCES %q(", refTable)
	qualifiedBare := fmt.Sprintf("REFERENCES %s.%s(", refSchema, refTable)
	qualifiedQuoted := fmt.Sprintf("REFERENCES %q.%q(", refSchema, refTable)

	switch {
	case strings.Contains(fkDef, qualifiedBare) || strings.Contains(fkDef, qualifiedQuoted):
		return fkDef
	case strings.Contains(fkDef, targetQuoted):
		return strings.Replace(fkDef, targetQuoted, qualifiedQuoted, 1)
	case strings.Contains(fkDef, targetBare):
		return strings.Replace(fkDef, targetBare, qualifiedQuoted, 1)
	default:
		return fkDef
	}
}
