package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// 与 worker.js 的 deriveKey / sealRecord / openRecord 完全对齐：
//
//	key = HKDF-SHA256(secret=MASTER_KEY, salt=每条记录随机 16B, info=<平台标签>, L=32)
//	box = AES-256-GCM(key, iv=12B) → JSON {"v":1,"s":salt,"i":iv,"d":ct||tag}
//
// 这样 Cloudflare KV 里已有的记录可以直接搬过来解密。
//
// 这两个常量是与上游（CF 版）互通的基准值。新平台按 `<key>-signin/v1/account`
// 派生同样的格式，platform_test.go 里有断言保证派生结果与这里一致。
const (
	infoTrae = "trae-signin/v1/account"
	infoWB   = "wb-signin/v1/account"
)

// hkdfSHA256 手写实现（RFC 5869），避免为一个函数引入外部依赖。
func hkdfSHA256(secret, salt, info []byte, length int) []byte {
	extract := hmac.New(sha256.New, salt)
	extract.Write(secret)
	prk := extract.Sum(nil)

	out := make([]byte, 0, length+sha256.Size)
	var prev []byte
	for counter := byte(1); len(out) < length; counter++ {
		h := hmac.New(sha256.New, prk)
		h.Write(prev)
		h.Write(info)
		h.Write([]byte{counter})
		prev = h.Sum(nil)
		out = append(out, prev...)
	}
	return out[:length]
}

type sealedBox struct {
	V int    `json:"v"`
	S string `json:"s"`
	I string `json:"i"`
	D string `json:"d"`
}

func deriveAEAD(master, salt []byte, info string) (cipher.AEAD, error) {
	key := hkdfSHA256(master, salt, []byte(info), 32)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand 不可用: " + err.Error())
	}
	return b
}

func sealRecord(master []byte, info string, v any) (string, error) {
	salt := randomBytes(16)
	iv := randomBytes(12)
	aead, err := deriveAEAD(master, salt, info)
	if err != nil {
		return "", err
	}
	plain, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	ct := aead.Seal(nil, iv, plain, nil)
	box := sealedBox{
		V: 1,
		S: base64.StdEncoding.EncodeToString(salt),
		I: base64.StdEncoding.EncodeToString(iv),
		D: base64.StdEncoding.EncodeToString(ct),
	}
	b, err := json.Marshal(box)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func openRecord(master []byte, info string, text string, out any) error {
	var box sealedBox
	if err := json.Unmarshal([]byte(text), &box); err != nil {
		return fmt.Errorf("记录格式异常: %w", err)
	}
	if box.V != 1 {
		return errors.New("不认识的记录版本")
	}
	salt, err := base64.StdEncoding.DecodeString(box.S)
	if err != nil {
		return errors.New("记录 salt 非法")
	}
	iv, err := base64.StdEncoding.DecodeString(box.I)
	if err != nil {
		return errors.New("记录 iv 非法")
	}
	data, err := base64.StdEncoding.DecodeString(box.D)
	if err != nil {
		return errors.New("记录密文非法")
	}
	aead, err := deriveAEAD(master, salt, info)
	if err != nil {
		return err
	}
	plain, err := aead.Open(nil, iv, data, nil)
	if err != nil {
		return errors.New("解密失败：MASTER_KEY 与数据不匹配")
	}
	return json.Unmarshal(plain, out)
}

/* ------------------------------------------------------------------ */
/* 主密钥：环境变量优先，否则首启生成并落盘到数据卷                      */
/* ------------------------------------------------------------------ */

func loadMasterKey(dataDir, fromEnv string) ([]byte, error) {
	if v := strings.TrimSpace(fromEnv); v != "" {
		raw, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, errors.New("MASTER_KEY 不是合法 base64")
		}
		if len(raw) != 32 {
			return nil, fmt.Errorf("MASTER_KEY 必须是 32 字节（base64 后 44 字符），当前 %d 字节", len(raw))
		}
		return raw, nil
	}

	path := filepath.Join(dataDir, "master.key")
	if b, err := os.ReadFile(path); err == nil {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(raw) != 32 {
			return nil, fmt.Errorf("%s 内容非法，请删除后重启（会重新生成，旧账号需重新扫码）", path)
		}
		return raw, nil
	}

	raw := randomBytes(32)
	enc := base64.StdEncoding.EncodeToString(raw)
	if err := os.WriteFile(path, []byte(enc+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("写入 %s 失败: %w", path, err)
	}
	logf("已生成新的 MASTER_KEY 并保存到 %s（丢失后只能重新扫码）", path)
	return raw, nil
}

func loadAdminToken(dataDir, fromEnv string) (string, bool, error) {
	if v := strings.TrimSpace(fromEnv); v != "" {
		return v, false, nil
	}
	path := filepath.Join(dataDir, "admin_token")
	if b, err := os.ReadFile(path); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s, false, nil
		}
	}
	token := randToken(9)
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", false, fmt.Errorf("写入 %s 失败: %w", path, err)
	}
	return token, true, nil
}
