package cell

import (
	"testing"
	"time"
)

func TestParseTime(t *testing.T) {
	want := time.Date(2023, 11, 14, 22, 13, 20, 0, time.UTC)
	for _, value := range []any{
		int64(1700000000), int64(1700000000000), float64(1700000000),
		"2023-11-14 22:13:20", "2023-11-14T22:13:20Z", "2023-11-15T05:13:20+07:00",
	} {
		got, err := ParseTime(value)
		if err != nil || !got.Equal(want) {
			t.Errorf("%v: %v %v", value, got, err)
		}
	}
	julian, err := ParseTime(2460263.42592593) // 2023-11-14 22:13:20
	if err != nil || julian.Sub(want).Abs() > time.Second {
		t.Errorf("julian: %v %v", julian, err)
	}
	for _, value := range []any{"yesterday", "", []byte{1}, int64(1 << 62)} {
		if _, err := ParseTime(value); err == nil {
			t.Errorf("%v must be rejected", value)
		}
	}
}

// A SQLite value and the form MySQL returns for it must compare as equal.
func TestCanonMatchesMySQLOutput(t *testing.T) {
	stamp := time.Date(2024, 1, 2, 3, 4, 5, 123456000, time.UTC)
	same := []struct {
		source, stored any
		Target         Target
	}{
		{stamp, []byte("2024-01-02 03:04:05.123456"), Classify("datetime", 6)},
		{stamp, []byte("2024-01-02 03:04:05.123"), Classify("datetime", 3)},
		{stamp, []byte("2024-01-02 03:04:05"), Classify("timestamp", 0)},
		{stamp, []byte("2024-01-02"), Classify("date", 0)},
		{0.1, []byte("0.1"), Classify("double", 0)},
		{0.1, []byte("0.100000000000000000000000000000"), Classify("decimal", 0)},
		{int64(3), []byte("3.00"), Classify("decimal", 0)},
		{0.1, float32(0.1), Classify("float", 0)},
		{int64(1), int64(1), Classify("tinyint", 0)},
		{int64(1), []byte("1"), Classify("bigint", 0)},
		{"abc", []byte("abc"), Classify("longtext", 0)},
		{`{"a":1}`, []byte(`{"a": 1}`), Classify("json", 0)},
	}
	for _, c := range same {
		if Hash(c.source, c.Target) != Hash(c.stored, c.Target) {
			t.Errorf("%v and %s must match", c.source, c.stored)
		}
	}
	different := []struct {
		source, stored any
		Target         Target
	}{
		{"abc", []byte("abd"), Classify("varchar", 0)},
		{"abcdef", []byte("abc"), Classify("varchar", 0)},
		{1.5, []byte("2"), Classify("int", 0)},
		{int64(1 << 60), []byte("1152921504606846977"), Classify("bigint", 0)},
		{nil, []byte(""), Classify("varchar", 0)},
		{stamp, []byte("2024-01-02 03:04:06.000000"), Classify("datetime", 6)},
	}
	for _, c := range different {
		if Hash(c.source, c.Target) == Hash(c.stored, c.Target) {
			t.Errorf("%v and %s must differ", c.source, c.stored)
		}
	}
}

func TestConvert(t *testing.T) {
	if got, _ := Convert(int64(5), Classify("longtext", 0)); got != "5" {
		t.Errorf("%#v", got)
	}
	if got, _ := Convert(int64(1700000000000), Classify("datetime", 3)); !got.(time.Time).Equal(time.UnixMilli(1700000000000)) {
		t.Errorf("%#v", got)
	}
	if _, err := Convert("yesterday", Classify("datetime", 3)); err == nil {
		t.Error("a bad date for a DATETIME column must fail, not become 0001-01-01")
	}
	if got, err := Convert(nil, Classify("datetime", 3)); got != nil || err != nil {
		t.Errorf("%#v %v", got, err)
	}
}
