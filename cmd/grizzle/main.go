// Package main implements the grizzle CLI.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/muandane/grizzle"
	_ "modernc.org/sqlite"
)

type hazardFlags []string

func (h *hazardFlags) String() string {
	return strings.Join(*h, ",")
}

func (h *hazardFlags) Set(val string) error {
	for p := range strings.SplitSeq(val, ",") {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			*h = append(*h, trimmed)
		}
	}
	return nil
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func getDSN(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if env := os.Getenv("GRIZZLE_DSN"); env != "" {
		return env
	}
	return os.Getenv("DATABASE_URL")
}

func redactDSN(dsn string) string {
	if dsn == "" {
		return ""
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return "<redacted-dsn>"
	}
	return u.Redacted()
}

func openDB(dsn string) (*sql.DB, error) {
	driver := "pgx"
	cleanDSN := dsn
	if strings.HasPrefix(dsn, "sqlite://") {
		driver = "sqlite"
		cleanDSN = strings.TrimPrefix(dsn, "sqlite://")
	} else if strings.HasPrefix(dsn, "sqlite:") {
		driver = "sqlite"
		cleanDSN = strings.TrimPrefix(dsn, "sqlite:")
	} else if strings.HasSuffix(dsn, ".db") || strings.HasSuffix(dsn, ".sqlite") || dsn == ":memory:" {
		driver = "sqlite"
	}
	return sql.Open(driver, cleanDSN)
}

func setupLogger(jsonLog bool) {
	var handler slog.Handler
	if jsonLog {
		handler = slog.NewJSONHandler(os.Stderr, nil)
	} else {
		handler = slog.NewTextHandler(os.Stderr, nil)
	}
	slog.SetDefault(slog.New(handler))
}

var (
	version = "v0.1.0"
	commit  = "none"
	date    = "unknown"
)

func run(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: grizzle <plan|apply|check|export|version> [flags]")
		return 1
	}

	command := args[0]
	if command == "version" || command == "--version" || command == "-version" || command == "-v" {
		fmt.Printf("grizzle %s (commit: %s, date: %s)\n", version, commit, date)
		return 0
	}

	fs := flag.NewFlagSet(command, flag.ContinueOnError)

	var (
		dsnFlag      = fs.String("dsn", "", "Database DSN connection string")
		schemaFile   = fs.String("schema", "schema.sql", "Path to schema SQL file")
		planFile     = fs.String("plan", "", "Path to plan JSON file")
		outFile      = fs.String("out", "", "Destination file or directory")
		formatFlag   = fs.String("format", "sql", "Export format: sql, goose, or atlas")
		allowDrop    = fs.Bool("allow-drop", false, "Permit destructive operations")
		jsonOutput   = fs.Bool("json", false, "Output in JSON format")
		jsonLog      = fs.Bool("json-log", false, "Emit logs in structured JSON format")
		expectedHash = fs.String("expected-hash", "", "Expected plan approval hash")
		versionFlag  = fs.String("version", version, "Version string for export headers")
	)

	var hazards hazardFlags
	fs.Var(&hazards, "accept-hazard", "Hazard code to accept (can be repeated or comma-separated)")

	if err := fs.Parse(args[1:]); err != nil {
		return 1
	}

	setupLogger(*jsonLog)

	ctx := context.Background()
	dsn := getDSN(*dsnFlag)

	switch command {
	case "plan":
		if dsn == "" {
			slog.Error("database DSN is required (via --dsn, GRIZZLE_DSN, or DATABASE_URL)")
			return 1
		}
		db, err := openDB(dsn)
		if err != nil {
			slog.Error("connecting to database", "target", redactDSN(dsn), "err", err)
			return 1
		}
		defer func() { _ = db.Close() }()

		content, err := os.ReadFile(filepath.Clean(*schemaFile)) //nolint:gosec // G304: CLI accepts user-provided schema file path
		if err != nil {
			slog.Error("reading schema file", "path", *schemaFile, "err", err)
			return 1
		}

		opts := grizzle.Options{
			SchemaSQL: string(content),
			AllowDrop: *allowDrop,
		}
		p, err := grizzle.PlanDiff(ctx, db, opts)
		if err != nil {
			slog.Error("computing plan diff", "err", err)
			return 1
		}

		data, err := p.ToJSON()
		if err != nil {
			slog.Error("serializing plan", "err", err)
			return 1
		}

		if *outFile != "" {
			if err := os.WriteFile(filepath.Clean(*outFile), data, 0600); err != nil { //nolint:gosec // G304: CLI accepts user-provided destination path
				slog.Error("writing plan file", "path", *outFile, "err", err)
				return 1
			}
			slog.Info("plan written successfully", "out", *outFile, "hash", p.Hash(), "steps", len(p.Steps))
		} else if *jsonOutput {
			fmt.Println(string(data))
		} else {
			_ = p.Format(os.Stdout, true)
			fmt.Fprintf(os.Stderr, "Plan Hash: %s\n", p.Hash())
		}
		return 0

	case "apply":
		if dsn == "" {
			slog.Error("database DSN is required (via --dsn, GRIZZLE_DSN, or DATABASE_URL)")
			return 1
		}
		db, err := openDB(dsn)
		if err != nil {
			slog.Error("connecting to database", "target", redactDSN(dsn), "err", err)
			return 1
		}
		defer func() { _ = db.Close() }()

		var p *grizzle.Plan
		var planHash string

		var acceptedCodes []grizzle.HazardCode
		for _, h := range hazards {
			acceptedCodes = append(acceptedCodes, grizzle.HazardCode(h))
		}

		if *planFile != "" {
			planData, err := os.ReadFile(filepath.Clean(*planFile)) //nolint:gosec // G304: CLI accepts user-provided plan file path
			if err != nil {
				slog.Error("reading plan file", "path", *planFile, "err", err)
				return 1
			}
			parsedPlan, recordedHash, err := grizzle.ParsePlanJSON(planData)
			if err != nil {
				slog.Error("parsing plan JSON", "err", err)
				return 1
			}
			p = parsedPlan
			planHash = recordedHash
		} else {
			content, err := os.ReadFile(filepath.Clean(*schemaFile)) //nolint:gosec // G304: CLI accepts user-provided schema file path
			if err != nil {
				slog.Error("reading schema file", "path", *schemaFile, "err", err)
				return 1
			}
			opts := grizzle.Options{
				SchemaSQL:     string(content),
				AllowDrop:     *allowDrop,
				AcceptHazards: acceptedCodes,
			}
			computedPlan, err := grizzle.PlanDiff(ctx, db, opts)
			if err != nil {
				slog.Error("computing plan", "err", err)
				return 1
			}
			p = computedPlan
			planHash = p.Hash()
		}

		expHash := planHash
		if *expectedHash != "" {
			expHash = *expectedHash
		}

		applyOpts := grizzle.ApplyOpts{
			ExpectedHash:  expHash,
			AcceptHazards: acceptedCodes,
		}

		if err := grizzle.Apply(ctx, db, p, applyOpts); err != nil {
			if errors.Is(err, grizzle.ErrPlanDrift) {
				slog.Error("plan drift detected; database modified since plan approval", "err", err)
				return 3
			}
			var hazardErr *grizzle.HazardError
			if errors.Is(err, grizzle.ErrHazardBlocked) || errors.As(err, &hazardErr) {
				slog.Error("migration blocked by unaccepted critical hazard", "err", err)
				return 2
			}
			slog.Error("applying plan", "err", err)
			return 1
		}

		slog.Info("schema applied successfully", "hash", planHash, "steps", len(p.Steps))
		return 0

	case "check":
		if dsn == "" {
			slog.Error("database DSN is required (via --dsn, GRIZZLE_DSN, or DATABASE_URL)")
			return 1
		}
		db, err := openDB(dsn)
		if err != nil {
			slog.Error("connecting to database", "target", redactDSN(dsn), "err", err)
			return 1
		}
		defer func() { _ = db.Close() }()

		content, err := os.ReadFile(filepath.Clean(*schemaFile)) //nolint:gosec // G304: CLI accepts user-provided schema file path
		if err != nil {
			slog.Error("reading schema file", "path", *schemaFile, "err", err)
			return 1
		}

		opts := grizzle.Options{
			SchemaSQL: string(content),
			AllowDrop: *allowDrop,
		}
		p, err := grizzle.PlanDiff(ctx, db, opts)
		if err != nil {
			slog.Error("checking schema drift", "err", err)
			return 1
		}

		if len(p.Steps) > 0 {
			slog.Warn("schema drift detected", "steps", len(p.Steps), "hash", p.Hash())
			fmt.Fprintf(os.Stderr, "Drift detected: %d pending changes (hash: %s)\n", len(p.Steps), p.Hash())
			return 4
		}

		slog.Info("database is in sync")
		fmt.Println("Database is in sync.")
		return 0

	case "export":
		var p *grizzle.Plan
		if *planFile != "" {
			planData, err := os.ReadFile(filepath.Clean(*planFile)) //nolint:gosec // G304: CLI accepts user-provided plan file path
			if err != nil {
				slog.Error("reading plan file", "path", *planFile, "err", err)
				return 1
			}
			parsedPlan, _, err := grizzle.ParsePlanJSON(planData)
			if err != nil {
				slog.Error("parsing plan JSON", "err", err)
				return 1
			}
			p = parsedPlan
		} else {
			if dsn == "" {
				slog.Error("database DSN or --plan is required for export")
				return 1
			}
			db, err := openDB(dsn)
			if err != nil {
				slog.Error("connecting to database", "target", redactDSN(dsn), "err", err)
				return 1
			}
			defer func() { _ = db.Close() }()

			content, err := os.ReadFile(filepath.Clean(*schemaFile)) //nolint:gosec // G304: CLI accepts user-provided schema file path
			if err != nil {
				slog.Error("reading schema file", "path", *schemaFile, "err", err)
				return 1
			}
			opts := grizzle.Options{
				SchemaSQL: string(content),
				AllowDrop: *allowDrop,
			}
			computedPlan, err := grizzle.PlanDiff(ctx, db, opts)
			if err != nil {
				slog.Error("computing plan for export", "err", err)
				return 1
			}
			p = computedPlan
		}

		artifacts, err := grizzle.Export(p, grizzle.ExportFormat(*formatFlag), *versionFlag)
		if err != nil {
			slog.Error("export failed", "err", err)
			return 1
		}

		outDir := *outFile
		if outDir == "" {
			outDir = "."
		}
		if err := os.MkdirAll(filepath.Clean(outDir), 0750); err != nil { //nolint:gosec // G703, G301: CLI creates user-specified export directory
			slog.Error("creating export directory", "path", outDir, "err", err)
			return 1
		}

		for _, art := range artifacts {
			targetPath := filepath.Join(filepath.Clean(outDir), art.Filename)
			if err := os.WriteFile(targetPath, []byte(art.Content), 0600); err != nil { //nolint:gosec // G304: CLI writes generated artifact to user directory
				slog.Error("writing exported artifact", "path", targetPath, "err", err)
				return 1
			}
			slog.Info("exported migration artifact", "path", targetPath, "format", *formatFlag)
			fmt.Printf("Exported: %s\n", targetPath)
		}
		return 0

	default:
		slog.Error("unknown command", "command", command)
		fmt.Fprintf(os.Stderr, "Unknown command: %q. Expected plan, apply, check, or export.\n", command)
		return 1
	}
}
