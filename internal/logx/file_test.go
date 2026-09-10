package logx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAttachFileWritesSingleLineRecords(t *testing.T) {
	silenceConsole(t)

	path := filepath.Join(t.TempDir(), "sub", "client.log")
	closeFn, err := AttachFile(path, 0)
	if err != nil {
		t.Fatalf("AttachFile: %v", err)
	}
	defer closeFn()

	Printf("第一条 %d", 1)
	Print("多行\n内容")

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	got := string(raw)
	for _, want := range []string{"第一条 1", `多行\n内容`} {
		if !strings.Contains(got, want) {
			t.Errorf("日志文件缺少 %q，实际内容:\n%s", want, got)
		}
	}
	if strings.Contains(got, "多行\n内容") {
		t.Errorf("日志文件里出现了未转义的换行:\n%s", got)
	}
	// 每条记录都必须带时间前缀，且各自独占一行。
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if len(line) < len("2006-01-02 15:04:05 ") || line[4] != '-' {
			t.Errorf("记录缺少时间前缀: %q", line)
		}
	}
}

func TestAttachFileRotatesOversizedFile(t *testing.T) {
	silenceConsole(t)

	path := filepath.Join(t.TempDir(), "client.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 128)), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	closeFn, err := AttachFile(path, 64)
	if err != nil {
		t.Fatalf("AttachFile: %v", err)
	}
	Print("新记录")
	closeFn()

	rotated, err := os.ReadFile(path + ".old")
	if err != nil {
		t.Fatalf("轮转后的旧文件不存在: %v", err)
	}
	if len(rotated) != 128 {
		t.Errorf("旧文件长度 = %d, want 128", len(rotated))
	}
	fresh, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(fresh), "xxxx") {
		t.Errorf("轮转后新文件里不该还有旧内容:\n%s", fresh)
	}
	if !strings.Contains(string(fresh), "新记录") {
		t.Errorf("轮转后新记录未写入:\n%s", fresh)
	}
}

func TestAttachFileUnsubscribeStopsWriting(t *testing.T) {
	silenceConsole(t)

	path := filepath.Join(t.TempDir(), "client.log")
	closeFn, err := AttachFile(path, 0)
	if err != nil {
		t.Fatalf("AttachFile: %v", err)
	}
	Print("写之前")
	closeFn()
	closeFn() // 重复调用必须安全
	Print("写之后")

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(raw), "写之后") {
		t.Errorf("注销后仍在写文件:\n%s", raw)
	}
}
