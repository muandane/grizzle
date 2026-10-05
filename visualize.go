package grizzle

import (
	"fmt"
	"io"
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
	summaryLine := fmt.Sprintf("\nPlan: %d to add, %d to alter, %d to destroy", adds, alters, drops)
	if blocked > 0 {
		summaryLine += fmt.Sprintf(" (%d blocked by safety policy)", blocked)
	}
	summaryLine += ".\n"

	if useColor {
		if blocked > 0 {
			summaryLine = fmt.Sprintf("\n%s%sPlan: %d to add, %d to alter, %d to destroy (%d blocked by safety policy).%s\n", colorBold, colorRed, adds, alters, drops, blocked, colorReset)
		} else {
			summaryLine = fmt.Sprintf("\n%sPlan: %d to add, %d to alter, %d to destroy.%s\n", colorBold, adds, alters, drops, colorReset)
		}
	}
	sb.WriteString(summaryLine)

	_, err := io.WriteString(w, sb.String())
	return err
}

// String returns a plain-text human-readable visual summary of the migration plan.
func (p *Plan) String() string {
	var sb strings.Builder
	_ = p.Format(&sb, false)
	return sb.String()
}
