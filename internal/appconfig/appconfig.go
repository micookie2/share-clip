// Package appconfig 负责 client 桌面模式（托盘 + 本地界面）的持久化配置。
//
// 双击运行的客户端没有命令行参数可传，服务器地址、机器名这些设置必须能存
// 下来：本包把它们放在用户配置目录下的 share-clip/config.json——
// Windows 是 %AppData%\share-clip\config.json，Linux 是
// ~/.config/share-clip/config.json（遵循 XDG，见 os.UserConfigDir）。
//
// 配置只是「用户上一次的选择」这一事实来源，不参与任何校验以外的逻辑：
// 读失败（文件不存在、内容损坏）一律退回默认值，好让界面还能打开把错误
// 显示出来，而不是启动即退出。
package appconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/micookie2/share-clip/internal/protocol"
)

// 默认值：与命令行选项保持一致，避免同一个设置有两个不同的默认解释。
const (
	DefaultPollMs     = 1000
	DefaultMaxPayload = protocol.DefaultMaxPayload
	DefaultPort       = "9000"
)

// dirName 是配置目录名，fileName 是配置文件名，logFileName 是桌面模式日志文件。
const (
	dirName     = "share-clip"
	fileName    = "config.json"
	logFileName = "client.log"
)

// Config 是 client 桌面模式的全部持久化设置。
type Config struct {
	Server     string `json:"server"`               // server 地址 host:port
	Name       string `json:"name,omitempty"`       // 本机显示名，空表示用主机名
	PollMs     int    `json:"pollMs,omitempty"`     // 剪贴板兜底轮询间隔（毫秒）
	MaxPayload int    `json:"maxPayload,omitempty"` // 单条内容最大字节数
}

// Default 返回全默认配置（server 为空：首次启动时由用户在界面里填写）。
func Default() Config {
	return Config{
		PollMs:     DefaultPollMs,
		MaxPayload: DefaultMaxPayload,
	}
}

// WithDefaults 补齐零值字段并去掉首尾空白，使旧版本写下的、缺字段的配置
// 文件也能正常使用。
func (c Config) WithDefaults() Config {
	c.Server = strings.TrimSpace(c.Server)
	c.Name = strings.TrimSpace(c.Name)
	if c.PollMs <= 0 {
		c.PollMs = DefaultPollMs
	}
	if c.MaxPayload <= 0 {
		c.MaxPayload = DefaultMaxPayload
	}
	return c
}

// Dir 返回配置目录（可能尚不存在）。
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("找不到用户配置目录: %w", err)
	}
	return filepath.Join(base, dirName), nil
}

// Path 返回配置文件路径。
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, fileName), nil
}

// LogPath 返回桌面模式的日志文件路径（与配置文件同目录）。托盘模式没有可见
// 的控制台，日志文件是排查问题的现场。
func LogPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, logFileName), nil
}

// Load 读取配置文件。文件不存在时返回默认配置且 err 为 nil；文件存在但内容
// 损坏时返回默认配置**和**错误，让调用方既能启动也能把问题显示给用户。
func Load() (Config, error) {
	path, err := Path()
	if err != nil {
		return Default(), err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return Default(), fmt.Errorf("读取配置 %s 失败: %w", path, err)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return Default(), fmt.Errorf("解析配置 %s 失败: %w", path, err)
	}
	return c.WithDefaults(), nil
}

// Save 原子地把配置写入用户配置目录（先写临时文件再改名，避免中途断电留下
// 半个文件）。目录与文件的权限都收窄到当前用户。
func (c Config) Save() error {
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("创建配置目录失败: %w", err)
	}
	raw, err := json.MarshalIndent(c.WithDefaults(), "", "  ")
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}
	raw = append(raw, '\n')

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("写入配置失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("保存配置失败: %w", err)
	}
	return nil
}

// NormalizeServer 把用户在界面上随手输入的地址整理成 client 需要的 host:port：
// 容忍带 ws://、http:// 前缀、带尾部 "/" 或 "/ws"、省略端口、IPv6 字面量等写法。
// 返回空地址时给出错误，便于界面直接提示。
func NormalizeServer(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("请填写 server 地址，例如 192.168.1.10:9000")
	}
	// 去掉协议前缀与路径：只保留 host[:port]。
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+len("://"):]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("server 地址不完整，例如 192.168.1.10:9000")
	}

	host, port, err := splitHostPort(s)
	if err != nil {
		return "", err
	}
	if port == "" {
		port = DefaultPort
	}
	if n, err := strconv.Atoi(port); err != nil || n <= 0 || n > 65535 {
		return "", fmt.Errorf("端口 %q 不合法", port)
	}
	if host == "" {
		return "", errors.New("server 地址缺少主机名，例如 192.168.1.10:9000")
	}
	if strings.ContainsAny(host, " \t") {
		return "", fmt.Errorf("server 地址 %q 含有空格", host)
	}
	return net.JoinHostPort(host, port), nil
}

// splitHostPort 把 s 拆成主机与端口，兼容以下写法：
//
//	192.168.1.10:9000   host:port
//	192.168.1.10        省略端口
//	[fe80::1]:9000      带方括号的 IPv6
//	fe80::1             裸 IPv6（无端口，不能按冒号切分）
func splitHostPort(s string) (host, port string, err error) {
	if strings.HasPrefix(s, "[") {
		// 带方括号：交给标准库，它知道端口在哪。
		if strings.Contains(s, "]:") {
			h, p, err := net.SplitHostPort(s)
			if err != nil {
				return "", "", fmt.Errorf("server 地址 %q 不合法: %w", s, err)
			}
			return h, p, nil
		}
		return strings.TrimSuffix(strings.TrimPrefix(s, "["), "]"), "", nil
	}
	switch strings.Count(s, ":") {
	case 0:
		return s, "", nil
	case 1:
		h, p, err := net.SplitHostPort(s)
		if err != nil {
			return "", "", fmt.Errorf("server 地址 %q 不合法: %w", s, err)
		}
		return h, p, nil
	default:
		// 多个冒号且没有方括号：只能是不带端口的 IPv6 字面量。用 ParseIP
		// 兜住 "host:9000:9000" 这类多写了端口的输入，否则会被当成合法主机
		// 名带进连接阶段，只能以更难懂的错误收场。
		if net.ParseIP(s) == nil {
			return "", "", fmt.Errorf("server 地址 %q 不合法，正确写法如 192.168.1.10:9000", s)
		}
		return s, "", nil
	}
}

// Validate 校验配置是否可用于启动 agent，返回的字符串可直接展示给用户。
func Validate(c Config) error {
	if strings.TrimSpace(c.Server) == "" {
		return errors.New("尚未配置 server 地址")
	}
	if _, err := NormalizeServer(c.Server); err != nil {
		return err
	}
	if c.PollMs != 0 && c.PollMs < 50 {
		return errors.New("轮询间隔不能小于 50 毫秒")
	}
	if c.MaxPayload != 0 && c.MaxPayload < 1024 {
		return errors.New("单条内容上限不能小于 1024 字节")
	}
	return nil
}

// Normalized 返回把 server 地址整理过、其余字段补齐默认值之后的配置，供真正
// 拿去连接前使用。
func (c Config) Normalized() (Config, error) {
	c = c.WithDefaults()
	addr, err := NormalizeServer(c.Server)
	if err != nil {
		return c, err
	}
	c.Server = addr
	return c, nil
}
