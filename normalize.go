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

	return strings.TrimSpace(d)
}

// normalizeDefinition replaces references to shadowSchema with targetSchema in DDL expressions
// and removes redundant schema qualifications from REFERENCES and type casts.
func normalizeDefinition(def, shadowSchema, targetSchema string) string {
	res := def
	if shadowSchema != "" {
		res = strings.ReplaceAll(res, shadowSchema+".", targetSchema+".")
		res = strings.ReplaceAll(res, `"`+shadowSchema+`".`, `"`+targetSchema+`".`)
	}
	if targetSchema != "" {
		// Strip schema prefix from REFERENCES <schema>.tbl -> REFERENCES tbl
		res = strings.ReplaceAll(res, "REFERENCES "+targetSchema+".", "REFERENCES ")
		res = strings.ReplaceAll(res, "REFERENCES \""+targetSchema+"\".", "REFERENCES ")
		res = strings.ReplaceAll(res, "references "+targetSchema+".", "references ")
		res = strings.ReplaceAll(res, "references \""+targetSchema+"\".", "references ")

		// Strip schema prefix from type casts ::<schema>.type -> ::type
		res = strings.ReplaceAll(res, "::"+targetSchema+".", "::")
		res = strings.ReplaceAll(res, "::\""+targetSchema+"\".", "::")
	}
	return strings.TrimSpace(res)
}


