package exec_test

import (
	"log/slog"
	"strings"
)

// capturingLogs is a minimal io.Writer sink for asserting slog output.
type capturingLogs struct {
	buf strings.Builder
}

func (c *capturingLogs) Write(p []byte) (int, error) {
	return c.buf.Write(p)
}

func (c *capturingLogs) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&c.buf, nil))
}
