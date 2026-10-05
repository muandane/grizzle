package grizzle

import (
	"fmt"
	"regexp"
	"strings"
)

var castRegex = regexp.MustCompile(`::[a-zA-Z0-9_\."\s]+(\[\])?$`)

// stripOuterParens strips surrounding balanced parentheses e.g. "('draft'::text)" -> "'draft'::text"
func stripOuterParens(s string) string {
	s = strings.TrimSpace(s)
	for strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
		count := 0
		balanced := true
		for i := 0; i < len(s)-1; i++ {
			if s[i] == '(' {
				count++
			} else if s[i] == ')' {
				count--
				if count == 0 {
					balanced = false
					break
				}
			}
		}
		if balanced && count == 1 {
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
func normalizeDefault(raw string) string {
	d := stripOuterParens(raw)
	if d == "" {
		return ""
	}

	// Normalize CURRENT_TIMESTAMP to now()
	if strings.EqualFold(d, "CURRENT_TIMESTAMP") || strings.EqualFold(d, "now()") {
		return "now()"
	}

	// Normalize nextval('schema.seq_name'::regclass) -> nextval('seq_name'::regclass)
	if after, ok := strings.CutPrefix(d, "nextval('"); ok {
		if seqName, _, ok := strings.Cut(after, "'"); ok {
			if _, seq, found := strings.CutLast(seqName, "."); found {
				seqName = seq
			}
			return fmt.Sprintf("nextval('%s'::regclass)", seqName)
		}
	}

	// Strip explicit Postgres type casts like 'foo'::character varying
	d = castRegex.ReplaceAllString(d, "")

	// Strip remaining balanced parens if cast stripping uncovered them (e.g. ('draft'))
	d = stripOuterParens(d)

	return strings.TrimSpace(d)
}

// normalizeDefinition standardizes index and constraint definitions between live and shadow schemas.
func normalizeDefinition(def, shadowSchema, targetSchema string) string {
	res := def
	schemas := []string{shadowSchema, targetSchema}
	for _, s := range schemas {
		if s == "" {
			continue
		}
		// Strip schema from "ON <schema>."
		res = strings.ReplaceAll(res, " ON "+s+".", " ON ")
		res = strings.ReplaceAll(res, " ON \""+s+"\".", " ON ")
		res = strings.ReplaceAll(res, " on "+s+".", " on ")
		res = strings.ReplaceAll(res, " on \""+s+"\".", " on ")

		// Strip schema from "REFERENCES <schema>."
		res = strings.ReplaceAll(res, "REFERENCES "+s+".", "REFERENCES ")
		res = strings.ReplaceAll(res, "REFERENCES \""+s+"\".", "REFERENCES ")
		res = strings.ReplaceAll(res, "references "+s+".", "references ")
		res = strings.ReplaceAll(res, "references \""+s+"\".", "references ")

		// Strip schema from type casts "::<schema>."
		res = strings.ReplaceAll(res, "::"+s+".", "::")
		res = strings.ReplaceAll(res, "::\""+s+"\".", "::")
	}

	// Also replace any remaining occurrences of shadowSchema with targetSchema
	if shadowSchema != "" && targetSchema != "" {
		res = strings.ReplaceAll(res, shadowSchema+".", targetSchema+".")
		res = strings.ReplaceAll(res, `"`+shadowSchema+`".`, `"`+targetSchema+`".`)
	}

	return strings.TrimSpace(res)
}
