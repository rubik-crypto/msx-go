// Package slug 把任意字符串收敛成适合放进 URL、文件名、分支名的短标识。
package slug

import "strings"

// Make 转小写，保留字母数字和 - _，其余换成 -，压掉连续分隔符并去掉首尾。
func Make(s string) string {
	var b strings.Builder
	lastSep := true // 开头不允许分隔符
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastSep = false
		case r == '-' || r == '_':
			b.WriteRune(r)
			lastSep = true
		default:
			if !lastSep {
				b.WriteByte('-')
				lastSep = true
			}
		}
	}
	return strings.Trim(b.String(), "-_")
}
