package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/asqw9527/qq-farm-bot-go-wj/models"
)

// QQ 扫码登录（NapCat 桥接）
//
// QQ 经典农场小程序登录 code 由 QQNT 内核 misc.loginWithAppId('1112386029') 直接签发，
// 本 BOT 通过 NapCat + qq-code 插件（fubiwangluo/qq-code）暴露的 /proto/code 接口取 code，
// 再用 platform=qq 连农场网关。前置条件：
//   1. 本机/可访问的 NapCat 已登录目标 QQ（扫码在 NapCat 完成，一次即可）；
//   2. NapCat 已安装并启用 napcat-plugin-qq-farm-code 插件（取 code 用）。
// 微信侧扫码（YYB）是小程序登录换 code，QQ 侧无对应自包含链路，故走 NapCat 桥接。

func registerQQLoginAPI(mux *http.ServeMux) {
	mux.HandleFunc("/api/qq/login", handleQQLogin)
	mux.HandleFunc("/api/qq/code", handleQQFetchCode)
	mux.HandleFunc("/api/qq/config", handleQQConfig)
}

// resolveQQCreds 解析 NapCat 凭据：请求体优先，回退系统配置。
func resolveQQCreds(body map[string]interface{}) (napcatBase, napcatKey, appId, ver, pluginPath string) {
	cfg := models.GetQQLoginConfig()
	if v, ok := body["napcatBase"].(string); ok && strings.TrimSpace(v) != "" {
		napcatBase = strings.TrimSpace(v)
	} else {
		napcatBase = cfg.NapcatBase
	}
	if v, ok := body["napcatKey"].(string); ok && strings.TrimSpace(v) != "" {
		napcatKey = strings.TrimSpace(v)
	} else {
		napcatKey = cfg.NapcatKey
	}
	if v, ok := body["appId"].(string); ok && strings.TrimSpace(v) != "" {
		appId = strings.TrimSpace(v)
	} else if cfg.AppID != "" {
		appId = cfg.AppID
	} else {
		appId = "1112386029"
	}
	if v, ok := body["ver"].(string); ok && strings.TrimSpace(v) != "" {
		ver = strings.TrimSpace(v)
	} else {
		ver = cfg.Ver
	}
	pluginPath = cfg.PluginPath
	if pluginPath == "" {
		pluginPath = "napcat-plugin-qq-farm-code"
	}
	return
}

// fetchQQFarmCode 调 NapCat 插件 /proto/code 取 QQ 农场登录 code。
// 返回协议：{ ok:true, uin, appid, code, raw } 或 { error:"..." }。
func fetchQQFarmCode(napcatBase, napcatKey, appId, ver, pluginPath string) (code string, uin string, err error) {
	if napcatBase == "" {
		return "", "", fmt.Errorf("未配置 NapCat 地址（请到系统设置填写，或登录时传入 napcatBase）")
	}
	base := strings.TrimRight(napcatBase, "/")
	endpoint := fmt.Sprintf("%s/plugin/%s/api/proto/code", base, pluginPath)
	q := url.Values{}
	q.Set("appid", appId)
	if ver != "" {
		q.Set("ver", ver)
	}
	if napcatKey != "" {
		q.Set("key", napcatKey)
	}
	endpoint += "?" + q.Encode()

	req, _ := http.NewRequest("GET", endpoint, nil)
	if napcatKey != "" {
		req.Header.Set("x-napcat-farm-key", napcatKey)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("请求 NapCat 失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var data map[string]interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		return "", "", fmt.Errorf("NapCat 返回非 JSON: %s", truncate(string(body), 200))
	}
	// 兼容 {ok:true, code:"..."} 或 {code:"..."}
	if c, ok := data["code"].(string); ok && c != "" {
		return c, strOrEmpty(data["uin"]), nil
	}
	if ok, _ := data["ok"].(bool); ok {
		if d, ok := data["data"].(map[string]interface{}); ok {
			if c, ok := d["code"].(string); ok && c != "" {
				return c, strOrEmpty(data["uin"]), nil
			}
		}
	}
	if e, ok := data["error"].(string); ok && e != "" {
		return "", "", fmt.Errorf("NapCat 取 code 失败: %s", e)
	}
	return "", "", fmt.Errorf("NapCat 未返回 code: %s", truncateStr(string(body), 200))
}

func strOrEmpty(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// handleQQFetchCode 仅取 code（不创建账号），用于验证/调试。
func handleQQFetchCode(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		writeError(w, 401, "unauthorized")
		return
	}
	var body map[string]interface{}
	json.NewDecoder(r.Body).Decode(&body)
	napcatBase, napcatKey, appId, ver, pluginPath := resolveQQCreds(body)
	code, uin, err := fetchQQFarmCode(napcatBase, napcatKey, appId, ver, pluginPath)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true, "data": map[string]interface{}{"code": code, "uin": uin, "appId": appId}})
}

// handleQQLogin 取 code 并创建 qq 平台账号、建立网关连接。
func handleQQLogin(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		writeError(w, 401, "unauthorized")
		return
	}
	var body map[string]interface{}
	json.NewDecoder(r.Body).Decode(&body)
	napcatBase, napcatKey, appId, ver, pluginPath := resolveQQCreds(body)
	code, uin, err := fetchQQFarmCode(napcatBase, napcatKey, appId, ver, pluginPath)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}

	name, _ := body["name"].(string)
	if name == "" {
		if uin != "" {
			name = "QQ" + uin
		} else {
			name = "QQ账号"
		}
	}

	if err := ensureAccountQuota(u.Username); err != nil {
		writeError(w, 403, err.Error())
		return
	}

	qqLogin := &models.QQLoginAccountConfig{
		NapcatBase: napcatBase,
		NapcatKey:  napcatKey,
		AppID:      appId,
		Ver:        ver,
	}

	// 去重：同一 UIN 的 QQ 账号重登复用已有记录（code 刷新即可）
	acc := models.Account{
		ID:         fmt.Sprintf("%d", time.Now().UnixNano()),
		Name:       name,
		Username:   u.Username,
		Code:       code,
		Platform:   "qq",
		QQ:         uin,
		UIN:        uin,
		Status:     "offline",
		CreatedAt:  time.Now().Format(time.RFC3339),
		QQLogin:    qqLogin,
	}
	if uin != "" {
		for _, a := range models.GetAccounts() {
			if a.UIN == uin && a.Platform == "qq" {
				acc = a
				acc.Code = code
				acc.Name = name
				acc.Username = u.Username
				acc.QQLogin = qqLogin
				acc.Thirdparty = nil // 切换到 NapCat 桥接，清掉第三方配置避免重连分支混乱
				break
			}
		}
	}

	result, err := models.AddOrUpdateAccount(acc)
	if err != nil {
		writeError(w, 500, "添加账号失败: "+err.Error())
		return
	}
	models.SetActiveAccount(acc.ID)
	connected := false
	if c, cerr := clientPool.Get(acc.ID); cerr == nil && c != nil && c.GID != 0 {
		connected = true
	}
	startAutomationForAccount(acc.ID)
	writeJSON(w, map[string]interface{}{
		"ok":            true,
		"data":          result,
		"activeAccountId": acc.ID,
		"connected":     connected,
		"uin":           uin,
	})
}

// handleQQConfig 读取/保存 QQ 扫码登录系统配置（NapCat 地址/key 等）。
func handleQQConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		cfg := models.GetQQLoginConfig()
		// 脱敏：key 非空仅返回掩码
		out := map[string]interface{}{
			"enabled":    cfg.Enabled,
			"napcatBase": cfg.NapcatBase,
			"appId":      cfg.AppID,
			"ver":        cfg.Ver,
			"pluginPath": cfg.PluginPath,
			"hasKey":     cfg.NapcatKey != "",
		}
		writeJSON(w, map[string]interface{}{"ok": true, "data": out})
	case "POST":
		u := currentUser(r)
		if u == nil || !isAdminUser(u) {
			writeError(w, 401, "unauthorized")
			return
		}
		var body struct {
			Enabled    *bool  `json:"enabled"`
			NapcatBase string `json:"napcatBase"`
			NapcatKey  string `json:"napcatKey"`
			AppID      string `json:"appId"`
			Ver        string `json:"ver"`
			PluginPath string `json:"pluginPath"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, 400, "invalid body")
			return
		}
		cfg := models.GetQQLoginConfig()
		if body.Enabled != nil {
			cfg.Enabled = *body.Enabled
		}
		if body.NapcatBase != "" {
			cfg.NapcatBase = strings.TrimSpace(body.NapcatBase)
		}
		if body.NapcatKey != "" {
			cfg.NapcatKey = strings.TrimSpace(body.NapcatKey)
		}
		if body.AppID != "" {
			cfg.AppID = strings.TrimSpace(body.AppID)
		}
		if body.Ver != "" {
			cfg.Ver = strings.TrimSpace(body.Ver)
		}
		if body.PluginPath != "" {
			cfg.PluginPath = strings.TrimSpace(body.PluginPath)
		}
		if err := models.SetQQLoginConfig(cfg); err != nil {
			writeError(w, 500, "保存失败: "+err.Error())
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true})
	default:
		writeError(w, 405, "method not allowed")
	}
}
