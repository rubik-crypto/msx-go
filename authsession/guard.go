package authsession

import (
	"net/http"
)

// Guard 是登录闸门：除了 Public 明确放行的请求，其他请求都必须带上
// 有效的登录 cookie 才能进入下一层。
type Guard struct {
	// Manager 不能为空。
	Manager *Manager
	// Current 返回当前管理员的账号名、凭据版本，以及是否已经设置过管理员账号。
	// 没有设置账号时，除了 Public 放行的路径，所有请求都会被拒绝。
	Current func() (username string, version int64, configured bool)
	// Public 返回 true 的请求无需登录，例如登录页、登录接口、健康检查，
	// 以及使用自己的一套令牌鉴权的回调接口。
	Public func(r *http.Request) bool
	// Unauthorized 可选，用来输出调用方自己的错误格式。为空时返回一段纯文本 401。
	Unauthorized func(w http.ResponseWriter, r *http.Request, configured bool)
}

// Wrap 包住下一层处理链。Manager 或 Current 为空时会直接 panic，
// 避免把「忘了配置」悄悄变成「完全不校验」。
func (g *Guard) Wrap(next http.Handler) http.Handler {
	if g == nil || g.Manager == nil || g.Current == nil {
		panic("authsession: Guard 需要 Manager 和 Current")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g.Public != nil && g.Public(r) {
			next.ServeHTTP(w, r)
			return
		}
		username, version, configured := g.Current()
		if configured {
			if _, ok := g.Manager.Session(r, username, version); ok {
				next.ServeHTTP(w, r)
				return
			}
		}
		if g.Unauthorized != nil {
			g.Unauthorized(w, r, configured)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "请先登录", http.StatusUnauthorized)
	})
}
