// Package logx prints one-line log records. Clipboard text, client names and
// error strings are all attacker- or user-controlled and may contain newlines;
// printing them raw splits a single event over many log lines, which breaks
// both reading and grepping. Every record goes through SingleLine so that one
// event always occupies exactly one line.
//
// 除了控制台，日志还可以分发给订阅者（见 Subscribe）：客户端的桌面模式没有
// 可见的控制台，界面里的日志视图就靠订阅取得同一条记录。
package logx

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

// Entry 是一条日志记录：At 为产生时刻，Text 为已转义成单行的正文（不含
// 时间前缀，控制台输出的时间前缀由标准库 log 包添加）。
type Entry struct {
	At   time.Time
	Text string
}

// 订阅者注册表。日志可能来自任意 goroutine（网络、剪贴板监听、HTTP 界面），
// 所以增删订阅与分发都要加锁。
var (
	subMu   sync.RWMutex
	subs    = map[int]func(Entry){}
	nextSub int
)

// Subscribe 注册一个日志订阅者，返回注销函数（可安全重复调用）。
//
// 订阅者会被同步调用，且与打印日志的 goroutine 是同一个：实现必须立即返回，
// 不能阻塞、不能反向写日志，否则会拖住整条日志链路。订阅者内部的 panic 会被
// 拦下（记录到控制台），不会影响其它订阅者或调用方。
func Subscribe(f func(Entry)) (unsubscribe func()) {
	if f == nil {
		return func() {}
	}
	subMu.Lock()
	id := nextSub
	nextSub++
	subs[id] = f
	subMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			subMu.Lock()
			delete(subs, id)
			subMu.Unlock()
		})
	}
}

// emit 打印一条已经过 SingleLine 处理的记录，并分发给所有订阅者。
func emit(text string) {
	log.Print(text) // 控制台输出保持原样（标准库 log 的时间前缀不变）

	e := Entry{At: time.Now(), Text: text}
	subMu.RLock()
	targets := make([]func(Entry), 0, len(subs))
	for _, f := range subs {
		targets = append(targets, f)
	}
	subMu.RUnlock()
	for _, f := range targets {
		callSink(f, e)
	}
}

// callSink 保证订阅者的 panic 不会掀翻打印日志的那条 goroutine（例如网络
// 读循环），否则一个界面 bug 就能把剪贴板同步整个搞停。
func callSink(f func(Entry), e Entry) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("logx: 日志订阅者 panic: %v", r)
		}
	}()
	f(e)
}

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
	emit(SingleLine(fmt.Sprintf(format, args...)))
}

// Print logs its operands as a single line.
func Print(args ...any) {
	emit(SingleLine(fmt.Sprint(args...)))
}

// Fatalf logs a formatted record as a single line and exits with status 1.
func Fatalf(format string, args ...any) {
	emit(SingleLine(fmt.Sprintf(format, args...)))
	os.Exit(1)
}

// Fatal logs its operands as a single line and exits with status 1.
func Fatal(args ...any) {
	emit(SingleLine(fmt.Sprint(args...)))
	os.Exit(1)
}
