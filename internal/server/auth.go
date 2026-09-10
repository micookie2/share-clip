package server

import (
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/micookie2/share-clip/internal/auth"
)

// loginPage 是未授权浏览器导航时返回的登录页。它自带样式与脚本，不引用任何
// 需要鉴权的静态资源，因此可以整页直接下发给未登录用户。
//
//go:embed login.html
var loginPage []byte

// 与登录相关的路由。这三个端点必须在 guard 里放行，否则用户永远进不来。
const (
	loginPagePath = "/login"
	loginAPIPath  = "/api/login"
	logoutAPIPath = "/api/logout"
)

// AuthEnabled 报告本次运行是否要求访问 key（即是否未使用 --no-auth）。
func (s *Server) AuthEnabled() bool { return s.key != "" }

// AccessKey 返回本次运行使用的访问 key；鉴权关闭时为空字符串。
func (s *Server) AccessKey() string { return s.key }

// KeyGenerated 报告访问 key 是否是启动时随机生成的（而非 --key 指定），
// 仅用于启动日志的措辞。
func (s *Server) KeyGenerated() bool { return s.keyGen }

// guard 是管理页、REST API 与 WebSocket 端点的统一鉴权入口。
//
// 凭据有两种形态，缺一不可：
//   - 主 key：客户端与脚本通过 X-Shareclip-Key（或 Authorization: Bearer、
//     ?key=）携带；
//   - 会话令牌：浏览器登录后由 HttpOnly Cookie 携带。
//
// 未通过时，浏览器导航会拿到登录页，其余请求（API、WebSocket 握手）统一收到
// 401，便于调用方判断而不是拿到一段 HTML。
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if !s.AuthEnabled() {
			next.ServeHTTP(w, r)
			return
		}
		// 登录/退出本身不能要求先登录。
		switch r.URL.Path {
		case loginPagePath, loginAPIPath, logoutAPIPath:
			next.ServeHTTP(w, r)
			return
		}
		if !s.authorize(r) {
			s.deny(w, r)
			return
		}
		// 用 `/?key=...` 直接进来：把主 key 换成会话 Cookie，并把地址栏里的
		// key 抹掉，免得它留在浏览历史、Referer 和旁人的视线里。
		if r.Method == http.MethodGet && strings.TrimSpace(r.URL.Query().Get(auth.QueryParam)) != "" {
			if token, err := s.sessions.Issue(); err == nil {
				s.setSessionCookie(w, token)
				if r.URL.Path == "/" {
					http.Redirect(w, r, "/", http.StatusFound)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// authorize 判定请求是否携带了有效凭据。鉴权关闭时恒为 true。
func (s *Server) authorize(r *http.Request) bool {
	if !s.AuthEnabled() {
		return true
	}
	if auth.Equal(auth.KeyFromRequest(r), s.key) {
		return true
	}
	return s.sessions.Valid(auth.CookieToken(r))
}

// deny 拒绝一次未授权的请求：浏览器导航给登录页，其余给 401 JSON。
func (s *Server) deny(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && wantsHTML(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write(loginPage)
		return
	}
	writeErr(w, http.StatusUnauthorized, "未授权：缺少或错误的访问 key")
}

// wantsHTML 判断请求是否来自浏览器地址栏导航（此时给登录页才有意义）。
func wantsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// handleLoginPage 返回登录页；已登录的浏览器直接回管理页。
func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if s.authorize(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(loginPage)
}

// handleLogin 校验页面提交的 key，成功后下发会话 Cookie。
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.AuthEnabled() {
		// 鉴权关闭时无所谓登录，直接放行，页面照常进入管理页。
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	var body struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if !auth.Equal(body.Key, s.key) {
		writeErr(w, http.StatusUnauthorized, "访问 key 不正确")
		return
	}
	token, err := s.sessions.Issue()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "无法创建会话")
		return
	}
	s.setSessionCookie(w, token)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleLogout 注销当前会话并清掉 Cookie。
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.sessions.Revoke(auth.CookieToken(r))
	s.clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// setSessionCookie 下发会话令牌。HttpOnly 让页面脚本读不到它；SameSite=Lax
// 让跨站 POST 带不上它，管理页的写接口因此天然免疫 CSRF。
func (s *Server) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     auth.CookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(auth.DefaultSessionTTL / time.Second),
	})
}

// clearSessionCookie 让浏览器立刻丢弃会话 Cookie。
func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     auth.CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}
