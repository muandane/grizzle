package schema

import (
	"regexp"
	"strings"
)

// createExtensionRe matches CREATE EXTENSION [IF NOT EXISTS] name [WITH SCHEMA schema].
// Captures: (1) extension name, (2) optional schema name.
var createExtensionRe = regexp.MustCompile(
	`(?is)^\s*CREATE\s+EXTENSION\s+(?:IF\s+NOT\s+EXISTS\s+)?(?:"([^"]+)"|([a-zA-Z_][\w$]*))(?:\s+WITH\s+SCHEMA\s+(?:"([^"]+)"|([a-zA-Z_][\w$]*)))?`)

// dmlStatementRe matches leading DML keywords for linting SchemaSQL.
var dmlStatementRe = regexp.MustCompile(`(?is)^\s*(INSERT|UPDATE|DELETE|TRUNCATE)\b`)

// SplitStatements splits SQL into top-level statements on unquoted semicolons,
// preserving string literals and comments.
func SplitStatements(sql string) []string {
	var stmts []string
	var b strings.Builder
	n := len(sql)
	i := 0
	for i < n {
		ch := sql[i]

		// Single-line comment
		if ch == '-' && i+1 < n && sql[i+1] == '-' {
			start := i
			for i < n && sql[i] != '\n' {
				i++
			}
			b.WriteString(sql[start:i])
			continue
		}

		// Multi-line comment
		if ch == '/' && i+1 < n && sql[i+1] == '*' {
			start := i
			i += 2
			for i+1 < n && (sql[i] != '*' || sql[i+1] != '/') {
				i++
			}
			if i+1 < n {
				i += 2
			}
			b.WriteString(sql[start:i])
			continue
		}

		// Dollar-quoted string ($$ or $tag$)
		if ch == '$' {
			tagEnd := i + 1
			for tagEnd < n && sql[tagEnd] != '$' &&
				((sql[tagEnd] >= 'a' && sql[tagEnd] <= 'z') ||
					(sql[tagEnd] >= 'A' && sql[tagEnd] <= 'Z') ||
					(sql[tagEnd] >= '0' && sql[tagEnd] <= '9') ||
					sql[tagEnd] == '_') {
				tagEnd++
			}
			if tagEnd < n && sql[tagEnd] == '$' {
				tag := sql[i : tagEnd+1]
				b.WriteString(tag)
				i = tagEnd + 1
				for i+len(tag) <= n {
					if sql[i:i+len(tag)] == tag {
						b.WriteString(tag)
						i += len(tag)
						break
					}
					b.WriteByte(sql[i])
					i++
				}
				continue
			}
		}

		// Single-quoted string
		if ch == '\'' {
			start := i
			i++
			for i < n {
				if sql[i] == '\'' {
					if i+1 < n && sql[i+1] == '\'' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			b.WriteString(sql[start:i])
			continue
		}

		// Double-quoted identifier
		if ch == '"' {
			start := i
			i++
			for i < n && sql[i] != '"' {
				i++
			}
			if i < n {
				i++
			}
			b.WriteString(sql[start:i])
			continue
		}

		if ch == ';' {
			stmt := strings.TrimSpace(b.String())
			if stmt != "" {
				stmts = append(stmts, stmt)
			}
			b.Reset()
			i++
			continue
		}

		b.WriteByte(ch)
		i++
	}
	if stmt := strings.TrimSpace(b.String()); stmt != "" {
		stmts = append(stmts, stmt)
	}
	return stmts
}

// ParseExtensions extracts CREATE EXTENSION statements from schema SQL.
// Keys are lowercased extension names. Schema is empty when WITH SCHEMA is omitted.
func ParseExtensions(sql string) map[string]*Extension {
	out := make(map[string]*Extension)
	for _, stmt := range SplitStatements(sql) {
		m := createExtensionRe.FindStringSubmatch(stmt)
		if m == nil {
			continue
		}
		name := m[1]
		if name == "" {
			name = m[2]
		}
		schemaName := m[3]
		if schemaName == "" {
			schemaName = m[4]
		}
		key := strings.ToLower(name)
		out[key] = &Extension{
			Name:   key,
			Schema: schemaName,
		}
	}
	return out
}

// StripExtensionStatements removes CREATE EXTENSION statements from SQL,
// returning the remaining DDL and the stripped extension statements
// (suitable for best-effort shadow re-execution).
func StripExtensionStatements(sql string) (cleaned string, extensions []string) {
	var kept []string
	for _, stmt := range SplitStatements(sql) {
		if createExtensionRe.MatchString(stmt) {
			extensions = append(extensions, stmt)
			continue
		}
		kept = append(kept, stmt)
	}
	if len(kept) == 0 {
		return "", extensions
	}
	return strings.Join(kept, ";\n") + ";", extensions
}

// FindDMLStatements returns statements that begin with INSERT/UPDATE/DELETE/TRUNCATE.
func FindDMLStatements(sql string) []string {
	var out []string
	for _, stmt := range SplitStatements(sql) {
		if dmlStatementRe.MatchString(stmt) {
			out = append(out, stmt)
		}
	}
	return out
}
