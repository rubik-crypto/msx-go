package authsession

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGuard(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	mgr := testManager(t, &now)
	token, err := mgr.Issue("owner", 2)
	if err != nil {
		t.Fatal(err)
	}

	configured := true
	unauthorizedCalls := 0
	guard := &Guard{
		Manager: mgr,
		Current: func() (string, int64, bool) {
			return "owner", 2, configured
		},
		Public: func(r *http.Request) bool {
			return r.URL.Path == "/login"
		},
		Unauthorized: func(w http.ResponseWriter, r *http.Request, isConfigured bool) {
			unauthorizedCalls++
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("denied"))
		},
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	handler := guard.Wrap(next)

	// 公开路径不需要 cookie。
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/login", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("公开路径应该放行: %d %q", rec.Code, rec.Body.String())
	}

	// 没有 cookie 的受保护路径被拒绝。
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/api/state", nil))
	if rec.Code != http.StatusUnauthorized || rec.Body.String() != "denied" {
		t.Fatalf("没有登录应该被拒绝: %d %q", rec.Code, rec.Body.String())
	}

	// 带有效 cookie 的请求放行。
	req := httptest.NewRequest(http.MethodGet, "http://example.com/api/state", nil)
	req.AddCookie(&http.Cookie{Name: mgr.CookieName(), Value: token})
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("有效登录应该放行: %d %q", rec.Code, rec.Body.String())
	}

	// 还没设置管理员账号时，受保护路径同样拒绝，并把 configured=false 传给回调。
	configured = false
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/api/state", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未设置账号时应该拒绝: %d", rec.Code)
	}
	if unauthorizedCalls != 2 {
		t.Fatalf("Unauthorized 回调次数不对: %d", unauthorizedCalls)
	}
}

func TestGuardPanicsWithoutConfig(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("缺少 Manager 或 Current 时应该 panic")
		}
	}()
	var guard *Guard
	guard.Wrap(http.NotFoundHandler())
}

func TestGuardDefaultUnauthorized(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	mgr := testManager(t, &now)
	guard := &Guard{
		Manager: mgr,
		Current: func() (string, int64, bool) { return "owner", 1, true },
	}
	rec := httptest.NewRecorder()
	guard.Wrap(http.NotFoundHandler()).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "http://example.com/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("默认应该返回 401，实际 %d", rec.Code)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("401 响应不应该被缓存")
	}
}
