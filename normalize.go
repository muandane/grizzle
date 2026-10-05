package grizzle

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	// Regexes to extract single-quoted string literals with optional PostgreSQL casts and parens
	singleQuotedWithCastParenRegex = regexp.MustCompile(`^\(('(?:''|[^'])*')::[a-zA-Z0-9_\."\s]+(?:\[\])?\)$`)
	singleQuotedWithCastRegex      = regexp.MustCompile(`^('(?:''|[^'])*')::[a-zA-Z0-9_\."\s]+(?:\[\])?$`)
	singleQuotedParenRegex         = regexp.MustCompile(`^\(('(?:''|[^'])*')\)$`)

	// Regexes for boolean and numeric literals with optional parens and casts
	boolParenRegex = regexp.MustCompile(`^\(?(true|false)(?:::bool(?:ean)?)?\)?$`)
	numParenRegex  = regexp.MustCompile(`^\(?(-?\d+(?:\.\d+)?)(?:::[\w\."\s]+)?\)?$`)
)

// stripOuterParens strips surrounding balanced parentheses while respecting quotes.
func stripOuterParens(s string) string {
	s = strings.TrimSpace(s)
	for strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
		count := 0
		balanced := true
		inSingle, inDouble, escaped := false, false, false

		for i := 0; i < len(s)-1; i++ {
			c := s[i]
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '\'' && !inDouble {
				inSingle = !inSingle
				continue
			}
			if c == '"' && !inSingle {
				inDouble = !inDouble
				continue
			}
			if inSingle || inDouble {
				continue
			}
			if c == '(' {
				count++
			} else if c == ')' {
				count--
				if count == 0 {
					balanced = false
					break
				}
			}
		}

		if balanced && count == 1 && !inSingle && !inDouble {
			s = strings.TrimSpace(s[1 : len(s)-1])
		} else {
			break
		}
	}
	return s
}

// normalizeType standardizes PostgreSQL data type representations to avoid false-positive diff loops.
func normalizeType(raw string) string {
	t := strings.TrimSpace(strings.ToLower(raw))

	// Normalize character varying to varchar
	if after, ok := strings.CutPrefix(t, "character varying"); ok {
		return "varchar" + after
	}
	if after, ok := strings.CutPrefix(t, "character("); ok {
		return "char(" + after
	}

	// Normalize integers and serials
	switch t {
	case "int", "int4", "serial":
		return "integer"
	case "int8", "bigserial":
		return "bigint"
	case "int2", "smallserial":
		return "smallint"
	case "bool":
		return "boolean"
	case "float8":
		return "double precision"
	case "float4":
		return "real"
	case "timestamp with time zone":
		return "timestamptz"
	case "timestamp without time zone":
		return "timestamp"
	case "time with time zone":
		return "timetz"
	case "time without time zone":
		return "time"
	case "decimal":
		return "numeric"
	default:
		return t
	}
}

// normalizeDefault sanitizes default values returned by pg_catalog.
// For example, "'draft'::character varying" becomes "'draft'", and "('draft'::text)" becomes "'draft'".
// Compound expressions like ('2024-01-01'::date + '1 day'::interval) are preserved verbatim.
func normalizeDefault(raw string) string {
	d := strings.TrimSpace(raw)
	if d == "" {
		return ""
	}

	// 1. Single quoted string literals with optional cast/parens
	if m := singleQuotedWithCastParenRegex.FindStringSubmatch(d); len(m) == 2 {
		return m[1]
	}
	if m := singleQuotedWithCastRegex.FindStringSubmatch(d); len(m) == 2 {
		return m[1]
	}
	if m := singleQuotedParenRegex.FindStringSubmatch(d); len(m) == 2 {
		return m[1]
	}

	// 2. Booleans
	if m := boolParenRegex.FindStringSubmatch(strings.ToLower(d)); len(m) == 2 {
		return m[1]
	}

	// 3. Numbers
	if m := numParenRegex.FindStringSubmatch(d); len(m) == 2 {
		return m[1]
	}

	// 4. Unwrap outer parens for function calls and keyword checks
	unwrapped := stripOuterParens(d)

	// Normalize CURRENT_TIMESTAMP to now()
	if strings.EqualFold(unwrapped, "CURRENT_TIMESTAMP") || strings.EqualFold(unwrapped, "now()") {
		return "now()"
	}

	// Normalize nextval('schema.seq_name'::regclass) -> nextval('seq_name'::regclass)
	if after, ok := strings.CutPrefix(unwrapped, "nextval('"); ok {
		if seqName, _, ok := strings.Cut(after, "'"); ok {
			if _, seq, found := strings.CutLast(seqName, "."); found {
				seqName = seq
			}
			return fmt.Sprintf("nextval('%s'::regclass)", seqName)
		}
	}

	return d
}

// normalizeDefinition standardizes index and constraint definitions between live and shadow schemas.
// It restricts ON <schema>. stripping to the index header to avoid mutating string literals in WHERE predicates.
func normalizeDefinition(def, shadowSchema, targetSchema string) string {
	res := def

	// Split index header from WHERE predicate if present, preserving predicate literals
	head := res
	pred := ""
	hasPred := false

	if idx := strings.Index(strings.ToUpper(res), " WHERE "); idx != -1 {
		head = res[:idx]
		pred = res[idx:] // includes " WHERE ..."
		hasPred = true
	}

	schemas := []string{shadowSchema, targetSchema}
	for _, s := range schemas {
		if s == "" {
			continue
		}
		// Strip schema from "ON <schema>." in the header only!
		head = strings.ReplaceAll(head, " ON "+s+".", " ON ")
		head = strings.ReplaceAll(head, " ON \""+s+"\".", " ON ")
		head = strings.ReplaceAll(head, " on "+s+".", " on ")
		head = strings.ReplaceAll(head, " on \""+s+"\".", " on ")

		// Strip schema from "REFERENCES <schema>."
		head = strings.ReplaceAll(head, "REFERENCES "+s+".", "REFERENCES ")
		head = strings.ReplaceAll(head, "REFERENCES \""+s+"\".", "REFERENCES ")
		head = strings.ReplaceAll(head, "references "+s+".", "references ")
		head = strings.ReplaceAll(head, "references \""+s+"\".", "references ")

		// Normalize type casts "::<schema>." in predicate and header
		if hasPred {
			pred = strings.ReplaceAll(pred, "::"+s+".", "::")
			pred = strings.ReplaceAll(pred, "::\""+s+"\".", "::")
		} else {
			head = strings.ReplaceAll(head, "::"+s+".", "::")
			head = strings.ReplaceAll(head, "::\""+s+"\".", "::")
		}
	}

	if hasPred {
		res = head + pred
	} else {
		res = head
	}

	// Also replace any remaining occurrences of shadowSchema with targetSchema in non-stripped positions
	if shadowSchema != "" && targetSchema != "" {
		res = strings.ReplaceAll(res, shadowSchema+".", targetSchema+".")
		res = strings.ReplaceAll(res, `"`+shadowSchema+`".`, `"`+targetSchema+`".`)
	}

	return strings.TrimSpace(res)
}
