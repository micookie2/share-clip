package appconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setConfigHome 把用户配置目录指到临时目录，让 Load/Save 的测试互不影响，
// 也不碰真实用户目录。三个环境变量分别对应 Linux/macOS 与 Windows 的取值。
func setConfigHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("AppData", dir)
	t.Setenv("HOME", dir)
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	setConfigHome(t, dir)

	want := Config{Server: "192.168.1.10:9000", Name: "desktop-1", PollMs: 750, MaxPayload: 4096}
	if err := want.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	path, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if filepath.Dir(path) != filepath.Join(dir, dirName) {
		t.Fatalf("配置路径 = %s，期望位于 %s 下", path, dir)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("配置文件未写入: %v", err)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != want {
		t.Errorf("Load = %+v, want %+v", got, want)
	}
}

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	setConfigHome(t, t.TempDir())

	got, err := Load()
	if err != nil {
		t.Fatalf("Load 缺文件时应无错误，得到 %v", err)
	}
	if got != Default() {
		t.Errorf("Load = %+v, want %+v", got, Default())
	}
}

func TestLoadCorruptFileFallsBackToDefaults(t *testing.T) {
	dir := t.TempDir()
	setConfigHome(t, dir)

	path, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := Load()
	if err == nil {
		t.Fatal("损坏的配置应返回错误，便于界面提示用户")
	}
	if got != Default() {
		t.Errorf("损坏配置应退回默认值，得到 %+v", got)
	}
}

func TestWithDefaultsFillsZeroValues(t *testing.T) {
	got := Config{Server: "  10.0.0.1:9000  ", PollMs: 0, MaxPayload: -1}.WithDefaults()
	want := Config{Server: "10.0.0.1:9000", PollMs: DefaultPollMs, MaxPayload: DefaultMaxPayload}
	if got != want {
		t.Errorf("WithDefaults = %+v, want %+v", got, want)
	}
}

func TestNormalizeServer(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"192.168.1.10:9000", "192.168.1.10:9000", false},
		{" 192.168.1.10:9000 ", "192.168.1.10:9000", false},
		{"192.168.1.10", "192.168.1.10:9000", false},
		{"ws://192.168.1.10:9000/ws", "192.168.1.10:9000", false},
		{"http://share-clip.lan:9100", "share-clip.lan:9100", false},
		// 本工具只走明文 ws://，因此带 https:// 前缀时同样落到默认端口。
		{"https://example.com/", "example.com:9000", false},
		{"localhost:9000", "localhost:9000", false},
		{"[fe80::1]:9000", "[fe80::1]:9000", false},
		{"fe80::1", "[fe80::1]:9000", false},
		{"", "", true},
		{"   ", "", true},
		{"host:0", "", true},
		{"host:70000", "", true},
		{"host:abc", "", true},
		{"host:9000:9000", "", true},
		{"a b:9000", "", true},
	}
	for _, c := range cases {
		got, err := NormalizeServer(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("NormalizeServer(%q) = %q，期望报错", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("NormalizeServer(%q) 报错: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("NormalizeServer(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestValidate(t *testing.T) {
	if err := Validate(Default()); err == nil {
		t.Error("未配置 server 时应校验失败")
	}
	if err := Validate(Config{Server: "10.0.0.1:9000"}); err != nil {
		t.Errorf("合法地址应通过校验: %v", err)
	}
	if err := Validate(Config{Server: "10.0.0.1:9000", PollMs: 10}); err == nil {
		t.Error("轮询间隔过小应校验失败")
	}
	if err := Validate(Config{Server: "10.0.0.1:9000", MaxPayload: 10}); err == nil {
		t.Error("内容上限过小应校验失败")
	}
}

func TestNormalizedAppliesAddressAndDefaults(t *testing.T) {
	got, err := Config{Server: "ws://10.0.0.1/ws"}.Normalized()
	if err != nil {
		t.Fatalf("Normalized: %v", err)
	}
	if got.Server != "10.0.0.1:9000" {
		t.Errorf("Server = %q", got.Server)
	}
	if got.PollMs != DefaultPollMs || got.MaxPayload != DefaultMaxPayload {
		t.Errorf("默认值未补齐: %+v", got)
	}
	if _, err := (Config{Server: ""}).Normalized(); err == nil {
		t.Error("空地址应报错")
	}
}

func TestSaveIsAtomicAndSelfContained(t *testing.T) {
	dir := t.TempDir()
	setConfigHome(t, dir)

	if err := (Config{Server: "10.0.0.1:9000"}).Save(); err != nil {
		t.Fatalf("首次 Save: %v", err)
	}
	if err := (Config{Server: "10.0.0.2:9000"}).Save(); err != nil {
		t.Fatalf("二次 Save: %v", err)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Server != "10.0.0.2:9000" {
		t.Errorf("Server = %q，期望覆盖为 10.0.0.2:9000", got.Server)
	}

	// 原子写入不应留下临时文件。
	entries, err := os.ReadDir(filepath.Join(dir, dirName))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("残留临时文件: %s", e.Name())
		}
	}
}
