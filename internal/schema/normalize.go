package schema

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

var (
	// Regexes to extract single-quoted string literals with optional PostgreSQL casts and parens
	singleQuotedWithCastParenRegex = regexp.MustCompile(`^\(('(?:''|[^'])*')::[a-zA-Z0-9_\."\s]+(?:\[\])?\)$`)
	singleQuotedWithCastRegex      = regexp.MustCompile(`^('(?:''|[^'])*')::[a-zA-Z0-9_\."\s]+(?:\[\])?$`)
	singleQuotedParenRegex         = regexp.MustCompile(`^\(('(?:''|[^'])*')\)$`)

	// Regexes for boolean and numeric literals with optional parens and casts
	boolParenRegex = regexp.MustCompile(`^\(?(true|false)(?:::bool(?:ean)?)?\)?$`)
	numParenRegex  = regexp.MustCompile(`^\(?(-?\d+(?:\.\d+)?)(?:::[\w\."\s]+)?\)?$`)

	// Redundant cast on parenthesized/simple numbers: (0)::numeric or 0::numeric or (-1)::integer
	numCastParenRegex = regexp.MustCompile(`\(?(-?\d+(?:\.\d+)?)\)?::(?:numeric|decimal|integer|int[248]?|bigint|smallint|float[48]?|double\s+precision)(?:\s*\(\s*\d+(?:\s*,\s*\d+)?\s*\))?(?:\[\])?`)

	// Redundant cast on identifiers in parens: (email)::text or (col)::character varying
	colCastParenRegex = regexp.MustCompile(`\(([a-zA-Z_][a-zA-Z0-9_]*)\)::(?:text|varchar|character\s+varying|char)(?:\s*\(\s*\d+\s*\))?(?:\[\])?`)

	// Boolean syntax variations: IS TRUE -> = true, IS FALSE -> = false
	isTrueRegex     = regexp.MustCompile(`(?i)\bIS\s+TRUE\b`)
	isFalseRegex    = regexp.MustCompile(`(?i)\bIS\s+FALSE\b`)
	isNotTrueRegex  = regexp.MustCompile(`(?i)\bIS\s+NOT\s+TRUE\b`)
	isNotFalseRegex = regexp.MustCompile(`(?i)\bIS\s+NOT\s+FALSE\b`)

	// Default index method in Postgres
	usingBtreeRegex = regexp.MustCompile(`(?i)\s+USING\s+btree\s+`)
)

// NormalizeGeneratedExpr sanitizes generated column expressions by stripping outer parentheses,
// redundant casts, and excess whitespace.
func NormalizeGeneratedExpr(expr string) string {
	return NormalizeExpression(expr, nil)
}

// NormalizeGeneratedExprWithMappings sanitizes generated column expressions across schema mappings.
func NormalizeGeneratedExprWithMappings(expr string, mappings map[string]string, currentSchema ...string) string {
	return NormalizeExpression(expr, mappings, currentSchema...)
}

// NormalizeCheckDef standardizes CHECK constraint definitions between live and shadow schemas.
func NormalizeCheckDef(def string, mappings map[string]string, currentSchema ...string) string {
	d := strings.TrimSpace(def)
	if d == "" {
		return ""
	}
	hasNotValid := false
	if strings.HasSuffix(strings.ToUpper(d), " NOT VALID") {
		hasNotValid = true
		d = strings.TrimSpace(d[:len(d)-len(" NOT VALID")])
	}
	if strings.HasPrefix(strings.ToUpper(d), "CHECK") {
		d = strings.TrimSpace(d[5:])
	}
	normalized := NormalizeExpression(d, mappings, currentSchema...)
	res := "CHECK (" + normalized + ")"
	if hasNotValid {
		res += " NOT VALID"
	}
	return res
}

// NormalizePolicyExpr standardizes RLS policy USING and WITH CHECK expressions.
func NormalizePolicyExpr(expr string, mappings map[string]string, currentSchema ...string) string {
	return NormalizeExpression(expr, mappings, currentSchema...)
}

// StripOuterParens strips surrounding balanced parentheses while respecting quotes.
func StripOuterParens(s string) string {
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

// NormalizeExpression standardizes SQL expressions across catalog decompile variations,
// stripping redundant outer/nested parentheses, redundant casts on literals and columns,
// standardizing boolean comparisons, and resolving schema qualifications.
func NormalizeExpression(expr string, mappings map[string]string, currentSchema ...string) string {
	s := strings.TrimSpace(expr)
	if s == "" {
		return ""
	}

	curr := ""
	if len(currentSchema) > 0 {
		curr = currentSchema[0]
	}

	// Strip current and mapped schemas outside string literals
	stripSchemas := make([]string, 0, len(mappings)*2+2)
	if curr != "" {
		stripSchemas = append(stripSchemas, curr)
	}
	for shadow, target := range mappings {
		if target != "" && !slices.Contains(stripSchemas, target) {
			stripSchemas = append(stripSchemas, target)
		}
		if shadow != "" && !slices.Contains(stripSchemas, shadow) {
			stripSchemas = append(stripSchemas, shadow)
		}
	}

	// 1. Strip outer parens
	s = StripOuterParens(s)

	// 2. Tokenize by string literals so transformations only apply outside quotes
	var b strings.Builder
	inSingle, inDouble, escaped := false, false, false
	start := 0

	applyTransforms := func(text string) string {
		if text == "" {
			return ""
		}
		leadingSpace := unicode.IsSpace(rune(text[0]))
		trailingSpace := unicode.IsSpace(rune(text[len(text)-1]))

		// Unmap shadow schemas outside string literals
		for shadow, target := range mappings {
			if shadow == "" || target == "" {
				continue
			}
			text = strings.ReplaceAll(text, shadow+".", target+".")
			text = strings.ReplaceAll(text, `"`+shadow+`".`, `"`+target+`".`)
		}

		// Strip current schema prefix on routines / types outside string literals
		for _, sc := range stripSchemas {
			if sc == "" {
				continue
			}
			text = strings.ReplaceAll(text, "::"+sc+".", "::")
			text = strings.ReplaceAll(text, "::\""+sc+"\".", "::")
			text = strings.ReplaceAll(text, "("+sc+".", "(")
			text = strings.ReplaceAll(text, "(\""+sc+"\".", "(")
			text = strings.ReplaceAll(text, ","+sc+".", ",")
			text = strings.ReplaceAll(text, ",\""+sc+"\".", ",")
			text = strings.ReplaceAll(text, " "+sc+".", " ")
			text = strings.ReplaceAll(text, " \""+sc+"\".", " ")
			if after, ok := strings.CutPrefix(text, sc+"."); ok {
				text = after
			}
			if after, ok := strings.CutPrefix(text, `"`+sc+`".`); ok {
				text = after
			}
		}

		t := text
		if strings.Contains(text, "IS ") || strings.Contains(text, "is ") || strings.Contains(text, "Is ") || strings.Contains(text, "iS ") {
			t = isNotTrueRegex.ReplaceAllString(t, "!= true")
			t = isNotFalseRegex.ReplaceAllString(t, "!= false")
			t = isTrueRegex.ReplaceAllString(t, "= true")
			t = isFalseRegex.ReplaceAllString(t, "= false")
		}

		if strings.Contains(text, "::") {
			t = numCastParenRegex.ReplaceAllString(t, "$1")
			t = colCastParenRegex.ReplaceAllString(t, "$1")
		}

		words := strings.Fields(t)
		collapsed := strings.Join(words, " ")

		collapsed = strings.ReplaceAll(collapsed, "( ", "(")
		collapsed = strings.ReplaceAll(collapsed, " )", ")")
		collapsed = strings.ReplaceAll(collapsed, " ,", ",")

		res := collapsed
		if leadingSpace && !strings.HasPrefix(res, " ") {
			res = " " + res
		}
		if trailingSpace && !strings.HasSuffix(res, " ") {
			res = res + " "
		}
		return res
	}

	for i := 0; i < len(s); i++ {
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
			if !inSingle {
				b.WriteString(applyTransforms(s[start:i]))
				start = i
				inSingle = true
			} else {
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				inSingle = false
				quoted := s[start : i+1]
				start = i + 1

				remaining := s[start:]
				if after, ok := strings.CutPrefix(remaining, "::"); ok {
					idx := 0
					for idx < len(after) && (after[idx] == '_' || after[idx] == '.' || after[idx] == '"' ||
						(after[idx] >= 'a' && after[idx] <= 'z') ||
						(after[idx] >= 'A' && after[idx] <= 'Z') ||
						(after[idx] >= '0' && after[idx] <= '9') ||
						after[idx] == ' ' || after[idx] == '[' || after[idx] == ']' ||
						after[idx] == '(' || after[idx] == ')' || after[idx] == ',') {
						idx++
					}
					castType := strings.TrimSpace(after[:idx])
					castLower := strings.ToLower(castType)
					if castLower == "text" || castLower == "character varying" || strings.HasPrefix(castLower, "varchar") || strings.HasPrefix(castLower, "char") {
						start += len("::") + len(after[:idx])
					}
				}
				b.WriteString(quoted)
			}
			continue
		}
		if c == '"' && !inSingle {
			inDouble = !inDouble
			continue
		}
	}
	if start < len(s) {
		b.WriteString(applyTransforms(s[start:]))
	}

	res := strings.TrimSpace(b.String())
	return StripOuterParens(res)
}

// NormalizeType standardizes PostgreSQL data type representations to avoid false-positive diff loops.
func NormalizeType(raw string) string {
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

// NormalizeDefault sanitizes default values returned by pg_catalog.
// For example, "'draft'::character varying" becomes "'draft'", and "('draft'::text)" becomes "'draft'".
// Compound expressions like ('2024-01-01'::date + '1 day'::interval) are preserved verbatim.
func NormalizeDefault(raw string) string {
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
	unwrapped := StripOuterParens(d)

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

// NormalizeDefinition standardizes index and constraint definitions between live and shadow schemas.
// NormalizeDefinition standardizes index and constraint definitions between live and shadow schemas.
// It restricts ON <schema>. stripping to the index header to avoid mutating string literals in WHERE predicates.
func NormalizeDefinition(def, shadowSchema, targetSchema string) string {
	mapping := make(map[string]string)
	if shadowSchema != "" {
		mapping[shadowSchema] = targetSchema
	}
	return NormalizeDefinitionWithMappings(def, mapping, targetSchema)
}

// NormalizeDefinitionWithMappings standardizes index and constraint definitions across multiple shadow and target schema pairs.
// It maps shadow schemas to their live target schema equivalents and strips current schema prefixes where applicable.
func NormalizeDefinitionWithMappings(def string, mappings map[string]string, currentSchema ...string) string {
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

	curr := ""
	if len(currentSchema) > 0 {
		curr = currentSchema[0]
	}

	// 1. Replace all shadow schema occurrences with their target schemas
	for shadow, target := range mappings {
		if shadow == "" || target == "" {
			continue
		}
		head = strings.ReplaceAll(head, shadow+".", target+".")
		head = strings.ReplaceAll(head, `"`+shadow+`".`, `"`+target+`".`)
		if hasPred {
			pred = strings.ReplaceAll(pred, shadow+".", target+".")
			pred = strings.ReplaceAll(pred, `"`+shadow+`".`, `"`+target+`".`)
		}
	}

	// 2. Strip schema from ON/REFERENCES and typecasts
	// For REFERENCES: only strip if referencing currentSchema (same schema), or if currentSchema not specified
	stripSchemas := make([]string, 0, len(mappings)*2+2)
	if curr != "" {
		stripSchemas = append(stripSchemas, curr)
	}
	for shadow, target := range mappings {
		if target != "" && !slices.Contains(stripSchemas, target) {
			stripSchemas = append(stripSchemas, target)
		}
		if shadow != "" && !slices.Contains(stripSchemas, shadow) {
			stripSchemas = append(stripSchemas, shadow)
		}
	}

	for _, s := range stripSchemas {
		if s == "" {
			continue
		}
		// Strip schema from "ON <schema>." in the header only!
		head = strings.ReplaceAll(head, " ON "+s+".", " ON ")
		head = strings.ReplaceAll(head, " ON \""+s+"\".", " ON ")
		head = strings.ReplaceAll(head, " on "+s+".", " on ")
		head = strings.ReplaceAll(head, " on \""+s+"\".", " on ")

		// Strip schema from "REFERENCES <schema>." ONLY IF s == curr or curr == ""
		if curr == "" || s == curr {
			head = strings.ReplaceAll(head, "REFERENCES "+s+".", "REFERENCES ")
			head = strings.ReplaceAll(head, "REFERENCES \""+s+"\".", "REFERENCES ")
			head = strings.ReplaceAll(head, "references "+s+".", "references ")
			head = strings.ReplaceAll(head, "references \""+s+"\".", "references ")
		}

		// Strip schema from functional expressions in head: "(<schema>." -> "(", ",<schema>." -> ","
		head = strings.ReplaceAll(head, "("+s+".", "(")
		head = strings.ReplaceAll(head, "(\""+s+"\".", "(")
		head = strings.ReplaceAll(head, ","+s+".", ",")
		head = strings.ReplaceAll(head, ",\""+s+"\".", ",")

		// Strip schema from routine references. pg_get_functiondef and
		// pg_get_triggerdef qualify routine names depending on the session
		// search_path, so live and shadow inspections can disagree. Only the
		// first occurrence is rewritten: the canonical pg_get_*def header
		// (CREATE ... FUNCTION / EXECUTE FUNCTION), never string literals in
		// routine bodies.
		head = strings.Replace(head, " FUNCTION "+s+".", " FUNCTION ", 1)
		head = strings.Replace(head, ` FUNCTION "`+s+`".`, ` FUNCTION "`, 1)
		head = strings.Replace(head, " PROCEDURE "+s+".", " PROCEDURE ", 1)
		head = strings.Replace(head, ` PROCEDURE "`+s+`".`, ` PROCEDURE "`, 1)

		// Normalize type casts "::<schema>." in predicate and header
		if hasPred {
			pred = strings.ReplaceAll(pred, "::"+s+".", "::")
			pred = strings.ReplaceAll(pred, "::\""+s+"\".", "::")
			pred = strings.ReplaceAll(pred, "("+s+".", "(")
			pred = strings.ReplaceAll(pred, "(\""+s+"\".", "(")
			pred = strings.ReplaceAll(pred, ","+s+".", ",")
			pred = strings.ReplaceAll(pred, ",\""+s+"\".", ",")
		} else {
			head = strings.ReplaceAll(head, "::"+s+".", "::")
			head = strings.ReplaceAll(head, "::\""+s+"\".", "::")
		}
	}

	upperDef := strings.ToUpper(strings.TrimSpace(def))
	isIndex := strings.HasPrefix(upperDef, "CREATE ") && (strings.Contains(upperDef, " INDEX ") || strings.Contains(upperDef, " UNIQUE INDEX "))

	if isIndex {
		// In index head: strip default "USING btree " and redundant column casts
		head = usingBtreeRegex.ReplaceAllString(head, " ")
		head = colCastParenRegex.ReplaceAllString(head, "$1")
	}

	if hasPred {
		if isIndex {
			predCond := strings.TrimSpace(strings.TrimPrefix(pred, " WHERE "))
			normCond := NormalizeExpression(predCond, mappings, currentSchema...)
			res = strings.TrimSpace(head) + " WHERE (" + normCond + ")"
		} else {
			res = head + pred
		}
	} else {
		res = head
	}

	return strings.TrimSpace(res)
}

// IsTypeNarrowing determines whether converting from oldType to newType narrows the type or risks data loss.
func IsTypeNarrowing(oldType, newType string) bool {
	oldT := NormalizeType(oldType)
	newT := NormalizeType(newType)
	if oldT == newT {
		return false
	}

	// 1. Integer rank
	intRank := map[string]int{
		"smallint": 1,
		"integer":  2,
		"bigint":   3,
	}
	oldInt, isOldInt := intRank[oldT]
	newInt, isNewInt := intRank[newT]
	if isOldInt && isNewInt {
		return newInt < oldInt
	}

	// 2. Float rank
	floatRank := map[string]int{
		"real":             1,
		"double precision": 2,
	}
	oldFloat, isOldFloat := floatRank[oldT]
	newFloat, isNewFloat := floatRank[newT]
	if isOldFloat && isNewFloat {
		return newFloat < oldFloat
	}

	// Integer -> Float/Double is widening (safe)
	if isOldInt && isNewFloat {
		return false
	}
	// Float -> Integer is narrowing
	if isOldFloat && isNewInt {
		return true
	}

	// 3. String types (varchar, char, text)
	parseStr := func(t string) (length int, isStr bool) {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "text" {
			return math.MaxInt, true
		}
		if t == "varchar" {
			return math.MaxInt, true
		}
		if after, ok := strings.CutPrefix(t, "varchar("); ok {
			if numStr, _, ok := strings.Cut(after, ")"); ok {
				if n, err := strconv.Atoi(strings.TrimSpace(numStr)); err == nil {
					return n, true
				}
			}
		}
		if after, ok := strings.CutPrefix(t, "char("); ok {
			if numStr, _, ok := strings.Cut(after, ")"); ok {
				if n, err := strconv.Atoi(strings.TrimSpace(numStr)); err == nil {
					return n, true
				}
			}
		}
		return 0, false
	}

	oldLen, isOldStr := parseStr(oldT)
	newLen, isNewStr := parseStr(newT)
	if isOldStr && isNewStr {
		return newLen < oldLen
	}

	// Any scalar -> string/text is widening (safe)
	if (isOldInt || isOldFloat) && isNewStr {
		return false
	}

	// 4. Numeric / Decimal
	parseNumeric := func(t string) (prec, scale int, isNum bool) {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "numeric" {
			return math.MaxInt, math.MaxInt, true
		}
		if after, ok := strings.CutPrefix(t, "numeric("); ok {
			if args, _, ok := strings.Cut(after, ")"); ok {
				parts := strings.Split(args, ",")
				if len(parts) == 1 {
					if p, err := strconv.Atoi(strings.TrimSpace(parts[0])); err == nil {
						return p, 0, true
					}
				} else if len(parts) == 2 {
					p, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
					s, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
					if err1 == nil && err2 == nil {
						return p, s, true
					}
				}
			}
		}
		return 0, 0, false
	}

	oldP, oldS, isOldNum := parseNumeric(oldT)
	newP, newS, isNewNum := parseNumeric(newT)
	if isOldNum && isNewNum {
		return newP < oldP || newS < oldS
	}

	// Integer -> Numeric is widening (safe)
	if isOldInt && isNewNum {
		return false
	}

	// Default fallback: any other non-identical type transition carries risk/narrowing
	return true
}

// NormalizeIdentityArgs standardizes a routine identity arguments string (e.g. "a int, b text").
func NormalizeIdentityArgs(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, ",")
	normParts := make([]string, len(parts))
	for i, part := range parts {
		part = strings.TrimSpace(part)
		words := strings.Fields(part)
		if len(words) == 0 {
			normParts[i] = ""
			continue
		}
		if len(words) == 1 {
			normParts[i] = NormalizeType(words[0])
		} else {
			typeName := strings.Join(words[1:], " ")
			normParts[i] = words[0] + " " + NormalizeType(typeName)
		}
	}
	return strings.Join(normParts, ", ")
}
