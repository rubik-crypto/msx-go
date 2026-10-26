// Package evmkeystore 按 Web3 Secret Storage（V3）格式加解密 EVM 钱包私钥。
//
// 加密结果是一个标准 JSON：私钥用 scrypt 从密码派生的密钥做 AES-128-CTR 加密，
// 再用 Keccak-256 校验密码是否正确。MetaMask、geth 等主流钱包导出的就是这种文件，
// 文件里只有地址和密文，不保存明文私钥。
//
// 用法：
//
//	raw, err := evmkeystore.Encrypt(privateKey, "本机密码", evmkeystore.DefaultKDF)
//	// 把 raw 存成 .json 文件即可，权限建议 0600
//	key, err := evmkeystore.Decrypt(raw, "本机密码")
//	if errors.Is(err, evmkeystore.ErrWrongPassword) { ... }
//
// 读取文件里的地址不需要密码：evmkeystore.PeekAddress(raw)。
package evmkeystore

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"golang.org/x/crypto/scrypt"
)

// 加密格式里的固定取值。
const (
	cipherName   = "aes-128-ctr"
	kdfName      = "scrypt"
	keyLength    = 32
	ivLength     = aes.BlockSize
	macLength    = 32
	saltMinLen   = 8
	saltMaxLen   = 64
	maxN         = 1 << 20
	maxR         = 32
	maxP         = 64
	maxWork      = 1 << 21 // N×R 的上限，约 256MB 内存
	versionValue = 3
)

var (
	// ErrWrongPassword 表示密码不对，或者文件在写完之后被改过（两者都会被 MAC 挡住）。
	ErrWrongPassword = errors.New("密码不正确")
	// ErrInvalidFile 表示文件不是完整的 Web3 Secret Storage V3 钱包文件。
	ErrInvalidFile = errors.New("钱包文件损坏")
)

// KDFParams 是 scrypt 的三个参数。
// N 必须是 2 的幂；N 越大越难暴力猜密码，但解锁也越慢、占内存越多。
type KDFParams struct {
	N int
	R int
	P int
}

// DefaultKDF 是默认参数：约 32MB 内存，普通电脑解锁只要一两秒。
var DefaultKDF = KDFParams{N: 1 << 15, R: 8, P: 1}

type cipherParams struct {
	IV string `json:"iv"`
}

type kdfParams struct {
	DKLen int    `json:"dklen"`
	N     int    `json:"n"`
	R     int    `json:"r"`
	P     int    `json:"p"`
	Salt  string `json:"salt"`
}

type cryptoData struct {
	Cipher       string       `json:"cipher"`
	CipherParams cipherParams `json:"cipherparams"`
	CipherText   string       `json:"ciphertext"`
	KDF          string       `json:"kdf"`
	KDFParams    kdfParams    `json:"kdfparams"`
	MAC          string       `json:"mac"`
}

type fileV3 struct {
	Address string     `json:"address"`
	Crypto  cryptoData `json:"crypto"`
	ID      string     `json:"id"`
	Version int        `json:"version"`
}

// Encrypt 把私钥加密成 Web3 Secret Storage V3 JSON。
// kdf 传零值（KDFParams{}）时使用 DefaultKDF；密码不能为空。
func Encrypt(privateKey *ecdsa.PrivateKey, password string, kdf KDFParams) ([]byte, error) {
	if privateKey == nil {
		return nil, errors.New("私钥不能为空")
	}
	if strings.TrimSpace(password) == "" {
		return nil, errors.New("密码不能为空")
	}
	params, err := normalizeKDF(kdf)
	if err != nil {
		return nil, err
	}
	plain := crypto.FromECDSA(privateKey)
	if len(plain) != keyLength {
		return nil, errors.New("私钥长度不正确")
	}
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return nil, errors.New("随机数生成失败")
	}
	iv := make([]byte, ivLength)
	if _, err := rand.Read(iv); err != nil {
		return nil, errors.New("随机数生成失败")
	}
	derived, err := scrypt.Key([]byte(password), salt, params.N, params.R, params.P, keyLength)
	if err != nil {
		return nil, errors.New("密码加密失败")
	}
	block, err := aes.NewCipher(derived[:16])
	if err != nil {
		return nil, errors.New("密码加密失败")
	}
	ciphertext := make([]byte, len(plain))
	cipher.NewCTR(block, iv).XORKeyStream(ciphertext, plain)
	mac := crypto.Keccak256(append(append([]byte(nil), derived[16:32]...), ciphertext...))

	address := crypto.PubkeyToAddress(privateKey.PublicKey)
	file := fileV3{
		Address: strings.ToLower(strings.TrimPrefix(address.Hex(), "0x")),
		Crypto: cryptoData{
			Cipher:       cipherName,
			CipherParams: cipherParams{IV: hex.EncodeToString(iv)},
			CipherText:   hex.EncodeToString(ciphertext),
			KDF:          kdfName,
			KDFParams: kdfParams{
				DKLen: keyLength,
				N:     params.N,
				R:     params.R,
				P:     params.P,
				Salt:  hex.EncodeToString(salt),
			},
			MAC: hex.EncodeToString(mac),
		},
		ID:      newUUID(),
		Version: versionValue,
	}
	return json.Marshal(file)
}

// Decrypt 解开整个 V3 钱包文件，返回私钥。
// 密码不对或文件被改过时返回 ErrWrongPassword；文件结构不对时返回 ErrInvalidFile。
func Decrypt(data []byte, password string) (*ecdsa.PrivateKey, error) {
	address, c, err := parseFile(data)
	if err != nil {
		return nil, err
	}
	if c.KDFParams.DKLen != keyLength {
		return nil, invalid("密码派生参数不正确")
	}
	if err := checkKDF(c.KDFParams.N, c.KDFParams.R, c.KDFParams.P); err != nil {
		return nil, err
	}
	salt, err := hex.DecodeString(c.KDFParams.Salt)
	if err != nil || len(salt) < saltMinLen || len(salt) > saltMaxLen {
		return nil, invalid("盐值不合理")
	}
	iv, err := hex.DecodeString(c.CipherParams.IV)
	if err != nil || len(iv) != ivLength {
		return nil, invalid("随机数长度不对")
	}
	ciphertext, err := hex.DecodeString(c.CipherText)
	if err != nil || len(ciphertext) != keyLength {
		return nil, invalid("密文长度不对")
	}
	mac, err := hex.DecodeString(c.MAC)
	if err != nil || len(mac) != macLength {
		return nil, invalid("校验值长度不对")
	}
	derived, err := scrypt.Key([]byte(password), salt, c.KDFParams.N, c.KDFParams.R, c.KDFParams.P, keyLength)
	if err != nil {
		return nil, errors.New("密码校验失败")
	}
	expected := crypto.Keccak256(append(append([]byte(nil), derived[16:32]...), ciphertext...))
	if subtle.ConstantTimeCompare(mac, expected) != 1 {
		return nil, ErrWrongPassword
	}
	block, err := aes.NewCipher(derived[:16])
	if err != nil {
		return nil, errors.New("密码校验失败")
	}
	plain := make([]byte, len(ciphertext))
	cipher.NewCTR(block, iv).XORKeyStream(plain, ciphertext)
	key, err := crypto.ToECDSA(plain)
	if err != nil {
		return nil, invalid("私钥内容不正确")
	}
	if crypto.PubkeyToAddress(key.PublicKey) != address {
		return nil, invalid("文件和私钥对不上")
	}
	return key, nil
}

// PeekAddress 只读取文件里记录的地址，不需要密码，也不解密私钥。
func PeekAddress(data []byte) (common.Address, error) {
	address, _, err := parseFile(data)
	if err != nil {
		return common.Address{}, err
	}
	return address, nil
}

// parseFile 解析 V3 文件的公共部分，兼容有些工具写成大写 Crypto 的情况。
func parseFile(data []byte) (common.Address, cryptoData, error) {
	var head map[string]json.RawMessage
	if err := json.Unmarshal(data, &head); err != nil {
		return common.Address{}, cryptoData{}, invalid("不是有效的 JSON")
	}
	versionText := strings.Trim(strings.TrimSpace(string(head["version"])), `"`)
	if versionText != "3" {
		return common.Address{}, cryptoData{}, invalid("只支持 V3 版本")
	}
	var addressText string
	if raw, ok := head["address"]; ok {
		if err := json.Unmarshal(raw, &addressText); err != nil {
			return common.Address{}, cryptoData{}, invalid("地址不是字符串")
		}
	}
	if !common.IsHexAddress(addressText) {
		return common.Address{}, cryptoData{}, invalid("地址不正确")
	}
	rawCrypto, ok := head["crypto"]
	if !ok {
		rawCrypto = head["Crypto"]
	}
	if len(rawCrypto) == 0 {
		return common.Address{}, cryptoData{}, invalid("缺少加密段")
	}
	var c cryptoData
	if err := json.Unmarshal(rawCrypto, &c); err != nil {
		return common.Address{}, cryptoData{}, invalid("加密段格式不正确")
	}
	if !strings.EqualFold(strings.TrimSpace(c.Cipher), cipherName) {
		return common.Address{}, cryptoData{}, invalid("不支持的加密方式")
	}
	if !strings.EqualFold(strings.TrimSpace(c.KDF), kdfName) {
		return common.Address{}, cryptoData{}, invalid("不支持的密码算法")
	}
	return common.HexToAddress(addressText), c, nil
}

func normalizeKDF(kdf KDFParams) (KDFParams, error) {
	if kdf.N == 0 && kdf.R == 0 && kdf.P == 0 {
		return DefaultKDF, nil
	}
	if err := checkKDF(kdf.N, kdf.R, kdf.P); err != nil {
		return KDFParams{}, err
	}
	return kdf, nil
}

func checkKDF(n, r, p int) error {
	if n < 2 || n&(n-1) != 0 || n > maxN {
		return invalid("scrypt 参数 N 必须是 2 的幂且不能太大")
	}
	if r < 1 || r > maxR || p < 1 || p > maxP {
		return invalid("scrypt 参数 R 或 P 超出范围")
	}
	// 内存占用和 N×R 成正比。参数来自文件，不能让一个被改过的文件把内存吃光。
	if int64(n)*int64(r) > maxWork {
		return invalid("scrypt 参数太大，无法安全解开")
	}
	return nil
}

func invalid(message string) error {
	return fmt.Errorf("%w：%s", ErrInvalidFile, message)
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
