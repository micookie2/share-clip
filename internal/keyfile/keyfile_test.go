package keyfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFormatParseRoundTrip(t *testing.T) {
	for _, key := range []string{
		"uRRbv71iN9iVjHWHlhAp2OuSBncil6hl", // 随机 key 的形状（base64url）
		"simple_key-1.2@3/4+5",
		"带 空格 和=号的 key",
		`quote"and\backslash`,
	} {
		got, err := Parse(Format(key))
		if err != nil {
			t.Fatalf("Parse(Format(%q)): %v", key, err)
		}
		if got != key {
			t.Fatalf("round-trip 失败: %q → %q", key, got)
		}
	}
}

func TestParseLenientFormats(t *testing.T) {
	cases := map[string]string{
		"SHARECLIP_KEY=abc123\n":            "abc123",
		"SHARECLIP_KEY=abc123\n# comment\n": "abc123",
		"  SHARECLIP_KEY=\"a b\"  \n":       "a b",
		"bare-raw-key\n":                    "bare-raw-key",
		"SHARECLIP_KEY=\"tab\\tsep\"\n":     "tab\tsep",
		"SHARECLIP_KEY=\"esc\\\"aped\"\n":   `esc"aped`,
	}
	for content, want := range cases {
		got, err := Parse(content)
		if err != nil {
			t.Fatalf("Parse(%q): %v", content, err)
		}
		if got != want {
			t.Fatalf("Parse(%q) = %q，期望 %q", content, got, want)
		}
	}
	for _, bad := range []string{"", "   \n", "SHARECLIP_KEY=\n", "one\ntwo\n"} {
		if _, err := Parse(bad); err == nil {
			t.Fatalf("Parse(%q) 应当报错", bad)
		}
	}
}

func TestFormatQuotesUnsafeValues(t *testing.T) {
	// $ 会被 systemd 做变量展开，必须落在引号里。
	line := Format("pay$load")
	if !strings.HasPrefix(line, `SHARECLIP_KEY="`) {
		t.Fatalf("含 $ 的 key 应加引号: %q", line)
	}
	if raw := Format("plain.key_1"); strings.Contains(raw, `"`) {
		t.Fatalf("安全字符集不应加引号: %q", raw)
	}
}

func TestValid(t *testing.T) {
	if err := Valid("good-key-1234"); err != nil {
		t.Fatalf("合法 key 报错: %v", err)
	}
	for _, bad := range []string{"", "ab", " lead", "trail ", "line1\nline2", "tab\there", strings.Repeat("x", 257)} {
		if err := Valid(bad); err == nil {
			t.Fatalf("非法 key 未报错: %q", bad)
		}
	}
}

func TestWriteReadAndPerms(t *testing.T) {
	dir := t.TempDir()
	path := PathFor(filepath.Join(dir, "sub")) // 目录不存在也要能写
	if err := Write(path, "abc123"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("新 key 文件权限 = %v，期望 0600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "SHARECLIP_KEY=abc123\n" {
		t.Fatalf("文件内容 = %q（应为 systemd EnvironmentFile 格式）", string(data))
	}
	// 覆盖写（轮换场景）。
	if err := Write(path, "rotated-key-value"); err != nil {
		t.Fatalf("覆盖写: %v", err)
	}
	got, err := Read(path)
	if err != nil || got != "rotated-key-value" {
		t.Fatalf("Read = %q, %v", got, err)
	}
	// 非法 key 拒绝写入。
	if err := Write(path, "no\nnewline"); err == nil {
		t.Fatal("写入含换行的 key 应报错")
	}
}

func TestGenerateShape(t *testing.T) {
	k, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if len(k) != 32 {
		t.Fatalf("随机 key 长度 = %d，期望 32（base64url 24 字节）", len(k))
	}
	if err := Valid(k); err != nil {
		t.Fatalf("生成的 key 应直接合法: %v", err)
	}
	k2, _ := Generate()
	if k == k2 {
		t.Fatal("两次生成不应相同")
	}
}
