package plan

import (
	"fmt"
	"io"
	"slices"
	"strings"
)

const (
	colorReset  = "\033[0m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorRed    = "\033[31m"
	colorBold   = "\033[1m"
	colorCyan   = "\033[36m"
)

// Format writes a human-readable visual summary of the migration plan to the given writer.
// If useColor is true, ANSI color escape codes are applied.
func (p *Plan) Format(w io.Writer, useColor bool) error {
	var sb strings.Builder

	header := fmt.Sprintf("Grizzle Migration Plan for schema %q:\n\n", p.TargetSchema)
	if useColor {
		header = fmt.Sprintf("%s%sGrizzle Migration Plan for schema %q:%s\n\n", colorBold, colorCyan, p.TargetSchema, colorReset)
	}
	sb.WriteString(header)

	if len(p.Steps) == 0 {
		msg := "  No changes. Database schema is already in sync.\n"
		if useColor {
			msg = fmt.Sprintf("  %sNo changes. Database schema is already in sync.%s\n", colorGreen, colorReset)
		}
		sb.WriteString(msg)
		_, err := io.WriteString(w, sb.String())
		return err
	}

	for _, s := range p.Steps {
		symbol := " "
		color := ""
		isBlocked := !p.Policy.IsAllowed(s)

		switch s.Type {
		case ChangeCreateEnum, ChangeCreateTable, ChangeAddColumn, ChangeCreateIndex, ChangeAddFK:
			symbol = "+"
			color = colorGreen
		case ChangeAlterColumn, ChangeAlterEnum:
			symbol = "~"
			color = colorYellow
		case ChangeDropTable, ChangeDropColumn, ChangeDropIndex, ChangeDropFK:
			symbol = "-"
			color = colorRed
		}

		blockedBadge := ""
		if isBlocked {
			if useColor {
				blockedBadge = fmt.Sprintf(" %s%s[BLOCKED by policy]%s", colorBold, colorRed, colorReset)
			} else {
				blockedBadge = " [BLOCKED by policy]"
			}
		}

		var line string
		if useColor {
			line = fmt.Sprintf("  %s%s%s %-16s %s%s\n", color, symbol, colorReset, "["+string(s.Type)+"]", s.SQL, blockedBadge)
		} else {
			line = fmt.Sprintf("  %s %-16s %s%s\n", symbol, "["+string(s.Type)+"]", s.SQL, blockedBadge)
		}
		sb.WriteString(line)
	}

	adds, alters, drops, blocked := p.Summary()
	summaryText := fmt.Sprintf("Plan: %d to add, %d to alter, %d to destroy", adds, alters, drops)
	if blocked > 0 {
		summaryText += fmt.Sprintf(" (%d blocked by safety policy)", blocked)
	}
	summaryText += "."

	if useColor {
		summaryColor := colorBold
		if blocked > 0 {
			summaryColor = colorBold + colorRed
		}
		fmt.Fprintf(&sb, "\n%s%s%s\n", summaryColor, summaryText, colorReset)
	} else {
		fmt.Fprintf(&sb, "\n%s\n", summaryText)
	}

	hazards := p.Hazards()
	if len(hazards) > 0 {
		hazardHeader := "\nDetected Hazards:\n"
		if useColor {
			hazardHeader = fmt.Sprintf("\n%s%sDetected Hazards:%s\n", colorBold, colorYellow, colorReset)
		}
		sb.WriteString(hazardHeader)
		for _, h := range hazards {
			if useColor {
				hColor := colorYellow
				if h.Level == HazardLevelCritical {
					hColor = colorRed
				}
				fmt.Fprintf(&sb, "  %s[%s]%s %s\n", hColor, h.Level, colorReset, h.Description)
			} else {
				fmt.Fprintf(&sb, "  [%s] %s\n", h.Level, h.Description)
			}
		}
	}

	_, err := io.WriteString(w, sb.String())
	return err
}

// String returns a plain-text human-readable visual summary of the migration plan.
func (p *Plan) String() string {
	var sb strings.Builder
	_ = p.Format(&sb, false)
	return sb.String()
}

// FormatInteractiveSummary writes a formatted table of planned steps and critical hazards
// for interactive terminal inspection, conforming to the TTY UX specification.
func (p *Plan) FormatInteractiveSummary(w io.Writer) error {
	var sb strings.Builder
	sb.WriteString("Planned changes:\n")

	if len(p.Steps) == 0 {
		sb.WriteString("  No changes. Database schema is already in sync.\n")
		_, err := io.WriteString(w, sb.String())
		return err
	}

	for _, s := range p.Steps {
		hazards := stepHazards(s)
		hasCriticalOrWarning := false
		for _, h := range hazards {
			if h.Level == HazardLevelCritical || h.Level == HazardLevelWarning {
				hasCriticalOrWarning = true
				break
			}
		}

		symbol := " "
		if hasCriticalOrWarning {
			symbol = "!"
		} else {
			switch s.Type {
			case ChangeCreateEnum, ChangeCreateTable, ChangeAddColumn, ChangeCreateIndex, ChangeAddFK, ChangeAttachPartition:
				symbol = "+"
			case ChangeAlterColumn, ChangeAlterEnum, ChangeValidateConstraint, ChangeRenameColumn:
				symbol = "~"
			case ChangeDropTable, ChangeDropColumn, ChangeDropIndex, ChangeDropFK, ChangeDetachPartition:
				symbol = "-"
			}
		}

		desc := stepOperationSummary(s)

		var badges []string
		for _, h := range hazards {
			badges = append(badges, fmt.Sprintf("[%s: %s]", h.Code, h.Level))
		}
		if !p.Policy.IsAllowed(s) {
			badges = append(badges, "[BLOCKED by policy]")
		}

		badgeStr := ""
		if len(badges) > 0 {
			badgeStr = " " + strings.Join(badges, " ")
		}

		fmt.Fprintf(&sb, "  %s %s%s\n", symbol, desc, badgeStr)
	}

	var criticalHazards []Hazard
	for _, h := range p.Hazards() {
		if h.Level == HazardLevelCritical {
			criticalHazards = append(criticalHazards, h)
		}
	}

	if len(criticalHazards) > 0 {
		sb.WriteString("\n")
		for _, h := range criticalHazards {
			fmt.Fprintf(&sb, "[%s] %s\n", h.Code, h.Description)
		}
	}

	_, err := io.WriteString(w, sb.String())
	return err
}

func cleanIdentifier(ident string) string {
	ident = strings.TrimSpace(ident)
	ident = strings.Trim(ident, "\"`")
	return ident
}

func formatStepTable(s Step) string {
	tbl := cleanIdentifier(s.Table)
	if s.Schema != "" && s.Schema != "public" && !strings.Contains(tbl, ".") {
		return s.Schema + "." + tbl
	}
	return tbl
}

func splitTopLevelSQL(s string) []string {
	var parts []string
	var current strings.Builder
	depth := 0
	for _, r := range s {
		switch r {
		case '(':
			depth++
			current.WriteRune(r)
		case ')':
			if depth > 0 {
				depth--
			}
			current.WriteRune(r)
		case ',':
			if depth == 0 {
				parts = append(parts, current.String())
				current.Reset()
			} else {
				current.WriteRune(r)
			}
		default:
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		parts = append(parts, current.String())
	}
	return parts
}

func extractTableColumns(sqlStr string) []string {
	openIdx := strings.Index(sqlStr, "(")
	closeIdx := strings.LastIndex(sqlStr, ")")
	if openIdx == -1 || closeIdx == -1 || closeIdx <= openIdx {
		return nil
	}
	body := sqlStr[openIdx+1 : closeIdx]
	parts := splitTopLevelSQL(body)
	var cols []string

	skipPrefixes := []string{"CONSTRAINT", "PRIMARY", "FOREIGN", "CHECK", "UNIQUE", "KEY", "PARTITION", "LIKE"}

	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		fields := strings.Fields(trimmed)
		if len(fields) == 0 {
			continue
		}
		upperFirst := strings.ToUpper(cleanIdentifier(fields[0]))
		skip := slices.Contains(skipPrefixes, upperFirst)
		if skip {
			continue
		}
		colName := cleanIdentifier(fields[0])
		if colName != "" {
			cols = append(cols, colName)
		}
	}
	return cols
}

func extractIndexName(sqlStr string) string {
	sqlClean := strings.ReplaceAll(sqlStr, "\n", " ")
	tokens := strings.Fields(sqlClean)
	for i, token := range tokens {
		upper := strings.ToUpper(cleanIdentifier(token))
		if upper == "INDEX" {
			for j := i + 1; j < len(tokens); j++ {
				u := strings.ToUpper(cleanIdentifier(tokens[j]))
				if u == "CONCURRENTLY" || u == "IF" || u == "NOT" || u == "EXISTS" {
					continue
				}
				if u == "ON" {
					break
				}
				return cleanIdentifier(tokens[j])
			}
		}
	}
	return ""
}

func stepOperationSummary(s Step) string {
	tbl := formatStepTable(s)
	col := cleanIdentifier(s.Column)
	oldCol := cleanIdentifier(s.OldColumn)
	parent := cleanIdentifier(s.ParentTable)

	switch s.Type {
	case ChangeCreateTable:
		cols := extractTableColumns(s.SQL)
		if len(cols) > 0 {
			return fmt.Sprintf("CREATE TABLE %s (%s)", tbl, strings.Join(cols, ", "))
		}
		if s.ParentTable != "" {
			return fmt.Sprintf("CREATE TABLE %s PARTITION OF %s", tbl, parent)
		}
		return fmt.Sprintf("CREATE TABLE %s", tbl)

	case ChangeDropTable:
		return fmt.Sprintf("DROP TABLE %s", tbl)

	case ChangeAddColumn:
		if col != "" {
			return fmt.Sprintf("ADD COLUMN %s.%s", tbl, col)
		}
		return fmt.Sprintf("ADD COLUMN ON %s", tbl)

	case ChangeDropColumn:
		if col != "" {
			return fmt.Sprintf("DROP COLUMN %s.%s", tbl, col)
		}
		return fmt.Sprintf("DROP COLUMN ON %s", tbl)

	case ChangeAlterColumn:
		if col != "" {
			return fmt.Sprintf("ALTER COLUMN %s.%s", tbl, col)
		}
		return fmt.Sprintf("ALTER COLUMN ON %s", tbl)

	case ChangeRenameColumn:
		if col != "" && oldCol != "" {
			return fmt.Sprintf("RENAME COLUMN %s.%s TO %s", tbl, oldCol, col)
		}
		return fmt.Sprintf("RENAME COLUMN ON %s", tbl)

	case ChangeCreateIndex:
		idxName := extractIndexName(s.SQL)
		if idxName != "" && tbl != "" {
			return fmt.Sprintf("CREATE INDEX %s ON %s", idxName, tbl)
		} else if tbl != "" {
			return fmt.Sprintf("CREATE INDEX ON %s", tbl)
		}
		return strings.TrimSuffix(strings.TrimSpace(s.SQL), ";")

	case ChangeDropIndex:
		idxName := extractIndexName(s.SQL)
		if idxName != "" {
			return fmt.Sprintf("DROP INDEX %s", idxName)
		}
		return strings.TrimSuffix(strings.TrimSpace(s.SQL), ";")

	case ChangeAddFK:
		if col != "" && s.RefTable != "" {
			return fmt.Sprintf("ADD FOREIGN KEY ON %s.%s -> %s", tbl, col, cleanIdentifier(s.RefTable))
		}
		return fmt.Sprintf("ADD FOREIGN KEY ON %s", tbl)

	case ChangeDropFK:
		return fmt.Sprintf("DROP FOREIGN KEY ON %s", tbl)

	case ChangeValidateConstraint:
		if col != "" {
			return fmt.Sprintf("VALIDATE CONSTRAINT %s ON %s", col, tbl)
		}
		return fmt.Sprintf("VALIDATE CONSTRAINT ON %s", tbl)

	case ChangeAttachPartition:
		if parent != "" {
			return fmt.Sprintf("ATTACH PARTITION %s TO %s", tbl, parent)
		}
		return fmt.Sprintf("ATTACH PARTITION %s", tbl)

	case ChangeDetachPartition:
		if parent != "" {
			return fmt.Sprintf("DETACH PARTITION %s FROM %s", tbl, parent)
		}
		return fmt.Sprintf("DETACH PARTITION %s", tbl)

	case ChangeCreateEnum:
		return fmt.Sprintf("CREATE TYPE %s AS ENUM", tbl)

	case ChangeAlterEnum:
		return fmt.Sprintf("ALTER TYPE %s ADD VALUE", tbl)

	default:
		return strings.TrimSuffix(strings.TrimSpace(s.SQL), ";")
	}
}

// FormatGitHubActions formats plan summary and hazards into GitHub Actions workflow commands.
func (p *Plan) FormatGitHubActions(w io.Writer, schemaFile string) error {
	summary := fmt.Sprintf("Adds: %d, Alters: %d, Drops: %d (Plan Hash: %s)", p.Additions(), p.Modifications(), p.Deletions(), p.Hash())
	_, _ = fmt.Fprintf(w, "::notice title=Grizzle Migration Plan::%s\n", summary)
	for _, h := range p.Hazards() {
		switch h.Level {
		case HazardLevelCritical:
			_, _ = fmt.Fprintf(w, "::error file=%s,title=Critical Hazard (%s)::%s\n", schemaFile, h.Code, h.Description)
		case HazardLevelWarning:
			_, _ = fmt.Fprintf(w, "::warning file=%s,title=Hazard Warning (%s)::%s\n", schemaFile, h.Code, h.Description)
		case HazardLevelNotice:
			_, _ = fmt.Fprintf(w, "::notice file=%s,title=Hazard Notice (%s)::%s\n", schemaFile, h.Code, h.Description)
		}
	}
	return nil
}
