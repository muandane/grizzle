// Package main implements the grizzle CLI.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
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
	"github.com/muandane/grizzle/internal/config"
	"github.com/muandane/grizzle/internal/dialect"
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
	version = "v0.1.0-rc4"
	commit  = "none"
	date    = "unknown"
)

func run(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: grizzle <plan|apply|check|lint|export|seed|init|history|status|baseline|repair|dump|version> [flags]")
		return 1
	}

	command := args[0]
	if command == "version" || command == "--version" || command == "-version" || command == "-v" {
		fmt.Printf("grizzle %s (commit: %s, date: %s)\n", version, commit, date)
		return 0
	}

	fs := flag.NewFlagSet(command, flag.ContinueOnError)

	var (
		dsnFlag        = fs.String("dsn", "", "Database DSN connection string")
		schemaFile     = fs.String("schema", "schema.sql", "Path to schema SQL file")
		planFile       = fs.String("plan", "", "Path to plan JSON file")
		outFile        = fs.String("out", "", "Destination file or directory")
		formatFlag     = fs.String("format", "sql", "Output/export format: sql, goose, atlas, or github")
		allowDrop      = fs.Bool("allow-drop", false, "Permit destructive operations")
		jsonOutput     = fs.Bool("json", false, "Output in JSON format")
		githubOutput   = fs.Bool("github", false, "Emit GitHub Actions workflow annotations")
		jsonLog        = fs.Bool("json-log", false, "Emit logs in structured JSON format")
		expectedHash   = fs.String("expected-hash", "", "Expected plan approval hash")
		versionFlag    = fs.String("version", version, "Version string for export headers")
		templateFlag   = fs.String("template", "", "Project template: sqlc, stdlib, or sqlite")
		dirFlag        = fs.String("dir", ".", "Destination directory for init")
		forceFlag      = fs.Bool("force", false, "Overwrite existing files during init, or force baseline")
		failOnWarn     = fs.Bool("fail-on-warning", false, "Fail lint when warnings are reported")
		dryRunFlag     = fs.Bool("dry-run", false, "Verify planned DDL against live data without persisting changes (apply)")
		seedFile       = fs.String("seed", "", "Path to seed SQL file (seed)")
		rolesFile      = fs.String("roles", "", "Path to roles SQL file (plan/apply/check): CREATE ROLE and GRANT statements diffed against live ACLs")
		catalogFile    = fs.String("catalog", "", "Path to catalog SQL file (plan/apply/check): CREATE PUBLICATION and CREATE EVENT TRIGGER statements diffed against live catalogs")
		targetSchema   = fs.String("target-schema", "", "Target database schema name")
		limitFlag      = fs.Int("limit", 20, "Maximum number of history records to list (history)")
		noteFlag       = fs.String("note", "", "Operator note for baseline or repair")
		configFlag     = fs.String("config", "", "Path to grizzle.yaml or grizzle.json configuration file")
		beforeSyncFlag = fs.String("before-sync", "", "SQL script or file to execute immediately before migration")
		afterSyncFlag  = fs.String("after-sync", "", "SQL script or file to execute immediately after migration")
	)

	var hazards hazardFlags
	fs.Var(&hazards, "accept-hazard", "Hazard code to accept (can be repeated or comma-separated)")

	renames := renameFlags{}
	fs.Var(&renames, "rename", "Explicit column rename mapping old=new, optionally table-qualified table.old=new (can be repeated)")
	expandContract := fs.Bool("expand-contract", false, "Emit staged expand-and-contract (ZDM) plans; new columns are added alongside existing ones and drops are deferred to a separate contract plan")
	var backfill backfillFlag
	fs.Var(&backfill, "backfill", "Backfill strategy during expand-contract (default copy when flag present); use --backfill, --backfill=copy, or --backfill copy")
	backfillFile := fs.String("backfill-file", "", "SQL file for expand-contract backfill with {table}, {old}, {new}, {batch} placeholders")
	backfillBatch := fs.Int("backfill-batch", 1000, "Rows per expand-contract backfill batch (default 1000)")

	if err := fs.Parse(normalizeBackfillArgs(args[1:])); err != nil {
		return 1
	}

	// Load configuration file if present
	configFile := config.FindConfigFile(*configFlag)
	if configFile != "" {
		cfg, err := config.Load(configFile)
		if err != nil {
			slog.Error("loading config file", "path", configFile, "err", err)
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}
		if *dsnFlag == "" && cfg.DSN != "" {
			*dsnFlag = cfg.DSN
		}
		if *schemaFile == "schema.sql" && cfg.Schema != "" {
			*schemaFile = cfg.Schema
		}
		if *targetSchema == "" && cfg.TargetSchema != "" {
			*targetSchema = cfg.TargetSchema
		}
		if *rolesFile == "" && cfg.Roles != "" {
			*rolesFile = cfg.Roles
		}
		if *catalogFile == "" && cfg.Catalog != "" {
			*catalogFile = cfg.Catalog
		}
		if *seedFile == "" && cfg.Seed != "" {
			*seedFile = cfg.Seed
		}
		if !*allowDrop && cfg.AllowDrop {
			*allowDrop = true
		}
		if len(hazards) == 0 && len(cfg.AcceptHazards) > 0 {
			for _, h := range cfg.AcceptHazards {
				hazards = append(hazards, h)
			}
		}
		if *beforeSyncFlag == "" && cfg.BeforeSync != "" {
			*beforeSyncFlag = cfg.BeforeSync
		}
		if *afterSyncFlag == "" && cfg.AfterSync != "" {
			*afterSyncFlag = cfg.AfterSync
		}
		if !*failOnWarn && cfg.Lint.FailOnWarning {
			*failOnWarn = true
		}
	}

	setupLogger(*jsonLog)

	ctx := context.Background()
	dsn := getDSN(*dsnFlag)

	backfillHook, err := resolveBackfillHook(*expandContract, backfill, *backfillFile, *backfillBatch, dsn)
	if err != nil {
		slog.Error("invalid backfill flags", "err", err)
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	beforeHook, err := resolveSyncHook(*beforeSyncFlag)
	if err != nil {
		slog.Error("invalid --before-sync hook", "err", err)
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	afterHook, err := resolveSyncHook(*afterSyncFlag)
	if err != nil {
		slog.Error("invalid --after-sync hook", "err", err)
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

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
		return runPlan(ctx, dsn, *schemaFile, rolesSQL, catalogSQL, *outFile, *allowDrop, isJSON, isGitHub, renames, *expandContract, backfillHook)
	case "apply":
		rolesSQL, ok := readRolesFile(*rolesFile)
		if !ok {
			return 1
		}
		catalogSQL, ok := readCatalogFile(*catalogFile)
		if !ok {
			return 1
		}
		return runApply(ctx, dsn, *planFile, *schemaFile, rolesSQL, catalogSQL, *expectedHash, *allowDrop, hazards, *dryRunFlag, renames, *expandContract, backfillHook, beforeHook, afterHook)
	case "check":
		rolesSQL, ok := readRolesFile(*rolesFile)
		if !ok {
			return 1
		}
		catalogSQL, ok := readCatalogFile(*catalogFile)
		if !ok {
			return 1
		}
		return runCheck(ctx, dsn, *schemaFile, rolesSQL, catalogSQL, *allowDrop, renames, *expandContract, backfillHook)
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
	case "history":
		return runHistory(ctx, dsn, *targetSchema, *limitFlag, *jsonOutput)
	case "status":
		return runStatus(ctx, dsn, *schemaFile, *targetSchema, *jsonOutput)
	case "baseline":
		return runBaseline(ctx, dsn, *schemaFile, *targetSchema, *noteFlag, *forceFlag)
	case "repair":
		return runRepair(ctx, dsn, *targetSchema, *noteFlag)
	case "dump":
		return runDump(ctx, dsn, *targetSchema, *outFile)
	default:
		slog.Error("unknown command", "command", command)
		fmt.Fprintf(os.Stderr, "Unknown command: %q. Expected plan, apply, check, lint, export, seed, init, history, status, baseline, repair, or dump.\n", command)
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

func loadOrComputePlan(ctx context.Context, db *sql.DB, planFile, schemaFile, rolesSQL, catalogSQL string, allowDrop bool, acceptedCodes []grizzle.HazardCode, renames map[string]string, expandContract bool, backfill grizzle.BackfillFunc) (*grizzle.Plan, string, error) {
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
		Backfill:       backfill,
	}
	computedPlan, err := grizzle.PlanDiff(ctx, db, opts)
	if err != nil {
		slog.Error("computing plan", "err", err)
		return nil, "", err
	}
	return computedPlan, computedPlan.Hash(), nil
}

func runPlan(ctx context.Context, dsn, schemaFile, rolesSQL, catalogSQL, outFile string, allowDrop, jsonOutput, githubOutput bool, renames map[string]string, expandContract bool, backfill grizzle.BackfillFunc) int {
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
		Backfill:       backfill,
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

func resolveSyncHook(scriptOrPath string) (grizzle.SyncHook, error) {
	if scriptOrPath == "" {
		return nil, nil
	}
	content := scriptOrPath
	if _, err := os.Stat(filepath.Clean(scriptOrPath)); err == nil { //nolint:gosec // G304: CLI accepts user-provided script file path
		data, err := os.ReadFile(filepath.Clean(scriptOrPath)) //nolint:gosec // G304: CLI accepts user-provided script file path
		if err != nil {
			return nil, fmt.Errorf("reading sync hook file %q: %w", scriptOrPath, err)
		}
		content = string(data)
	}
	return func(ctx context.Context, dbtx dialect.DBTX) error {
		_, err := dbtx.ExecContext(ctx, content)
		return err
	}, nil
}

func runApply(ctx context.Context, dsn, planFile, schemaFile, rolesSQL, catalogSQL, expectedHash string, allowDrop bool, hazards []string, dryRun bool, renames map[string]string, expandContract bool, backfill grizzle.BackfillFunc, hooks ...grizzle.SyncHook) int {
	var beforeSync, afterSync grizzle.SyncHook
	if len(hooks) > 0 {
		beforeSync = hooks[0]
	}
	if len(hooks) > 1 {
		afterSync = hooks[1]
	}
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
		return runDryRunApply(ctx, db, planFile, schemaFile, rolesSQL, catalogSQL, allowDrop, acceptedCodes, renames, expandContract, backfill)
	}

	p, planHash, err := loadOrComputePlan(ctx, db, planFile, schemaFile, rolesSQL, catalogSQL, allowDrop, acceptedCodes, renames, expandContract, backfill)
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
		Backfill:      backfill,
		BeforeSync:    beforeSync,
		AfterSync:     afterSync,
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

func runCheck(ctx context.Context, dsn, schemaFile, rolesSQL, catalogSQL string, allowDrop bool, renames map[string]string, expandContract bool, backfill grizzle.BackfillFunc) int {
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
		Backfill:       backfill,
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

func runHistory(ctx context.Context, dsn, targetSchema string, limit int, jsonOutput bool) int {
	db, err := initDB(dsn)
	if err != nil {
		return 1
	}
	defer func() { _ = db.Close() }()

	entries, err := grizzle.ListHistory(ctx, db, grizzle.HistoryOptions{
		TargetSchema: targetSchema,
		Limit:        limit,
		Descending:   true,
	})
	if err != nil {
		slog.Error("listing migration history", "err", err)
		return 1
	}

	if jsonOutput {
		data, err := json.MarshalIndent(entries, "", "  ")
		if err != nil {
			slog.Error("serializing history entries to json", "err", err)
			return 1
		}
		fmt.Println(string(data))
		return 0
	}

	if len(entries) == 0 {
		fmt.Println("No migration history found.")
		return 0
	}

	fmt.Printf("%-5s %-9s %-16s %-10s %-20s %-12s %s\n", "ID", "STATUS", "PLAN HASH", "DURATION", "APPLIED AT", "APPLIED BY", "NOTE/ERROR")
	fmt.Println(strings.Repeat("-", 85))
	for _, e := range entries {
		hashShort := e.PlanHash
		if len(hashShort) > 16 {
			hashShort = hashShort[:16]
		}
		dur := fmt.Sprintf("%dms", e.DurationMs)
		appliedAt := e.AppliedAt.Format("2006-01-02 15:04:05")
		noteOrErr := e.Error
		if noteOrErr == "" {
			noteOrErr = "-"
		}
		fmt.Printf("%-5d %-9s %-16s %-10s %-20s %-12s %s\n", e.ID, e.Status, hashShort, dur, appliedAt, e.AppliedBy, noteOrErr)
	}
	return 0
}

func runStatus(ctx context.Context, dsn, schemaFile, targetSchema string, jsonOutput bool) int {
	db, err := initDB(dsn)
	if err != nil {
		return 1
	}
	defer func() { _ = db.Close() }()

	var schemaSQL string
	if schemaFile != "" {
		if content, err := os.ReadFile(filepath.Clean(schemaFile)); err == nil { //nolint:gosec // G304: CLI accepts user-provided schema file path
			schemaSQL = string(content)
		} else if schemaFile != "schema.sql" {
			slog.Error("reading schema file for status", "path", schemaFile, "err", err)
			return 1
		}
	}

	stat, err := grizzle.Status(ctx, db, grizzle.Options{
		TargetSchema: targetSchema,
		SchemaSQL:    schemaSQL,
	})
	if err != nil {
		slog.Error("getting database migration status", "err", err)
		return 1
	}

	if jsonOutput {
		data, err := json.MarshalIndent(stat, "", "  ")
		if err != nil {
			slog.Error("serializing status report to json", "err", err)
			return 1
		}
		fmt.Println(string(data))
		if !stat.InSync {
			return 1
		}
		return 0
	}

	fmt.Println("Database Status:")
	fmt.Printf("  Dialect:       %s\n", stat.Dialect)
	if stat.TargetSchema != "" {
		fmt.Printf("  Target Schema: %s\n", stat.TargetSchema)
	}
	if schemaSQL != "" {
		if stat.InSync {
			fmt.Printf("  Sync State:    In Sync (0 pending steps)\n")
		} else {
			fmt.Printf("  Sync State:    Drift Detected (%d pending step(s))\n", stat.PendingSteps)
		}
	}
	fmt.Printf("  History:       %d applied, %d partial, %d failed\n", stat.AppliedCount, stat.PartialCount, stat.FailedCount)
	if stat.Latest != nil {
		fmt.Printf("  Latest Run:    #%d [%s] (hash: %s, at %s)\n",
			stat.Latest.ID, stat.Latest.Status, stat.Latest.PlanHash, stat.Latest.AppliedAt.Format("2006-01-02 15:04:05"))
		if stat.Latest.Error != "" {
			fmt.Printf("                 Note/Error: %s\n", stat.Latest.Error)
		}
	}

	if schemaSQL != "" && !stat.InSync {
		return 1
	}
	return 0
}

func runBaseline(ctx context.Context, dsn, schemaFile, targetSchema, note string, force bool) int {
	db, err := initDB(dsn)
	if err != nil {
		return 1
	}
	defer func() { _ = db.Close() }()

	content, err := os.ReadFile(filepath.Clean(schemaFile)) //nolint:gosec // G304: CLI accepts user-provided schema file path
	if err != nil {
		slog.Error("reading schema file for baseline", "path", schemaFile, "err", err)
		return 1
	}

	entry, err := grizzle.Baseline(ctx, db, string(content), grizzle.BaselineOptions{
		TargetSchema: targetSchema,
		Note:         note,
		Force:        force,
	})
	if err != nil {
		if errors.Is(err, grizzle.ErrBaselineDrift) {
			slog.Error("baseline rejected: live database differs from desired schema", "err", err)
			fmt.Fprintf(os.Stderr, "Error: live database differs from schema file %q; use --force to baseline anyway.\n", schemaFile)
			return 1
		}
		slog.Error("recording baseline failed", "err", err)
		return 1
	}

	fmt.Printf("Baseline recorded successfully: entry #%d (hash %s) marked as baseline.\n", entry.ID, entry.PlanHash)
	return 0
}

func runRepair(ctx context.Context, dsn, targetSchema, note string) int {
	db, err := initDB(dsn)
	if err != nil {
		return 1
	}
	defer func() { _ = db.Close() }()

	entry, err := grizzle.Repair(ctx, db, grizzle.RepairOptions{
		TargetSchema: targetSchema,
		Note:         note,
	})
	if err != nil {
		if errors.Is(err, grizzle.ErrNoFailedMigration) {
			fmt.Println("No failed or partial migrations found to repair.")
			return 0
		}
		slog.Error("repairing migration history failed", "err", err)
		return 1
	}

	fmt.Printf("Migration #%d (hash %s) marked as resolved.\n", entry.ID, entry.PlanHash)
	return 0
}

func runDump(ctx context.Context, dsn, targetSchema, outFile string) int {
	db, err := initDB(dsn)
	if err != nil {
		return 1
	}
	defer func() { _ = db.Close() }()

	ddl, err := grizzle.Dump(ctx, db, grizzle.DumpOptions{
		TargetSchema: targetSchema,
	})
	if err != nil {
		slog.Error("dumping database schema failed", "err", err)
		return 1
	}

	if outFile != "" {
		if err := os.WriteFile(filepath.Clean(outFile), []byte(ddl), 0600); err != nil { //nolint:gosec // G304: CLI writes to user-specified destination path
			slog.Error("writing dumped schema file", "path", outFile, "err", err)
			return 1
		}
		slog.Info("schema dumped successfully", "out", outFile)
		fmt.Printf("Schema dumped to %s\n", outFile)
	} else {
		fmt.Print(ddl)
	}
	return 0
}
