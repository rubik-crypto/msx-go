// Package realip 从代理头里挑出**可信的**访客地址。
//
// 站点挂在反向代理后面时，拿来做限流键、访问统计去重、按 IP 查城市的那个地址
// 不能是「来访者自称的地址」。常见的写法是取 X-Forwarded-For 的**第一个**值，
// 而第一个值恰恰是客户端自己就能写的：
//
//	curl -H 'X-Forwarded-For: 1.2.3.4' https://站点/api/login
//
// 每换一个值就是一个新的限流桶，登录框上那句「连着错 5 次锁 5 分钟」等于没有；
// 访问统计也会被一次请求一个「新访客」灌水。
//
// 正确的方向是**从右往左看**：链路上每个代理都会往右边追加「自己看到的对端地址」，
// 所以越靠右越可信（常见的 `$proxy_add_x_forwarded_for` 就是「原样保留 + 追加」，
// 客户端伪造的那一段只会留在最左边）。从右往左跳过内网地址（反代自己那一跳
// 通常就是内网），第一个公网地址才是真实的访客。
//
//	ip := realip.FromRequest(r)
//
// 拿不到任何可用值时返回 TCP 连接的对端地址（r.RemoteAddr 的主机部分）——
// 那一个谁也伪造不了。
//
// # 一条限制，用之前先知道
//
// X-Forwarded-For 只有一段时，这里没法分辨它是「反代追加的真实客户端」还是
// 「客户端自己写的一段」——两种情况的字节完全一样。按「反代总会追加」这个
// 常见约定，单段就当成真实地址用。
//
// 如果你的部署里反代是**覆写**而不是追加（例如
// `proxy_set_header X-Forwarded-For $remote_addr`），客户端伪造的那一段会被
// 直接替换掉，那反而更干净；真正要小心的是「本站前面没有反代」而站点直接
// 暴露在公网——那时候客户端可以随便写这个头，务必改成只信 RemoteAddr
// （直接调 Host(r.RemoteAddr)），别调 FromRequest。
package realip

import (
	"net"
	"net/http"
	"strings"
)

// From 从三个来源里挑出访客地址：X-Forwarded-For、X-Real-Ip、连接对端地址。
// 参数就是那三个头的原始字符串（没有就传空串），remoteAddr 形如 "1.2.3.4:5678"。
//
// 返回的一定是「最可信的那个」，不会是客户端的自称（除非整条链都是内网，
// 那时候只能退回最靠近本站的一跳）。全都拿不到时返回空串。
func From(xff, xRealIP, remoteAddr string) string {
	if xff != "" {
		parts := strings.Split(xff, ",")
		// 从右往左找第一个公网地址。
		for i := len(parts) - 1; i >= 0; i-- {
			s := strings.TrimSpace(parts[i])
			if s == "" {
				continue
			}
			if ip := net.ParseIP(s); ip != nil && IsPublicIP(ip) {
				return s
			}
		}
		// 都是内网 / 认不出来的：取最右边那一个（最靠近本站的那一跳）。
		for i := len(parts) - 1; i >= 0; i-- {
			if s := strings.TrimSpace(parts[i]); s != "" {
				return s
			}
		}
	}
	if v := strings.TrimSpace(xRealIP); v != "" {
		return v
	}
	return Host(remoteAddr)
}

// FromRequest 是 From 的便捷版本。
func FromRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	return From(r.Header.Get("X-Forwarded-For"), r.Header.Get("X-Real-Ip"), r.RemoteAddr)
}

// Host 从 "1.2.3.4:5678" / "[::1]:80" 里取出主机部分。
// 拆不出来（没有端口、是别的形状）就原样返回。
func Host(remoteAddr string) string {
	remoteAddr = strings.TrimSpace(remoteAddr)
	if remoteAddr == "" {
		return ""
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

// IsPublic 判断一个地址字符串是不是公网地址。
// 解析不出来（空串、域名、乱码）一律返回 false。
func IsPublic(s string) bool {
	ip := net.ParseIP(strings.TrimSpace(s))
	if ip == nil {
		return false
	}
	return IsPublicIP(ip)
}

// IsPublicIP 判断一个地址是不是「公网可路由」的地址（IPv4 和 IPv6 都管）。
//
// 除了内网、回环、链路本地、组播这些标准判定，还要挡掉几段**看起来是公网、
// 其实不可能是真访客**的地址：运营商级 NAT（100.64/10）、文档示例段
// （192.0.2/24、198.51.100/24、203.0.113/24）、基准测试段（198.18/15）。
// 这些东西留在链里只会让限流桶错乱（比如所有内网用户共用一个桶）。
func IsPublicIP(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	if ip4 := ip.To4(); ip4 != nil {
		switch {
		case ip4[0] == 0, ip4[0] >= 224: // 0.0.0.0/8、组播与保留
			return false
		case ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127: // 100.64/10 CGNAT
			return false
		case ip4[0] == 192 && ip4[1] == 0 && (ip4[2] == 0 || ip4[2] == 2): // 192.0.0/24、192.0.2/24
			return false
		case ip4[0] == 198 && ip4[1] == 51 && ip4[2] == 100: // 198.51.100/24
			return false
		case ip4[0] == 203 && ip4[1] == 0 && ip4[2] == 113: // 203.0.113/24
			return false
		case ip4[0] == 198 && (ip4[1] == 18 || ip4[1] == 19): // 198.18/15
			return false
		}
	}
	return true
}
