package models

import (
	cryptorand "crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/pbkdf2"
)

const (
	defaultAccountLimit = 2
	loginAttemptsLimit  = 5
	lockoutDuration     = 15 * time.Minute
	rateLimitWindow     = 1 * time.Minute
	maxAttemptsPerIP    = 6
	ipLockoutDuration   = 10 * time.Minute
	saltLength          = 32
	iterations          = 100000
	keyLength           = 64
)

var (
	users               []User
	cards               []Card
	loginAttempts       = make(map[string]LoginAttempt)
	cardClaimRecords    = make(map[string]CardClaimRecord)
	userStoreMu         sync.Mutex
	userDataDir         string
	cardClaimEnabled    = true
	userTokens          = make(map[string]time.Time) // token -> expiry
	userTokenToUsername = make(map[string]string)    // token -> username
	userTokensMu        sync.Mutex
)

type LoginAttempt struct {
	Count        int   `json:"count"`
	LockedUntil  int64 `json:"lockedUntil,omitempty"`
	WindowStart  int64 `json:"windowStart"`
	FirstAttempt int64 `json:"firstAttempt,omitempty"`
	LastAttempt  int64 `json:"lastAttempt,omitempty"`
}

type CardClaimRecord struct {
	UAHash    string `json:"uaHash"`
	ClaimTime int64  `json:"claimTime"`
	CardCode  string `json:"cardCode"`
	Username  string `json:"username,omitempty"`
}

type CardDuration struct {
	Days        int64 `json:"days"`
	DurationMs  int64 `json:"durationMs"`
	IsPermanent bool  `json:"isPermanent"`
}

type Card struct {
	Code        string `json:"code"`
	Description string `json:"description"`
	Type        string `json:"type"` // "time" or "quota"
	Enabled     bool   `json:"enabled"`
	UsedBy      string `json:"usedBy,omitempty"`
	UsedAt      int64  `json:"usedAt,omitempty"`
	CreatedAt   int64  `json:"createdAt"`
	Value       int    `json:"value,omitempty"` // for quota cards
	Days        int    `json:"days,omitempty"`
	DurationMs  int64  `json:"durationMs,omitempty"`  // duration in milliseconds
	IsPermanent bool   `json:"isPermanent,omitempty"` // permanent access flag
	ExpiresAt   int64  `json:"expiresAt,omitempty"`
}

type User struct {
	Username     string `json:"username"`
	Password     string `json:"password"`
	Role         string `json:"role"` // "admin", "user"
	CardCode     string `json:"cardCode,omitempty"`
	Card         *Card  `json:"card,omitempty"`
	AccountLimit int    `json:"accountLimit,omitempty"`
	ExpiresAt    int64  `json:"expiresAt,omitempty"`
	IsPermanent  bool   `json:"isPermanent,omitempty"`
	CreatedAt    int64  `json:"createdAt"`
}

type AuthResult struct {
	Username     string `json:"username"`
	Role         string `json:"role"`
	CardCode     string `json:"cardCode,omitempty"`
	AccountLimit int    `json:"accountLimit,omitempty"`
}

type RegisterResult struct {
	Ok    bool        `json:"ok"`
	Error string      `json:"error,omitempty"`
	User  *AuthResult `json:"user,omitempty"`
}

type CardClaimResult struct {
	Ok         bool   `json:"ok"`
	CardCode   string `json:"cardCode,omitempty"`
	Days       int    `json:"days,omitempty"`
	DurationMs int64  `json:"durationMs,omitempty"`
	Error      string `json:"error,omitempty"`
}

func InitUserStore(dir string) {
	userDataDir = dir
	loadUsers()
	loadCards()
	loadLoginAttempts()
	loadCardClaimRecords()
	initDefaultAdmin()
}

func usersFilePath() string {
	return filepath.Join(userDataDir, "users.json")
}

func cardsFilePath() string {
	return filepath.Join(userDataDir, "cards.json")
}

func loginAttemptsPath() string {
	return filepath.Join(userDataDir, "login-attempts.json")
}

func cardClaimPath() string {
	return filepath.Join(userDataDir, "card-claim.json")
}

func loadUsers() {
	filePath := usersFilePath()
	data, err := os.ReadFile(filePath)
	if err != nil {
		users = []User{}
		saveUsers()
		return
	}
	var wrapper struct {
		Users []User `json:"users"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		users = []User{}
		return
	}
	users = wrapper.Users
	if users == nil {
		users = []User{}
	}
}

func saveUsers() {
	data, _ := json.MarshalIndent(struct {
		Users []User `json:"users"`
	}{Users: users}, "", "  ")
	os.WriteFile(usersFilePath(), data, 0644)
}

func loadCards() {
	filePath := cardsFilePath()
	data, err := os.ReadFile(filePath)
	if err != nil {
		cards = []Card{}
		saveCards()
		return
	}
	var wrapper struct {
		Cards []Card `json:"cards"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		cards = []Card{}
		return
	}
	cards = wrapper.Cards
	if cards == nil {
		cards = []Card{}
	}
}

func saveCards() {
	data, _ := json.MarshalIndent(struct {
		Cards []Card `json:"cards"`
	}{Cards: cards}, "", "  ")
	os.WriteFile(cardsFilePath(), data, 0644)
}

func loadLoginAttempts() {
	filePath := loginAttemptsPath()
	data, err := os.ReadFile(filePath)
	if err != nil {
		return
	}
	json.Unmarshal(data, &loginAttempts)
}

func saveLoginAttempts() {
	data, _ := json.MarshalIndent(loginAttempts, "", "  ")
	os.WriteFile(loginAttemptsPath(), data, 0644)
}

func loadCardClaimRecords() {
	filePath := cardClaimPath()
	data, err := os.ReadFile(filePath)
	if err != nil {
		return
	}
	var wrapper struct {
		Enabled bool                       `json:"enabled"`
		Records map[string]CardClaimRecord `json:"records"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return
	}
	cardClaimEnabled = wrapper.Enabled
	cardClaimRecords = wrapper.Records
}

func saveCardClaimRecords() {
	wrapper := struct {
		Enabled bool                       `json:"enabled"`
		Records map[string]CardClaimRecord `json:"records"`
	}{
		Enabled: cardClaimEnabled,
		Records: cardClaimRecords,
	}
	data, _ := json.MarshalIndent(wrapper, "", "  ")
	os.WriteFile(cardClaimPath(), data, 0644)
}

func hashPassword(password string) string {
	saltBytes := make([]byte, saltLength)
	cryptorand.Read(saltBytes)
	salt := hex.EncodeToString(saltBytes)
	key := pbkdf2.Key([]byte(password), []byte(salt), iterations, keyLength, sha256.New)
	return fmt.Sprintf("%s:%s", salt, hex.EncodeToString(key))
}

func verifyPassword(password, stored string) bool {
	if stored == "" {
		return false
	}
	parts := strings.SplitN(stored, ":", 2)
	if len(parts) != 2 {
		return false
	}
	salt, expectedHash := parts[0], parts[1]
	computedHash := hex.EncodeToString(pbkdf2.Key([]byte(password), []byte(salt), iterations, keyLength, sha256.New))
	return subtle.ConstantTimeCompare([]byte(expectedHash), []byte(computedHash)) == 1
}

func needsRehash(stored string) bool {
	return stored == "" || !strings.Contains(stored, ":")
}

func validatePasswordStrength(password string) []string {
	var errors []string
	if len(password) < 6 {
		errors = append(errors, "密码长度至少6位")
	}
	if len(password) > 128 {
		errors = append(errors, "密码长度不能超过128位")
	}
	return errors
}

func generateCardCode() string {
	chars := "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 16)
	cryptorand.Read(b)
	for i := range b {
		b[i] = chars[b[i]%byte(len(chars))]
	}
	return string(b)
}

func initDefaultAdmin() {
	for i := range users {
		if users[i].Username == "admin" {
			return
		}
	}
	// 默认管理员账号，密码 admin，需要提醒开发者修改
	defaultPassword := "admin"
	users = append(users, User{
		Username:     "admin",
		Password:     hashPassword(defaultPassword),
		Role:         "admin",
		AccountLimit: -1,
		CreatedAt:    time.Now().UnixMilli(),
	})
	saveUsers()
	log.Println("[security] 已创建默认管理员账号 admin/admin，请尽快登录修改密码！")
}

func checkRateLimit(ip string) (allowed bool, remainingMs int64, message string) {
	key := fmt.Sprintf("ip:%s", ip)
	now := time.Now().UnixMilli()

	if attempt, exists := loginAttempts[key]; exists {
		if attempt.LockedUntil > 0 && attempt.LockedUntil > now {
			remainingMs = attempt.LockedUntil - now
			return false, remainingMs, fmt.Sprintf("该 IP 登录失败过多，请 %d 秒后重试", remainingMs/1000)
		}
		if attempt.LockedUntil > 0 && attempt.LockedUntil <= now {
			delete(loginAttempts, key)
		}
		if now-attempt.WindowStart > int64(rateLimitWindow.Milliseconds()) {
			loginAttempts[key] = LoginAttempt{Count: 1, WindowStart: now}
			saveLoginAttempts()
			return true, 0, ""
		}
		if attempt.Count >= maxAttemptsPerIP {
			attempt.LockedUntil = now + int64(ipLockoutDuration.Milliseconds())
			loginAttempts[key] = attempt
			saveLoginAttempts()
			remainingMs = int64(ipLockoutDuration.Milliseconds())
			return false, remainingMs, fmt.Sprintf("该 IP 登录失败过多，请 %d 秒后重试", remainingMs/1000)
		}
		attempt.Count++
		loginAttempts[key] = attempt
		saveLoginAttempts()
	} else {
		loginAttempts[key] = LoginAttempt{Count: 1, WindowStart: now}
		saveLoginAttempts()
	}
	return true, 0, ""
}

func checkAccountLockout(username string) (locked bool, remainingMs int64, message string) {
	key := fmt.Sprintf("user:%s", username)
	now := time.Now().UnixMilli()

	if attempt, exists := loginAttempts[key]; exists {
		if attempt.LockedUntil > 0 && attempt.LockedUntil > now {
			remainingMs = attempt.LockedUntil - now
			return true, remainingMs, fmt.Sprintf("账户已被锁定，请 %d 分钟后重试", remainingMs/60000)
		}
		if attempt.LockedUntil > 0 && attempt.LockedUntil <= now {
			delete(loginAttempts, key)
		}
	}
	return false, 0, ""
}

func recordFailedAttempt(username string) (locked bool, message string) {
	key := fmt.Sprintf("user:%s", username)
	now := time.Now().UnixMilli()

	if attempt, exists := loginAttempts[key]; exists {
		attempt.Count++
		attempt.LastAttempt = now
		if attempt.Count >= loginAttemptsLimit {
			attempt.LockedUntil = now + int64(lockoutDuration.Milliseconds())
			loginAttempts[key] = attempt
			saveLoginAttempts()
			return true, fmt.Sprintf("登录失败次数过多，账户已被锁定 %.0f 分钟", lockoutDuration.Minutes())
		}
		loginAttempts[key] = attempt
		saveLoginAttempts()
		return false, fmt.Sprintf("用户名或密码错误，剩余尝试次数: %d", loginAttemptsLimit-attempt.Count)
	}

	loginAttempts[key] = LoginAttempt{
		Count:        1,
		FirstAttempt: now,
		LastAttempt:  now,
	}
	saveLoginAttempts()
	return false, "用户名或密码错误"
}

func clearFailedAttempts(username string) {
	key := fmt.Sprintf("user:%s", username)
	delete(loginAttempts, key)
	saveLoginAttempts()
}

func clearIpAttempts(ip string) {
	key := fmt.Sprintf("ip:%s", ip)
	delete(loginAttempts, key)
	saveLoginAttempts()
}

func ValidateUser(username, password, ip string) (AuthResult, error) {
	if allowed, _, msg := checkRateLimit(ip); !allowed {
		return AuthResult{}, errors.New(msg)
	}

	locked, _, msg := checkAccountLockout(username)
	if locked {
		return AuthResult{}, errors.New(msg)
	}

	var user *User
	for i := range users {
		if users[i].Username == username {
			user = &users[i]
			break
		}
	}

	if user == nil {
		recordFailedAttempt(username)
		return AuthResult{}, errors.New("用户名或密码错误")
	}

	if !verifyPassword(password, user.Password) {
		locked, msg = recordFailedAttempt(username)
		if locked {
			return AuthResult{}, errors.New(msg)
		}
		return AuthResult{}, errors.New(msg)
	}

	clearFailedAttempts(username)
	clearIpAttempts(ip)

	if needsRehash(user.Password) {
		user.Password = hashPassword(password)
		saveUsers()
	}

	limit := user.AccountLimit
	if limit == 0 {
		limit = defaultAccountLimit
	}

	return AuthResult{
		Username:     user.Username,
		Role:         user.Role,
		CardCode:     user.CardCode,
		AccountLimit: limit,
	}, nil
}

func RegisterUser(username, password, cardCode string) RegisterResult {
	if len(username) < 3 || len(username) > 32 {
		return RegisterResult{Error: "用户名长度需在3-32位之间"}
	}
	if !regexp.MustCompile(`^\w+$`).MatchString(username) {
		return RegisterResult{Error: "用户名只能包含字母、数字和下划线"}
	}
	for _, u := range users {
		if u.Username == username {
			return RegisterResult{Error: "用户名已存在"}
		}
	}

	if errs := validatePasswordStrength(password); len(errs) > 0 {
		return RegisterResult{Error: errs[0]}
	}

	// 卡密为必填项，不允许无卡密注册
	if cardCode == "" {
		// 检查是否有可用的空闲卡密
		availableCount := 0
		for _, c := range cards {
			if c.Type == "time" && c.UsedBy == "" && c.Enabled {
				availableCount++
			}
		}
		if availableCount == 0 {
			return RegisterResult{Error: "注册失败，请联系管理员"}
		}
		return RegisterResult{Error: "请输入卡密"}
	}

	var card *Card
	for i := range cards {
		if cards[i].Code == cardCode {
			card = &cards[i]
			break
		}
	}

	if card == nil {
		return RegisterResult{Error: "卡密不存在"}
	}
	if !card.Enabled {
		return RegisterResult{Error: "卡密已被禁用，请联系管理员"}
	}
	if card.UsedBy != "" {
		return RegisterResult{Error: "卡密已被使用"}
	}
	if card.Type == "quota" {
		return RegisterResult{Error: "注册只能使用时间卡密，额度卡密请登录后在续费中使用"}
	}

	now := time.Now().UnixMilli()
	durationMs := card.DurationMs
	if durationMs == 0 {
		durationMs = 24 * 60 * 60 * 1000 // default 1 day
	}
	expiresAt := now + durationMs

	cardCopy := *card
	cardCopy.UsedBy = username
	cardCopy.UsedAt = now
	cardCopy.ExpiresAt = expiresAt
	newUser := User{
		Username:     username,
		Password:     hashPassword(password),
		Role:         "user",
		CardCode:     cardCode,
		Card:         &cardCopy,
		AccountLimit: defaultAccountLimit,
		ExpiresAt:    expiresAt,
		CreatedAt:    now,
	}

	// 更新卡密使用状态
	cards[findCardIndex(cardCode)].UsedBy = username
	cards[findCardIndex(cardCode)].UsedAt = now
	cards[findCardIndex(cardCode)].ExpiresAt = expiresAt

	users = append(users, newUser)
	saveUsers()
	saveCards()

	return RegisterResult{Ok: true, User: &AuthResult{
		Username:     newUser.Username,
		Role:         newUser.Role,
		CardCode:     newUser.CardCode,
		AccountLimit: newUser.AccountLimit,
	}}
}

// findCardIndex 根据卡密代码查找卡密在 cards 切片中的索引
func findCardIndex(code string) int {
	for i, c := range cards {
		if c.Code == code {
			return i
		}
	}
	return -1
}

func RenewUser(username, cardCode string) (bool, string, error) {
	var user *User
	for i := range users {
		if users[i].Username == username {
			user = &users[i]
			break
		}
	}
	if user == nil {
		return false, "", errors.New("用户不存在")
	}

	var card *Card
	for i := range cards {
		if cards[i].Code == cardCode {
			card = &cards[i]
			break
		}
	}
	if card == nil {
		return false, "", errors.New("卡密不存在")
	}
	if !card.Enabled {
		return false, "", errors.New("卡密已被禁用")
	}
	if card.UsedBy != "" {
		return false, "", errors.New("卡密已被使用")
	}

	now := time.Now().UnixMilli()

	msg := "续费成功"
	if card.Type == "quota" {
		add := card.Value
		if add <= 0 {
			add = 1
		}
		if user.AccountLimit == -1 {
			msg = "当前账号上限为无限，额度卡密已核销"
		} else {
			if user.AccountLimit <= 0 {
				user.AccountLimit = defaultAccountLimit
			}
			user.AccountLimit += add
			msg = fmt.Sprintf("续费成功，账号上限已提升至 %d", user.AccountLimit)
		}
	} else {
		if user.Card == nil {
			copied := *card
			user.Card = &copied
		}

		prevExpiresAt := user.ExpiresAt
		if prevExpiresAt == 0 {
			prevExpiresAt = user.Card.ExpiresAt
		}
		if prevExpiresAt > 0 && prevExpiresAt < now {
			prevExpiresAt = 0
		}

		if card.IsPermanent {
			user.IsPermanent = true
			user.ExpiresAt = 0
			user.Card.ExpiresAt = 0
			user.Card.DurationMs = -1
			user.Card.Days = -1
			user.Card.IsPermanent = true
			msg = "续费成功，已开通永久时长"
		} else {
			var newExp int64
			if prevExpiresAt > now {
				newExp = prevExpiresAt + card.DurationMs
			} else {
				newExp = now + card.DurationMs
			}
			user.ExpiresAt = newExp
			user.Card.ExpiresAt = newExp
			user.Card.Days = card.Days
			user.Card.DurationMs = card.DurationMs
		}
		user.CardCode = card.Code
	}

	idx := findCardIndex(cardCode)
	if idx >= 0 {
		cards[idx].UsedBy = username
		cards[idx].UsedAt = now
	}

	saveUsers()
	saveCards()

	return true, msg, nil
}

func GetAllUsers() []User {
	var result []User
	for _, u := range users {
		if u.Role != "super_admin" {
			result = append(result, u)
		}
	}
	return result
}

func GetCardClaimStatus() map[string]interface{} {
	return map[string]interface{}{
		"enabled": cardClaimEnabled,
	}
}

func GetAvailableTimeCardCount() int {
	count := 0
	for _, c := range cards {
		if c.Type == "time" && c.UsedBy == "" && c.Enabled {
			count++
		}
	}
	return count
}

func SetCardClaimStatus(enabled bool) {
	cardClaimEnabled = enabled
	saveCardClaimRecords()
}

func ClaimCardByUA(ua, username string) CardClaimResult {
	if !cardClaimEnabled {
		return CardClaimResult{Error: "卡密领取功能未开启"}
	}

	uaHash := fmt.Sprintf("%x", sha256.Sum256([]byte(ua)))

	if record, exists := cardClaimRecords[uaHash]; exists {
		if time.Now().UnixMilli()-record.ClaimTime < 86400000 {
			remaining := 86400000 - (time.Now().UnixMilli() - record.ClaimTime)
			return CardClaimResult{Error: fmt.Sprintf("您已经在24小时内领取过一次卡密了！还剩 %d 秒", remaining/1000)}
		}
	}

	var availableCards []Card
	for _, c := range cards {
		if c.Type == "time" && c.UsedBy == "" && c.Enabled {
			availableCards = append(availableCards, c)
		}
	}

	if len(availableCards) == 0 {
		return CardClaimResult{Error: "卡密库存不足，请联系管理员！"}
	}

	cardClaimRecords[uaHash] = CardClaimRecord{
		UAHash:    uaHash,
		ClaimTime: time.Now().UnixMilli(),
		CardCode:  availableCards[0].Code,
		Username:  username,
	}
	saveCardClaimRecords()

	return CardClaimResult{
		Ok:         true,
		CardCode:   availableCards[0].Code,
		Days:       availableCards[0].Days,
		DurationMs: availableCards[0].DurationMs,
	}
}

func DeleteUser(username string) error {
	for i, u := range users {
		if u.Username == username {
			if u.Role == "admin" || u.Role == "super_admin" {
				return errors.New("不能删除管理员账号")
			}
			users = append(users[:i], users[i+1:]...)
			saveUsers()
			return nil
		}
	}
	return errors.New("用户不存在")
}

// ---- 用户 Token 管理 ----

const userTokenTTL = 24 * time.Hour

// NewUserToken 生成用户登录 token
func NewUserToken() (string, error) {
	b := make([]byte, 32)
	if _, err := cryptorand.Read(b); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(b)
	userTokensMu.Lock()
	userTokens[tok] = time.Now().Add(userTokenTTL)
	userTokensMu.Unlock()
	return tok, nil
}

// NewUserTokenForUser 为指定用户生成登录 token
func NewUserTokenForUser(username string) (string, error) {
	tok, err := NewUserToken()
	if err != nil {
		return "", err
	}
	userTokensMu.Lock()
	userTokenToUsername[tok] = username
	userTokensMu.Unlock()
	return tok, nil
}

// UserTokenValid 校验用户 token 是否有效
func UserTokenValid(tok string) bool {
	if tok == "" {
		return false
	}
	userTokensMu.Lock()
	defer userTokensMu.Unlock()
	exp, ok := userTokens[tok]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(userTokens, tok)
		delete(userTokenToUsername, tok)
		return false
	}
	return true
}

// GetUserByToken 根据 token 获取用户信息
func GetUserByToken(tok string) *User {
	if tok == "" {
		return nil
	}

	userTokensMu.Lock()
	defer userTokensMu.Unlock()

	exp, ok := userTokens[tok]
	if !ok {
		return nil
	}
	if time.Now().After(exp) {
		delete(userTokens, tok)
		delete(userTokenToUsername, tok)
		return nil
	}

	username, ok := userTokenToUsername[tok]
	if !ok {
		return nil
	}

	for i := range users {
		if users[i].Username == username {
			return &users[i]
		}
	}
	return nil
}

// RevokeUserToken 注销用户 token
func RevokeUserToken(tok string) {
	userTokensMu.Lock()
	delete(userTokens, tok)
	delete(userTokenToUsername, tok)
	userTokensMu.Unlock()
}

// GetUserByUsername 根据用户名获取用户
func GetUserByUsername(username string) (*User, error) {
	for i := range users {
		if users[i].Username == username {
			return &users[i], nil
		}
	}
	return nil, errors.New("用户不存在")
}

// GetUserRole 获取用户角色
func GetUserRole(username string) (string, error) {
	user, err := GetUserByUsername(username)
	if err != nil {
		return "", err
	}
	return user.Role, nil
}

func GetCardClaimRecords() []CardClaimRecord {
	var records []CardClaimRecord
	for _, r := range cardClaimRecords {
		records = append(records, r)
	}
	return records
}

// ChangeUserPassword 修改用户密码
func ChangeUserPassword(username, oldPassword, newPassword string) error {
	var user *User
	for i := range users {
		if users[i].Username == username {
			user = &users[i]
			break
		}
	}
	if user == nil {
		return errors.New("用户不存在")
	}
	if !verifyPassword(oldPassword, user.Password) {
		return errors.New("原密码错误")
	}
	if errs := validatePasswordStrength(newPassword); len(errs) > 0 {
		return errors.New(errs[0])
	}
	user.Password = hashPassword(newPassword)
	saveUsers()
	return nil
}

// AdminChangeUserPassword 管理员直接修改用户密码（不校验旧密码）
func AdminChangeUserPassword(username, newPassword string) error {
	userStoreMu.Lock()
	defer userStoreMu.Unlock()
	var user *User
	for i := range users {
		if users[i].Username == username {
			user = &users[i]
			break
		}
	}
	if user == nil {
		return errors.New("用户不存在")
	}
	if user.Role == "admin" || user.Role == "super_admin" {
		return errors.New("不能修改管理员密码")
	}
	if errs := validatePasswordStrength(newPassword); len(errs) > 0 {
		return errors.New(errs[0])
	}
	user.Password = hashPassword(newPassword)
	saveUsers()
	return nil
}

// GetAllCards 获取所有卡密列表
func GetAllCards() []Card {
	userStoreMu.Lock()
	defer userStoreMu.Unlock()

	result := make([]Card, len(cards))
	copy(result, cards)
	return result
}

// GenerateCards 批量生成时间卡密
func GenerateCards(count int, description string, durationMs int64) ([]string, error) {
	userStoreMu.Lock()
	defer userStoreMu.Unlock()

	if count <= 0 || count > 1000 {
		return nil, errors.New("生成数量必须在 1-1000 之间")
	}

	codes := make([]string, count)
	now := time.Now().UnixMilli()
	days := 0
	if durationMs > 0 {
		days = int(durationMs / (24 * 60 * 60 * 1000))
	}

	for i := 0; i < count; i++ {
		code := generateCardCode()
		card := Card{
			Code:        code,
			Description: description,
			Type:        "time",
			Enabled:     true,
			CreatedAt:   now,
			Days:        days,
			DurationMs:  durationMs,
			IsPermanent: false,
		}
		cards = append(cards, card)
		codes[i] = code
	}

	saveCards()
	return codes, nil
}

// GenerateQuotaCards 批量生成额度卡密（提升账号上限）
func GenerateQuotaCards(count int, description string, value int) ([]string, error) {
	userStoreMu.Lock()
	defer userStoreMu.Unlock()

	if count <= 0 || count > 1000 {
		return nil, errors.New("生成数量必须在 1-1000 之间")
	}
	if value <= 0 {
		value = 1
	}

	codes := make([]string, count)
	now := time.Now().UnixMilli()
	if description == "" {
		description = fmt.Sprintf("额度卡密 +%d 账号", value)
	}

	for i := 0; i < count; i++ {
		code := generateCardCode()
		card := Card{
			Code:        code,
			Description: description,
			Type:        "quota",
			Enabled:     true,
			CreatedAt:   now,
			Value:       value,
		}
		cards = append(cards, card)
		codes[i] = code
	}

	saveCards()
	return codes, nil
}

// UpdateUserLimits 管理员修改用户时长与账号上限。
// expiresAtMs < 0 表示不改到期时间；expiresAtMs == 0 表示永久。
// accountLimit == nil 表示不改账号上限；*accountLimit == -1 表示无限。
func UpdateUserLimits(username string, expiresAtMs int64, accountLimit *int) error {
	userStoreMu.Lock()
	defer userStoreMu.Unlock()

	var user *User
	for i := range users {
		if users[i].Username == username {
			user = &users[i]
			break
		}
	}
	if user == nil {
		return errors.New("用户不存在")
	}
	if user.Role == "admin" || user.Role == "super_admin" {
		return errors.New("不能修改管理员账号")
	}

	if expiresAtMs >= 0 {
		if expiresAtMs == 0 {
			user.IsPermanent = true
			user.ExpiresAt = 0
			if user.Card != nil {
				user.Card.IsPermanent = true
				user.Card.ExpiresAt = 0
				user.Card.DurationMs = -1
				user.Card.Days = -1
			}
		} else {
			user.IsPermanent = false
			user.ExpiresAt = expiresAtMs
			if user.Card != nil {
				user.Card.IsPermanent = false
				user.Card.ExpiresAt = expiresAtMs
			}
		}
	}

	if accountLimit != nil {
		if *accountLimit < -1 {
			return errors.New("账号上限无效")
		}
		user.AccountLimit = *accountLimit
	}

	saveUsers()
	return nil
}

func EffectiveAccountLimit(user *User) int {
	if user == nil {
		return defaultAccountLimit
	}
	if user.AccountLimit == -1 {
		return -1
	}
	if user.AccountLimit <= 0 {
		return defaultAccountLimit
	}
	return user.AccountLimit
}

func UserExpired(user *User) bool {
	if user == nil {
		return true
	}
	if user.Role == "admin" || user.Role == "super_admin" || user.IsPermanent {
		return false
	}
	exp := user.ExpiresAt
	if exp == 0 && user.Card != nil {
		if user.Card.IsPermanent {
			return false
		}
		exp = user.Card.ExpiresAt
	}
	if exp == 0 {
		return false
	}
	return time.Now().UnixMilli() > exp
}

// ToggleCards 批量启用/禁用卡密
func ToggleCards(codes []string, enabled bool) {
	userStoreMu.Lock()
	defer userStoreMu.Unlock()

	for _, code := range codes {
		for i := range cards {
			if cards[i].Code == code {
				cards[i].Enabled = enabled
				break
			}
		}
	}

	saveCards()
}

// DeleteCards 按卡密代码删除，已使用的也可删。返回实际删除数量。
func DeleteCards(codes []string) int {
	userStoreMu.Lock()
	defer userStoreMu.Unlock()

	want := map[string]bool{}
	for _, code := range codes {
		if code != "" {
			want[code] = true
		}
	}
	if len(want) == 0 {
		return 0
	}
	kept := cards[:0]
	deleted := 0
	for _, c := range cards {
		if want[c.Code] {
			deleted++
			continue
		}
		kept = append(kept, c)
	}
	cards = kept
	saveCards()
	return deleted
}
