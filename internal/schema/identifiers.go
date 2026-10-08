package schema

import "strings"

// ParseQualifiedIdentifier splits a PostgreSQL qualified identifier without
// treating dots inside quoted identifier components as separators.
func ParseQualifiedIdentifier(identifier string) []string {
	return splitQualifiedIdentifier(identifier)
}

// CanonicalIdentifierPart returns the canonical SQL-token identity for one
// identifier component: unquoted names are folded to lower case and quoted
// names retain their decoded case and escaping.
func CanonicalIdentifierPart(identifier string) string {
	return canonicalGrantIdentifierPart(identifier)
}

// CanonicalQualifiedIdentifier returns the canonical identity for a qualified
// SQL identifier. An unqualified name is resolved against targetSchema when
// targetSchema is non-empty.
func CanonicalQualifiedIdentifier(identifier, targetSchema string) string {
	parts := splitQualifiedIdentifier(identifier)
	if len(parts) == 0 || (len(parts) == 1 && strings.TrimSpace(parts[0]) == "") {
		return ""
	}
	if len(parts) == 1 && strings.TrimSpace(targetSchema) != "" {
		parts = append([]string{canonicalTargetIdentifierPart(targetSchema)}, parts...)
	}
	for i, part := range parts {
		parts[i] = canonicalGrantIdentifierPart(part)
	}
	return strings.Join(parts, ".")
}

func isSimpleLowerIdentifier(value string) bool {
	return value != "" &&
		catalogIdentifierPartRe.MatchString(value) &&
		value == strings.ToLower(value)
}
