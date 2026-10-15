// Package evmabi 实现以太坊智能合约的 ABI 编解码与函数选择器计算。
//
// 解决的问题：想在 Go 里跟 EVM 合约说话（拼 calldata、解返回值），常见做法是拖进
// go-ethereum 这一整套依赖。可真正用到的只是「类型 → 32 字节字」这一层机械逻辑。
// 本包用标准库把它写全：函数选择器由签名现场算出来（不手抄易错的选择器常量），
// 参数按 ABI 规范编码成 calldata，返回值按同一套规则解回来。
//
//	v := evmabi.Call("transfer(address,uint256)",
//	    evmabi.Address("0x…"), evmabi.Uint(1000))
//	data, err := v.Data()          // 0xa9059cbb + 参数
//	out, err := v.DecodeReturns(res) // 解返回值
//
// 支持的类型：uintN / intN / address / bool / bytesN / bytes / string /
// 定长与动态数组 / 元组（含元组里套动态类型、数组装动态类型）。
package evmabi

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// kind 是 ABI 类型的种类。
type kind int

const (
	kindUint kind = iota
	kindInt
	kindAddress
	kindBool
	kindBytes     // 动态字节串
	kindFixedByte // bytes1..bytes32
	kindString
	kindArray // 数组，len < 0 表示动态数组
	kindTuple
)

// Type 是一个解析好的 ABI 类型。
type Type struct {
	kind  kind
	size  int    // uintN/intN 的位数、bytesN 的字节数
	elem  *Type  // 数组元素类型
	alen  int    // 数组长度，-1 表示动态
	field []Type // 元组字段
}

// ParseType 解析一个 ABI 类型字符串，例如 "uint256"、"address[]"、
// "(address,address,uint256)"。
func ParseType(s string) (Type, error) {
	p := &typeParser{src: s}
	t, err := p.parse()
	if err != nil {
		return Type{}, err
	}
	if p.pos != len(p.src) {
		return Type{}, fmt.Errorf("类型 %q 解析不完，剩余 %q", s, p.src[p.pos:])
	}
	return t, nil
}

// MustParseType 和 ParseType 一样，但类型字符串写错时直接 panic。
// 适合类型是代码里的常量字面量、写错就属于编程错误的场合。
func MustParseType(s string) Type {
	t, err := ParseType(s)
	if err != nil {
		panic(err)
	}
	return t
}

type typeParser struct {
	src string
	pos int
}

func (p *typeParser) parse() (Type, error) {
	if p.pos >= len(p.src) {
		return Type{}, errors.New("类型字符串提前结束")
	}
	var base Type
	switch {
	case p.src[p.pos] == '(':
		p.pos++
		base = Type{kind: kindTuple}
		if p.pos < len(p.src) && p.src[p.pos] == ')' {
			p.pos++
		} else {
			for {
				f, err := p.parse()
				if err != nil {
					return Type{}, err
				}
				base.field = append(base.field, f)
				if p.pos < len(p.src) && p.src[p.pos] == ',' {
					p.pos++
					continue
				}
				if p.pos < len(p.src) && p.src[p.pos] == ')' {
					p.pos++
					break
				}
				return Type{}, fmt.Errorf("元组里第 %d 个字段后面既不是逗号也不是右括号", len(base.field))
			}
		}
	case strings.HasPrefix(p.src[p.pos:], "uint"):
		p.pos += 4
		n, err := p.readBits(256)
		if err != nil {
			return Type{}, err
		}
		if n%8 != 0 || n < 8 || n > 256 {
			return Type{}, fmt.Errorf("uint%d 不是合法的位宽", n)
		}
		base = Type{kind: kindUint, size: n}
	case strings.HasPrefix(p.src[p.pos:], "int"):
		p.pos += 3
		n, err := p.readBits(256)
		if err != nil {
			return Type{}, err
		}
		if n%8 != 0 || n < 8 || n > 256 {
			return Type{}, fmt.Errorf("int%d 不是合法的位宽", n)
		}
		base = Type{kind: kindInt, size: n}
	case strings.HasPrefix(p.src[p.pos:], "address"):
		p.pos += 7
		base = Type{kind: kindAddress}
	case strings.HasPrefix(p.src[p.pos:], "bool"):
		p.pos += 4
		base = Type{kind: kindBool}
	case strings.HasPrefix(p.src[p.pos:], "string"):
		p.pos += 6
		base = Type{kind: kindString}
	case strings.HasPrefix(p.src[p.pos:], "bytes"):
		p.pos += 5
		n := p.readInt()
		if n == 0 {
			base = Type{kind: kindBytes}
		} else if n >= 1 && n <= 32 {
			base = Type{kind: kindFixedByte, size: n}
		} else {
			return Type{}, fmt.Errorf("bytes%d 不是合法的长度", n)
		}
	default:
		return Type{}, fmt.Errorf("看不懂的类型 %q", p.src[p.pos:])
	}

	// 数组后缀，支持多维
	for p.pos < len(p.src) && p.src[p.pos] == '[' {
		p.pos++
		n := -1
		if p.pos < len(p.src) && p.src[p.pos] != ']' {
			n = p.readInt()
			if n < 0 {
				return Type{}, errors.New("数组长度写错了")
			}
		}
		if p.pos >= len(p.src) || p.src[p.pos] != ']' {
			return Type{}, errors.New("数组缺少右方括号")
		}
		p.pos++
		elem := base
		base = Type{kind: kindArray, elem: &elem, alen: n}
	}
	return base, nil
}

func (p *typeParser) readBits(max int) (int, error) {
	if p.pos >= len(p.src) || p.src[p.pos] < '0' || p.src[p.pos] > '9' {
		// 省略位宽时按 256 算（规范里 uint 等于 uint256）
		return max, nil
	}
	return p.readInt(), nil
}

func (p *typeParser) readInt() int {
	n := 0
	got := false
	for p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '9' {
		n = n*10 + int(p.src[p.pos]-'0')
		if n > 1<<20 {
			n = -1
			break
		}
		p.pos++
		got = true
	}
	if !got {
		return 0
	}
	return n
}

// String 返回类型规范写法（和 Parse 的输入同形）。
func (t Type) String() string {
	var base string
	switch t.kind {
	case kindUint:
		base = fmt.Sprintf("uint%d", t.size)
	case kindInt:
		base = fmt.Sprintf("int%d", t.size)
	case kindAddress:
		base = "address"
	case kindBool:
		base = "bool"
	case kindBytes:
		base = "bytes"
	case kindFixedByte:
		base = fmt.Sprintf("bytes%d", t.size)
	case kindString:
		base = "string"
	case kindArray:
		if t.alen < 0 {
			return t.elem.String() + "[]"
		}
		return fmt.Sprintf("%s[%d]", t.elem.String(), t.alen)
	case kindTuple:
		parts := make([]string, len(t.field))
		for i, f := range t.field {
			parts[i] = f.String()
		}
		base = "(" + strings.Join(parts, ",") + ")"
	}
	return base
}

// Dynamic 表示这个类型在 ABI 编码里是不是「长度不定」（决定要不要写成偏移量）。
func (t Type) Dynamic() bool {
	switch t.kind {
	case kindBytes, kindString:
		return true
	case kindArray:
		return t.alen < 0 || t.elem.Dynamic()
	case kindTuple:
		for _, f := range t.field {
			if f.Dynamic() {
				return true
			}
		}
	}
	return false
}

// staticSize 是静态类型占用的字节数。
func (t Type) staticSize() int {
	switch t.kind {
	case kindArray:
		return t.alen * t.elem.staticSize()
	case kindTuple:
		n := 0
		for _, f := range t.field {
			n += f.staticSize()
		}
		return n
	}
	return 32
}

// Value 是一个 ABI 参数值，用哪个字段由对应类型决定。
type Value struct {
	Uint  *big.Int // uintN / intN
	Addr  string   // address
	Bool  bool     // bool
	Bytes []byte   // bytes / bytesN / string
	Tuple []Value  // tuple
	Array []Value  // 数组
}

// Uint 构造一个无符号整数值。
func Uint(x uint64) Value { return Value{Uint: new(big.Int).SetUint64(x)} }

// BigUint 构造一个任意精度整数值。
func BigUint(x *big.Int) Value {
	if x == nil {
		x = new(big.Int)
	}
	return Value{Uint: new(big.Int).Set(x)}
}

// Address 构造一个地址值。地址大小写不敏感，编码前统一转小写。
func Address(s string) Value { return Value{Addr: s} }

// BytesVal 构造一个字节串值（bytes 或 bytesN）。
func BytesVal(b []byte) Value { return Value{Bytes: b} }

// StringVal 构造一个字符串值。
func StringVal(s string) Value { return Value{Bytes: []byte(s)} }

// BoolVal 构造一个布尔值。
func BoolVal(b bool) Value { return Value{Bool: b} }

// TupleVal 构造一个元组值。
func TupleVal(vs ...Value) Value { return Value{Tuple: vs} }

// ArrayVal 构造一个数组值。
func ArrayVal(vs ...Value) Value { return Value{Array: vs} }

// EncodeUint 把一个整数写成 ABI 的 32 字节字（大端、左补零）。
// 手写测试用例、或者要拼一个不带函数选择器的裸参数时很方便。
func EncodeUint(n uint64) []byte {
	return leftPad32(new(big.Int).SetUint64(n).Bytes())
}

// Hex 返回 0x 开头的十六进制字符串。
func Hex(b []byte) string { return "0x" + hex.EncodeToString(b) }

// NormalizeAddress 校验并规范化地址：统一小写、补 0x 前缀。
func NormalizeAddress(s string) (string, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if len(s) != 40 {
		return "", fmt.Errorf("地址 %q 长度不对", s)
	}
	s = strings.ToLower(s)
	if _, err := hex.DecodeString(s); err != nil {
		return "", fmt.Errorf("地址 %q 里有不是十六进制的字符", s)
	}
	return "0x" + s, nil
}

// AddressBytes 把地址转成 20 字节。
func AddressBytes(s string) ([]byte, error) {
	n, err := NormalizeAddress(s)
	if err != nil {
		return nil, err
	}
	return hex.DecodeString(n[2:])
}

// Encode 把一组参数按类型编码成 ABI 数据（不含选择器）。
func Encode(types []Type, values []Value) ([]byte, error) {
	if len(types) != len(values) {
		return nil, fmt.Errorf("类型有 %d 个，值有 %d 个，对不上", len(types), len(values))
	}
	return encodeTuple(types, values)
}

func encodeTuple(types []Type, values []Value) ([]byte, error) {
	headSize := 0
	for _, t := range types {
		if t.Dynamic() {
			headSize += 32
		} else {
			headSize += t.staticSize()
		}
	}
	head := make([]byte, headSize)
	var tail []byte
	pos := 0
	for i, t := range types {
		if t.Dynamic() {
			pos += 32
			continue
		}
		enc, err := encodeStatic(t, values[i])
		if err != nil {
			return nil, fmt.Errorf("第 %d 个参数（%s）：%w", i+1, t, err)
		}
		copy(head[pos:], enc)
		pos += len(enc)
	}
	for i, t := range types {
		if !t.Dynamic() {
			continue
		}
		off := headSize + len(tail)
		putWord(head[i*32:], new(big.Int).SetInt64(int64(off)))
		enc, err := encodeDynamic(t, values[i])
		if err != nil {
			return nil, fmt.Errorf("第 %d 个参数（%s）：%w", i+1, t, err)
		}
		tail = append(tail, enc...)
	}
	return append(head, tail...), nil
}

func encodeStatic(t Type, v Value) ([]byte, error) {
	switch t.kind {
	case kindUint, kindInt:
		if v.Uint == nil {
			return nil, errors.New("缺少整数值")
		}
		n := new(big.Int).Set(v.Uint)
		if n.Sign() < 0 {
			if t.kind != kindInt {
				return nil, fmt.Errorf("无符号类型不能是负数 %s", n)
			}
			n.Add(n, new(big.Int).Lsh(big.NewInt(1), 256)) // 补码
		}
		if n.BitLen() > 256 {
			return nil, fmt.Errorf("数值 %s 超出 256 位", v.Uint)
		}
		return leftPad32(n.Bytes()), nil
	case kindAddress:
		b, err := AddressBytes(v.Addr)
		if err != nil {
			return nil, err
		}
		return leftPad32(b), nil
	case kindBool:
		out := make([]byte, 32)
		if v.Bool {
			out[31] = 1
		}
		return out, nil
	case kindFixedByte:
		if len(v.Bytes) != t.size {
			return nil, fmt.Errorf("bytes%d 需要 %d 字节，给了 %d 字节", t.size, t.size, len(v.Bytes))
		}
		out := make([]byte, 32)
		copy(out, v.Bytes)
		return out, nil
	case kindArray:
		if t.elem.Dynamic() {
			return nil, errors.New("动态元素数组不在这里编码")
		}
		if len(v.Array) != t.alen {
			return nil, fmt.Errorf("定长数组要 %d 个元素，给了 %d 个", t.alen, len(v.Array))
		}
		var out []byte
		for _, e := range v.Array {
			enc, err := encodeStatic(*t.elem, e)
			if err != nil {
				return nil, err
			}
			out = append(out, enc...)
		}
		return out, nil
	case kindTuple:
		if len(v.Tuple) != len(t.field) {
			return nil, fmt.Errorf("元组要 %d 个字段，给了 %d 个", len(t.field), len(v.Tuple))
		}
		var out []byte
		for i, f := range t.field {
			enc, err := encodeStatic(f, v.Tuple[i])
			if err != nil {
				return nil, err
			}
			out = append(out, enc...)
		}
		return out, nil
	}
	return nil, fmt.Errorf("类型 %s 不是静态类型", t)
}

func encodeDynamic(t Type, v Value) ([]byte, error) {
	switch t.kind {
	case kindBytes, kindString:
		out := leftPad32(new(big.Int).SetInt64(int64(len(v.Bytes))).Bytes())
		return append(out, rightPad(v.Bytes)...), nil
	case kindArray:
		n := len(v.Array)
		if t.alen >= 0 && n != t.alen {
			return nil, fmt.Errorf("定长数组要 %d 个元素，给了 %d 个", t.alen, n)
		}
		types := make([]Type, n)
		for i := range types {
			types[i] = *t.elem
		}
		body, err := encodeTuple(types, v.Array)
		if err != nil {
			return nil, err
		}
		return append(leftPad32(new(big.Int).SetInt64(int64(n)).Bytes()), body...), nil
	case kindTuple:
		if len(v.Tuple) != len(t.field) {
			return nil, fmt.Errorf("元组要 %d 个字段，给了 %d 个", len(t.field), len(v.Tuple))
		}
		return encodeTuple(t.field, v.Tuple)
	}
	return nil, fmt.Errorf("类型 %s 不是动态类型", t)
}

func leftPad32(b []byte) []byte {
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

func rightPad(b []byte) []byte {
	if len(b)%32 == 0 && len(b) > 0 {
		out := make([]byte, len(b))
		copy(out, b)
		return out
	}
	out := make([]byte, (len(b)/32+1)*32)
	copy(out, b)
	return out
}

func putWord(dst []byte, n *big.Int) {
	b := n.Bytes()
	copy(dst[32-len(b):], b)
}

// Decode 按类型把 ABI 数据解回一组值。
func Decode(types []Type, data []byte) ([]Value, error) {
	return decodeTuple(types, data)
}

func decodeTuple(types []Type, data []byte) ([]Value, error) {
	vals := make([]Value, len(types))
	pos := 0
	for i, t := range types {
		if t.Dynamic() {
			if pos+32 > len(data) {
				return nil, fmt.Errorf("第 %d 个参数（%s）的偏移量越界", i+1, t)
			}
			off := new(big.Int).SetBytes(data[pos : pos+32])
			pos += 32
			if !off.IsInt64() || off.Int64() < 0 || int(off.Int64()) > len(data) {
				return nil, fmt.Errorf("第 %d 个参数（%s）的偏移量 %s 越界", i+1, t, off)
			}
			v, err := decodeDynamic(t, data[off.Int64():])
			if err != nil {
				return nil, fmt.Errorf("第 %d 个参数（%s）：%w", i+1, t, err)
			}
			vals[i] = v
			continue
		}
		sz := t.staticSize()
		if pos+sz > len(data) {
			return nil, fmt.Errorf("第 %d 个参数（%s）数据不够：需要 %d 字节，只剩 %d 字节", i+1, t, sz, len(data)-pos)
		}
		v, err := decodeStatic(t, data[pos:pos+sz])
		if err != nil {
			return nil, fmt.Errorf("第 %d 个参数（%s）：%w", i+1, t, err)
		}
		vals[i] = v
		pos += sz
	}
	return vals, nil
}

func decodeStatic(t Type, data []byte) (Value, error) {
	switch t.kind {
	case kindUint, kindInt:
		n := new(big.Int).SetBytes(data[:32])
		if t.kind == kindInt && n.Bit(255) == 1 {
			n.Sub(n, new(big.Int).Lsh(big.NewInt(1), 256))
		}
		return Value{Uint: n}, nil
	case kindAddress:
		return Value{Addr: "0x" + hex.EncodeToString(data[12:32])}, nil
	case kindBool:
		return Value{Bool: data[31] != 0}, nil
	case kindFixedByte:
		b := make([]byte, t.size)
		copy(b, data[:t.size])
		return Value{Bytes: b}, nil
	case kindArray:
		vals := make([]Value, 0, t.alen)
		for i := 0; i < t.alen; i++ {
			v, err := decodeStatic(*t.elem, data[i*t.elem.staticSize():])
			if err != nil {
				return Value{}, err
			}
			vals = append(vals, v)
		}
		return Value{Array: vals}, nil
	case kindTuple:
		vals := make([]Value, 0, len(t.field))
		pos := 0
		for _, f := range t.field {
			v, err := decodeStatic(f, data[pos:])
			if err != nil {
				return Value{}, err
			}
			vals = append(vals, v)
			pos += f.staticSize()
		}
		return Value{Tuple: vals}, nil
	}
	return Value{}, fmt.Errorf("类型 %s 不是静态类型", t)
}

func decodeDynamic(t Type, data []byte) (Value, error) {
	if len(data) < 32 {
		return Value{}, errors.New("动态数据不足 32 字节")
	}
	switch t.kind {
	case kindBytes, kindString:
		n := new(big.Int).SetBytes(data[:32])
		if !n.IsInt64() || n.Int64() < 0 || int(n.Int64()) > len(data)-32 {
			return Value{}, errors.New("动态字节串的长度越界")
		}
		b := make([]byte, n.Int64())
		copy(b, data[32:32+n.Int64()])
		return Value{Bytes: b}, nil
	case kindArray:
		n := new(big.Int).SetBytes(data[:32])
		if !n.IsInt64() || n.Int64() < 0 {
			return Value{}, errors.New("数组长度不合法")
		}
		types := make([]Type, n.Int64())
		for i := range types {
			types[i] = *t.elem
		}
		vals, err := decodeTuple(types, data[32:])
		if err != nil {
			return Value{}, err
		}
		return Value{Array: vals}, nil
	case kindTuple:
		vals, err := decodeTuple(t.field, data)
		if err != nil {
			return Value{}, err
		}
		return Value{Tuple: vals}, nil
	}
	return Value{}, fmt.Errorf("类型 %s 不是动态类型", t)
}

// Int 取出整数值（uintN / intN）。
func (v Value) Int() *big.Int {
	if v.Uint == nil {
		return new(big.Int)
	}
	return v.Uint
}

// Text 把值还原成人看的字符串（bytes / string）。
func (v Value) Text() string { return string(v.Bytes) }

// Call 是一个准备好了的函数调用：选择器 + 参数编码。
type Call struct {
	signature string
	types     []Type
	values    []Value
}

// NewCall 按函数签名和参数造一个调用。签名要写规范写法，
// 例如 "swapExactTokensForTokens(uint256,uint256,address[],address,uint256)"。
func NewCall(signature string, args ...Value) (Call, error) {
	name, types, err := ParseSignature(signature)
	if err != nil {
		return Call{}, err
	}
	if len(types) != len(args) {
		return Call{}, fmt.Errorf("%s 需要 %d 个参数，给了 %d 个", name, len(types), len(args))
	}
	return Call{signature: signature, types: types, values: args}, nil
}

// MustCall 和 NewCall 一样，但签名写错时直接 panic。适合写在包级变量里、
// 签名是常量字面量的场合（写错属于编程错误，启动就该炸掉）。
func MustCall(signature string, args ...Value) Call {
	c, err := NewCall(signature, args...)
	if err != nil {
		panic(err)
	}
	return c
}

// Signature 返回函数签名。
func (c Call) Signature() string { return c.signature }

// Selector 返回 4 字节选择器。
func (c Call) Selector() []byte {
	return Selector(c.signature)
}

// Data 返回完整的 calldata：选择器 + 编码后的参数。
func (c Call) Data() ([]byte, error) {
	body, err := Encode(c.types, c.values)
	if err != nil {
		return nil, err
	}
	return append(c.Selector(), body...), nil
}

// DecodeReturns 按返回值类型解 calldata 之外的数据。
func (c Call) DecodeReturns(returnTypes []Type, data []byte) ([]Value, error) {
	return Decode(returnTypes, data)
}

// ParseSignature 把 "name(type1,type2)" 拆成函数名和参数类型表。
func ParseSignature(sig string) (string, []Type, error) {
	sig = strings.TrimSpace(sig)
	open := strings.Index(sig, "(")
	if open < 0 || !strings.HasSuffix(sig, ")") {
		return "", nil, fmt.Errorf("函数签名 %q 写法不对", sig)
	}
	name := sig[:open]
	if name == "" {
		return "", nil, fmt.Errorf("函数签名 %q 缺少函数名", sig)
	}
	inner := sig[open+1 : len(sig)-1]
	if strings.TrimSpace(inner) == "" {
		return name, nil, nil
	}
	types, err := ParseTypeList(inner)
	if err != nil {
		return "", nil, fmt.Errorf("函数 %s 的参数：%w", name, err)
	}
	return name, types, nil
}

// ParseTypeList 解析逗号分隔的类型表（顶层逗号才分隔，括号里的逗号不算）。
func ParseTypeList(s string) ([]Type, error) {
	var out []Type
	depth := 0
	start := 0
	flush := func(end int) error {
		part := strings.TrimSpace(s[start:end])
		if part == "" {
			return errors.New("类型表里有空的类型")
		}
		t, err := ParseType(part)
		if err != nil {
			return err
		}
		out = append(out, t)
		return nil
	}
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return nil, errors.New("类型表里的括号不配对")
			}
		case ',':
			if depth == 0 {
				if err := flush(i); err != nil {
					return nil, err
				}
				start = i + 1
			}
		}
	}
	if depth != 0 {
		return nil, errors.New("类型表里的括号不配对")
	}
	if err := flush(len(s)); err != nil {
		return nil, err
	}
	return out, nil
}
