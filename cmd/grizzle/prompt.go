package main

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/mattn/go-isatty"
	"github.com/muandane/grizzle"
)

var isTerminalFunc = func(fd uintptr) bool {
	return isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd)
}

func readPromptLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if err != nil && len(line) == 0 {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// promptInteractiveApply displays planned changes and hazards in an interactive terminal,
// prompting the user for confirmation before applying.
func promptInteractiveApply(in io.Reader, out io.Writer, p *grizzle.Plan) (bool, error) {
	if p == nil || len(p.Steps) == 0 {
		_, _ = fmt.Fprintln(out, "Planned changes:")
		_, _ = fmt.Fprintln(out, "  No changes. Database schema is already in sync.")
		return false, nil
	}

	if err := p.FormatInteractiveSummary(out); err != nil {
		return false, err
	}

	_, _ = fmt.Fprint(out, "\nApply these changes? [y/N/details]: ")

	reader := bufio.NewReader(in)
	line, err := readPromptLine(reader)
	if err != nil || line == "" {
		_, _ = fmt.Fprintln(out, "Migration aborted.")
		return false, nil
	}

	ans := strings.ToLower(line)
	switch ans {
	case "y", "yes":
		return true, nil
	case "d", "details":
		_, _ = fmt.Fprintln(out, "\nFull SQL statements:")
		for i, s := range p.Steps {
			_, _ = fmt.Fprintf(out, "  %d. %s\n", i+1, strings.TrimSpace(s.SQL))
		}
		_, _ = fmt.Fprint(out, "\nApply these changes? [y/N]: ")
		secondLine, err := readPromptLine(reader)
		if err != nil || secondLine == "" {
			_, _ = fmt.Fprintln(out, "Migration aborted.")
			return false, nil
		}
		secondAns := strings.ToLower(secondLine)
		if secondAns == "y" || secondAns == "yes" {
			return true, nil
		}
		_, _ = fmt.Fprintln(out, "Migration aborted.")
		return false, nil
	default:
		_, _ = fmt.Fprintln(out, "Migration aborted.")
		return false, nil
	}
}
