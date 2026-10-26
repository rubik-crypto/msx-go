// Package authsession 为网页应用提供管理员登录的会话层：HMAC 签名 cookie、
// 有效期与凭据版本、按来源限制登录失败次数，以及只放行白名单路径的登录闸门。
//
// 它不负责保存账号和校验密码：调用方自己保存账号与密码哈希（可配合 password 包），
// 登录成功后调用 Manager.Issue 拿到令牌并写进 cookie，之后用 Guard 包住需要登录的处理链。
//
// 最小用法：
//
//	mgr, err := authsession.New(secret, authsession.Options{})
//	if err != nil {
//		log.Fatal(err)
//	}
//	// 登录成功后
//	token, err := mgr.Issue(username, version)
//	if err != nil {
//		// 处理错误
//	}
//	mgr.SetCookie(w, r, token)
//
//	// 需要登录的接口
//	guard := &authsession.Guard{
//		Manager: mgr,
//		Current: func() (string, int64, bool) {
//			// 返回当前账号、凭据版本，以及是否已经设置过账号
//			return currentUsername(), currentVersion(), configured()
//		},
//		Public: func(r *http.Request) bool {
//			return r.URL.Path == "/login" || r.URL.Path == "/api/login"
//		},
//	}
//	http.Handle("/", guard.Wrap(mux))
//
// 修改密码时把凭据版本加一并换一个签名密钥，旧 cookie 会立即失效；
// 令牌本身是无状态的，退出登录只能清掉当前浏览器里的 cookie，
// 要让所有设备下线，应当提升凭据版本。
package authsession
