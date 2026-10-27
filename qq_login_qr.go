package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

// QQ 扫码登录二维码会话（NapCat WebUI 桥接）
//
// 体验对齐微信扫码：本端调 NapCat WebUI 的 /api/QQLogin/GetQQLoginQrcode 拿
// QQNT 登录二维码并渲染成 PNG，用户用手机 QQ 扫码授权 NapCat 登录；
// 轮询期间只要 qq-code 插件能签出农场 code（即 NapCat 已登录），即自动
// 创建账号并连网关——若 NapCat 本来就已登录，start 一步直接成功，无需扫码。
//
// NapCat WebUI 鉴权：POST /api/auth/login 提交 SHA256(token + ".napcat")
// 换 Credential，业务接口带 Authorization: Bearer <Credential>。
// 业务接口（POST）：/api/QQLogin/GetQQLoginQrcode、CheckLoginStatus、
// RefreshQRcode。响应包装 { code:0, data:{...} }。

// ---------- NapCat WebUI 客户端 ----------

// napcatWebuiCall 调 NapCat WebUI 业务接口，返回解包后的 data（尽力解包）。
func napcatWebuiCall(base, cred, path string, body map[string]interface{}, timeout time.Duration) (map[string]interface{}, error) {
	if base == "" {
		return nil, fmt.Errorf("未配置 NapCat 地址")
	}
	url := strings.TrimRight(base, "/") + path
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest("POST", url, rd)
	req.Header.Set("Content-Type", "application/json")
	if cred != "" {
		req.Header.Set("Authorization", "Bearer "+cred)
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接 NapCat 失败: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 401 {
		return nil, fmt.Errorf("NapCat WebUI 需要登录令牌：请在 NapCat 的 webui.json（或启动日志）中查看 token 并填入")
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("NapCat WebUI 返回 %d: %s", resp.StatusCode, truncateStr(string(raw), 200))
	}
	var env struct {
		Code    int             `json:"code"`
		Data    json.RawMessage `json:"data"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("NapCat WebUI 返回非 JSON: %s", truncateStr(string(raw), 200))
	}
	if env.Code != 0 && env.Code != 200 {
		msg := env.Message
		if msg == "" {
			msg = truncateStr(string(env.Data), 200)
		}
		return nil, fmt.Errorf("NapCat WebUI 错误(%d): %s", env.Code, msg)
	}
	// 优先解包 data；data 缺失时把整个响应当 data 用
	var data map[string]interface{}
	if len(env.Data) > 0 && string(env.Data) != "null" {
		if err := json.Unmarshal(env.Data, &data); err == nil && data != nil {
			return data, nil
		}
	}
	json.Unmarshal(raw, &data)
	return data, nil
}

// napcatWebuiLogin 用 WebUI token 换 Credential；token 为空时返回空 cred（尝试免鉴权访问）。
func napcatWebuiLogin(c qqCreds) (string, error) {
	if c.WebUIToken == "" {
		return "", nil
	}
	url := strings.TrimRight(c.Base, "/") + "/api/auth/login"
	// NapCat WebUI 登录：提交 hash = SHA256(token + ".napcat")，字段名是 hash（非 token）
	payload, _ := json.Marshal(map[string]string{"hash": sha256Hex([]byte(c.WebUIToken + ".napcat"))})
	req, _ := http.NewRequest("POST", url, strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("连接 NapCat 失败: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 401 {
		return "", fmt.Errorf("WebUI Token 不正确（请在 NapCat 的 webui.json 或启动日志查看 token）")
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Credential string `json:"Credential"`
			Credential2 string `json:"credential"`
		} `json:"data"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", fmt.Errorf("NapCat WebUI 登录返回异常: %s", truncateStr(string(raw), 200))
	}
	if env.Data.Credential == "" {
		env.Data.Credential = env.Data.Credential2
	}
	if env.Code != 0 && env.Code != 200 {
		return "", fmt.Errorf("NapCat WebUI 登录失败(%d): %s", env.Code, env.Message)
	}
	return env.Data.Credential, nil
}

// findStrKey 在 data 顶层按候选键找第一个非空字符串。
func findStrKey(data map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := data[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// napcatGetQrUrl 获取当前 QQ 登录二维码 URL。
func napcatGetQrUrl(base, cred string) (string, error) {
	data, err := napcatWebuiCall(base, cred, "/api/QQLogin/GetQQLoginQrcode", map[string]interface{}{}, 10*time.Second)
	if err != nil {
		return "", err
	}
	if u := findStrKey(data, "qrcodeUrl", "QrcodeUrl", "qrcode_url", "qrUrl", "url", "URL"); u != "" {
		return u, nil
	}
	// 兜底：找第一个 http 开头的字符串值
	for _, v := range data {
		if s, ok := v.(string); ok && strings.HasPrefix(s, "http") {
			return s, nil
		}
	}
	return "", fmt.Errorf("NapCat 未返回二维码地址: %s", truncateStr(fmt.Sprintf("%v", data), 200))
}

// napcatRefreshQR 触发 NapCat 重新生成二维码并取回新地址。
func napcatRefreshQR(base, cred string) (string, error) {
	_, _ = napcatWebuiCall(base, cred, "/api/QQLogin/RefreshQRcode", map[string]interface{}{}, 10*time.Second)
	return napcatGetQrUrl(base, cred)
}

// napcatLoginStatus CheckLoginStatus 的宽松解析结果。
type napcatLoginStatus struct {
	Online   bool
	Phase    string
	ErrText  string
	OKParsed bool
}

// napcatCheckLogin 查询登录阶段/在线状态（尽力而为，接口差异不影响主流程——
// 主成功信号是 qq-code 插件能签出 code）。
func napcatCheckLogin(base, cred string) napcatLoginStatus {
	st := napcatLoginStatus{}
	data, err := napcatWebuiCall(base, cred, "/api/QQLogin/CheckLoginStatus", map[string]interface{}{}, 10*time.Second)
	if err != nil {
		st.ErrText = err.Error()
		return st
	}
	st.OKParsed = true
	if b, ok := data["online"].(bool); ok && b {
		st.Online = true
	}
	if b, ok := data["login"].(bool); ok && b {
		st.Online = true
	}
	if s, ok := data["status"].(string); ok && (s == "online" || s == "logged_in") {
		st.Online = true
	}
	st.Phase = findStrKey(data, "phase", "loginPhase", "state")
	st.ErrText = findStrKey(data, "error", "errMsg", "err")
	return st
}

// qrDataURL 把内容渲染为二维码 PNG data URL。
func qrDataURL(content string) string {
	q, err := qrcode.New(content, qrcode.Medium)
	if err != nil {
		return ""
	}
	b, err := q.PNG(280)
	if err != nil {
		return ""
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(b)
}

// ---------- 扫码会话 ----------

type qqQrSession struct {
	id    string
	user  string
	name  string
	creds qqCreds
	cred  string // NapCat WebUI Credential

	mu      sync.Mutex
	state   string // waiting | scanned | success | expired | error
	qrImage string // data URL
	qrUrl   string
	uin     string
	err     string
	hint    string
	done    bool
	plugErr string // 最近一次插件取 code 的错误
	failCnt int    // NapCat 在线但插件连续取 code 失败次数
	created time.Time
}

var (
	qqQrMu       sync.Mutex
	qqQrSessions = map[string]*qqQrSession{}
)

func (s *qqQrSession) snapshot() map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]interface{}{
		"state":   s.state,
		"qrImage": s.qrImage,
		"qrUrl":   s.qrUrl,
		"uin":     s.uin,
		"error":   s.err,
		"hint":    s.hint,
	}
}

func (s *qqQrSession) setState(st string) {
	s.mu.Lock()
	s.state = st
	s.mu.Unlock()
}

func (s *qqQrSession) finish(st, uin, errMsg string) {
	s.mu.Lock()
	s.state = st
	if uin != "" {
		s.uin = uin
	}
	s.err = errMsg
	s.mu.Unlock()
}

func (s *qqQrSession) isDone() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done
}

// pruneQQQrSessions 清理超过 30 分钟的旧会话。
func pruneQQQrSessions() {
	qqQrMu.Lock()
	defer qqQrMu.Unlock()
	for id, s := range qqQrSessions {
		if time.Since(s.created) > 30*time.Minute {
			delete(qqQrSessions, id)
		}
	}
}

func newQQQrSessionID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// run 轮询循环：插件能签 code 即成功；否则跟 CheckLoginStatus 更新阶段。
func (s *qqQrSession) run() {
	defer func() {
		time.Sleep(5 * time.Minute)
		qqQrMu.Lock()
		delete(qqQrSessions, s.id)
		qqQrMu.Unlock()
	}()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		if s.isDone() {
			return
		}
		if time.Now().After(deadline) {
			s.finish("error", "", "登录超时，请重新开始扫码")
			return
		}

		// 1) 插件取 code —— NapCat 登录成功的权威信号
		code, uin, err := fetchQQFarmCodeTimeout(s.creds.Base, s.creds.Key, s.creds.AppID, s.creds.Ver, s.creds.PluginPath, 8*time.Second)
		if err == nil {
			if _, aerr := createQQAccountAndConnect(s.user, s.name, s.creds, code, uin); aerr != nil {
				s.finish("error", uin, aerr.Error())
				return
			}
			s.finish("success", uin, "")
			return
		}
		s.mu.Lock()
		s.plugErr = err.Error()
		s.mu.Unlock()

		// 2) CheckLoginStatus（尽力而为，仅优化提示）
		st := napcatCheckLogin(s.creds.Base, s.cred)
		low := strings.ToLower(st.ErrText)
		if st.ErrText != "" && (strings.Contains(st.ErrText, "过期") || strings.Contains(st.ErrText, "失效") ||
			strings.Contains(low, "expire") || strings.Contains(low, "timeout")) {
			s.finish("expired", "", "二维码已失效，请刷新后重新扫码")
			return
		}
		if st.Online {
			s.failCnt++
			if s.failCnt >= 4 {
				hint := "QQ 已在 NapCat 登录，但取农场 code 失败：" + s.plugErr +
					"（请确认 NapCat 已安装并启用 napcat-plugin-qq-farm-code 插件）"
				s.finish("error", "", hint)
				return
			}
		} else {
			s.failCnt = 0
			phase := strings.ToLower(st.Phase)
			if strings.Contains(phase, "confirm") || phase == "scanned" {
				s.setState("scanned")
			} else {
				s.setState("waiting")
			}
		}

		time.Sleep(2 * time.Second)
	}
}

// ---------- HTTP 处理器 ----------

// handleQQQrStart 开始 QQ 扫码登录：
//   - 未配置 NapCat → { state:"need_config" }
//   - NapCat 已登录（插件可签 code）→ 直接建号连网关 → { state:"success" }
//   - 否则取二维码 → { state:"qr", sessionId, qrImage }
func handleQQQrStart(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		writeError(w, 401, "unauthorized")
		return
	}
	var body map[string]interface{}
	json.NewDecoder(r.Body).Decode(&body)
	c := napcatEnvCreds(resolveQQCreds(body))
	name, _ := body["name"].(string)

	pruneQQQrSessions()

	// 1) NapCat 已登录？插件直接取 code 成功即免扫码
	code, uin, err := fetchQQFarmCodeTimeout(c.Base, c.Key, c.AppID, c.Ver, c.PluginPath, 8*time.Second)
	if err == nil {
		accID, aerr := createQQAccountAndConnect(u.Username, name, c, code, uin)
		if aerr != nil {
			writeError(w, errCode(aerr), aerr.Error())
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true, "data": map[string]interface{}{
			"state": "success", "uin": uin, "activeAccountId": accID,
		}})
		return
	}

	// 2) NapCat 未登录 → WebUI 鉴权 + 取二维码
	cred, aerr := napcatWebuiLogin(c)
	if aerr != nil {
		// 插件取码与 WebUI 都连不上 → 视为没有可用的 NapCat，引导配置
		if isNapcatConnErr(aerr) && isNapcatConnErr(err) {
			writeJSON(w, map[string]interface{}{"ok": true, "data": map[string]interface{}{
				"state": "need_config", "triedBase": c.Base,
				"hint": "未在 " + c.Base + " 检测到可用的 NapCat（手机 QQ 扫码需经 NapCat 出二维码）。可在本机一键部署：仓库 deploy/napcat/setup.sh；或填写远程 NapCat 地址。",
			}})
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true, "data": map[string]interface{}{
			"state": "error", "error": aerr.Error(), "needConfig": true,
			"hint": "NapCat WebUI 启用了登录令牌。Token 取值：①部署时 setup.sh 末尾打印的 Token；②`docker exec napcat-farm printenv WEBUI_TOKEN`；③`cat deploy/napcat/config/webui.json` 里的 token 字段。填到下方 WebUI Token 后重试。",
		}})
		return
	}
	qrUrl, gerr := napcatGetQrUrl(c.Base, cred)
	if gerr != nil {
		if isNapcatConnErr(gerr) && isNapcatConnErr(err) {
			writeJSON(w, map[string]interface{}{"ok": true, "data": map[string]interface{}{
				"state": "need_config", "triedBase": c.Base,
				"hint": "未在 " + c.Base + " 检测到可用的 NapCat，请部署后重试或填写远程 NapCat 地址。",
			}})
			return
		}
		// NapCat 上的 QQ 已登录 → WebUI 不再出登录二维码。此时本应走
		// 「插件直接取 code 免扫码」路径，说明第 1 步插件取码失败——
		// 重试一次；仍失败则把插件真实错误暴露出来引导排查。
		if strings.Contains(strings.ToLower(gerr.Error()), "logined") || strings.Contains(gerr.Error(), "已登录") {
			code2, uin2, err2 := fetchQQFarmCodeTimeout(c.Base, c.Key, c.AppID, c.Ver, c.PluginPath, 15*time.Second)
			if err2 == nil {
				accID, aerr := createQQAccountAndConnect(u.Username, name, c, code2, uin2)
				if aerr != nil {
					writeError(w, errCode(aerr), aerr.Error())
					return
				}
				writeJSON(w, map[string]interface{}{"ok": true, "data": map[string]interface{}{
					"state": "success", "uin": uin2, "activeAccountId": accID,
				}})
				return
			}
			writeJSON(w, map[string]interface{}{"ok": true, "data": map[string]interface{}{
				"state": "error", "needConfig": false,
				"error": "NapCat 已登录 QQ（无需扫码），但取农场 code 失败",
				"hint": "NapCat 上的 QQ 已登录，本应免扫码直接登录，但 qq-code 插件取农场 code 失败：" + err2.Error() +
					"。排查：① `docker logs napcat-farm 2>&1 | grep -i plugin` 看插件是否加载（若报 not in official plugin whitelist，重跑 bash deploy/napcat/setup.sh 打白名单补丁）；" +
					"② 面板「插件 Key」需与插件 data/config.json 的 key 一致（默认可留空）；" +
					"③ 重跑 bash deploy/napcat/setup.sh 会重装插件并重启 NapCat。修好后回到本页点「生成二维码」即可（会自动免扫码登录）。",
			}})
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true, "data": map[string]interface{}{
			"state": "error", "error": gerr.Error(), "needConfig": true,
		}})
		return
	}

	s := &qqQrSession{
		id:      newQQQrSessionID(),
		user:    u.Username,
		name:    name,
		creds:   c,
		cred:    cred,
		created: time.Now(),
	}
	s.qrUrl = qrUrl
	s.qrImage = qrDataURL(qrUrl)
	s.state = "waiting"

	qqQrMu.Lock()
	qqQrSessions[s.id] = s
	qqQrMu.Unlock()

	go s.run()

	writeJSON(w, map[string]interface{}{"ok": true, "data": map[string]interface{}{
		"state":   "qr",
		"qrImage": s.qrImage,
		"qrUrl":   qrUrl,
		"hint":    "请使用手机 QQ「扫一扫」扫描二维码，并在手机上确认登录",
	}})
}

// handleQQQrStatus 轮询扫码状态。
func handleQQQrStatus(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		writeError(w, 401, "unauthorized")
		return
	}
	id := r.URL.Query().Get("id")
	qqQrMu.Lock()
	s := qqQrSessions[id]
	qqQrMu.Unlock()
	if s == nil || s.user != u.Username {
		writeError(w, 404, "会话不存在或已过期")
		return
	}
	out := s.snapshot()
	out["ok"] = true
	writeJSON(w, out)
}

// handleQQQrRefresh 刷新二维码（过期后前端调用）。
func handleQQQrRefresh(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		writeError(w, 401, "unauthorized")
		return
	}
	var body struct {
		SessionID string `json:"sessionId"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	qqQrMu.Lock()
	s := qqQrSessions[body.SessionID]
	qqQrMu.Unlock()
	if s == nil || s.user != u.Username {
		writeError(w, 404, "会话不存在或已过期")
		return
	}
	if s.isDone() {
		writeError(w, 400, "会话已结束，请重新开始")
		return
	}
	qrUrl, err := napcatRefreshQR(s.creds.Base, s.cred)
	if err != nil {
		writeError(w, 502, "刷新二维码失败: "+err.Error())
		return
	}
	s.mu.Lock()
	s.qrUrl = qrUrl
	s.qrImage = qrDataURL(qrUrl)
	s.state = "waiting"
	s.err = ""
	img := s.qrImage
	s.mu.Unlock()
	writeJSON(w, map[string]interface{}{"ok": true, "data": map[string]interface{}{"qrImage": img, "qrUrl": qrUrl}})
}

// handleQQQrCancel 取消扫码（前端关闭弹窗时调用，.best effort）。
func handleQQQrCancel(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		writeError(w, 401, "unauthorized")
		return
	}
	var body struct {
		SessionID string `json:"sessionId"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	qqQrMu.Lock()
	s := qqQrSessions[body.SessionID]
	qqQrMu.Unlock()
	if s != nil && s.user == u.Username {
		s.mu.Lock()
		s.done = true
		s.mu.Unlock()
	}
	writeJSON(w, map[string]interface{}{"ok": true})
}
