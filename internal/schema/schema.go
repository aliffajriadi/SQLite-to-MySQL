// Package schema reads the structure and rows of a SQLite database.
package schema

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"sqlite-mysql-migrator/internal/cell"
	"sqlite-mysql-migrator/internal/report"
)

type Column struct {
	Name    string
	Type    string
	NotNull bool
	Default sql.NullString
	PK      int // position in the primary key, 0 = not part of it
}

type Index struct {
	Name    string
	Unique  bool
	Columns []string
}

type ForeignKey struct {
	Columns    []string
	RefTable   string
	RefColumns []string // empty = the primary key of the parent
	OnUpdate   string
	OnDelete   string
}

type Table struct {
	Name        string
	Columns     []Column
	Indexes     []Index
	ForeignKeys []ForeignKey
	KeyColumns  map[string]bool // lowercase names of columns used by a PK, index or FK
	Rows        int64
	NextID      int64         // from sqlite_sequence, 0 = none
	Targets     []cell.Target // MySQL column types, parallel to Columns
}

// Open opens a SQLite file read-only.
func Open(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(abs); err != nil {
		return nil, fmt.Errorf("SQLite file: %w", err)
	} else if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory, not a SQLite file", abs)
	}
	// url.URL escapes spaces, ? and # in the file name.
	uri := url.URL{Scheme: "file", Path: abs, RawQuery: "mode=ro&_busy_timeout=5000"}
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		return nil, fmt.Errorf("SQLite: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("SQLite: %w", err)
	}
	return db, nil
}

// Read returns the structure of every regular table in the database.
func Read(lite *sql.Tx) ([]Table, error) {
	type entry struct{ name, kind, ddl string }
	var entries []entry
	err := eachRow(lite, `
		SELECT l.name, l.type, COALESCE(m.sql, '')
		FROM pragma_table_list l
		LEFT JOIN sqlite_master m ON m.name = l.name
		WHERE l.schema = 'main' AND substr(l.name, 1, 7) != 'sqlite_'
		ORDER BY l.name`, nil, func(r *sql.Rows) error {
		var e entry
		err := r.Scan(&e.name, &e.kind, &e.ddl)
		entries = append(entries, e)
		return err
	})
	if err != nil {
		return nil, err
	}

	checkRe := regexp.MustCompile(`(?i)\bCHECK\s*\(`)
	var tables []Table
	for _, e := range entries {
		switch e.kind {
		case "view":
			report.Warn("view %s was not migrated", e.name)
		case "virtual":
			report.Warn("virtual table %s was not migrated (e.g. FTS)", e.name)
		case "table":
			t := Table{Name: e.name, KeyColumns: map[string]bool{}}
			if t.Columns, err = readColumns(lite, e.name); err != nil {
				return nil, err
			}
			if t.Indexes, err = readIndexes(lite, e.name); err != nil {
				return nil, err
			}
			if t.ForeignKeys, err = readForeignKeys(lite, e.name); err != nil {
				return nil, err
			}
			if checkRe.MatchString(e.ddl) {
				report.Warn("CHECK constraints on table %s were not migrated", e.name)
			}
			tables = append(tables, t)
		}
	}

	err = eachRow(lite, "SELECT name FROM sqlite_master WHERE type = 'trigger'", nil, func(r *sql.Rows) error {
		var name string
		err := r.Scan(&name)
		report.Warn("trigger %s was not migrated", name)
		return err
	})
	if err != nil {
		return nil, err
	}

	var orphans int64
	if err := lite.QueryRow("SELECT COUNT(*) FROM pragma_foreign_key_check").Scan(&orphans); err != nil {
		report.Warn("could not check SQLite foreign keys: %v", err)
	} else if orphans > 0 {
		report.Warn("%d rows violate foreign keys in SQLite; copied as they are", orphans)
	}

	markKeyColumns(tables)
	return tables, readSequences(lite, tables)
}

func readColumns(lite *sql.Tx, table string) ([]Column, error) {
	var columns []Column
	// table_xinfo also lists generated columns, which are copied as regular columns.
	err := eachRow(lite, `SELECT name, type, "notnull", dflt_value, pk, hidden FROM pragma_table_xinfo(?) ORDER BY cid`,
		[]any{table}, func(r *sql.Rows) error {
			var c Column
			var hidden int
			if err := r.Scan(&c.Name, &c.Type, &c.NotNull, &c.Default, &c.PK, &hidden); err != nil {
				return err
			}
			if hidden == 2 || hidden == 3 {
				report.Warn("generated column %s.%s became a regular column", table, c.Name)
			}
			if hidden != 1 {
				columns = append(columns, c)
			}
			return nil
		})
	return columns, err
}

func readIndexes(lite *sql.Tx, table string) ([]Index, error) {
	type entry struct {
		name, origin    string
		unique, partial bool
	}
	var entries []entry
	err := eachRow(lite, `SELECT name, "unique", origin, partial FROM pragma_index_list(?) ORDER BY seq DESC`,
		[]any{table}, func(r *sql.Rows) error {
			var e entry
			err := r.Scan(&e.name, &e.unique, &e.origin, &e.partial)
			entries = append(entries, e)
			return err
		})
	if err != nil {
		return nil, err
	}

	var indexes []Index
	for n, e := range entries {
		if e.origin == "pk" {
			continue
		}
		if e.partial {
			report.Warn("partial index %s (%s) was not migrated", e.name, table)
			continue
		}
		index := Index{Name: e.name, Unique: e.unique}
		expression := false
		err := eachRow(lite, "SELECT name FROM pragma_index_info(?) ORDER BY seqno", []any{e.name}, func(r *sql.Rows) error {
			var column sql.NullString
			err := r.Scan(&column)
			expression = expression || !column.Valid
			index.Columns = append(index.Columns, column.String)
			return err
		})
		if err != nil {
			return nil, err
		}
		if expression {
			report.Warn("expression index %s (%s) was not migrated", e.name, table)
			continue
		}
		// sqlite_autoindex_* names mean nothing, and MySQL index names only need to be unique per table.
		if e.origin != "c" || len(e.name) > 64 {
			index.Name = fmt.Sprintf("idx_%d", n+1)
		}
		indexes = append(indexes, index)
	}
	return indexes, nil
}

func readForeignKeys(lite *sql.Tx, table string) ([]ForeignKey, error) {
	byID := map[int]*ForeignKey{}
	var order []int
	err := eachRow(lite, `SELECT id, "table", "from", "to", on_update, on_delete FROM pragma_foreign_key_list(?) ORDER BY id, seq`,
		[]any{table}, func(r *sql.Rows) error {
			var id int
			var parent, from, onUpdate, onDelete string
			var to sql.NullString
			if err := r.Scan(&id, &parent, &from, &to, &onUpdate, &onDelete); err != nil {
				return err
			}
			fk := byID[id]
			if fk == nil {
				fk = &ForeignKey{RefTable: parent, OnUpdate: onUpdate, OnDelete: onDelete}
				byID[id] = fk
				order = append(order, id)
			}
			fk.Columns = append(fk.Columns, from)
			if to.Valid {
				fk.RefColumns = append(fk.RefColumns, to.String)
			}
			return nil
		})
	foreignKeys := make([]ForeignKey, 0, len(order))
	for _, id := range order {
		foreignKeys = append(foreignKeys, *byID[id])
	}
	return foreignKeys, err
}

func readSequences(lite *sql.Tx, tables []Table) error {
	var exists bool
	if err := lite.QueryRow("SELECT COUNT(*) > 0 FROM sqlite_master WHERE name = 'sqlite_sequence'").Scan(&exists); err != nil || !exists {
		return err
	}
	return eachRow(lite, "SELECT name, seq FROM sqlite_sequence", nil, func(r *sql.Rows) error {
		var name string
		var seq int64
		if err := r.Scan(&name, &seq); err != nil {
			return err
		}
		for i := range tables {
			if strings.EqualFold(tables[i].Name, name) {
				tables[i].NextID = seq + 1
			}
		}
		return nil
	})
}

// markKeyColumns flags the columns that need an indexable MySQL type,
// including parent columns referenced by foreign keys.
func markKeyColumns(tables []Table) {
	byName := map[string]*Table{}
	for i := range tables {
		byName[strings.ToLower(tables[i].Name)] = &tables[i]
	}
	for i := range tables {
		t := &tables[i]
		for _, c := range t.Columns {
			if c.PK > 0 {
				t.KeyColumns[strings.ToLower(c.Name)] = true
			}
		}
		for _, index := range t.Indexes {
			for _, c := range index.Columns {
				t.KeyColumns[strings.ToLower(c)] = true
			}
		}
		for j := range t.ForeignKeys {
			fk := &t.ForeignKeys[j]
			for _, c := range fk.Columns {
				t.KeyColumns[strings.ToLower(c)] = true
			}
			parent := byName[strings.ToLower(fk.RefTable)]
			if parent == nil {
				continue
			}
			if len(fk.RefColumns) == 0 {
				fk.RefColumns = PrimaryKey(*parent)
			}
			for _, c := range fk.RefColumns {
				parent.KeyColumns[strings.ToLower(c)] = true
			}
		}
	}
}

// PrimaryKey returns the primary key columns in key order.
func PrimaryKey(t Table) []string {
	columns := make([]string, 0, 1)
	for pos := 1; ; pos++ {
		found := false
		for _, c := range t.Columns {
			if c.PK == pos {
				columns = append(columns, c.Name)
				found = true
			}
		}
		if !found {
			return columns
		}
	}
}

// CountRows fills Table.Rows and returns the total.
func CountRows(lite *sql.Tx, tables []Table) (int64, error) {
	var total int64
	for i := range tables {
		t := &tables[i]
		if err := lite.QueryRow("SELECT COUNT(*) FROM " + Quote(t.Name)).Scan(&t.Rows); err != nil {
			return 0, fmt.Errorf("count %s: %w", t.Name, err)
		}
		total += t.Rows
		fmt.Printf("  %-30s %d rows\n", t.Name, t.Rows)
	}
	return total, nil
}

// EachSourceRow reads raw values. The unary + stops the driver from guessing a type from the
// column declaration; otherwise an unreadable date silently becomes 0001-01-01.
func EachSourceRow(lite *sql.Tx, t Table, fn func([]any) error) error {
	selects := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		selects[i] = "+" + Quote(c.Name)
	}
	values, pointers := ScanBuffers(len(t.Columns))
	return eachRow(lite, "SELECT "+strings.Join(selects, ", ")+" FROM "+Quote(t.Name), nil, func(r *sql.Rows) error {
		if err := r.Scan(pointers...); err != nil {
			return err
		}
		return fn(values)
	})
}

// ScanBuffers returns a row buffer and the pointers that rows.Scan needs to fill it.
func ScanBuffers(n int) (values, pointers []any) {
	values = make([]any, n)
	pointers = make([]any, n)
	for i := range values {
		pointers[i] = &values[i]
	}
	return values, pointers
}

func eachRow(lite *sql.Tx, query string, args []any, fn func(*sql.Rows) error) error {
	rows, err := lite.Query(query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Quote works for both SQLite and MySQL, which both accept backticks.
func Quote(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// QuoteList quotes names and joins them with commas.
func QuoteList(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = Quote(name)
	}
	return strings.Join(quoted, ", ")
}
