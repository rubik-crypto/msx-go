package chainkit

import (
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestFormatUnits(t *testing.T) {
	pow18 := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)

	cases := []struct {
		name     string
		value    *big.Int
		decimals int
		want     string
	}{
		{"nil", nil, 18, "0"},
		{"零", big.NewInt(0), 18, "0"},
		{"整枚", pow18, 18, "1"},
		{"六位精度的零头", big.NewInt(1234500), 6, "1.2345"},
		{"去尾随零", big.NewInt(1500000), 6, "1.5"},
		{"负数", new(big.Int).Neg(pow18), 18, "-1"},
		{"decimals 为 0", big.NewInt(5), 0, "5"},
		{"最小单位 1", big.NewInt(1), 18, "0.000000000000000001"},
		{"小于一枚", big.NewInt(999), 6, "0.000999"},
		{"负数小数", big.NewInt(-1500000), 6, "-1.5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := FormatUnits(c.value, c.decimals); got != c.want {
				t.Errorf("FormatUnits = %q, 期望 %q", got, c.want)
			}
		})
	}
}

func TestParseUnits(t *testing.T) {
	pow18 := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)

	cases := []struct {
		in       string
		decimals int
		want     *big.Int
	}{
		{"1", 18, pow18},
		{"1.2345", 6, big.NewInt(1234500)},
		{"0.1", 18, new(big.Int).Div(pow18, big.NewInt(10))},
		{"0", 18, big.NewInt(0)},
		{" 2 ", 6, big.NewInt(2000000)},
		{"5", 0, big.NewInt(5)},
	}
	for _, c := range cases {
		got, err := ParseUnits(c.in, c.decimals)
		if err != nil {
			t.Errorf("ParseUnits(%q, %d) 报错: %v", c.in, c.decimals, err)
			continue
		}
		if got.Cmp(c.want) != 0 {
			t.Errorf("ParseUnits(%q, %d) = %s, 期望 %s", c.in, c.decimals, got, c.want)
		}
	}
}

// 精度超出必须报错而不是静默截断——截断等于丢钱
func TestParseUnitsRejectsPrecisionLoss(t *testing.T) {
	for _, in := range []string{"1.2345678", "0.0000001"} {
		if got, err := ParseUnits(in, 6); err == nil {
			t.Errorf("ParseUnits(%q, 6) 应当报错，实际得到 %s", in, got)
		}
	}
	for _, in := range []string{"", "  ", "abc", "1.2.3"} {
		if got, err := ParseUnits(in, 18); err == nil {
			t.Errorf("ParseUnits(%q, 18) 应当报错，实际得到 %s", in, got)
		}
	}
}

// 格式化与解析必须能往返
func TestUnitsRoundTrip(t *testing.T) {
	values := []*big.Int{
		big.NewInt(0),
		big.NewInt(1),
		big.NewInt(999999999999999999),
		new(big.Int).Exp(big.NewInt(10), big.NewInt(30), nil),
	}
	for _, v := range values {
		for _, decimals := range []int{0, 6, 8, 18} {
			text := FormatUnits(v, decimals)
			back, err := ParseUnits(text, decimals)
			if err != nil {
				t.Fatalf("ParseUnits(FormatUnits(%s, %d)=%q) 报错: %v", v, decimals, text, err)
			}
			if back.Cmp(v) != 0 {
				t.Errorf("往返不一致: %s → %q → %s（decimals=%d）", v, text, back, decimals)
			}
		}
	}
}

// 地址进 topics 时必须左补零到 32 字节，否则节点匹配不到
func TestAddrTopic(t *testing.T) {
	addr := "0x55d398326f99059ff775485246999027b3197955"
	got := addrTopic(addr)

	if got != common.BytesToHash(common.HexToAddress(addr).Bytes()) {
		t.Errorf("addrTopic 结果不对: %s", got)
	}
	if got.Hex()[:26] != "0x000000000000000000000000" {
		t.Errorf("addrTopic 未左补零: %s", got.Hex())
	}
	if !strings.HasSuffix(got.Hex(), "55d398326f99059ff775485246999027b3197955") {
		t.Errorf("addrTopic 尾部不是地址: %s", got.Hex())
	}
	// 大小写不同的地址必须归一化成同一个 topic
	if addrTopic(strings.ToUpper(addr[2:])) != got {
		t.Error("addrTopic 对大小写敏感")
	}
}

func TestTopicToAddress(t *testing.T) {
	addr := "0x55d398326f99059ff775485246999027b3197955"
	if got := topicToAddress(addrTopic(addr)); !strings.EqualFold(got, addr) {
		t.Errorf("topicToAddress 往返失败: %s", got)
	}
}

// topicToAddress 必须输出小写。common.Address.Hex() 返回的是 EIP-55 校验和形式
// （0x8894E0a0...），而关注地址集合是按小写存的——直接比对会一条都匹配不上，
// 表现是代币充值全部静默丢失。这个 bug 真实发生过，靠真机跑才暴露出来。
func TestTopicToAddressIsLowercase(t *testing.T) {
	addr := "0x8894E0a0c962CB723c1976a4421c95949bE2D4E3"
	got := topicToAddress(addrTopic(addr))
	if got != strings.ToLower(addr) {
		t.Fatalf("topicToAddress = %s，期望小写 %s", got, strings.ToLower(addr))
	}
	if got != strings.ToLower(got) {
		t.Fatal("返回值里仍有大写字母")
	}
}

func TestERC20TransferData(t *testing.T) {
	to := "0x55d398326f99059ff775485246999027b3197955"
	data := erc20TransferData(to, big.NewInt(1))

	if len(data) != 4+32+32 {
		t.Fatalf("calldata 长度应为 68，实际 %d", len(data))
	}
	if string(data[:4]) != string(transferMethodID) {
		t.Error("方法选择器不是 transfer(address,uint256)")
	}
	// 地址参数在第二段，左补零
	if got := common.BytesToAddress(data[4:36]).Hex(); !strings.EqualFold(got, to) {
		t.Errorf("地址参数错误: %s", got)
	}
	// 金额在第三段，大端
	if got := new(big.Int).SetBytes(data[36:68]); got.Cmp(big.NewInt(1)) != 0 {
		t.Errorf("金额参数错误: %s", got)
	}

	// 大额与零金额也要能正确右对齐
	big1 := new(big.Int).Exp(big.NewInt(10), big.NewInt(30), nil)
	large := erc20TransferData(to, big1)
	if got := new(big.Int).SetBytes(large[36:68]); got.Cmp(big1) != 0 {
		t.Errorf("大额金额参数错误: %s", got)
	}
}

func TestTokenIsNative(t *testing.T) {
	if !(Token{Symbol: "BNB"}).IsNative() {
		t.Error("合约地址为空应视为原生币")
	}
	if (Token{Symbol: "USDT", Contract: "0x1"}).IsNative() {
		t.Error("有合约地址不应视为原生币")
	}
}

func TestTransferKeyDistinguishesKind(t *testing.T) {
	// 同一笔交易里原生转账与代币转账的序号可能撞车，去重键必须能区分
	native := &Transfer{TxHash: "0xaa", Kind: KindNative, Index: 3}
	token := &Transfer{TxHash: "0xaa", Kind: KindToken, Index: 3}
	if native.Key() == token.Key() {
		t.Fatalf("原生与代币的去重键相同: %s", native.Key())
	}
}
