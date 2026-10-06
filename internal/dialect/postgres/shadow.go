package postgres

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/muandane/grizzle/internal/dialect"
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

	setPathSQL := fmt.Sprintf("SET LOCAL search_path TO %q, public;", shadowSchema)
	if _, err := dbtx.ExecContext(ctx, setPathSQL); err != nil {
		return fmt.Errorf("failed setting search_path to %q: %w", shadowSchema, err)
	}

	if _, err := dbtx.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("schema compilation in shadow schema failed: %w", err)
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
// Single schema "public" defaults to "_grizzle_shadow" for backward compatibility.
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
	for _, s := range targetSchemas {
		m[s] = prefix + "_" + s
	}
	return m
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

	setPathSQL := fmt.Sprintf("SET LOCAL search_path TO %s, public;", strings.Join(shadowQuoted, ", "))
	if _, err := dbtx.ExecContext(ctx, setPathSQL); err != nil {
		return fmt.Errorf("failed setting multi-schema shadow search_path: %w", err)
	}

	rewrittenSQL := RewriteShadowSQL(schemaSQL, shadowMap)
	if _, err := dbtx.ExecContext(ctx, rewrittenSQL); err != nil {
		return fmt.Errorf("schema compilation in shadow schema failed: %w", err)
	}

	var targetQuoted []string
	for _, t := range targetSchemas {
		if err := ValidateIdentifier(t); err != nil {
			return err
		}
		targetQuoted = append(targetQuoted, fmt.Sprintf("%q", t))
	}
	restorePathSQL := fmt.Sprintf("SET LOCAL search_path TO %s, public;", strings.Join(targetQuoted, ", "))
	if _, err := dbtx.ExecContext(ctx, restorePathSQL); err != nil {
		return fmt.Errorf("failed restoring target search_path: %w", err)
	}

	return nil
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
			for i+1 < n && !(sqlStr[i] == '*' && sqlStr[i+1] == '/') {
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
