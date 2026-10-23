package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	goembed "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/asqw9527/qq-farm-bot-go-wj/models"
	"yyb_go/embed"
)

var dataDir string

// 前端 Vue 构建产物（web/dist）在编译期嵌入二进制，部署只需换二进制。
// 注意：必须用 all: 前缀，否则 embed 默认跳过文件名以 _ 或 . 开头的文件；
// Vite 产出的共享块 _plugin-vue_export-helper-*.js 以 _ 开头，漏嵌会导致该块
// 线上返回 index.html(content-type=text/html) → 浏览器解析模块报错 → 整页黑屏。
//
//go:embed all:web/dist
var webDistFS goembed.FS

// 内置应用宝(YYB)服务：BOT 自带换 code 能力，不依赖外部 YYB_API_URL/YYB_API_KEY。
// embeddedYybBaseURL 非空时表示内置服务已成功监听 127.0.0.1 随机端口。
var (
	embeddedYybBaseURL string
	embeddedYybClose   func() error
)

// newEmbeddedYybServer 初始化并启动内置 YYB http 服务，返回 baseURL 与清理函数。
// 监听 127.0.0.1 随机端口，ResourceRoot 指向运行目录下的 yyb-resource/。
func newEmbeddedYybServer() (string, func() error, error) {
	// 内置服务鉴权需要一个非空 token：未通过 YYB_API_TOKEN 配置时生成一个。
	// embed 门面对 httpapi.ApiToken 提供 Get/Set，内置服务鉴权与 BOT 调用使用同一值。
	if embed.GetApiToken() == "" {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return "", nil, err
		}
		embed.SetApiToken("yyb-" + hex.EncodeToString(buf))
	}

	srv, err := embed.Start(embed.Config{
		ResourceRoot:   "yyb-resource",
		DBFilename:     embed.DefaultDBFilename,
		SessionTTL:     30 * time.Minute,
		RequestTimeout: 45 * time.Second,
		AvatarTimeout:  10 * time.Second,
		ScanTimeout:    180 * time.Second,
		QRSessionTTL:   5 * time.Minute,
	})
	if err != nil {
		return "", nil, err
	}
	return srv.BaseURL(), srv.Close, nil
}

// resetAdminArg 解析 -reset-admin[=新密码] 命令行参数（存在即重置并退出）
func resetAdminArg() (password string, ok bool) {
	for _, a := range os.Args[1:] {
		if a == "-reset-admin" || a == "--reset-admin" {
			return "", true
		}
		for _, p := range []string{"-reset-admin=", "--reset-admin="} {
			if s, found := strings.CutPrefix(a, p); found {
				return s, true
			}
		}
	}
	return "", false
}

func main() {
	// -reset-admin[=新密码]：重置 admin 用户密码后退出（忘记密码恢复登录用）
	if val, ok := resetAdminArg(); ok {
		homeDir, _ := os.UserHomeDir()
		dataDir = homeDir + "/.qq-farm-bot"
		models.InitStore(dataDir)
		models.InitUserStore(dataDir)
		models.ResetAdminPassword(val)
		return
	}

	homeDir, _ := os.UserHomeDir()
	dataDir = homeDir + "/.qq-farm-bot"
	models.InitStore(dataDir)
	initStealToStore(dataDir + "/stealTo.json")

	// 初始化多用户/卡密存储（users.json / cards.json / login-attempts.json / card-claim.json）
	models.InitUserStore(dataDir)

	gwCfg := models.GetGatewayConfig()
	adminPort := gwCfg.AdminPort
	if adminPort == 0 {
		adminPort = 3009
	}

	mux := http.NewServeMux()

	// 启动内置应用宝(YYB)服务：BOT 自带换 code 能力。失败仅告警不阻塞 BOT 启动。
	if base, closeFn, err := newEmbeddedYybServer(); err != nil {
		log.Printf("[yyb] warn: failed to init embedded yyb service: %v", err)
	} else {
		embeddedYybBaseURL = base
		embeddedYybClose = closeFn
		log.Printf("[yyb] embedded yyb service listening at %s", base)
	}

	// 启动后台掉线自动重连扫描（增强：不破坏现有懒重连，Get() 仍可用）
	clientPool.StartAutoReconnect(context.Background())

	// 启动神秘商人自动购买
	startMysteryAutoBuyLoop(context.Background())

	// 启动奶茶经营收益自动收取（S4 奶茶热销中）
	startMilkTeaAutoClaimLoop(context.Background())

	// 启动定时收益推送调度（每天北京时间指定时刻推一次）
	startDailyReportScheduler()

	// 启动时为所有已配置账号建立初始连接。
	// 后台异步执行，不阻塞 HTTP 启动；失败仅记录（重连/懒连接兜底）。
	go func() {
		for _, acc := range models.GetAccounts() {
			if _, err := clientPool.Get(acc.ID); err != nil {
				log.Printf("[startup] 账号 %s 初始连接失败: %v", acc.ID, err)
			}
		}
	}()

	// 注册经验上限持久化回调（经验满/跨日重置时按账号持久化标志）
	initExpLimitPersistence()

	// 启动所有账号的自动化引擎
	startAllAutomation()

	// 启动时扫描 game-config/seed_images_named，建立 itemId → 图片URL 映射（working dir 为游戏配置根目录）
	InitImageMap("game-config")
	initGameConfig("game-config")

	corsHandler := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 收紧 CORS：仅回显同源（含 localhost 开发）的 Origin，不再对任意站点放开 "*"。
			// 面板使用自定义 token 头鉴权，同源部署无需通配。
			if origin := r.Header.Get("Origin"); origin != "" && corsOriginAllowed(origin, r.Host) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Add("Vary", "Origin")
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, x-account-id, x-admin-token, x-user-token")
			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusOK)
				return
			}
			next.ServeHTTP(w, r)
		})
	}

	api := http.NewServeMux()
	api.HandleFunc("/api/health", handleHealth)
	registerUserAPI(api)
	registerHomeAPI(api)
	registerProfileAPI(api)
	registerCareerAPI(api)
	registerAccountAPI(api)
	registerFriendOpsAPI(api)
	registerFriendExtraAPI(api)
	registerFriendBotScanAPI(api)
	registerReconnectAPI(api)
	registerSettingsAPI(api)
	registerIllustratedAPI(api)
	registerAnalyticsAPI(api)
	registerActivityAPI(api)
	registerShopAPI(api)
	registerTaskAPI(api)
	registerAdminAuthAPI(api)
	registerAssetsSyncAPI(api)

	mux.Handle("/api/", corsHandler(adminAuthMiddleware(api)))

	// 游戏静态资源（种子/作物图片等）：/game-config/** → game-config/ 目录
	mux.Handle("/game-config/", http.StripPrefix("/game-config/", http.FileServer(http.Dir("game-config"))))

	// 内置 YYB 的 /qr/**（二维码图片、轮询、确认）：转发到内嵌 YYB 服务。
	// 随机监听端口无法预绑定，改为请求时动态重建反向代理，避免并发竞争。
	mux.Handle("/qr/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if embeddedYybBaseURL == "" {
			http.Error(w, "YYB 服务未就绪", http.StatusServiceUnavailable)
			return
		}
		proxy := qrReverseProxy(embeddedYybBaseURL)
		if proxy == nil {
			http.Error(w, "YYB 服务地址无效", http.StatusInternalServerError)
			return
		}
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/qr")
		proxy.ServeHTTP(w, r)
	}))

	// 根路径：serve 嵌入的前端静态产物（Vue 构建的 web/dist），带 SPA history 回退。
	distSub, err := fs.Sub(webDistFS, "web/dist")
	if err != nil {
		log.Fatalf("[admin] 嵌入前端失败: %v", err)
	}
	distFileServer := http.FileServer(http.FS(distSub))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		// 资源文件存在则直接返回（带正确 Content-Type）
		if p != "" {
			if f, openErr := distSub.Open(p); openErr == nil {
				f.Close()
				distFileServer.ServeHTTP(w, r)
				return
			}
		}
		// 否则回退到 index.html（客户端路由 /farm、/shop 等直接刷新可命中）
		idx, readErr := webDistFS.ReadFile("web/dist/index.html")
		if readErr != nil {
			http.Error(w, "前端未构建", http.StatusInternalServerError)
			return
		}
		// index.html 必须 no-cache：它引用带哈希的 asset，若被浏览器缓存旧版本会导致
		// 前端更新后仍加载旧 JS（asset 哈希变化旧文件已删 → 页面功能不更新/报错）
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(idx)
	})

	addr := fmt.Sprintf("0.0.0.0:%d", adminPort)
	certPath, keyPath, tlsOn := adminTLS(dataDir)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second, // 防慢速请求头（slowloris）
		ReadTimeout:       90 * time.Second,
		IdleTimeout:       120 * time.Second,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
		// 不设 WriteTimeout：部分管理接口（配置/素材同步）单次处理可能较长，
		// 其内部已由 adminAuthMiddleware 注入 120s context 超时兜底。
	}
	if tlsOn {
		log.Printf("[admin] QQ Farm Bot Go · https://localhost:%d（TLS 已启用）", adminPort)
	} else {
		log.Printf("[admin] QQ Farm Bot Go · http://localhost:%d", adminPort)
		log.Printf("[admin] %s", tlsHint())
	}

	// 优雅退出：收到 SIGINT/SIGTERM 后停止接收新连接并等待在途请求结束。
	shutdownDone := make(chan struct{})
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Printf("[admin] 收到退出信号，开始优雅关闭…")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("[admin] 优雅关闭超时: %v", err)
		}
		close(shutdownDone)
	}()

	var serveErr error
	if tlsOn {
		serveErr = srv.ListenAndServeTLS(certPath, keyPath)
	} else {
		serveErr = srv.ListenAndServe()
	}
	if serveErr != nil && serveErr != http.ErrServerClosed {
		log.Fatalf("[admin] failed to start: %v", serveErr)
	}
	<-shutdownDone
	// 主循环退出后关闭内置 YYB 服务
	if embeddedYybClose != nil {
		_ = embeddedYybClose()
	}
}

// qr 反向代理缓存：内嵌 YYB 服务地址固定后复用同一 ReverseProxy，
// 避免每个请求都新建（含独立 Transport），并消除并发重建竞争。
var (
	qrProxyMu  sync.Mutex
	qrProxyURL string
	qrProxy    *httputil.ReverseProxy
)

func qrReverseProxy(target string) *httputil.ReverseProxy {
	qrProxyMu.Lock()
	defer qrProxyMu.Unlock()
	if qrProxy != nil && qrProxyURL == target {
		return qrProxy
	}
	u, err := url.Parse(target)
	if err != nil {
		return nil
	}
	qrProxy = httputil.NewSingleHostReverseProxy(u)
	qrProxyURL = target
	return qrProxy
}

// corsOriginAllowed 仅允许同源 Origin（含 localhost 开发与平台预览域名）。
func corsOriginAllowed(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, host) {
		return true
	}
	h := u.Hostname()
	if h == "localhost" || h == "127.0.0.1" {
		return true
	}
	// 平台在线预览域名（预览代理可能改写 Host，需按域名后缀放行）
	if strings.HasSuffix(h, ".monkeycode-ai.online") {
		return true
	}
	return false
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]interface{}{
		"ok": true, "status": "ok", "uptime": time.Since(startTime).Seconds(),
		"version": buildVersion, "buildTime": buildTime,
	})
}

var startTime = time.Now()

func writeJSON(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": msg})
}
