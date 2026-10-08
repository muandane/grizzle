package sqlite_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/muandane/grizzle/internal/dialect/sqlite"
	_ "modernc.org/sqlite"
)

func TestAttachMemorySchemas_CompileRoundTrip(t *testing.T) {
	ctx := t.Context()
	desired := `
		CREATE TABLE main.users (id INTEGER PRIMARY KEY);
		CREATE TABLE aux.accounts (id INTEGER PRIMARY KEY, user_id INTEGER);
	`
	m, err := sqlite.CompileInShadowSchemas(ctx, desired, []string{"main", "aux"})
	if err != nil {
		t.Fatalf("CompileInShadowSchemas: %v", err)
	}
	if _, ok := m["main"].Tables["users"]; !ok {
		t.Fatal("expected users in main")
	}
	if _, ok := m["aux"].Tables["accounts"]; !ok {
		t.Fatal("expected accounts in aux")
	}
}

func TestAttachDatabases_LiveFiles(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	auxPath := filepath.Join(dir, "aux.db")

	aux, err := sql.Open("sqlite", auxPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := aux.Exec(`CREATE TABLE items (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	_ = aux.Close()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	if err := sqlite.AttachDatabases(ctx, conn, map[string]string{"aux": auxPath}); err != nil {
		t.Fatalf("AttachDatabases: %v", err)
	}
	s, err := sqlite.InspectSchema(ctx, conn, "aux")
	if err != nil {
		t.Fatalf("InspectSchema: %v", err)
	}
	if _, ok := s.Tables["items"]; !ok {
		t.Fatal("expected items in aux")
	}
	if err := sqlite.DetachDatabases(ctx, conn, []string{"aux"}); err != nil {
		t.Fatalf("DetachDatabases: %v", err)
	}
}
