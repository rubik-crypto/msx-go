package evmkeystore

import (
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

// 公开的测试私钥，只用于单元测试，里面没有真实资产。
const testPrivateKey = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
const testAddress = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"

func mustKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := crypto.HexToECDSA(testPrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestRoundTrip(t *testing.T) {
	key := mustKey(t)
	raw, err := Encrypt(key, "password123", KDFParams{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(raw)), testPrivateKey) {
		t.Fatal("加密结果里不能出现明文私钥")
	}
	var stored fileV3
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Version != 3 || stored.Crypto.Cipher != "aes-128-ctr" || stored.Crypto.KDF != "scrypt" {
		t.Fatalf("不是标准 V3 格式: %s", raw)
	}
	if stored.Address != strings.ToLower(strings.TrimPrefix(testAddress, "0x")) {
		t.Fatalf("地址格式不对: %s", stored.Address)
	}
	if stored.ID == "" || stored.Crypto.MAC == "" || stored.Crypto.KDFParams.Salt == "" {
		t.Fatalf("缺少必要字段: %s", raw)
	}
	got, err := Decrypt(raw, "password123")
	if err != nil {
		t.Fatal(err)
	}
	if crypto.PubkeyToAddress(got.PublicKey).Hex() != testAddress {
		t.Fatalf("解出来的地址不对: %s", crypto.PubkeyToAddress(got.PublicKey).Hex())
	}
	addr, err := PeekAddress(raw)
	if err != nil {
		t.Fatal(err)
	}
	if addr.Hex() != testAddress {
		t.Fatalf("PeekAddress 返回的地址不对: %s", addr.Hex())
	}
}

func TestWrongPassword(t *testing.T) {
	raw, err := Encrypt(mustKey(t), "password123", KDFParams{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decrypt(raw, "wrong-password"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("错误密码应该返回 ErrWrongPassword，得到 %v", err)
	}
}

func TestTamperedCiphertext(t *testing.T) {
	raw, err := Encrypt(mustKey(t), "password123", KDFParams{})
	if err != nil {
		t.Fatal(err)
	}
	var stored fileV3
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	// 改一个字节的密文，MAC 必须对不上。
	first := stored.Crypto.CipherText[:2]
	replacement := "00"
	if first == "00" {
		replacement = "01"
	}
	stored.Crypto.CipherText = replacement + stored.Crypto.CipherText[2:]
	tampered, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decrypt(tampered, "password123"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("文件被改过应该解不开，得到 %v", err)
	}
}

func TestInvalidFiles(t *testing.T) {
	cases := []string{
		``,
		`不是 JSON`,
		`{"address":"0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266","crypto":{},"version":1}`,
		`{"address":"0x123","crypto":{},"version":3}`,
		`{"address":"0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266","crypto":{"cipher":"aes-256-gcm","kdf":"scrypt"},"version":3}`,
		`{"address":"0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266","crypto":{"cipher":"aes-128-ctr","kdf":"pbkdf2"},"version":3}`,
	}
	for _, raw := range cases {
		if _, err := Decrypt([]byte(raw), "password123"); !errors.Is(err, ErrInvalidFile) {
			t.Fatalf("应该返回 ErrInvalidFile，输入 %q 得到 %v", raw, err)
		}
	}
}

func TestUppercaseCryptoField(t *testing.T) {
	raw, err := Encrypt(mustKey(t), "password123", KDFParams{})
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(raw), `"crypto":`, `"Crypto":`, 1)
	if _, err := Decrypt([]byte(text), "password123"); err != nil {
		t.Fatalf("大写 Crypto 的文件也应该能解开: %v", err)
	}
}

func TestCustomKDF(t *testing.T) {
	raw, err := Encrypt(mustKey(t), "password123", KDFParams{N: 4096, R: 8, P: 6})
	if err != nil {
		t.Fatal(err)
	}
	var stored fileV3
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Crypto.KDFParams.N != 4096 || stored.Crypto.KDFParams.P != 6 {
		t.Fatalf("参数没有写进文件: %+v", stored.Crypto.KDFParams)
	}
	if _, err := Decrypt(raw, "password123"); err != nil {
		t.Fatal(err)
	}
}

func TestEncryptRejectsBadInput(t *testing.T) {
	if _, err := Encrypt(nil, "password123", KDFParams{}); err == nil {
		t.Fatal("空私钥应该被拒绝")
	}
	if _, err := Encrypt(mustKey(t), "   ", KDFParams{}); err == nil {
		t.Fatal("空密码应该被拒绝")
	}
	if _, err := Encrypt(mustKey(t), "password123", KDFParams{N: 3000, R: 8, P: 1}); err == nil {
		t.Fatal("不是 2 的幂的 N 应该被拒绝")
	}
}

func TestDecryptGethVector(t *testing.T) {
	raw, err := os.ReadFile("testdata/geth_v3.json")
	if err != nil {
		t.Fatal(err)
	}
	key, err := Decrypt(raw, "password123")
	if err != nil {
		t.Fatalf("应该能解开 geth 生成的 V3 文件: %v", err)
	}
	if crypto.PubkeyToAddress(key.PublicKey).Hex() != testAddress {
		t.Fatal("解出来的地址不对")
	}
	addr, err := PeekAddress(raw)
	if err != nil || addr.Hex() != testAddress {
		t.Fatalf("PeekAddress 结果不对: %s %v", addr.Hex(), err)
	}
	if _, err := Decrypt(raw, "wrong-password"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("错误密码应该返回 ErrWrongPassword，得到 %v", err)
	}
}
