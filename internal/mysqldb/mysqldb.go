// Package mysqldb checks and prepares the MySQL side of a migration.
package mysqldb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"sqlite-mysql-migrator/internal/cell"
	"sqlite-mysql-migrator/internal/report"
	"sqlite-mysql-migrator/internal/schema"
)

// Open returns one pinned connection, because SET SESSION only applies to that connection.
func Open(ctx context.Context, cfg *mysql.Config) (*sql.DB, *sql.Conn, error) {
	cfg.ParseTime = false    // verification reads dates back as plain text
	cfg.Loc = time.UTC       // SQLite times are copied as UTC
	cfg.MaxAllowedPacket = 0 // follow the server limit
	cfg.MultiStatements = false

	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, nil, err
	}
	db := sql.OpenDB(connector)
	conn, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	if _, err := conn.ExecContext(ctx, `SET SESSION
		sql_mode = 'STRICT_ALL_TABLES,NO_AUTO_VALUE_ON_ZERO,NO_ENGINE_SUBSTITUTION',
		time_zone = '+00:00',
		foreign_key_checks = 1`); err != nil {
		conn.Close()
		db.Close()
		return nil, nil, err
	}
	return db, conn, nil
}

// CheckTargets changes nothing and returns the tables missing from MySQL.
func CheckTargets(ctx context.Context, conn *sql.Conn, tables []schema.Table) (map[string]bool, error) {
	missing := map[string]bool{}
	var problems []string
	for _, t := range tables {
		columns, err := mysqlColumns(ctx, conn, t.Name)
		if err != nil {
			return nil, fmt.Errorf("check %s: %w", t.Name, err)
		}
		if len(columns) == 0 {
			missing[t.Name] = true
			fmt.Printf("  + %-30s will be created\n", t.Name)
			continue
		}
		var count int64
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+schema.Quote(t.Name)).Scan(&count); err != nil {
			return nil, fmt.Errorf("count %s: %w", t.Name, err)
		}
		if count > 0 {
			problems = append(problems, fmt.Sprintf("table %s already has %d rows", t.Name, count))
		}
		for _, c := range t.Columns {
			if _, ok := columns[strings.ToLower(c.Name)]; !ok {
				problems = append(problems, fmt.Sprintf("column %s.%s is missing in MySQL (its data would be lost)", t.Name, c.Name))
			}
		}
		fmt.Printf("  = %-30s exists\n", t.Name)
	}
	if len(problems) > 0 {
		return nil, errors.New("stopped, MySQL was not changed:\n  - " + strings.Join(problems, "\n  - "))
	}
	return missing, nil
}

func mysqlColumns(ctx context.Context, conn *sql.Conn, table string) (map[string]cell.Target, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT COLUMN_NAME, DATA_TYPE, DATETIME_PRECISION
		FROM INFORMATION_SCHEMA.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := map[string]cell.Target{}
	for rows.Next() {
		var name, dataType string
		var precision sql.NullInt64
		if err := rows.Scan(&name, &dataType, &precision); err != nil {
			return nil, err
		}
		columns[strings.ToLower(name)] = cell.Classify(dataType, int(precision.Int64))
	}
	return columns, rows.Err()
}

// LoadTargets reads the real MySQL column types of a table into t.Targets.
func LoadTargets(ctx context.Context, conn *sql.Conn, t *schema.Table) error {
	columns, err := mysqlColumns(ctx, conn, t.Name)
	if err != nil {
		return fmt.Errorf("read columns of %s: %w", t.Name, err)
	}
	t.Targets = make([]cell.Target, len(t.Columns))
	for i, c := range t.Columns {
		t.Targets[i] = columns[strings.ToLower(c.Name)]
	}
	return nil
}

// CreateTables creates the missing tables, then their indexes and foreign keys.
func CreateTables(ctx context.Context, conn *sql.Conn, lite *sql.Tx, tables []schema.Table, missing map[string]bool, created *[]string) error {
	for _, t := range tables {
		if !missing[t.Name] {
			continue
		}
		profiles, err := profileTable(lite, t)
		if err != nil {
			return fmt.Errorf("read data of %s: %w", t.Name, err)
		}
		for i, c := range t.Columns {
			columnType := mysqlType(c, profiles[i], t.KeyColumns[strings.ToLower(c.Name)])
			if columnType == "LONGTEXT" && c.Type != "" && !isTextType(c.Type) {
				report.Warn("column %s.%s is declared %s but holds other values; using LONGTEXT", t.Name, c.Name, c.Type)
			}
		}

		query, skipped := createTableSQL(t, profiles, true)
		if _, err := conn.ExecContext(ctx, query); err != nil {
			// MySQL can reject a DEFAULT that is valid in SQLite; the data matters more.
			retry, _ := createTableSQL(t, profiles, false)
			if _, retryErr := conn.ExecContext(ctx, retry); retryErr != nil {
				return fmt.Errorf("create table %s: %w", t.Name, err)
			}
			report.Warn("DEFAULT values of table %s were rejected by MySQL and skipped: %v", t.Name, err)
		} else if len(skipped) > 0 {
			report.Warn("DEFAULT values of table %s were not migrated: %s", t.Name, strings.Join(skipped, ", "))
		}
		*created = append(*created, t.Name)
		fmt.Printf("  + %s\n", t.Name)

		for _, index := range t.Indexes {
			if _, err := conn.ExecContext(ctx, indexSQL(t, index)); err != nil {
				report.Warn("index %s (%s) skipped: %v", index.Name, t.Name, err)
			}
		}
	}

	// Foreign keys come last, once every parent table exists.
	for _, t := range tables {
		if !missing[t.Name] {
			continue
		}
		for _, fk := range t.ForeignKeys {
			if fk.OnDelete == "SET DEFAULT" || fk.OnUpdate == "SET DEFAULT" {
				report.Warn("foreign key %s -> %s skipped: InnoDB does not support SET DEFAULT", t.Name, fk.RefTable)
				continue
			}
			if _, err := conn.ExecContext(ctx, foreignKeySQL(t, fk)); err != nil {
				report.Warn("foreign key %s -> %s skipped: %v", t.Name, fk.RefTable, err)
			}
		}
	}
	return nil
}

// DropTables removes the tables a failed run created.
func DropTables(ctx context.Context, conn *sql.Conn, created []string) {
	if len(created) == 0 {
		return
	}
	if _, err := conn.ExecContext(ctx, "SET SESSION foreign_key_checks = 0"); err != nil {
		fmt.Fprintln(os.Stderr, "Could not clean up tables:", err)
	}
	for _, name := range created {
		if _, err := conn.ExecContext(ctx, "DROP TABLE "+schema.Quote(name)); err != nil {
			fmt.Fprintf(os.Stderr, "Could not drop table %s: %v\n", name, err)
		}
	}
	fmt.Fprintf(os.Stderr, "✓ Dropped the %d tables created by this run.\n", len(created))
}

// SyncAutoIncrement continues each counter where SQLite left off.
func SyncAutoIncrement(ctx context.Context, conn *sql.Conn, tables []schema.Table) {
	for _, t := range tables {
		if t.NextID == 0 {
			continue
		}
		query := fmt.Sprintf("ALTER TABLE %s AUTO_INCREMENT = %d", schema.Quote(t.Name), t.NextID)
		if _, err := conn.ExecContext(ctx, query); err != nil {
			report.Warn("AUTO_INCREMENT %s: %v", t.Name, err)
		}
	}
}
