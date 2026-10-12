package chainkit

import (
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// transferTopic ERC20 Transfer(address,address,uint256) 事件签名哈希
var transferTopic = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))

// transferMethodID ERC20 transfer(address,uint256) 的方法选择器
var transferMethodID = crypto.Keccak256([]byte("transfer(address,uint256)"))[:4]

// balanceOfMethodID ERC20 balanceOf(address) 的方法选择器
var balanceOfMethodID = crypto.Keccak256([]byte("balanceOf(address)"))[:4]

// FormatUnits 把最小单位整数按 decimals 格式化为人类可读的十进制字符串（去掉尾随零）。
//
//	FormatUnits(1234500, 6) == "1.2345"
//	FormatUnits(big.NewInt(1e18), 18) == "1"
//
// decimals <= 0 时原样返回整数字符串。
func FormatUnits(v *big.Int, decimals int) string {
	if v == nil {
		return "0"
	}
	if decimals <= 0 {
		return v.String()
	}
	neg := v.Sign() < 0
	divisor := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	q, r := new(big.Int).QuoRem(new(big.Int).Abs(v), divisor, new(big.Int))
	if r.Sign() == 0 {
		if neg {
			return "-" + q.String()
		}
		return q.String()
	}
	frac := strings.TrimRight(fmt.Sprintf("%0*s", decimals, r.String()), "0")
	out := q.String() + "." + frac
	if neg {
		return "-" + out
	}
	return out
}

// ParseUnits 把人类可读的十进制金额按 decimals 换算成最小单位整数。
// 精度超出 decimals 位时返回错误（不做静默截断——金额截断是丢钱）。
func ParseUnits(s string, decimals int) (*big.Int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("金额为空")
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("无法解析金额 %q", s)
	}
	if decimals > 0 {
		scale := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil))
		r.Mul(r, scale)
	}
	if !r.IsInt() {
		return nil, fmt.Errorf("金额 %q 的小数位超过 %d 位", s, decimals)
	}
	return new(big.Int).Set(r.Num()), nil
}

// addrTopic 把地址左补零成 32 字节的日志 topic
func addrTopic(addr string) common.Hash {
	return common.BytesToHash(common.HexToAddress(addr).Bytes())
}

// topicToAddress 从 32 字节 topic 还原地址。
//
// 必须显式转小写：common.Address.Hex() 输出的是 EIP-55 校验和形式
// （0x8894E0a0...），而关注地址集合是按小写存的，直接比对会全部不匹配——
// 表现是代币充值一条都识别不出来，且没有任何报错。
func topicToAddress(t common.Hash) string {
	return strings.ToLower(common.BytesToAddress(t.Bytes()).Hex())
}

// erc20TransferData 拼 transfer(address,uint256) 的调用数据
func erc20TransferData(to string, amount *big.Int) []byte {
	data := make([]byte, 0, 4+32+32)
	data = append(data, transferMethodID...)
	data = append(data, common.LeftPadBytes(common.HexToAddress(to).Bytes(), 32)...)
	data = append(data, common.LeftPadBytes(amount.Bytes(), 32)...)
	return data
}

// erc20BalanceOfData 拼 balanceOf(address) 的调用数据
func erc20BalanceOfData(addr string) []byte {
	data := make([]byte, 0, 4+32)
	data = append(data, balanceOfMethodID...)
	data = append(data, common.LeftPadBytes(common.HexToAddress(addr).Bytes(), 32)...)
	return data
}
