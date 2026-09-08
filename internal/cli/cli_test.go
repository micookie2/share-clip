package cli

import (
	"bytes"
	"strings"
	"testing"
)

// 长写法与简写必须落到同一个变量上，且默认值不受注册顺序影响。
func TestShortAndLongAreEquivalent(t *testing.T) {
	newCmd := func() (*Cmd, *string, *int, *bool) {
		c := New("shareclip-client")
		server := c.String("s", "server", "", "server 地址")
		poll := c.Int("p", "poll", 1000, "轮询间隔")
		quiet := c.Bool("q", "quiet", false, "减少日志")
		return c, server, poll, quiet
	}

	cases := []struct {
		name string
		args []string
	}{
		{"简写 + 空格", []string{"-s", "1.2.3.4:9000"}},
		{"长写 + 等号", []string{"--server=1.2.3.4:9000"}},
		{"单横线也算长写", []string{"-server", "1.2.3.4:9000"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, server, poll, quiet := newCmd()
			c.ParseArgs(tc.args)
			if got := *server; got != "1.2.3.4:9000" {
				t.Errorf("-s/--server 解析结果 = %q, 期望 %q", got, "1.2.3.4:9000")
			}
			// 未出现的选项必须保持默认值，别名注册不能把默认值覆盖成零值。
			if *poll != 1000 {
				t.Errorf("poll 默认值被改写: got %d, want 1000", *poll)
			}
			if *quiet {
				t.Error("quiet 默认值应为 false")
			}
		})
	}
}

// 别名之间互不干扰：同时给出时后写的覆盖先写的。
func TestLaterOccurrenceWins(t *testing.T) {
	c := New("shareclip-client")
	poll := c.Int("p", "poll", 1000, "轮询间隔")
	c.ParseArgs([]string{"-p", "200", "--poll", "300"})
	if *poll != 300 {
		t.Errorf("poll = %d, 期望 300", *poll)
	}
}

// 简写开关不吞掉后面的选项，多余的位置参数照常返回。
func TestBoolFlagsAndRest(t *testing.T) {
	c := New("shareclip-server")
	db := c.String("d", "db", "shareclip.db", "数据库路径")
	quiet := c.Bool("q", "quiet", false, "减少日志")
	rest := c.ParseArgs([]string{"-q", "-d", "x.db", "extra"})
	if !*quiet {
		t.Error("-q 应把 quiet 置为 true")
	}
	if *db != "x.db" {
		t.Errorf("-q 吞掉了后面的选项: db = %q, 期望 %q", *db, "x.db")
	}
	if len(rest) != 1 || rest[0] != "extra" {
		t.Errorf("位置参数 = %v, 期望 [extra]", rest)
	}
}

// -q=false 这种显式写法在简写形式上同样有效。
func TestBoolExplicitValue(t *testing.T) {
	c := New("shareclip-server")
	quiet := c.Bool("q", "quiet", true, "减少日志")
	c.ParseArgs([]string{"-q=false"})
	if *quiet {
		t.Error("-q=false 应把 quiet 置为 false")
	}
}

// 帮助里每个选项只占一行，长名与简写合并展示。
func TestUsageMergesNames(t *testing.T) {
	var buf bytes.Buffer
	c := New("shareclip-server")
	c.SetOutput(&buf)
	c.SetIntro("share-clip server 0.1.0")
	c.String("a", "addr", ":9000", "监听地址")
	c.Int("l", "history-limit", 500, "历史保留条数")
	c.Bool("q", "quiet", false, "减少日志")

	c.Usage()
	out := buf.String()

	for _, want := range []string{
		"-a, --addr string",
		"监听地址（默认 :9000）",
		"-l, --history-limit int",
		"-q, --quiet",
		"减少日志",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("帮助缺少 %q:\n%s", want, out)
		}
	}
	// 别名不应再单独占一行：addr 只出现一次，quiet 的默认 false 不必提示。
	if got := strings.Count(out, "--addr"); got != 1 {
		t.Errorf("--addr 出现 %d 次，期望 1 次:\n%s", got, out)
	}
	if strings.Contains(out, "(default") || strings.Count(out, "减少日志") != 1 {
		t.Errorf("帮助里有重复的选项行:\n%s", out)
	}
	if strings.Contains(out, "false") {
		t.Errorf("默认 false 的开关不该在帮助里提示默认值:\n%s", out)
	}
}
