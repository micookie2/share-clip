package auth_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/micookie2/share-clip/internal/auth"
)

func TestGenerateProducesDistinctURLSafeKeys(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 32; i++ {
		k, err := auth.Generate()
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if len(k) != 32 {
			t.Fatalf("key length = %d (%q), want 32", len(k), k)
		}
		if strings.ContainsAny(k, "+/= ") {
			t.Fatalf("key %q is not URL-safe base64", k)
		}
		if seen[k] {
			t.Fatalf("duplicate key %q", k)
		}
		seen[k] = true
	}
}

func TestEqual(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"abc", "abc", true},
		{" abc ", "abc", true},
		{"abc", "abd", false},
		{"", "", false},
		{"abc", "", false},
		{"", "abc", false},
	}
	for _, c := range cases {
		if got := auth.Equal(c.a, c.b); got != c.want {
			t.Errorf("Equal(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestKeyFromRequest(t *testing.T) {
	newReq := func(mutate func(*http.Request)) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/?key=from-query", nil)
		if mutate != nil {
			mutate(r)
		}
		return r
	}

	// 请求头优先于 query。
	r := newReq(func(r *http.Request) { r.Header.Set(auth.HeaderName, "from-header") })
	if got := auth.KeyFromRequest(r); got != "from-header" {
		t.Fatalf("header should win, got %q", got)
	}
	// Authorization: Bearer。
	r = newReq(func(r *http.Request) { r.Header.Set("Authorization", "bearer from-bearer") })
	if got := auth.KeyFromRequest(r); got != "from-bearer" {
		t.Fatalf("bearer = %q, want from-bearer", got)
	}
	// 都没有时回落到 query 并去掉空白。
	r = newReq(func(r *http.Request) { r.URL.RawQuery = "key=%20from-query%20" })
	if got := auth.KeyFromRequest(r); got != "from-query" {
		t.Fatalf("query = %q, want from-query", got)
	}
	// 完全没有凭据。
	if got := auth.KeyFromRequest(httptest.NewRequest(http.MethodGet, "/", nil)); got != "" {
		t.Fatalf("empty request key = %q, want empty", got)
	}
}

func TestCookieToken(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: " tok "})
	if got := auth.CookieToken(r); got != "tok" {
		t.Fatalf("CookieToken = %q, want tok", got)
	}
	if got := auth.CookieToken(httptest.NewRequest(http.MethodGet, "/", nil)); got != "" {
		t.Fatalf("CookieToken without cookie = %q, want empty", got)
	}
}

func TestSessions(t *testing.T) {
	s := auth.NewSessions(0)
	token, err := s.Issue()
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if !s.Valid(token) {
		t.Fatal("fresh token should be valid")
	}
	if s.Valid("") || s.Valid("nope") {
		t.Fatal("unknown tokens must not validate")
	}
	s.Revoke(token)
	if s.Valid(token) {
		t.Fatal("revoked token must not validate")
	}

	// 过期令牌立刻失效。
	short := auth.NewSessions(time.Millisecond)
	expiring, err := short.Issue()
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	if short.Valid(expiring) {
		t.Fatal("expired token must not validate")
	}
}
