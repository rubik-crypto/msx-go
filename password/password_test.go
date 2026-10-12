package password

import (
	"strings"
	"testing"
)

func TestHashPassword(t *testing.T) {
	const pw = "testpassword123"
	hash, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword 失败: %v", err)
	}
	if hash == "" {
		t.Fatal("HashPassword 返回空串")
	}
	ok, err := VerifyPassword(pw, hash)
	if err != nil {
		t.Fatalf("VerifyPassword 失败: %v", err)
	}
	if !ok {
		t.Fatal("正确密码没通过")
	}
	ok, err = VerifyPassword("wrongpassword", hash)
	if err != nil {
		t.Fatalf("错误密码不该报错: %v", err)
	}
	if ok {
		t.Fatal("错误密码却通过了")
	}
}

func TestHashPasswordWithAlgorithm(t *testing.T) {
	const pw = "testpassword123"
	for _, alg := range []string{AlgorithmArgon2id, AlgorithmBcrypt} {
		hash, err := HashPasswordWithAlgorithm(pw, alg)
		if err != nil {
			t.Fatalf("HashPasswordWithAlgorithm(%s) 失败: %v", alg, err)
		}
		ok, err := VerifyPassword(pw, hash)
		if err != nil || !ok {
			t.Fatalf("%s 的哈希没通过校验: ok=%v err=%v", alg, ok, err)
		}
	}
}

func TestBcryptBackwardCompatibility(t *testing.T) {
	// 裸 bcrypt 串（不是结构化 JSON）也要验得过：老库里的密码就是这个形态。
	const pw = "testpassword"
	hash, err := HashPasswordWithAlgorithm(pw, AlgorithmBcrypt)
	if err != nil {
		t.Fatalf("生成 bcrypt 哈希失败: %v", err)
	}
	ok, err := VerifyPassword(pw, hash)
	if err != nil || !ok {
		t.Fatalf("bcrypt 哈希没通过: ok=%v err=%v", ok, err)
	}
	ok, err = VerifyPassword("wrongpassword", hash)
	if err != nil || ok {
		t.Fatalf("错误密码在 bcrypt 上当通过了: ok=%v err=%v", ok, err)
	}
}

func TestStructuredArgon2idEncoding(t *testing.T) {
	const pw = "testpassword123"
	hash, err := HashPasswordWithAlgorithm(pw, AlgorithmArgon2id)
	if err != nil {
		t.Fatalf("生成 Argon2id 哈希失败: %v", err)
	}
	hp, err := decodeHashedPassword(hash)
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if hp.Algorithm != AlgorithmArgon2id || hp.Hash == "" || hp.Salt == "" {
		t.Fatalf("结构化哈希字段不齐: %+v", hp)
	}
	if hp.Iterations == 0 || hp.Memory == 0 || hp.Parallelism == 0 {
		t.Fatalf("Argon2id 参数没写进去: %+v", hp)
	}
}

func TestHashPasswordRejectsUnknownAlgorithm(t *testing.T) {
	_, err := HashPasswordWithAlgorithm("x", "invalid-algorithm")
	if err == nil {
		t.Fatal("不认识的算法应当报错")
	}
	if !strings.Contains(err.Error(), "不支持的密码哈希算法") {
		t.Fatalf("错误信息不对: %v", err)
	}
}

func TestVerifyRejectsCorruptHash(t *testing.T) {
	// 结构化 JSON 但缺字段：应当报错，而不是当成 bcrypt 静默返回 false。
	if _, err := VerifyPassword("x", `{"algorithm":"argon2id","hash":"abc"}`); err == nil {
		t.Fatal("缺 salt 的 Argon2id 哈希应当报错")
	}
}
