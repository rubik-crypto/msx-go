package evmabi

import (
	"encoding/binary"
	"hash"
	"math/bits"
)

// Keccak-256（以太坊里说的 "sha3"，注意不是 FIPS-202 的 SHA3-256：
// 两者只在填充字节上不同，以太坊用的是原版 Keccak 的 0x01 填充）。
//
// 自己实现的原因：Go 标准库到 1.24 才带 crypto/sha3，为了不抬高项目的最低
// Go 版本，这里用标准库的位运算自己算一遍，并用公开的测试向量钉住。

const keccakRate = 136 // 1088 位，Keccak-256 的 rate

// roundConstants 是 Keccak-f[1600] 的 24 轮常数。
var roundConstants = [24]uint64{
	0x0000000000000001, 0x0000000000008082, 0x800000000000808a, 0x8000000080008000,
	0x000000000000808b, 0x0000000080000001, 0x8000000080008081, 0x8000000000008009,
	0x000000000000008a, 0x0000000000000088, 0x0000000080008009, 0x000000008000000a,
	0x000000008000808b, 0x800000000000008b, 0x8000000000008089, 0x8000000000008003,
	0x8000000000008002, 0x8000000000000080, 0x000000000000800a, 0x800000008000000a,
	0x8000000080008081, 0x8000000000008080, 0x0000000080000001, 0x8000000080008008,
}

// rotationOffsets[x][y] 是 rho 步骤里每个 lane 的循环左移位数。
// 按 Keccak 规范里那条「从 (1,0) 出发沿 (x,y)→(y,2x+3y) 走」的规则算出来，
// 不手抄表格，免得抄错一位还看不出来（_test.go 里有对照用例）。
var rotationOffsets = func() [5][5]uint {
	var r [5][5]uint
	x, y := 1, 0
	for t := 0; t < 24; t++ {
		r[x][y] = uint(((t + 1) * (t + 2) / 2) % 64)
		x, y = y, (2*x+3*y)%5
	}
	return r
}()

// keccakF1600 是 Keccak 的置换函数，state 以 lane 下标 x+5y 展平存放。
func keccakF1600(a *[25]uint64) {
	var c, d [5]uint64
	var b [25]uint64
	for round := 0; round < 24; round++ {
		// theta
		for x := 0; x < 5; x++ {
			c[x] = a[x] ^ a[x+5] ^ a[x+10] ^ a[x+15] ^ a[x+20]
		}
		for x := 0; x < 5; x++ {
			d[x] = c[(x+4)%5] ^ bits.RotateLeft64(c[(x+1)%5], 1)
		}
		for x := 0; x < 5; x++ {
			for y := 0; y < 5; y++ {
				a[x+5*y] ^= d[x]
			}
		}
		// rho + pi：B[y][2x+3y] = rot(A[x][y], r[x][y])
		for x := 0; x < 5; x++ {
			for y := 0; y < 5; y++ {
				b[y+5*((2*x+3*y)%5)] = bits.RotateLeft64(a[x+5*y], int(rotationOffsets[x][y]))
			}
		}
		// chi
		for x := 0; x < 5; x++ {
			for y := 0; y < 5; y++ {
				a[x+5*y] = b[x+5*y] ^ (^b[(x+1)%5+5*y] & b[(x+2)%5+5*y])
			}
		}
		// iota
		a[0] ^= roundConstants[round]
	}
}

func keccakAbsorb(a *[25]uint64, block []byte) {
	for i := 0; i < keccakRate/8; i++ {
		a[i] ^= binary.LittleEndian.Uint64(block[i*8:])
	}
	keccakF1600(a)
}

// Keccak256 计算若干段字节拼接后的 Keccak-256 摘要（32 字节）。
func Keccak256(parts ...[]byte) []byte {
	var a [25]uint64
	var buf [keccakRate]byte
	n := 0
	for _, p := range parts {
		for len(p) > 0 {
			k := copy(buf[n:], p)
			n += k
			p = p[k:]
			if n == keccakRate {
				keccakAbsorb(&a, buf[:])
				n = 0
			}
		}
	}
	for i := n; i < keccakRate; i++ {
		buf[i] = 0
	}
	buf[n] = 0x01             // 原版 Keccak 的填充（SHA3-256 这里是 0x06）
	buf[keccakRate-1] |= 0x80 // 末位补齐
	keccakAbsorb(&a, buf[:])

	out := make([]byte, 32)
	for i := 0; i < 4; i++ {
		binary.LittleEndian.PutUint64(out[i*8:], a[i])
	}
	return out
}

// NewKeccak256 返回一个可增量写入的 Keccak-256 实现（满足 hash.Hash）。
func NewKeccak256() hash.Hash { return &keccakDigest{} }

type keccakDigest struct {
	a   [25]uint64
	buf [keccakRate]byte
	n   int
}

func (d *keccakDigest) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		k := copy(d.buf[d.n:], p)
		d.n += k
		p = p[k:]
		if d.n == keccakRate {
			keccakAbsorb(&d.a, d.buf[:])
			d.n = 0
		}
	}
	return total, nil
}

func (d *keccakDigest) Sum(b []byte) []byte {
	// 复制一份状态，保证可以继续 Write
	cp := *d
	cp.buf[cp.n] = 0x01
	for i := cp.n + 1; i < keccakRate; i++ {
		cp.buf[i] = 0
	}
	cp.buf[keccakRate-1] |= 0x80
	keccakAbsorb(&cp.a, cp.buf[:])
	var out [32]byte
	for i := 0; i < 4; i++ {
		binary.LittleEndian.PutUint64(out[i*8:], cp.a[i])
	}
	return append(b, out[:]...)
}

func (d *keccakDigest) Reset()         { *d = keccakDigest{} }
func (d *keccakDigest) Size() int      { return 32 }
func (d *keccakDigest) BlockSize() int { return keccakRate }

// Selector 由函数签名字符串算出 4 字节选择器，例如
// Selector("transfer(address,uint256)") == 0xa9059cbb。
// 签名必须写规范的写法：类型之间不要空格，元组用圆括号包起来。
func Selector(signature string) []byte {
	return Keccak256([]byte(signature))[:4]
}
