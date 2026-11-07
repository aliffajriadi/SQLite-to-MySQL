package cli

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func TestInteractiveInput(t *testing.T) {
	t.Setenv("MYSQL_DSN", "")
	file := filepath.Join(t.TempDir(), "data.db")
	if err := os.WriteFile(file, []byte(sqliteHeader), 0o600); err != nil {
		t.Fatal(err)
	}
	// Lines: wrong file, quoted file, default host, bad port, good port,
	// default user, password with spaces and @, empty database, good database.
	input := "/no/such.db\n'" + file + "'\n\nabc\n3307\n\n p@ss:/word \n\ntoko\n"
	path, cfg, err := ResolveInputs(nil, bufio.NewReader(strings.NewReader(input)))
	if err != nil {
		t.Fatal(err)
	}
	if path != file {
		t.Errorf("path: %q", path)
	}
	if cfg.Addr != "127.0.0.1:3307" || cfg.User != "root" || cfg.Passwd != " p@ss:/word " || cfg.DBName != "toko" {
		t.Errorf("%+v", cfg)
	}
	// An unusual password must survive the round trip through a DSN.
	parsed, err := mysql.ParseDSN(cfg.FormatDSN())
	if err != nil || parsed.Passwd != cfg.Passwd {
		t.Errorf("%v %q", err, parsed.Passwd)
	}
}

func TestInputSources(t *testing.T) {
	t.Setenv("MYSQL_DSN", "u:p@tcp(h:1)/env")
	path, cfg, err := ResolveInputs([]string{"x.db"}, bufio.NewReader(strings.NewReader("")))
	if err != nil || path != "x.db" || cfg.DBName != "env" {
		t.Errorf("%v %v %+v", err, path, cfg)
	}
	if _, cfg, err = ResolveInputs([]string{"x.db", "u:p@tcp(h:1)/arg"}, nil); err != nil || cfg.DBName != "arg" {
		t.Errorf("%v %+v", err, cfg)
	}
	if _, _, err = ResolveInputs([]string{"x.db", "u:p@tcp(h:1)/"}, nil); err == nil {
		t.Error("a DSN without a database must be rejected")
	}
	if _, _, err = ResolveInputs([]string{"a", "b", "c"}, nil); err == nil {
		t.Error("extra arguments must be rejected")
	}

	// Running out of input must not loop forever.
	t.Setenv("MYSQL_DSN", "")
	if _, _, err = ResolveInputs(nil, bufio.NewReader(strings.NewReader(""))); err == nil {
		t.Error("EOF must be an error")
	}
}

func TestPickFileFromFolder(t *testing.T) {
	t.Chdir(t.TempDir())
	dir := t.TempDir()
	older := filepath.Join(dir, "older.bak")
	newer := filepath.Join(dir, "newer.db")
	for _, path := range []string{older, newer} {
		if err := os.WriteFile(path, []byte(sqliteHeader+"data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.db"), []byte("not a database, just some text"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(older, time.Time{}, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	// Lines: number with no list yet, folder with arrow key codes, number out of range, valid number.
	input := "1\n" + dir + "/\x1b[D\x1b[C\n9\n2\n"
	path, err := askSQLitePath(bufio.NewReader(strings.NewReader(input)))
	if err != nil {
		t.Fatal(err)
	}
	if path != older {
		t.Errorf("got %q, want the second newest file %q", path, older)
	}

	// A non-database file is refused, then the input runs out.
	if _, err := askSQLitePath(bufio.NewReader(strings.NewReader(filepath.Join(dir, "notes.db") + "\n"))); err == nil {
		t.Error("a file without the SQLite header must not be accepted")
	}
}
