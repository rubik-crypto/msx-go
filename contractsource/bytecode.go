package contractsource

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/rubik-crypto/msx-go/evmabi"
)

// 合约引用了库（library）时，编译出来的创建代码里会留下占位符（形如 __$1a2b…$__），
// 部署前必须替换成库的真实地址，否则链上会直接失败。
var libraryPlaceholderRe = regexp.MustCompile(`__[0-9A-Za-z$]{1,40}__`)

// NormalizeHex 把十六进制串收拾干净：去掉空白与 0x 前缀、校验内容、转成小写。
// 库地址占位符不是十六进制，但必须原样留着等填地址，所以校验时按等长的 0 看待。
func NormalizeHex(s string) (string, error) {
	s = strings.TrimSpace(s)
	s = strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r':
			return -1
		}
		return r
	}, s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if s == "" {
		return "", errors.New("内容是空的")
	}
	if _, err := DecodeHex(s); err != nil {
		return "", err
	}
	return strings.ToLower(s), nil
}

// DecodeHex 把十六进制串解成字节。库地址占位符按「等长的 0」处理，
// 解出来的字节只用来算长度、判断该从哪里切开，不要拿去广播。
func DecodeHex(s string) ([]byte, error) {
	s = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(s), "0x"), "0X")
	probe := libraryPlaceholderRe.ReplaceAllStringFunc(s, func(m string) string {
		return strings.Repeat("0", len(m))
	})
	if len(probe)%2 != 0 {
		return nil, errors.New("十六进制位数是单数，看不完整")
	}
	b, err := hex.DecodeString(probe)
	if err != nil {
		return nil, errors.New("这里面有不是十六进制的字符")
	}
	return b, nil
}

// LibraryPlaceholders 列出创建代码里还没填地址的库占位符，按出现顺序去重。
func LibraryPlaceholders(code string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range libraryPlaceholderRe.FindAllString(code, -1) {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

// FillLibraries 把库占位符替换成对应地址（地址可以带 0x 前缀）。
func FillLibraries(code string, addresses map[string]string) (string, error) {
	for placeholder, addr := range addresses {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}
		if !IsAddress(addr) {
			return "", fmt.Errorf("库地址 %q 不合法，应该是 0x 加 40 位十六进制", addr)
		}
		code = strings.ReplaceAll(code, placeholder, strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(addr, "0x"), "0X")))
	}
	return code, nil
}

// SplitConstructorArgs 把「创建代码 + 末尾粘着的构造参数」分成两段。
//
// 区块浏览器给的创建代码大多是链上那笔交易的原样数据，末尾粘着部署时填的参数；
// 想换成自己的参数，就必须先把这一段切下来。做法：参数编码的长度一定是 32 的倍数，
// 从最短的可能长度开始试——谁能被完整解出来、而且重新编码后和原样一模一样，就认谁。
// 切不出来时 ok=false，raw 原样返回，调用方不要自作主张去改。
func SplitConstructorArgs(raw []byte, types []evmabi.Type) (code, args []byte, ok bool) {
	if len(types) == 0 || len(raw) == 0 {
		return raw, nil, false
	}
	limit := 8192
	if limit > len(raw) {
		limit = len(raw)
	}
	for tail := 32; tail <= limit; tail += 32 {
		start := len(raw) - tail
		if start < 0 {
			break
		}
		vals, err := evmabi.Decode(types, raw[start:])
		if err != nil {
			continue
		}
		back, err := evmabi.Encode(types, vals)
		if err != nil || len(back) != tail || !bytes.Equal(back, raw[start:]) {
			continue
		}
		return raw[:start], raw[start:], true
	}
	return raw, nil, false
}

// CleanCreationBytecode 把创建代码末尾粘着的构造参数切下来。
// 参数类型取自合约 ABI；读不到 ABI 或切不出来时 ok=false。
//
// 返回的 code 与 args 都是不带 0x 前缀的十六进制串（里面可能还有库占位符），
// 直接 code + args 就是一笔部署交易的 data。
func (i *Info) CleanCreationBytecode() (code, args string, ok bool) {
	if !i.HasCreationBytecode() {
		return "", "", false
	}
	full := strings.TrimPrefix(i.CreationBytecode, "0x")
	// 数据源已经把参数单独给了、而且就贴在末尾：按长度切，确定无疑。
	if i.ConstructorArgs != "" {
		trimmed := strings.TrimPrefix(i.ConstructorArgs, "0x")
		if strings.HasSuffix(full, trimmed) {
			return full[:len(full)-len(trimmed)], trimmed, true
		}
	}
	types, err := i.ConstructorArgTypes()
	if err != nil || len(types) == 0 {
		return "", "", false
	}
	raw, err := DecodeHex(full)
	if err != nil {
		return "", "", false
	}
	_, _, tailLen, ok := splitLen(raw, types)
	if !ok {
		return "", "", false
	}
	// 按原字符串切，别把代码里的库占位符弄丢
	cut := len(full) - tailLen*2
	return full[:cut], full[cut:], true
}

// splitLen 只关心切点的字节长度，切下来的内容由调用方按长度从原字符串里取。
func splitLen(raw []byte, types []evmabi.Type) (code, args []byte, tailLen int, ok bool) {
	code, args, ok = SplitConstructorArgs(raw, types)
	if !ok {
		return raw, nil, 0, false
	}
	return code, args, len(args), true
}

// BuildDeployData 把创建代码和构造参数拼成可以直接发出去部署的 data（0x 开头）。
// values 与 types 一一对应，用 evmabi 的值构造器准备（evmabi.Uint、evmabi.StringVal 等）。
func BuildDeployData(creationBytecode string, types []evmabi.Type, values []evmabi.Value, libraries map[string]string) (string, error) {
	code, err := NormalizeHex(creationBytecode)
	if err != nil {
		return "", fmt.Errorf("创建代码不合法：%w", err)
	}
	if len(libraries) > 0 {
		if code, err = FillLibraries(code, libraries); err != nil {
			return "", err
		}
	}
	if holes := LibraryPlaceholders(code); len(holes) > 0 {
		return "", fmt.Errorf("创建代码里还有 %d 处库地址没有填", len(holes))
	}
	enc, err := evmabi.Encode(types, values)
	if err != nil {
		return "", err
	}
	return "0x" + code + hex.EncodeToString(enc), nil
}
