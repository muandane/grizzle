package postgres

import (
	"context"
	"fmt"
	"hash/fnv"
	"regexp"
	"strings"

	"github.com/muandane/grizzle/internal/dialect"
	"github.com/muandane/grizzle/internal/plan"
	"github.com/muandane/grizzle/internal/schema"
)

var validIdentRegex = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

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
	if err := ValidateIdentifier(targetSchema); err != nil {
		return err
	}

	setPathSQL := fmt.Sprintf("SET LOCAL search_path TO %q, %q, public;", shadowSchema, targetSchema)
	if _, err := dbtx.ExecContext(ctx, setPathSQL); err != nil {
		return fmt.Errorf("failed setting search_path to %q: %w", shadowSchema, err)
	}

	// Strip CREATE EXTENSION from the main DDL body, then best-effort install
	// into the shadow schema so extension-provided types (citext, etc.) resolve
	// during compile. Failures are ignored per statement (privilege / already
	// installed); the desired Extensions IR comes from statement parsing.
	cleanedSQL, extStmts := schema.StripExtensionStatements(schemaSQL)
	for _, extStmt := range extStmts {
		rewritten := rewriteExtensionSchema(extStmt, shadowSchema)
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
		if _, err := dbtx.ExecContext(ctx, ddlSQL); err != nil {
			if immErr := wrapImmutableIndexError(err, schemaSQL); immErr != err {
				return fmt.Errorf("schema compilation in shadow schema failed: %w", immErr)
			}
			if partErr := wrapPartitionShadowError(err); partErr != err {
				return fmt.Errorf("schema compilation in shadow schema failed: %w", partErr)
			}
			return fmt.Errorf("schema compilation in shadow schema failed: %w", err)
		}
	}

	restorePathSQL := fmt.Sprintf("SET LOCAL search_path TO %q, public;", targetSchema)
	if _, err := dbtx.ExecContext(ctx, restorePathSQL); err != nil {
		return fmt.Errorf("failed restoring search_path to %q: %w", targetSchema, err)
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
	setPathSQL := fmt.Sprintf("SET LOCAL search_path TO %s, %s, public;", strings.Join(shadowQuoted, ", "), strings.Join(targetQuoted, ", "))
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
			if immErr := wrapImmutableIndexError(err, schemaSQL); immErr != err {
				return fmt.Errorf("schema compilation in shadow schema failed: %w", immErr)
			}
			if partErr := wrapPartitionShadowError(err); partErr != err {
				return fmt.Errorf("schema compilation in shadow schema failed: %w", partErr)
			}
			return fmt.Errorf("schema compilation in shadow schema failed: %w", err)
		}
	}

	restorePathSQL := fmt.Sprintf("SET LOCAL search_path TO %s, public;", strings.Join(targetQuoted, ", "))
	if _, err := dbtx.ExecContext(ctx, restorePathSQL); err != nil {
		return fmt.Errorf("failed restoring target search_path: %w", err)
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
				if shadow, ok := shadowMap[ident]; ok {
					fmt.Fprintf(&b, "%q.", shadow)
					i++ // consume .
					prevWord = ident
					continue
				}
			}
			// Check if preceded by SCHEMA (e.g. CREATE SCHEMA "billing")
			if strings.EqualFold(prevWord, "SCHEMA") {
				if shadow, ok := shadowMap[ident]; ok {
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
				if shadow, ok := shadowMap[word]; ok {
					b.WriteString(shadow)
					b.WriteByte('.')
					i++ // consume .
					prevWord = word
					continue
				}
			}

			// Check if preceded by SCHEMA (e.g. CREATE SCHEMA billing or CREATE SCHEMA IF NOT EXISTS billing)
			if strings.EqualFold(prevWord, "SCHEMA") || strings.EqualFold(prevWord, "EXISTS") {
				if shadow, ok := shadowMap[word]; ok {
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

func wrapPartitionShadowError(err error) error {
	if err == nil {
		return nil
	}
	errStr := strings.ToLower(err.Error())
	if strings.Contains(errStr, "unique constraint on partitioned table must include all partitioning columns") ||
		strings.Contains(errStr, "primary key on partitioned table must include all partitioning columns") {
		return fmt.Errorf("%w: %w", plan.ErrPartitionKeyNotInUnique, err)
	}
	return err
}
