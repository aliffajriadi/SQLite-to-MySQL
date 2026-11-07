package schema_test

import (
	"strings"
	"testing"

	"sqlite-mysql-migrator/internal/report"
	"sqlite-mysql-migrator/internal/schema"
	"sqlite-mysql-migrator/internal/schema/schematest"
)

func TestSchemaIsReadDynamically(t *testing.T) {
	_, tables := schematest.Fixture(t)
	if len(tables) != 3 {
		t.Fatalf("want 3 tables, got %d", len(tables))
	}
	users := schematest.Find(t, tables, "users")
	if users.NextID != 4 {
		t.Errorf("want NextID 4 from sqlite_sequence, got %d", users.NextID)
	}
	if len(users.Columns) != 13 {
		t.Errorf("generated column must be included: %d columns", len(users.Columns))
	}
	memberships := schematest.Find(t, tables, "memberships")
	if got := schema.PrimaryKey(memberships); strings.Join(got, ",") != "team,user_id" {
		t.Errorf("wrong composite PK order: %v", got)
	}
	for _, fk := range memberships.ForeignKeys {
		if fk.RefTable == "users" && strings.Join(fk.RefColumns, ",") != "id" {
			t.Errorf("FK without columns must point at the parent PK: %v", fk.RefColumns)
		}
	}
	all := strings.Join(report.All(), "\n")
	schematest.MustContain(t, all, "view v", "trigger trg", "partial index idx_partial", "expression index idx_expr",
		"generated column users.upper_name", "CHECK constraints on table users", "violate foreign keys")
}

func TestSourceRowsAreRaw(t *testing.T) {
	lite, tables := schematest.Fixture(t)
	users := schematest.Find(t, tables, "users")
	var row map[string]any
	err := schema.EachSourceRow(lite, users, func(values []any) error {
		if row == nil {
			row = map[string]any{}
			for i, c := range users.Columns {
				row[c.Name] = values[i]
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if row["messy"] != "yesterday" {
		t.Errorf("a bad date must stay text: %#v", row["messy"])
	}
	if row["created"] != int64(1700000000123) {
		t.Errorf("created must be raw: %#v", row["created"])
	}
	if row["big"] != int64(9007199254740993) {
		t.Errorf("big number changed: %#v", row["big"])
	}
	if b, ok := row["photo"].([]byte); !ok || len(b) != 2 || b[1] != 0xff {
		t.Errorf("blob changed: %#v", row["photo"])
	}
}
