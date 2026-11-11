package main

// 微信登录凭据（应用宝 LoginBuffer / refresh token）失效识别与处置。
//
// 现象：微信账号多次掉线后自动重连始终失败，必须手动重新扫码。
// 根因：换 code 前内置 YYB 会先用 refresh token 刷新 LoginBuffer，刷新失败返回
// 409 "account login_buffer expired (refresh failed); re-scan required"。之前 BOT 把
// 这类错误当成普通换 code 失败，降级用旧 code 硬连，重试 3 次后 stopped，前端看不到
// 真实原因，用户只能靠重新扫码试出来。这里把「凭据过期」单独识别并终止无效重试。

import (
	"errors"
	"fmt"
	"log"
	"strings"
)

// errWxCredentialExpired 微信登录凭据已失效，只能重新扫码恢复。
var errWxCredentialExpired = errors.New("微信登录凭据已过期，需要重新扫码登录")

// wxRescanHint 统一的用户可见提示。
const wxRescanHint = "微信登录凭据已过期，请在账号页重新扫码登录（自动重连已停止，手动重试无效）"

// isWxCredentialExpired 判断换 code / 刷新凭据的错误是否为凭据过期。
func isWxCredentialExpired(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errWxCredentialExpired) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "re-scan required") ||
		strings.Contains(s, "login_buffer expired") ||
		strings.Contains(s, "code=409")
}

// wrapWxCredentialError 把应用宝返回的凭据过期错误归一化，便于上层 errors.Is 判断。
func wrapWxCredentialError(err error) error {
	if isWxCredentialExpired(err) {
		return fmt.Errorf("%w: %v", errWxCredentialExpired, err)
	}
	return err
}

// needRescanReason 返回账号需要重新扫码的原因，空串表示不需要。
func (p *ClientPool) needRescanReason(accountID string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.needRescan[accountID]
}

// markNeedRescan 标记账号需重新扫码：写前端可见日志、推送通知、停止自动重连。
// 幂等：同一账号只记一次，避免每轮重连重复推送。
func (p *ClientPool) markNeedRescan(accountID, reason string) {
	if reason == "" {
		reason = "微信登录凭据已过期"
	}
	p.mu.Lock()
	if _, exists := p.needRescan[accountID]; exists {
		p.mu.Unlock()
		return
	}
	p.needRescan[accountID] = reason
	p.stopped[accountID] = true
	delete(p.reconnectAttempts, accountID)
	p.mu.Unlock()

	appendOpLog(accountID, "掉线", wxRescanHint+"；原因："+reason)
	notifyOffline(accountID, wxRescanHint)
	log.Printf("[wx-cred] 账号 %s 需重新扫码: %s", accountID, reason)
}

// clearNeedRescan 重新扫码 / 手动更新 code 成功后清除标记，并重置重连计数。
func (p *ClientPool) clearNeedRescan(accountID string) {
	p.mu.Lock()
	delete(p.needRescan, accountID)
	delete(p.stopped, accountID)
	delete(p.reconnectAttempts, accountID)
	delete(p.offlineSince, accountID)
	p.mu.Unlock()
}
