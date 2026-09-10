package logx

import (
	"io"
	"log"
	"strings"
	"testing"
	"time"
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

// silenceConsole 把标准库日志输出丢进黑洞，避免测试用例把日志打到屏幕上。
func silenceConsole(t *testing.T) {
	t.Helper()
	out := log.Writer()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(out) })
}

func TestSubscribeReceivesSingleLineEntries(t *testing.T) {
	silenceConsole(t)

	got := make(chan Entry, 4)
	unsubscribe := Subscribe(func(e Entry) { got <- e })
	defer unsubscribe()

	before := time.Now()
	Printf("多行\n内容 %d", 42)

	select {
	case e := <-got:
		if want := `多行\n内容 42`; e.Text != want {
			t.Errorf("Entry.Text = %q, want %q", e.Text, want)
		}
		if e.At.Before(before) || e.At.After(time.Now()) {
			t.Errorf("Entry.At = %v，不在本次调用区间内", e.At)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("订阅者未收到日志")
	}
}

func TestSubscribeFansOutToEverySubscriber(t *testing.T) {
	silenceConsole(t)

	first := make(chan Entry, 1)
	second := make(chan Entry, 1)
	defer Subscribe(func(e Entry) { first <- e })()
	defer Subscribe(func(e Entry) { second <- e })()

	Print("hello")

	for i, ch := range []chan Entry{first, second} {
		select {
		case e := <-ch:
			if e.Text != "hello" {
				t.Errorf("订阅者 %d 收到 %q", i, e.Text)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("订阅者 %d 未收到日志", i)
		}
	}
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	silenceConsole(t)

	got := make(chan Entry, 4)
	unsubscribe := Subscribe(func(e Entry) { got <- e })
	Print("第一条")
	unsubscribe()
	unsubscribe() // 重复注销必须是安全的
	Print("第二条")

	select {
	case e := <-got:
		if e.Text != "第一条" {
			t.Fatalf("收到意外记录 %q", e.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("订阅者未收到第一条日志")
	}
	select {
	case e := <-got:
		t.Fatalf("注销后仍收到 %q", e.Text)
	case <-time.After(50 * time.Millisecond):
	}
}

// 一个订阅者 panic 不能影响其它订阅者，也不能掀翻打印日志的调用方。
func TestPanickingSubscriberIsIsolated(t *testing.T) {
	silenceConsole(t)

	got := make(chan Entry, 1)
	defer Subscribe(func(Entry) { panic("boom") })()
	defer Subscribe(func(e Entry) { got <- e })()

	Print("still alive")

	select {
	case e := <-got:
		if e.Text != "still alive" {
			t.Errorf("Entry.Text = %q", e.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("panic 的订阅者影响了后续订阅者")
	}
}

// 订阅者拿到的正文里不能有换行，否则界面日志视图会被一条记录撑成多行。
func TestSubscriberNeverSeesNewlines(t *testing.T) {
	silenceConsole(t)

	got := make(chan Entry, 1)
	defer Subscribe(func(e Entry) { got <- e })()

	Print("a\nb\rc\td")

	select {
	case e := <-got:
		if strings.ContainsAny(e.Text, "\n\r") {
			t.Fatalf("Entry.Text 含换行: %q", e.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("订阅者未收到日志")
	}
}
