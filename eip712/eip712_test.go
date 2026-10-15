package eip712

import (
	"encoding/hex"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

// etherMailTypedData 是 EIP-712 规范里那个经典示例，各大钱包库的文档都用它，
// 算出来的摘要是一个公开的固定值，正好拿来钉住整条编码链路。
func etherMailTypedData() TypedData {
	return TypedData{
		Types: map[string][]Field{
			"Person": {
				{Name: "name", Type: "string"},
				{Name: "wallet", Type: "address"},
			},
			"Mail": {
				{Name: "from", Type: "Person"},
				{Name: "to", Type: "Person"},
				{Name: "contents", Type: "string"},
			},
		},
		PrimaryType: "Mail",
		Domain: Domain{
			Name:              "Ether Mail",
			Version:           "1",
			ChainID:           1,
			VerifyingContract: "0xCcCCccccCCCCcCCCCCCcCcCccCcCCCcCcccccccC",
		},
		Message: map[string]any{
			"from": map[string]any{
				"name":   "Cow",
				"wallet": "0xCD2a3d9F938E13CD947Ec05AbC7FE734Df8DD826",
			},
			"to": map[string]any{
				"name":   "Bob",
				"wallet": "0xbBbBBBBbbBBBbbbBbbBbbbbBBbBbbbbBbBbbBBbB",
			},
			"contents": "Hello, Bob!",
		},
	}
}

// 公开的固定向量：EIP-712 规范示例的最终摘要。
// 这个值来自规范，并被主流钱包库反复引用，本包的实现必须算出同一个。
func TestEtherMailDigest(t *testing.T) {
	const want = "be609aee343fb3c4b28e1df9e632fca64fcfaede20f02e86244efddf30957bd2"

	got, err := etherMailTypedData().Hash()
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(got[:]) != want {
		t.Fatalf("摘要对不上：\n得到 %x\n应为 %s", got[:], want)
	}
}

// 第二个固定向量，覆盖数组（定长 + 动态）、布尔、bytes32、bytes、以及非 ASCII 字符串。
// 这个值是用 go-ethereum 的 apitypes 实现算出来的，两边独立算一遍互相对照。
func TestComplexDigest(t *testing.T) {
	const want = "41a995de1fce33e5641838a44333c6a040b1cf294bba061e623fd50b9a0351bc"

	td := TypedData{
		Types: map[string][]Field{
			"Order": {
				{Name: "maker", Type: "address"},
				{Name: "amount", Type: "uint256"},
				{Name: "sizes", Type: "uint32[3]"},
				{Name: "tags", Type: "string[]"},
				{Name: "active", Type: "bool"},
				{Name: "digest", Type: "bytes32"},
				{Name: "blob", Type: "bytes"},
				{Name: "note", Type: "string"},
			},
		},
		PrimaryType: "Order",
		Domain: Domain{
			Name:              "Example Exchange",
			Version:           "2",
			ChainID:           137,
			VerifyingContract: "0x0000000000000000000000000000000000000abc",
		},
		Message: map[string]any{
			"maker":  "0xCD2a3d9F938E13CD947Ec05AbC7FE734Df8DD826",
			"amount": mustBig(t, "12345678901234567890"),
			"sizes":  []any{uint32(1), uint32(2), uint32(3)},
			"tags":   []any{"a", "b"},
			"active": true,
			"digest": mustHex(t, "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"),
			"blob":   []byte{0xde, 0xad, 0xbe, 0xef},
			"note":   "中文也要能签",
		},
	}

	got, err := td.Hash()
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(got[:]) != want {
		t.Fatalf("摘要对不上：\n得到 %x\n应为 %s", got[:], want)
	}
}

// 签完要能还原出签名者，说明签名格式（末尾补 27/28）也是对的。
func TestSignAndRecover(t *testing.T) {
	key, err := crypto.HexToECDSA("4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318")
	if err != nil {
		t.Fatal(err)
	}
	want := crypto.PubkeyToAddress(key.PublicKey)

	td := etherMailTypedData()
	sig, err := td.Sign(key)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sig, "0x") || len(sig) != 132 {
		t.Fatalf("签名应该是 0x 开头的 65 字节，实际 %q", sig)
	}
	if v := sig[len(sig)-2:]; v != "1b" && v != "1c" {
		t.Fatalf("签名的恢复标识应该是 1b 或 1c（也就是 27/28），实际 %s", v)
	}

	got, err := Recover(td, sig)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("还原出来的地址不对：得到 %s，应为 %s", got.Hex(), want.Hex())
	}
}

// 数据改一个字，摘要就必须变——这是防篡改的全部意义所在。
func TestDigestChangesWithData(t *testing.T) {
	base, err := etherMailTypedData().Hash()
	if err != nil {
		t.Fatal(err)
	}

	td := etherMailTypedData()
	td.Message["contents"] = "Hello, Bob?"
	changed, err := td.Hash()
	if err != nil {
		t.Fatal(err)
	}
	if base == changed {
		t.Fatal("改了内容摘要却没变")
	}

	// 域里改链号同理：这正是域存在的目的——防止把 A 链的签名拿到 B 链上用。
	td2 := etherMailTypedData()
	td2.Domain.ChainID = 137
	other, err := td2.Hash()
	if err != nil {
		t.Fatal(err)
	}
	if base == other {
		t.Fatal("换了链号摘要却没变")
	}
}

// 域里没给的项不能参与计算，否则和对方（钱包/合约）算出来的对不上。
func TestDomainOnlyUsesProvidedFields(t *testing.T) {
	full, err := Domain{Name: "X", Version: "1", ChainID: 1}.Separator()
	if err != nil {
		t.Fatal(err)
	}
	withContract, err := Domain{
		Name: "X", Version: "1", ChainID: 1,
		VerifyingContract: "0x0000000000000000000000000000000000000001",
	}.Separator()
	if err != nil {
		t.Fatal(err)
	}
	if full == withContract {
		t.Fatal("多了一项 verifyingContract，域哈希应该跟着变")
	}

	// 同样的域算两次必须稳定。
	again, err := Domain{Name: "X", Version: "1", ChainID: 1}.Separator()
	if err != nil {
		t.Fatal(err)
	}
	if full != again {
		t.Fatal("同样的域两次算出来应该一样")
	}
}

// 类型声明要按 EIP-712 的规矩拼：主类型在前，引用到的类型按字母序跟在后面。
func TestEncodeTypeLayout(t *testing.T) {
	got, err := encodeType(etherMailTypedData().Types, "Mail")
	if err != nil {
		t.Fatal(err)
	}
	const want = "Mail(Person from,Person to,string contents)Person(string name,address wallet)"
	if got != want {
		t.Fatalf("类型声明不对：\n得到 %s\n应为 %s", got, want)
	}
}

// 多个引用类型必须按字母序，顺序不对签名就没人认。
func TestEncodeTypeSortsDependencies(t *testing.T) {
	types := map[string][]Field{
		"Root":  {{Name: "z", Type: "Zeta"}, {Name: "a", Type: "Alpha"}},
		"Zeta":  {{Name: "x", Type: "uint256"}},
		"Alpha": {{Name: "x", Type: "uint256"}},
	}
	got, err := encodeType(types, "Root")
	if err != nil {
		t.Fatal(err)
	}
	const want = "Root(Zeta z,Alpha a)Alpha(uint256 x)Zeta(uint256 x)"
	if got != want {
		t.Fatalf("引用类型应该按字母序排在主类型之后：\n得到 %s\n应为 %s", got, want)
	}
}

// 负数要按两补码编码，而不是直接报错。
func TestNegativeIntUsesTwosComplement(t *testing.T) {
	mk := func(v int64) TypedData {
		return TypedData{
			Types:       map[string][]Field{"S": {{Name: "v", Type: "int256"}}},
			PrimaryType: "S",
			Message:     map[string]any{"v": big.NewInt(v)},
		}
	}
	minusOne, err := mk(-1).Hash()
	if err != nil {
		t.Fatal(err)
	}
	// -1 在 256 位里全是 1，也就是 0xff…ff。
	word, err := encodeAtomic("int256", big.NewInt(-1))
	if err != nil {
		t.Fatal(err)
	}
	for i, b := range word {
		if b != 0xff {
			t.Fatalf("两补码 -1 的第 %d 个字节应该是 0xff，实际 %#x", i, b)
		}
	}
	if one, _ := mk(1).Hash(); one == minusOne {
		t.Fatal("1 和 -1 的摘要不该一样")
	}
}

// 无符号类型不许填负数——让它在算之前就报错，别悄悄算出一个怪值。
func TestUintRejectsNegative(t *testing.T) {
	td := TypedData{
		Types:       map[string][]Field{"S": {{Name: "v", Type: "uint256"}}},
		PrimaryType: "S",
		Message:     map[string]any{"v": big.NewInt(-5)},
	}
	if _, err := td.Hash(); err == nil {
		t.Fatal("uint256 填负数应该报错")
	}
}

// 写错的类型名、漏声明的类型、缺字段的值，都要给出看得懂的报错，
// 而不是等算出一串谁也验不过的哈希。
func TestErrorMessages(t *testing.T) {
	cases := []struct {
		name string
		td   TypedData
		want string
	}{
		{
			name: "主类型没声明",
			td:   TypedData{PrimaryType: "Nope", Message: map[string]any{}},
			want: "没有在 Types 里声明",
		},
		{
			name: "字段用到的类型没声明",
			td: TypedData{
				Types:       map[string][]Field{"S": {{Name: "v", Type: "Missing"}}},
				PrimaryType: "S",
				Message:     map[string]any{"v": 1},
			},
			want: "没有在 Types 里声明",
		},
		{
			name: "字段名不合法",
			td: TypedData{
				Types:       map[string][]Field{"S": {{Name: "v", Type: "uint256[abc]"}}},
				PrimaryType: "S",
				Message:     map[string]any{"v": 1},
			},
			want: "数组长度不合法",
		},
		{
			name: "消息里缺字段的值",
			td: TypedData{
				Types:       map[string][]Field{"S": {{Name: "v", Type: "uint256"}}},
				PrimaryType: "S",
				Message:     map[string]any{},
			},
			want: "缺少字段",
		},
		{
			name: "定长数组长度不对",
			td: TypedData{
				Types:       map[string][]Field{"S": {{Name: "v", Type: "uint8[3]"}}},
				PrimaryType: "S",
				Message:     map[string]any{"v": []any{1, 2}},
			},
			want: "定长数组要 3 个元素",
		},
		{
			name: "地址写错",
			td: TypedData{
				Types:       map[string][]Field{"S": {{Name: "v", Type: "address"}}},
				PrimaryType: "S",
				Message:     map[string]any{"v": "not-an-address"},
			},
			want: "不是合法的以太坊地址",
		},
		{
			name: "域里的 salt 长度不对",
			td: TypedData{
				Types:       map[string][]Field{"S": {{Name: "v", Type: "uint8"}}},
				PrimaryType: "S",
				Domain:      Domain{Salt: []byte{1, 2, 3}},
				Message:     map[string]any{"v": 1},
			},
			want: "salt 必须是 32 字节",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := c.td.Hash()
			if err == nil {
				t.Fatalf("应该报错，内容要包含 %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("报错内容不对：%v（期望包含 %q）", err, c.want)
			}
		})
	}
}

// 嵌套结构体、套娃数组、以及域里带 salt 的情况都要能算。
func TestNestedAndArrayOfStructs(t *testing.T) {
	td := TypedData{
		Types: map[string][]Field{
			"Item":  {{Name: "sku", Type: "string"}, {Name: "qty", Type: "uint16"}},
			"Batch": {{Name: "items", Type: "Item[]"}, {Name: "grid", Type: "uint8[2][2]"}},
		},
		PrimaryType: "Batch",
		Domain:      Domain{Name: "Nested", Version: "1", ChainID: 1, Salt: make([]byte, 32)},
		Message: map[string]any{
			"items": []any{
				map[string]any{"sku": "a", "qty": uint16(2)},
				map[string]any{"sku": "b", "qty": uint16(3)},
			},
			"grid": []any{[]any{1, 2}, []any{3, 4}},
		},
	}
	h1, err := td.Hash()
	if err != nil {
		t.Fatal(err)
	}

	// 改一个嵌套项，摘要必须跟着变。
	td.Message["items"] = []any{
		map[string]any{"sku": "a", "qty": uint16(2)},
		map[string]any{"sku": "b", "qty": uint16(4)},
	}
	h2, err := td.Hash()
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Fatal("改了嵌套结构体里的字段，摘要却没变")
	}
}

// 字段名大小写对不上时应该宽容一点：前端传 JSON 时经常对不上。
func TestFieldNameCaseInsensitive(t *testing.T) {
	td := TypedData{
		Types:       map[string][]Field{"S": {{Name: "amount", Type: "uint256"}}},
		PrimaryType: "S",
		Message:     map[string]any{"Amount": 7},
	}
	if _, err := td.Hash(); err != nil {
		t.Fatalf("字段名大小写不同也应该能取到值：%v", err)
	}
}

// 还原签名时常见的两种输入错误要给出明确报错。
func TestRecoverRejectsBadSignature(t *testing.T) {
	td := etherMailTypedData()
	if _, err := Recover(td, "0x1234"); err == nil {
		t.Fatal("长度不对的签名应该报错")
	}
	if _, err := Recover(td, "0x"+strings.Repeat("zz", 65)); err == nil {
		t.Fatal("不是十六进制的签名应该报错")
	}
	long := "0x" + strings.Repeat("11", 65)
	if _, err := Recover(td, long); err == nil {
		t.Fatal("恢复标识不合法的签名应该报错")
	}
}

func mustBig(t *testing.T, s string) *big.Int {
	t.Helper()
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("解析大整数失败：%s", s)
	}
	return n
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
