// Package logx prints one-line log records. Clipboard text, client names and
// error strings are all attacker- or user-controlled and may contain newlines;
// printing them raw splits a single event over many log lines, which breaks
// both reading and grepping. Every record goes through SingleLine so that one
// event always occupies exactly one line.
package logx

import (
	"fmt"
	"log"
	"strings"
)

// SingleLine escapes every character that could move the cursor to another
// line (or make output unreadable) and returns a string without newlines.
// Real control bytes become two-character escapes (\n, \r, \t, \xNN), so
// information is preserved rather than dropped.
func SingleLine(s string) string {
	if !strings.ContainsFunc(s, breaksLine) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\v':
			b.WriteString(`\v`)
		case r == '\f':
			b.WriteString(`\f`)
		case r == '\u0085': // NEL
			b.WriteString(`\u0085`)
		case r == '\u2028': // line separator
			b.WriteString(`\u2028`)
		case r == '\u2029': // paragraph separator
			b.WriteString(`\u2029`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// breaksLine reports whether r would move the cursor off the current line or
// is a control byte that should not reach a terminal raw.
func breaksLine(r rune) bool {
	switch r {
	case '\n', '\r', '\t', '\v', '\f', '\u0085', '\u2028', '\u2029':
		return true
	}
	return r < 0x20 || r == 0x7f
}

// Printf logs a formatted record as a single line.
func Printf(format string, args ...any) {
	log.Print(SingleLine(fmt.Sprintf(format, args...)))
}

// Print logs its operands as a single line.
func Print(args ...any) {
	log.Print(SingleLine(fmt.Sprint(args...)))
}

// Fatalf logs a formatted record as a single line and exits with status 1.
func Fatalf(format string, args ...any) {
	log.Fatal(SingleLine(fmt.Sprintf(format, args...)))
}

// Fatal logs its operands as a single line and exits with status 1.
func Fatal(args ...any) {
	log.Fatal(SingleLine(fmt.Sprint(args...)))
}
