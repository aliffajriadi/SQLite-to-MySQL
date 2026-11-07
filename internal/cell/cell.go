// Package cell converts SQLite values for MySQL and compares them across both databases.
package cell

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/mattn/go-sqlite3"
)

const (
	kindText   = iota // compared byte for byte
	kindInt           // compared as decimal text
	kindFloat         // compared up to Target.digits
	kindTime          // converted to time.Time
	kindOpaque        // MySQL rewrites the value (JSON, BIT), so only NULL-ness is compared
)

// Target describes a MySQL column for conversion and comparison.
type Target struct {
	kind     int
	digits   int  // kindFloat
	scale    int  // kindTime: fractional second digits
	dateOnly bool // kindTime: DATE column
}

// Classify turns an INFORMATION_SCHEMA data type into a Target.
func Classify(dataType string, precision int) Target {
	switch strings.ToLower(dataType) {
	case "tinyint", "smallint", "mediumint", "int", "integer", "bigint", "year":
		return Target{kind: kindInt}
	case "float":
		return Target{kind: kindFloat, digits: 6}
	case "double", "decimal":
		return Target{kind: kindFloat, digits: 15}
	case "date":
		return Target{kind: kindTime, dateOnly: true}
	case "datetime", "timestamp":
		return Target{kind: kindTime, scale: precision}
	case "json", "bit":
		return Target{kind: kindOpaque}
	}
	return Target{kind: kindText}
}

// ParseTime reads any date form SQLite may hold and returns it in UTC.
func ParseTime(value any) (time.Time, error) {
	var t time.Time
	switch v := value.(type) {
	case int64:
		// 13 digits means milliseconds (Prisma), anything else seconds.
		if v > 1e12 || v < -1e12 {
			t = time.UnixMilli(v)
		} else {
			t = time.Unix(v, 0)
		}
	case float64:
		if v >= 1e6 && v < 1e7 {
			v = (v - 2440587.5) * 86400 // Julian day, as returned by julianday()
		}
		sec, frac := math.Modf(v)
		t = time.Unix(int64(sec), int64(frac*1e9))
	case string:
		s := strings.TrimSuffix(v, "Z")
		for _, format := range sqlite3.SQLiteTimestampFormats {
			parsed, err := time.ParseInLocation(format, s, time.UTC)
			if err == nil {
				t = parsed
				break
			}
		}
		if t.IsZero() {
			return t, fmt.Errorf("not a date: %q", shorten(v))
		}
	default:
		return t, fmt.Errorf("not a date: %T", value)
	}
	if year := t.UTC().Year(); year < 1 || year > 9999 {
		return t, fmt.Errorf("year outside the MySQL range: %d", year)
	}
	return t.UTC(), nil
}

// Convert prepares a SQLite value for its Target MySQL column.
func Convert(value any, t Target) (any, error) {
	if value == nil {
		return nil, nil
	}
	switch t.kind {
	case kindTime:
		return ParseTime(value)
	case kindText:
		switch value.(type) {
		case int64, float64:
			return text(value), nil
		}
	}
	return value, nil
}

func text(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case uint64:
		return strconv.FormatUint(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 32)
	}
	return fmt.Sprint(value)
}

// canon gives equal values the same form, whether read from SQLite or MySQL.
func canon(value any, t Target) string {
	switch {
	case value == nil:
		return "\x00"
	case t.kind == kindOpaque:
		return "\x01"
	}
	switch t.kind {
	case kindTime:
		parsed, ok := value.(time.Time)
		if !ok {
			var err error
			if parsed, err = ParseTime(text(value)); err != nil {
				return "\x02" + text(value)
			}
		}
		if t.dateOnly {
			return "\x02" + parsed.Format("2006-01-02")
		}
		unit := time.Second / time.Duration(math.Pow10(t.scale))
		return "\x02" + parsed.Truncate(time.Microsecond).Round(unit).Format("2006-01-02 15:04:05.000000")
	case kindFloat:
		if f, err := strconv.ParseFloat(text(value), 64); err == nil {
			return "\x02" + strconv.FormatFloat(f, 'g', t.digits, 64)
		}
	}
	return "\x02" + text(value)
}

// Hash returns a value that callers sum per column, so a checksum does not depend on row order.
func Hash(value any, t Target) uint64 {
	sum := sha256.Sum256([]byte(canon(value, t)))
	return binary.LittleEndian.Uint64(sum[:8])
}

func shorten(s string) string {
	if len(s) > 60 {
		return s[:60] + "..."
	}
	return s
}
