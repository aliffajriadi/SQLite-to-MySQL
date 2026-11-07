// Package cli asks for the SQLite file and the MySQL connection.
package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-sql-driver/mysql"
)

// ResolveInputs uses the arguments or MYSQL_DSN when given and prompts for the rest.
func ResolveInputs(args []string, in *bufio.Reader) (string, *mysql.Config, error) {
	if len(args) > 2 {
		return "", nil, errors.New("usage: migrator [source.db] [dsn]")
	}
	var sqlitePath string
	var err error
	if len(args) > 0 {
		sqlitePath = args[0]
	} else if sqlitePath, err = askSQLitePath(in); err != nil {
		return "", nil, err
	}

	dsn := os.Getenv("MYSQL_DSN")
	if len(args) == 2 {
		dsn = args[1]
		fmt.Fprintln(os.Stderr, "Note: a password passed as an argument is visible in `ps` and shell history.")
	}
	if dsn == "" {
		cfg, err := askMySQL(in)
		return sqlitePath, cfg, err
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return "", nil, fmt.Errorf("invalid MySQL DSN: %w", err)
	}
	if cfg.DBName == "" {
		return "", nil, errors.New("the DSN must include a database name")
	}
	return sqlitePath, cfg, nil
}

func askSQLitePath(in *bufio.Reader) (string, error) {
	choices := findSQLiteFiles(".")
	printChoices(".", choices)
	for {
		answer, err := ask(in, "SQLite file (number, file or folder)", "")
		if err != nil {
			return "", err
		}
		path := cleanPath(answer)
		if n, err := strconv.Atoi(path); err == nil && n >= 1 && n <= len(choices) {
			return choices[n-1], nil
		}
		info, err := os.Stat(path)
		switch {
		case err != nil:
			fmt.Println("  Not found. Type a file path, or a folder to list its SQLite files.")
		case info.IsDir():
			choices = findSQLiteFiles(path)
			if len(choices) == 0 {
				fmt.Println("  No SQLite files in that folder.")
			}
			printChoices(path, choices)
		case isSQLite(path):
			return path, nil
		default:
			fmt.Println("  That file is not a SQLite database.")
		}
	}
}

// cleanPath undoes what a terminal adds: arrow key codes, quotes from dragging a file in,
// and ~, which only the shell expands.
func cleanPath(answer string) string {
	path := strings.TrimSpace(escapeCodes.ReplaceAllString(answer, ""))
	path = strings.Trim(path, `'"`)
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return path
}

var escapeCodes = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// findSQLiteFiles checks file contents, not extensions, and returns the newest first.
func findSQLiteFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	modified := map[string]time.Time{}
	var files []string
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		info, err := os.Stat(path)
		if err != nil || info.IsDir() || !isSQLite(path) {
			continue
		}
		modified[path] = info.ModTime()
		files = append(files, path)
	}
	sort.Slice(files, func(i, j int) bool { return modified[files[i]].After(modified[files[j]]) })
	return files
}

func isSQLite(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	header := make([]byte, len(sqliteHeader))
	_, err = io.ReadFull(file, header)
	return err == nil && string(header) == sqliteHeader
}

const sqliteHeader = "SQLite format 3\x00"

func printChoices(dir string, files []string) {
	if len(files) == 0 {
		return
	}
	fmt.Printf("SQLite files in %s:\n", dir)
	for i, path := range files {
		line := filepath.Base(path)
		if info, err := os.Stat(path); err == nil {
			line = fmt.Sprintf("%-45s %6.1f MB  %s", line, float64(info.Size())/(1<<20), info.ModTime().Format("2006-01-02"))
		}
		fmt.Printf("  %2d. %s\n", i+1, line)
	}
}

func askMySQL(in *bufio.Reader) (*mysql.Config, error) {
	host, err := ask(in, "MySQL host", "127.0.0.1")
	if err != nil {
		return nil, err
	}
	var port string
	for {
		if port, err = ask(in, "Port", "3306"); err != nil {
			return nil, err
		}
		if n, err := strconv.Atoi(port); err == nil && n > 0 && n < 65536 {
			break
		}
		fmt.Println("  Port must be a number from 1 to 65535.")
	}
	user, err := ask(in, "User", "root")
	if err != nil {
		return nil, err
	}
	password, err := askPassword(in)
	if err != nil {
		return nil, err
	}
	var database string
	for database == "" {
		if database, err = ask(in, "Database name", ""); err != nil {
			return nil, err
		}
	}
	cfg := mysql.NewConfig()
	cfg.Net = "tcp"
	cfg.Addr = net.JoinHostPort(host, port)
	cfg.User = user
	cfg.Passwd = password
	cfg.DBName = database
	return cfg, nil
}

func ask(in *bufio.Reader, label, fallback string) (string, error) {
	if fallback != "" {
		fmt.Printf("%s [%s]: ", label, fallback)
	} else {
		fmt.Printf("%s: ", label)
	}
	line, err := readLine(in)
	if err != nil {
		return "", err
	}
	if line = strings.TrimSpace(line); line != "" {
		return line, nil
	}
	return fallback, nil
}

func readLine(in *bufio.Reader) (string, error) {
	line, err := in.ReadString('\n')
	if err != nil && line == "" {
		return "", errors.New("input ended early")
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// askPassword turns off terminal echo. When stdin is a pipe, stty fails and there is nothing to hide.
func askPassword(in *bufio.Reader) (string, error) {
	fmt.Print("Password: ")
	if stty("-echo") == nil {
		// Without this, Ctrl+C here would leave the terminal with echo off.
		interrupted := make(chan os.Signal, 1)
		signal.Notify(interrupted, os.Interrupt, syscall.SIGTERM)
		done := make(chan struct{})
		go func() {
			select {
			case <-interrupted:
				stty("echo")
				fmt.Println()
				os.Exit(130)
			case <-done:
			}
		}()
		defer func() {
			signal.Stop(interrupted)
			close(done)
			stty("echo")
			fmt.Println()
		}()
	}
	return readLine(in)
}

func stty(mode string) error {
	cmd := exec.Command("stty", mode)
	cmd.Stdin = os.Stdin
	return cmd.Run()
}
