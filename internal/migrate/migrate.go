// Package migrate copies rows into MySQL and verifies them.
package migrate

import (
	"database/sql"
	"fmt"
	"strings"

	"sqlite-mysql-migrator/internal/cell"
	"sqlite-mysql-migrator/internal/schema"
)

const (
	maxBatchRows  = 1000
	maxBatchBytes = 512 << 10 // stays below max_allowed_packet on older servers
	maxParams     = 65535     // MySQL prepared statement placeholder limit
)

// CopyTable inserts every row in batches and returns a checksum per column.
func CopyTable(lite *sql.Tx, tx *sql.Tx, t schema.Table) ([]uint64, error) {
	names := columnNames(t)
	rowPlaceholder := "(" + strings.TrimSuffix(strings.Repeat("?, ", len(names)), ", ") + ")"
	batchLimit := min(maxBatchRows, maxParams/len(names))

	var batch []any
	rows, size := 0, 0
	flush := func() error {
		if rows == 0 {
			return nil
		}
		query := fmt.Sprintf("INSERT INTO %s (%s) VALUES %s", schema.Quote(t.Name), schema.QuoteList(names),
			strings.TrimSuffix(strings.Repeat(rowPlaceholder+", ", rows), ", "))
		_, err := tx.Exec(query, batch...)
		batch, rows, size = nil, 0, 0
		return err
	}

	sums := make([]uint64, len(t.Columns))
	err := schema.EachSourceRow(lite, t, func(values []any) error {
		for i, value := range values {
			converted, err := cell.Convert(value, t.Targets[i])
			if err != nil {
				return fmt.Errorf("column %s: %w", t.Columns[i].Name, err)
			}
			sums[i] += cell.Hash(converted, t.Targets[i])
			batch = append(batch, converted)
			if s, ok := converted.(string); ok {
				size += len(s)
			} else if b, ok := converted.([]byte); ok {
				size += len(b)
			}
		}
		rows++
		if rows >= batchLimit || size >= maxBatchBytes {
			return flush()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return sums, flush()
}

// VerifyTable reads the rows back from MySQL and compares each column checksum with the source.
func VerifyTable(tx *sql.Tx, t schema.Table, expected []uint64) error {
	names := columnNames(t)
	values, pointers := schema.ScanBuffers(len(names))
	rows, err := tx.Query("SELECT " + schema.QuoteList(names) + " FROM " + schema.Quote(t.Name))
	if err != nil {
		return err
	}
	defer rows.Close()

	var count int64
	sums := make([]uint64, len(names))
	for rows.Next() {
		if err := rows.Scan(pointers...); err != nil {
			return err
		}
		for i, value := range values {
			sums[i] += cell.Hash(value, t.Targets[i])
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if count != t.Rows {
		return fmt.Errorf("row count differs: SQLite %d, MySQL %d", t.Rows, count)
	}
	var different []string
	for i := range sums {
		if sums[i] != expected[i] {
			different = append(different, names[i])
		}
	}
	if len(different) > 0 {
		return fmt.Errorf("column contents changed in MySQL: %s", strings.Join(different, ", "))
	}
	return nil
}

func columnNames(t schema.Table) []string {
	names := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		names[i] = c.Name
	}
	return names
}
