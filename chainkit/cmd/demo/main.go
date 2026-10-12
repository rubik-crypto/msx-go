// chainkit DEMO：连一条 EVM 链，监听充值并演示提现。
//
// 它刻意做得很薄——真正值得看的是 Config 里那几项由调用方注入的东西：
// Addresses / Cursor / OnTransfer。模块接入时把它们换成自己的（读地址表、
// 写游标表、入账），本文件里的其余部分可以照抄。
//
// 运行：
//
//	go build -o bin/chainkit-demo ./chainkit/cmd/demo
//
//	./bin/chainkit-demo \
//	    -rpc https://bsc.meowrpc.com \
//	    -chain bsc \
//	    -tokens USDT:0x55d398326f99059fF775485246999027B3197955:18,BNB:-:18 \
//	    -addr 0x<你的充值地址> \
//	    -lookback 200 -confirmations 15
//
// 另加 -balance 或 -send 可演示余额查询与提现（-send 需要 -key）。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/rubik-crypto/msx-go/chainkit"
)

// listFlag 可重复传、也可用逗号分隔的字符串列表
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }

func (l *listFlag) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			*l = append(*l, p)
		}
	}
	return nil
}

func main() {
	var (
		rpcs          listFlag
		tokens        listFlag
		addrs         listFlag
		chain         = flag.String("chain", "bsc", "链名标识，仅用于日志与 Transfer.Chain")
		rpcType       = flag.String("type", "evm", "链类型")
		chainID       = flag.Uint64("chain-id", 0, "链 ID（0 = 以第一个可用端点为准）")
		confirmations = flag.Uint64("confirmations", 0, "可选的扫描滞后块数；0 = 扫到链头（默认）。确认数由充值业务自己跟")
		batch         = flag.Uint64("batch", 1000, "单轮推进的最大区块数，也是代币日志的单次查询区间")
		nativeBatch   = flag.Uint64("native-batch", 20, "原生币单次批量取块的块数")
		lookback      = flag.Uint64("lookback", 200, "首次启动回溯的块数")
		start         = flag.Uint64("start", 0, "强制起始高度（>0 时覆盖 lookback）")
		interval      = flag.Duration("interval", 0, "追平后的空转间隔（0 = 默认 3s）")
		gasWallet     = flag.String("gas-wallet", "", "gas 钱包地址；它发出的原生转账会被跳过")
		key           = flag.String("key", "", "提现私钥（hex）")
		balanceFlag   = flag.String("balance", "", "演示余额查询：<地址>[:币种符号]")
		sendFlag      = flag.String("send", "", "演示提现：<收款地址>:<金额>:<币种符号>")
	)
	flag.Var(&rpcs, "rpc", "RPC 端点，可重复或用逗号分隔（第一个是首选，其余是故障转移）")
	flag.Var(&tokens, "tokens", "币种：SYMBOL:CONTRACT:DECIMALS，原生币的 CONTRACT 写 -")
	flag.Var(&addrs, "addr", "要监听的充值地址，可重复")
	flag.Parse()

	if len(rpcs) == 0 {
		fatal("-rpc 至少要给一个 RPC 端点")
	}
	if len(tokens) == 0 {
		fatal("-tokens 至少要给一个币种")
	}
	if len(addrs) == 0 {
		fatal("-addr 至少要给一个要监听的地址")
	}

	tokenList, err := parseTokens(tokens)
	if err != nil {
		fatal("%v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 关注地址：DEMO 用固定列表。生产里这个函数应该去查库，
	// 它每轮扫描前都会被调用一次，所以新增地址能即时生效。
	addresses := func() []string { return addrs }

	// 只读用途（不提现）时不要配私钥：库只在 Send 时才会用到它
	var privateKey string
	if sendFlag != nil && *sendFlag != "" {
		if *key == "" {
			fatal("-send 需要同时提供 -key")
		}
		privateKey = *key
	}

	// 去重表：回调是 at-least-once，重扫时会重放已回调过的转账。
	// 真实模块要把这张表换成数据库里的唯一键，进程重启也不能丢。
	seen := make(map[string]struct{})
	var seenMu sync.Mutex

	// 先声明再赋值：回调里要用它查当前确认数
	var c *chainkit.Chain

	c, err = chainkit.New(chainkit.Config{
		Chain:         *chain,
		Type:          *rpcType,
		RPCs:          rpcs,
		ChainID:       *chainID,
		Tokens:        tokenList,
		Confirmations: *confirmations,
		Batch:         *batch,
		NativeBatch:   *nativeBatch,
		Lookback:      *lookback,
		StartBlock:    *start,
		Interval:      *interval,
		GasWallet:     *gasWallet,
		Addresses:     addresses,
		PrivateKey:    privateKey,
		// 看到就回调，不等确认数——下面顺手把确认数打出来，
		// 就是为了演示「入账判据要由调用方自己拿这个数去比」。
		OnTransfer: func(ctx context.Context, t *chainkit.Transfer) error {
			seenMu.Lock()
			_, dup := seen[t.Key()]
			seen[t.Key()] = struct{}{}
			seenMu.Unlock()

			tag := ""
			if dup {
				tag = " [重复，调用方应忽略]"
			}
			// 链头还没到这个块时是 0；刚出的块是 1，不足以入账
			n, err := c.Confirmations(ctx, t.Block)
			if err != nil {
				n = 0
			}
			fmt.Printf("转账 %s 块=%d 确认数=%d %s %s → %s  tx=%s%s\n",
				t.Chain, t.Block, n, t.Kind, t.Human()+" "+t.Token.Symbol, t.To, t.TxHash, tag)
			return nil
		},
	})
	if err != nil {
		fatal("初始化失败: %v", err)
	}
	defer c.Close()

	id, err := c.ChainID(ctx)
	if err != nil {
		fatal("获取链 ID 失败: %v", err)
	}
	// 取链头失败不该让 DEMO 退出：公共节点限流很常见，扫描循环自己会退避重试
	head, err := c.Head(ctx)
	if err != nil {
		fmt.Printf("已连接 %s（链 ID %d），但暂时取不到链头: %v\n", *chain, id, err)
	} else {
		fmt.Printf("已连接 %s（链 ID %d），当前高度 %d，监听 %d 个地址\n", *chain, id, head, len(addrs))
	}
	for _, t := range tokenList {
		if t.IsNative() {
			fmt.Printf("  币种 %s：原生币\n", t.Symbol)
			continue
		}
		fmt.Printf("  币种 %s：%s\n", t.Symbol, t.Contract)
	}

	if *balanceFlag != "" {
		runBalance(ctx, c, tokenList, *balanceFlag)
	}
	if *sendFlag != "" {
		runSend(ctx, c, tokenList, *sendFlag)
	}

	fmt.Println("开始扫描，Ctrl-C 结束……")
	if err := c.Watch(ctx); err != nil {
		fmt.Printf("扫描已停止: %v\n", err)
	}
}

// runBalance 演示余额查询
func runBalance(ctx context.Context, c *chainkit.Chain, tokens []chainkit.Token, spec string) {
	addr, symbol, _ := strings.Cut(spec, ":")
	token, err := findToken(tokens, symbol)
	if err != nil {
		fmt.Printf("余额查询跳过: %v\n", err)
		return
	}
	bal, err := c.Balance(ctx, addr, token)
	if err != nil {
		fmt.Printf("余额查询失败: %v\n", err)
		return
	}
	fmt.Printf("余额 %s = %s %s\n", addr, chainkit.FormatUnits(bal, token.Decimals), token.Symbol)
}

// runSend 演示提现：签名并广播一笔转账
func runSend(ctx context.Context, c *chainkit.Chain, tokens []chainkit.Token, spec string) {
	parts := strings.Split(spec, ":")
	if len(parts) != 3 {
		fmt.Println("提现跳过: -send 格式应为 <收款地址>:<金额>:<币种符号>")
		return
	}
	token, err := findToken(tokens, parts[2])
	if err != nil {
		fmt.Printf("提现跳过: %v\n", err)
		return
	}
	// 人类可读金额 → 最小单位
	amount, err := chainkit.ParseUnits(parts[1], token.Decimals)
	if err != nil {
		fmt.Printf("提现跳过: %v\n", err)
		return
	}
	txHash, err := c.Send(ctx, &chainkit.SendRequest{To: parts[0], Amount: amount, Token: *token})
	if err != nil {
		fmt.Printf("提现失败: %v\n", err)
		return
	}
	fmt.Printf("提现已广播 %s %s → %s  tx=%s\n", parts[1], token.Symbol, parts[0], txHash)
}

// findToken 按符号找币种，符号传空时返回第一个原生币
func findToken(tokens []chainkit.Token, symbol string) (*chainkit.Token, error) {
	if symbol == "" {
		for i := range tokens {
			if tokens[i].IsNative() {
				return &tokens[i], nil
			}
		}
		return nil, fmt.Errorf("未指定币种，且配置里没有原生币")
	}
	for i := range tokens {
		if strings.EqualFold(tokens[i].Symbol, symbol) {
			return &tokens[i], nil
		}
	}
	return nil, fmt.Errorf("没有配置名为 %q 的币种", symbol)
}

// parseTokens 解析 SYMBOL:CONTRACT:DECIMALS（原生币的 CONTRACT 写 -）
func parseTokens(specs []string) ([]chainkit.Token, error) {
	out := make([]chainkit.Token, 0, len(specs))
	for _, spec := range specs {
		parts := strings.Split(spec, ":")
		if len(parts) != 3 {
			return nil, fmt.Errorf("币种格式应为 SYMBOL:CONTRACT:DECIMALS（原生币的 CONTRACT 写 -），收到 %q", spec)
		}
		decimals, err := strconv.Atoi(parts[2])
		if err != nil {
			return nil, fmt.Errorf("币种 %q 的精度不是整数: %w", spec, err)
		}
		contract := strings.TrimSpace(parts[1])
		if contract == "-" {
			contract = ""
		}
		out = append(out, chainkit.Token{
			Symbol:   parts[0],
			Contract: contract,
			Decimals: decimals,
		})
	}
	return out, nil
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
