// Package prompt 提供安装向导那类「问题 [默认值]: 」的交互问答原语。
//
// 设计约定（install 子命令依赖这些行为）：
//   - 每个问题都带默认值，用户直接回车（或输入空白）即采用默认值；
//   - UI 关闭交互（非终端 stdin、--yes）时所有方法不打扰终端，直接返回默认值；
//   - 输入到达末尾（Ctrl-D / 管道读完）时自动退回非交互模式，后续问题都取默认，
//     不会死循环等输入；
//   - 带校验的 Int / TextFunc 在答案非法时提示后重试，连续失败多次才报错，
//     校验函数同时负责规范化（如把纯端口号补成 ":9090"）。
package prompt

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// maxRetries 是单个问题允许的无效重试次数，超过后 Int 回退默认值、
// TextFunc 返回错误，防止脚本或手滑把用户永久卡在同一个问题上。
const maxRetries = 3

// UI 是一个问答会话：in 读答案，out 印问题。请用 New 创建，零值不可用。
type UI struct {
	r  *bufio.Reader
	w  io.Writer
	on bool
}

// New 创建问答会话。active=false 表示不提问（所有方法直接返回默认值）。
func New(in io.Reader, out io.Writer, active bool) *UI {
	return &UI{r: bufio.NewReader(in), w: out, on: active}
}

// Active 报告当前是否真的在提问（非交互或已遇到输入末尾时为 false）。
func (u *UI) Active() bool { return u.on }

// StdinInteractive 判断标准输入是否是交互终端（字符设备）。管道、重定向、
// CI 里都返回 false，install 据此自动降级为「参数 + 默认值」模式。
func StdinInteractive() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// ask 印出问题并读一行答案。返回 raw=false 表示应当直接采用默认值
// （非交互、空行、或输入已到末尾——末尾之后会话自动退出交互模式）。
func (u *UI) ask(question, defShown string) (string, bool) {
	if !u.on {
		return "", false
	}
	fmt.Fprintf(u.w, "%s [%s]: ", question, defShown)
	line, err := u.r.ReadString('\n')
	line = strings.TrimSpace(line)
	if err != nil && line == "" {
		// EOF：先让本问题落到默认值，再把会话降级为非交互，后续问题静默取默认。
		u.on = false
		fmt.Fprintln(u.w, defShown)
		return "", false
	}
	if line == "" {
		return "", false
	}
	return line, true
}

// defHint 决定方括号里显示什么：空默认值（如 key、可选项）显示「留空」。
func defHint(def string) string {
	if def == "" {
		return "留空"
	}
	return def
}

// Text 提一个字符串问题；直接回车返回 def。
func (u *UI) Text(question, def string) string {
	if raw, ok := u.ask(question, defHint(def)); ok {
		return raw
	}
	return def
}

// TextFunc 提一个带校验/规范化的字符串问题：check 返回规范值或错误说明。
// 答案非法时提示并重试，最多 maxRetries 次后返回错误（调用方决定去留）。
func (u *UI) TextFunc(question, def string, check func(string) (string, error)) (string, error) {
	for i := 0; i < maxRetries; i++ {
		raw, ok := u.ask(question, defHint(def))
		if !ok {
			raw = def
		}
		v, err := check(raw)
		if err == nil {
			return v, nil
		}
		if !u.on { // 非交互模式下默认值都不合法：没有重试的意义
			return "", fmt.Errorf("%s: %v", question, err)
		}
		fmt.Fprintf(u.w, "  无效输入：%v\n", err)
	}
	return "", fmt.Errorf("%s：连续 %d 次输入无效", question, maxRetries)
}

// Confirm 提一个是/否问题；回车采用 def。接受 y/yes/n/no（不区分大小写）。
func (u *UI) Confirm(question string, def bool) bool {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	for i := 0; i < maxRetries; i++ {
		raw, ok := u.ask(question+"？", hint)
		if !ok {
			return def
		}
		switch strings.ToLower(raw) {
		case "y", "yes":
			return true
		case "n", "no":
			return false
		}
		if !u.on {
			return def
		}
		fmt.Fprintf(u.w, "  请输入 y 或 n\n")
	}
	return def
}

// Int 提一个整数问题；hi 为 0 表示不设上限。超出范围或非法时重试，
// 重试用尽后回退默认值（安装器里宁可用默认值继续，也不要崩在参数上）。
func (u *UI) Int(question string, def, lo, hi int) int {
	for i := 0; i < maxRetries; i++ {
		raw, ok := u.ask(question, strconv.Itoa(def))
		if !ok {
			return def
		}
		n, err := strconv.Atoi(raw)
		if err == nil && n >= lo && (hi == 0 || n <= hi) {
			return n
		}
		if !u.on {
			return def
		}
		if hi == 0 {
			fmt.Fprintf(u.w, "  请输入不小于 %d 的整数\n", lo)
		} else {
			fmt.Fprintf(u.w, "  请输入 %d 到 %d 之间的整数\n", lo, hi)
		}
	}
	return def
}
