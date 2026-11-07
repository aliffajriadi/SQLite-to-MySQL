package mysqldb

import (
	"database/sql"
	"strings"
	"testing"

	"sqlite-mysql-migrator/internal/schema"
	"sqlite-mysql-migrator/internal/schema/schematest"
)

func createSQL(t *testing.T, lite *sql.Tx, table schema.Table) string {
	t.Helper()
	profiles, err := profileTable(lite, table)
	if err != nil {
		t.Fatal(err)
	}
	query, _ := createTableSQL(table, profiles, true)
	return query
}

func TestCreateTableSQL(t *testing.T) {
	lite, tables := schematest.Fixture(t)

	schematest.MustContain(t, createSQL(t, lite, schematest.Find(t, tables, "settings")),
		"`key` VARCHAR(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL", "PRIMARY KEY (`key`)")

	schematest.MustContain(t, createSQL(t, lite, schematest.Find(t, tables, "memberships")),
		"`user_id` BIGINT NOT NULL", "`team` VARCHAR(191)", "PRIMARY KEY (`team`, `user_id`)")

	users := createSQL(t, lite, schematest.Find(t, tables, "users"))
	schematest.MustContain(t, users,
		"`id` BIGINT NOT NULL AUTO_INCREMENT",
		"`name` VARCHAR(191) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT 'it''s'",
		"`active` TINYINT(1) NULL DEFAULT 1",
		"`balance` DOUBLE NULL DEFAULT 0",
		"`big` BIGINT NULL",
		"`created` DATETIME(6) NULL DEFAULT CURRENT_TIMESTAMP(6)",
		"`messy` LONGTEXT NULL",
		"`age` BIGINT NULL",
		"`weird` LONGTEXT NULL",
		"`photo` LONGBLOB NULL",
		"`anything` BIGINT NULL")
	if strings.Count(users, "PRIMARY KEY") != 1 {
		t.Error(users)
	}
}

func TestMySQLTypeFollowsData(t *testing.T) {
	cases := []struct {
		declared string
		values   []any
		key      bool
		want     string
	}{
		{"INTEGER", nil, false, "BIGINT"},
		{"INTEGER", []any{int64(1), 1.5}, false, "DOUBLE"},
		{"BOOLEAN", []any{int64(500)}, false, "BIGINT"},
		{"BOOLEAN", []any{"ya"}, false, "LONGTEXT"},
		{"TEXT", []any{"\xff\xfe"}, false, "LONGBLOB"},
		{"TEXT", nil, false, "LONGTEXT"},
		{"TEXT", []any{strings.Repeat("é", 300)}, true, "VARCHAR(300) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin"},
		{"TEXT", []any{strings.Repeat("x", 20000)}, true, "LONGTEXT"},
		{"BLOB", []any{[]byte{1}}, true, "VARBINARY(191)"},
		{"NUMERIC", []any{int64(1 << 60), 1.5}, false, "LONGTEXT"},
		{"", []any{int64(1)}, false, "BIGINT"},
		{"", nil, false, "LONGTEXT"},
		{"DATE", []any{"2024-01-02"}, false, "DATETIME(6)"},
		{"TIME", []any{"12:30:00"}, false, "LONGTEXT"},
		{"DATETIME", []any{int64(1700000000000)}, false, "DATETIME(6)"},
		{"DATETIME", []any{int64(1 << 62)}, false, "BIGINT"},
	}
	for _, c := range cases {
		var p profile
		for _, v := range c.values {
			p.observe(v, isDateType(c.declared))
		}
		if got := mysqlType(schema.Column{Type: c.declared}, p, c.key); got != c.want {
			t.Errorf("%q %v: got %s, want %s", c.declared, c.values, got, c.want)
		}
	}
}

func TestDefaultClause(t *testing.T) {
	cases := []struct {
		raw, columnType, want string
		ok                    bool
	}{
		{"'a\\b'", "VARCHAR(191)", ` DEFAULT 'a\\b'`, true},
		{"'x'", "LONGTEXT", " DEFAULT ('x')", true},
		{"-1.5", "DOUBLE", " DEFAULT -1.5", true},
		{"false", "TINYINT(1)", " DEFAULT 0", true},
		{"CURRENT_TIMESTAMP", "LONGTEXT", "", false},
		{"lower('X')", "LONGTEXT", "", false},
		{"1); DROP TABLE x; --", "BIGINT", "", false},
	}
	for _, c := range cases {
		column := schema.Column{Default: sql.NullString{String: c.raw, Valid: true}}
		got, ok := defaultClause(column, c.columnType)
		if got != c.want || ok != c.ok {
			t.Errorf("%q: got %q %v", c.raw, got, ok)
		}
	}
}
