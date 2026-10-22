package prompt

import (
	"errors"
	"strings"
	"testing"
)

// newTestUI 造一个以 script 为输入的交互会话，返回 UI 与输出缓冲。
func newTestUI(script string) (*UI, *strings.Builder) {
	out := &strings.Builder{}
	return New(strings.NewReader(script), out, true), out
}

func TestTextUsesDefaultOnEmpty(t *testing.T) {
	u, _ := newTestUI("\n")
	if got := u.Text("安装位置", "/opt/share-clip"); got != "/opt/share-clip" {
		t.Fatalf("空行应回退默认值，得到 %q", got)
	}
}

func TestTextReadsAnswer(t *testing.T) {
	u, out := newTestUI("  /srv/clip  \n")
	if got := u.Text("安装位置", "/opt/share-clip"); got != "/srv/clip" {
		t.Fatalf("答案未去空白: %q", got)
	}
	if !strings.Contains(out.String(), "安装位置 [/opt/share-clip]:") {
		t.Fatalf("问题里应显示默认值，输出: %q", out.String())
	}
}

func TestEmptyDefaultShowsPlaceholder(t *testing.T) {
	u, out := newTestUI("\n")
	if got := u.Text("固定访问 key", ""); got != "" {
		t.Fatalf("留空问题应返回空串，得到 %q", got)
	}
	if !strings.Contains(out.String(), "[留空]") {
		t.Fatalf("空默认值应显示「留空」占位，输出: %q", out.String())
	}
}

func TestInactiveReturnsDefaultsWithoutEcho(t *testing.T) {
	out := &strings.Builder{}
	u := New(strings.NewReader(""), out, false)
	if got := u.Text("q", "def"); got != "def" {
		t.Fatalf("非交互应返回默认值，得到 %q", got)
	}
	if !u.Confirm("q", true) {
		t.Fatal("非交互 Confirm 应返回默认值 true")
	}
	if out.Len() != 0 {
		t.Fatalf("非交互不应向终端输出任何东西，得到 %q", out.String())
	}
}

func TestEOFDowngradesToDefaults(t *testing.T) {
	// 只有一个答案，第二、第三个问题应自动取默认值而不是卡住。
	u, out := newTestUI("yes\n")
	if !u.Confirm("立即启动？", false) {
		t.Fatal("第一个问题应读到 yes")
	}
	// EOF 要在下一次读取时才暴露：第二问读完后会话降级……
	if got := u.Text("单元名", "share-clip"); got != "share-clip" {
		t.Fatalf("EOF 后应取默认值，得到 %q", got)
	}
	if u.Active() {
		t.Fatal("EOF 后应退出交互模式")
	}
	// ……第三问及之后则完全静默取默认。
	if got := u.Int("条数", 500, 1, 0); got != 500 {
		t.Fatalf("降级后应取默认值，得到 %d", got)
	}
	// 只有前两个问题打印了提示行；降级后的问题保持安静。
	if n := strings.Count(out.String(), "["); n != 2 {
		t.Fatalf("降级后不应继续打印问题，输出: %q", out.String())
	}
}

func TestConfirmParsesAndRetries(t *testing.T) {
	u, _ := newTestUI("maybe\nN\n")
	if u.Confirm("关闭鉴权？", true) {
		t.Fatal("第二次回答 N 应为 false")
	}
}

func TestIntRangeAndRetryThenDefault(t *testing.T) {
	u, _ := newTestUI("abc\n0\n7\n")
	if got := u.Int("端口", 9090, 1, 65535); got != 7 {
		t.Fatalf("第三次合法输入应生效，得到 %d", got)
	}
	// 连续超限：用尽重试后回退默认值。
	u2, _ := newTestUI("-1\n-1\n-1\n")
	if got := u2.Int("条数", 500, 1, 0); got != 500 {
		t.Fatalf("重试用尽应回退默认值，得到 %d", got)
	}
}

func TestTextFuncNormalizesAndErrors(t *testing.T) {
	check := func(s string) (string, error) {
		if s == "9090" {
			return ":9090", nil
		}
		return "", errors.New("格式不对")
	}
	u, _ := newTestUI("junk\n9090\n")
	if got, err := u.TextFunc("监听地址", ":9090", check); err != nil || got != ":9090" {
		t.Fatalf("重试后应成功，得到 %q, %v", got, err)
	}
	u2, _ := newTestUI("junk\njunk\njunk\n")
	if _, err := u2.TextFunc("监听地址", ":9090", check); err == nil {
		t.Fatal("连续无效应返回错误")
	}
}
