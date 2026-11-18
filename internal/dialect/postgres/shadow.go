package postgres

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/muandane/grizzle/internal/dialect"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
)

var (
	validIdentRegex     = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	undefinedTypeRe     = regexp.MustCompile(`(?i)\btype\s+("?[a-zA-Z0-9_.]+"?(?:\."?[a-zA-Z0-9_.]+"?)?)\s+does\s+not\s+exist\b`)
	undefinedFuncRe     = regexp.MustCompile(`(?i)\bfunction\s+("?[a-zA-Z0-9_.]+"?(?:\([^)]*\))?)\s+does\s+not\s+exist\b`)
	undefinedRelationRe = regexp.MustCompile(`(?i)\b(relation|table)\s+("?[a-zA-Z0-9_.]+"?(?:\."?[a-zA-Z0-9_.]+"?)?)\s+does\s+not\s+exist\b`)
	undefinedColumnRe   = regexp.MustCompile(`(?i)\bcolumn\s+("?[a-zA-Z0-9_.]+"?)(?:\s+of\s+relation\s+("?[a-zA-Z0-9_.]+"?))?\s+does\s+not\s+exist\b`)
	duplicateObjectRe   = regexp.MustCompile(`(?i)\b(relation|table|type|function|schema|index)\s+("?[a-zA-Z0-9_.]+"?(?:\."?[a-zA-Z0-9_.]+"?)?)\s+already\s+exists\b`)
	partitionTableRe    = regexp.MustCompile(`(?i)partitioned\s+table\s+("?[a-zA-Z0-9_.]+"?)`)
)

// ValidateIdentifier ensures schema names contain only safe SQL identifier characters.
func ValidateIdentifier(ident string) error {
	if !validIdentRegex.MatchString(ident) {
		return fmt.Errorf("invalid SQL identifier: %q", ident)
	}
	return nil
}

// SetupShadowSchema creates a clean, isolated temporary schema for DDL compilation.
func SetupShadowSchema(ctx context.Context, dbtx dialect.DBTX, shadowSchema string) error {
	if err := ValidateIdentifier(shadowSchema); err != nil {
		return err
	}

	cleanSQL := fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE; CREATE SCHEMA %q;", shadowSchema, shadowSchema)
	if _, err := dbtx.ExecContext(ctx, cleanSQL); err != nil {
		return fmt.Errorf("failed creating shadow schema %q: %w", shadowSchema, err)
	}
	return nil
}

// RunShadowDDL sets search_path to the shadow schema, runs the user's schemaSQL, and restores search_path.
func RunShadowDDL(ctx context.Context, dbtx dialect.DBTX, shadowSchema, targetSchema, schemaSQL string) error {
	if err := ValidateIdentifier(shadowSchema); err != nil {
		return err
	}
	if targetSchema == "" {
		targetSchema = "public"
	}
	if err := ValidateIdentifier(targetSchema); err != nil {
		return err
	}

	setPathSQL := fmt.Sprintf("SET search_path TO %q, %q, public;", shadowSchema, targetSchema)
	if _, err := dbtx.ExecContext(ctx, setPathSQL); err != nil {
		return fmt.Errorf("failed setting search_path to %q: %w", shadowSchema, err)
	}

	shadowMap := map[string]string{targetSchema: shadowSchema}

	// Strip CREATE EXTENSION from the main DDL body, then best-effort install
	// into the shadow schema so extension-provided types (citext, etc.) resolve
	// during compile. Failures are ignored per statement (privilege / already
	// installed); the desired Extensions IR comes from statement parsing.
	cleanedSQL, extStmts := schema.StripExtensionStatements(schemaSQL)
	for _, extStmt := range extStmts {
		rewritten := rewriteExtensionSchema(RewriteShadowSQL(extStmt, shadowMap), shadowSchema)
		_, _ = dbtx.ExecContext(ctx, rewritten)
	}

	ddlSQL := cleanedSQL
	if strings.TrimSpace(ddlSQL) == "" {
		ddlSQL = schemaSQL // no extensions stripped; run original
		if len(extStmts) > 0 {
			ddlSQL = "" // schema was extension-only
		}
	}
	if strings.TrimSpace(ddlSQL) != "" {
		rewrittenSQL := RewriteShadowSQL(ddlSQL, shadowMap)
		if _, err := dbtx.ExecContext(ctx, rewrittenSQL); err != nil {
			return fmt.Errorf("schema compilation in shadow schema failed: %w", WrapShadowCompileError(err, schemaSQL))
		}
	}

	restorePathSQL := fmt.Sprintf("SET search_path TO %q;", shadowSchema)
	if _, err := dbtx.ExecContext(ctx, restorePathSQL); err != nil {
		return fmt.Errorf("failed restoring search_path to %q: %w", shadowSchema, err)
	}

	return nil
}

// DropShadowSchema safely destroys the temporary shadow schema.
func DropShadowSchema(ctx context.Context, dbtx dialect.DBTX, shadowSchema string) error {
	if err := ValidateIdentifier(shadowSchema); err != nil {
		return err
	}
	dropSQL := fmt.Sprintf("DROP SCHEMA IF EXISTS %q CASCADE;", shadowSchema)
	if _, err := dbtx.ExecContext(ctx, dropSQL); err != nil {
		return fmt.Errorf("failed dropping shadow schema %q: %w", shadowSchema, err)
	}
	return nil
}

// ComputeShadowSchemas returns a mapping from each target schema to its mirroring shadow schema.
// Names are bounded to PostgreSQL's 63-byte identifier limit. When truncation
// would collide, a deterministic FNV-1a suffix keyed on the full target name
// disambiguates. Single schema "public" with the default prefix stays
// "_grizzle_shadow" for backward compatibility when the prefix is exactly that
// default (unique per-run prefixes always include the target disambiguator).
func ComputeShadowSchemas(shadowPrefix string, targetSchemas []string) map[string]string {
	prefix := shadowPrefix
	if prefix == "" {
		prefix = "_grizzle_shadow"
	}
	m := make(map[string]string, len(targetSchemas))
	if len(targetSchemas) == 1 && prefix == "_grizzle_shadow" && targetSchemas[0] == "public" {
		m[targetSchemas[0]] = "_grizzle_shadow"
		return m
	}
	used := make(map[string]string, len(targetSchemas))
	for _, s := range targetSchemas {
		name := boundShadowName(prefix, s)
		if other, ok := used[name]; ok && other != s {
			// Extremely unlikely with FNV suffix; force a second suffix pass.
			name = boundShadowName(prefix+"_"+fmt.Sprintf("%08x", fnv32a(prefix+s)), s)
		}
		used[name] = s
		m[s] = name
	}
	return m
}

const pgMaxIdentLen = 63

func fnv32a(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

// boundShadowName builds prefix_target truncated to 63 bytes with a
// deterministic hash suffix when truncation is required.
func boundShadowName(prefix, target string) string {
	name := prefix + "_" + target
	if len(name) <= pgMaxIdentLen {
		return name
	}
	suffix := fmt.Sprintf("_%08x", fnv32a(target))
	baseBudget := pgMaxIdentLen - len(suffix)
	if baseBudget < 1 {
		return suffix[1:] // unreachable for normal prefixes; keep identifier-shaped
	}
	base := prefix + "_" + target
	if len(base) > baseBudget {
		base = base[:baseBudget]
	}
	return base + suffix
}

// SetupShadowSchemas creates clean, isolated temporary shadow schemas for DDL compilation.
func SetupShadowSchemas(ctx context.Context, dbtx dialect.DBTX, shadowSchemas []string) error {
	for _, s := range shadowSchemas {
		if err := SetupShadowSchema(ctx, dbtx, s); err != nil {
			return err
		}
	}
	return nil
}

// DropShadowSchemas destroys all temporary shadow schemas.
func DropShadowSchemas(ctx context.Context, dbtx dialect.DBTX, shadowSchemas []string) error {
	for _, s := range shadowSchemas {
		if err := DropShadowSchema(ctx, dbtx, s); err != nil {
			return err
		}
	}
	return nil
}

// RunMultiShadowDDL configures search_path across all shadow schemas, rewrites schema references,
// executes the desired schema SQL in shadow, and restores search_path.
func RunMultiShadowDDL(ctx context.Context, dbtx dialect.DBTX, shadowMap map[string]string, targetSchemas []string, schemaSQL string) error {
	var shadowQuoted []string
	for _, t := range targetSchemas {
		if shadow, ok := shadowMap[t]; ok {
			if err := ValidateIdentifier(shadow); err != nil {
				return err
			}
			shadowQuoted = append(shadowQuoted, fmt.Sprintf("%q", shadow))
		}
	}
	if len(shadowQuoted) == 0 {
		return fmt.Errorf("no shadow schemas configured")
	}

	var targetQuoted []string
	for _, t := range targetSchemas {
		if err := ValidateIdentifier(t); err != nil {
			return err
		}
		targetQuoted = append(targetQuoted, fmt.Sprintf("%q", t))
	}
	setPathSQL := fmt.Sprintf("SET search_path TO %s, %s, public;", strings.Join(shadowQuoted, ", "), strings.Join(targetQuoted, ", "))
	if _, err := dbtx.ExecContext(ctx, setPathSQL); err != nil {
		return fmt.Errorf("failed setting multi-schema shadow search_path: %w", err)
	}

	cleanedSQL, extStmts := schema.StripExtensionStatements(schemaSQL)
	primaryShadow := shadowQuoted[0]
	// Best-effort install extensions into the first shadow schema.
	for _, extStmt := range extStmts {
		rewritten := rewriteExtensionSchema(RewriteShadowSQL(extStmt, shadowMap), strings.Trim(primaryShadow, `"`))
		_, _ = dbtx.ExecContext(ctx, rewritten)
	}
	ddlSQL := cleanedSQL
	if strings.TrimSpace(ddlSQL) == "" && len(extStmts) == 0 {
		ddlSQL = schemaSQL
	}
	if strings.TrimSpace(ddlSQL) != "" {
		rewrittenSQL := RewriteShadowSQL(ddlSQL, shadowMap)
		if _, err := dbtx.ExecContext(ctx, rewrittenSQL); err != nil {
			return fmt.Errorf("schema compilation in shadow schema failed: %w", WrapShadowCompileError(err, schemaSQL))
		}
	}

	restorePathSQL := fmt.Sprintf("SET search_path TO %s;", strings.Join(shadowQuoted, ", "))
	if _, err := dbtx.ExecContext(ctx, restorePathSQL); err != nil {
		return fmt.Errorf("failed restoring shadow search_path: %w", err)
	}

	return nil
}

func parseIndexAndTable(stmt string) (idxName, tblName string) {
	tokens := strings.Fields(stmt)
	for i := range tokens {
		tokUpper := strings.ToUpper(tokens[i])
		if tokUpper == "INDEX" {
			j := i + 1
			for j < len(tokens) {
				ju := strings.ToUpper(tokens[j])
				if ju == "CONCURRENTLY" || ju == "IF" || ju == "NOT" || ju == "EXISTS" {
					j++
					continue
				}
				break
			}
			if j < len(tokens) {
				idxName = strings.Trim(tokens[j], `"'`+"`;")
				for k := j + 1; k < len(tokens); k++ {
					if strings.ToUpper(tokens[k]) == "ON" && k+1 < len(tokens) {
						rawTbl := tokens[k+1]
						if paren := strings.Index(rawTbl, "("); paren != -1 {
							rawTbl = rawTbl[:paren]
						}
						tblParts := strings.Split(rawTbl, ".")
						tblName = strings.Trim(tblParts[len(tblParts)-1], `"'`+"`;")
						return idxName, tblName
					}
				}
			}
		}
	}
	return idxName, tblName
}

func wrapImmutableIndexError(err error, sqlStr string) error {
	if err == nil {
		return nil
	}
	errStr := err.Error()
	if !strings.Contains(strings.ToLower(errStr), "must be marked immutable") && !strings.Contains(errStr, "42P17") {
		return err
	}
	statements := strings.SplitSeq(sqlStr, ";")
	for stmt := range statements {
		stmtTrim := strings.TrimSpace(stmt)
		upper := strings.ToUpper(stmtTrim)
		if strings.HasPrefix(upper, "CREATE ") && strings.Contains(upper, "INDEX ") {
			idxName, tblName := parseIndexAndTable(stmtTrim)
			if idxName != "" && tblName != "" {
				return fmt.Errorf("index %q on table %q: expression must be IMMUTABLE: %w", idxName, tblName, err)
			}
		}
	}
	return fmt.Errorf("index expression must be IMMUTABLE: %w", err)
}

func isWhitespace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

func lookupShadow(shadowMap map[string]string, name string) (string, bool) {
	if s, ok := shadowMap[name]; ok {
		return s, true
	}
	if s, ok := shadowMap[strings.ToLower(name)]; ok {
		return s, true
	}
	for k, v := range shadowMap {
		if strings.EqualFold(k, name) {
			return v, true
		}
	}
	return "", false
}

// RewriteShadowSQL redirects references to target schemas in SQL to their corresponding shadow schemas.
func RewriteShadowSQL(sqlStr string, shadowMap map[string]string) string {
	if len(shadowMap) == 0 {
		return sqlStr
	}

	var b strings.Builder
	b.Grow(len(sqlStr))

	n := len(sqlStr)
	i := 0
	var prevWord string

	for i < n {
		ch := sqlStr[i]

		// Single-line comment --
		if ch == '-' && i+1 < n && sqlStr[i+1] == '-' {
			start := i
			for i < n && sqlStr[i] != '\n' {
				i++
			}
			b.WriteString(sqlStr[start:i])
			continue
		}

		// Multi-line comment /* ... */
		if ch == '/' && i+1 < n && sqlStr[i+1] == '*' {
			start := i
			i += 2
			for i+1 < n && (sqlStr[i] != '*' || sqlStr[i+1] != '/') {
				i++
			}
			if i+1 < n {
				i += 2
			}
			b.WriteString(sqlStr[start:i])
			continue
		}

		// String literal '...'
		if ch == '\'' {
			start := i
			i++
			for i < n {
				if sqlStr[i] == '\'' {
					if i+1 < n && sqlStr[i+1] == '\'' {
						i += 2 // Escaped quote ''
						continue
					}
					i++
					break
				}
				i++
			}
			b.WriteString(sqlStr[start:i])
			continue
		}

		// Quoted identifier "..."
		if ch == '"' {
			start := i
			i++
			for i < n && sqlStr[i] != '"' {
				i++
			}
			if i < n {
				i++ // consume closing quote
			}
			ident := sqlStr[start+1 : i-1]
			// Check if followed by dot .
			if i < n && sqlStr[i] == '.' {
				if shadow, ok := lookupShadow(shadowMap, ident); ok {
					fmt.Fprintf(&b, "%q.", shadow)
					i++ // consume .
					prevWord = ident
					continue
				}
			}
			// Check if preceded by SCHEMA (e.g. CREATE SCHEMA "billing" or CREATE SCHEMA IF NOT EXISTS "billing")
			if strings.EqualFold(prevWord, "SCHEMA") || strings.EqualFold(prevWord, "EXISTS") {
				if shadow, ok := lookupShadow(shadowMap, ident); ok {
					fmt.Fprintf(&b, "%q", shadow)
					prevWord = ident
					continue
				}
			}
			b.WriteString(sqlStr[start:i])
			prevWord = ident
			continue
		}

		// Unquoted identifier or keyword
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch == '_' {
			start := i
			for i < n && ((sqlStr[i] >= 'a' && sqlStr[i] <= 'z') || (sqlStr[i] >= 'A' && sqlStr[i] <= 'Z') || (sqlStr[i] >= '0' && sqlStr[i] <= '9') || sqlStr[i] == '_') {
				i++
			}
			word := sqlStr[start:i]

			// Check if followed by dot .
			if i < n && sqlStr[i] == '.' {
				if shadow, ok := lookupShadow(shadowMap, word); ok {
					b.WriteString(shadow)
					b.WriteByte('.')
					i++ // consume .
					prevWord = word
					continue
				}
			}

			// Check if preceded by SCHEMA (e.g. CREATE SCHEMA billing or CREATE SCHEMA IF NOT EXISTS billing)
			if strings.EqualFold(prevWord, "SCHEMA") || strings.EqualFold(prevWord, "EXISTS") {
				if shadow, ok := lookupShadow(shadowMap, word); ok {
					b.WriteString(shadow)
					prevWord = word
					continue
				}
			}

			b.WriteString(word)
			prevWord = word
			continue
		}

		// Non-identifier character (spaces, punctuation, operators)
		b.WriteByte(ch)
		if !isWhitespace(ch) {
			if ch == ';' {
				prevWord = ""
			}
		}
		i++
	}

	return b.String()
}

// rewriteExtensionSchema forces WITH SCHEMA to the shadow schema so
// extension objects land in the compile sandbox when possible.
func rewriteExtensionSchema(stmt, shadowSchema string) string {
	upper := strings.ToUpper(stmt)
	if strings.Contains(upper, "WITH SCHEMA") {
		// Replace existing WITH SCHEMA clause target.
		re := regexp.MustCompile(`(?i)WITH\s+SCHEMA\s+("[^"]+"|[a-zA-Z_][\w$]*)`)
		return re.ReplaceAllString(stmt, "WITH SCHEMA "+quoteIdentifier(shadowSchema))
	}
	trimmed := strings.TrimRight(strings.TrimSpace(stmt), ";")
	return trimmed + " WITH SCHEMA " + quoteIdentifier(shadowSchema)
}

func wrapPartitionShadowError(err error, sqlStr ...string) error {
	if err == nil {
		return nil
	}
	errStr := strings.ToLower(err.Error())
	isPartErr := (strings.Contains(errStr, "unique constraint on partitioned table") ||
		strings.Contains(errStr, "primary key on partitioned table")) &&
		strings.Contains(errStr, "must include all partitioning columns")
	if isPartErr {
		tblName := parsePartitionTable(err.Error())
		if tblName == "" && len(sqlStr) > 0 && sqlStr[0] != "" {
			tblName = parsePartitionTableFromSQL(sqlStr[0])
		}
		if tblName != "" {
			return fmt.Errorf("table %q: %w: %w", tblName, plan.ErrPartitionKeyNotInUnique, err)
		}
		return fmt.Errorf("%w: %w", plan.ErrPartitionKeyNotInUnique, err)
	}
	return err
}

func parsePartitionTable(errStr string) string {
	m := partitionTableRe.FindStringSubmatch(errStr)
	if len(m) > 1 && m[1] != "" {
		tbl := cleanIdentifier(m[1])
		if !strings.EqualFold(tbl, "must") {
			return tbl
		}
	}
	return ""
}

func parsePartitionTableFromSQL(sqlStr string) string {
	stmts := schema.SplitStatements(sqlStr)
	for _, stmt := range stmts {
		if strings.Contains(strings.ToUpper(stmt), "PARTITION BY") {
			target := parseStatementTarget(stmt)
			if target != "" {
				return cleanIdentifier(extractBareName(target))
			}
		}
	}
	return ""
}

// WrapShadowCompileError maps shadow compile failures to the failing object/statement and an actionable hint.
func WrapShadowCompileError(err error, sqlStr string) error {
	if err == nil {
		return nil
	}

	// 1. Non-immutable index expressions (existing wrapImmutableIndexError, keep index and table names)
	if immErr := wrapImmutableIndexError(err, sqlStr); immErr != err {
		return immErr
	}

	// 2. Partition key issues (existing wrapPartitionShadowError)
	if partErr := wrapPartitionShadowError(err, sqlStr); partErr != err {
		return partErr
	}

	errStr := err.Error()

	var pgErr *pgconn.PgError
	errors.As(err, &pgErr)

	// 3. Missing extension / undefined type
	if m := undefinedTypeRe.FindStringSubmatch(errStr); m != nil || (pgErr != nil && pgErr.Code == "42704") {
		var rawType string
		if m != nil {
			rawType = m[1]
		} else if pgErr != nil {
			rawType = pgErr.DataTypeName
		}
		if typeName := cleanIdentifier(rawType); typeName != "" {
			objDesc := fmt.Sprintf("type %q", typeName)
			stmtTarget := findStatementForIdentifier(sqlStr, typeName, objDesc)
			target := formatFullTarget(stmtTarget, objDesc)
			return fmt.Errorf("%s: hint: missing extension or type definition: %w", target, err)
		}
	}

	// 4. Undefined function
	if m := undefinedFuncRe.FindStringSubmatch(errStr); m != nil || (pgErr != nil && pgErr.Code == "42883") {
		var funcRaw string
		if m != nil {
			funcRaw = m[1]
		} else if pgErr != nil {
			funcRaw = pgErr.Routine
		}
		if funcRaw != "" {
			fnDisplay, fnSearch := formatFuncNames(funcRaw)
			objDesc := fmt.Sprintf("function %q", fnDisplay)
			stmtTarget := findStatementForIdentifier(sqlStr, fnSearch, objDesc)
			target := formatFullTarget(stmtTarget, objDesc)
			return fmt.Errorf("%s: hint: missing extension (e.g. 'uuid-ossp' or pgcrypto) or function definition: %w", target, err)
		}
	}

	// 5. Missing column (checked before relation to catch 'column X of relation Y does not exist')
	if m := undefinedColumnRe.FindStringSubmatch(errStr); m != nil || (pgErr != nil && pgErr.Code == "42703") {
		var colName, tblInErr string
		if m != nil {
			colName = cleanIdentifier(m[1])
			if len(m) > 2 {
				tblInErr = cleanIdentifier(m[2])
			}
		} else if pgErr != nil {
			colName = cleanIdentifier(pgErr.ColumnName)
			tblInErr = cleanIdentifier(pgErr.TableName)
		}
		if colName != "" {
			objDesc := fmt.Sprintf("column %q", colName)
			var stmtTarget string
			if tblInErr != "" {
				stmtTarget = fmt.Sprintf("table %q", tblInErr)
			} else {
				stmtTarget = findStatementForIdentifier(sqlStr, colName, objDesc)
			}
			target := formatFullTarget(stmtTarget, objDesc)
			return fmt.Errorf("%s: hint: check column name spelling or declaration order: %w", target, err)
		}
	}

	// 6. Missing table/relation in foreign key or view
	if m := undefinedRelationRe.FindStringSubmatch(errStr); m != nil || (pgErr != nil && pgErr.Code == "42P01") {
		var rawRel string
		if m != nil {
			rawRel = m[2]
		} else if pgErr != nil {
			rawRel = pgErr.TableName
		}
		if relName := cleanIdentifier(rawRel); relName != "" {
			objDesc := fmt.Sprintf("relation %q", relName)
			stmtTarget := findStatementForIdentifier(sqlStr, relName, objDesc)
			target := formatFullTarget(stmtTarget, objDesc)
			return fmt.Errorf("%s: hint: verify table creation order or schema qualification: %w", target, err)
		}
	}

	// 7. Duplicate object
	if m := duplicateObjectRe.FindStringSubmatch(errStr); m != nil || (pgErr != nil && (pgErr.Code == "42P07" || pgErr.Code == "42710" || pgErr.Code == "42723" || pgErr.Code == "42P06")) {
		kind := "object"
		objName := ""
		if m != nil {
			kind = strings.ToLower(m[1])
			objName = cleanIdentifier(m[2])
		} else if pgErr != nil {
			if pgErr.TableName != "" {
				kind = "relation"
				objName = cleanIdentifier(pgErr.TableName)
			} else if pgErr.DataTypeName != "" {
				kind = "type"
				objName = cleanIdentifier(pgErr.DataTypeName)
			}
		}
		if objName != "" {
			objDesc := fmt.Sprintf("%s %q", kind, objName)
			return fmt.Errorf("%s: hint: duplicate definition: %w", objDesc, err)
		}
	}

	// Fallback: see if we can identify failing statement from SQL or error position
	if pgErr != nil && pgErr.Position > 0 {
		if stmtTarget := findStatementAtPosition(sqlStr, int(pgErr.Position)); stmtTarget != "" {
			return fmt.Errorf("%s: %w", stmtTarget, err)
		}
	} else if sqlStr != "" {
		stmts := schema.SplitStatements(sqlStr)
		if len(stmts) == 1 {
			if stmtTarget := parseStatementTarget(stmts[0]); stmtTarget != "" {
				return fmt.Errorf("%s: %w", stmtTarget, err)
			}
		}
	}

	return err
}

func formatFuncNames(raw string) (display, search string) {
	raw = strings.TrimSpace(raw)
	if idx := strings.Index(raw, "("); idx != -1 {
		namePart := strings.Trim(raw[:idx], `"'`)
		argsPart := raw[idx:]
		search = namePart
		display = namePart + argsPart
	} else {
		namePart := strings.Trim(raw, `"'`)
		search = namePart
		display = namePart + "()"
	}
	return display, search
}

func formatFullTarget(stmtTarget, objDesc string) string {
	if stmtTarget == "" || targetEquals(stmtTarget, objDesc) {
		return objDesc
	}
	return fmt.Sprintf("%s: %s", stmtTarget, objDesc)
}

func targetEquals(a, b string) bool {
	cleanA := extractBareName(a)
	cleanB := extractBareName(b)
	return cleanA != "" && cleanB != "" && strings.EqualFold(cleanA, cleanB)
}

func extractBareName(s string) string {
	if start := strings.Index(s, `"`); start != -1 {
		if end := strings.Index(s[start+1:], `"`); end != -1 {
			name := s[start+1 : start+1+end]
			if p := strings.Index(name, "("); p != -1 {
				name = name[:p]
			}
			return name
		}
	}
	fields := strings.Fields(s)
	if len(fields) > 0 {
		return fields[len(fields)-1]
	}
	return ""
}

func findStatementForIdentifier(sqlStr, ident, objDesc string) string {
	if sqlStr == "" || ident == "" {
		return ""
	}
	identLower := strings.ToLower(ident)
	re, err := regexp.Compile(`(?i)\b` + regexp.QuoteMeta(identLower) + `\b`)
	if err != nil {
		return ""
	}

	stmts := schema.SplitStatements(sqlStr)
	var fallback string
	for _, stmt := range stmts {
		stmtTrimmed := strings.TrimSpace(stmt)
		if !re.MatchString(strings.ToLower(stmtTrimmed)) {
			continue
		}
		target := parseStatementTarget(stmtTrimmed)
		if target == "" {
			continue
		}
		if targetEquals(target, objDesc) {
			if fallback == "" {
				fallback = target
			}
			continue
		}
		return target
	}
	return fallback
}

func stripLeadingComments(s string) string {
	for {
		s = strings.TrimSpace(s)
		switch {
		case strings.HasPrefix(s, "--"):
			if nl := strings.IndexByte(s, '\n'); nl >= 0 {
				s = s[nl+1:]
				continue
			}
			return ""
		case strings.HasPrefix(s, "/*"):
			if end := strings.Index(s[2:], "*/"); end >= 0 {
				s = s[end+4:]
				continue
			}
			return s
		default:
			return s
		}
	}
}

func parseStatementTarget(stmt string) string {
	stmt = stripLeadingComments(stmt)
	tokens := strings.Fields(stmt)
	if len(tokens) == 0 {
		return ""
	}

	upperTokens := make([]string, len(tokens))
	for i, t := range tokens {
		upperTokens[i] = strings.ToUpper(t)
	}

	for i := range tokens {
		kw := upperTokens[i]
		switch kw {
		case "CREATE":
			for j := i + 1; j < len(tokens); j++ {
				nextKw := upperTokens[j]
				if nextKw == "OR" && j+1 < len(tokens) && upperTokens[j+1] == "REPLACE" {
					j++
					continue
				}
				if nextKw == "TEMP" || nextKw == "TEMPORARY" || nextKw == "UNLOGGED" {
					continue
				}
				if nextKw == "TABLE" {
					tbl := extractObjectName(tokens, upperTokens, j+1)
					if tbl != "" {
						return fmt.Sprintf("table %q", cleanIdentifier(tbl))
					}
					return "table"
				}
				if nextKw == "INDEX" {
					idxName, tblName := parseIndexAndTable(stmt)
					if idxName != "" && tblName != "" {
						return fmt.Sprintf("index %q on table %q", cleanIdentifier(idxName), cleanIdentifier(tblName))
					} else if idxName != "" {
						return fmt.Sprintf("index %q", cleanIdentifier(idxName))
					}
					return "index"
				}
				if nextKw == "UNIQUE" && j+1 < len(tokens) && upperTokens[j+1] == "INDEX" {
					idxName, tblName := parseIndexAndTable(stmt)
					if idxName != "" && tblName != "" {
						return fmt.Sprintf("index %q on table %q", cleanIdentifier(idxName), cleanIdentifier(tblName))
					} else if idxName != "" {
						return fmt.Sprintf("index %q", cleanIdentifier(idxName))
					}
					return "index"
				}
				if nextKw == "MATERIALIZED" && j+1 < len(tokens) && upperTokens[j+1] == "VIEW" {
					v := extractObjectName(tokens, upperTokens, j+2)
					if v != "" {
						return fmt.Sprintf("materialized view %q", cleanIdentifier(v))
					}
					return "materialized view"
				}
				if nextKw == "VIEW" {
					v := extractObjectName(tokens, upperTokens, j+1)
					if v != "" {
						return fmt.Sprintf("view %q", cleanIdentifier(v))
					}
					return "view"
				}
				if nextKw == "TYPE" {
					tName := extractObjectName(tokens, upperTokens, j+1)
					if tName != "" {
						return fmt.Sprintf("type %q", cleanIdentifier(tName))
					}
					return "type"
				}
				if nextKw == "DOMAIN" {
					dName := extractObjectName(tokens, upperTokens, j+1)
					if dName != "" {
						return fmt.Sprintf("domain %q", cleanIdentifier(dName))
					}
					return "domain"
				}
				if nextKw == "FUNCTION" {
					fName := extractObjectName(tokens, upperTokens, j+1)
					if fName != "" {
						return fmt.Sprintf("function %q", cleanIdentifier(fName))
					}
					return "function"
				}
				if nextKw == "TRIGGER" {
					trg := extractObjectName(tokens, upperTokens, j+1)
					for k := j + 2; k < len(tokens)-1; k++ {
						if upperTokens[k] == "ON" {
							tbl := extractObjectName(tokens, upperTokens, k+1)
							if trg != "" && tbl != "" {
								return fmt.Sprintf("trigger %q on table %q", cleanIdentifier(trg), cleanIdentifier(tbl))
							}
						}
					}
					if trg != "" {
						return fmt.Sprintf("trigger %q", cleanIdentifier(trg))
					}
					return "trigger"
				}
				break
			}
		case "ALTER":
			if i+1 < len(tokens) {
				nextKw := upperTokens[i+1]
				if nextKw == "TABLE" {
					tbl := extractObjectName(tokens, upperTokens, i+2)
					if tbl != "" {
						return fmt.Sprintf("table %q", cleanIdentifier(tbl))
					}
					return "table"
				}
				if nextKw == "TYPE" {
					tName := extractObjectName(tokens, upperTokens, i+2)
					if tName != "" {
						return fmt.Sprintf("type %q", cleanIdentifier(tName))
					}
					return "type"
				}
				if nextKw == "VIEW" {
					v := extractObjectName(tokens, upperTokens, i+2)
					if v != "" {
						return fmt.Sprintf("view %q", cleanIdentifier(v))
					}
					return "view"
				}
			}
		}
	}
	return ""
}

func extractObjectName(tokens, upperTokens []string, start int) string {
	idx := start
	for idx < len(tokens) {
		kw := upperTokens[idx]
		if kw == "IF" && idx+2 < len(tokens) && upperTokens[idx+1] == "NOT" && upperTokens[idx+2] == "EXISTS" {
			idx += 3
			continue
		}
		if kw == "IF" && idx+1 < len(tokens) && upperTokens[idx+1] == "EXISTS" {
			idx += 2
			continue
		}
		if kw == "ONLY" {
			idx++
			continue
		}
		break
	}
	if idx < len(tokens) {
		raw := tokens[idx]
		if p := strings.IndexAny(raw, "(;,"); p != -1 {
			raw = raw[:p]
		}
		return raw
	}
	return ""
}

// cleanIdentifier strips quotes and removes the temporary shadow schema prefix
// (e.g. "_grizzle_shadow_public.users" -> "users") so that error messages report
// the desired schema object name rather than internal compilation sandboxes.
func cleanIdentifier(s string) string {
	s = strings.Trim(s, `"'`+"`;")
	if _, last, ok := strings.CutLast(s, "."); ok {
		s = last
	}
	s = strings.Trim(s, `"'`+"`;")
	if after, ok := strings.CutPrefix(s, "_grizzle_shadow_"); ok {
		s = after
	} else if s == "_grizzle_shadow" {
		s = ""
	}
	return s
}

func findStatementAtPosition(sqlStr string, pos int) string {
	if pos <= 0 || sqlStr == "" {
		return ""
	}
	idx := pos - 1
	stmts := schema.SplitStatements(sqlStr)
	curr := 0
	for _, stmt := range stmts {
		stmtPos := strings.Index(sqlStr[curr:], stmt)
		if stmtPos == -1 {
			continue
		}
		start := curr + stmtPos
		end := start + len(stmt)
		if idx >= start && idx <= end {
			return parseStatementTarget(stmt)
		}
		curr = end
	}
	return ""
}
