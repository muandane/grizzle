package schema

import (
	"fmt"
	"maps"
	"strconv"
	"strings"
	"unicode"
)

// CloneRole deep-copies a Role including Config.
func CloneRole(r *Role) *Role {
	return cloneRole(r)
}

// cloneRole deep-copies a Role including Config.
func cloneRole(r *Role) *Role {
	if r == nil {
		return nil
	}
	out := &Role{
		Name:        r.Name,
		Password:    r.Password,
		HasPassword: r.HasPassword,
		ValidUntil:  r.ValidUntil,
	}
	if r.Login != nil {
		v := *r.Login
		out.Login = &v
	}
	if r.ConnectionLimit != nil {
		v := *r.ConnectionLimit
		out.ConnectionLimit = &v
	}
	if r.Inherit != nil {
		v := *r.Inherit
		out.Inherit = &v
	}
	if r.CreateDB != nil {
		v := *r.CreateDB
		out.CreateDB = &v
	}
	if r.CreateRole != nil {
		v := *r.CreateRole
		out.CreateRole = &v
	}
	if len(r.Config) > 0 {
		out.Config = make(map[string]string, len(r.Config))
		maps.Copy(out.Config, r.Config)
	}
	return out
}

// mergeRoleAttrs copies attributes from src onto dst (src wins). Name is kept
// from dst unless empty.
func mergeRoleAttrs(dst, src *Role) {
	if dst == nil || src == nil {
		return
	}
	if src.Name != "" {
		dst.Name = src.Name
	}
	if src.Login != nil {
		v := *src.Login
		dst.Login = &v
	}
	if src.HasPassword {
		dst.HasPassword = true
		dst.Password = src.Password
	}
	if src.ValidUntil != "" {
		dst.ValidUntil = src.ValidUntil
	}
	if src.ConnectionLimit != nil {
		v := *src.ConnectionLimit
		dst.ConnectionLimit = &v
	}
	if src.Inherit != nil {
		v := *src.Inherit
		dst.Inherit = &v
	}
	if src.CreateDB != nil {
		v := *src.CreateDB
		dst.CreateDB = &v
	}
	if src.CreateRole != nil {
		v := *src.CreateRole
		dst.CreateRole = &v
	}
	if src.Config != nil {
		if dst.Config == nil {
			dst.Config = make(map[string]string)
		}
		maps.Copy(dst.Config, src.Config)
	}
}

// parseCreateRoleStatement parses CREATE ROLE/USER name [WITH] options.
func parseCreateRoleStatement(stmt string) (*Role, error) {
	m := createRoleRe.FindStringSubmatch(stmt)
	if m == nil {
		return nil, fmt.Errorf("malformed CREATE ROLE/USER statement: %q", stmt)
	}
	name := statementIdentifier(m)
	if name == "" {
		return nil, fmt.Errorf("role identifier must not be empty: %q", stmt)
	}
	if !validStatementIdentifier(m) {
		return nil, fmt.Errorf("role identifier is invalid or exceeds PostgreSQL's 63-byte limit: %q", stmt)
	}
	role := &Role{Name: name}
	rest := strings.TrimSpace(stmt[len(m[0]):])
	if rest == "" {
		return role, nil
	}
	if err := applyRoleOptionClause(rest, role); err != nil {
		return nil, err
	}
	return role, nil
}

// parseAlterRoleStatement parses ALTER ROLE/USER name WITH options | SET/RESET.
// Returns the role name and a partial Role carrying only the altered fields.
// For RESET, Config contains the key mapped to "" as a tombstone that callers
// must delete rather than store.
func parseAlterRoleStatement(stmt string) (name string, partial *Role, resetKeys []string, err error) {
	upper := strings.ToUpper(stmt)
	prefixLen := 0
	switch {
	case strings.HasPrefix(upper, "ALTER ROLE"):
		prefixLen = len("ALTER ROLE")
	case strings.HasPrefix(upper, "ALTER USER"):
		prefixLen = len("ALTER USER")
	default:
		return "", nil, nil, fmt.Errorf("malformed ALTER ROLE/USER statement: %q", stmt)
	}
	rest := strings.TrimSpace(stmt[prefixLen:])
	name, rest, ok := splitLeadingIdentifier(rest)
	if !ok || name == "" {
		return "", nil, nil, fmt.Errorf("role identifier must not be empty: %q", stmt)
	}
	if len([]byte(name)) > 63 {
		return "", nil, nil, fmt.Errorf("role identifier is invalid or exceeds PostgreSQL's 63-byte limit: %q", stmt)
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", nil, nil, fmt.Errorf("ALTER ROLE/USER requires WITH options or SET/RESET: %q", stmt)
	}
	restUpper := strings.ToUpper(rest)
	switch {
	case strings.HasPrefix(restUpper, "SET "):
		partial = &Role{Name: name, Config: make(map[string]string)}
		if err := applyRoleSetClause(strings.TrimSpace(rest[3:]), partial); err != nil {
			return "", nil, nil, err
		}
		return name, partial, nil, nil
	case strings.HasPrefix(restUpper, "RESET "):
		key := strings.TrimSpace(rest[5:])
		if key == "" || strings.ContainsAny(key, " \t\n\r") {
			return "", nil, nil, fmt.Errorf("unsupported ALTER ROLE RESET form: %q", stmt)
		}
		if strings.EqualFold(key, "ALL") {
			return "", nil, nil, fmt.Errorf("ALTER ROLE RESET ALL is not supported: %q", stmt)
		}
		return name, &Role{Name: name}, []string{strings.ToLower(key)}, nil
	case strings.HasPrefix(restUpper, "IN DATABASE"):
		return "", nil, nil, fmt.Errorf("ALTER ROLE ... IN DATABASE is not supported (only role-level settings): %q", stmt)
	default:
		partial = &Role{Name: name}
		if err := applyRoleOptionClause(rest, partial); err != nil {
			return "", nil, nil, err
		}
		return name, partial, nil, nil
	}
}

func applyRoleSetClause(rest string, role *Role) error {
	param, rest, ok := splitLeadingConfigParam(rest)
	if !ok || param == "" {
		return fmt.Errorf("ALTER ROLE SET requires a parameter name")
	}
	rest = strings.TrimSpace(rest)
	restUpper := strings.ToUpper(rest)
	switch {
	case strings.HasPrefix(restUpper, "FROM CURRENT"):
		tail := strings.TrimSpace(rest[len("FROM CURRENT"):])
		if tail != "" {
			return fmt.Errorf("unexpected trailing tokens after SET ... FROM CURRENT: %q", tail)
		}
		role.Config[param] = ConfigFromCurrent
		return nil
	case strings.HasPrefix(rest, "=") || strings.HasPrefix(restUpper, "TO "):
		var value string
		if strings.HasPrefix(rest, "=") {
			value = strings.TrimSpace(rest[1:])
		} else {
			value = strings.TrimSpace(rest[2:])
		}
		if value == "" {
			return fmt.Errorf("ALTER ROLE SET requires a value")
		}
		normalized, err := normalizeConfigValue(value)
		if err != nil {
			return err
		}
		role.Config[param] = normalized
		return nil
	default:
		return fmt.Errorf("unsupported ALTER ROLE SET form")
	}
}

func normalizeConfigValue(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("empty SET value")
	}
	if len(value) >= 2 && value[0] == '\'' {
		decoded, err := decodeSingleQuotedLiteral(value)
		if err != nil {
			return "", err
		}
		return decoded, nil
	}
	// Unquoted literals / numbers / identifiers: collapse whitespace.
	return strings.Join(strings.Fields(value), " "), nil
}

func applyRoleOptionClause(rest string, role *Role) error {
	rest = strings.TrimSpace(rest)
	if strings.HasPrefix(strings.ToUpper(rest), "WITH ") {
		rest = strings.TrimSpace(rest[4:])
	}
	tokens, err := tokenizeRoleOptions(rest)
	if err != nil {
		return err
	}
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		upper := strings.ToUpper(tok)
		switch upper {
		case "LOGIN":
			role.Login = new(true)
		case "NOLOGIN":
			role.Login = new(false)
		case "INHERIT":
			role.Inherit = new(true)
		case "NOINHERIT":
			role.Inherit = new(false)
		case "CREATEDB":
			role.CreateDB = new(true)
		case "NOCREATEDB":
			role.CreateDB = new(false)
		case "CREATEROLE":
			role.CreateRole = new(true)
		case "NOCREATEROLE":
			role.CreateRole = new(false)
		case "SUPERUSER", "NOSUPERUSER", "REPLICATION", "NOREPLICATION", "BYPASSRLS", "NOBYPASSRLS":
			return fmt.Errorf("%s is not supported for managed roles", upper)
		case "PASSWORD", "ENCRYPTED":
			if upper == "ENCRYPTED" {
				if i+1 >= len(tokens) || !strings.EqualFold(tokens[i+1], "PASSWORD") {
					return fmt.Errorf("expected PASSWORD after ENCRYPTED")
				}
				i++
			}
			if i+1 >= len(tokens) {
				return fmt.Errorf("PASSWORD requires a quoted literal")
			}
			lit := tokens[i+1]
			if strings.EqualFold(lit, "NULL") {
				return fmt.Errorf("PASSWORD NULL is not supported")
			}
			decoded, err := decodeSingleQuotedLiteral(lit)
			if err != nil {
				return fmt.Errorf("PASSWORD requires a quoted literal: %w", err)
			}
			role.Password = decoded
			role.HasPassword = true
			i++
		case "VALID":
			if i+1 >= len(tokens) || !strings.EqualFold(tokens[i+1], "UNTIL") {
				return fmt.Errorf("expected UNTIL after VALID")
			}
			i += 2
			if i >= len(tokens) {
				return fmt.Errorf("VALID UNTIL requires a quoted timestamp")
			}
			decoded, err := decodeSingleQuotedLiteral(tokens[i])
			if err != nil {
				return fmt.Errorf("VALID UNTIL requires a quoted timestamp: %w", err)
			}
			role.ValidUntil = decoded
		case "CONNECTION":
			if i+1 >= len(tokens) || !strings.EqualFold(tokens[i+1], "LIMIT") {
				return fmt.Errorf("expected LIMIT after CONNECTION")
			}
			i += 2
			if i >= len(tokens) {
				return fmt.Errorf("CONNECTION LIMIT requires an integer")
			}
			n, err := strconv.Atoi(tokens[i])
			if err != nil {
				return fmt.Errorf("CONNECTION LIMIT requires an integer: %w", err)
			}
			role.ConnectionLimit = new(n)
		default:
			return fmt.Errorf("unsupported role option %q", tok)
		}
	}
	return nil
}

func tokenizeRoleOptions(s string) ([]string, error) {
	var tokens []string
	i := 0
	for i < len(s) {
		for i < len(s) && unicode.IsSpace(rune(s[i])) {
			i++
		}
		if i >= len(s) {
			break
		}
		if s[i] == '\'' {
			end, err := scanSingleQuotedLiteral(s, i)
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, s[i:end])
			i = end
			continue
		}
		start := i
		for i < len(s) && !unicode.IsSpace(rune(s[i])) {
			i++
		}
		tokens = append(tokens, s[start:i])
	}
	return tokens, nil
}

func scanSingleQuotedLiteral(s string, start int) (int, error) {
	if start >= len(s) || s[start] != '\'' {
		return 0, fmt.Errorf("expected quoted literal")
	}
	i := start + 1
	for i < len(s) {
		if s[i] == '\'' {
			if i+1 < len(s) && s[i+1] == '\'' {
				i += 2
				continue
			}
			return i + 1, nil
		}
		i++
	}
	return 0, fmt.Errorf("unterminated string literal")
}

func decodeSingleQuotedLiteral(lit string) (string, error) {
	lit = strings.TrimSpace(lit)
	if len(lit) < 2 || lit[0] != '\'' || lit[len(lit)-1] != '\'' {
		return "", fmt.Errorf("not a quoted literal")
	}
	var b strings.Builder
	for i := 1; i < len(lit)-1; i++ {
		if lit[i] == '\'' {
			if i+1 < len(lit)-1 && lit[i+1] == '\'' {
				b.WriteByte('\'')
				i++
				continue
			}
			return "", fmt.Errorf("malformed quoted literal")
		}
		b.WriteByte(lit[i])
	}
	return b.String(), nil
}

func splitLeadingIdentifier(s string) (name, rest string, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", false
	}
	if s[0] == '"' {
		i := 1
		for i < len(s) {
			if s[i] == '"' {
				if i+1 < len(s) && s[i+1] == '"' {
					i += 2
					continue
				}
				raw := s[:i+1]
				return decodeIdentifier(raw), strings.TrimSpace(s[i+1:]), true
			}
			i++
		}
		return "", "", false
	}
	i := 0
	for i < len(s) {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '$' {
			i++
			continue
		}
		break
	}
	if i == 0 {
		return "", "", false
	}
	return strings.ToLower(s[:i]), strings.TrimSpace(s[i:]), true
}

func splitLeadingConfigParam(s string) (param, rest string, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", false
	}
	i := 0
	for i < len(s) {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '.' {
			i++
			continue
		}
		break
	}
	if i == 0 {
		return "", "", false
	}
	return strings.ToLower(s[:i]), strings.TrimSpace(s[i:]), true
}

// applyAlterToRole merges an ALTER ROLE partial into an existing role, applying
// RESET tombstones by deleting keys.
func applyAlterToRole(role *Role, partial *Role, resetKeys []string) {
	if role == nil {
		return
	}
	if partial != nil {
		mergeRoleAttrs(role, partial)
	}
	for _, key := range resetKeys {
		if role.Config != nil {
			delete(role.Config, key)
		}
	}
}

// RedactRolePasswordsSQL replaces PASSWORD '...' literals with the redacted
// placeholder so plan documents, Plan.Hash(), and step SQL never echo secrets.
// Hash and Document must use the same redaction so ParsePlanJSON round-trips.
func RedactRolePasswordsSQL(sql string) string {
	if sql == "" || !strings.Contains(strings.ToUpper(sql), "PASSWORD") {
		return sql
	}
	var b strings.Builder
	upper := strings.ToUpper(sql)
	i := 0
	for i < len(sql) {
		// Find PASSWORD keyword outside quotes roughly via uppercase scan.
		idx := strings.Index(upper[i:], "PASSWORD")
		if idx < 0 {
			b.WriteString(sql[i:])
			break
		}
		idx += i
		// Ensure keyword boundary.
		if idx > 0 {
			prev := sql[idx-1]
			if (prev >= 'a' && prev <= 'z') || (prev >= 'A' && prev <= 'Z') || (prev >= '0' && prev <= '9') || prev == '_' {
				b.WriteString(sql[i : idx+8])
				i = idx + 8
				continue
			}
		}
		b.WriteString(sql[i:idx])
		b.WriteString(sql[idx : idx+8]) // PASSWORD
		j := idx + 8
		for j < len(sql) && unicode.IsSpace(rune(sql[j])) {
			b.WriteByte(sql[j])
			j++
		}
		if j < len(sql) && strings.EqualFold(sql[j:min(j+4, len(sql))], "NULL") {
			b.WriteString(sql[j : j+4])
			i = j + 4
			continue
		}
		if j >= len(sql) || sql[j] != '\'' {
			i = j
			continue
		}
		end, err := scanSingleQuotedLiteral(sql, j)
		if err != nil {
			b.WriteString(sql[j:])
			break
		}
		b.WriteByte('\'')
		b.WriteString(PasswordRedacted)
		b.WriteByte('\'')
		i = end
	}
	return b.String()
}

// RoleHasUsablePassword reports whether role IR carries a real plaintext
// password suitable for apply (not empty and not the redaction placeholder).
func RoleHasUsablePassword(role *Role) bool {
	if role == nil || !role.HasPassword {
		return false
	}
	if role.Password == "" || role.Password == PasswordRedacted {
		return false
	}
	return true
}
