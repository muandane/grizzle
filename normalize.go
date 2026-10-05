package grizzle

import (
	"fmt"
	"regexp"
	"strings"
)

var castRegex = regexp.MustCompile(`::[a-zA-Z0-9_\."\s]+(\[\])?$`)

// normalizeType standardizes PostgreSQL data type representations to avoid false-positive diff loops.
func normalizeType(raw string) string {
	t := strings.TrimSpace(strings.ToLower(raw))

	// Normalize character varying to varchar
	if strings.HasPrefix(t, "character varying") {
		return strings.Replace(t, "character varying", "varchar", 1)
	}
	if strings.HasPrefix(t, "char(") || strings.HasPrefix(t, "character(") {
		return strings.Replace(t, "character(", "char(", 1)
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
// For example, "'draft'::character varying" becomes "'draft'", and "CURRENT_TIMESTAMP" becomes "now()".
func normalizeDefault(raw string) string {
	d := strings.TrimSpace(raw)
	if d == "" {
		return ""
	}

	// Normalize CURRENT_TIMESTAMP to now()
	if strings.EqualFold(d, "CURRENT_TIMESTAMP") || strings.EqualFold(d, "now()") {
		return "now()"
	}

	// Normalize nextval('schema.seq_name'::regclass) -> nextval('seq_name'::regclass)
	if strings.HasPrefix(d, "nextval('") {
		start := len("nextval('")
		if end := strings.Index(d[start:], "'"); end != -1 {
			seqName := d[start : start+end]
			if dot := strings.LastIndex(seqName, "."); dot != -1 {
				seqName = seqName[dot+1:]
			}
			return fmt.Sprintf("nextval('%s'::regclass)", seqName)
		}
	}

	// Strip explicit Postgres type casts like 'foo'::character varying
	d = castRegex.ReplaceAllString(d, "")

	return strings.TrimSpace(d)
}
