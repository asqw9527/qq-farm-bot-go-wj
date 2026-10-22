package models

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	encKey = make([]byte, 32)
	if _, err := rand.Read(encKey); err != nil {
		t.Fatal(err)
	}
	in := struct {
		Accounts []Account `json:"accounts"`
	}{Accounts: []Account{{
		ID: "1", Name: "小号", Code: "SECRET_CODE_123", OpenID: "OPENID_X",
		UIN: "10001", Thirdparty: &ThirdpartyConfig{APIBase: "https://x", APIToken: "TOKEN_SECRET"},
	}}}
	blob, err := encryptJSON(in)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	// 密文不得包含任何敏感明文
	for _, s := range []string{"SECRET_CODE_123", "TOKEN_SECRET", "OPENID_X"} {
		if bytes.Contains(blob, []byte(s)) {
			t.Fatalf("密文中泄露明文字段: %s", s)
		}
	}
	var env struct {
		V         int    `json:"v"`
		Encrypted string `json:"encrypted"`
	}
	if err := json.Unmarshal(blob, &env); err != nil || env.Encrypted == "" {
		t.Fatalf("信封结构异常: %v", err)
	}
	var out struct {
		Accounts []Account `json:"accounts"`
	}
	if err := decryptJSON(blob, &out); err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if len(out.Accounts) != 1 || out.Accounts[0].Code != "SECRET_CODE_123" {
		t.Fatalf("解密结果不符: %+v", out.Accounts)
	}
	if out.Accounts[0].Thirdparty == nil || out.Accounts[0].Thirdparty.APIToken != "TOKEN_SECRET" {
		t.Fatalf("thirdparty 解密结果不符: %+v", out.Accounts[0].Thirdparty)
	}
}

func TestDecryptLegacyPlaintext(t *testing.T) {
	encKey = make([]byte, 32)
	_, _ = rand.Read(encKey)
	legacy := []byte(`{"accounts":[{"id":"1","code":"PLAIN"}]}`)
	var out struct {
		Accounts []Account `json:"accounts"`
	}
	err := decryptJSON(legacy, &out)
	if !errors.Is(err, ErrNotEncrypted) {
		t.Fatalf("旧明文应返回 ErrNotEncrypted, got %v", err)
	}
}

func TestDecryptWrongKey(t *testing.T) {
	encKey = make([]byte, 32)
	_, _ = rand.Read(encKey)
	blob, err := encryptJSON(map[string]string{"a": "b"})
	if err != nil {
		t.Fatal(err)
	}
	encKey = make([]byte, 32)
	_, _ = rand.Read(encKey)
	var out map[string]string
	if err := decryptJSON(blob, &out); err == nil {
		t.Fatal("密钥不匹配时应解密失败")
	}
}

func TestDecodeKey(t *testing.T) {
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	cases := []string{hex.EncodeToString(k), base64.StdEncoding.EncodeToString(k), base64.RawURLEncoding.EncodeToString(k)}
	for _, c := range cases {
		got, err := decodeKey(c)
		if err != nil {
			t.Fatalf("decodeKey(%s): %v", c, err)
		}
		if !bytes.Equal(got, k) {
			t.Fatal("解码结果与原文不一致")
		}
	}
	if _, err := decodeKey("short"); err == nil {
		t.Fatal("非法密钥应报错")
	}
}
