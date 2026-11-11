package main

// 微信登录凭据定时保活。
//
// config.AutoCodeRefresh 此前只有配置结构体、没有任何运行代码：refresh token 只在
// 「掉线换 code」时才被使用，长期不用或多次重连并发刷新导致轮换冲突后就会失效，
// 用户只能重新扫码。这里补上串行保活循环：定时刷新 LoginBuffer / refresh token，
// 并顺带把新 code 存回账号（不重连），使凭据始终处于活跃状态。
//
// 串行执行（单 goroutine 顺序遍历）是刻意设计：同一账号的 refresh token 不能并发刷新，
// 否则服务端轮换后旧 token 失效会被误判为 expired。

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/asqw9527/qq-farm-bot-go-wj/models"
)

const (
	wxKeepaliveInitialDelay = 60 * time.Second
	wxKeepaliveTick         = 30 * time.Minute
	wxKeepaliveDefaultGap   = 60 * time.Minute
)

var (
	wxKeepaliveMu   sync.Mutex
	wxKeepaliveLast = map[string]time.Time{}
)

// startWxCredentialKeepalive 启动后台串行保活（main 启动时调用一次）。
func startWxCredentialKeepalive(ctx context.Context) {
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wxKeepaliveInitialDelay):
		}
		runWxCredentialKeepaliveOnce()
		t := time.NewTicker(wxKeepaliveTick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				runWxCredentialKeepaliveOnce()
			}
		}
	}()
}

// runWxCredentialKeepaliveOnce 串行处理所有内置 YYB 微信账号。
func runWxCredentialKeepaliveOnce() {
	for _, acc := range models.GetAccounts() {
		if !isBuiltinWxAccount(acc) {
			continue
		}
		cfg := models.GetAccountConfig(acc.ID).AutoCodeRefresh
		if !cfg.Enabled {
			continue
		}
		if reason := clientPool.needRescanReason(acc.ID); reason != "" {
			continue
		}
		gap := time.Duration(cfg.IntervalMinutes) * time.Minute
		if gap <= 0 {
			gap = wxKeepaliveDefaultGap
		}
		wxKeepaliveMu.Lock()
		last := wxKeepaliveLast[acc.ID]
		if time.Since(last) < gap {
			wxKeepaliveMu.Unlock()
			continue
		}
		wxKeepaliveLast[acc.ID] = time.Now()
		wxKeepaliveMu.Unlock()

		if err := keepaliveWxCredentials(acc); err != nil {
			if isWxCredentialExpired(err) {
				clientPool.markNeedRescan(acc.ID, err.Error())
			} else {
				// 网络抖动等临时失败：保留状态，等下一轮重试（不标记需重扫）。
				log.Printf("[wx-cred] 账号 %s 凭据保活失败（下轮重试）: %v", acc.ID, err)
			}
		}
	}
}

// isBuiltinWxAccount 仅处理走内置应用宝的微信账号；QQ/NapCat 与第三方接口不走这里。
func isBuiltinWxAccount(acc models.Account) bool {
	return acc.Platform == "wx" && strings.TrimSpace(acc.OpenID) != "" &&
		acc.Thirdparty == nil && acc.QQLogin == nil
}

// keepaliveWxCredentials 刷新凭据并顺带换新 code 存回账号（不重建网关连接）。
func keepaliveWxCredentials(acc models.Account) error {
	apiBase, apiKey := resolveYybCreds(nil)
	if apiBase == "" || apiKey == "" {
		return fmt.Errorf("内置应用宝服务未就绪")
	}
	if err := refreshYybAccountCredentials(apiBase, apiKey, acc.OpenID); err != nil {
		return err
	}
	code, err := getCodeFromYyb(apiBase, apiKey, acc.OpenID, "")
	if err != nil {
		return err
	}
	fresh := models.GetAccountByID(acc.ID)
	if fresh == nil {
		return nil
	}
	fresh.Code = code
	_, err = models.AddOrUpdateAccount(*fresh)
	return err
}
