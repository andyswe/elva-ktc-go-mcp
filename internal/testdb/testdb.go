// Package testdb gives each test package its own schema in the test PostgreSQL.
package testdb

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// URL returns ELVA_TEST_DATABASE_URL pointed at its own, freshly emptied schema, so
// test packages running in parallel don't share a measurement table. It skips the
// test when the variable is unset. Never point it at a database with real data.
func URL(t testing.TB, schema string) string {
	t.Helper()
	url := os.Getenv("ELVA_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ELVA_TEST_DATABASE_URL not set; skipping PostgreSQL test")
	}
	conn, err := pgx.Connect(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	id := pgx.Identifier{schema}.Sanitize()
	if _, err := conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+id+" CASCADE; CREATE SCHEMA "+id); err != nil {
		t.Fatal(err)
	}
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	return url + sep + "search_path=" + schema // pgx passes unknown URL parameters as session settings
}
