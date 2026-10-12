// Package password 密码哈希与校验：默认 Argon2id，也支持 bcrypt，校验时自动识别算法。
//
// 哈希结果自带算法与参数，直接当一段字符串存库即可；将来调参数，旧哈希照样校验得通。
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/bcrypt"
)

// 算法标识。
const (
	AlgorithmBcrypt   = "bcrypt"
	AlgorithmArgon2id = "argon2id"
)

// Argon2id 默认参数。
const (
	argon2idMemory      = 64 * 1024 // 64MB
	argon2idIterations  = 3
	argon2idParallelism = 4
	argon2idSaltLength  = 16
	argon2idKeyLength   = 32
)

// HashedPassword 密码哈希的结构化表示。
type HashedPassword struct {
	Algorithm   string `json:"algorithm"`
	Hash        string `json:"hash"`
	Salt        string `json:"salt,omitempty"` // Argon2id 需要盐
	Iterations  uint32 `json:"iterations,omitempty"`
	Memory      uint32 `json:"memory,omitempty"`
	Parallelism uint8  `json:"parallelism,omitempty"`
}

// HashPassword 用默认算法（Argon2id）哈希密码，返回可直接入库的字符串。
func HashPassword(password string) (string, error) {
	return HashPasswordWithAlgorithm(password, AlgorithmArgon2id)
}

// HashPasswordWithAlgorithm 用指定算法哈希密码。
func HashPasswordWithAlgorithm(password, algorithm string) (string, error) {
	switch algorithm {
	case AlgorithmBcrypt:
		return hashBcrypt(password)
	case AlgorithmArgon2id:
		return hashArgon2id(password)
	default:
		return "", fmt.Errorf("不支持的密码哈希算法: %s", algorithm)
	}
}

// VerifyPassword 校验明文密码是否匹配哈希，自动识别算法。
// 返回的第一个值表示是否匹配；只有哈希本身损坏（解析不了、参数非法）时才有 error。
func VerifyPassword(password, hashedPassword string) (bool, error) {
	// 能解析成结构化哈希就按结构化的验证，否则当成裸 bcrypt 串
	if hp, err := decodeHashedPassword(hashedPassword); err == nil {
		return verifyStructuredPassword(password, hp)
	}
	return verifyBcrypt(password, hashedPassword)
}

func hashBcrypt(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("bcrypt 哈希失败: %w", err)
	}
	return string(hash), nil
}

func hashArgon2id(password string) (string, error) {
	salt := make([]byte, argon2idSaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("生成随机盐失败: %w", err)
	}
	hash := argon2.IDKey([]byte(password), salt, argon2idIterations, argon2idMemory, argon2idParallelism, argon2idKeyLength)
	hp := &HashedPassword{
		Algorithm:   AlgorithmArgon2id,
		Hash:        base64.RawStdEncoding.EncodeToString(hash),
		Salt:        base64.RawStdEncoding.EncodeToString(salt),
		Iterations:  argon2idIterations,
		Memory:      argon2idMemory,
		Parallelism: argon2idParallelism,
	}
	return encodeHashedPassword(hp), nil
}

func verifyBcrypt(password, hash string) (bool, error) {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	if err != nil {
		if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
			return false, nil
		}
		return false, fmt.Errorf("bcrypt 校验失败: %w", err)
	}
	return true, nil
}

func verifyStructuredPassword(password string, hp *HashedPassword) (bool, error) {
	switch hp.Algorithm {
	case AlgorithmBcrypt:
		return verifyBcrypt(password, hp.Hash)
	case AlgorithmArgon2id:
		return verifyArgon2id(password, hp)
	default:
		return false, fmt.Errorf("不支持的密码哈希算法: %s", hp.Algorithm)
	}
}

func verifyArgon2id(password string, hp *HashedPassword) (bool, error) {
	salt, err := base64.RawStdEncoding.DecodeString(hp.Salt)
	if err != nil {
		return false, fmt.Errorf("解码盐失败: %w", err)
	}
	hash, err := base64.RawStdEncoding.DecodeString(hp.Hash)
	if err != nil {
		return false, fmt.Errorf("解码哈希失败: %w", err)
	}
	other := argon2.IDKey([]byte(password), salt, hp.Iterations, hp.Memory, hp.Parallelism, uint32(len(hash)))
	return subtle.ConstantTimeCompare(hash, other) == 1, nil
}

// decodeHashedPassword 把存储里的字符串解成结构体。解析不了（比如裸 bcrypt 串）返回错误，
// 由调用方回落去当 bcrypt 验。
func decodeHashedPassword(s string) (*HashedPassword, error) {
	var hp HashedPassword
	if err := json.Unmarshal([]byte(s), &hp); err != nil {
		return nil, fmt.Errorf("不是结构化的哈希: %w", err)
	}
	if hp.Algorithm == "" || hp.Hash == "" {
		return nil, errors.New("结构化的哈希缺 algorithm 或 hash")
	}
	if hp.Algorithm == AlgorithmArgon2id && hp.Salt == "" {
		return nil, errors.New("Argon2id 的哈希缺 salt")
	}
	return &hp, nil
}

func encodeHashedPassword(hp *HashedPassword) string {
	data, err := json.Marshal(hp)
	if err != nil {
		// 理论上不会发生；真发生也得留下能用的东西，不能返回空串把密码弄丢。
		return fmt.Sprintf(`{"algorithm":%q,"hash":%q}`, hp.Algorithm, hp.Hash)
	}
	return string(data)
}
