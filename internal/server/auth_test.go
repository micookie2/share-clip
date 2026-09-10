package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/micookie2/share-clip/internal/auth"
	"github.com/micookie2/share-clip/internal/protocol"
	"github.com/micookie2/share-clip/internal/server"
)

const testKey = "test-key-0123456789abcdef"

// startAuthServer 起一个固定 key 的 server，用于验证鉴权本身。
func startAuthServer(t *testing.T, key string) *server.Server {
	t.Helper()
	s, err := server.New(server.Config{
		Addr:         ":0",
		DBPath:       filepath.Join(t.TempDir(), "history.db"),
		HistoryLimit: 500,
		MaxPayload:   32 << 20,
		Key:          key,
		Quiet:        true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// dialWS 用给定的主 key 发起一次 WebSocket 握手；key 为空表示不带凭据。
func dialWS(t *testing.T, base, key string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var opts *websocket.DialOptions
	if key != "" {
		opts = &websocket.DialOptions{
			HTTPHeader: http.Header{auth.HeaderName: []string{key}},
		}
	}
	return websocket.Dial(ctx, wsURL(base), opts)
}

func TestAccessKeyRequired(t *testing.T) {
	s := startAuthServer(t, testKey)
	defer s.Close()
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	if !s.AuthEnabled() || s.AccessKey() != testKey || s.KeyGenerated() {
		t.Fatalf("auth state: enabled=%v key=%q generated=%v", s.AuthEnabled(), s.AccessKey(), s.KeyGenerated())
	}

	// 不带凭据的 API 请求被拒。
	resp, err := http.Get(ts.URL + "/api/status")
	if err != nil {
		t.Fatalf("GET /api/status: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no key: status = %d, want 401", resp.StatusCode)
	}

	// 带错误 key 同样被拒。
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/status", nil)
	req.Header.Set(auth.HeaderName, "wrong-key")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET with bad key: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad key: status = %d, want 401", resp.StatusCode)
	}

	// 带正确 key 放行。
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/api/status", nil)
	req.Header.Set(auth.HeaderName, testKey)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET with key: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("good key: status = %d, want 200", resp.StatusCode)
	}

	// Authorization: Bearer 也是合法的携带方式。
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/api/status", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET with bearer: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bearer: status = %d, want 200", resp.StatusCode)
	}

	// 未带 key 的 WebSocket 握手失败；带上 key 后正常收到 welcome。
	if _, resp, err := dialWS(t, ts.URL, ""); err == nil {
		t.Fatal("WebSocket without key should fail")
	} else if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("WebSocket without key: resp = %+v, err = %v", resp, err)
	}
	conn, _, err := dialWS(t, ts.URL, testKey)
	if err != nil {
		t.Fatalf("WebSocket with key: %v", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(int64(protocol.DefaultMaxPayload) + protocol.FrameOverhead)
	send(t, conn, &protocol.Msg{Kind: protocol.KindHello, ClientID: "id-a", ClientName: "host-a"})
	if m := readMsg(t, conn, 5*time.Second); m == nil || m.Kind != protocol.KindWelcome {
		t.Fatalf("no welcome with valid key, got %+v", m)
	}
}

func TestAdminPageRequiresLogin(t *testing.T) {
	s := startAuthServer(t, testKey)
	defer s.Close()
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// 浏览器导航拿到登录页而不是管理页。
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	req.Header.Set("Accept", "text/html")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /: status = %d, want 401", resp.StatusCode)
	}
	if !strings.Contains(body, "访问 key") || !strings.Contains(body, "/api/login") {
		t.Fatalf("login page missing expected content: %q", truncate(body))
	}

	// 登录页本身可以匿名打开。
	resp, err = http.Get(ts.URL + "/login")
	if err != nil {
		t.Fatalf("GET /login: %v", err)
	}
	readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /login: status = %d, want 200", resp.StatusCode)
	}
}

func TestLoginFlowIssuesSessionCookie(t *testing.T) {
	s := startAuthServer(t, testKey)
	defer s.Close()
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	client := &http.Client{Jar: jar}

	// 错误的 key 不能换到会话。
	if code := postLogin(t, client, ts.URL, "nope"); code != http.StatusUnauthorized {
		t.Fatalf("login with bad key: status = %d, want 401", code)
	}
	if got := getStatus(t, client, ts.URL); got != http.StatusUnauthorized {
		t.Fatalf("after failed login: status = %d, want 401", got)
	}

	// 正确的 key 换到 HttpOnly 会话 Cookie，之后的 API 调用靠它放行。
	loginBody, _ := json.Marshal(map[string]string{"key": testKey})
	loginResp, err := client.Post(ts.URL+"/api/login", "application/json", bytes.NewReader(loginBody))
	if err != nil {
		t.Fatalf("POST /api/login: %v", err)
	}
	readAll(t, loginResp)
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("login: status = %d, want 200", loginResp.StatusCode)
	}
	var session *http.Cookie
	for _, c := range loginResp.Cookies() {
		if c.Name == auth.CookieName {
			session = c
		}
	}
	if session == nil || !session.HttpOnly || session.Value == testKey {
		t.Fatalf("session cookie = %+v, want an HttpOnly token distinct from the key", session)
	}
	if got := getStatus(t, client, ts.URL); got != http.StatusOK {
		t.Fatalf("after login: status = %d, want 200", got)
	}

	// 管理页 HTML 现在也能拿到了。
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	req.Header.Set("Accept", "text/html")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET / with session: %v", err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "share-clip") {
		t.Fatalf("GET / with session: status = %d, body = %q", resp.StatusCode, truncate(body))
	}

	// 退出后会话立刻失效。
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/logout", nil)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("POST /api/logout: %v", err)
	}
	readAll(t, resp)
	if got := getStatus(t, client, ts.URL); got != http.StatusUnauthorized {
		t.Fatalf("after logout: status = %d, want 401", got)
	}
}

func TestQueryKeyBecomesSessionCookie(t *testing.T) {
	s := startAuthServer(t, testKey)
	defer s.Close()
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := noRedirect.Get(ts.URL + "/?key=" + testKey)
	if err != nil {
		t.Fatalf("GET /?key=: %v", err)
	}
	readAll(t, resp)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("GET /?key=: status = %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/" {
		t.Fatalf("Location = %q, want /", loc)
	}
	var token string
	for _, c := range resp.Cookies() {
		if c.Name == auth.CookieName {
			token = c.Value
		}
	}
	if token == "" {
		t.Fatal("no session cookie issued for ?key=")
	}

	// 拿这个 Cookie 就能打开管理页。
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET / with session: %v", err)
	}
	readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / with session: status = %d, want 200", resp.StatusCode)
	}

	// 错误的 ?key= 不会换到 Cookie，也不会被放行。
	resp, err = noRedirect.Get(ts.URL + "/?key=wrong")
	if err != nil {
		t.Fatalf("GET /?key=wrong: %v", err)
	}
	readAll(t, resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET with wrong ?key=: status = %d, want 401", resp.StatusCode)
	}
}

func TestGeneratedKeyAndNoAuth(t *testing.T) {
	gen := startAuthServer(t, "")
	defer gen.Close()
	if !gen.AuthEnabled() || !gen.KeyGenerated() {
		t.Fatalf("generated key: enabled=%v generated=%v", gen.AuthEnabled(), gen.KeyGenerated())
	}
	if len(gen.AccessKey()) < 24 {
		t.Fatalf("generated key too short: %q", gen.AccessKey())
	}
	other := startAuthServer(t, "")
	defer other.Close()
	if gen.AccessKey() == other.AccessKey() {
		t.Fatal("two servers generated the same key")
	}

	// --no-auth：不带任何凭据也能访问 API 与 WebSocket，且不发放 key。
	open, err := server.New(server.Config{
		Addr:       ":0",
		DBPath:     filepath.Join(t.TempDir(), "history.db"),
		MaxPayload: 32 << 20,
		NoAuth:     true,
		Quiet:      true,
	})
	if err != nil {
		t.Fatalf("New(NoAuth): %v", err)
	}
	defer open.Close()
	if open.AuthEnabled() || open.AccessKey() != "" {
		t.Fatalf("NoAuth: enabled=%v key=%q", open.AuthEnabled(), open.AccessKey())
	}
	ts := httptest.NewServer(open.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/status")
	if err != nil {
		t.Fatalf("GET /api/status: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("NoAuth API: status = %d, want 200", resp.StatusCode)
	}
	conn, _, err := dialWS(t, ts.URL, "")
	if err != nil {
		t.Fatalf("NoAuth WebSocket: %v", err)
	}
	defer conn.CloseNow()
}

// --- 小工具 -----------------------------------------------------------------

func postLogin(t *testing.T, client *http.Client, base, key string) int {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"key": key})
	resp, err := client.Post(base+"/api/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/login: %v", err)
	}
	readAll(t, resp)
	return resp.StatusCode
}

func getStatus(t *testing.T, client *http.Client, base string) int {
	t.Helper()
	resp, err := client.Get(base + "/api/status")
	if err != nil {
		t.Fatalf("GET /api/status: %v", err)
	}
	readAll(t, resp)
	return resp.StatusCode
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	return buf.String()
}

func truncate(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
