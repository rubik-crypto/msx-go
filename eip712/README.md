# eip712

EIP-712 结构化数据的哈希与签名，够用就好的那一层。

## 什么时候用

- 要给「有结构的消息」签名：限价单、授权凭证、白名单、元交易；
- 服务端要验客户端传来的签名，并还原出签名者地址；
- 需要自己算 EIP-712 摘要去做比对（比如核对某个第三方接口给的哈希）。

不适合的场景：只想签一个普通消息（那是 EIP-191，直接 `personal_sign` 就行），
或者要签一笔链上交易（那是交易签名，不是结构化数据）。

## 怎么用

```go
td := eip712.TypedData{
    Domain: eip712.Domain{
        Name: "Example Exchange", Version: "1", ChainID: 137,
        VerifyingContract: "0x0000000000000000000000000000000000000001",
    },
    Types: map[string][]eip712.Field{
        "Order": {
            {Name: "maker", Type: "address"},
            {Name: "amount", Type: "uint256"},
            {Name: "note", Type: "string"},
        },
    },
    PrimaryType: "Order",
    Message: map[string]any{
        "maker":  "0xCD2a3d9F938E13CD947Ec05AbC7FE734Df8DD826",
        "amount": big.NewInt(1000),
        "note":   "hello",
    },
}

digest, err := td.Hash()        // 要签的 32 字节摘要
sig, err := td.Sign(privateKey) // 0x 开头的 65 字节签名，末尾补 27/28
addr, err := eip712.Recover(td, sig)
```

服务端验签就是从请求里把域、类型、消息原样重建出来，然后 `Recover` 一下，
拿还原出的地址和它声称的身份比。

## 几个必须知道的点

- **域里只写真正给了值的项**。域的类型声明是按「有哪些值」现场拼的，
  少一项多一项签出来完全不同——这是域的意义所在（把不同应用、不同链、不同合约隔开）。
- **字段顺序就是签名的内容**。改顺序等于换了一份数据，对方一定验不过。
- **被引用的结构体会按字母序追加在类型声明后面**，这是 EIP-712 的规定，不是可选的。
- **服务端和客户端必须用同一份 `Types` 和同样的值**。差异排查的第一招是把 `Hash()`
  的结果打出来对一下——摘要一样，后面就不可能错。
- 支持的字段类型：`uintN` / `intN` / `address` / `bool` / `bytes` / `bytesN` / `string`、
  它们的定长或动态数组、以及嵌套结构体。结构体要写进 `Types`。

## 验证过的东西

`eip712_test.go` 里钉了两个固定向量：

- EIP-712 规范里那个经典的 `Ether Mail` 示例，摘要是公开值；
- 一个覆盖数组、布尔、`bytes32`、`bytes`、中文的用例，摘要用 go-ethereum 的
  `signer/core/apitypes` 独立算过一遍，两边一致。

另外还覆盖了签名还原、改一个字节摘要就变、负数两补码、以及各种写错时的报错内容。
