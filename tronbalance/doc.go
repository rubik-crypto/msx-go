// Package tronbalance 查询波场地址的 TRX 与 TRC20 代币余额，并读取代币符号和小数位。
//
// 它只依赖标准库，直接调用 TronGrid 兼容的 HTTP API，不保存私钥、不构造或广播交易。
// 可以传入多个节点地址，查询失败时按顺序自动换用下一个。
//
// 典型用法：
//
//	client := tronbalance.New()
//	trx, err := client.NativeBalance(ctx, "T...")
//	info, err := client.TRC20Info(ctx, "T...", "T...")
//	usdt, err := client.TRC20Balance(ctx, "T...", "T...")
package tronbalance
