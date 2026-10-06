// Package griztest provides testing utilities and ergonomic primitives
// for testing applications powered by Grizzle schema migrations.
//
// # Overview
//
// griztest simplifies test setup and teardown by exposing helpers that automatically
// apply schemas, verify execution plans, and reset state using test-friendly defaults
// (such as AllowDrop: true).
//
// # Usage
//
//	func TestMain(m *testing.M) {
//	    db := setupTestDB()
//	    defer db.Close()
//	    os.Exit(m.Run())
//	}
//
//	func TestUsers(t *testing.T) {
//	    db := openDB(t)
//	    griztest.MustSync(t, db, schemaSQL)
//	    // ... test queries against clean schema ...
//	}
package griztest
