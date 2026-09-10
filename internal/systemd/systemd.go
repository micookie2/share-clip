// Package systemd 负责把 share-clip server 安装成 systemd 服务。
//
// 生成单元文件是纯函数：UnitFile/UnitPath/Commands/Plan 只做字符串与路径计算，
// 不碰文件系统、也不调用 systemctl，因此可以放心单测。真正产生副作用的是
// Install/Uninstall，它们执行的每一条外部命令都经过包级变量 run，测试替换
// run 之后即可在没有任何 systemd 的机器上验证完整流程。
package systemd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Scope 选择单元的作用域：系统级单元放在 /etc/systemd/system，由 PID 1 运行；
// 用户级单元放在 ~/.config/systemd/user，由用户自己的 systemd --user 运行。
type Scope int

const (
	ScopeSystem Scope = iota
	ScopeUser
)

// Action 是本包支持的两个动作，用来挑选要执行的 systemctl 命令。
type Action int

const (
	ActionInstall Action = iota
	ActionUninstall
)

// 默认值。与 server.DefaultHistory、protocol.DefaultMaxPayload 保持一致；
// 这里再写一份是为了不让本包依赖 server（那会把 sqlite、websocket 一并拖进来）。
const (
	defaultHistory    = 500
	defaultMaxPayload = 32 << 20
	defaultAddr       = ":9000"
)

// 单元文件里的固定内容。
const (
	unitDescription   = "share-clip clipboard sharing server"
	unitDocumentation = "https://github.com/micookie2/share-clip"
	serviceSuffix     = ".service"
	systemStateDir    = "/var/lib/share-clip"
	userStateSubdir   = ".local/share/share-clip"
	unitFileMode      = 0o644
	unitDirMode       = 0o755
)

// Options 描述要生成的单元以及如何安装它。
type Options struct {
	Scope        Scope    // 单元作用域
	Unit         string   // 单元名（不含 ".service"）；空表示 share-clip
	ExecPath     string   // shareclip-server 可执行文件的绝对路径
	Addr         string   // server -a 的值，如 ":9000"
	DBPath       string   // server -d 的值；空则按作用域推导
	HistoryLimit int      // server -l 的值
	MaxPayload   int      // server -m 的值
	RunAs        string   // 非空 => User=<RunAs>；空 => DynamicUser=yes（仅系统级）
	NoAuth       bool     // 追加 --no-auth：关闭 server 的访问鉴权
	ExtraArgs    []string // 追加到 ExecStart 末尾的额外参数（如 -q），逐个加引号
	UnitDir      string   // 覆盖单元目录（测试用）
	NoStart      bool     // 只 enable，不立即启动
	Force        bool     // 覆盖已存在的单元文件
	Purge        bool     // 仅 Uninstall：同时删除数据目录
}

// run 是执行外部命令的唯一入口，测试通过替换它来避免真的调用 systemctl。
var run = func(ctx context.Context, out io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = out
	cmd.Stderr = out
	return cmd.Run()
}

// DefaultUnitName 返回默认单元名（不含 ".service"）。
func DefaultUnitName() string { return "share-clip" }

// StateDir 返回服务的数据目录：系统级为 /var/lib/share-clip，用户级为
// <home>/.local/share/share-clip；家目录不可知时用户级返回空字符串。
func StateDir(o Options) string {
	if o.Scope == ScopeUser {
		home := homeDir()
		if home == "" {
			return ""
		}
		return filepath.Join(home, filepath.FromSlash(userStateSubdir))
	}
	return systemStateDir
}

// DefaultDBPath 返回按作用域推导出的默认数据库路径。
func DefaultDBPath(o Options) string {
	return filepath.Join(StateDir(o), "shareclip.db")
}

// UnitPath 返回单元文件的落盘路径：优先使用 UnitDir，否则按作用域取默认目录。
func UnitPath(o Options) (string, error) {
	name, err := unitName(o)
	if err != nil {
		return "", err
	}
	dir := o.UnitDir
	if dir == "" {
		if o.Scope == ScopeUser {
			home := homeDir()
			if home == "" {
				return "", errors.New("无法确定家目录，请用 --unit-dir 指定用户级单元目录")
			}
			dir = filepath.Join(home, ".config", "systemd", "user")
		} else {
			dir = "/etc/systemd/system"
		}
	}
	return filepath.Join(dir, name+serviceSuffix), nil
}

// UnitFile 返回完整的 [Unit]/[Service]/[Install] 单元文本。
func UnitFile(o Options) string {
	var b strings.Builder
	line := func(s string) { b.WriteString(s); b.WriteByte('\n') }

	line("[Unit]")
	line("Description=" + unitDescription)
	line("Documentation=" + unitDocumentation)
	line("After=network-online.target")
	line("Wants=network-online.target")
	line("")

	line("[Service]")
	line("Type=simple")
	line("ExecStart=" + execStart(o))
	line("Restart=always")
	line("RestartSec=2")
	line("StandardOutput=journal")
	line("StandardError=journal")

	if o.Scope == ScopeUser {
		// 用户级：用 %h 说明符指向家目录，避免写死用户名；不能加 ProtectHome，
		// 否则 systemd --user 反而访问不到 %h 下的文件。
		dir := unitStateDir(o)
		line("ExecStartPre=/bin/mkdir -p " + quoteArg(dir))
		line("WorkingDirectory=" + quoteArg(dir))
		line("NoNewPrivileges=true")
		line("PrivateTmp=true")
	} else {
		line("StateDirectory=share-clip")
		line("WorkingDirectory=" + systemStateDir)
		if o.RunAs != "" {
			line("User=" + o.RunAs)
		} else {
			line("DynamicUser=yes")
		}
		line("NoNewPrivileges=true")
		line("PrivateTmp=true")
		line("ProtectSystem=full")
		line("ProtectHome=true")
		line("ReadWritePaths=" + quoteArg(readWriteDir(o)))
	}

	line("")
	line("[Install]")
	if o.Scope == ScopeUser {
		line("WantedBy=default.target")
	} else {
		line("WantedBy=multi-user.target")
	}
	return b.String()
}

// Commands 返回该动作会执行的 systemctl 命令，供 --dry-run 展示。
func Commands(o Options, action Action) [][]string {
	name := unitNameOr(o)
	if action == ActionUninstall {
		return [][]string{
			systemctl(o, "disable", "--now", name),
			systemctl(o, "daemon-reload"),
			systemctl(o, "reset-failed", name),
		}
	}
	enable := []string{"enable"}
	if !o.NoStart {
		enable = append(enable, "--now")
	}
	enable = append(enable, name)
	return [][]string{
		systemctl(o, "daemon-reload"),
		systemctl(o, enable...),
	}
}

// Plan 汇总 --dry-run 需要的一切：单元文件路径、文本与将执行的命令。
// 它不读也不写任何文件。
func Plan(o Options, action Action) (unitPath, unitText string, cmds [][]string, err error) {
	unitPath, err = UnitPath(o)
	if err != nil {
		return "", "", nil, err
	}
	return unitPath, UnitFile(o), Commands(o, action), nil
}

// Install 写入单元文件并启用服务。系统级需要 root；ExecPath 必须真实存在。
func Install(ctx context.Context, o Options, out io.Writer) error {
	if err := checkRoot(o); err != nil {
		return err
	}
	if err := checkExecPath(o.ExecPath); err != nil {
		return err
	}
	unitPath, err := UnitPath(o)
	if err != nil {
		return err
	}
	if _, err := os.Stat(unitPath); err == nil {
		if !o.Force {
			return fmt.Errorf("单元文件已存在: %s（如需覆盖请加 --force）", unitPath)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("检查单元文件失败 %s: %w", unitPath, err)
	}
	if err := os.MkdirAll(filepath.Dir(unitPath), unitDirMode); err != nil {
		return fmt.Errorf("创建单元目录失败 %s: %w", filepath.Dir(unitPath), err)
	}
	if err := os.WriteFile(unitPath, []byte(UnitFile(o)), unitFileMode); err != nil {
		return fmt.Errorf("写入单元文件失败 %s: %w", unitPath, err)
	}
	if err := os.Chmod(unitPath, unitFileMode); err != nil {
		return fmt.Errorf("设置单元文件权限失败 %s: %w", unitPath, err)
	}
	fmt.Fprintf(out, "已写入 systemd 单元: %s\n", unitPath)
	for _, argv := range Commands(o, ActionInstall) {
		if err := runCommand(ctx, out, argv); err != nil {
			return err
		}
	}
	return nil
}

// Uninstall 停止并禁用服务、删除单元文件，然后重载 systemd。
// disable 与 reset-failed 的失败会被容忍（服务可能本来就没在跑）。
func Uninstall(ctx context.Context, o Options, out io.Writer) error {
	if err := checkRoot(o); err != nil {
		return err
	}
	unitPath, err := UnitPath(o)
	if err != nil {
		return err
	}
	cmds := Commands(o, ActionUninstall)
	if err := runCommand(ctx, out, cmds[0]); err != nil {
		fmt.Fprintf(out, "忽略失败（服务可能未运行）: %v\n", err)
	}
	if err := os.Remove(unitPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(out, "单元文件不存在，跳过: %s\n", unitPath)
		} else {
			return fmt.Errorf("删除单元文件失败 %s: %w", unitPath, err)
		}
	} else {
		fmt.Fprintf(out, "已删除 systemd 单元: %s\n", unitPath)
	}
	if err := runCommand(ctx, out, cmds[1]); err != nil {
		return err
	}
	if err := runCommand(ctx, out, cmds[2]); err != nil {
		fmt.Fprintf(out, "忽略失败: %v\n", err)
	}
	if o.Purge {
		return purgeStateDir(o, out)
	}
	return nil
}

// runCommand 先打印命令再执行，并把子进程输出直接写到 out。
func runCommand(ctx context.Context, out io.Writer, argv []string) error {
	fmt.Fprintf(out, "执行: %s\n", strings.Join(argv, " "))
	if err := run(ctx, out, argv[0], argv[1:]...); err != nil {
		return fmt.Errorf("命令执行失败 %s: %w", strings.Join(argv, " "), err)
	}
	return nil
}

// checkRoot 在系统级作用域下要求 root 权限；用户级不需要。
func checkRoot(o Options) error {
	if o.Scope == ScopeSystem && os.Geteuid() != 0 {
		return errors.New("安装系统级 systemd 服务需要 root 权限: 请用 sudo 重新运行，或改用用户级服务（加 --user）")
	}
	return nil
}

// checkExecPath 校验待安装的可执行文件：必须是绝对路径且指向真实存在的普通文件。
func checkExecPath(path string) error {
	if path == "" {
		return errors.New("未能确定 shareclip-server 可执行文件路径")
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("可执行文件路径必须是绝对路径: %s", path)
	}
	// 先认 go run 的临时产物：这类路径同样可能已经不存在，给出更贴切的提示。
	if strings.Contains(path, "go-build") {
		return fmt.Errorf("可执行文件看起来是 go run 的临时产物: %s；该文件会被清理，重启后服务将无法启动，请先 go build 安装到固定路径，再用该路径重新运行 install", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("可执行文件不存在: %s", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("可执行文件不是普通文件: %s", path)
	}
	return nil
}

// purgeStateDir 删除数据目录。只删除本工具按作用域推导出的那个目录，
// 绝不会删掉 /、家目录本身或预期之外的路径。
func purgeStateDir(o Options, out io.Writer) error {
	dir := StateDir(o)
	if dir == "" || !filepath.IsAbs(dir) {
		return fmt.Errorf("拒绝删除数据目录（路径不可信）: %q", dir)
	}
	clean := filepath.Clean(dir)
	if clean != dir || clean == string(filepath.Separator) || clean == homeDir() ||
		filepath.Base(clean) != DefaultUnitName() {
		return fmt.Errorf("拒绝删除数据目录（不在预期范围内）: %s", dir)
	}
	info, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(out, "数据目录不存在，跳过: %s\n", dir)
		return nil
	}
	if err != nil {
		return fmt.Errorf("检查数据目录失败 %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("数据路径不是目录，拒绝删除: %s", dir)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("删除数据目录失败 %s: %w", dir, err)
	}
	fmt.Fprintf(out, "已删除数据目录: %s\n", dir)
	return nil
}

// --- 纯计算辅助函数 ---------------------------------------------------------

// execStart 拼出 ExecStart 行，参数按 systemd 规则各自加引号。
// ExtraArgs 追加在末尾：Go 的 flag 包对重复出现的标量选项「后者生效」，
// 于是 `--exec-arg=-l --exec-arg=1000` 也能覆盖前面写死的值。
func execStart(o Options) string {
	args := []string{
		escapePercent(o.ExecPath),
		"-a", escapePercent(addr(o)),
		"-d", unitDBPath(o),
		"-l", strconv.Itoa(historyLimit(o)),
		"-m", strconv.Itoa(maxPayload(o)),
	}
	if o.NoAuth {
		// 写在固定参数之后、ExtraArgs 之前：Go 的 flag 包对重复标量选项「后者
		// 生效」，所以用户仍可用 --exec-arg 覆盖上面任何一项，包括这一项。
		args = append(args, "--no-auth")
	}
	for _, extra := range o.ExtraArgs {
		// 走 escapePercent + quoteArg：换行会被转义成 \n 留在引号内，注入不了
		// 新的单元指令；% 也会变成 %%，不会被 systemd 当说明符展开。
		args = append(args, escapePercent(extra))
	}
	for i, a := range args {
		args[i] = quoteArg(a)
	}
	return strings.Join(args, " ")
}

// dbPath 返回实际使用的数据库路径：Options.DBPath 优先，否则按作用域推导。
func dbPath(o Options) string {
	if o.DBPath != "" {
		return o.DBPath
	}
	return DefaultDBPath(o)
}

// unitDBPath 返回 ExecStart 里 -d 的值：用户级用 %h 代替家目录，并转义字面量 %。
func unitDBPath(o Options) string {
	if o.Scope == ScopeUser {
		return withHomeSpecifier(dbPath(o))
	}
	return escapePercent(dbPath(o))
}

// readWriteDir 返回系统级单元需要放行的可写目录（数据库所在目录）。
func readWriteDir(o Options) string {
	db := dbPath(o)
	if !filepath.IsAbs(db) {
		// 相对路径由 WorkingDirectory 决定去向，放行整个状态目录即可。
		return systemStateDir
	}
	return escapePercent(filepath.Dir(db))
}

// unitStateDir 返回写进单元的数据库目录：用户级用 %h 替换家目录。
func unitStateDir(o Options) string {
	dir := StateDir(o)
	if dir == "" {
		return "%h/" + userStateSubdir
	}
	return withHomeSpecifier(dir)
}

// withHomeSpecifier 把路径开头的家目录换成 systemd 的 %h 说明符，其余部分里的
// 字面量 % 会被转义，避免 systemd 把它当成非法说明符。
func withHomeSpecifier(p string) string {
	home := homeDir()
	if home == "" || p == "" {
		return escapePercent(p)
	}
	if p == home {
		return "%h"
	}
	if strings.HasPrefix(p, home+string(filepath.Separator)) {
		return "%h" + escapePercent(p[len(home):])
	}
	return escapePercent(p)
}

// escapePercent 把 % 写成 %%，让 systemd 按字面量处理。
func escapePercent(s string) string { return strings.ReplaceAll(s, "%", "%%") }

// homeDir 返回当前用户的家目录，取不到时返回空字符串。
func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	return ""
}

// unitName 返回并校验单元名（不含 ".service"）。
func unitName(o Options) (string, error) {
	name := unitNameOr(o)
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`+" \t\r\n") {
		return "", fmt.Errorf("非法的单元名: %q", o.Unit)
	}
	return name, nil
}

func unitNameOr(o Options) string {
	if o.Unit != "" {
		return o.Unit
	}
	return DefaultUnitName()
}

func addr(o Options) string {
	if o.Addr != "" {
		return o.Addr
	}
	return defaultAddr
}

func historyLimit(o Options) int {
	if o.HistoryLimit > 0 {
		return o.HistoryLimit
	}
	return defaultHistory
}

func maxPayload(o Options) int {
	if o.MaxPayload > 0 {
		return o.MaxPayload
	}
	return defaultMaxPayload
}

// systemctl 组装一条 systemctl（用户级带 --user）命令。
func systemctl(o Options, args ...string) []string {
	argv := make([]string, 0, len(args)+2)
	argv = append(argv, "systemctl")
	if o.Scope == ScopeUser {
		argv = append(argv, "--user")
	}
	return append(argv, args...)
}

// quoteArg 按 systemd 的 ExecStart 规则给单个参数加引号：只含安全字符时原样
// 返回，否则用双引号包起来并转义 " 与 \，换行/制表符写成 C 风格转义。
// 这里不处理 %：调用方已经用 escapePercent 转义了用户数据，而 %h 说明符必须
// 保持原样交给 systemd 展开。
func quoteArg(s string) string {
	if s == "" {
		return `""`
	}
	if !strings.ContainsFunc(s, needsQuote) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// needsQuote 判断字符是否必须被引号保护。
func needsQuote(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return false
	}
	switch r {
	case '/', '.', '_', '-', ':', '=', '+', ',', '@', '%':
		return false
	}
	return true
}
