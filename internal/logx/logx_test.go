package logx

import (
	"log"
	"strings"
	"testing"
)

func TestSingleLine(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"plain text", "plain text"},
		{"hello 你好", "hello 你好"},
		{"one\ntwo", `one\ntwo`},
		{"one\r\ntwo", `one\r\ntwo`},
		{"a\tb", `a\tb`},
		{"bell\x07", `bell\x07`},
		{"ansi\x1b[31mred", `ansi\x1b[31mred`},
		{"del\x7f", `del\x7f`},
		{"line\u2028sep", `line\u2028sep`},
		{"nel\u0085x", `nel\u0085x`},
		// paths keep their backslashes
		{`C:\Users\me`, `C:\Users\me`},
		{"%s %d", "%s %d"},
	}
	for _, c := range cases {
		if got := SingleLine(c.in); got != c.want {
			t.Errorf("SingleLine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSingleLineNeverReturnsNewlines(t *testing.T) {
	raw := "multi\nline\r\ntext\twith\x00\x1b control\u2028\u2029\u0085"
	got := SingleLine(raw)
	if strings.ContainsAny(got, "\n\r\v\f") || strings.Contains(got, "\u2028") || strings.Contains(got, "\u2029") || strings.Contains(got, "\u0085") {
		t.Fatalf("SingleLine left a line break in %q", got)
	}
	if !strings.Contains(got, `\n`) {
		t.Errorf("SingleLine dropped information: %q", got)
	}
}

func TestSingleLineKeepsPlainStringsIntact(t *testing.T) {
	s := "client desktop-1 shared text (12 B)"
	if got := SingleLine(s); got != s {
		t.Errorf("SingleLine(%q) = %q", s, got)
	}
}

func TestPrintfCollapsesRecordToLine(t *testing.T) {
	var buf strings.Builder
	flags, prefix := log.Flags(), log.Prefix()
	out := log.Writer()
	log.SetOutput(&buf)
	log.SetFlags(0)
	log.SetPrefix("")
	defer func() {
		log.SetOutput(out)
		log.SetFlags(flags)
		log.SetPrefix(prefix)
	}()

	Printf("[client] 收到 %s 的文本：%s", "bob", "a\nb\tc")
	record := strings.TrimSuffix(buf.String(), "\n")
	if strings.Contains(record, "\n") {
		t.Fatalf("record spans several lines: %q", record)
	}
	if want := `[client] 收到 bob 的文本：a\nb\tc`; record != want {
		t.Errorf("record = %q, want %q", record, want)
	}
}
