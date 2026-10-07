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

type renameFlags map[string]string

func (r renameFlags) String() string {
	pairs := make([]string, 0, len(r))
	for k, v := range r {
		pairs = append(pairs, k+"="+v)
	}
	return strings.Join(pairs, ",")
}

// Set parses a single --rename old=new mapping. The key may be a bare column
// name ("old_col") or table-qualified ("table.old_col"); the value is the new
// column name.
func (r renameFlags) Set(val string) error {
	old, new, ok := strings.Cut(val, "=")
	old = strings.TrimSpace(old)
	new = strings.TrimSpace(new)
	if !ok || old == "" || new == "" {
		return fmt.Errorf("invalid --rename mapping %q: expected --rename old=new", val)
	}
	if r == nil {
		return fmt.Errorf("rename map not initialized")
	}
	r[old] = new
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
	version = "v0.3.0"
	commit  = "none"
	date    = "unknown"
)

func run(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: grizzle <plan|apply|check|lint|export|seed|init|version> [flags]")
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
		formatFlag   = fs.String("format", "sql", "Output/export format: sql, goose, atlas, or github")
		allowDrop    = fs.Bool("allow-drop", false, "Permit destructive operations")
		jsonOutput   = fs.Bool("json", false, "Output in JSON format")
		githubOutput = fs.Bool("github", false, "Emit GitHub Actions workflow annotations")
		jsonLog      = fs.Bool("json-log", false, "Emit logs in structured JSON format")
		expectedHash = fs.String("expected-hash", "", "Expected plan approval hash")
		versionFlag  = fs.String("version", version, "Version string for export headers")
		templateFlag = fs.String("template", "", "Project template: sqlc, stdlib, or sqlite")
		dirFlag      = fs.String("dir", ".", "Destination directory for init")
		forceFlag    = fs.Bool("force", false, "Overwrite existing files during init")
		failOnWarn   = fs.Bool("fail-on-warning", false, "Fail lint when warnings are reported")
		dryRunFlag   = fs.Bool("dry-run", false, "Verify planned DDL against live data without persisting changes (apply)")
		seedFile     = fs.String("seed", "", "Path to seed SQL file (seed)")
		rolesFile    = fs.String("roles", "", "Path to roles SQL file (plan/apply/check): CREATE ROLE and GRANT statements diffed against live ACLs")
		catalogFile  = fs.String("catalog", "", "Path to catalog SQL file (plan/apply/check): CREATE PUBLICATION and CREATE EVENT TRIGGER statements diffed against live catalogs")
	)

	var hazards hazardFlags
	fs.Var(&hazards, "accept-hazard", "Hazard code to accept (can be repeated or comma-separated)")

	renames := renameFlags{}
	fs.Var(&renames, "rename", "Explicit column rename mapping old=new, optionally table-qualified table.old=new (experimental; can be repeated)")
	expandContract := fs.Bool("expand-contract", false, "Emit staged expand-and-contract (ZDM) plans; new columns are added alongside existing ones and drops are deferred to a separate contract plan (experimental)")

	if err := fs.Parse(args[1:]); err != nil {
		return 1
	}

	setupLogger(*jsonLog)

	ctx := context.Background()
	dsn := getDSN(*dsnFlag)

	switch command {
	case "init":
		return runInit(*templateFlag, *dirFlag, *forceFlag)
	case "plan":
		isGitHub := *githubOutput || *formatFlag == "github"
		isJSON := *jsonOutput || *formatFlag == "json"
		rolesSQL, ok := readRolesFile(*rolesFile)
		if !ok {
			return 1
		}
		catalogSQL, ok := readCatalogFile(*catalogFile)
		if !ok {
			return 1
		}
		return runPlan(ctx, dsn, *schemaFile, rolesSQL, catalogSQL, *outFile, *allowDrop, isJSON, isGitHub, renames, *expandContract)
	case "apply":
		rolesSQL, ok := readRolesFile(*rolesFile)
		if !ok {
			return 1
		}
		catalogSQL, ok := readCatalogFile(*catalogFile)
		if !ok {
			return 1
		}
		return runApply(ctx, dsn, *planFile, *schemaFile, rolesSQL, catalogSQL, *expectedHash, *allowDrop, hazards, *dryRunFlag, renames, *expandContract)
	case "check":
		rolesSQL, ok := readRolesFile(*rolesFile)
		if !ok {
			return 1
		}
		catalogSQL, ok := readCatalogFile(*catalogFile)
		if !ok {
			return 1
		}
		return runCheck(ctx, dsn, *schemaFile, rolesSQL, catalogSQL, *allowDrop, renames, *expandContract)
	case "lint":
		lintFormat := *formatFlag
		if lintFormat == "sql" {
			lintFormat = "text"
		}
		return runLint(ctx, dsn, *schemaFile, lintFormat, *failOnWarn)
	case "export":
		return runExport(ctx, dsn, *planFile, *schemaFile, *formatFlag, *versionFlag, *outFile, *allowDrop)
	case "seed":
		return runSeed(ctx, dsn, *seedFile, *forceFlag)
	default:
		slog.Error("unknown command", "command", command)
		fmt.Fprintf(os.Stderr, "Unknown command: %q. Expected plan, apply, check, lint, export, seed, or init.\n", command)
		return 1
	}
}

func initDB(dsn string) (*sql.DB, error) {
	if dsn == "" {
		slog.Error("database DSN is required (via --dsn, GRIZZLE_DSN, or DATABASE_URL)")
		return nil, errors.New("missing database DSN")
	}
	db, err := openDB(dsn)
	if err != nil {
		slog.Error("connecting to database", "target", redactDSN(dsn), "err", err)
		return nil, err
	}
	return db, nil
}

// readRolesFile loads the optional --roles file. A missing path yields empty
// content (roles management disabled); a read failure reports and fails.
func readRolesFile(path string) (string, bool) {
	return readOptionalSQLFile(path, "roles")
}

// readCatalogFile loads the optional --catalog file. A missing path yields
// empty content (catalog management disabled); a read failure reports and
// fails.
func readCatalogFile(path string) (string, bool) {
	return readOptionalSQLFile(path, "catalog")
}

// readOptionalSQLFile loads an optional side-channel SQL file. A missing
// path yields empty content (feature disabled); a read failure reports and
// fails.
func readOptionalSQLFile(path, label string) (string, bool) {
	if path == "" {
		return "", true
	}
	content, err := os.ReadFile(filepath.Clean(path)) //nolint:gosec // G304: CLI accepts user-provided SQL file path
	if err != nil {
		slog.Error("reading "+label+" file", "path", path, "err", err)
		return "", false
	}
	return string(content), true
}

func loadOrComputePlan(ctx context.Context, db *sql.DB, planFile, schemaFile, rolesSQL, catalogSQL string, allowDrop bool, acceptedCodes []grizzle.HazardCode, renames map[string]string, expandContract bool) (*grizzle.Plan, string, error) {
	if planFile != "" {
		planData, err := os.ReadFile(filepath.Clean(planFile)) //nolint:gosec // G304: CLI accepts user-provided plan file path
		if err != nil {
			slog.Error("reading plan file", "path", planFile, "err", err)
			return nil, "", err
		}
		parsedPlan, recordedHash, err := grizzle.ParsePlanJSON(planData)
		if err != nil {
			slog.Error("parsing plan JSON", "err", err)
			return nil, "", err
		}
		return parsedPlan, recordedHash, nil
	}

	content, err := os.ReadFile(filepath.Clean(schemaFile)) //nolint:gosec // G304: CLI accepts user-provided schema file path
	if err != nil {
		slog.Error("reading schema file", "path", schemaFile, "err", err)
		return nil, "", err
	}
	opts := grizzle.Options{
		SchemaSQL:      string(content),
		RolesSQL:       rolesSQL,
		CatalogSQL:     catalogSQL,
		AllowDrop:      allowDrop,
		AcceptHazards:  acceptedCodes,
		Renames:        renames,
		ExpandContract: expandContract,
	}
	computedPlan, err := grizzle.PlanDiff(ctx, db, opts)
	if err != nil {
		slog.Error("computing plan", "err", err)
		return nil, "", err
	}
	return computedPlan, computedPlan.Hash(), nil
}

func runPlan(ctx context.Context, dsn, schemaFile, rolesSQL, catalogSQL, outFile string, allowDrop, jsonOutput, githubOutput bool, renames map[string]string, expandContract bool) int {
	db, err := initDB(dsn)
	if err != nil {
		return 1
	}
	defer func() { _ = db.Close() }()

	content, err := os.ReadFile(filepath.Clean(schemaFile)) //nolint:gosec // G304: CLI accepts user-provided schema file path
	if err != nil {
		slog.Error("reading schema file", "path", schemaFile, "err", err)
		return 1
	}

	opts := grizzle.Options{
		SchemaSQL:      string(content),
		RolesSQL:       rolesSQL,
		CatalogSQL:     catalogSQL,
		AllowDrop:      allowDrop,
		Renames:        renames,
		ExpandContract: expandContract,
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

	if outFile != "" {
		if err := os.WriteFile(filepath.Clean(outFile), data, 0600); err != nil { //nolint:gosec // G304: CLI accepts user-provided destination path
			slog.Error("writing plan file", "path", outFile, "err", err)
			return 1
		}
		slog.Info("plan written successfully", "out", outFile, "hash", p.Hash(), "steps", len(p.Steps))
	} else if jsonOutput {
		fmt.Println(string(data))
	} else if githubOutput {
		_ = p.FormatGitHubActions(os.Stdout, schemaFile)
	} else {
		_ = p.Format(os.Stdout, true)
		fmt.Fprintf(os.Stderr, "Plan Hash: %s\n", p.Hash())
	}
	return 0
}

func runApply(ctx context.Context, dsn, planFile, schemaFile, rolesSQL, catalogSQL, expectedHash string, allowDrop bool, hazards []string, dryRun bool, renames map[string]string, expandContract bool) int {
	db, err := initDB(dsn)
	if err != nil {
		return 1
	}
	defer func() { _ = db.Close() }()

	var acceptedCodes []grizzle.HazardCode
	for _, h := range hazards {
		acceptedCodes = append(acceptedCodes, grizzle.HazardCode(h))
	}

	if dryRun {
		return runDryRunApply(ctx, db, planFile, schemaFile, rolesSQL, catalogSQL, allowDrop, acceptedCodes, renames, expandContract)
	}

	p, planHash, err := loadOrComputePlan(ctx, db, planFile, schemaFile, rolesSQL, catalogSQL, allowDrop, acceptedCodes, renames, expandContract)
	if err != nil {
		return 1
	}

	expHash := planHash
	if expectedHash != "" {
		expHash = expectedHash
	}

	if planFile == "" && isTerminalFunc(os.Stdin.Fd()) {
		if len(p.Steps) == 0 {
			fmt.Println("Planned changes:")
			fmt.Println("  No changes. Database schema is already in sync.")
			return 0
		}
		confirmed, promptErr := promptInteractiveApply(os.Stdin, os.Stdout, p)
		if promptErr != nil || !confirmed {
			return 1
		}
		for _, h := range p.Hazards() {
			acceptedCodes = append(acceptedCodes, h.Code)
		}
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
}

func runCheck(ctx context.Context, dsn, schemaFile, rolesSQL, catalogSQL string, allowDrop bool, renames map[string]string, expandContract bool) int {
	db, err := initDB(dsn)
	if err != nil {
		return 1
	}
	defer func() { _ = db.Close() }()

	content, err := os.ReadFile(filepath.Clean(schemaFile)) //nolint:gosec // G304: CLI accepts user-provided schema file path
	if err != nil {
		slog.Error("reading schema file", "path", schemaFile, "err", err)
		return 1
	}

	opts := grizzle.Options{
		SchemaSQL:      string(content),
		RolesSQL:       rolesSQL,
		CatalogSQL:     catalogSQL,
		AllowDrop:      allowDrop,
		Renames:        renames,
		ExpandContract: expandContract,
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
}

func runExport(ctx context.Context, dsn, planFile, schemaFile, format, versionFlag, outFile string, allowDrop bool) int {
	var p *grizzle.Plan
	if planFile != "" {
		planData, err := os.ReadFile(filepath.Clean(planFile)) //nolint:gosec // G304: CLI accepts user-provided plan file path
		if err != nil {
			slog.Error("reading plan file", "path", planFile, "err", err)
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
		db, err := initDB(dsn)
		if err != nil {
			return 1
		}
		defer func() { _ = db.Close() }()

		content, err := os.ReadFile(filepath.Clean(schemaFile)) //nolint:gosec // G304: CLI accepts user-provided schema file path
		if err != nil {
			slog.Error("reading schema file", "path", schemaFile, "err", err)
			return 1
		}
		opts := grizzle.Options{
			SchemaSQL: string(content),
			AllowDrop: allowDrop,
		}
		computedPlan, err := grizzle.PlanDiff(ctx, db, opts)
		if err != nil {
			slog.Error("computing plan for export", "err", err)
			return 1
		}
		p = computedPlan
	}

	artifacts, err := grizzle.Export(p, grizzle.ExportFormat(format), versionFlag)
	if err != nil {
		slog.Error("export failed", "err", err)
		return 1
	}

	outDir := outFile
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
		slog.Info("exported migration artifact", "path", targetPath, "format", format)
		fmt.Printf("Exported: %s\n", targetPath)
	}
	return 0
}
