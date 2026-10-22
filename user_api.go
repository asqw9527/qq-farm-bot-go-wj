package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/asqw9527/qq-farm-bot-go-wj/models"
)

// registerUserAPI 注册多用户系统API路由
func registerUserAPI(mux *http.ServeMux) {
	mux.HandleFunc("/api/users", handleUsers)
	mux.HandleFunc("/api/users/login", handleUserLogin)
	mux.HandleFunc("/api/users/register", handleUserRegister)
	mux.HandleFunc("/api/users/claim-card", handleUserClaimCard)
	mux.HandleFunc("/api/users/renew", handleUserRenew)
	mux.HandleFunc("/api/users/me", handleUserMe)
	mux.HandleFunc("/api/users/delete", handleUserDelete)
	mux.HandleFunc("/api/users/change-password", handleUserChangePassword)
	mux.HandleFunc("/api/cards", handleCards)
	// 管理员卡密管理接口
	mux.HandleFunc("/api/admin/cards", handleAdminCards)
	mux.HandleFunc("/api/admin/users", handleAdminUsers)
}

// ---- 用户管理 ----

// handleUsers: 用户列表管理（GET: 列出用户, POST: 创建用户）
func handleUsers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		users := models.GetAllUsers()
		var result []map[string]interface{}
		for _, u := range users {
			result = append(result, map[string]interface{}{
				"username":     u.Username,
				"role":         u.Role,
				"cardCode":     u.CardCode,
				"accountLimit": models.EffectiveAccountLimit(&u),
				"createdAt":    u.CreatedAt,
			})
		}
		writeJSON(w, map[string]interface{}{"ok": true, "data": result})
	case http.MethodPost:
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
			CardCode string `json:"cardCode,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "参数错误")
			return
		}
		if body.Username == "" || body.Password == "" {
			writeError(w, http.StatusBadRequest, "用户名和密码不能为空")
			return
		}
		result := models.RegisterUser(body.Username, body.Password, body.CardCode)
		if !result.Ok {
			writeError(w, http.StatusBadRequest, result.Error)
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true, "user": result.User})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleUserLogin: 用户登录，返回token
func handleUserLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "参数错误")
		return
	}
	if body.Username == "" || body.Password == "" {
		writeError(w, http.StatusBadRequest, "用户名和密码不能为空")
		return
	}

	clientIP := strings.Split(r.RemoteAddr, ":")[0]
	result, err := models.ValidateUser(body.Username, body.Password, clientIP)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}

	// 生成登录 token（关联用户名）
	token, err := models.NewUserTokenForUser(body.Username)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "生成token失败")
		return
	}

	writeJSON(w, map[string]interface{}{
		"ok":    true,
		"token": token,
		"user": map[string]interface{}{
			"username":     result.Username,
			"role":         result.Role,
			"accountLimit": result.AccountLimit,
		},
	})
}

// handleUserRegister: 用户注册接口
func handleUserRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		CardCode string `json:"cardCode,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "参数错误")
		return
	}
	if body.Username == "" || body.Password == "" {
		writeError(w, http.StatusBadRequest, "用户名和密码不能为空")
		return
	}

	result := models.RegisterUser(body.Username, body.Password, body.CardCode)
	if !result.Ok {
		writeError(w, http.StatusBadRequest, result.Error)
		return
	}

	writeJSON(w, map[string]interface{}{
		"ok":   true,
		"user": result.User,
	})
}

// handleUserMe: 获取当前用户信息（基于 token）
func handleUserMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// 从 token 中获取用户名
	token := userTokenFromRequest(r)
	if token == "" {
		writeError(w, http.StatusUnauthorized, "未登录")
		return
	}

	user := models.GetUserByToken(token)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "token 无效")
		return
	}

	limit := models.EffectiveAccountLimit(user)
	expiresAt := user.ExpiresAt
	if expiresAt == 0 && user.Card != nil {
		expiresAt = user.Card.ExpiresAt
	}
	writeJSON(w, map[string]interface{}{
		"ok": true,
		"user": map[string]interface{}{
			"username":     user.Username,
			"role":         user.Role,
			"cardCode":     user.CardCode,
			"accountLimit": limit,
			"expiresAt":    expiresAt,
			"isPermanent":  user.IsPermanent || (user.Card != nil && user.Card.IsPermanent),
			"createdAt":    user.CreatedAt,
		},
	})
}

// handleUserRenew: 用户续期（使用卡密）
func handleUserRenew(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var body struct {
		Username string `json:"username"`
		CardCode string `json:"cardCode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "参数错误")
		return
	}
	if tok := userTokenFromRequest(r); tok != "" {
		if u := models.GetUserByToken(tok); u != nil {
			body.Username = u.Username
		}
	}
	if body.Username == "" || body.CardCode == "" {
		writeError(w, http.StatusBadRequest, "参数错误")
		return
	}

	success, msg, err := models.RenewUser(body.Username, body.CardCode)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{
		"ok":      success,
		"message": msg,
	})
}

// handleUserDelete: 删除用户
func handleUserDelete(w http.ResponseWriter, r *http.Request) {
	if !requireAdminUser(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var body struct {
		Username string `json:"username"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "参数错误")
		return
	}

	err := models.DeleteUser(body.Username)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true})
}

// handleUserChangePassword: 修改用户密码
func handleUserChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var body struct {
		OldPassword string `json:"oldPassword"`
		NewPassword string `json:"newPassword"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "参数错误")
		return
	}
	if body.OldPassword == "" || body.NewPassword == "" {
		writeError(w, http.StatusBadRequest, "旧密码和新密码不能为空")
		return
	}

	username := r.URL.Query().Get("username")
	if username == "" {
		writeError(w, http.StatusBadRequest, "缺少用户名")
		return
	}

	err := models.ChangeUserPassword(username, body.OldPassword, body.NewPassword)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true})
}

// ---- 卡密管理 ----

// handleCards: 卡密列表
func handleCards(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var cards []models.Card
	users := models.GetAllUsers()
	for _, u := range users {
		if u.Card != nil {
			cards = append(cards, *u.Card)
		}
	}
	writeJSON(w, map[string]interface{}{"ok": true, "data": cards})
}

// handleUserClaimCard: 用户领取卡密（防刷机制）
func handleUserClaimCard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var body struct {
		Username  string `json:"username"`
		UserAgent string `json:"userAgent,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "参数错误")
		return
	}

	ua := body.UserAgent
	if ua == "" {
		ua = "default"
	}

	result := models.ClaimCardByUA(ua, body.Username)
	writeJSON(w, map[string]interface{}{
		"ok":         result.Ok,
		"cardCode":   result.CardCode,
		"days":       result.Days,
		"durationMs": result.DurationMs,
		"error":      result.Error,
	})
}

func requireAdminUser(w http.ResponseWriter, r *http.Request) bool {
	token := userTokenFromRequest(r)
	if token == "" {
		writeError(w, http.StatusUnauthorized, "未登录或登录已过期，请先登录")
		return false
	}
	u := models.GetUserByToken(token)
	if !isAdminUser(u) {
		writeError(w, http.StatusForbidden, "请联系管理员")
		return false
	}
	return true
}

// ---- 管理员卡密管理 ----

// handleAdminCards: 管理员卡密管理
func handleAdminCards(w http.ResponseWriter, r *http.Request) {
	if !requireAdminUser(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		// 获取所有卡密列表
		cards := models.GetAllCards()
		writeJSON(w, map[string]interface{}{"ok": true, "cards": cards})
	case http.MethodPost:
		var body struct {
			Action      string   `json:"action"`
			Description string   `json:"description"`
			Days        int      `json:"days"`
			Count       int      `json:"count"`
			CardType    string   `json:"cardType,omitempty"`
			Value       int      `json:"value,omitempty"`
			Codes       []string `json:"codes,omitempty"`
			Enabled     *bool    `json:"enabled,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "参数错误")
			return
		}

		switch body.Action {
		case "generate":
			if body.Count <= 0 {
				body.Count = 10
			}
			cardType := body.CardType
			if cardType == "" {
				cardType = "time"
			}
			var cardCodes []string
			var err error
			if cardType == "quota" {
				value := body.Value
				if value <= 0 {
					value = 1
				}
				cardCodes, err = models.GenerateQuotaCards(body.Count, body.Description, value)
			} else {
				if body.Days <= 0 {
					body.Days = 30
				}
				durationMs := int64(body.Days) * 24 * 60 * 60 * 1000
				cardCodes, err = models.GenerateCards(body.Count, body.Description, durationMs)
			}
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, map[string]interface{}{"ok": true, "codes": cardCodes})
		case "toggle":
			if len(body.Codes) == 0 {
				writeError(w, http.StatusBadRequest, "请指定卡密")
				return
			}
			if body.Enabled == nil {
				writeError(w, http.StatusBadRequest, "请指定启用状态")
				return
			}
			models.ToggleCards(body.Codes, *body.Enabled)
			writeJSON(w, map[string]interface{}{"ok": true})
		case "delete":
			if len(body.Codes) == 0 {
				writeError(w, http.StatusBadRequest, "请指定卡密")
				return
			}
			n := models.DeleteCards(body.Codes)
			writeJSON(w, map[string]interface{}{"ok": true, "deleted": n})
		default:
			writeError(w, http.StatusBadRequest, "未知操作")
		}
	case http.MethodDelete:
		var body struct {
			Codes []string `json:"codes"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "参数错误")
			return
		}
		n := models.DeleteCards(body.Codes)
		writeJSON(w, map[string]interface{}{"ok": true, "deleted": n})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// ---- 管理员用户管理 ----

// handleAdminUsers: 管理员用户管理
func handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	if !requireAdminUser(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		accountsByUser := map[string][]map[string]interface{}{}
		var unbound []map[string]interface{}
		for _, acc := range models.GetAccounts() {
			info := accountLoginSummary(acc)
			if acc.Username == "" {
				unbound = append(unbound, info)
				continue
			}
			accountsByUser[acc.Username] = append(accountsByUser[acc.Username], info)
		}
		if unbound == nil {
			unbound = []map[string]interface{}{}
		}
		users := models.GetAllUsers()
		var result []map[string]interface{}
		for _, u := range users {
			if u.Role == "super_admin" {
				continue
			}
			limit := models.EffectiveAccountLimit(&u)
			expiresAt := u.ExpiresAt
			if expiresAt == 0 && u.Card != nil {
				expiresAt = u.Card.ExpiresAt
			}
			accs := accountsByUser[u.Username]
			if accs == nil {
				accs = []map[string]interface{}{}
			}
			result = append(result, map[string]interface{}{
				"username":     u.Username,
				"role":         u.Role,
				"cardCode":     u.CardCode,
				"accountLimit": limit,
				"expiresAt":    expiresAt,
				"isPermanent":  u.IsPermanent || (u.Card != nil && u.Card.IsPermanent),
				"createdAt":    u.CreatedAt,
				"accounts":     accs,
			})
		}
		writeJSON(w, map[string]interface{}{"ok": true, "users": result, "unboundAccounts": unbound})
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		var body struct {
			Username     string `json:"username"`
			AccountLimit *int   `json:"accountLimit"`
			ExpiresAt    *int64 `json:"expiresAt"`
			Days         *int   `json:"days"`
			Permanent    *bool  `json:"permanent"`
			Password     string `json:"password,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "参数错误")
			return
		}
		if body.Username == "" {
			writeError(w, http.StatusBadRequest, "请指定用户名")
			return
		}
		expiresAt := int64(-1)
		if body.Permanent != nil && *body.Permanent {
			expiresAt = 0
		} else if body.ExpiresAt != nil {
			expiresAt = *body.ExpiresAt
		} else if body.Days != nil {
			if *body.Days < 0 {
				writeError(w, http.StatusBadRequest, "天数无效")
				return
			}
			if *body.Days == 0 {
				expiresAt = 0
			} else {
				expiresAt = time.Now().UnixMilli() + int64(*body.Days)*24*60*60*1000
			}
		}
		if err := models.UpdateUserLimits(body.Username, expiresAt, body.AccountLimit); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if body.Password != "" {
			if err := models.AdminChangeUserPassword(body.Username, body.Password); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		writeJSON(w, map[string]interface{}{"ok": true})
	case http.MethodDelete:
		var body struct {
			Usernames []string `json:"usernames"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "参数错误")
			return
		}
		for _, username := range body.Usernames {
			models.DeleteUser(username)
		}
		writeJSON(w, map[string]interface{}{"ok": true})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}
