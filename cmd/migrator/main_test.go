package main

import (
	"strings"
	"testing"
)

func TestRunDoesNotLeakPassword(t *testing.T) {
	err := run([]string{"no-such.db", "user:secret@tcp(127.0.0.1:1)/db"})
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Errorf("%v", err)
	}
}
