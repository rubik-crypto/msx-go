// Package eip712 实现 EIP-712 结构化数据的哈希与签名。
//
// 解决的问题：在 EVM 生态里给「有结构的消息」签名——限价单、授权凭证、白名单、
// 元交易——钱包和合约都按 EIP-712 校验。可 Go 这边要自己拼类型声明、算三层哈希
// （类型哈希 → 结构体哈希 → 最终摘要），少一个空格、字段顺序错一位，签出来的
// 东西就没有任何一方会认，而且报错信息只会含糊地说「签名无效」。
// 本包把这套编码规则写全，并用公开测试向量和 go-ethereum 的实现交叉验证过。
//
//	td := eip712.TypedData{
//	    Domain: eip712.Domain{
//	        Name: "Example Exchange", Version: "1", ChainID: 137,
//	        VerifyingContract: "0x0000000000000000000000000000000000000001",
//	    },
//	    Types: map[string][]eip712.Field{
//	        "Order": {
//	            {Name: "maker", Type: "address"},
//	            {Name: "amount", Type: "uint256"},
//	            {Name: "note", Type: "string"},
//	        },
//	    },
//	    PrimaryType: "Order",
//	    Message: map[string]any{
//	        "maker": "0xCD2a3d9F938E13CD947Ec05AbC7FE734Df8DD826",
//	        "amount": big.NewInt(1000),
//	        "note": "hello",
//	    },
//	}
//	digest, err := td.Hash()          // 要签的 32 字节摘要
//	sig, err := td.Sign(privateKey)   // 0x 开头的 65 字节签名
//	addr, err := eip712.Recover(td, sig)
//
// 支持的字段类型：uintN / intN / address / bool / bytes / bytesN / string、
// 上面这些组成的定长或动态数组、以及嵌套的结构体（结构体要写进 Types）。
// 赋值时可以用 *big.Int、big.Int、各种整数、bool、string、[]byte、common.Address、
// 十六进制地址字符串；数组用 []any，嵌套结构体用 map[string]any。
package eip712

import (
	"crypto/ecdsa"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/rubik-crypto/msx-go/evmabi"
)

// Field 是结构体里的一个字段。
type Field struct {
	Name string
	Type string
}

// Domain 是 EIP-712 里的「域」，用来把不同应用、不同链、不同合约的签名隔开。
// 除了 Name，其余字段都是可选的：只写进去的字段才会参与类型声明和哈希，
// 所以域里少一项和多一项签出来的结果完全不同。
type Domain struct {
	Name              string
	Version           string
	ChainID           uint64
	VerifyingContract string // 十六进制地址；留空表示不带这一项
	Salt              []byte // 32 字节；留空表示不带这一项
}

// TypedData 是一份完整的 EIP-712 待签数据。
type TypedData struct {
	// Types 是全部结构体的字段声明，键是结构体名。主类型也必须写在这里。
	Types map[string][]Field
	// PrimaryType 是要签的那个结构体名。
	PrimaryType string
	// Domain 是签名所属的域。
	Domain Domain
	// Message 是按字段名给出的取值。
	Message map[string]any
}

// Has 返回该类型是否在 Types 里声明过。
func (t TypedData) Has(name string) bool {
	_, ok := t.Types[name]
	return ok
}

// DomainSeparator 计算域的哈希。
//
// 域本身也是一个 EIP-712 结构体，叫 EIP712Domain，字段按 EIP-712 规定的顺序排列，
// 而且只列真正给了值的那些。
func (d Domain) Separator() ([32]byte, error) {
	var zero [32]byte
	var fields []Field
	msg := map[string]any{}

	if d.Name != "" {
		fields = append(fields, Field{"name", "string"})
		msg["name"] = d.Name
	}
	if d.Version != "" {
		fields = append(fields, Field{"version", "string"})
		msg["version"] = d.Version
	}
	if d.ChainID != 0 {
		fields = append(fields, Field{"chainId", "uint256"})
		msg["chainId"] = new(big.Int).SetUint64(d.ChainID)
	}
	if d.VerifyingContract != "" {
		if _, err := asAddress(d.VerifyingContract); err != nil {
			return zero, fmt.Errorf("域里的合约地址不对：%w", err)
		}
		fields = append(fields, Field{"verifyingContract", "address"})
		msg["verifyingContract"] = d.VerifyingContract
	}
	if len(d.Salt) > 0 {
		if len(d.Salt) != 32 {
			return zero, fmt.Errorf("域里的 salt 必须是 32 字节，实际 %d 字节", len(d.Salt))
		}
		fields = append(fields, Field{"salt", "bytes32"})
		msg["salt"] = d.Salt
	}

	h, err := hashStruct(map[string][]Field{"EIP712Domain": fields}, "EIP712Domain", msg)
	if err != nil {
		return zero, err
	}
	return h, nil
}

// Hash 返回最终要签的那 32 字节摘要。
//
// 算法是 keccak256(0x19 ‖ 0x01 ‖ 域哈希 ‖ 主结构体哈希)。
// 开头那两个字节是 EIP-191 定的前缀，作用是把 EIP-712 的签名和普通消息签名区分开。
func (t TypedData) Hash() ([32]byte, error) {
	var zero [32]byte
	if t.PrimaryType == "" {
		return zero, errors.New("没有指定主类型")
	}
	if !t.Has(t.PrimaryType) {
		return zero, fmt.Errorf("主类型 %q 没有在 Types 里声明", t.PrimaryType)
	}
	if err := t.checkAllTypes(); err != nil {
		return zero, err
	}

	domain, err := t.Domain.Separator()
	if err != nil {
		return zero, err
	}
	body, err := hashStruct(t.Types, t.PrimaryType, t.Message)
	if err != nil {
		return zero, err
	}

	buf := make([]byte, 0, 66)
	buf = append(buf, 0x19, 0x01)
	buf = append(buf, domain[:]...)
	buf = append(buf, body[:]...)
	sum := evmabi.Keccak256(buf)
	copy(zero[:], sum)
	return zero, nil
}

// Sign 用私钥签这份数据，返回 0x 开头的 65 字节十六进制签名。
//
// 末尾那个字节按钱包惯例补成 27/28——服务端和合约都是按这个格式验的。
func (t TypedData) Sign(key *ecdsa.PrivateKey) (string, error) {
	if key == nil {
		return "", errors.New("私钥为空")
	}
	digest, err := t.Hash()
	if err != nil {
		return "", err
	}
	return SignHash(digest, key)
}

// SignHash 对已经算好的摘要做 ECDSA 签名，返回值同样是 0x 开头补过 27/28 的签名。
func SignHash(digest [32]byte, key *ecdsa.PrivateKey) (string, error) {
	if key == nil {
		return "", errors.New("私钥为空")
	}
	sig, err := crypto.Sign(digest[:], key)
	if err != nil {
		return "", err
	}
	sig[64] += 27
	return "0x" + hex.EncodeToString(sig), nil
}

// Recover 从签名还原出签名者地址。
//
// 这个函数在服务端验签时很有用：把客户端传来的签名还原成地址，
// 跟它声称的身份比一比，就能确认这份数据确实是它签的。
func Recover(t TypedData, signature string) (common.Address, error) {
	digest, err := t.Hash()
	if err != nil {
		return common.Address{}, err
	}
	return RecoverHash(digest, signature)
}

// RecoverHash 从已经算好的摘要和签名还原地址。
func RecoverHash(digest [32]byte, signature string) (common.Address, error) {
	raw, err := decodeSignature(signature)
	if err != nil {
		return common.Address{}, err
	}
	pub, err := crypto.SigToPub(digest[:], raw)
	if err != nil {
		return common.Address{}, fmt.Errorf("从签名还原公钥失败：%w", err)
	}
	return crypto.PubkeyToAddress(*pub), nil
}

func decodeSignature(signature string) ([]byte, error) {
	s := strings.TrimSpace(signature)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	raw, err := hex.DecodeString(s)
	if err != nil {
		return nil, errors.New("签名不是合法的十六进制串")
	}
	if len(raw) != 65 {
		return nil, fmt.Errorf("签名应该是 65 字节，实际 %d 字节", len(raw))
	}
	// 钱包给的是 27/28，本地还原函数要的是 0/1。
	if raw[64] >= 27 {
		raw[64] -= 27
	}
	if raw[64] > 1 {
		return nil, errors.New("签名的最后一个字节不是合法的恢复标识")
	}
	return raw, nil
}

// ———————————————————— 编码 ————————————————————

// checkAllTypes 提前把所有类型声明扫一遍，让写法错误在算哈希之前就暴露出来，
// 而不是等到某个分支没走到才发现。
func (t TypedData) checkAllTypes() error {
	names := make([]string, 0, len(t.Types))
	for name := range t.Types {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, f := range t.Types[name] {
			if strings.TrimSpace(f.Name) == "" {
				return fmt.Errorf("类型 %s 里有字段没写名字", name)
			}
			base, _, err := parseRef(f.Type)
			if err != nil {
				return fmt.Errorf("类型 %s 的字段 %s：%w", name, f.Name, err)
			}
			if _, isBuiltin := builtinSize(base); isBuiltin {
				continue
			}
			if !t.Has(base) {
				return fmt.Errorf("类型 %s 的字段 %s 用了 %q，但它没有在 Types 里声明", name, f.Name, base)
			}
		}
	}
	return nil
}

// hashStruct 按 EIP-712 算一个结构体的哈希：keccak256(类型哈希 ‖ 各字段的编码)。
func hashStruct(types map[string][]Field, name string, message map[string]any) ([32]byte, error) {
	var zero [32]byte
	typeStr, err := encodeType(types, name)
	if err != nil {
		return zero, err
	}
	buf := evmabi.Keccak256([]byte(typeStr))

	for _, f := range types[name] {
		value, ok := lookupField(message, f.Name)
		if !ok {
			return zero, fmt.Errorf("结构体 %s 缺少字段 %q 的值", name, f.Name)
		}
		word, err := encodeValue(types, f.Type, value)
		if err != nil {
			return zero, fmt.Errorf("结构体 %s 的字段 %s：%w", name, f.Name, err)
		}
		buf = append(buf, word...)
	}
	sum := evmabi.Keccak256(buf)
	copy(zero[:], sum)
	return zero, nil
}

// lookupField 取值时对大小写宽容一点：前端传 JSON 时字段名大小写经常对不上。
func lookupField(message map[string]any, name string) (any, bool) {
	if v, ok := message[name]; ok {
		return v, true
	}
	for k, v := range message {
		if strings.EqualFold(k, name) {
			return v, true
		}
	}
	return nil, false
}

// encodeType 拼出这个结构体的类型声明，再把它引用到的结构体按字母序追加在后面。
//
// 例：主类型是 Mail、里面用到 Person，拼出来就是
// "Mail(Person from,Person to,string contents)Person(string name,address wallet)"。
// 被引用的类型必须带上、且必须按字母序，少一个或者顺序不对，typeHash 就不是对方算的那个。
func encodeType(types map[string][]Field, primary string) (string, error) {
	fields, ok := types[primary]
	if !ok {
		return "", fmt.Errorf("类型 %q 没有声明", primary)
	}

	deps := map[string]bool{}
	for _, f := range fields {
		if err := collectDeps(types, f.Type, deps); err != nil {
			return "", err
		}
	}
	delete(deps, primary)

	names := make([]string, 0, len(deps))
	for n := range deps {
		names = append(names, n)
	}
	sort.Strings(names)

	var sb strings.Builder
	writeTypeDecl(&sb, primary, types[primary])
	for _, n := range names {
		writeTypeDecl(&sb, n, types[n])
	}
	return sb.String(), nil
}

func writeTypeDecl(sb *strings.Builder, name string, fields []Field) {
	sb.WriteString(name)
	sb.WriteByte('(')
	for i, f := range fields {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(f.Type)
		sb.WriteByte(' ')
		sb.WriteString(f.Name)
	}
	sb.WriteByte(')')
}

// collectDeps 递归找出某个字段类型引用到的全部结构体。
// 类型名可以是 "Person"，也可以是 "Person[]"、"Person[3][2]" 这种。
func collectDeps(types map[string][]Field, typeName string, out map[string]bool) error {
	base, _, err := parseRef(typeName)
	if err != nil {
		return err
	}
	if _, isBuiltin := builtinSize(base); isBuiltin {
		return nil
	}
	if out[base] {
		return nil
	}
	fields, ok := types[base]
	if !ok {
		return fmt.Errorf("类型 %q 用了 %q，但它没有在 Types 里声明", typeName, base)
	}
	out[base] = true
	for _, f := range fields {
		if err := collectDeps(types, f.Type, out); err != nil {
			return err
		}
	}
	return nil
}

// encodeValue 把一个字段值编码成 32 字节（或数组时是若干个 32 字节拼起来的哈希输入）。
func encodeValue(types map[string][]Field, typeName string, value any) ([]byte, error) {
	base, dims, err := parseRef(typeName)
	if err != nil {
		return nil, err
	}

	// 数组：逐项编码后拼起来，再整体取一次哈希。
	if len(dims) > 0 {
		items, ok := asSlice(value)
		if !ok {
			return nil, fmt.Errorf("需要数组，实际拿到 %T", value)
		}
		if dims[0] > 0 && len(items) != dims[0] {
			return nil, fmt.Errorf("定长数组要 %d 个元素，实际 %d 个", dims[0], len(items))
		}
		elemType := rebuildRef(base, dims[1:])
		var buf []byte
		for i, item := range items {
			word, err := encodeValue(types, elemType, item)
			if err != nil {
				return nil, fmt.Errorf("数组第 %d 项：%w", i, err)
			}
			buf = append(buf, word...)
		}
		return evmabi.Keccak256(buf), nil
	}

	// 嵌套结构体：递归算它的结构体哈希。
	if _, isBuiltin := builtinSize(base); !isBuiltin {
		nested, ok := asMap(value)
		if !ok {
			return nil, fmt.Errorf("结构体 %s 需要一个按字段名给值的对象，实际拿到 %T", base, value)
		}
		h, err := hashStruct(types, base, nested)
		if err != nil {
			return nil, err
		}
		return h[:], nil
	}

	return encodeAtomic(base, value)
}

// encodeAtomic 处理最底层的那些类型。
func encodeAtomic(base string, value any) ([]byte, error) {
	switch base {
	case "string":
		s, err := asString(value)
		if err != nil {
			return nil, err
		}
		return evmabi.Keccak256([]byte(s)), nil

	case "bytes":
		b, err := asBytes(value)
		if err != nil {
			return nil, err
		}
		return evmabi.Keccak256(b), nil

	case "bool":
		b, ok := value.(bool)
		if !ok {
			return nil, fmt.Errorf("需要布尔值，实际拿到 %T", value)
		}
		word := make([]byte, 32)
		if b {
			word[31] = 1
		}
		return word, nil

	case "address":
		bytesAddr, err := asAddress(value)
		if err != nil {
			return nil, err
		}
		word := make([]byte, 32)
		copy(word[12:], bytesAddr)
		return word, nil

	default:
		if n, ok := bytesN(base); ok {
			b, err := asBytes(value)
			if err != nil {
				return nil, err
			}
			if len(b) != n {
				return nil, fmt.Errorf("%s 需要 %d 字节，实际 %d 字节", base, n, len(b))
			}
			word := make([]byte, 32)
			copy(word, b)
			return word, nil
		}
		if isUintType(base) || isIntType(base) {
			n, err := asBigInt(value)
			if err != nil {
				return nil, err
			}
			if isUintType(base) && n.Sign() < 0 {
				return nil, fmt.Errorf("%s 不能是负数", base)
			}
			word := make([]byte, 32)
			// FillBytes 对负数会 panic，所以负数走两补码那条路。
			if n.Sign() < 0 {
				two := new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), 256), n)
				two.FillBytes(word)
			} else {
				if n.BitLen() > 256 {
					return nil, errors.New("数值超出 256 位")
				}
				n.FillBytes(word)
			}
			return word, nil
		}
	}
	return nil, fmt.Errorf("不认识的类型 %q", base)
}

// ———————————————————— 类型名解析 ————————————————————

// parseBase 只检查一个类型名本身是否合法，不看它有没有在 Types 里声明。
func parseBase(s string) (string, error) {
	base, _, err := parseRef(s)
	return base, err
}

// parseRef 把 "uint256"、"Person[]"、"uint8[3][2]" 这样的类型名拆成基础类型和维度。
// dims[0] 是最外层的维度，0 表示动态数组。
func parseRef(s string) (string, []int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil, errors.New("类型名为空")
	}
	var dims []int
	for strings.HasSuffix(s, "]") {
		open := strings.LastIndexByte(s, '[')
		if open < 0 {
			return "", nil, fmt.Errorf("类型名 %q 的方括号不配对", s)
		}
		inner := s[open+1 : len(s)-1]
		if inner == "" {
			dims = append(dims, 0) // 动态数组
		} else {
			n, err := strconv.Atoi(inner)
			if err != nil || n <= 0 {
				return "", nil, fmt.Errorf("类型名 %q 里的数组长度不合法", s)
			}
			dims = append(dims, n)
		}
		s = s[:open]
	}
	// 上面是从右往左剥的，反转一下让 dims[0] 对应最外层。
	for i, j := 0, len(dims)-1; i < j; i, j = i+1, j-1 {
		dims[i], dims[j] = dims[j], dims[i]
	}

	if _, ok := builtinSize(s); !ok {
		if !isIdentifier(s) {
			return "", nil, fmt.Errorf("类型名 %q 不合法", s)
		}
	}
	return s, dims, nil
}

func rebuildRef(base string, dims []int) string {
	var sb strings.Builder
	sb.WriteString(base)
	for _, d := range dims {
		sb.WriteByte('[')
		if d > 0 {
			sb.WriteString(strconv.Itoa(d))
		}
		sb.WriteByte(']')
	}
	return sb.String()
}

func isIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// builtinSize 判断是不是内置类型。返回值表示定长字节类型的字节数，
// 第二个返回值表示「这是不是内置类型」。
func builtinSize(t string) (int, bool) {
	switch t {
	case "string", "bytes", "bool", "address":
		return 0, true
	}
	if isUintType(t) || isIntType(t) {
		return 0, true
	}
	if n, ok := bytesN(t); ok {
		return n, true
	}
	return 0, false
}

func isUintType(t string) bool {
	if !strings.HasPrefix(t, "uint") {
		return false
	}
	rest := t[4:]
	if rest == "" {
		return true // "uint" 是 uint256 的别名
	}
	n, err := strconv.Atoi(rest)
	return err == nil && n > 0 && n <= 256 && n%8 == 0
}

func isIntType(t string) bool {
	if !strings.HasPrefix(t, "int") {
		return false
	}
	rest := t[3:]
	if rest == "" {
		return true
	}
	n, err := strconv.Atoi(rest)
	return err == nil && n > 0 && n <= 256 && n%8 == 0
}

func bytesN(t string) (int, bool) {
	if !strings.HasPrefix(t, "bytes") {
		return 0, false
	}
	n, err := strconv.Atoi(t[5:])
	if err != nil || n <= 0 || n > 32 {
		return 0, false
	}
	return n, true
}

// ———————————————————— 取值转换 ————————————————————

func asSlice(v any) ([]any, bool) {
	switch x := v.(type) {
	case []any:
		return x, true
	case nil:
		return nil, false
	default:
		return nil, false
	}
}

func asMap(v any) (map[string]any, bool) {
	switch x := v.(type) {
	case map[string]any:
		return x, true
	case nil:
		return nil, false
	default:
		return nil, false
	}
}

func asString(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case []byte:
		return string(x), nil
	case fmt.Stringer:
		return x.String(), nil
	default:
		return "", fmt.Errorf("需要字符串，实际拿到 %T", v)
	}
}

func asBytes(v any) ([]byte, error) {
	switch x := v.(type) {
	case []byte:
		return x, nil
	case [32]byte:
		return x[:], nil
	case string:
		s := strings.TrimSpace(x)
		if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
			b, err := hex.DecodeString(s[2:])
			if err != nil {
				return nil, errors.New("看起来是十六进制串，但解不开")
			}
			return b, nil
		}
		return []byte(x), nil
	default:
		return nil, fmt.Errorf("需要字节串，实际拿到 %T", v)
	}
}

func asAddress(v any) ([]byte, error) {
	s, ok := v.(string)
	if !ok {
		if a, isAddr := v.(common.Address); isAddr {
			return a.Bytes(), nil
		}
		return nil, fmt.Errorf("需要地址，实际拿到 %T", v)
	}
	s = strings.TrimSpace(s)
	if !common.IsHexAddress(s) {
		return nil, fmt.Errorf("%q 不是合法的以太坊地址", s)
	}
	return common.HexToAddress(s).Bytes(), nil
}

func asBigInt(v any) (*big.Int, error) {
	switch x := v.(type) {
	case *big.Int:
		if x == nil {
			return nil, errors.New("数值是空的")
		}
		return x, nil
	case big.Int:
		return &x, nil
	case int:
		return big.NewInt(int64(x)), nil
	case int8:
		return big.NewInt(int64(x)), nil
	case int16:
		return big.NewInt(int64(x)), nil
	case int32:
		return big.NewInt(int64(x)), nil
	case int64:
		return big.NewInt(x), nil
	case uint:
		return new(big.Int).SetUint64(uint64(x)), nil
	case uint8:
		return new(big.Int).SetUint64(uint64(x)), nil
	case uint16:
		return new(big.Int).SetUint64(uint64(x)), nil
	case uint32:
		return new(big.Int).SetUint64(uint64(x)), nil
	case uint64:
		return new(big.Int).SetUint64(x), nil
	case string:
		s := strings.TrimSpace(x)
		n, ok := new(big.Int).SetString(s, 10)
		if !ok {
			return nil, fmt.Errorf("%q 不是十进制整数", s)
		}
		return n, nil
	default:
		return nil, fmt.Errorf("需要整数，实际拿到 %T", v)
	}
}
