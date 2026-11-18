package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/muandane/grizzle/internal/dialect"
	"github.com/muandane/grizzle/internal/schema"
)

// inspectExtensions inspects database-wide installed extensions.
func inspectExtensions(ctx context.Context, dbtx dialect.DBTX, s *schema.Schema) error {
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
		return fmt.Errorf("inspecting extensions: %w", err)
	}
	defer func() { _ = extRows.Close() }()
	for extRows.Next() {
		var name, extSchema, version string
		if err := extRows.Scan(&name, &extSchema, &version); err != nil {
			return fmt.Errorf("scanning extension: %w", err)
		}
		key := strings.ToLower(name)
		s.Extensions[key] = &schema.Extension{
			Name:    key,
			Schema:  extSchema,
			Version: version,
		}
	}
	if err := extRows.Err(); err != nil {
		return err
	}
	_ = extRows.Close()
	return nil
}

// inspectEnums inspects custom ENUM types in the specified schema.
func inspectEnums(ctx context.Context, dbtx dialect.DBTX, schemaName string, s *schema.Schema) error {
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
		return fmt.Errorf("inspecting enums in schema %q: %w", schemaName, err)
	}
	defer func() { _ = enumRows.Close() }()

	for enumRows.Next() {
		var enumName, enumVal string
		if err := enumRows.Scan(&enumName, &enumVal); err != nil {
			return fmt.Errorf("scanning enum in schema %q: %w", schemaName, err)
		}
		e, exists := s.Enums[enumName]
		if !exists {
			e = &schema.Enum{Name: enumName}
			s.Enums[enumName] = e
		}
		e.Values = append(e.Values, enumVal)
	}
	if err := enumRows.Err(); err != nil {
		return err
	}
	_ = enumRows.Close()
	return nil
}

// inspectSequences inspects managed and unmanaged sequences.
func inspectSequences(ctx context.Context, dbtx dialect.DBTX, schemaName string, s *schema.Schema) error {
	seqQuery := `
		SELECT
			c.relname AS seq_name,
			format_type(sq.seqtypid, NULL) AS data_type,
			sq.seqstart AS start_val,
			sq.seqincrement AS increment_val,
			sq.seqmax AS max_val,
			sq.seqmin AS min_val,
			sq.seqcache AS cache_val,
			sq.seqcycle AS is_cycle,
			COALESCE(t.relname, '') AS owned_by_table,
			COALESCE(a.attname, '') AS owned_by_col
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_sequence sq ON sq.seqrelid = c.oid
		LEFT JOIN pg_depend d ON d.objid = c.oid 
		    AND d.classid = 'pg_class'::regclass 
		    AND d.refclassid = 'pg_class'::regclass 
		    AND d.deptype = 'a'
		LEFT JOIN pg_class t ON t.oid = d.refobjid
		LEFT JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = d.refobjsubid
		WHERE n.nspname = $1
		  AND c.relkind = 'S'
		  AND NOT EXISTS (
		      SELECT 1 FROM pg_depend dep 
		      WHERE dep.objid = c.oid 
		        AND dep.deptype IN ('e', 'i')
		  )
		ORDER BY c.relname;
	`
	seqRows, err := dbtx.QueryContext(ctx, seqQuery, schemaName)
	if err != nil {
		return fmt.Errorf("inspecting sequences in schema %q: %w", schemaName, err)
	}
	defer func() { _ = seqRows.Close() }()

	for seqRows.Next() {
		var (
			seqName, dataType, ownedTable, ownedCol          string
			startVal, incrementVal, maxVal, minVal, cacheVal int64
			isCycle                                          bool
		)
		if err := seqRows.Scan(
			&seqName,
			&dataType,
			&startVal,
			&incrementVal,
			&maxVal,
			&minVal,
			&cacheVal,
			&isCycle,
			&ownedTable,
			&ownedCol,
		); err != nil {
			return fmt.Errorf("scanning sequence in schema %q: %w", schemaName, err)
		}

		seq := &schema.Sequence{
			Schema:        schemaName,
			Name:          seqName,
			DataType:      dataType,
			Start:         startVal,
			Increment:     incrementVal,
			MinValue:      &minVal,
			MaxValue:      &maxVal,
			Cache:         cacheVal,
			Cycle:         isCycle,
			OwnedByTable:  ownedTable,
			OwnedByColumn: ownedCol,
		}
		s.Sequences[seqName] = seq

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
		return fmt.Errorf("iterating sequence rows in schema %q: %w", schemaName, err)
	}
	_ = seqRows.Close()
	return nil
}

// inspectDomains inspects managed domains and domain CHECK constraints.
func inspectDomains(ctx context.Context, dbtx dialect.DBTX, schemaName string, s *schema.Schema) error {
	domainQuery := `
		SELECT
			t.typname AS domain_name,
			format_type(t.typbasetype, t.typtypmod) AS base_type,
			t.typnotnull AS not_null,
			COALESCE(pg_get_expr(t.typdefaultbin, 0), '') AS default_expr
		FROM pg_type t
		JOIN pg_namespace n ON n.oid = t.typnamespace
		WHERE n.nspname = $1
		  AND t.typtype = 'd'
		  AND NOT EXISTS (
		      SELECT 1 FROM pg_depend d
		      WHERE d.objid = t.oid AND d.deptype = 'e'
		  )
		ORDER BY t.typname;
	`
	domainRows, err := dbtx.QueryContext(ctx, domainQuery, schemaName)
	if err != nil {
		return fmt.Errorf("inspecting domains in schema %q: %w", schemaName, err)
	}
	defer func() { _ = domainRows.Close() }()

	var domainNames []string
	for domainRows.Next() {
		var domainName, baseType, defaultExpr string
		var notNull bool
		if err := domainRows.Scan(&domainName, &baseType, &notNull, &defaultExpr); err != nil {
			return fmt.Errorf("scanning domain in schema %q: %w", schemaName, err)
		}
		if s.Domains == nil {
			s.Domains = make(map[string]*schema.Domain)
		}
		s.Domains[domainName] = &schema.Domain{
			Name:       domainName,
			BaseType:   baseType,
			IsNullable: !notNull,
			Default:    defaultExpr,
		}
		domainNames = append(domainNames, domainName)
	}
	if err := domainRows.Err(); err != nil {
		return err
	}
	_ = domainRows.Close()

	if len(domainNames) > 0 {
		domainCheckQuery := `
			SELECT
				t.typname AS domain_name,
				con.conname AS constraint_name,
				pg_get_constraintdef(con.oid) AS constraint_def
			FROM pg_constraint con
			JOIN pg_type t ON t.oid = con.contypid
			JOIN pg_namespace n ON n.oid = t.typnamespace
			WHERE n.nspname = $1
			  AND con.conrelid = 0
			ORDER BY t.typname, con.conname;
		`
		dCheckRows, err := dbtx.QueryContext(ctx, domainCheckQuery, schemaName)
		if err != nil {
			return fmt.Errorf("inspecting domain constraints in schema %q: %w", schemaName, err)
		}
		for dCheckRows.Next() {
			var domainName, conName, conDef string
			if err := dCheckRows.Scan(&domainName, &conName, &conDef); err != nil {
				_ = dCheckRows.Close()
				return fmt.Errorf("scanning domain constraint in schema %q: %w", schemaName, err)
			}
			if d := s.Domains[domainName]; d != nil {
				d.Checks = append(d.Checks, &schema.CheckConstraint{
					Name:       conName,
					TableName:  domainName,
					Definition: conDef,
					IsValid:    true,
				})
			}
		}
		if err := dCheckRows.Err(); err != nil {
			_ = dCheckRows.Close()
			return err
		}
		_ = dCheckRows.Close()
	}

	return nil
}

// inspectTypes inspects custom range types and custom composite types.
func inspectTypes(ctx context.Context, dbtx dialect.DBTX, schemaName string, s *schema.Schema) error {
	// Range Types
	rangeQuery := `
		SELECT
			t.typname AS range_name,
			format_type(r.rngsubtype, NULL) AS subtype,
			COALESCE(opc.opcname, '') AS subtype_opclass,
			COALESCE(c.collname, '') AS collation,
			COALESCE(p_can.proname, '') AS canonical,
			COALESCE(p_diff.proname, '') AS subtype_diff,
			COALESCE(mt.typname, '') AS multirange_name
		FROM pg_type t
		JOIN pg_range r ON r.rngtypid = t.oid
		JOIN pg_namespace n ON n.oid = t.typnamespace
		LEFT JOIN pg_collation c ON c.oid = r.rngcollation AND r.rngcollation != 0
		LEFT JOIN pg_opclass opc ON opc.oid = r.rngsubopc AND r.rngsubopc != 0
		LEFT JOIN pg_type mt ON mt.oid = r.rngmultitypid AND r.rngmultitypid != 0
		LEFT JOIN pg_proc p_can ON p_can.oid = r.rngcanonical AND r.rngcanonical != 0
		LEFT JOIN pg_proc p_diff ON p_diff.oid = r.rngsubdiff AND r.rngsubdiff != 0
		WHERE n.nspname = $1
		  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid = t.oid AND d.deptype = 'e')
		ORDER BY t.typname;
	`
	rangeRows, err := dbtx.QueryContext(ctx, rangeQuery, schemaName)
	if err != nil {
		return fmt.Errorf("inspecting range types in schema %q: %w", schemaName, err)
	}
	defer func() { _ = rangeRows.Close() }()

	for rangeRows.Next() {
		var name, rawSubtype, opclass, collation, canonical, diffFunc, multirange string
		if err := rangeRows.Scan(&name, &rawSubtype, &opclass, &collation, &canonical, &diffFunc, &multirange); err != nil {
			return fmt.Errorf("scanning range type in schema %q: %w", schemaName, err)
		}
		if s.RangeTypes == nil {
			s.RangeTypes = make(map[string]*schema.RangeType)
		}
		cleanedSubtype := strings.TrimPrefix(rawSubtype, schemaName+".")
		cleanedSubtype = strings.TrimPrefix(cleanedSubtype, `"`+schemaName+`".`)
		if collation == "default" {
			collation = ""
		}
		s.RangeTypes[name] = &schema.RangeType{
			Schema:         schemaName,
			Name:           name,
			Subtype:        schema.NormalizeType(cleanedSubtype),
			SubtypeOpclass: opclass,
			Collation:      collation,
			Canonical:      canonical,
			SubtypeDiff:    diffFunc,
			MultirangeName: multirange,
		}
	}
	if err := rangeRows.Err(); err != nil {
		return err
	}
	_ = rangeRows.Close()

	// Composite Types
	compQuery := `
		SELECT
			t.typname AS composite_name,
			a.attname AS attr_name,
			format_type(a.atttypid, a.atttypmod) AS attr_type,
			COALESCE(col.collname, '') AS collation
		FROM pg_type t
		JOIN pg_class c ON c.oid = t.typrelid AND c.relkind = 'c'
		JOIN pg_namespace n ON n.oid = t.typnamespace
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
		LEFT JOIN pg_collation col ON col.oid = a.attcollation AND a.attcollation != 0
		WHERE n.nspname = $1
		  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid = t.oid AND d.deptype = 'e')
		ORDER BY t.typname, a.attnum;
	`
	compRows, err := dbtx.QueryContext(ctx, compQuery, schemaName)
	if err != nil {
		return fmt.Errorf("inspecting composite types in schema %q: %w", schemaName, err)
	}
	defer func() { _ = compRows.Close() }()

	for compRows.Next() {
		var compName, attrName, rawType, collation string
		if err := compRows.Scan(&compName, &attrName, &rawType, &collation); err != nil {
			return fmt.Errorf("scanning composite type attribute in schema %q: %w", schemaName, err)
		}
		if s.CompositeTypes == nil {
			s.CompositeTypes = make(map[string]*schema.CompositeType)
		}
		ct, exists := s.CompositeTypes[compName]
		if !exists {
			ct = &schema.CompositeType{
				Schema: schemaName,
				Name:   compName,
			}
			s.CompositeTypes[compName] = ct
		}
		cleanedType := strings.TrimPrefix(rawType, schemaName+".")
		cleanedType = strings.TrimPrefix(cleanedType, `"`+schemaName+`".`)
		if collation == "default" {
			collation = ""
		}
		ct.Attributes = append(ct.Attributes, &schema.CompositeAttribute{
			Name:      attrName,
			DataType:  schema.NormalizeType(cleanedType),
			Collation: collation,
		})
	}
	if err := compRows.Err(); err != nil {
		return err
	}
	_ = compRows.Close()

	return nil
}
