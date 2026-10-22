package models

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// ============================================================
// accounts.json 敏感数据落盘加密（AES-256-GCM）
//
// 密钥来源（按优先级）：
//  1. 环境变量 QQ_FARM_ENC_KEY（32 字节，hex 或 base64 编码）——生产推荐
//  2. 数据目录 .enckey 文件（首次运行随机生成，权限 0600）
//
// 加密后 accounts.json 只保存信封：
//   { "v": 1, "encrypted": "<base64(nonce|ciphertext)>" }
// 明文 Code / apiToken / openId / uin / uin 不再落盘。
// 旧版明文 accounts.json 在首次读取时自动迁移为加密格式。
// ============================================================

// ErrNotEncrypted 表示文件为旧版明文结构（非加密信封），调用方应走明文兼容路径。
var ErrNotEncrypted = errors.New("models: not an encrypted envelope")

const encKeyEnv = "QQ_FARM_ENC_KEY"

var encKey []byte

// loadEncryptionKey 必须在 InitStore 中、loadAccounts 之前调用。
// 解析顺序：环境变量 → 已有 .enckey → 随机生成并写入（权限 0600）。
func loadEncryptionKey() {
	if k := os.Getenv(encKeyEnv); k != "" {
		key, err := decodeKey(k)
		if err != nil {
			log.Fatalf("[crypto] %s 无效（需 32 字节，hex 或 base64 编码）：%v", encKeyEnv, err)
		}
		encKey = key
		log.Printf("[crypto] 使用环境变量 %s 作为 accounts.json 加密密钥", encKeyEnv)
		return
	}
	keyPath := filepath.Join(dataDir, ".enckey")
	if b, err := os.ReadFile(keyPath); err == nil {
		if len(b) == 32 {
			encKey = b
			log.Printf("[crypto] 使用数据目录 .enckey 作为 accounts.json 加密密钥（请务必备份此文件）")
			return
		}
		log.Printf("[crypto] 警告: .enckey 长度异常（%d 字节），将重新生成", len(b))
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		log.Fatalf("[crypto] 生成加密密钥失败: %v", err)
	}
	if err := os.WriteFile(keyPath, key, 0600); err != nil {
		log.Fatalf("[crypto] 写入 .enckey 失败: %v", err)
	}
	encKey = key
	log.Printf("[crypto] 已随机生成 accounts.json 加密密钥并写入 %s（权限 0600；丢失将无法解密，请备份）", keyPath)
}

// decodeKey 解析 32 字节密钥：优先 hex(64)，其次 base64/std 或 base64/urlsafe。
func decodeKey(s string) ([]byte, error) {
	if len(s) == 64 {
		if b, err := hex.DecodeString(s); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil && len(b) == 32 {
		return b, nil
	}
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil && len(b) == 32 {
		return b, nil
	}
	return nil, fmt.Errorf("密钥长度/编码无效，需 32 字节")
}

// encryptJSON 将 v 序列化后用 AES-256-GCM 加密，输出加密信封 JSON。
func encryptJSON(v interface{}) ([]byte, error) {
	if len(encKey) != 32 {
		return nil, errors.New("加密密钥未初始化")
	}
	plaintext, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	sealed := gcm.Seal(nil, nonce, plaintext, nil)
	envelope := map[string]interface{}{
		"v":         1,
		"encrypted": base64.StdEncoding.EncodeToString(append(nonce, sealed...)),
	}
	return json.Marshal(envelope)
}

// decryptJSON 解析加密信封并解密到 out。文件为旧版明文时返回 ErrNotEncrypted。
func decryptJSON(data []byte, out interface{}) error {
	if len(encKey) != 32 {
		return errors.New("加密密钥未初始化")
	}
	var env struct {
		V         int    `json:"v"`
		Encrypted string `json:"encrypted"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return err
	}
	if env.Encrypted == "" {
		return ErrNotEncrypted
	}
	raw, err := base64.StdEncoding.DecodeString(env.Encrypted)
	if err != nil {
		return fmt.Errorf("base64 解码失败: %w", err)
	}
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	ns := gcm.NonceSize()
	if len(raw) < ns {
		return errors.New("密文过短")
	}
	nonce, ct := raw[:ns], raw[ns:]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return fmt.Errorf("解密失败（密钥不匹配或数据损坏）: %w", err)
	}
	return json.Unmarshal(pt, out)
}

// writeEncFileAtomic 原子写入加密后的 accounts.json（权限 0600，仅运行用户可读）。
func writeEncFileAtomic(filePath string, data []byte) error {
	tmpPath := filePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmpPath, filePath)
}
