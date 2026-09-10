//go:build linux

// 这些测试针对 systemd——只有 Linux 才有，而且单元文件里写的全是 POSIX 绝对
// 路径（/var/lib/share-clip、/etc/systemd/system/…）。在 Windows 上 filepath.Join
// 会用反斜杠拼路径，这些断言本来就无从成立，因此本文件只在 Linux 上编译运行；
// 生产代码本身仍然是跨平台可编译的（Windows 上 install 会因缺少 root/systemd
// 而失败，这正是预期）。
package systemd

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubRun 替换包级 run，记录所有命令；failFor 非空时以它为前缀的命令返回错误。
func stubRun(t *testing.T, failFor string) *[][]string {
	t.Helper()
	old := run
	var calls [][]string
	run = func(_ context.Context, _ io.Writer, name string, args ...string) error {
		argv := append([]string{name}, args...)
		calls = append(calls, argv)
		line := strings.Join(argv, " ")
		if failFor != "" && strings.HasPrefix(line, failFor) {
			return errors.New("模拟失败")
		}
		return nil
	}
	t.Cleanup(func() { run = old })
	return &calls
}

// writeExe 造一个「真实存在的可执行文件」供 Install 校验。
func writeExe(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shareclip-server")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("写测试可执行文件: %v", err)
	}
	return path
}

func hasLine(text, line string) bool {
	for _, l := range strings.Split(text, "\n") {
		if l == line {
			return true
		}
	}
	return false
}

func TestDefaultUnitName(t *testing.T) {
	if got := DefaultUnitName(); got != "share-clip" {
		t.Fatalf("DefaultUnitName() = %q", got)
	}
}

func TestStateDirAndDefaultDBPath(t *testing.T) {
	if got := StateDir(Options{Scope: ScopeSystem}); got != "/var/lib/share-clip" {
		t.Fatalf("系统级 StateDir = %q", got)
	}
	if got := DefaultDBPath(Options{Scope: ScopeSystem}); got != "/var/lib/share-clip/shareclip.db" {
		t.Fatalf("系统级 DefaultDBPath = %q", got)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	wantDir := filepath.Join(home, ".local", "share", "share-clip")
	if got := StateDir(Options{Scope: ScopeUser}); got != wantDir {
		t.Fatalf("用户级 StateDir = %q，want %q", got, wantDir)
	}
	if got := DefaultDBPath(Options{Scope: ScopeUser}); got != filepath.Join(wantDir, "shareclip.db") {
		t.Fatalf("用户级 DefaultDBPath = %q", got)
	}
}

func TestUnitPathDefaultsAndOverride(t *testing.T) {
	path, err := UnitPath(Options{Scope: ScopeSystem})
	if err != nil {
		t.Fatalf("UnitPath: %v", err)
	}
	if path != "/etc/systemd/system/share-clip.service" {
		t.Fatalf("系统级 UnitPath = %q", path)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err = UnitPath(Options{Scope: ScopeUser, Unit: "clip"})
	if err != nil {
		t.Fatalf("UnitPath: %v", err)
	}
	want := filepath.Join(home, ".config", "systemd", "user", "clip.service")
	if path != want {
		t.Fatalf("用户级 UnitPath = %q，want %q", path, want)
	}

	dir := t.TempDir()
	path, err = UnitPath(Options{Scope: ScopeSystem, Unit: "clip", UnitDir: dir})
	if err != nil {
		t.Fatalf("UnitPath: %v", err)
	}
	if path != filepath.Join(dir, "clip.service") {
		t.Fatalf("UnitDir 覆盖失效: %q", path)
	}

	if _, err := UnitPath(Options{Scope: ScopeSystem, Unit: "a/b"}); err == nil {
		t.Fatal("含 / 的单元名应当报错")
	}
}

func TestUnitFileSystemScope(t *testing.T) {
	o := Options{
		Scope:        ScopeSystem,
		ExecPath:     "/usr/local/bin/shareclip-server",
		Addr:         ":9000",
		HistoryLimit: 500,
		MaxPayload:   32 << 20,
	}
	text := UnitFile(o)
	wantLines := []string{
		"[Unit]",
		"Description=share-clip clipboard sharing server",
		"Documentation=https://github.com/micookie2/share-clip",
		"After=network-online.target",
		"Wants=network-online.target",
		"[Service]",
		"Type=simple",
		"ExecStart=/usr/local/bin/shareclip-server -a :9000 -d /var/lib/share-clip/shareclip.db -l 500 -m 33554432",
		"Restart=always",
		"RestartSec=2",
		"StandardOutput=journal",
		"StandardError=journal",
		"StateDirectory=share-clip",
		"WorkingDirectory=/var/lib/share-clip",
		"DynamicUser=yes",
		"NoNewPrivileges=true",
		"PrivateTmp=true",
		"ProtectSystem=full",
		"ProtectHome=true",
		"ReadWritePaths=/var/lib/share-clip",
		"[Install]",
		"WantedBy=multi-user.target",
	}
	for _, l := range wantLines {
		if !hasLine(text, l) {
			t.Errorf("系统级单元缺少行 %q\n---\n%s", l, text)
		}
	}
	if strings.Contains(text, "\nUser=") {
		t.Errorf("RunAs 为空时不应出现 User=:\n%s", text)
	}
	if strings.Contains(text, "%h") {
		t.Errorf("系统级单元不应出现 %%h:\n%s", text)
	}
}

func TestUnitFileSystemScopeRunAs(t *testing.T) {
	text := UnitFile(Options{Scope: ScopeSystem, ExecPath: "/usr/local/bin/shareclip-server", RunAs: "share"})
	if !hasLine(text, "User=share") {
		t.Fatalf("缺少 User=share:\n%s", text)
	}
	if strings.Contains(text, "DynamicUser") {
		t.Fatalf("指定 RunAs 后不应再有 DynamicUser:\n%s", text)
	}
}

func TestUnitFileUserScope(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	o := Options{
		Scope:        ScopeUser,
		ExecPath:     "/usr/local/bin/shareclip-server",
		Addr:         ":9000",
		HistoryLimit: 500,
		MaxPayload:   32 << 20,
	}
	text := UnitFile(o)
	wantLines := []string{
		"ExecStart=/usr/local/bin/shareclip-server -a :9000 -d %h/.local/share/share-clip/shareclip.db -l 500 -m 33554432",
		"ExecStartPre=/bin/mkdir -p %h/.local/share/share-clip",
		"WorkingDirectory=%h/.local/share/share-clip",
		"NoNewPrivileges=true",
		"PrivateTmp=true",
		"WantedBy=default.target",
	}
	for _, l := range wantLines {
		if !hasLine(text, l) {
			t.Errorf("用户级单元缺少行 %q\n---\n%s", l, text)
		}
	}
	for _, forbidden := range []string{"ProtectHome", "DynamicUser", "StateDirectory", "User=", "multi-user.target"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("用户级单元不应包含 %q:\n%s", forbidden, text)
		}
	}
}

func TestUnitFileQuoting(t *testing.T) {
	o := Options{
		Scope:        ScopeSystem,
		ExecPath:     "/opt/my tools/shareclip-server",
		Addr:         ":9000",
		DBPath:       "/srv/My Data/shareclip.db",
		HistoryLimit: 10,
		MaxPayload:   1024,
	}
	want := `ExecStart="/opt/my tools/shareclip-server" -a :9000 -d "/srv/My Data/shareclip.db" -l 10 -m 1024`
	if !hasLine(UnitFile(o), want) {
		t.Fatalf("带空格的参数未正确加引号:\n%s", UnitFile(o))
	}
	if got := quoteArg(`a"b\c`); got != `"a\"b\\c"` {
		t.Fatalf("quoteArg = %s", got)
	}
	if got := quoteArg(""); got != `""` {
		t.Fatalf("quoteArg(\"\") = %s", got)
	}
	if got := quoteArg("/var/lib/share-clip/shareclip.db"); got != "/var/lib/share-clip/shareclip.db" {
		t.Fatalf("普通路径不应加引号: %s", got)
	}
}

// TestPercentEscaping 覆盖字面量 % 的转义：systemd 把 % 当说明符前缀，
// 用户路径里的 % 必须写成 %%，而用户级单元刻意保留的 %h 不能被转义。
func TestPercentEscaping(t *testing.T) {
	sys := Options{Scope: ScopeSystem, ExecPath: "/opt/100%/shareclip-server", DBPath: "/srv/100%/shareclip.db"}
	text := UnitFile(sys)
	if !hasLine(text, "ExecStart=/opt/100%%/shareclip-server -a :9000 -d /srv/100%%/shareclip.db -l 500 -m 33554432") {
		t.Fatalf("系统级字面量 %% 未转义:\n%s", text)
	}
	if !hasLine(text, "ReadWritePaths=/srv/100%%") {
		t.Fatalf("ReadWritePaths 字面量 %% 未转义:\n%s", text)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	user := Options{Scope: ScopeUser, ExecPath: "/usr/local/bin/shareclip-server", DBPath: filepath.Join(home, "100%", "shareclip.db")}
	if !hasLine(UnitFile(user), "ExecStart=/usr/local/bin/shareclip-server -a :9000 -d %h/100%%/shareclip.db -l 500 -m 33554432") {
		t.Fatalf("用户级 %%h 应保留、其余 %% 应转义:\n%s", UnitFile(user))
	}
}

func TestCommands(t *testing.T) {
	sys := Commands(Options{Scope: ScopeSystem}, ActionInstall)
	if len(sys) != 2 || strings.Join(sys[0], " ") != "systemctl daemon-reload" ||
		strings.Join(sys[1], " ") != "systemctl enable --now share-clip" {
		t.Fatalf("系统级安装命令 = %v", sys)
	}

	usr := Commands(Options{Scope: ScopeUser, Unit: "clip", NoStart: true}, ActionInstall)
	if strings.Join(usr[0], " ") != "systemctl --user daemon-reload" ||
		strings.Join(usr[1], " ") != "systemctl --user enable clip" {
		t.Fatalf("用户级 NoStart 命令 = %v", usr)
	}

	del := Commands(Options{Scope: ScopeUser, Unit: "clip"}, ActionUninstall)
	want := []string{
		"systemctl --user disable --now clip",
		"systemctl --user daemon-reload",
		"systemctl --user reset-failed clip",
	}
	for i, w := range want {
		if strings.Join(del[i], " ") != w {
			t.Fatalf("卸载命令[%d] = %v，want %s", i, del[i], w)
		}
	}
}

func TestPlan(t *testing.T) {
	dir := t.TempDir()
	o := Options{Scope: ScopeUser, Unit: "clip", UnitDir: dir, ExecPath: "/usr/local/bin/shareclip-server"}
	path, text, cmds, err := Plan(o, ActionInstall)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if path != filepath.Join(dir, "clip.service") || text != UnitFile(o) || len(cmds) != 2 {
		t.Fatalf("Plan = %q, %d 字节, %v", path, len(text), cmds)
	}
}

func TestInstallWritesUnitAndRunsCommands(t *testing.T) {
	dir := t.TempDir()
	exe := writeExe(t)
	o := Options{Scope: ScopeUser, UnitDir: dir, ExecPath: exe, Addr: ":9000", HistoryLimit: 500, MaxPayload: 32 << 20}

	calls := stubRun(t, "")
	var out strings.Builder
	if err := Install(context.Background(), o, &out); err != nil {
		t.Fatalf("Install: %v", err)
	}

	unitPath := filepath.Join(dir, "share-clip.service")
	data, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("读取单元文件: %v", err)
	}
	if string(data) != UnitFile(o) {
		t.Fatalf("单元内容不符:\n%s", data)
	}
	info, err := os.Stat(unitPath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("单元文件权限 = %v", info.Mode().Perm())
	}
	if len(*calls) != 2 || strings.Join((*calls)[0], " ") != "systemctl --user daemon-reload" ||
		strings.Join((*calls)[1], " ") != "systemctl --user enable --now share-clip" {
		t.Fatalf("执行的命令 = %v", *calls)
	}
	for _, want := range []string{"已写入 systemd 单元", "执行: systemctl --user daemon-reload", "执行: systemctl --user enable --now"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("输出缺少 %q:\n%s", want, out.String())
		}
	}
}

func TestInstallRefusesOverwriteUnlessForced(t *testing.T) {
	dir := t.TempDir()
	exe := writeExe(t)
	unitPath := filepath.Join(dir, "share-clip.service")
	if err := os.WriteFile(unitPath, []byte("旧单元"), 0o644); err != nil {
		t.Fatalf("准备旧单元: %v", err)
	}
	stubRun(t, "")

	o := Options{Scope: ScopeUser, UnitDir: dir, ExecPath: exe}
	if err := Install(context.Background(), o, io.Discard); err == nil {
		t.Fatal("已存在单元文件时应拒绝覆盖")
	} else if !strings.Contains(err.Error(), "--force") {
		t.Fatalf("错误应提示 --force: %v", err)
	}

	o.Force = true
	if err := Install(context.Background(), o, io.Discard); err != nil {
		t.Fatalf("--force 覆盖失败: %v", err)
	}
	data, _ := os.ReadFile(unitPath)
	if string(data) != UnitFile(o) {
		t.Fatalf("--force 未写入新内容:\n%s", data)
	}
}

func TestInstallValidatesExecPath(t *testing.T) {
	dir := t.TempDir()
	stubRun(t, "")
	base := Options{Scope: ScopeUser, UnitDir: dir}

	cases := []struct {
		name string
		exe  string
		want []string
	}{
		{"相对路径", "bin/shareclip-server", []string{"绝对路径"}},
		{"不存在", filepath.Join(dir, "nope"), []string{"不存在"}},
		{"目录", dir, []string{"不是普通文件"}},
		{"go run 临时产物", "/tmp/go-build123/b001/exe/shareclip-server", []string{"go run", "go build"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := base
			o.ExecPath = c.exe
			err := Install(context.Background(), o, io.Discard)
			if err == nil {
				t.Fatalf("应拒绝 ExecPath %q", c.exe)
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("错误 %q 应包含 %q", err, w)
				}
			}
			if _, statErr := os.Stat(filepath.Join(dir, "share-clip.service")); statErr == nil {
				t.Error("校验失败时不应写入单元文件")
			}
		})
	}
}

func TestUninstallRemovesUnit(t *testing.T) {
	dir := t.TempDir()
	unitPath := filepath.Join(dir, "share-clip.service")
	if err := os.WriteFile(unitPath, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatalf("准备单元: %v", err)
	}
	calls := stubRun(t, "")
	o := Options{Scope: ScopeUser, UnitDir: dir}

	var out strings.Builder
	if err := Uninstall(context.Background(), o, &out); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(unitPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("单元文件未被删除: %v", err)
	}
	if len(*calls) != 3 {
		t.Fatalf("命令数 = %v", *calls)
	}
	if !strings.Contains(out.String(), "已删除 systemd 单元") {
		t.Errorf("输出缺少删除提示:\n%s", out.String())
	}
}

func TestUninstallMissingUnitAndFailures(t *testing.T) {
	dir := t.TempDir()
	// disable 与 reset-failed 都失败：应当被容忍，daemon-reload 成功即可。
	calls := stubRun(t, "systemctl --user disable")
	run = func(_ context.Context, _ io.Writer, name string, args ...string) error {
		argv := append([]string{name}, args...)
		*calls = append(*calls, argv)
		line := strings.Join(argv, " ")
		if strings.HasPrefix(line, "systemctl --user disable") || strings.HasPrefix(line, "systemctl --user reset-failed") {
			return errors.New("模拟失败")
		}
		return nil
	}

	o := Options{Scope: ScopeUser, UnitDir: dir}
	var out strings.Builder
	if err := Uninstall(context.Background(), o, &out); err != nil {
		t.Fatalf("Uninstall 应容忍可忽略的失败: %v", err)
	}
	if !strings.Contains(out.String(), "单元文件不存在，跳过") {
		t.Errorf("输出应提示单元文件缺失:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "忽略失败") {
		t.Errorf("输出应提示已忽略失败:\n%s", out.String())
	}
	if len(*calls) != 3 {
		t.Fatalf("命令数 = %v", *calls)
	}
}

func TestUninstallPurgeStateDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateDir := filepath.Join(home, ".local", "share", "share-clip")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("准备数据目录: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "shareclip.db"), []byte("db"), 0o644); err != nil {
		t.Fatalf("准备数据库: %v", err)
	}
	unitDir := t.TempDir()
	stubRun(t, "")

	o := Options{Scope: ScopeUser, UnitDir: unitDir, Purge: true}
	var out strings.Builder
	if err := Uninstall(context.Background(), o, &out); err != nil {
		t.Fatalf("Uninstall --purge: %v", err)
	}
	if _, err := os.Stat(stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("数据目录未被删除: %v", err)
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatalf("家目录本身必须保留: %v", err)
	}
	if !strings.Contains(out.String(), "已删除数据目录") {
		t.Errorf("输出缺少清理提示:\n%s", out.String())
	}
}

func TestCheckRoot(t *testing.T) {
	if err := checkRoot(Options{Scope: ScopeUser}); err != nil {
		t.Fatalf("用户级不该要求 root: %v", err)
	}
	err := checkRoot(Options{Scope: ScopeSystem})
	if os.Geteuid() == 0 {
		if err != nil {
			t.Fatalf("root 下系统级应通过: %v", err)
		}
		return
	}
	if err == nil {
		t.Fatal("非 root 安装系统级单元必须报错")
	}
	for _, want := range []string{"root", "sudo", "--user"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误 %q 应包含 %q", err, want)
		}
	}
}

// 额外启动参数必须原样（且逐个加引号）出现在 ExecStart 末尾，用户可以靠它
// 传入 install 没有专门开关的服务器参数（例如 -q）。
func TestUnitFileExtraArgs(t *testing.T) {
	o := Options{
		Scope:        ScopeSystem,
		ExecPath:     "/usr/local/bin/shareclip-server",
		ExtraArgs:    []string{"-q", "-n", "100% 机器", "带\"引号"},
		HistoryLimit: 500,
		MaxPayload:   32 << 20,
	}
	text := UnitFile(o)
	want := `ExecStart=/usr/local/bin/shareclip-server -a :9000 -d /var/lib/share-clip/shareclip.db ` +
		`-l 500 -m 33554432 -q -n "100%% 机器" "带\"引号"`
	if !hasLine(text, want) {
		t.Fatalf("ExecStart 未按预期追加额外参数:\n%s", text)
	}
}

// 额外参数里塞换行不能逃出引号变成新的单元指令（注入防护）。
func TestUnitFileExtraArgsCannotInjectDirectives(t *testing.T) {
	text := UnitFile(Options{
		Scope:     ScopeUser,
		ExecPath:  "/usr/local/bin/shareclip-server",
		ExtraArgs: []string{"x\n[Service]\nUser=root"},
	})
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "User=") {
			t.Fatalf("额外参数注入了单元指令:\n%s", text)
		}
	}
	if !strings.Contains(text, `x\n[Service]\nUser=root`) {
		t.Fatalf("换行应被转义后留在同一行:\n%s", text)
	}
}

// --no-auth 必须写进 ExecStart，且在固定参数之后、ExtraArgs 之前：附加参数
// 仍可覆盖它（Go 的 flag 包对重复标量选项「后者生效」）。
func TestUnitFileNoAuth(t *testing.T) {
	o := Options{
		Scope:        ScopeSystem,
		ExecPath:     "/usr/local/bin/shareclip-server",
		NoAuth:       true,
		HistoryLimit: 500,
		MaxPayload:   32 << 20,
	}
	want := "ExecStart=/usr/local/bin/shareclip-server -a :9000 -d /var/lib/share-clip/shareclip.db " +
		"-l 500 -m 33554432 --no-auth"
	if !hasLine(UnitFile(o), want) {
		t.Fatalf("ExecStart 未包含 --no-auth:\n%s", UnitFile(o))
	}
	// 不开启时不能凭空多出这个参数。
	o.NoAuth = false
	if strings.Contains(UnitFile(o), "--no-auth") {
		t.Fatalf("未开启 NoAuth 却写入了 --no-auth:\n%s", UnitFile(o))
	}
}
