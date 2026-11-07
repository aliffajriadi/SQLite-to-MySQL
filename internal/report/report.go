// Package report collects what could not be migrated, for the final summary.
package report

import (
	"fmt"
)

var warnings []string

// Warn prints a warning and keeps it for the summary.
func Warn(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	warnings = append(warnings, msg)
	fmt.Println("  ⚠", msg)
}

// All returns the warnings collected so far.
func All() []string { return warnings }

// Reset clears the warnings. Tests use it.
func Reset() { warnings = nil }
