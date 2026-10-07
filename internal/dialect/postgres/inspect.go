package postgres

import (
	"context"
	"fmt"
	"regexp"
	"sort"
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

// pinSearchPath pins search_path to pg_catalog for the remainder of the
// current transaction so pg_get_*def / pg_get_expr emit fully qualified,
// search-path-independent definitions. It is a no-op when dbtx is not inside
// a transaction (e.g. direct *sql.DB inspection, where the statement-scoped
// pin is impossible and the session path is whatever the pool provides).
// Call unpinSearchPath after the pin-affected queries to restore the
// transaction path.
func pinSearchPath(ctx context.Context, dbtx dialect.DBTX) {
	_, _ = dbtx.ExecContext(ctx, "SAVEPOINT grizzle_inspect_pin; SET LOCAL search_path TO pg_catalog;")
}

// unpinSearchPath reverts the pinned path (ROLLBACK TO SAVEPOINT undoes the
// SET LOCAL effect) and discards the savepoint (RELEASE).
func unpinSearchPath(ctx context.Context, dbtx dialect.DBTX) {
	_, _ = dbtx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT grizzle_inspect_pin; RELEASE SAVEPOINT grizzle_inspect_pin;")
}

// Inspect reads the relational state of the specified schema directly from pg_catalog.
func Inspect(ctx context.Context, dbtx dialect.DBTX, schemaName string) (*schema.Schema, error) {
	s := &schema.Schema{
		Name:       schemaName,
		Tables:     make(map[string]*schema.Table),
		Enums:      make(map[string]*schema.Enum),
		Extensions: make(map[string]*schema.Extension),
		Routines:   make(map[string]*schema.Routine),
		Views:      make(map[string]*schema.View),
		Unmanaged:  make(map[string]*schema.UnmanagedObject),
	}

	// 0. Inspect Extensions (database-wide; keyed by lowercased name)
	extQuery := `
		SELECT
			e.extname AS ext_name,
			COALESCE(n.nspname, '') AS ext_schema,
			COALESCE(e.extversion, '') AS ext_version
		FROM pg_extension e
		LEFT JOIN pg_namespace n ON n.oid = e.extnamespace
		ORDER BY e.extname;
	`
	extRows, err := dbtx.QueryContext(ctx, extQuery)
	if err != nil {
		return nil, fmt.Errorf("inspecting extensions: %w", err)
	}
	defer func() { _ = extRows.Close() }()
	for extRows.Next() {
		var name, extSchema, version string
		if err := extRows.Scan(&name, &extSchema, &version); err != nil {
			return nil, fmt.Errorf("scanning extension: %w", err)
		}
		key := strings.ToLower(name)
		s.Extensions[key] = &schema.Extension{
			Name:    key,
			Schema:  extSchema,
			Version: version,
		}
	}
	if err := extRows.Err(); err != nil {
		return nil, err
	}
	_ = extRows.Close()

	// 1. Inspect Custom ENUM Types
	enumQuery := `
		SELECT
			t.typname AS enum_name,
			e.enumlabel AS enum_value
		FROM pg_type t
		JOIN pg_enum e ON e.enumtypid = t.oid
		JOIN pg_namespace n ON n.oid = t.typnamespace
		WHERE n.nspname = $1
		ORDER BY t.typname, e.enumsortorder;
	`
	enumRows, err := dbtx.QueryContext(ctx, enumQuery, schemaName)
	if err != nil {
		return nil, fmt.Errorf("inspecting enums in schema %q: %w", schemaName, err)
	}
	defer func() { _ = enumRows.Close() }()

	for enumRows.Next() {
		var enumName, enumVal string
		if err := enumRows.Scan(&enumName, &enumVal); err != nil {
			return nil, fmt.Errorf("scanning enum in schema %q: %w", schemaName, err)
		}
		e, exists := s.Enums[enumName]
		if !exists {
			e = &schema.Enum{Name: enumName}
			s.Enums[enumName] = e
		}
		e.Values = append(e.Values, enumVal)
	}
	if err := enumRows.Err(); err != nil {
		return nil, err
	}
	_ = enumRows.Close()

	// 2. Inspect Tables and Columns
	colQuery := `
		SELECT
			c.relname AS table_name,
			a.attname AS column_name,
			format_type(a.atttypid, a.atttypmod) AS formatted_type,
			NOT a.attnotnull AS is_nullable,
			COALESCE(pg_get_expr(d.adbin, d.adrelid), '') AS column_default,
			a.attnum AS ordinal_position,
			a.attidentity AS identity_type,
			a.attgenerated AS generated_type
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
		return nil, fmt.Errorf("inspecting columns in schema %q: %w", schemaName, err)
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
		)

		if err := rows.Scan(&tableName, &colName, &rawType, &isNullable, &rawDefault, &position, &identityType, &generatedType); err != nil {
			return nil, fmt.Errorf("scanning column data in schema %q: %w", schemaName, err)
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
		return nil, err
	}
	_ = rows.Close()

	// 2b. Inspect Partitioned Tables
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
		return nil, fmt.Errorf("inspecting partitioned tables in schema %q: %w", schemaName, err)
	}
	defer func() { _ = partRows.Close() }()

	for partRows.Next() {
		var (
			tableName string
			strat     string
			partKey   string
		)
		if err := partRows.Scan(&tableName, &strat, &partKey); err != nil {
			return nil, fmt.Errorf("scanning partitioned table data in schema %q: %w", schemaName, err)
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
		return nil, err
	}
	_ = partRows.Close()

	// 2c. Inspect Attached Partitions
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
		return nil, fmt.Errorf("inspecting partition inheritance in schema %q: %w", schemaName, err)
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
			return nil, fmt.Errorf("scanning partition bounds in schema %q: %w", schemaName, err)
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
		return nil, err
	}
	_ = inhRows.Close()

	// 2d. Inspect RLS flags
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
		return nil, fmt.Errorf("inspecting RLS flags in schema %q: %w", schemaName, err)
	}
	defer func() { _ = rlsRows.Close() }()
	for rlsRows.Next() {
		var tblName string
		var rlsEnabled, rlsForced bool
		if err := rlsRows.Scan(&tblName, &rlsEnabled, &rlsForced); err != nil {
			return nil, fmt.Errorf("scanning RLS flags in schema %q: %w", schemaName, err)
		}
		if tbl, exists := s.Tables[tblName]; exists {
			tbl.RLSEnabled = rlsEnabled
			tbl.RLSForced = rlsForced
		}
	}
	if err := rlsRows.Err(); err != nil {
		return nil, err
	}
	_ = rlsRows.Close()

	// 2e. Inspect row-level security policies. pg_get_expr renders schema
	// references per the session search_path, so live and shadow inspections
	// can disagree; pin search_path to pg_catalog for all pg_get_*def-based
	// queries from here to the end of Inspect (see pinSearchPath).
	pinSearchPath(ctx, dbtx)
	defer unpinSearchPath(ctx, dbtx)

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
		return nil, fmt.Errorf("inspecting policies in schema %q: %w", schemaName, err)
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
			return nil, fmt.Errorf("scanning policy in schema %q: %w", schemaName, err)
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
		return nil, err
	}
	_ = policyRows.Close()

	// 2f. Inspect triggers on tables (managed surface). pg_get_triggerdef
	// gives the canonical definition; the ON <table> reference inside it is
	// schema-qualified and unmapped by the diff normalize step for shadow
	// introspection.
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
		return nil, fmt.Errorf("inspecting triggers in schema %q: %w", schemaName, err)
	}
	defer func() { _ = triggerRows.Close() }()
	for triggerRows.Next() {
		var tblName, triggerName, definition string
		if err := triggerRows.Scan(&tblName, &triggerName, &definition); err != nil {
			return nil, fmt.Errorf("scanning trigger in schema %q: %w", schemaName, err)
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
		return nil, err
	}
	_ = triggerRows.Close()

	// 3. Inspect Primary Keys
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
		return nil, fmt.Errorf("inspecting primary keys in schema %q: %w", schemaName, err)
	}
	defer func() { _ = pkRows.Close() }()

	for pkRows.Next() {
		var (
			tableName  string
			pkName     string
			colsJoined string
		)
		if err := pkRows.Scan(&tableName, &pkName, &colsJoined); err != nil {
			return nil, fmt.Errorf("scanning primary key in schema %q: %w", schemaName, err)
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
		return nil, err
	}
	_ = pkRows.Close()

	// 4. Inspect Indexes (excluding primary keys)
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
		return nil, fmt.Errorf("inspecting indexes in schema %q: %w", schemaName, err)
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
			return nil, fmt.Errorf("scanning index in schema %q: %w", schemaName, err)
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
		return nil, err
	}
	_ = idxRows.Close()

	// 5. Inspect Foreign Keys
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
		return nil, fmt.Errorf("inspecting foreign keys in schema %q: %w", schemaName, err)
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
			return nil, fmt.Errorf("scanning foreign key in schema %q: %w", schemaName, err)
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
		return nil, err
	}
	_ = fkRows.Close()

	// 5b. Inspect Check Constraints (table constraints only; domain checks are
	// out of scope and inherited constraints are managed via their parent table)
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
		return nil, fmt.Errorf("inspecting check constraints in schema %q: %w", schemaName, err)
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
			return nil, fmt.Errorf("scanning check constraint in schema %q: %w", schemaName, err)
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
		return nil, err
	}
	_ = checkRows.Close()

	// 6. Inspect Views
	// 6a. Views and Materialized Views (managed surface). pg_get_viewdef is
	// the canonical definition; schema-qualified references inside it are
	// unmapped by the diff normalize step for shadow introspection.
	viewQuery := `
		SELECT
			c.relname AS view_name,
			c.relkind AS view_kind,
			pg_get_viewdef(c.oid) AS definition,
			COALESCE((
				SELECT string_agg(a.attname, ',' ORDER BY a.attnum)
				FROM pg_attribute a
				WHERE a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
			), '') AS columns_csv
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1
		  AND c.relkind IN ('v', 'm')
		ORDER BY c.relname;
	`
	viewRows, err := dbtx.QueryContext(ctx, viewQuery, schemaName)
	if err != nil {
		return nil, fmt.Errorf("inspecting views in schema %q: %w", schemaName, err)
	}
	defer func() { _ = viewRows.Close() }()

	for viewRows.Next() {
		var viewName, viewKind, definition, columnsCSV string
		if err := viewRows.Scan(&viewName, &viewKind, &definition, &columnsCSV); err != nil {
			return nil, fmt.Errorf("scanning view in schema %q: %w", schemaName, err)
		}
		if s.Views == nil {
			s.Views = make(map[string]*schema.View)
		}
		v := &schema.View{
			Name:       viewName,
			IsMatView:  viewKind == "m",
			Definition: definition,
		}
		if columnsCSV != "" {
			v.Columns = strings.Split(columnsCSV, ",")
		}
		s.Views[viewName] = v
	}
	if err := viewRows.Err(); err != nil {
		return nil, err
	}
	_ = viewRows.Close()

	// 6b. View column dependencies: managed views are folded into column-drop
	// protection by diff.findSurvivingViewDeps using the canonical view
	// definition; no unmanaged registration is needed.

	// 6c. Triggers on managed tables are collected in 2f (Table.Triggers).
	// Triggers are part of the managed surface as of Phase 1; no unmanaged
	// trigger registration is needed here.

	// 6d. Managed routines. Plain SQL/plpgsql functions become part of the
	// managed surface: desired side comes from the shadow compile, keyed by
	// name + identity args (pg_get_functiondef is the canonical definition).
	// Procedures and aggregates stay unmanaged (pg_get_functiondef does not
	// support them) and are handled by 6d.1 below.
	routineQuery := `
		SELECT
			p.proname AS routine_name,
			pg_get_function_identity_arguments(p.oid) AS identity_args,
			pg_get_function_result(p.oid) AS return_type,
			l.lanname AS language,
			CASE p.provolatile WHEN 'i' THEN 'IMMUTABLE' WHEN 's' THEN 'STABLE' ELSE 'VOLATILE' END AS volatility,
			p.prosecdef AS security_definer,
			pg_get_functiondef(p.oid) AS definition
		FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		JOIN pg_language l ON l.oid = p.prolang
		WHERE n.nspname = $1
		  AND p.prokind = 'f'
		  AND NOT EXISTS (
		      SELECT 1 FROM pg_depend d
		      WHERE d.objid = p.oid AND d.deptype = 'e'
		  )
		ORDER BY p.proname, identity_args;
	`
	routineRows, err := dbtx.QueryContext(ctx, routineQuery, schemaName)
	if err != nil {
		return nil, fmt.Errorf("inspecting routines in schema %q: %w", schemaName, err)
	}
	for routineRows.Next() {
		var name, identityArgs, returnType, lang, volatility, definition string
		var securityDefiner bool
		if err := routineRows.Scan(&name, &identityArgs, &returnType, &lang, &volatility, &securityDefiner, &definition); err != nil {
			_ = routineRows.Close()
			return nil, fmt.Errorf("scanning routine in schema %q: %w", schemaName, err)
		}
		if s.Routines == nil {
			s.Routines = make(map[string]*schema.Routine)
		}
		s.Routines[schema.RoutineKey(name, identityArgs)] = &schema.Routine{
			Name:            name,
			Kind:            "FUNCTION",
			IdentityArgs:    identityArgs,
			ReturnType:      returnType,
			Language:        lang,
			Volatility:      volatility,
			SecurityDefiner: securityDefiner,
			Definition:      definition,
		}
	}
	if err := routineRows.Err(); err != nil {
		_ = routineRows.Close()
		return nil, err
	}
	_ = routineRows.Close()

	// 6d.1 Unmanaged procedures and aggregates (functions are managed above).
	procQuery := `
		SELECT
			p.proname AS proc_name,
			COALESCE(p.prosrc, '') AS source
		FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = $1
		  AND p.prokind <> 'f'
		  AND NOT EXISTS (
		      SELECT 1 FROM pg_depend d
		      WHERE d.objid = p.oid AND d.deptype = 'e'
		  )
		ORDER BY p.proname;
	`
	procRows, err := dbtx.QueryContext(ctx, procQuery, schemaName)
	if err != nil {
		return nil, fmt.Errorf("inspecting procedures in schema %q: %w", schemaName, err)
	}
	defer func() { _ = procRows.Close() }()

	procSources := make(map[string]string)
	for procRows.Next() {
		var procName, procSource string
		if err := procRows.Scan(&procName, &procSource); err != nil {
			return nil, fmt.Errorf("scanning procedure in schema %q: %w", schemaName, err)
		}
		key := "function:" + procName
		s.Unmanaged[key] = &schema.UnmanagedObject{
			Name: procName,
			Kind: schema.UnmanagedFunction,
		}
		procSources[procName] = procSource
	}
	if err := procRows.Err(); err != nil {
		return nil, err
	}
	_ = procRows.Close()

	// 6d.1 Function Table/Column Dependencies from pg_depend.
	// SQL-standard function bodies (BEGIN ATOMIC, PostgreSQL 14+) record
	// normal ('n') dependencies on every referenced table and column, giving
	// exact protection parity with views.
	funcDepQuery := `
		SELECT
			p.proname AS proc_name,
			dep_c.relname AS referenced_table,
			COALESCE(a.attname, '') AS referenced_column
		FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		JOIN pg_depend d ON d.objid = p.oid AND d.classid = 'pg_proc'::regclass AND d.refclassid = 'pg_class'::regclass
		JOIN pg_class dep_c ON dep_c.oid = d.refobjid
		LEFT JOIN pg_attribute a ON a.attrelid = dep_c.oid AND a.attnum = d.refobjsubid AND NOT a.attisdropped
		WHERE n.nspname = $1
		  AND d.deptype = 'n'
		  AND dep_c.relkind IN ('r', 'p')
		ORDER BY p.proname, dep_c.relname, a.attname;
	`
	funcDepRows, err := dbtx.QueryContext(ctx, funcDepQuery, schemaName)
	if err != nil {
		return nil, fmt.Errorf("inspecting function dependencies in schema %q: %w", schemaName, err)
	}
	defer func() { _ = funcDepRows.Close() }()

	funcsWithDeps := make(map[string]bool)
	seenFuncDeps := make(map[string]bool)
	for funcDepRows.Next() {
		var procName, refTable, refCol string
		if err := funcDepRows.Scan(&procName, &refTable, &refCol); err != nil {
			return nil, fmt.Errorf("scanning function dependency: %w", err)
		}
		obj, ok := s.Unmanaged["function:"+procName]
		if !ok {
			continue
		}
		dedupeKey := procName + "\x00" + refTable + "\x00" + refCol
		if seenFuncDeps[dedupeKey] {
			continue
		}
		seenFuncDeps[dedupeKey] = true
		obj.DependsOn = append(obj.DependsOn, schema.DependencyRef{
			Table:  refTable,
			Column: refCol,
		})
		funcsWithDeps[procName] = true
	}
	if err := funcDepRows.Err(); err != nil {
		return nil, err
	}
	_ = funcDepRows.Close()

	// 6d.2 Heuristic source scan for string-bodied functions. Functions
	// declared with a quoted body (LANGUAGE sql AS '...', plpgsql) record no
	// pg_depend entries; scan their source for managed table names so
	// destructive operations stay protected. A false positive only adds
	// protection, never removes it.
	for _, procName := range sortedProcNames(procSources) {
		if funcsWithDeps[procName] {
			continue
		}
		source := procSources[procName]
		if strings.TrimSpace(source) == "" {
			continue
		}
		obj, ok := s.Unmanaged["function:"+procName]
		if !ok {
			continue
		}
		for _, tblName := range sortedManagedTableNames(s) {
			if tblName == "" {
				continue
			}
			re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(tblName) + `\b`)
			if re.MatchString(source) {
				obj.DependsOn = append(obj.DependsOn, schema.DependencyRef{Table: tblName})
			}
		}
	}

	// 6e. Sequences not owned by managed tables
	seqQuery := `
		SELECT
			c.relname AS seq_name,
			COALESCE(t.relname, '') AS owned_by_table,
			COALESCE(a.attname, '') AS owned_by_col
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_depend d ON d.objid = c.oid AND d.classid = 'pg_class'::regclass AND d.refclassid = 'pg_class'::regclass AND d.deptype = 'a'
		LEFT JOIN pg_class t ON t.oid = d.refobjid
		LEFT JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = d.refobjsubid
		WHERE n.nspname = $1
		  AND c.relkind = 'S'
		ORDER BY c.relname;
	`
	seqRows, err := dbtx.QueryContext(ctx, seqQuery, schemaName)
	if err != nil {
		return nil, fmt.Errorf("inspecting sequences in schema %q: %w", schemaName, err)
	}
	defer func() { _ = seqRows.Close() }()

	for seqRows.Next() {
		var seqName, ownedTable, ownedCol string
		if err := seqRows.Scan(&seqName, &ownedTable, &ownedCol); err != nil {
			return nil, fmt.Errorf("scanning sequence in schema %q: %w", schemaName, err)
		}
		if ownedTable == "" || s.Tables[ownedTable] == nil {
			seqObj := &schema.UnmanagedObject{
				Name: seqName,
				Kind: schema.UnmanagedSequence,
			}
			if ownedTable != "" {
				seqObj.Table = ownedTable
				seqObj.DependsOn = append(seqObj.DependsOn, schema.DependencyRef{
					Table:  ownedTable,
					Column: ownedCol,
				})
			}
			s.Unmanaged["sequence:"+seqName] = seqObj
		}
	}
	if err := seqRows.Err(); err != nil {
		return nil, err
	}
	_ = seqRows.Close()

	// 6f. Domains
	domainQuery := `
		SELECT
			t.typname AS domain_name
		FROM pg_type t
		JOIN pg_namespace n ON n.oid = t.typnamespace
		WHERE n.nspname = $1
		  AND t.typtype = 'd'
		ORDER BY t.typname;
	`
	domainRows, err := dbtx.QueryContext(ctx, domainQuery, schemaName)
	if err != nil {
		return nil, fmt.Errorf("inspecting domains in schema %q: %w", schemaName, err)
	}
	defer func() { _ = domainRows.Close() }()

	for domainRows.Next() {
		var domainName string
		if err := domainRows.Scan(&domainName); err != nil {
			return nil, fmt.Errorf("scanning domain in schema %q: %w", schemaName, err)
		}
		s.Unmanaged["domain:"+domainName] = &schema.UnmanagedObject{
			Name: domainName,
			Kind: schema.UnmanagedDomain,
		}
	}
	if err := domainRows.Err(); err != nil {
		return nil, err
	}

	return s, nil
}

// InspectSchemas reads the relational states across all specified schema names.
func InspectSchemas(ctx context.Context, dbtx dialect.DBTX, schemaNames []string) (map[string]*schema.Schema, error) {
	schemas := make(map[string]*schema.Schema, len(schemaNames))
	for _, name := range schemaNames {
		s, err := Inspect(ctx, dbtx, name)
		if err != nil {
			return nil, err
		}
		schemas[name] = s
	}
	return schemas, nil
}

func qualifyCrossSchemaFK(fkDef, refSchema, refTable string) string {
	if refSchema == "" {
		return fkDef
	}
	if strings.Contains(fkDef, refSchema+".") || strings.Contains(fkDef, `"`+refSchema+`".`) {
		return fkDef
	}
	upper := strings.ToUpper(fkDef)
	idx := strings.Index(upper, "REFERENCES ")
	if idx == -1 {
		return fkDef
	}
	rest := fkDef[idx+len("REFERENCES "):]
	before, _, ok := strings.Cut(rest, "(")
	if !ok {
		return fkDef
	}
	tblPart := strings.TrimSpace(before)
	if strings.Contains(tblPart, ".") {
		return fkDef
	}
	return fkDef[:idx] + "REFERENCES " + refSchema + "." + strings.TrimSpace(rest)
}

// sortedProcNames returns proc source keys in deterministic order.
func sortedProcNames(procSources map[string]string) []string {
	names := make([]string, 0, len(procSources))
	for n := range procSources {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// sortedManagedTableNames returns managed table names in deterministic order.
func sortedManagedTableNames(s *schema.Schema) []string {
	names := make([]string, 0, len(s.Tables))
	for n := range s.Tables {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
