// Package webpush 向浏览器推送系统通知：生成并保管 VAPID 密钥，按 RFC 8291
// 加密消息内容，再按 RFC 8292 签名后发送到浏览器厂商的推送服务。
//
// 它不依赖第三方推送平台，也不会经过调用方的业务服务器之外的服务；
// 适合给网页应用增加「页面关闭或锁屏时也能收到通知」的能力。
//
// 典型用法：
//
//	key, err := webpush.LoadOrCreateKey(filepath.Join(dataDir, "vapid.json"))
//	if err != nil {
//		log.Fatal(err)
//	}
//	// 把 key.PublicKey() 交给前端，作为 pushManager.subscribe 的 applicationServerKey
//
//	client := webpush.NewClient(key, "mailto:ops@example.com")
//	err = client.Send(ctx, webpush.Subscription{
//		Endpoint: sub.Endpoint,
//		P256DH:   sub.Keys.P256DH,
//		Auth:     sub.Keys.Auth,
//	}, []byte(`{"title":"提示","body":"有一件新事情"}`))
//	if errors.Is(err, webpush.ErrGone) {
//		// 这台设备已经取消订阅，应当从存储里删掉
//	}
//
// 推送地址只允许浏览器厂商的域名，避免订阅地址被替换成内网地址后
// 让服务端去请求内部网络。密钥文件请放在长期保存的数据目录里，
// 重新部署后继续使用同一个公钥，已订阅的设备才不会失效。
package webpush
