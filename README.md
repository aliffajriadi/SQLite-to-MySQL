# sqlite-mysql-migrator

Copy any SQLite database into MySQL or MariaDB with one command. Nothing is hard-coded: tables, columns, keys and column types are read from the SQLite file.

> **Status:** early. Unit tests cover the SQLite side, type mapping, SQL generation and checksums, but a full run against a live server has not been tested yet. Try it on an empty test database first.

## Install

You need Go 1.26+ and a C compiler, because the SQLite driver uses cgo.

```bash
go build -o migrator ./cmd/migrator
```

## Usage

Run it and answer the prompts. The password is not shown as you type.

```
$ ./migrator
SQLite file (number, file or folder): ~/data
SQLite files in /home/you/data:
   1. shop.db                                          3.4 MB  2026-08-16
   2. shop_backup.db                                   2.7 MB  2026-07-07
SQLite file (number, file or folder): 1
MySQL host [127.0.0.1]:
Port [3306]:
User [root]:
Password:
Database name: shop
```

The target database must already exist. To skip the prompts, for example in a script:

```bash
export MYSQL_DSN='user:password@tcp(host:3306)/database'
./migrator source.db
```

A DSN can also be passed as a second argument, but it will then be visible in `ps` and shell history.

## How it works

1. **Read** the structure from SQLite inside one read transaction, so everything comes from the same snapshot. The file is opened read-only.
2. **Check** MySQL. It stops before changing anything if a target table already has rows, or lacks a column that exists in SQLite.
3. **Prepare** tables. Missing tables are created with their indexes and foreign keys. Existing tables, such as ones made by Prisma, are used as they are.
4. **Copy and verify** in one transaction. Each column is read back from MySQL and compared by checksum before the commit.
5. **Sync** `AUTO_INCREMENT` from `sqlite_sequence`.

If a step fails, the inserted rows are rolled back and the tables created by that run are dropped.

## Type mapping

SQLite does not enforce column types, so the actual data is checked as well as the declared type.

| SQLite | MySQL | Falls back when |
|---|---|---|
| `INTEGER` | `BIGINT` | Decimals → `DOUBLE`, text → `LONGTEXT` |
| `REAL`, `FLOAT`, `DOUBLE`, `DECIMAL`, `NUMERIC` | `DOUBLE` | Integers beyond 2^53 → `BIGINT`, text → `LONGTEXT` |
| `BOOLEAN` | `TINYINT(1)` | Other numbers → `BIGINT`, text → `LONGTEXT` |
| `DATE`, `DATETIME`, `TIMESTAMP` | `DATETIME(6)` | Unreadable dates keep their stored form |
| `TEXT`, `VARCHAR`, `CLOB` | `LONGTEXT` | Invalid UTF-8 → `LONGBLOB` |
| `BLOB` | `LONGBLOB` | |

Text and blob columns used in a key become `VARCHAR` with `utf8mb4_bin`, or `VARBINARY`, because MySQL cannot index `LONGTEXT` without a prefix length and SQLite comparisons are case-sensitive.

Dates can be text, Unix seconds, Unix milliseconds (as written by Prisma) or Julian days, and are stored as UTC.

## Limitations

These are not migrated, and each one is listed as a warning at the end of the run:

- Views, triggers and `CHECK` constraints
- Partial indexes and expression indexes
- Virtual tables such as FTS
- `DEFAULT` expressions with no MySQL equivalent
- Foreign keys using `SET DEFAULT`

Generated columns are copied as regular columns. One run handles one SQLite file, and target tables must be empty, so use a separate database for each file whose table names overlap.

## Project layout

```
cmd/migrator/       Entry point and overall flow
internal/
  cli/              Interactive prompts
  schema/           Reading the SQLite structure and rows
  mysqldb/          Choosing MySQL types, checking and creating tables
  migrate/          Copying rows and verifying them
  cell/             Converting and comparing single values
  report/           Warnings for the final summary
```

Run the tests with `go test ./...`.
