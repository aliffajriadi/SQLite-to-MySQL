// Command migrator copies any SQLite database into MySQL or MariaDB.
//
// Nothing is hard-coded: tables, columns, keys and column types all come from
// the SQLite file. See README.md for the steps and their guarantees.
package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"sqlite-mysql-migrator/internal/cli"
	"sqlite-mysql-migrator/internal/migrate"
	"sqlite-mysql-migrator/internal/mysqldb"
	"sqlite-mysql-migrator/internal/report"
	"sqlite-mysql-migrator/internal/schema"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "\nMIGRATION FAILED:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	sqlitePath, cfg, err := cli.ResolveInputs(args, bufio.NewReader(os.Stdin))
	if err != nil {
		return err
	}
	fmt.Println("\nSQLite :", sqlitePath)
	fmt.Printf("MySQL  : %s@%s(%s)/%s\n\n", cfg.User, cfg.Net, cfg.Addr, cfg.DBName)

	ctx := context.Background()

	liteDB, err := schema.Open(sqlitePath)
	if err != nil {
		return err
	}
	defer liteDB.Close()

	// One read transaction is one snapshot: all counts and rows describe the same state.
	lite, err := liteDB.Begin()
	if err != nil {
		return fmt.Errorf("SQLite: %w", err)
	}
	defer lite.Rollback()

	myDB, conn, err := mysqldb.Open(ctx, cfg)
	if err != nil {
		return fmt.Errorf("MySQL: %w", err)
	}
	defer myDB.Close()
	defer conn.Close()

	fmt.Println("[1/5] Reading the SQLite structure...")
	tables, err := schema.Read(lite)
	if err != nil {
		return fmt.Errorf("read SQLite structure: %w", err)
	}
	if len(tables) == 0 {
		return errors.New("the SQLite file has no tables")
	}
	totalRows, err := schema.CountRows(lite, tables)
	if err != nil {
		return err
	}

	fmt.Println("\n[2/5] Checking MySQL...")
	missing, err := mysqldb.CheckTargets(ctx, conn, tables)
	if err != nil {
		return err
	}

	// All checks passed. MySQL is changed from here on, so failures must clean up.
	var created []string
	var tx *sql.Tx
	finished := false
	defer func() {
		if finished {
			return
		}
		if tx != nil {
			if err := tx.Rollback(); err != nil {
				fmt.Fprintln(os.Stderr, "Rollback error:", err)
			} else {
				fmt.Fprintln(os.Stderr, "✓ Inserted rows were rolled back.")
			}
		}
		mysqldb.DropTables(ctx, conn, created)
	}()

	fmt.Println("\n[3/5] Preparing MySQL tables...")
	if err := mysqldb.CreateTables(ctx, conn, lite, tables, missing, &created); err != nil {
		return err
	}
	for i := range tables {
		if err := mysqldb.LoadTargets(ctx, conn, &tables[i]); err != nil {
			return err
		}
	}

	// Table order, circular foreign keys and orphaned SQLite rows must not stop the copy.
	if _, err := conn.ExecContext(ctx, "SET SESSION foreign_key_checks = 0"); err != nil {
		return fmt.Errorf("MySQL: %w", err)
	}
	if tx, err = conn.BeginTx(ctx, nil); err != nil {
		return fmt.Errorf("MySQL: %w", err)
	}

	fmt.Println("\n[4/5] Copying and verifying data...")
	for _, t := range tables {
		fmt.Printf("  %-30s %d rows... ", t.Name, t.Rows)
		sums, err := migrate.CopyTable(lite, tx, t)
		if err != nil {
			fmt.Println()
			return fmt.Errorf("table %s: %w", t.Name, err)
		}
		if err := migrate.VerifyTable(tx, t, sums); err != nil {
			fmt.Println()
			return fmt.Errorf("verify table %s: %w", t.Name, err)
		}
		fmt.Println("OK ✓")
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	finished = true

	fmt.Println("\n[5/5] Syncing AUTO_INCREMENT...")
	mysqldb.SyncAutoIncrement(ctx, conn, tables)

	printSummary(len(tables), totalRows)
	return nil
}

func printSummary(tableCount int, rowCount int64) {
	fmt.Println("\n======================================")
	fmt.Println("       MIGRATION SUCCEEDED")
	fmt.Println("======================================")
	fmt.Printf("Tables : %d\nRows   : %d\n", tableCount, rowCount)
	if len(report.All()) > 0 {
		fmt.Printf("\n%d warnings. The data is safe, but these items were skipped or changed:\n", len(report.All()))
		for _, w := range report.All() {
			fmt.Println("  -", w)
		}
	}
}
