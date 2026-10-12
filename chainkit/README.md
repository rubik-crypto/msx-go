# chainkit 扫链库

多模块共用的链上读写库：**监听充值 + 发起提现**，不依赖任何数据库。

解决的重复劳动：区块抓取、ERC20 日志解析、确认数等待、游标推进、RPC 限流退避。
这些与业务无关，但此前在 `wallet`、`scanner`、`dex`、`chain-pay` 里各写了一遍
（`Transfer` 的 topic 常量就抄了 3 份）。

```
调用方提供：游标存储、关注地址、入账回调
chainkit 提供：RPC 连接池 + 扫描循环 + 日志解析 + 签名广播
```

## 快速开始

```go
c, err := chainkit.New(chainkit.Config{
    Chain: "bsc",
    RPCs:  []string{"https://rpc1.example.com", "https://rpc2.example.com"},
    Tokens: []chainkit.Token{
        {Symbol: "BNB"},                                                       // 原生币：Contract 为空
        {Symbol: "USDT", Contract: "0x55d3...7955", Decimals: 18},
    },

    Addresses: func() []string { return myDepositAddresses() }, // 每轮扫描前调用，新地址即时生效
    Cursor:    myMongoCursor,                                   // 库不落库，游标由调用方存

    // 看到就回调，不等确认数——确认由充值业务自己跟（见下）
    OnTransfer: func(ctx context.Context, t *chainkit.Transfer) error {
        if seen(t.Key()) { // 必须去重：回调是 at-least-once
            return nil
        }
        return savePendingDeposit(t) // 先记 pending，确认数够了再入账
    },
})
if err != nil {
    return err
}
defer c.Close()

go c.Watch(ctx) // 阻塞式扫描循环

// 确认数由调用方自己攒（Transfer.Block 已经在回调里给了）
n, err := c.Confirmations(ctx, t.Block) // 该块之上又出了几个块
if n >= requiredConfirmations {
    credit(t.Key())
}

// 提现（可选，需要 PrivateKey）
txHash, err := c.Send(ctx, &chainkit.SendRequest{
    To:     "0x...",
    Amount: mustParse("100", 18), // 最小单位
    Token:  usdt,
})
```

`Config` 的完整字段见 `chainkit.go`。几项要点：

| 字段 | 说明 |
|---|---|
| `RPCs` | 按优先级排列，**故障转移**关系（见下） |
| `Tokens` | 静态币种配置。原生币是 `Contract` 为空的那一项；不配则不扫原生 |
| `TokenSource` | 动态币种来源，设置后覆盖 `Tokens`；每轮读一次，增删改免重启（见「运行时增删改」） |
| `Confirmations` | 可选滞后，默认 0（一路扫到链头）。只用来滤掉浅层重组噪音，**不产生「已确认」保证** |
| `Addresses` | 每轮调用一次，返回空列表即暂停监听。新地址下一轮生效 |
| `Cursor` | 留空 = 内存游标（进程重启即丢，仅 DEMO/测试可用） |
| `PrivateKey` | 只有提现用得到；只读场景不要配 |
| `Logf` | 库不依赖 `common/logger`（那要求先初始化 config.yaml），默认打到 stderr |

## 三条必须先知道的语义

### 1. 看到就回调，不等确认数

扫描一路追到链头：链上出现一笔打给关注地址的转账，立刻回调。库只回答「链上出现了什么」。

**等待确认、判断这笔充值是否最终成立，是充值业务自己的事**——它才有「记一笔 pending
充值、够了再入账」所需要的那张表。库这边不替调用方保管未确认状态，也不做那个状态机。

所以 `OnTransfer` 的语义是**看到**，不是**已确认**：它可能是随后被回滚的交易。入账前
自己攒确认数，`Transfer.Block` 已经在回调里给了：

```go
OnTransfer: func(ctx context.Context, t *chainkit.Transfer) error {
    if seen(t.Key()) { return nil } // 幂等
    return savePendingDeposit(t)    // 记 pending，别直接入账
}

// 另一个 goroutine / 定时任务里
for _, d := range pendingDeposits() {
    n, err := c.Confirmations(ctx, d.Block) // 该块之上又出了几个块
    if err != nil || n < required {
        continue
    }
    credit(d.Key()) // 到这一步才动钱
}
```

配套地，游标的语义是「**已经扫到**哪个高度」，不是「哪个高度的充值已经可以入账」。
两者别混用同一个值：前者可以一路追到链头，后者必须比链头落后一个确认数窗口。

`Config.Confirmations` 默认 0（扫到链头）。设成 >0 只是把最近几个块挡在回调之外，
用来滤掉浅层重组的噪音；**它不产生任何「已确认」保证**。想让入账更稳，把确认数加在
自己那一步的判断上，而不是指望这个字段。

### 2. 回调是 at-least-once，调用方必须去重

`OnTransfer` 返回 error 时**不推进游标**，该区间下一轮重扫，之前的转账会重放。
用 `Transfer.Key()`（`txHash:kind:index`）做幂等键，且必须落在数据库里——
进程内 map 挡不住重启。

`Kind` 不能省：同一笔交易里原生转账的序号和代币日志的序号可能相同，只用
`(txHash, index)` 会互相覆盖。

### 3. 游标与地址都由调用方提供

库不做任何持久化。`Cursor` 是两方法的接口：

```go
type Cursor interface {
    Load(ctx context.Context) (block uint64, ok bool, err error)
    Save(ctx context.Context, block uint64) error
}
```

`Save` 只在整段扫描成功之后调用，所以「游标在哪个高度」就等于「哪一段的副作用已经做完了」。

## 多 RPC：故障转移，不是负载均衡

同一时刻只用 `RPCs[0]`，它出问题才切下一个；首选端点恢复后会切回去。

不做轮询是有原因的：不同节点的 head 高度不一致，轮询会让目标高度忽高忽低、游标来回抖；
更糟的是拿到偏矮的 head 会让扫描「倒退」，而这在无数据库的设计里无法与链重组区分。

- 启动时逐个校验 `eth_chainId`，**不匹配的端点直接剔除**（端点数组里混进测试网地址是静默的数据损坏）
- 传输层错误与限流 → 换端点，并按指数退避给它冷却
- 参数错误、方法不支持 → 不换端点（换到哪都一样），走回退阶梯

## 成本模型：代币便宜，原生币贵

| | 做法 | 成本 |
|---|---|---|
| 代币 | `eth_getLogs`，`address=[代币合约]` + `topics[2]=[关注地址]` | 一次请求覆盖一整段区间 |
| 原生币 | 逐块取回完整交易列表，本地筛 `tx.To` | 随链上交易量线性上涨 |

**收款方过滤下推给节点**是代币路径省开销的本质。代价是请求里必须同时限定代币合约——
不限定的话，任意合约伪造一条 `Transfer(?, 我们的地址, ?)` 日志就能凭空入账。

原生币没有这种接口（EVM 不提供按收款方筛交易的 RPC），所以**默认不扫原生**，
只有配了 `Contract` 为空的 Token 才走这条路，且用更小的 `NativeBatch`。

请求过大时的回退阶梯：

```
限流            → 整批放弃，交给端点池轮换 / 外层退避（绝不拆小请求：碎请求只会把限流撞得更死）
区间/结果集过大 → 二分区间重查，并记住更小的安全区间
方法不支持      → 改走 eth_getBlockReceipts，本地筛日志（收据路径批量单独限流，被拒就减半）
地址数量过多    → 按地址分片查询后合并去重
```

批量请求还有一层自适应：`eth_getLogs` 的区间、单次地址数、`eth_getBlockReceipts` 的批大小、
`eth_getBlockByNumber` 的批大小，都会在节点以「太大」为由拒绝时缩小并记住，之后一直用更小的值。
这不是为了迁就小气节点——被拒意味着整批数据都拿不到，重试必须缩请求才有意义。

缩哪个维度取决于错误说的是什么，两者不能混：
- 「结果集太大」（`query exceeds max results`）→ 缩**区间**
- 「请求体太大」（HTTP 400/413）→ 缩**数组**（地址数/批量）

注意不少节点把「请求体太大」也报成 **HTTP 400**，所以 400 按请求过大处理；
而 `-32602` 这类带 "max results" 字样的才当区间问题。

**免费公共节点**经常干脆不支持 `eth_getLogs`（返回 `-32000 ... is not supported`），
这时收据路径会自动接管——但要清楚它贵得多，生产环境请用支持 `eth_getLogs` 的节点。

## 运行时增删改：币种与地址

两样东西都可以不停服改。**关注地址**用 `Addresses`，**币种**用 `TokenSource`——
都是每轮扫描前调用一次，返回什么就监听什么。

### 地址会一直增加

新用户拿到充值地址后，下一轮扫描就生效，不用重启也不用通知库。但规模是有代价的：
地址是**下推给节点做过滤**的（`topics[2]`），每个地址在请求体里占 66 字节。

| 关注地址数 | 单次请求体 | 每轮请求数（区间一次覆盖） |
|---|---|---|
| 1000 | ~66 KB | 1 |
| 1 万 | ~66 KB/片 | 10 |
| 10 万 | 同上 | 100 |

分片大小会自适应：节点以「请求太大」拒收时自动对半拆并记住能用的尺寸，
所以地址涨上去不会突然整体扫不动。但**这是 EVM 的固有特性**——没有「按收款方查历史转账」
这种 RPC，只能把地址列表带上去。上万地址时先把 `Batch` 调小，十万级就该考虑自建节点，
或者换成全量索引那套（`server/scanner`）。

### 币种可以增删改

```go
TokenSource: func() []chainkit.Token { return myTokenCache.All() }, // 优先于静态的 Tokens
```

传个读自己缓存的函数即可，怎么刷、多久刷由你决定（wallet 那边是 30s 刷一次）。

有一点必须清楚：**改配置不会回补历史**。

- **新增**代币：从当前游标往后开始识别，在它加进来之前收到的充值不会被扫到
- **删除**：立刻停止识别；之后再加回来，中间那段同样不会补扫
- **改 decimals**：只影响之后回调里的 `Transfer.Token`，已回调的不动

要补历史就显式指定 `StartBlock` 重扫那段区间——重复回调靠 `Key()` 去重吸收。

还有个反直觉的地方：把原生币（`Contract` 为空）加进列表会开启**原生扫描**，
那是逐块取全量交易的贵路径，别拿它来回切。

## DEMO

```bash
go build -o bin/chainkit-demo ./chainkit/cmd/demo

./bin/chainkit-demo \
    -rpc https://bsc.meowrpc.com \
    -chain bsc \
    -tokens USDT:0x55d398326f99059fF775485246999027B3197955:18,BNB:-:18 \
    -addr 0x<你的充值地址> \
    -lookback 200 -confirmations 15
```

DEMO 会把每笔识别到的充值打印出来，重复回调的会标注 `[重复，调用方应忽略]`
（用来演示去重是必需的）。加 `-balance <地址>:<币种>` 查余额、`-send <地址>:<金额>:<币种> -key <私钥>` 演示提现。

## 已知限制

- 只实现了 EVM。`Type: "chainos"` 会返回明确的未实现错误，接口已按可扩展设计。
- 原生币路径不查收据，因此拿不到交易执行结果。对充值地址（EOA）没有实际影响——
  往没有代码的地址转原生币不可能回滚；但把合约地址当收款地址配的话，回滚的交易也会算成充值。
- 提现用 legacy（EIP-155）交易，不做手续费/余额预检、不做 nonce 复用、不支持 EIP-1559。
- 不做深层重组的完整修复：首选节点报出的链头低于游标时会回退重扫（重复回调由调用方去重吸收），
  备用节点报出同样的现象时只等待——那只是节点落后。
- 不处理非标准 ERC20（没有 `Transfer` 事件、`decimals` 异常）。
- 落后超过 `MaxCatchup`（默认 1 万块）会快进，**跳过区间内的充值不会回调**，日志里会有告警。

## 测试

不需要任何外部依赖：每个测试在本进程内起一个假的 JSON-RPC 节点。

```bash
go test ./chainkit/... -race
```

覆盖了：`eth_getLogs` 参数拼装（含中间位必须是 `null` 的回归）、区间过大二分、限流不缩区间、
地址分片合并去重、原生币过滤（合约创建 / 零值 / gas 钱包补气 / 非关注地址）、
错误分类、端点粘性与轮换、链 ID 剔除、默认扫到链头 / 可选滞后、回调失败不推进游标、
批处理上限、连扫 100 块的请求数（代币 1 次 / 原生 5 次）、
首选节点回滚回退 vs 备用节点落后不回退、落伍快进、首启回溯、
收据路径回退与缩批、批量被拒后缩批（400）、`transactions` 只给哈希字符串的区块响应、
地址大小写归一化、确认数计算、币种运行时增删改（含 TokenSource 覆盖静态配置）、
地址分片被拒后自适应缩分片。

真实链验证（用 `cmd/demo`，需要能出网的节点）：

```bash
./bin/chainkit-demo -rpc <支持 eth_getLogs 的节点> -chain bsc \
    -tokens USDT:0x55d398326f99059fF775485246999027B3197955:18 \
    -addr <一个近期有进账的地址> -lookback 500 -batch 500 -confirmations 15
```
