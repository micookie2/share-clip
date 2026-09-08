// Package cli 给命令行程序加上「单字母简写」：-s 与 --server 指向同一个变量，
// 完全等价，帮助里也只占一行。
//
// 标准库 flag 把每个名字当作互不相干的选项，想让 -s 和 --server 共存，就得各写
// 一遍注册、帮助里也会各印一行（还容易漏掉默认值说明）。本包把这层重复收掉，
// 于是日常启动可以直接写最短的形式：
//
//	shareclip-client -s 192.168.1.10:9000
//	shareclip-server -a :9000 -l 500
package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
)

// Cmd 是一个程序用到的全部选项。请用 New 创建，零值不可用。
type Cmd struct {
	fs    *flag.FlagSet
	intro string // 选项列表之前的说明文字
	items []item
}

// item 记录一个选项在帮助里的样子（长名 + 简写合并成一行）。
type item struct {
	short string
	long  string
	typ   string // 值类型提示：string / int / bool
	usage string
	def   string // 默认值提示；为空表示不展示（默认值即零值）
}

// New 创建一个选项集合，name 出现在解析报错里（如 shareclip-client）。
// 遇到 -h 或参数非法时打印帮助并退出，等价于标准库的 ExitOnError。
func New(name string) *Cmd {
	c := &Cmd{fs: flag.NewFlagSet(name, flag.ExitOnError)}
	c.fs.Usage = c.Usage
	return c
}

// SetIntro 设置帮助里选项列表之前的说明文字（原样输出，可多行）。
func (c *Cmd) SetIntro(s string) { c.intro = s }

// SetOutput 改变帮助与解析报错的输出目标，默认是 stderr。
func (c *Cmd) SetOutput(w io.Writer) { c.fs.SetOutput(w) }

// String 注册字符串选项，返回其地址，供 Parse 之后读取。
// short 传空字符串表示只保留长名。
func (c *Cmd) String(short, long, def, usage string) *string {
	p := c.fs.String(long, def, usage)
	if short != "" {
		c.fs.Var(aliasString{p}, short, "")
	}
	c.add(short, long, "string", usage, def)
	return p
}

// Int 注册整型选项。def 为 0 时不在帮助里重复说明。
func (c *Cmd) Int(short, long string, def int, usage string) *int {
	p := c.fs.Int(long, def, usage)
	if short != "" {
		c.fs.Var(aliasInt{p}, short, "")
	}
	defText := ""
	if def != 0 {
		defText = strconv.Itoa(def)
	}
	c.add(short, long, "int", usage, defText)
	return p
}

// Bool 注册开关选项，帮助里不显示类型提示（-q 即为 true，无需写 -q true）。
func (c *Cmd) Bool(short, long string, def bool, usage string) *bool {
	p := c.fs.Bool(long, def, usage)
	if short != "" {
		c.fs.Var(aliasBool{p}, short, "")
	}
	defText := ""
	if def {
		defText = "true"
	}
	c.add(short, long, "bool", usage, defText)
	return p
}

// Parse 解析 os.Args[1:]，返回剩余的位置参数。
// 参数非法时 New 设定的 ExitOnError 已经打印帮助并退出进程，这里无需处理错误。
func (c *Cmd) Parse() []string {
	return c.ParseArgs(os.Args[1:])
}

// ParseArgs 解析给定的参数列表，便于测试与自行控制入参。
func (c *Cmd) ParseArgs(args []string) []string {
	_ = c.fs.Parse(args)
	return c.fs.Args()
}

// Usage 打印帮助。它同时是标准库在 -h / 报错时要调用的回调。
func (c *Cmd) Usage() {
	w := c.fs.Output()
	if c.intro != "" {
		fmt.Fprintln(w, c.intro)
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w, "选项:")
	for _, it := range c.items {
		names := "--" + it.long
		if it.short != "" {
			names = "-" + it.short + ", --" + it.long
		}
		if it.typ != "bool" {
			names += " " + it.typ
		}
		fmt.Fprintf(w, "  %s\n", names)
		line := it.usage
		if it.def != "" {
			line += "（默认 " + it.def + "）"
		}
		fmt.Fprintf(w, "    \t%s\n", line)
	}
	fmt.Fprintln(w, "  -h, --help")
	fmt.Fprint(w, "    \t显示本帮助\n")
}

func (c *Cmd) add(short, long, typ, usage, def string) {
	c.items = append(c.items, item{short: short, long: long, typ: typ, usage: usage, def: def})
}

// 下面的别名类型只是把短名转发到长名注册的变量上。
//
// 之所以不直接再调一次 c.fs.StringVar(p, short, def, "")：flag.Var 不会写入目标
// 变量（它只记录 value.String() 作为默认值），而再次传默认值会让两个名字互相覆盖
// 注册顺序里的默认值。转发则保证「只有一个默认值、只有一个变量」。

type aliasString struct{ p *string }

func (a aliasString) String() string     { return *a.p }
func (a aliasString) Set(s string) error { *a.p = s; return nil }

type aliasInt struct{ p *int }

func (a aliasInt) String() string { return strconv.Itoa(*a.p) }
func (a aliasInt) Set(s string) error {
	n, err := strconv.Atoi(s)
	if err != nil {
		return err
	}
	*a.p = n
	return nil
}

type aliasBool struct{ p *bool }

func (a aliasBool) String() string { return strconv.FormatBool(*a.p) }
func (a aliasBool) Set(s string) error {
	b, err := strconv.ParseBool(s)
	if err != nil {
		return err
	}
	*a.p = b
	return nil
}

// IsBoolFlag 让 flag 包把简写也当成开关。少了它，"-q -d x.db" 会去把 "-d"
// 当作 -q 的取值，报 "invalid boolean value"，而不是把两个开关分开理解。
func (a aliasBool) IsBoolFlag() bool { return true }
