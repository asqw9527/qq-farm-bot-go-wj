package main

// 管理面板 TLS（HTTPS）支持。
//
// 三种启用方式（按优先级）：
//  1. ADMIN_TLS_CERT=/path/cert.pem ADMIN_TLS_KEY=/path/key.pem  —— 正式证书或前置反代证书
//  2. ADMIN_TLS_AUTO=1                                            —— 首次启动自动生成自签名证书（数据目录 cert.pem/key.pem，权限 0600）
//  3. 均未设置                                                     —— 仍以明文 HTTP 启动并打印告警（生产环境不推荐）
//
// 启用后登录口令、账号 Code / apiToken 等敏感数据均为加密传输，不再是明文 HTTP。

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// adminTLS 返回可用的证书/密钥路径；enabled=false 表示回退明文 HTTP。
func adminTLS(dataDir string) (certPath, keyPath string, enabled bool) {
	cert := os.Getenv("ADMIN_TLS_CERT")
	key := os.Getenv("ADMIN_TLS_KEY")
	if cert != "" && key != "" {
		if _, err := tls.LoadX509KeyPair(cert, key); err != nil {
			log.Fatalf("[tls] 加载证书失败（ADMIN_TLS_CERT/ADMIN_TLS_KEY）: %v", err)
		}
		log.Printf("[tls] 已加载证书: %s", cert)
		return cert, key, true
	}
	if os.Getenv("ADMIN_TLS_AUTO") == "1" {
		c, k, err := ensureSelfSignedCert(dataDir)
		if err != nil {
			log.Printf("[tls] 自签名证书生成失败，回退明文 HTTP: %v", err)
			return "", "", false
		}
		return c, k, true
	}
	return "", "", false
}

// ensureSelfSignedCert 保证数据目录存在自签名证书；已存在则直接复用（避免重启后浏览器需重新信任）。
func ensureSelfSignedCert(dataDir string) (string, string, error) {
	certPath := filepath.Join(dataDir, "cert.pem")
	keyPath := filepath.Join(dataDir, "key.pem")
	if _, err1 := os.Stat(certPath); err1 == nil {
		if _, err2 := os.Stat(keyPath); err2 == nil {
			if _, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
				return certPath, keyPath, nil
			}
		}
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", "", err
	}
	hosts := []string{"localhost", "127.0.0.1", "::1"}
	if hn, err := os.Hostname(); err == nil && hn != "" {
		hosts = append(hosts, hn)
	}
	if ips, err := localIPs(); err == nil {
		hosts = append(hosts, ips...)
	}

	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "qq-farm-bot-go-wj", Organization: []string{"QQ Farm Bot"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(5, 0, 0), // 5 年
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return "", "", err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return "", "", err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := os.WriteFile(certPath, certPEM, 0600); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		return "", "", err
	}
	log.Printf("[tls] 已生成自签名证书: %s（浏览器首次访问需手动信任；也可改用正式证书）", certPath)
	return certPath, keyPath, nil
}

// localIPs 收集本机非回环 IPv4，加入证书 SAN，便于内网直接以 IP 访问。
func localIPs() ([]string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() {
			continue
		}
		if v4 := ipNet.IP.To4(); v4 != nil {
			out = append(out, v4.String())
		}
	}
	return out, nil
}

// schemeOf TLS 是否启用决定面板访问协议。
func schemeOf(enabled bool) string {
	if enabled {
		return "https"
	}
	return "http"
}

// tlsHint 未启用 TLS 时的告警文案。
func tlsHint() string {
	return fmt.Sprintf("⚠️ 未启用 TLS：登录口令与账号凭据仍以明文 HTTP 传输。" +
		"建议设置 ADMIN_TLS_CERT/ADMIN_TLS_KEY（正式证书），或 ADMIN_TLS_AUTO=1（自动生成自签名证书），或前置 Nginx 反代 HTTPS。")
}
