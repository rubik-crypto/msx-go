// Package mask 把邮箱、地址这类敏感串打码后再展示或记日志。
package mask

import "strings"

// Email 保留邮箱首尾各一小段，中间用 ** 代替；不是邮箱（没有 @）就原样返回。
func Email(email string) string {
	if email == "" {
		return ""
	}
	at := strings.Index(email, "@")
	if at <= 0 {
		return email
	}
	local, domain := email[:at], email[at:]
	n := len(local)
	keep := n / 3
	if keep == 0 {
		keep = 1
	}
	if 2*keep >= n {
		return local[:1] + "**" + local[n-1:] + domain
	}
	return local[:keep] + "**" + local[n-keep:] + domain
}

// Address 保留地址首尾各 4 个字符（0x 开头的保留前 6 个），中间用 … 代替。
func Address(addr string) string {
	if addr == "" {
		return ""
	}
	head, tail := 4, 4
	if strings.HasPrefix(addr, "0x") {
		head = 6
	}
	if len(addr) <= head+tail {
		return addr
	}
	return addr[:head] + "…" + addr[len(addr)-tail:]
}
