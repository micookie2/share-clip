// Package keyfile 读写 server 的固定访问 key 文件。
//
// 文件放在安装目录（与 shareclip-server 可执行文件同目录），默认名
// shareclip.key。内容采用 systemd EnvironmentFile 的原生格式：
//
//	SHARECLIP_KEY=<key>
//
// 这样单元只需写一行 EnvironmentFile=，由 systemd 以 root 身份读取并注入
// 服务进程——DynamicUser 场景下服务进程不需要（也拿不到）文件读权限，
// key 也不再出现在 ExecStart / systemctl cat / ps 里。
//
// 读取时保持宽松：带前缀的行、带双引号的值、以及「文件里只有一行裸 key」
// 的手写格式都能解析，方便非 systemd 用户配合 -K/--key-file 使用。
package keyfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/micookie2/share-clip/internal/auth"
)

// FileName 是 key 文件的默认名；envPrefix 是 systemd EnvironmentFile 的赋值前缀。
const (
	FileName  = "shareclip.key"
	envPrefix = "SHARECLIP_KEY="
)

// keyPerms 是新文件的权限：只有属主（安装者/systemd 的 root）可读。
const keyPerms = 0o600

// Generate 生成一个新的随机访问 key（与服务随机模式同源：base64url，32 字符）。
func Generate() (string, error) {
	return auth.Generate()
}

// PathFor 返回安装目录 dir 下默认 key 文件的路径。
func PathFor(dir string) string { return filepath.Join(dir, FileName) }

// Valid 检查用户手填的 key：长度适中、无换行制表符、无首尾空白。
// 换行会拆散 EnvironmentFile 的行，首尾空白会在各处被 Trim 掉造成隐性不一致。
func Valid(key string) error {
	if key == "" {
		return fmt.Errorf("key 不能为空")
	}
	if strings.TrimSpace(key) != key {
		return fmt.Errorf("key 不能包含首尾空白")
	}
	if strings.ContainsAny(key, "\n\r\t") {
		return fmt.Errorf("key 不能包含换行或制表符")
	}
	if n := len(key); n < 4 || n > 256 {
		return fmt.Errorf("key 长度需在 4~256 字符之间（当前 %d）", n)
	}
	return nil
}

// Parse 解析 key 文件内容：优先 SHARECLIP_KEY= 行（含双引号值），否则把去
// 空白后的整个内容当作裸 key。解析不出非空 key 时报错。
func Parse(content string) (string, error) {
	s := strings.TrimSpace(content)
	if s == "" {
		return "", fmt.Errorf("key 文件内容为空")
	}
	line := s
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i]) // 取第一行，容忍尾部注释/空行
	}
	if strings.HasPrefix(line, envPrefix) {
		v := strings.TrimPrefix(line, envPrefix)
		if len(v) >= 2 && strings.HasPrefix(v, `"`) && strings.HasSuffix(v, `"`) {
			var b strings.Builder
			rs := []rune(v[1 : len(v)-1])
			for i := 0; i < len(rs); i++ {
				if rs[i] == '\\' && i+1 < len(rs) {
					i++
					switch rs[i] {
					case 'n':
						b.WriteByte('\n')
					case 'r':
						b.WriteByte('\r')
					case 't':
						b.WriteByte('\t')
					default:
						b.WriteRune(rs[i])
					}
					continue
				}
				b.WriteRune(rs[i])
			}
			v = b.String()
		}
		if v == "" {
			return "", fmt.Errorf("key 文件里的 %s 值为空", envPrefix)
		}
		return v, nil
	}
	if strings.ContainsAny(s, "\n") {
		return "", fmt.Errorf("无法解析 key 文件：缺少 %s 前缀且不止一行", envPrefix)
	}
	return line, nil
}

// Format 生成要写入文件的内容（含结尾换行）。安全字符集内直接裸写，
// 其余按 systemd 规则加双引号并转义 \ 与 "。
func Format(key string) string {
	if safeKeyValue(key) {
		return envPrefix + key + "\n"
	}
	var b strings.Builder
	b.WriteString(envPrefix)
	b.WriteByte('"')
	for _, r := range key {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteString("\"\n")
	return b.String()
}

// safeKeyValue 判断值能否不加引号：字母数字与几个无歧义符号。刻意排除
// $（systemd 会做变量展开）和空白、引号等。
func safeKeyValue(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '-', r == '.', r == ':', r == '@', r == '/', r == '+', r == '%':
		default:
			return false
		}
	}
	return true
}

// Read 读取并解析 key 文件。
func Read(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return Parse(string(data))
}

// Write 以 SHARECLIP_KEY= 格式写入 key 文件：目录不存在则创建；新文件权限
// 0600，覆盖写保留原文件的属主与权限。写入走临时文件 + rename，读者永远
// 看不到半截内容。
func Write(path, key string) error {
	if err := Valid(key); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建 key 文件目录失败 %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, FileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("创建临时文件失败 %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后是无害的 no-op
	if _, err := tmp.WriteString(Format(key)); err != nil {
		tmp.Close()
		return fmt.Errorf("写入 key 文件失败: %w", err)
	}
	if err := tmp.Chmod(keyPerms); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("写入 key 文件失败 %s: %w", path, err)
	}
	// rename 替换的是整个 inode：新文件固定 0600、属主为执行写入的用户。
	// 这正好符合要求——读文件的是以 root 激活服务的 systemd（或手动跑
	// server 的人），服务进程本身通过环境变量拿到 key，不需要文件权限。
	return nil
}
