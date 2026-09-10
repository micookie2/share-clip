// Package auth 提供 share-clip 的访问鉴权原语：主访问 key 的生成与恒定时间
// 比较，以及管理页会话令牌的内存存储。
//
// 约束这套设计的几个事实：
//   - key 默认由 server 每次启动随机生成（只有显式给了 --key 才固定），因此它
//     天然是「本次运行的访问口令」，不需要也不应该落盘；
//   - 客户端（WebSocket）能自定义握手请求头，直接携带主 key 即可；
//   - 浏览器做不到给页面请求加自定义头，所以登录成功后改用 HttpOnly Cookie 里
//     的会话令牌，令牌只存在于内存，进程重启就全部失效。
//
// 本包不接触磁盘、不读配置，纯计算加一层互斥保护的内存表，便于单测。
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 凭据在 HTTP 请求里的几个入口。
const (
	// HeaderName 是客户端与脚本携带主 key 的请求头。
	HeaderName = "X-Shareclip-Key"
	// QueryParam 让浏览器可以直接用 `/?key=...` 打开管理页，免去手输。
	QueryParam = "key"
	// CookieName 保存登录成功后的会话令牌（不是主 key 本身）。
	CookieName = "shareclip_session"
)

// keyBytes 是随机凭据的熵：24 字节 = 192 位，base64url 后 32 个字符——短到
// 能手抄，又远超暴力枚举的范围。
const keyBytes = 24

// DefaultSessionTTL 是会话令牌的默认有效期。令牌本来就会随进程消失，这个
// 上限只是给长期不关的标签页兜底。
const DefaultSessionTTL = 30 * 24 * time.Hour

// Generate 生成一个新的随机主 key（base64url，无填充）。
func Generate() (string, error) {
	return randomToken()
}

// Equal 以恒定时间比较两个凭据。任一侧为空（未配置或未提供）一律判为不相等，
// 避免「鉴权关闭」与「空 key」意外互相匹配。
func Equal(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// KeyFromRequest 提取请求里以「主 key」形式给出的凭据：自定义请求头优先，
// 其次是 Authorization: Bearer，最后是 query（浏览器首次打开页面最方便）。
// Cookie 里的会话令牌不走这里，见 CookieToken。
func KeyFromRequest(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get(HeaderName)); v != "" {
		return v
	}
	if v := bearer(r.Header.Get("Authorization")); v != "" {
		return v
	}
	return strings.TrimSpace(r.URL.Query().Get(QueryParam))
}

// CookieToken 返回浏览器带来的会话令牌；没有则返回空字符串。
func CookieToken(r *http.Request) string {
	c, err := r.Cookie(CookieName)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(c.Value)
}

// bearer 从 "Bearer xxx" 里取出 xxx（scheme 不区分大小写）。
func bearer(header string) string {
	const prefix = "Bearer "
	if len(header) > len(prefix) && strings.EqualFold(header[:len(prefix)], prefix) {
		return strings.TrimSpace(header[len(prefix):])
	}
	return ""
}

// Sessions 是内存里的会话令牌表：登录成功发一个令牌，浏览器之后靠它访问管理
// 接口。零值不可用，请用 NewSessions 创建。
type Sessions struct {
	ttl time.Duration

	mu sync.Mutex
	m  map[string]time.Time // token -> 过期时刻
}

// NewSessions 创建一个会话表；ttl <= 0 时使用 DefaultSessionTTL。
func NewSessions(ttl time.Duration) *Sessions {
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	return &Sessions{ttl: ttl, m: make(map[string]time.Time)}
}

// Issue 生成并记住一个新令牌。
func (s *Sessions) Issue() (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(now)
	s.m[token] = now.Add(s.ttl)
	return token, nil
}

// Valid 报告令牌是否存在且未过期。
func (s *Sessions) Valid(token string) bool {
	token = strings.TrimSpace(token)
	if token == "" {
		return false
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.m[token]
	if !ok {
		return false
	}
	if now.After(exp) {
		delete(s.m, token)
		return false
	}
	return true
}

// Revoke 让一个令牌立刻失效（退出登录）。
func (s *Sessions) Revoke(token string) {
	token = strings.TrimSpace(token)
	if token == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, token)
}

// sweepLocked 顺手清掉过期令牌，避免长期运行且反复登录时表无限增长。
// 调用方必须已持有 s.mu。
func (s *Sessions) sweepLocked(now time.Time) {
	for token, exp := range s.m {
		if now.After(exp) {
			delete(s.m, token)
		}
	}
}

// randomToken 生成 base64url 编码的随机令牌。
func randomToken() (string, error) {
	b := make([]byte, keyBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
