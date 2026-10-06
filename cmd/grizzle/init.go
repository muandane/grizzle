package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/muandane/grizzle"
)

type templateFile struct {
	Name    string
	Content string
}

func getTemplateFiles(template string) []templateFile {
	switch template {
	case "sqlc":
		return []templateFile{
			{
				Name: "schema.sql",
				Content: `-- schema.sql: Single source of truth for database schema
CREATE TABLE users (
    id BIGSERIAL PRIMARY KEY,
    email TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
`,
			},
			{
				Name: "queries.sql",
				Content: `-- queries.sql: Type-safe SQL queries compiled by sqlc
-- name: GetUser :one
SELECT id, email, name, created_at FROM users
WHERE id = $1 LIMIT 1;

-- name: ListUsers :many
SELECT id, email, name, created_at FROM users
ORDER BY created_at DESC;

-- name: CreateUser :one
INSERT INTO users (email, name)
VALUES ($1, $2)
RETURNING id, email, name, created_at;
`,
			},
			{
				Name: "sqlc.yaml",
				Content: `version: "2"
sql:
  - engine: "postgresql"
    schema: "schema.sql"
    queries: "queries.sql"
    gen:
      go:
        package: "db"
        out: "db"
        sql_package: "pgx/v5"
`,
			},
			{
				Name: "main.go",
				Content: `package main

import (
	"context"
	"database/sql"
	_ "embed"
	"log"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/muandane/grizzle"
)

//go:embed schema.sql
var schemaSQL string

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres:postgres@localhost:5432/myapp?sslmode=disable"
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Fatalf("failed opening database: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := grizzle.Sync(ctx, db, grizzle.Options{
		SchemaSQL: schemaSQL,
		AllowDrop: false,
	}); err != nil {
		log.Fatalf("grizzle schema sync failed: %v", err)
	}

	log.Println("Database schema synchronized successfully with Grizzle.")
}
`,
			},
		}
	case "stdlib":
		return []templateFile{
			{
				Name: "schema.sql",
				Content: `-- schema.sql: Single source of truth for database schema
CREATE TABLE users (
    id BIGSERIAL PRIMARY KEY,
    email TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
`,
			},
			{
				Name: "main.go",
				Content: `package main

import (
	"context"
	"database/sql"
	_ "embed"
	"log"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/muandane/grizzle"
)

//go:embed schema.sql
var schemaSQL string

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres:postgres@localhost:5432/myapp?sslmode=disable"
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Fatalf("failed opening database: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := grizzle.Sync(ctx, db, grizzle.Options{
		SchemaSQL: schemaSQL,
		AllowDrop: false,
	}); err != nil {
		log.Fatalf("grizzle schema sync failed: %v", err)
	}

	log.Println("Database schema synchronized successfully with Grizzle.")
}
`,
			},
		}
	case "sqlite":
		return []templateFile{
			{
				Name: "schema.sql",
				Content: `-- schema.sql: Single source of truth for SQLite database schema
CREATE TABLE users (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    email TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
`,
			},
			{
				Name: "main.go",
				Content: `package main

import (
	"context"
	"database/sql"
	_ "embed"
	"log"

	"github.com/muandane/grizzle"
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

func main() {
	db, err := sql.Open("sqlite", "app.db")
	if err != nil {
		log.Fatalf("failed opening database: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := grizzle.Sync(ctx, db, grizzle.Options{
		Dialect:   grizzle.DialectSQLite,
		SchemaSQL: schemaSQL,
		AllowDrop: false,
	}); err != nil {
		log.Fatalf("grizzle schema sync failed: %v", err)
	}

	log.Println("SQLite database synchronized successfully with Grizzle.")
}
`,
			},
		}
	default:
		return nil
	}
}

func promptInteractiveInit(in io.Reader, out io.Writer) (string, error) {
	_, _ = fmt.Fprintln(out, "Select a project template:")
	_, _ = fmt.Fprintln(out, "  1) sqlc   - Type-safe queries with sqlc + PostgreSQL (recommended)")
	_, _ = fmt.Fprintln(out, "  2) stdlib - Standard database/sql + PostgreSQL")
	_, _ = fmt.Fprintln(out, "  3) sqlite - Embedded pure-Go SQLite")
	_, _ = fmt.Fprint(out, "Choose template [1-3] (default 1): ")
	reader := bufio.NewReader(in)
	line, err := readPromptLine(reader)
	if err != nil {
		return "", err
	}
	switch strings.ToLower(line) {
	case "", "1", "sqlc":
		return "sqlc", nil
	case "2", "stdlib":
		return "stdlib", nil
	case "3", "sqlite":
		return "sqlite", nil
	default:
		return "", fmt.Errorf("unknown template choice %q. Expected sqlc, stdlib, or sqlite", line)
	}
}

func runInit(template, dir string, force bool) int {
	if template == "" {
		if isTerminalFunc(os.Stdin.Fd()) {
			chosen, err := promptInteractiveInit(os.Stdin, os.Stdout)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Initialization error: %v\n", err)
				return 1
			}
			template = chosen
		} else {
			fmt.Fprintln(os.Stderr, "Error: --template is required in non-interactive environments (options: sqlc, stdlib, sqlite)")
			return 1
		}
	}

	files := getTemplateFiles(template)
	if files == nil {
		fmt.Fprintf(os.Stderr, "Unknown template %q. Supported templates: sqlc, stdlib, sqlite\n", template)
		return 1
	}

	if dir == "" {
		dir = "."
	}
	cleanDir := filepath.Clean(dir)
	if err := os.MkdirAll(cleanDir, 0750); err != nil { //nolint:gosec // G703, G301: CLI creates user-specified init directory
		fmt.Fprintf(os.Stderr, "Failed to create directory %q: %v\n", cleanDir, err)
		return 1
	}

	if !force {
		for _, f := range files {
			targetPath := filepath.Join(cleanDir, f.Name)
			if _, err := os.Stat(targetPath); err == nil { //nolint:gosec // G703: checking file existence in user directory
				fmt.Fprintf(os.Stderr, "Error: file %q already exists (use --force to overwrite)\n", targetPath)
				return 1
			}
		}
	}

	for _, f := range files {
		targetPath := filepath.Join(cleanDir, f.Name)
		if err := os.WriteFile(targetPath, []byte(f.Content), 0600); err != nil { //nolint:gosec // G304: CLI writes template file to user directory
			fmt.Fprintf(os.Stderr, "Failed writing file %q: %v\n", targetPath, err)
			return 1
		}
	}

	fmt.Printf("Initialized Grizzle project with template %q in %s\n", template, cleanDir)
	for _, f := range files {
		fmt.Printf("  Created %s\n", filepath.Join(cleanDir, f.Name))
	}
	return 0
}

func outputGitHubActions(w io.Writer, p *grizzle.Plan, schemaFile string) {
	summary := fmt.Sprintf("Adds: %d, Alters: %d, Drops: %d (Plan Hash: %s)", p.Additions(), p.Modifications(), p.Deletions(), p.Hash())
	_, _ = fmt.Fprintf(w, "::notice title=Grizzle Migration Plan::%s\n", summary)
	for _, h := range p.Hazards() {
		switch h.Level {
		case grizzle.HazardLevelCritical:
			_, _ = fmt.Fprintf(w, "::error file=%s,title=Critical Hazard (%s)::%s\n", schemaFile, h.Code, h.Description)
		case grizzle.HazardLevelWarning:
			_, _ = fmt.Fprintf(w, "::warning file=%s,title=Hazard Warning (%s)::%s\n", schemaFile, h.Code, h.Description)
		case grizzle.HazardLevelNotice:
			_, _ = fmt.Fprintf(w, "::notice file=%s,title=Hazard Notice (%s)::%s\n", schemaFile, h.Code, h.Description)
		}
	}
}
