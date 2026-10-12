package chainkit

import (
	"context"
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common/hexutil"
)

// TestNewClient_ReadOnlyBalance 只读客户端：不提供 OnTransfer/Addresses 也能建起来并查余额。
//
// 这两个字段是给扫描用的（没有关注地址就无从判断哪笔转账与我们有关），
// 但「只读余额」这类调用方两者都不需要。New 强制要求它们，等于逼调用方
// 传两个假函数来满足库的类型——这里的构造函数就是为了不那样做而存在。
func TestNewClient_ReadOnlyBalance(t *testing.T) {
	const want = 2_000_000_000_000_000_000 // 2 BNB
	node := newFakeNode(t, 56)
	node.handle = func(n *fakeNode, method string, params []json.RawMessage) (any, *rpcError) {
		if method == "eth_getBalance" {
			return hexutil.Big(*big.NewInt(want)), nil
		}
		return nil, nil
	}

	c, err := NewClient(Config{
		Chain:   "bsc",
		RPCs:    []string{node.URL()},
		ChainID: 56,
	})
	if err != nil {
		t.Fatalf("NewClient 失败: %v", err)
	}
	defer func() { _ = c.Close() }()

	got, err := c.Balance(context.Background(), "0x0000000000000000000000000000000000000001",
		&Token{Symbol: "BNB", Decimals: 18})
	if err != nil {
		t.Fatalf("Balance 失败: %v", err)
	}
	if got.String() != big.NewInt(want).String() {
		t.Errorf("余额 = %s，期望 %d", got, want)
	}
}

// TestNewClient_CannotWatch 只读实例调 Watch 要给明确错误，
// 而不是扫到一半才发现没有回调（或更糟：nil 函数 panic）。
func TestNewClient_CannotWatch(t *testing.T) {
	node := newFakeNode(t, 56)
	c, err := NewClient(Config{Chain: "bsc", RPCs: []string{node.URL()}, ChainID: 56})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	err = c.Watch(context.Background())
	if err == nil {
		t.Fatal("只读客户端不该能 Watch")
	}
	if !strings.Contains(err.Error(), "只读") {
		t.Errorf("错误信息应说明是只读实例: %v", err)
	}
}

// TestNewClient_RequiresRPC 一个端点都没有时要早失败
func TestNewClient_RequiresRPC(t *testing.T) {
	if _, err := NewClient(Config{Chain: "bsc"}); err == nil {
		t.Error("没有 RPC 端点时应当报错")
	}
}

// TestNew_StillRequiresScanFields 回归守卫：改造 New 的时候不能顺手把它的
// 前置校验丢掉——现有调用方（chain-pay 的扫链）依赖它早失败。
func TestNew_StillRequiresScanFields(t *testing.T) {
	node := newFakeNode(t, 56)
	base := Config{
		Chain:   "bsc",
		RPCs:    []string{node.URL()},
		ChainID: 56,
		Tokens:  []Token{{Symbol: "BNB"}},
	}

	t.Run("缺 OnTransfer", func(t *testing.T) {
		cfg := base
		cfg.Addresses = func() []string { return nil }
		if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "OnTransfer") {
			t.Errorf("应当报缺 OnTransfer，得到 %v", err)
		}
	})
	t.Run("缺 Addresses", func(t *testing.T) {
		cfg := base
		cfg.OnTransfer = func(context.Context, *Transfer) error { return nil }
		if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "Addresses") {
			t.Errorf("应当报缺 Addresses，得到 %v", err)
		}
	})
	t.Run("缺币种", func(t *testing.T) {
		cfg := Config{Chain: "bsc", RPCs: []string{node.URL()}, ChainID: 56,
			OnTransfer: func(context.Context, *Transfer) error { return nil },
			Addresses:  func() []string { return nil }}
		if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "币种") {
			t.Errorf("应当报缺币种，得到 %v", err)
		}
	})
	t.Run("齐全可建", func(t *testing.T) {
		cfg := base
		cfg.OnTransfer = func(context.Context, *Transfer) error { return nil }
		cfg.Addresses = func() []string { return nil }
		c, err := New(cfg)
		if err != nil {
			t.Fatalf("字段齐全时应当能建: %v", err)
		}
		_ = c.Close()
	})
}
