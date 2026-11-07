// Package schematest provides a shared SQLite fixture for tests.
package schematest

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"sqlite-mysql-migrator/internal/report"
	"sqlite-mysql-migrator/internal/schema"
)

const nastySchema = `
CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT);
CREATE TABLE users (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL DEFAULT 'it''s',
  email VARCHAR(50) UNIQUE,
  active BOOLEAN DEFAULT TRUE,
  balance DECIMAL(10,2) DEFAULT 0,
  big NUMERIC,
  created DATETIME DEFAULT CURRENT_TIMESTAMP,
  messy DATETIME,
  age INTEGER CHECK (age > 0),
  weird INTEGER,
  photo BLOB,
  anything,
  upper_name TEXT GENERATED ALWAYS AS (upper(name)) VIRTUAL
);
CREATE TABLE memberships (
  user_id INTEGER, team TEXT, role TEXT DEFAULT (lower('X')),
  PRIMARY KEY (team, user_id),
  FOREIGN KEY (user_id) REFERENCES users ON DELETE CASCADE,
  FOREIGN KEY (team) REFERENCES settings (key) ON DELETE SET DEFAULT
);
CREATE INDEX idx_name ON users (name);
CREATE INDEX idx_partial ON users (age) WHERE age > 1;
CREATE INDEX idx_expr ON users (lower(name));
CREATE VIEW v AS SELECT id FROM users;
CREATE TRIGGER trg AFTER INSERT ON users BEGIN SELECT 1; END;
INSERT INTO settings VALUES ('abc', '1'), ('ABC', '2');
INSERT INTO users (name, email, active, balance, big, created, messy, age, weird, photo, anything) VALUES
  ('a', 'x@y', 1, 10.5, 9007199254740993, 1700000000123, 'yesterday', 5, 'not a number', x'00ff', 7),
  ('b', NULL, 0, 3, 1, '2024-01-02 03:04:05', NULL, 6, 1, NULL, 'teks');
INSERT INTO memberships (user_id, team) VALUES (1, 'abc'), (99, 'abc');
DELETE FROM users WHERE id = 2;
INSERT INTO users (name, age) VALUES ('c', 7);
DELETE FROM users WHERE name = 'c';
`

// Fixture opens a database full of awkward cases and reads its structure.
func Fixture(t *testing.T) (*sql.Tx, []schema.Table) {
	t.Helper()
	report.Reset()
	// Spaces, ? and # are deliberate: such names used to break the SQLite URI.
	path := filepath.Join(t.TempDir(), "my file?#.db")
	rw, err := sql.Open("sqlite3", "file:"+strings.NewReplacer("?", "%3F", "#", "%23").Replace(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rw.Exec(nastySchema); err != nil {
		t.Fatal(err)
	}
	rw.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}

	db, err := schema.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	lite, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lite.Rollback() })
	tables, err := schema.Read(lite)
	if err != nil {
		t.Fatal(err)
	}
	return lite, tables
}

// Find returns the table with the given name.
func Find(t *testing.T, tables []schema.Table, name string) schema.Table {
	t.Helper()
	for _, table := range tables {
		if table.Name == name {
			return table
		}
	}
	t.Fatalf("table %s was not read", name)
	return schema.Table{}
}

// MustContain fails the test unless s contains every part.
func MustContain(t *testing.T, s string, parts ...string) {
	t.Helper()
	for _, part := range parts {
		if !strings.Contains(s, part) {
			t.Errorf("missing %q in:\n%s", part, s)
		}
	}
}
