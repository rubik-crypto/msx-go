package evmabi

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// 这些都是公开可查的 Keccak-256 测试向量：只要实现是对的，它们必须一字不差。
func TestKeccak256KnownVectors(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470"},
		{"abc", "4e03657aea45a94fc7d47ba826c8d667c0d1e6e33a64a036ec44f58fa12d6c45"},
		{"hello", "1c8aff950685c2ed4bc3174f3472287b56d9517b9c948127319a09a7a36deac8"},
	}
	for _, c := range cases {
		got := hex.EncodeToString(Keccak256([]byte(c.in)))
		if got != c.want {
			t.Errorf("Keccak256(%q) = %s, 期望 %s", c.in, got, c.want)
		}
	}
}

// 分块喂数据（每块长度都不同）必须和一次性算出来的一样，
// 包括正好落在 rate 边界上的那几种长度——链上数据是按块吸的。
func TestKeccakChunkingIsStable(t *testing.T) {
	base := make([]byte, 300)
	for i := range base {
		base[i] = byte(i * 7 % 251)
	}
	for _, size := range []int{0, 1, 63, 135, 136, 137, 271, 272, 273, 300} {
		data := base[:size]
		want := Keccak256(data)
		for _, chunk := range []int{1, 7, 64, 136, 137} {
			var parts [][]byte
			for off := 0; off < len(data); off += chunk {
				end := off + chunk
				if end > len(data) {
					end = len(data)
				}
				parts = append(parts, data[off:end])
			}
			got := Keccak256(parts...)
			if !bytes.Equal(got, want) {
				t.Fatalf("长度 %d 按 %d 字节分块的结果和一次性结果不一致", size, chunk)
			}
		}
	}
}

// 选择器是最容易抄错的东西，这里用几个众所周知的值钉住。
func TestSelectorKnownValues(t *testing.T) {
	cases := map[string]string{
		"transfer(address,uint256)":             "a9059cbb",
		"approve(address,uint256)":              "095ea7b3",
		"transferFrom(address,address,uint256)": "23b872dd",
		"balanceOf(address)":                    "70a08231",
		"allowance(address,address)":            "dd62ed3e",
		"decimals()":                            "313ce567",
		"symbol()":                              "95d89b41",
		"name()":                                "06fdde03",
		"getAmountsOut(uint256,address[])":      "d06ca61f",
		// Uniswap V2 Router02
		"swapExactTokensForTokens(uint256,uint256,address[],address,uint256)": "38ed1739",
		"swapExactETHForTokens(uint256,address[],address,uint256)":            "7ff36ab5",
		"swapExactTokensForETH(uint256,uint256,address[],address,uint256)":    "18cbafe5",
		// Uniswap V3 QuoterV2
		"quoteExactInputSingle((address,address,uint256,uint24,uint160))": "c6a5026a",
		"quoteExactInput(bytes,uint256)":                                  "cdca1753",
		// Uniswap V3 SwapRouter（V1 世代，参数里带 deadline）
		"exactInputSingle((address,address,uint24,address,uint256,uint256,uint256,uint160))": "414bf389",
		"exactInput((bytes,address,uint256,uint256,uint256))":                                "c04b8d59",
		// Uniswap V3 SwapRouter02（参数里没有 deadline）
		"exactInputSingle((address,address,uint24,address,uint256,uint256,uint160))": "04e45aaf",
		"exactInput((bytes,address,uint256,uint256))":                                "b858183f",
		// SwapRouter 的辅助函数
		"multicall(uint256,bytes[])":   "5ae401dc",
		"unwrapWETH9(uint256,address)": "49404b7c",
		"refundETH()":                  "12210e8a",
		"factory()":                    "c45a0155",
	}
	for sig, want := range cases {
		got := hex.EncodeToString(Selector(sig))
		if got != want {
			t.Errorf("Selector(%q) = %s, 期望 %s", sig, got, want)
		}
	}
}

// rho 的移位数表必须是标准里那张表，生成逻辑写错了这里会立刻发现。
func TestRotationOffsetsMatchSpec(t *testing.T) {
	want := [5][5]uint{
		{0, 36, 3, 41, 18},
		{1, 44, 10, 45, 2},
		{62, 6, 43, 15, 61},
		{28, 55, 25, 21, 56},
		{27, 20, 39, 8, 14},
	}
	if rotationOffsets != want {
		t.Errorf("rotationOffsets = %v, 期望 %v", rotationOffsets, want)
	}
}

// 增量写入与一次性写入必须得到一样的结果（喂数据时是分块的）。
func TestKeccakIncrementalMatchesOneShot(t *testing.T) {
	parts := [][]byte{[]byte("0123456789"), []byte("abcdefghij"), []byte("XYZ")}
	oneShot := Keccak256(parts...)
	h := NewKeccak256()
	for _, p := range parts {
		if _, err := h.Write(p); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(h.Sum(nil), oneShot) {
		t.Errorf("增量结果 %x 与一次性结果 %x 不一致", h.Sum(nil), oneShot)
	}
	// Sum 之后还能继续写（Go 的 hash.Hash 约定：Sum 不改状态）
	h.Write([]byte("!"))
	if bytes.Equal(h.Sum(nil), oneShot) {
		t.Error("继续写入后摘要应当变化")
	}
}
