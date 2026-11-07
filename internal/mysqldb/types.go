package mysqldb

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"sqlite-mysql-migrator/internal/cell"
	"sqlite-mysql-migrator/internal/schema"
)

const (
	keyTextLen    = 191   // minimum length of a text column used as a key
	maxKeyTextLen = 16000 // a longer utf8mb4 VARCHAR does not fit in one row
)

// profile summarizes the values actually stored, because SQLite does not enforce column types.
type profile struct {
	hasText, hasInt, hasReal, hasBlob bool
	bigInt                            bool // would lose precision as DOUBLE
	notTiny                           bool // does not fit TINYINT(1)
	badTime                           bool // holds a value that is not a readable date
	maxLen                            int
}

func (p *profile) observe(value any, dateColumn bool) {
	switch v := value.(type) {
	case nil:
		return
	case int64:
		p.hasInt = true
		p.notTiny = p.notTiny || v < -128 || v > 127
		p.bigInt = p.bigInt || v > 1<<53 || v < -(1<<53)
	case float64:
		p.hasReal = true
	case string:
		if utf8.ValidString(v) {
			p.hasText = true
		} else {
			p.hasBlob = true
		}
		p.maxLen = max(p.maxLen, utf8.RuneCountInString(v))
	case []byte:
		p.hasBlob = true
		p.maxLen = max(p.maxLen, len(v))
	}
	if dateColumn && !p.badTime {
		_, err := cell.ParseTime(value)
		p.badTime = err != nil
	}
}

func typeContains(sqliteType string, words ...string) bool {
	upper := strings.ToUpper(sqliteType)
	for _, word := range words {
		if strings.Contains(upper, word) {
			return true
		}
	}
	return false
}

func isDateType(sqliteType string) bool { return typeContains(sqliteType, "DATE", "TIME") }

func isTextType(sqliteType string) bool { return typeContains(sqliteType, "CHAR", "CLOB", "TEXT") }

// mysqlType starts from the declared type and corrects it using the real data.
func mysqlType(c schema.Column, p profile, key bool) string {
	numericData := !p.hasText && !p.hasBlob
	floatDeclared := typeContains(c.Type, "REAL", "FLOA", "DOUB", "DEC", "NUM")

	switch {
	case p.hasBlob || typeContains(c.Type, "BLOB"):
		if key && p.maxLen <= maxKeyTextLen {
			return fmt.Sprintf("VARBINARY(%d)", max(keyTextLen, p.maxLen))
		}
		return "LONGBLOB"
	case typeContains(c.Type, "BOOL") && numericData && !p.hasReal && !p.notTiny:
		return "TINYINT(1)"
	case isDateType(c.Type) && !p.badTime:
		return "DATETIME(6)"
	case !isTextType(c.Type) && numericData:
		// SQLite stores DECIMAL as a float too, so DOUBLE keeps the exact value.
		switch {
		case p.hasReal && p.bigInt:
			// no numeric type keeps this mix intact: fall through to text
		case p.hasReal, floatDeclared && !p.bigInt:
			return "DOUBLE"
		case p.hasInt, floatDeclared, typeContains(c.Type, "INT"):
			return "BIGINT"
		}
	}
	if key && p.maxLen <= maxKeyTextLen {
		// utf8mb4_bin: SQLite is case-sensitive, so 'abc' and 'ABC' must not collide.
		return fmt.Sprintf("VARCHAR(%d) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin", max(keyTextLen, p.maxLen))
	}
	return "LONGTEXT"
}

var (
	numberRe = regexp.MustCompile(`^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$`)
	stringRe = regexp.MustCompile(`^'([^']|'')*'$`)
)

// defaultClause translates a SQLite DEFAULT; ok=false means MySQL has no equivalent.
func defaultClause(c schema.Column, columnType string) (clause string, ok bool) {
	raw := strings.TrimSpace(c.Default.String)
	upper := strings.ToUpper(raw)
	var value string
	switch {
	case !c.Default.Valid:
		return "", true
	case upper == "NULL":
		return "", !c.NotNull
	case upper == "CURRENT_TIMESTAMP":
		if strings.HasPrefix(columnType, "DATETIME") {
			return " DEFAULT CURRENT_TIMESTAMP(6)", true
		}
		return "", false
	case upper == "TRUE":
		value = "1"
	case upper == "FALSE":
		value = "0"
	case numberRe.MatchString(raw):
		value = raw
	case stringRe.MatchString(raw):
		value = strings.ReplaceAll(raw, `\`, `\\`) // backslash escapes in MySQL but not in SQLite
	default:
		return "", false
	}
	if strings.HasPrefix(columnType, "LONG") {
		return " DEFAULT (" + value + ")", true // TEXT and BLOB columns only accept expression defaults
	}
	return " DEFAULT " + value, true
}

func createTableSQL(t schema.Table, profiles []profile, withDefaults bool) (string, []string) {
	var definitions, skippedDefaults []string
	pk := schema.PrimaryKey(t)
	for i, c := range t.Columns {
		columnType := mysqlType(c, profiles[i], t.KeyColumns[strings.ToLower(c.Name)])
		definition := schema.Quote(c.Name) + " " + columnType
		// Only a single INTEGER PRIMARY KEY is an automatic rowid in SQLite.
		if len(pk) == 1 && c.PK == 1 && strings.EqualFold(strings.TrimSpace(c.Type), "INTEGER") {
			definitions = append(definitions, definition+" NOT NULL AUTO_INCREMENT")
			continue
		}
		if c.NotNull || c.PK > 0 {
			definition += " NOT NULL"
		} else {
			definition += " NULL"
		}
		if withDefaults {
			clause, ok := defaultClause(c, columnType)
			if !ok {
				skippedDefaults = append(skippedDefaults, fmt.Sprintf("%s=%s", c.Name, c.Default.String))
			}
			definition += clause
		}
		definitions = append(definitions, definition)
	}
	if len(pk) > 0 {
		definitions = append(definitions, "PRIMARY KEY ("+schema.QuoteList(pk)+")")
	}
	query := fmt.Sprintf("CREATE TABLE %s (%s) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci",
		schema.Quote(t.Name), strings.Join(definitions, ", "))
	return query, skippedDefaults
}

func indexSQL(t schema.Table, index schema.Index) string {
	kind := "INDEX"
	if index.Unique {
		kind = "UNIQUE INDEX"
	}
	return fmt.Sprintf("CREATE %s %s ON %s (%s)", kind, schema.Quote(index.Name), schema.Quote(t.Name), schema.QuoteList(index.Columns))
}

func foreignKeySQL(t schema.Table, fk schema.ForeignKey) string {
	return fmt.Sprintf("ALTER TABLE %s ADD FOREIGN KEY (%s) REFERENCES %s (%s) ON DELETE %s ON UPDATE %s",
		schema.Quote(t.Name), schema.QuoteList(fk.Columns), schema.Quote(fk.RefTable), schema.QuoteList(fk.RefColumns), fk.OnDelete, fk.OnUpdate)
}

func profileTable(lite *sql.Tx, t schema.Table) ([]profile, error) {
	profiles := make([]profile, len(t.Columns))
	err := schema.EachSourceRow(lite, t, func(values []any) error {
		for i, value := range values {
			profiles[i].observe(value, isDateType(t.Columns[i].Type))
		}
		return nil
	})
	return profiles, err
}
