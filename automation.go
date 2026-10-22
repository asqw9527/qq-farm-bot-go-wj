package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/asqw9527/qq-farm-bot-go-wj/config"
	"github.com/asqw9527/qq-farm-bot-go-wj/gw"
	"github.com/asqw9527/qq-farm-bot-go-wj/models"
	"github.com/asqw9527/qq-farm-bot-go-wj/proto"
)

// ============================================================
// 自动化引擎
// + services/planting-service.js + services/farm-fertilizer.js + services/farm-scheduler.js
// 设计：每个账号一个 runner（含若干 goroutine）按 config.AutomationConfig 决定开哪些循环。
// 目前接入：农场巡田（水/草/虫/收/种/施肥/升级）、自动卖果实、自动买化肥。
// 好友巡查（偷/帮/捣）、护主犬刷新、每日任务等在后续批次接入。
// ============================================================

var (
	automationMu      sync.Mutex
	automationRunners = map[string]*automationRunner{}
	// farm 执行互斥：farm_push 推送触发与 automationLoop 串行 farm 不能同时执行
	farmBusyMu  sync.Mutex
	farmBusySet = map[string]bool{}

	// 2x2 预留机制
	reserved2x2Mu      sync.Mutex
	reserved2x2        = map[string]map[string][]int64{} // accountID -> (groupKey -> landIDs)
	failed2x2RetriesMu sync.Mutex
	failed2x2Retries   = map[string]map[string]int64{} // accountID -> (retryKey -> retryUntil Unix)
	last2x2WaitKey     = map[string]string{}           // accountID -> lastGroupKey（避免重复日志）
)

type automationRunner struct {
	stop chan struct{}
}

// 游戏网络心跳间隔/连续丢失阈值
const (
	gwHeartbeatInterval = 25 * time.Second
	hbMissLimit         = 5
)

// ===== 账号串行执行线：前端 HTTP 账号操作也投递到这里执行 =====
// 保证同一账号任何时刻只有一条执行线访问 TSDK，杜绝并发（out of bounds 根因）。
type accountWork struct {
	fn   func()
	done chan struct{}
}

var (
	workMu       sync.Mutex
	accountWorks = map[string]chan *accountWork{}
	// 账号串行线内嵌套执行的标记：同一账号 line 正在执行外部任务（work.fn）时置 true，
	// 使该 goroutine 内的嵌套 submitAccountWork 直接内联执行，避免向自身队列提交而死锁。
	lineExecMu sync.Mutex
	lineExec   = map[string]bool{}
)

func ensureAccountWork(accountID string) chan *accountWork {
	workMu.Lock()
	defer workMu.Unlock()
	if q, ok := accountWorks[accountID]; ok {
		return q
	}
	q := make(chan *accountWork, 64)
	accountWorks[accountID] = q
	return q
}

func workQueue(accountID string) chan *accountWork {
	workMu.Lock()
	defer workMu.Unlock()
	return accountWorks[accountID]
}

func dropAccountWork(accountID string) {
	workMu.Lock()
	delete(accountWorks, accountID)
	workMu.Unlock()
}

// startAutomationForAccount 为账号启动自动化调度器（已存在则先停后起）
// 每账号只有【一个】串行调度器，绝不为同账号起多个并行 goroutine。
func startAutomationForAccount(accountID string) {
	// 账号未连接（离线/初始化失败）不启动自动化，避免“没在线还跑自动化/日志刷屏”。
	// worker 随 socket 连接存在，断开即停止。连接成功后由 connectLocked 触发启动。
	if clientPool.cached(accountID) == nil {
		return
	}
	automationMu.Lock()
	if r, ok := automationRunners[accountID]; ok {
		close(r.stop)
		delete(automationRunners, accountID)
	}
	stop := make(chan struct{})
	automationRunners[accountID] = &automationRunner{stop: stop}
	ensureAccountWork(accountID)
	automationMu.Unlock()

	go automationLoop(accountID, stop)
	log.Printf("[automation] 账号 %s 自动化已启动", accountID)
}

// stopAutomationForAccount 停止账号的全部自动化 goroutine 并清理账号相关的内存预留/重试缓存
func stopAutomationForAccount(accountID string) {
	automationMu.Lock()
	defer automationMu.Unlock()
	if r, ok := automationRunners[accountID]; ok {
		close(r.stop)
		delete(automationRunners, accountID)
		log.Printf("[automation] 账号 %s 自动化已停止", accountID)
	}

	// 清理该账号的 2x2 预留状态与重试缓存，防止内存泄漏
	reserved2x2Mu.Lock()
	delete(reserved2x2, accountID)
	delete(last2x2WaitKey, accountID)
	reserved2x2Mu.Unlock()

	failed2x2RetriesMu.Lock()
	delete(failed2x2Retries, accountID)
	failed2x2RetriesMu.Unlock()
}

// startAllAutomation 为所有已配置账号启动自动化（main 启动时调用）
func startAllAutomation() {
	for _, acc := range models.GetAccounts() {
		startAutomationForAccount(acc.ID)
	}
}

// ── 间隔工具 ──

func randomIntervalMs(minMs, maxMs int) time.Duration {
	if maxMs <= minMs {
		if minMs < 0 {
			minMs = 0
		}
		return time.Duration(minMs) * time.Millisecond
	}
	return time.Duration(minMs+rand.Intn(maxMs-minMs+1)) * time.Millisecond
}

// farmInterval 农场巡查间隔（秒→毫秒）默认 2–5s
// nextHelpTime 帮忙通道下次运行时刻：极速务农开启时同样读前端 HelpMin/Max 配置（turbo 快照也读前端）
func nextHelpTime(iv config.IntervalsConfig, turbo bool) time.Time {
	if turbo {
		// 极速务农专注抢帮：固定 1.5s 快轮（比普通 helpInterval 更快，配合只跑护主犬独占模式）
		return time.Now().Add(1500 * time.Millisecond)
	}
	return time.Now().Add(helpInterval(iv))
}

func farmInterval(iv config.IntervalsConfig) time.Duration {
	minS, maxS := iv.FarmMin, iv.FarmMax
	if minS <= 0 {
		minS = iv.Farm
	}
	if minS <= 0 {
		minS = 2
	}
	if maxS <= 0 {
		maxS = minS
	}
	if maxS < minS {
		maxS = minS
	}
	return randomIntervalMs(minS*1000, maxS*1000)
}

// stealInterval 偷菜巡查间隔（秒→毫秒）读前端设置的 StealMin/Max
func stealInterval(iv config.IntervalsConfig) time.Duration {
	minS, maxS := iv.StealMin, iv.StealMax
	if minS <= 0 {
		minS = 10
	}
	if maxS <= 0 {
		maxS = 15
	}
	if maxS < minS {
		maxS = minS
	}
	return randomIntervalMs(minS*1000, maxS*1000)
}

// helpInterval 帮忙巡查间隔（秒→毫秒）读前端设置的 HelpMin/Max
func helpInterval(iv config.IntervalsConfig) time.Duration {
	minS, maxS := iv.HelpMin, iv.HelpMax
	if minS <= 0 {
		minS = 15
	}
	if maxS <= 0 {
		maxS = 20
	}
	if maxS < minS {
		maxS = minS
	}
	return randomIntervalMs(minS*1000, maxS*1000)
}

// ── 单账号统一串行调度器 ──

// automationLoop 每个账号仅一个 goroutine：把农场巡田、帮忙、偷菜、买化肥统一到【一条串行时间线】。
// 参考 Node runUnifiedTick —— 一次 tick 内按 farm→help→steal 顺序 await 执行，绝不并发；
// scheduleUnifiedNextTick —— sleep 到最近到期时刻再驱动下一次 tick。
func automationLoop(accountID string, stop chan struct{}) {
	getCfg := func() config.AccountConfig { return models.GetAccountConfig(accountID) }

	// 各任务下次运行时刻
	nextFarm := time.Now().Add(farmInterval(getCfg().Intervals))
	nextSteal := time.Now().Add(stealInterval(getCfg().Intervals))
	nextHelp := nextHelpTime(getCfg().Intervals, getCfg().Automation.FriendTurboMode)
	// 买化肥首跑延迟 30s，此后按配置间隔
	nextBuy := time.Now().Add(30 * time.Second)
	// 自动做任务首跑延迟 15s，此后每 30s 周期扫描领取
	nextTask := time.Now().Add(15 * time.Second)
	// 每日例行（登录后立即执行一次 + 跨天检测再执行，
	// 独立于任务开关 runDailyRoutinesGo 内部按日期防重）——lastDailyDate 初始为空 → 首轮立即执行
	lastDailyDate := ""
	// 游戏网络心跳；并入统一串行线，不再独立 goroutine
	nextHb := time.Now().Add(gwHeartbeatInterval)
	hbMiss := 0

	appendOpLog(accountID, "系统", "自动化循环已启动")

	for {
		// 与自动化共用同一条串行执行线：驱动 ACE 周期任务
		if ace := getAceService(accountID); ace != nil {
			ace.tick(time.Now())
		}
		cfg := getCfg()
		now := time.Now()

		// 极速务农(turbo)：，
		// 只跑「护主犬循环 + 心跳/ACE」，避免其它巡查抢占 WS 使心跳被淹没导致假断连
		turbo := computeEffectiveTurbo(cfg)
		shouldFarm := cfg.Automation.Farm && !turbo && now.After(nextFarm)
		shouldSteal := cfg.Automation.Friend && cfg.Automation.FriendSteal && !turbo && now.After(nextSteal)
		shouldHelp := cfg.Automation.Friend &&
			(cfg.Automation.FriendHelp || cfg.Automation.FriendBad || cfg.Automation.FriendGoldenBug || turbo) &&
			now.After(nextHelp)
		shouldBuy := (cfg.Automation.FertilizerBuyOrganic || cfg.Automation.FertilizerBuyNormal) && !turbo && now.After(nextBuy)
		shouldTask := cfg.Automation.Task && now.After(nextTask)
		shouldHb := now.After(nextHb)
		// 每日例行：登录后首轮立即执行（lastDailyDate 为空）此后仅跨天执行
		shouldDaily := lastDailyDate != todayKey()

		// 无到期任务：睡到最近到期时刻再驱动一次 tick
		if !shouldFarm && !shouldSteal && !shouldHelp && !shouldBuy && !shouldTask && !shouldHb && !shouldDaily {
			nearest := nextFarm
			if nextSteal.Before(nearest) {
				nearest = nextSteal
			}
			if nextHelp.Before(nearest) {
				nearest = nextHelp
			}
			if nextBuy.Before(nearest) {
				nearest = nextBuy
			}
			if nextTask.Before(nearest) {
				nearest = nextTask
			}
			if nextHb.Before(nearest) {
				nearest = nextHb
			}
			// ACE 周期任务也纳入最近到期计算，保证准点驱动
			if ace := getAceService(accountID); ace != nil {
				if an := ace.nearestTick(); !an.IsZero() && an.Before(nearest) {
					nearest = an
				}
			}
			wait := nearest.Sub(now)
			if wait < 50*time.Millisecond {
				wait = 50 * time.Millisecond
			}
			// 空闲期同时监听前端投递的账号操作，在唯一串行线上执行
			q := workQueue(accountID)
			t := time.NewTimer(wait)
			select {
			case <-stop:
				t.Stop()
				return
			case w := <-q:
				t.Stop()
				lineExecMu.Lock()
				lineExec[accountID] = true
				lineExecMu.Unlock()
				w.fn()
				lineExecMu.Lock()
				lineExec[accountID] = false
				lineExecMu.Unlock()
				close(w.done)
				continue
			case <-t.C:
				continue
			}
		}

		// 统一 tick：按序【串行】执行，同账号绝不并发
		c, err := clientPool.Get(accountID)
		if err != nil || c == nil {
			// 连接不可用：各任务整体顺延一轮，避免忙等
			nextFarm = now.Add(farmInterval(getCfg().Intervals))
			nextSteal = now.Add(stealInterval(getCfg().Intervals))
			nextHelp = now.Add(helpInterval(getCfg().Intervals))
			continue
		}

		if shouldHb {
			if c.HeartbeatOnce() {
				hbMiss = 0
			} else {
				hbMiss++
				if hbMiss >= hbMissLimit {
					c.Close()
				}
			}
			nextHb = time.Now().Add(gwHeartbeatInterval)
		}

		if shouldFarm {
			if tryLockFarm(accountID) {
				runFarmOnce(accountID, c, cfg)
				unlockFarm(accountID)
			}
			// 化肥礼包并入 farm tick
			// 每日一次由 runFertilizerGiftOnce 内部防重
			if cfg.Automation.FertilizerGift {
				runFertilizerGiftOnce(accountID, c)
			}
			nextFarm = time.Now().Add(farmInterval(getCfg().Intervals))
		}
		if shouldSteal {
			stolen := checkFriends(c, accountID, cfg, true, false)
			if stolen > 0 {
				nextSteal = time.Now().Add(rapidStealInterval) // 本轮偷到 → 1s 快扫
			} else {
				nextSteal = time.Now().Add(stealInterval(getCfg().Intervals))
			}
		}
		if shouldHelp {
			checkFriends(c, accountID, cfg, false, true)
			nextHelp = nextHelpTime(getCfg().Intervals, cfg.Automation.FriendTurboMode)
		}
		if shouldBuy {
			doCheckAndBuyFertilizer(accountID, c, cfg)
			nextBuy = time.Now().Add(fetchBuyInterval(accountID))
		}
		if shouldTask {
			runTaskAuto(accountID, c)
			nextTask = time.Now().Add(30 * time.Second)
		}
		// 每日例行（独立于任务开关）——放在 task 之后执行
		if shouldDaily {
			runDailyRoutinesGo(accountID, c)
			lastDailyDate = todayKey()
		}
	}
}

// fetchBuyInterval 自动买化肥间隔
func fetchBuyInterval(accountID string) time.Duration {
	ivMin := models.GetAccountConfig(accountID).FertilizerBuyCheckIntervalMinutes
	if ivMin <= 0 {
		ivMin = 60
	}
	return time.Duration(ivMin) * time.Minute
}

// tryLockFarm 尝试占用指定账号的 farm 执行权（成功返回 true 并加锁；已被占用则跳过）
func tryLockFarm(accountID string) bool {
	farmBusyMu.Lock()
	defer farmBusyMu.Unlock()
	if farmBusySet[accountID] {
		return false
	}
	farmBusySet[accountID] = true
	return true
}

// unlockFarm 释放指定账号的 farm 执行权
func unlockFarm(accountID string) {
	farmBusyMu.Lock()
	delete(farmBusySet, accountID)
	farmBusyMu.Unlock()
}

// inQuietHours 检查是否处于好友静默时段
func inQuietHours(cfg config.AccountConfig) bool {
	qh := cfg.FriendQuietHours
	if !qh.Enabled || qh.Start == "" || qh.End == "" {
		return false
	}
	// 统一按北京时间判断（与离线通知、日报调度一致），避免服务器时区不是东八区时静默时段整体偏移
	now := time.Now().In(time.FixedZone("CST", 8*3600))
	startMin := parseTimeToMinutes(qh.Start)
	endMin := parseTimeToMinutes(qh.End)
	if startMin < 0 || endMin < 0 {
		return false
	}
	curMin := now.Hour()*60 + now.Minute()
	if startMin == endMin {
		return true // Full-day quiet
	}
	if startMin < endMin {
		return curMin >= startMin && curMin < endMin
	}
	// Crosses midnight (e.g. 23:00 - 07:30)
	return curMin >= startMin || curMin < endMin
}

// parseTimeToMinutes 解析 HH:MM 格式的时间字符串转为分钟数
func parseTimeToMinutes(s string) int {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return -1
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil {
		return -1
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil {
		return -1
	}
	return h*60 + m
}

// newFarmPushHandler 构建农场推送触发巡田处理器：
// 收到 LandsNotify 后 500ms 去抖，再延迟 1s 执行 runFarmOnce；若 farm 正在执行则跳过。
func newFarmPushHandler(accountID string) func(string) {
	last := time.Now().Add(-time.Hour)
	var mu sync.Mutex
	return func(_ string) {
		now := time.Now()
		mu.Lock()
		if now.Sub(last) < 500*time.Millisecond {
			mu.Unlock()
			return
		}
		last = now
		mu.Unlock()
		time.AfterFunc(1*time.Second, func() {
			// 推送巡田同样受「自动种植收获」与「推送触发巡田」两个开关约束，
			// 否则关闭采收后手动催熟/作物成熟推送仍会触发整轮收获与补种
			cfg := models.GetAccountConfig(accountID)
			if !cfg.Automation.FarmPush || !cfg.Automation.Farm {
				return
			}
			if !tryLockFarm(accountID) {
				return
			}
			defer unlockFarm(accountID)
			c, err := clientPool.Get(accountID)
			if err != nil || c == nil {
				return
			}
			runFarmOnce(accountID, c, cfg)
		})
	}
}

// runFarmOnce 单次巡田（('all') 顺序）
func runFarmOnce(accountID string, c *gw.Client, cfg config.AccountConfig) {
	ctx := context.Background()
	rep, err := c.Request(ctx, plantService, "AllLands", proto.EncodeAllLandsRequest(0), 15*time.Second)
	if err != nil {
		return
	}
	lands := proto.DecodeAllLandsReply(rep.Body).Lands
	now := time.Now().Unix()
	a := analyzeFarmLands(lands, now)

	// 1. 一键务农（水/草/虫）
	var farmingIDs []int64
	farmingIDs = append(farmingIDs, a.needWater...)
	if !cfg.Automation.SkipOwnWeedBug {
		farmingIDs = append(farmingIDs, a.needWeed...)
		farmingIDs = append(farmingIDs, a.needBug...)
	}
	// GoldenBugClear：独立开关控制清除好友放置的黄金虫
	if cfg.Automation.GoldenBugClear {
		farmingIDs = append(farmingIDs, a.needGoldenBug...)
	}
	farmingIDs = dedupeInt64(farmingIDs)
	if len(farmingIDs) > 0 {
		if err := execFarmOp(c, "Farming", proto.EncodeFarmingRequest(farmingIDs, c.GID)); err == nil {
			recordOperation(accountID, "farming", int64(len(farmingIDs)))
			// 金虫单独统计（('goldenBugClear')）
			if len(a.needGoldenBug) > 0 {
				recordOperation(accountID, "goldenBugClear", int64(len(a.needGoldenBug)))
			}
			appendOpLog(accountID, "farm", fmt.Sprintf("一键务农 %d 块地", len(farmingIDs)))
		}
		time.Sleep(200 * time.Millisecond)
	}

	// 2. 收获
	if len(a.harvestable) > 0 {
		if err := execFarmOp(c, "Harvest", proto.EncodeHarvestRequest(a.harvestable, c.GID, true)); err == nil {
			recordOperation(accountID, "harvest", int64(len(a.harvestable)))
			appendOpLog(accountID, "farm", fmt.Sprintf("收获 %d 块地", len(a.harvestable)))
			if cfg.Automation.Sell {
				autoSellAfterHarvest(accountID, c)
			}
		}
		time.Sleep(200 * time.Millisecond)
	}

	// 2.5 重新拉取农场地块，刷新枯死/空地/刚收获变空的地
	// 收获后 mature 地块变空，需并入种植目标；dead/empty 也在本次快照内重算
	if len(a.harvestable) > 0 || len(a.dead) > 0 || len(a.empty) > 0 {
		if rep2, e2 := c.Request(ctx, plantService, "AllLands", proto.EncodeAllLandsRequest(0), 15*time.Second); e2 == nil {
			lands = proto.DecodeAllLandsReply(rep2.Body).Lands
			a = analyzeFarmLands(lands, time.Now().Unix())
		}
	}

	// 3. 解锁 + 升级（空金土地升紫必须在种植之前，否则新作物会占住地块）
	if cfg.Automation.LandUpgrade {
		landChanged := false
		for _, id := range a.couldUnlock {
			if err := execFarmOp(c, "UnlockLand", proto.EncodeUnlockLandRequest(id, false)); err == nil {
				landChanged = true
				appendOpLog(accountID, "farm", fmt.Sprintf("解锁土地 %d", id))
			} else {
				appendOpLog(accountID, "farm", fmt.Sprintf("解锁土地 %d 失败: %v", id, err))
			}
			time.Sleep(200 * time.Millisecond)
		}
		for _, id := range a.couldUpgrade {
			if err := execFarmOp(c, "UpgradeLand", proto.EncodeUpgradeLandRequest(id)); err == nil {
				landChanged = true
				recordOperation(accountID, "upgrade", 1)
				appendOpLog(accountID, "farm", fmt.Sprintf("升级土地 %d", id))
			} else {
				appendOpLog(accountID, "farm", fmt.Sprintf("升级土地 %d 失败: %v", id, err))
			}
			time.Sleep(200 * time.Millisecond)
		}
		if landChanged {
			if rep3, e3 := c.Request(ctx, plantService, "AllLands", proto.EncodeAllLandsRequest(0), 15*time.Second); e3 == nil {
				lands = proto.DecodeAllLandsReply(rep3.Body).Lands
				a = analyzeFarmLands(lands, time.Now().Unix())
			}
		}
	}

	// 4. 种植（枯死 + 空地）
	plantTargets := append([]int64{}, a.dead...)
	plantTargets = append(plantTargets, a.empty...)
	plantTargets = dedupeInt64(plantTargets)
	if len(plantTargets) > 0 {
		autoPlantLands(accountID, c, cfg, lands, plantTargets)
		time.Sleep(300 * time.Millisecond)
	}

	// 5. 多季补肥，reason=multi_season
	if cfg.Automation.FertilizerMultiSeason && cfg.Automation.Fertilizer != "final_normal" && len(a.growing) > 0 {
		runFertilizerByConfig(accountID, c, cfg, a.growing, "multi_season", false)
	}

	// 6. 智能施肥（farm 循环内，runFertilizerByConfig([], {skipNormal:true})）
	mode := cfg.Automation.Fertilizer
	if mode == "smart" || mode == "smart_only" || mode == "smart_normal" || mode == "final_normal" || mode == "final_organic" {
		runFertilizerByConfig(accountID, c, cfg, nil, "", true)
	}
}

// farmAnalysis 地块分类，只处理已解锁且非从属地块
type farmAnalysis struct {
	needWater     []int64
	needWeed      []int64
	needBug       []int64
	needGoldenBug []int64
	harvestable   []int64
	dead          []int64
	empty         []int64
	growing       []int64
	couldUnlock   []int64
	couldUpgrade  []int64
}

func analyzeFarmLands(lands []*proto.LandInfo, now int64) farmAnalysis {
	var a farmAnalysis
	for _, l := range lands {
		if l == nil || l.ID <= 0 {
			continue
		}
		if !l.Unlocked {
			if l.CouldUnlock {
				a.couldUnlock = append(a.couldUnlock, l.ID)
			}
			continue
		}
		// 从属土地（2x2 的 slave）由 master 统一处理，跳过单独操作
		if l.MasterLandID > 0 {
			continue
		}
		if l.Plant == nil || len(l.Plant.Phases) == 0 {
			a.empty = append(a.empty, l.ID)
			if l.CouldUpgrade {
				a.couldUpgrade = append(a.couldUpgrade, l.ID)
			}
			continue
		}
		ph := currentPhase(l.Plant.Phases, now)
		if ph == nil {
			a.empty = append(a.empty, l.ID)
			if l.CouldUpgrade {
				a.couldUpgrade = append(a.couldUpgrade, l.ID)
			}
			continue
		}
		switch ph.Phase {
		case proto.PhaseDead:
			a.dead = append(a.dead, l.ID)
			continue
		case proto.PhaseMature:
			a.harvestable = append(a.harvestable, l.ID)
			continue
		}
		// growing
		a.growing = append(a.growing, l.ID)
		if l.Plant.DryNum > 0 || (ph.DryTime > 0 && ph.DryTime <= now) {
			a.needWater = append(a.needWater, l.ID)
		}
		if len(l.Plant.WeedOwners) > 0 || (ph.WeedsTime > 0 && ph.WeedsTime <= now) {
			a.needWeed = append(a.needWeed, l.ID)
		}
		if len(l.Plant.InsectOwners) > 0 || (ph.InsectTime > 0 && ph.InsectTime <= now) {
			a.needBug = append(a.needBug, l.ID)
		}
		// 黄金虫判定：好友放置到作物上的社交金虫（plantpb.proto social_items 字段35）
		// item_id==301101 && type==2
		if hasGoldenBug(l.Plant) {
			a.needGoldenBug = append(a.needGoldenBug, l.ID)
		}
	}
	return a
}

// hasGoldenBug 判断作物上是否有好友放置的黄金虫
func hasGoldenBug(p *proto.PlantInfo) bool {
	if p == nil {
		return false
	}
	for _, si := range p.SocialItems {
		if si != nil && si.Type == 2 && si.ID == 301101 {
			return true
		}
	}
	return false
}

const farmGridCols = 4

type land2x2Group struct {
	master int64
	ids    []int64
}

func farmGridRows(landByID map[int64]*proto.LandInfo) int {
	maxID := int64(0)
	for id := range landByID {
		if id > maxID {
			maxID = id
		}
	}
	if maxID <= 0 {
		return 6
	}
	rows := int((maxID + int64(farmGridCols) - 1) / int64(farmGridCols))
	if rows < 2 {
		return 2
	}
	return rows
}

func landHasLivePlant(l *proto.LandInfo) bool {
	if l == nil || l.Plant == nil || len(l.Plant.Phases) == 0 {
		return false
	}
	ph := currentPhase(l.Plant.Phases, time.Now().Unix())
	return ph != nil && ph.Phase != proto.PhaseDead
}

// isLockedPlantError 判断种植错误是否为等级未解锁/锁定。
// 这类错误表示该种子当前不可种，应清零该种子数量，避免死循环重试或阻塞地块。
func isLockedPlantError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "锁定") ||
		strings.Contains(msg, "未解锁") ||
		strings.Contains(msg, "等级不足") ||
		strings.Contains(msg, "level") ||
		strings.Contains(msg, "lock") ||
		strings.Contains(msg, "unlock")
}

// build2x2LandGroups 构建所有合法 2x2 组合（4 列网格、左下角 master 锚点）。
func build2x2LandGroups(landByID map[int64]*proto.LandInfo) []land2x2Group {
	rows := farmGridRows(landByID)
	var groups []land2x2Group
	for bottomRow := 1; bottomRow < rows; bottomRow++ {
		for column := 0; column < farmGridCols-1; column++ {
			master := int64(bottomRow*farmGridCols + column + 1)
			ids := []int64{
				master,
				master + 1,
				master - int64(farmGridCols),
				master - int64(farmGridCols) + 1,
			}
			ok := true
			for _, id := range ids {
				l := landByID[id]
				if l == nil || !l.Unlocked || l.MasterLandID > 0 {
					ok = false
					break
				}
			}
			if ok {
				groups = append(groups, land2x2Group{master: master, ids: ids})
			}
		}
	}
	return groups
}

func inferEmpty2x2IDs(lid int64, landByID map[int64]*proto.LandInfo) []int64 {
	if lid <= 0 {
		return nil
	}
	col := int((lid - 1) % int64(farmGridCols))
	row := int((lid - 1) / int64(farmGridCols))
	if col >= farmGridCols-1 || row < 1 {
		return nil
	}
	ids := []int64{
		lid,
		lid + 1,
		lid - int64(farmGridCols),
		lid - int64(farmGridCols) + 1,
	}
	for _, id := range ids {
		l := landByID[id]
		if l == nil || !l.Unlocked || l.MasterLandID > 0 {
			return nil
		}
		if landHasLivePlant(l) {
			return nil
		}
	}
	return ids
}

func selectReady2x2Groups(groups []land2x2Group, landByID map[int64]*proto.LandInfo, seen map[int64]bool) (ready, waiting []land2x2Group) {
	occupied := map[int64]bool{}
	for id, v := range seen {
		if v {
			occupied[id] = true
		}
	}
	for _, g := range groups {
		blocked := false
		for _, id := range g.ids {
			if occupied[id] {
				blocked = true
				break
			}
		}
		if blocked {
			continue
		}
		empty := 0
		valid := true
		for _, id := range g.ids {
			l := landByID[id]
			if l == nil || !l.Unlocked || l.MasterLandID > 0 {
				valid = false
				break
			}
			if landHasLivePlant(l) {
				continue
			}
			empty++
		}
		if !valid {
			continue
		}
		if empty == 4 {
			ready = append(ready, g)
			for _, id := range g.ids {
				occupied[id] = true
			}
			continue
		}
		waiting = append(waiting, g)
	}
	return ready, waiting
}

// selectMaxNonOverlappingGroups 在候选组中挑选数量最多的互不重叠组合（回溯搜索）。
// 贪心从左到右选择在规则矩形上等价，但在地块不连续/部分占用时会少选，
// 导致本可合种的地块退化成 1x1 普通种植。
func selectMaxNonOverlappingGroups(groups []land2x2Group, limit int) []land2x2Group {
	if len(groups) == 0 {
		return nil
	}
	if limit <= 0 || limit > len(groups) {
		limit = len(groups)
	}
	var best []land2x2Group
	var search func(idx int, selected []land2x2Group, occupied map[int64]bool)
	search = func(idx int, selected []land2x2Group, occupied map[int64]bool) {
		if len(selected) > len(best) {
			best = append([]land2x2Group(nil), selected...)
		}
		if len(selected) >= limit || idx >= len(groups) {
			return
		}
		if len(selected)+len(groups)-idx <= len(best) {
			return
		}
		g := groups[idx]
		blocked := false
		for _, id := range g.ids {
			if occupied[id] {
				blocked = true
				break
			}
		}
		if !blocked {
			next := make(map[int64]bool, len(occupied)+len(g.ids))
			for k := range occupied {
				next[k] = true
			}
			for _, id := range g.ids {
				next[id] = true
			}
			search(idx+1, append(selected, g), next)
		}
		search(idx+1, selected, occupied)
	}
	search(0, nil, map[int64]bool{})
	return best
}

// total2x2SeedCount 统计本组可用四格种子的总颗数，用于限制 2x2 组数量：
// 与官方一致，种子不够时不预留地块，剩余空地交由普通种植填充。
func total2x2SeedCount(seeds []bagSeedItem) int64 {
	var n int64
	for _, s := range seeds {
		if s.count > 0 {
			n += s.count
		}
	}
	return n
}

// reservedLandSet 返回指定账号已预留 2x2 单元的所有土地 ID 集合。
func reservedLandSet(accountID string) map[int64]bool {
	reserved2x2Mu.Lock()
	defer reserved2x2Mu.Unlock()
	m := make(map[int64]bool)
	if accountID == "" {
		for _, groups := range reserved2x2 {
			for _, ids := range groups {
				for _, id := range ids {
					m[id] = true
				}
			}
		}
		return m
	}
	for _, ids := range reserved2x2[accountID] {
		for _, id := range ids {
			m[id] = true
		}
	}
	return m
}

func dedupeInt64(in []int64) []int64 {
	if len(in) == 0 {
		return in
	}
	seen := map[int64]bool{}
	out := make([]int64, 0, len(in))
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// ── 种植 ──

// plantSummary 统计单轮自动种植的实际结果（2x2组数、普通块数）
type plantSummary struct {
	Count2x2   int // 成功种植的 2x2 合种组数（每组4块地）
	Count1x1   int // 成功种植的普通单地块数
	TotalLands int // 参与种植的地块总数 (Count2x2*4 + Count1x1)
}

// autoPlantLands 在给定空地/枯死地上种植（targetLandIDs 为 master/standalone 地块）
//  1. 枯死先铲除；2) 2x2 优先（用背包四格种子预留并种植）；
//  3. bag_priority 按背包种子顺序消耗（并按地块品质拆分）其余/剩余走第二优先策略；
//  4. 其余策略走商城购买种植。
func autoPlantLands(accountID string, c *gw.Client, cfg config.AccountConfig, lands []*proto.LandInfo, targetLandIDs []int64) plantSummary {
	var summary plantSummary
	landByID := map[int64]*proto.LandInfo{}
	for _, l := range lands {
		if l != nil {
			landByID[l.ID] = l
		}
	}
	// 枯死先铲，并清本地 Plant，否则后续无法把四块空地推断成 2x2
	for _, id := range targetLandIDs {
		l := landByID[id]
		if l == nil || l.Plant == nil || len(l.Plant.Phases) == 0 {
			continue
		}
		if ph := currentPhase(l.Plant.Phases, time.Now().Unix()); ph != nil && ph.Phase == proto.PhaseDead {
			ids := []int64{id}
			if l.LandSize > 1 && len(l.SlaveLandIDs) > 0 {
				ids = append(ids, l.SlaveLandIDs...)
			}
			_ = execFarmOp(c, "RemovePlant", proto.EncodeRemovePlantRequest(ids))
			for _, lid := range ids {
				if ll := landByID[lid]; ll != nil {
					ll.Plant = nil
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	type unit struct {
		master int64
		ids    []int64
		is2x2  bool
	}
	var units []unit
	seen := map[int64]bool{}
	for _, id := range targetLandIDs {
		if seen[id] {
			continue
		}
		l := landByID[id]
		if l == nil {
			continue
		}
		if l.LandSize > 1 && len(l.SlaveLandIDs) > 0 {
			ids := append([]int64{id}, l.SlaveLandIDs...)
			for _, s := range ids {
				seen[s] = true
			}
			units = append(units, unit{master: id, ids: ids, is2x2: true})
		}
	}

	var usable2x2 []bagSeedItem
	if cfg.Prioritize2x2Crops {
		seeds, seedsErr := listBagSeeds(accountID, c, cfg, 2)
		if seedsErr != nil {
			appendOpLog(accountID, "farm", "2x2 背包读取失败: "+seedsErr.Error())
		}
		// 背包内种子不做等级/解锁校验：有货即可种植，等级门槛交由服务端裁决。
		for _, s := range seeds {
			if s.count <= 0 || s.seedID <= 0 {
				continue
			}
			usable2x2 = append(usable2x2, s)
		}
		if len(usable2x2) == 0 {
			appendOpLog(accountID, "farm", "2x2 优先开启但背包无可用四格种子")
		} else {
			names := make([]string, 0, len(usable2x2))
			for _, s := range usable2x2 {
				names = append(names, fmt.Sprintf("%s×%d", seedPlantName(s.seedID), s.count))
			}
			appendOpLog(accountID, "farm", "2x2 背包种子: "+strings.Join(names, "、"))
			groups := build2x2LandGroups(landByID)
			ready, waiting := selectReady2x2Groups(groups, landByID, seen)
			// 与官方一致：2x2 组数上限 = 可用四格种子颗数。
			// 超出种子数的就绪组不预留、不占位，直接留给普通种植填充，
			// 避免"种子不够却预留一堆空地"或反过来"有种子却只合种少量"。
			limit := int(total2x2SeedCount(usable2x2))
			selected := selectMaxNonOverlappingGroups(ready, limit)
			appendOpLog(accountID, "farm", fmt.Sprintf("2x2 选组: 构建%d 就绪%d 等待%d 上限%d 选中%d",
				len(groups), len(ready), len(waiting), limit, len(selected)))
			for _, g := range selected {
				for _, id := range g.ids {
					seen[id] = true
				}
				units = append(units, unit{master: g.master, ids: g.ids, is2x2: true})
			}
			for _, g := range waiting {
				overlap := false
				for _, id := range g.ids {
					if seen[id] {
						overlap = true
						break
					}
				}
				if overlap {
					continue
				}
				groupKey := fmt.Sprintf("2x2:%d", g.master)
				reserved2x2Mu.Lock()
				if reserved2x2[accountID] == nil {
					reserved2x2[accountID] = map[string][]int64{}
				}
				reserved2x2[accountID][groupKey] = g.ids
				reserved2x2Mu.Unlock()
				for _, id := range g.ids {
					seen[id] = true
				}
				if last2x2WaitKey[accountID] != groupKey {
					appendOpLog(accountID, "farm", fmt.Sprintf("2x2预留 等待地块%v", g.ids))
					last2x2WaitKey[accountID] = groupKey
				}
				break
			}
		}
	}
	for _, id := range targetLandIDs {
		if seen[id] {
			continue
		}
		l := landByID[id]
		if l == nil {
			continue
		}
		seen[id] = true
		units = append(units, unit{master: id, ids: []int64{id}})
	}
	if len(units) == 0 {
		return summary
	}

	strategy := cfg.PlantingStrategy
	if strategy == "" {
		strategy = "level"
	}

	if cfg.Prioritize2x2Crops && len(usable2x2) > 0 {
		var remainUnits []unit
		tryPlant2x2 := func(ids []int64) bool {
			if len(ids) != 4 {
				return false
			}
			for i := range usable2x2 {
				s := &usable2x2[i]
				if s.count <= 0 {
					continue
				}
				retryKey := fmt.Sprintf("2x2:%v:%d", ids, s.seedID)
				failed2x2RetriesMu.Lock()
				if accRetries, ok := failed2x2Retries[accountID]; ok {
					if retryUntil, hasRetry := accRetries[retryKey]; hasRetry && time.Now().Unix() < retryUntil {
						failed2x2RetriesMu.Unlock()
						continue
					}
				}
				failed2x2RetriesMu.Unlock()
				realSeed, e2 := ensureSeedOwned(c, s.seedID, 0, 0, 1)
				if e2 != nil || realSeed <= 0 {
					appendOpLog(accountID, "farm", fmt.Sprintf("2x2 种子未就绪 seed=%d: %v", s.seedID, e2))
					continue
				}
				plantErr := execFarmOp(c, "Plant", proto.EncodePlantRequest(realSeed, ids))
				if plantErr == nil {
					s.count--
					recordOperation(accountID, "plant", int64(len(ids)))
					appendOpLog(accountID, "farm", fmt.Sprintf("2x2 种植%s → 地块%v", seedPlantName(realSeed), ids))
					time.Sleep(plantDelay(cfg) + 200*time.Millisecond)
					return true
				}
				if isLockedPlantError(plantErr) {
					appendOpLog(accountID, "farm", fmt.Sprintf("2x2 作物%s 未解锁/锁定，跳过: %v", seedPlantName(realSeed), plantErr))
					s.count = 0
					continue
				}
				failed2x2RetriesMu.Lock()
				if failed2x2Retries[accountID] == nil {
					failed2x2Retries[accountID] = map[string]int64{}
				}
				failed2x2Retries[accountID][retryKey] = time.Now().Unix() + 90
				failed2x2RetriesMu.Unlock()
				appendOpLog(accountID, "farm", fmt.Sprintf("2x2 种植失败 seed=%d lands=%v: %v", realSeed, ids, plantErr))
			}
			return false
		}
		for _, u := range units {
			if !u.is2x2 {
				remainUnits = append(remainUnits, u)
				continue
			}
			groupKey := fmt.Sprintf("2x2:%d", u.master)
			if tryPlant2x2(u.ids) {
				summary.Count2x2++
				summary.TotalLands += len(u.ids)
				reserved2x2Mu.Lock()
				if reserved2x2[accountID] != nil {
					delete(reserved2x2[accountID], groupKey)
				}
				reserved2x2Mu.Unlock()
				continue
			}
			// 种植未成功且没有正在重试的种子时，释放该组预留，回流给后续普通种植，
			// 避免因某颗种子无法种下导致整整 4 块地被永久卡死为空地
			hasRetry := false
			nowUnix := time.Now().Unix()
			failed2x2RetriesMu.Lock()
			if accRetries, ok := failed2x2Retries[accountID]; ok {
				for _, s := range usable2x2 {
					if s.count > 0 {
						rKey := fmt.Sprintf("2x2:%v:%d", u.ids, s.seedID)
						if t, ok := accRetries[rKey]; ok && nowUnix < t {
							hasRetry = true
							break
						}
					}
				}
			}
			failed2x2RetriesMu.Unlock()
			if hasRetry {
				reserved2x2Mu.Lock()
				if reserved2x2[accountID] == nil {
					reserved2x2[accountID] = map[string][]int64{}
				}
				reserved2x2[accountID][groupKey] = u.ids
				reserved2x2Mu.Unlock()
			} else {
				reserved2x2Mu.Lock()
				if reserved2x2[accountID] != nil {
					delete(reserved2x2[accountID], groupKey)
				}
				reserved2x2Mu.Unlock()
				// 将该 2x2 组的 4 块地作为独立单地回流，让普通种植能够种上
				for _, id := range u.ids {
					remainUnits = append(remainUnits, unit{master: id, ids: []int64{id}})
				}
			}
		}
		units = remainUnits
	}

	// bag_priority：先按背包种子顺序消耗；按地块品质拆分
	if strategy == "bag_priority" {
		reserved := reservedLandSet(accountID)
		allowed := normalizeFertilizerLandTypes(cfg.BagPriorityLandTypes) // 5 种品质，空集/全选=不限制
		unrestricted := len(allowed) == 0 || len(allowed) >= 5
		var prefMasters, otherMasters []int64
		if !unrestricted {
			allowedSet := map[string]bool{}
			for _, t := range allowed {
				allowedSet[t] = true
			}
			for _, u := range units {
				if reserved[u.master] {
					continue
				}
				lt := landTypeByLevel(landByID[u.master].Level)
				if allowedSet[lt] {
					prefMasters = append(prefMasters, u.master)
				} else {
					otherMasters = append(otherMasters, u.master)
				}
			}
		} else {
			for _, u := range units {
				if reserved[u.master] {
					continue
				}
				prefMasters = append(prefMasters, u.master)
			}
		}
		remaining, fallbackAllowed, bagPlanted, e := plantBagSeedsForLands(accountID, c, cfg, prefMasters)
		summary.Count1x1 += bagPlanted
		summary.TotalLands += bagPlanted
		if e != nil {
			appendOpLog(accountID, "farm", "背包种子读取失败，跳过本轮: "+e.Error())
			return summary
		}
		fallbackLands := append([]int64{}, otherMasters...)
		if fallbackAllowed {
			fallbackLands = append(fallbackLands, remaining...)
		}
		if len(fallbackLands) > 0 {
			fb := cfg.BagSeedFallbackStrategy
			if fb == "" {
				fb = "level"
			}
			shopPlanted := plantFromShopLands(accountID, c, cfg, fallbackLands, fb)
			summary.Count1x1 += shopPlanted
			summary.TotalLands += shopPlanted
		}
		return summary
	}

	// 其余策略：商城购买种植
	reserved := reservedLandSet(accountID)
	var masters []int64
	for _, u := range units {
		if reserved[u.master] {
			continue
		}
		skip := false
		for _, id := range u.ids {
			if reserved[id] {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		masters = append(masters, u.master)
	}
	shopPlanted := plantFromShopLands(accountID, c, cfg, masters, "")
	summary.Count1x1 += shopPlanted
	summary.TotalLands += shopPlanted
	return summary
}

// plantDelay 返回种植延迟（默认 2s）
func plantDelay(cfg config.AccountConfig) time.Duration {
	d := time.Duration(cfg.PlantDelaySeconds) * time.Second
	if d <= 0 {
		return 2 * time.Second
	}
	return d
}

// autoPlantEmptyLands 手动"一键种植"：对当前农场所有空地/枯死地用种植策略自动选种种植
func autoPlantEmptyLands(accountID string, c *gw.Client, cfg config.AccountConfig) (plantSummary, error) {
	rep, err := c.Request(context.Background(), plantService, "AllLands",
		proto.EncodeAllLandsRequest(0), 15*time.Second)
	if err != nil {
		return plantSummary{}, err
	}
	lands := proto.DecodeAllLandsReply(rep.Body).Lands
	a := analyzeFarmLands(lands, time.Now().Unix())
	targets := append([]int64{}, a.dead...)
	targets = append(targets, a.empty...)
	targets = dedupeInt64(targets)
	if len(targets) == 0 {
		return plantSummary{}, nil
	}
	summary := autoPlantLands(accountID, c, cfg, lands, targets)
	return summary, nil
}

// plantOnLands 在指定地块种植指定种子
// 逐块传 [landId]，2x2 补全从属地，枯死地块先铲除，确保种子库存后种植）。
// 用于前端手动种植 / /api/farm/action action=plant。
func plantOnLands(accountID string, c *gw.Client, seedID int64, landIDs []int64) (int, error) {
	if len(landIDs) == 0 || seedID <= 0 {
		return 0, fmt.Errorf("invalid params")
	}
	rep, err := c.Request(context.Background(), plantService, "AllLands", proto.EncodeAllLandsRequest(0), 15*time.Second)
	if err != nil {
		return 0, err
	}
	lands := proto.DecodeAllLandsReply(rep.Body).Lands
	landByID := map[int64]*proto.LandInfo{}
	for _, l := range lands {
		if l != nil {
			landByID[l.ID] = l
		}
	}
	var fullIDs []int64
	for _, id := range landIDs {
		l := landByID[id]
		fullIDs = append(fullIDs, id)
		if l != nil && l.LandSize > 1 && len(l.SlaveLandIDs) > 0 {
			fullIDs = append(fullIDs, l.SlaveLandIDs...)
		}
	}
	fullIDs = dedupeInt64(fullIDs)
	// 枯死地块先铲除
	for _, id := range fullIDs {
		l := landByID[id]
		if l != nil && l.Plant != nil && len(l.Plant.Phases) > 0 {
			if ph := currentPhase(l.Plant.Phases, time.Now().Unix()); ph != nil && ph.Phase == proto.PhaseDead {
				_ = execFarmOp(c, "RemovePlant", proto.EncodeRemovePlantRequest([]int64{id}))
				time.Sleep(200 * time.Millisecond)
			}
		}
	}
	// 按 2x2 分组（主地+从属地 = 一次 Plant RPC，消耗 1 颗种子）
	type pg struct{ ids []int64 }
	var pgroups []pg
	seenG := map[int64]bool{}
	for _, id := range fullIDs {
		if seenG[id] {
			continue
		}
		l := landByID[id]
		g := pg{ids: []int64{id}}
		if l != nil && l.LandSize > 1 && len(l.SlaveLandIDs) > 0 {
			g.ids = append(g.ids, l.SlaveLandIDs...)
			for _, s := range l.SlaveLandIDs {
				seenG[s] = true
			}
		}
		seenG[id] = true
		pgroups = append(pgroups, g)
	}
	// 购买：每组 1 颗（2x2 一组消耗 1 颗，(landIds/footprint)）
	realSeed, err := ensureSeedOwned(c, seedID, 0, 0, len(pgroups))
	if err != nil || realSeed <= 0 {
		return 0, err
	}
	planted := 0
	for _, g := range pgroups {
		// 枯死地块先铲除（仅主地）
		if l := landByID[g.ids[0]]; l != nil && l.Plant != nil && len(l.Plant.Phases) > 0 {
			if ph := currentPhase(l.Plant.Phases, time.Now().Unix()); ph != nil && ph.Phase == proto.PhaseDead {
				_ = execFarmOp(c, "RemovePlant", proto.EncodeRemovePlantRequest([]int64{g.ids[0]}))
				time.Sleep(200 * time.Millisecond)
			}
		}
		if err := execFarmOp(c, "Plant", proto.EncodePlantRequest(realSeed, []int64{g.ids[0]})); err != nil {
			appendOpLog(accountID, "plant", fmt.Sprintf("手动种植 %d 失败: %v", realSeed, err))
			continue
		}
		planted += len(g.ids)
		recordOperation(accountID, "plant", int64(len(g.ids)))
	}
	appendOpLog(accountID, "plant", fmt.Sprintf("手动种植种子 %d → %d 块地", realSeed, planted))
	return planted, nil
}

// seedCand 商店候选种子
type seedCand struct {
	seedID  int64
	goodsID int64
	price   int64
	reqLvl  int
}

// eventSeeds 活动种子
// 活动种子只从背包种植（event_plant 模式）【禁止从商店购买】——商店候选需排除，
// 否则"最高经验/时"策略会选到活动种子 → 购买失败 → 一直卡在种植失败。
var eventSeeds = map[int64]bool{
	20224:   true, // 昙花
	20249:   true, // 荷包牡丹
	20025:   true, // 银杏树苗
	20109:   true, // 蝴蝶兰
	20112:   true, // 风信子
	20121:   true, // 蔷薇
	29003:   true, // 星语铃花（种子 ID，exp/h=640 排第一且 lvl=None 不会被等级过滤，商店买不到）
	49003:   true, // 星语铃花（物品/果实 ID 段，兜底）
	1049003: true, // 黄金·星语铃花（变异）
	20046:   true, // 爱心果（exp/h=640 限定种子，商店买不到）
	21032:   true, // 琉璃宝荷（exp/h=640 限定种子，商店买不到）
	20416:   true, // 哈哈南瓜（exp/h=640 限定种子，商店买不到）
	29998:   true, // 哈哈南瓜（活动变体）
	29004:   true, // 泡泡棉花糖（2x2 活动种子，商店买不到）
}

// failedBuySeeds 动态黑名单：购买失败（活动种子/不可购）的种子记入，后续轮次跳过候选。
// 活动种子会持续新增，静态黑名单追不上，购买失败即自动排除（自愈，无需维护清单）。
var failedBuySeeds = struct {
	sync.Mutex
	m map[int64]time.Time // seedID → 失败时间（当日有效）
}{m: map[int64]time.Time{}}

// markBuySeedFailed 记录一次购买失败（供 ensureSeedOwned 调用）
func markBuySeedFailed(seedID int64) {
	failedBuySeeds.Lock()
	failedBuySeeds.m[seedID] = time.Now()
	failedBuySeeds.Unlock()
}

// isBuySeedBlocked 判断种子是否在动态黑名单（当日有效；跨天自然解封，活动种子可能下架）
func isBuySeedBlocked(seedID int64) bool {
	failedBuySeeds.Lock()
	defer failedBuySeeds.Unlock()
	t, ok := failedBuySeeds.m[seedID]
	if !ok {
		return false
	}
	if time.Since(t) > 24*time.Hour {
		delete(failedBuySeeds.m, seedID)
		return false
	}
	return true
}

// bagSeedItem 背包种子（按 plantSize 过滤后用于种植）
type bagSeedItem struct {
	seedID int64
	count  int64
	reqLvl int
	prio   int
}

var errNoSeed = &seedErr{"no available seed"}

// errGoldShort 金币不足以购买所需种子。
// 调用方应据此跳过该组种植，而非当作致命错误。
var errGoldShort = errors.New("金币不足，跳过购买")

type seedErr struct{ msg string }

func (e *seedErr) Error() string { return e.msg }

// pickSeedForPlanting 按策略选种，返回 seedID / goodsID / price
// pickBagSeedByID 查背包指定种子是否有货（1x1），供"优先种植指定种子"策略使用。
func pickBagSeedByID(accountID string, c *gw.Client, seedID int64) (int64, bool) {
	rep, err := c.Request(context.Background(), "gamepb.itempb.ItemService", "Bag",
		proto.EncodeBagRequest(), 12*time.Second)
	if err != nil {
		return 0, false
	}
	for _, it := range proto.DecodeBagReply(rep.Body).Items {
		if it.ID != seedID || it.Count <= 0 || !isSeedItemID(it.ID) {
			continue
		}
		pe, ok := plantForSeed(int(it.ID))
		if !ok || pe.Size != 1 {
			continue
		}
		return it.ID, true
	}
	return 0, false
}

// findSeedCandidates 按策略从种子商店构建排序后的候选序列。
// 候选序列语义：按策略优先级降序排列，调用方逐个尝试——某个候选不可买/种失败时
// 继续尝试下一个，保证最终种上"当前策略下最合适的可买种子"。
func findSeedCandidates(c *gw.Client, cfg config.AccountConfig, strategy string) ([]seedCand, error) {
	shop, err := c.Request(context.Background(), "gamepb.shoppb.ShopService", "ShopInfo",
		proto.EncodeShopInfoRequest(2), 15*time.Second)
	if err != nil {
		return nil, err
	}
	level := c.Level()
	var cands []seedCand
	candMap := map[int64]seedCand{}
	for _, g := range proto.DecodeShopInfoReply(shop.Body).GoodsList {
		if !g.Unlocked || g.ItemID <= 0 {
			continue
		}
		if g.LimitCount > 0 && g.BoughtNum >= g.LimitCount {
			continue
		}
		if g.Price <= 0 {
			continue // price<=0 不入候选
		}
		reqLvl := 0
		for _, cd := range g.Conds {
			if cd.Type == 1 { // MIN_LEVEL
				reqLvl = int(cd.Param)
			}
		}
		if reqLvl > 0 && int(level) < reqLvl {
			continue
		}
		cc := seedCand{seedID: g.ItemID, goodsID: g.ID, price: g.Price, reqLvl: reqLvl}
		cands = append(cands, cc)
		candMap[g.ItemID] = cc
	}
	if len(cands) == 0 {
		return nil, errNoSeed
	}

	switch strategy {
	case "max_exp", "max_fert_exp", "max_profit", "max_fert_profit":
		sortSeedCandsByRanking(cands, candMap, strategy, level)
	case "preferred":
		sortSeedCandsByLevel(cands)
		if cfg.PreferredSeedID > 0 {
			for i, cc := range cands {
				if cc.seedID == int64(cfg.PreferredSeedID) {
					cands = append(cands[:i], append([]seedCand{cc}, cands[i+1:]...)...)
					break
				}
			}
		}
	default: // "level" 及未知策略
		sortSeedCandsByLevel(cands)
	}
	return cands, nil
}

// sortSeedCandsByLevel 按等级门槛降序、seedId 升序稳定排序（同等级选种结果确定）
func sortSeedCandsByLevel(cands []seedCand) {
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].reqLvl != cands[j].reqLvl {
			return cands[i].reqLvl > cands[j].reqLvl
		}
		return cands[i].seedID < cands[j].seedID
	})
}

// sortSeedCandsByRanking 按植物排行（exp/h 等）对商店候选排序：
// 排行越靠前越优先；不在排行中的候选按等级降序+seedId 升序兜底排最后。
// 商店买不到的种子本就进不了候选，排行脱节不会导致乱选。
func sortSeedCandsByRanking(cands []seedCand, candMap map[int64]seedCand, strategy string, userLevel int64) {
	key := "exp"
	switch strategy {
	case "max_fert_exp":
		key = "fert"
	case "max_profit":
		key = "profit"
	case "max_fert_profit":
		key = "fert_profit"
	}
	rank := map[int64]int{}
	idx := 0
	for _, r := range getPlantRankings(key) {
		seedID, _ := r["seedId"].(float64)
		lvl, _ := r["level"].(float64)
		if seedID <= 0 {
			continue
		}
		if _, ok := candMap[int64(seedID)]; !ok {
			continue
		}
		// 排行等级高于用户等级的种子不进排行序（交给等级兜底排序）
		if lvl > 0 && int64(lvl) > userLevel {
			continue
		}
		if _, dup := rank[int64(seedID)]; !dup {
			rank[int64(seedID)] = idx
			idx++
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		ri, okI := rank[cands[i].seedID]
		rj, okJ := rank[cands[j].seedID]
		switch {
		case okI && okJ:
			if ri != rj {
				return ri < rj
			}
		case okI:
			return true
		case okJ:
			return false
		}
		if cands[i].reqLvl != cands[j].reqLvl {
			return cands[i].reqLvl > cands[j].reqLvl
		}
		return cands[i].seedID < cands[j].seedID
	})
}

// ensureSeedOwned 背包种子不足时从商店购买。
// 返回值为实际应种植的种子 ID：购买成功时优先用 buyResult.get_items[0].id
// （部分礼包商品实际产出种子 ID 与 goods.item_id 不同）未购买（背包已有）时返回入参 seedID。
func ensureSeedOwned(c *gw.Client, seedID, goodsID, price int64, need int) (int64, error) {
	rep, err := c.Request(context.Background(), "gamepb.itempb.ItemService", "Bag",
		proto.EncodeBagRequest(), 12*time.Second)
	if err != nil {
		return 0, err
	}
	have := int64(0)
	for _, it := range proto.DecodeBagReply(rep.Body).Items {
		if it.ID == seedID {
			have += it.Count
		}
	}
	if have >= int64(need) {
		return seedID, nil // 背包已够，无需购买
	}
	buy := int64(need) - have
	if goodsID <= 0 || price <= 0 {
		shop, err := c.Request(context.Background(), "gamepb.shoppb.ShopService", "ShopInfo",
			proto.EncodeShopInfoRequest(2), 15*time.Second)
		if err != nil {
			return 0, err
		}
		for _, g := range proto.DecodeShopInfoReply(shop.Body).GoodsList {
			if g.ItemID == seedID {
				goodsID, price = g.ID, g.Price
				break
			}
		}
	}
	if goodsID <= 0 {
		return 0, errNoSeed
	}
	// 金币预检。
	// Go 按组种植，每个 ensureSeedOwned 只买 need 颗（通常为 1）；单价高于余额或总价超余额时跳过该组，
	// 不再直接发起购买（避免无谓失败与误扣）。
	if price > 0 && c.Gold() > 0 {
		if price > c.Gold() {
			return 0, errGoldShort
		}
		if price*buy > c.Gold() {
			affordable := c.Gold() / price
			if affordable <= 0 {
				return 0, errGoldShort
			}
			buy = affordable
		}
	}
	brep, err := c.Request(context.Background(), "gamepb.shoppb.ShopService", "BuyGoods",
		proto.EncodeBuyGoodsRequest(goodsID, buy, price), 12*time.Second)
	if err != nil {
		// 购买失败 → 记入动态黑名单（活动种子/不可购种子自动排除，防循环卡死）
		markBuySeedFailed(seedID)
		return 0, err
	}
	// 从购买结果取真实种子 ID
	if got := proto.DecodeBuyGoodsReply(brep.Body); got != nil && len(got.GetItems) > 0 && got.GetItems[0].ID > 0 {
		return got.GetItems[0].ID, nil
	}
	return seedID, nil
}

// listBagSeeds 返回背包中可用种子（count>0 且 plantSize==size）按 BagSeedPriority 排序。
// 排序键：priority 升序 → requiredLevel 降序 → seedId 升序。
func listBagSeeds(accountID string, c *gw.Client, cfg config.AccountConfig, size int) ([]bagSeedItem, error) {
	rep, err := c.Request(context.Background(), "gamepb.itempb.ItemService", "Bag",
		proto.EncodeBagRequest(), 12*time.Second)
	if err != nil {
		return nil, err
	}
	priority := map[int]int{}
	for i, s := range cfg.BagSeedPriority {
		priority[s] = i
	}
	var seeds []bagSeedItem
	for _, it := range proto.DecodeBagReply(rep.Body).Items {
		if it.ID <= 0 || it.Count <= 0 || !isSeedItemID(it.ID) {
			continue
		}
		pe, ok := plantForSeed(int(it.ID))
		if !ok || pe.Size != size {
			continue
		}
		p, has := priority[int(it.ID)]
		if !has {
			p = 1 << 30
		}
		seeds = append(seeds, bagSeedItem{seedID: it.ID, count: it.Count, reqLvl: getSeedLevel(int(it.ID)), prio: p})
	}
	sort.SliceStable(seeds, func(i, j int) bool {
		if seeds[i].prio != seeds[j].prio {
			return seeds[i].prio < seeds[j].prio
		}
		if seeds[i].reqLvl != seeds[j].reqLvl {
			return seeds[i].reqLvl > seeds[j].reqLvl
		}
		return seeds[i].seedID < seeds[j].seedID
	})
	return seeds, nil
}

// plantBagSeedsForLands 用背包 1x1 种子按优先级顺序种植。
// 每种背包种子按其数量消耗；返回未被背包种子覆盖的剩余空地主地 ID，以及是否允许用第二优先策略补种
// （某背包种子部分种植失败时 fallbackAllowed=false，避免误购商城种子）。
func plantBagSeedsForLands(accountID string, c *gw.Client, cfg config.AccountConfig, masters []int64) (remaining []int64, fallbackAllowed bool, plantedCount int, err error) {
	fallbackAllowed = true
	if len(masters) == 0 {
		return nil, true, 0, nil
	}
	seeds, err := listBagSeeds(accountID, c, cfg, 1)
	if err != nil {
		return masters, false, 0, err
	}
	if len(seeds) == 0 {
		return masters, true, 0, nil
	}
	reserved := reservedLandSet(accountID)
	remainingSet := map[int64]bool{}
	for _, m := range masters {
		if reserved[m] {
			continue
		}
		remainingSet[m] = true
	}
	for _, s := range seeds {
		if len(remainingSet) == 0 {
			break
		}
		maxCount := int64(len(remainingSet))
		if s.count < maxCount {
			maxCount = s.count
		}
		if maxCount <= 0 {
			continue
		}
		planted := 0
		// 背包等级锁定种子（reqLvl>当前等级）尝试失败不应阻断第二策略补种
		levelLocked := int64(s.reqLvl) > c.Level()
		for m := range remainingSet {
			if planted >= int(maxCount) {
				break
			}
			realSeed, e := ensureSeedOwned(c, s.seedID, 0, 0, 1)
			if e != nil || realSeed <= 0 {
				if !levelLocked {
					fallbackAllowed = false
				}
				break
			}
			if e2 := execFarmOp(c, "Plant", proto.EncodePlantRequest(realSeed, []int64{m})); e2 != nil {
				if !levelLocked {
					fallbackAllowed = false
				}
				break
			}
			delete(remainingSet, m)
			planted++
			plantedCount++
			recordOperation(accountID, "plant", 1)
			appendOpLog(accountID, "farm", fmt.Sprintf("背包种植种子 %d → 1 块地", realSeed))
			time.Sleep(plantDelay(cfg) + 200*time.Millisecond)
		}
		// 实际种植数少于请求数 → 部分失败，避免误购商城
		// 等级锁定种子整批无法种植（planted==0）时不阻断 fallback
		if planted < int(maxCount) && len(remainingSet) > 0 && (!levelLocked || planted > 0) {
			fallbackAllowed = false
			break
		}
	}
	for m := range remainingSet {
		remaining = append(remaining, m)
	}
	return remaining, fallbackAllowed, plantedCount, nil
}

// plantFromShopLands 对给定主地列表按策略从商城选种购买并种植，返回成功种植的单地数。
func plantFromShopLands(accountID string, c *gw.Client, cfg config.AccountConfig, masters []int64, overrideStrategy string) int {
	strategy := overrideStrategy
	if strategy == "" {
		strategy = cfg.PlantingStrategy
		if strategy == "" {
			strategy = "level"
		}
	}
	reserved := reservedLandSet(accountID)
	remaining := map[int64]bool{}
	for _, m := range masters {
		if reserved[m] {
			continue
		}
		remaining[m] = true
	}
	startRemaining := len(remaining)
	// 优先种植指定种子：背包里有就直接用（商店无货也能种），商店兜底
	if strategy == "preferred" && cfg.PreferredSeedID > 0 {
		if seedID, ok := pickBagSeedByID(accountID, c, int64(cfg.PreferredSeedID)); ok {
			plantOneSeed(accountID, c, cfg, seedID, 0, 0, remaining)
			if len(remaining) == 0 {
				return startRemaining - len(remaining)
			}
		}
	}
	cands, err := findSeedCandidates(c, cfg, strategy)
	if err != nil {
		appendOpLog(accountID, "farm", "种植跳过：无可用种子 ("+err.Error()+")")
		return startRemaining - len(remaining)
	}
	for _, cand := range cands {
		if len(remaining) == 0 {
			break
		}
		plantOneSeed(accountID, c, cfg, cand.seedID, cand.goodsID, cand.price, remaining)
	}
	return startRemaining - len(remaining)
}

// plantOneSeed 用指定种子在剩余主地上种植（逐块）。购买或种植失败即停，剩余地交给调用方换候选。
func plantOneSeed(accountID string, c *gw.Client, cfg config.AccountConfig, seedID, goodsID, price int64, remaining map[int64]bool) {
	for m := range remaining {
		realSeed, err := ensureSeedOwned(c, seedID, goodsID, price, 1)
		if err != nil || realSeed <= 0 {
			// 购买失败（金币不足/限购/买不到）：停止该种子，换下一个候选
			if errors.Is(err, errGoldShort) {
				appendOpLog(accountID, "farm", fmt.Sprintf("金币不足，跳过种子 %d（单价 %d）", seedID, price))
			} else {
				appendOpLog(accountID, "farm", fmt.Sprintf("购买种子 %d 失败: %v", seedID, err))
			}
			return
		}
		if err := execFarmOp(c, "Plant", proto.EncodePlantRequest(realSeed, []int64{m})); err != nil {
			// 种植失败：同种子对账号级限制大概率全失败，换下一个候选
			appendOpLog(accountID, "farm", fmt.Sprintf("种植 %d 失败: %v", realSeed, err))
			return
		}
		delete(remaining, m)
		recordOperation(accountID, "plant", 1)
		appendOpLog(accountID, "farm", fmt.Sprintf("种植种子 %d → 1 块地", realSeed))
		time.Sleep(plantDelay(cfg) + 200*time.Millisecond)
	}
}

// ── 智能施肥 ──

func normalizeFertilizerLandTypes(in []string) []string {
	valid := map[string]bool{"purple": true, "gold": true, "black": true, "red": true, "normal": true}
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, t := range in {
		t = normalizeLandTypeName(t)
		if valid[t] && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

func normalizeLandTypeName(t string) string {
	switch t {
	case "紫", "紫色", "紫土地":
		return "purple"
	case "金", "金色", "金土地":
		return "gold"
	case "黑", "黑土地":
		return "black"
	case "红", "红土地":
		return "red"
	case "普通", "普通地", "normal":
		return "normal"
	}
	return t
}

// landTypeByLevel 5→purple,4→gold,3→black,2→red,else normal
func landTypeByLevel(level int64) string {
	switch level {
	case 5:
		return "purple"
	case 4:
		return "gold"
	case 3:
		return "black"
	case 2:
		return "red"
	default:
		return "normal"
	}
}

func runFertilizerByConfig(accountID string, c *gw.Client, cfg config.AccountConfig, explicitLandIDs []int64, reason string, skipNormal bool) {
	mode := cfg.Automation.Fertilizer
	if mode == "" || mode == "none" {
		return
	}
	landTypes := normalizeFertilizerLandTypes(cfg.Automation.FertilizerLandTypes)
	if len(landTypes) == 0 {
		return // 未选土地类型则不施肥
	}

	rep, err := c.Request(context.Background(), plantService, "AllLands",
		proto.EncodeAllLandsRequest(0), 15*time.Second)
	if err != nil {
		return
	}
	lands := proto.DecodeAllLandsReply(rep.Body).Lands
	now := time.Now().Unix()
	smartSeconds := cfg.Automation.FertilizerSmartSeconds
	if smartSeconds <= 0 {
		smartSeconds = 3600 //
	}

	// 按土地类型过滤（从属地块随 master 处理：仅对非 slave 地块施肥）
	typeLandMap := map[int64]string{}
	var standalone []*proto.LandInfo
	for _, l := range lands {
		if l == nil || l.ID <= 0 || l.MasterLandID > 0 {
			continue
		}
		typeLandMap[l.ID] = landTypeByLevel(l.Level)
		standalone = append(standalone, l)
	}
	filterByType := func(ids []int64) []int64 {
		out := ids[:0:0]
		for _, id := range ids {
			if t, ok := typeLandMap[id]; ok {
				for _, want := range landTypes {
					if t == want {
						out = append(out, id)
						break
					}
				}
			}
		}
		return out
	}

	// 候选池：multi_season 用显式地块，其余用全部 standalone
	basePool := standaloneIDs(standalone)
	if reason == "multi_season" && len(explicitLandIDs) > 0 {
		basePool = explicitLandIDs
	}
	// 智能/最终阶段候选
	fast := filterByType(getFastMatureLands(standalone, int64(smartSeconds), now))

	var normalTargets, organicTargets []int64
	switch mode {
	case "normal":
		if !skipNormal {
			normalTargets = filterByType(basePool)
		}
	case "organic":
		organicTargets = filterByType(getOrganicFertilizerTargetsFromLands(standalone))
	case "both":
		if !skipNormal {
			normalTargets = filterByType(basePool)
		}
		organicTargets = filterByType(getOrganicFertilizerTargetsFromLands(standalone))
	case "smart":
		// 普通(显式快成熟) + 有机(快成熟)，
		if !skipNormal {
			normalTargets = fast
		}
		organicTargets = fast
	case "smart_only":
		organicTargets = fast
	case "smart_normal":
		if !skipNormal {
			normalTargets = fast
		}
	case "final_normal":
		if !skipNormal {
			normalTargets = filterByType(getFinalStageLands(standalone, false, now))
		}
	case "final_organic":
		organicTargets = filterByType(getFinalStageLands(standalone, true, now))
	}

	if len(normalTargets) > 0 {
		fertilizedNormal := fertilizeLands(c, normalTargets, normalFertilizerID)
		recordOperation(accountID, "fertilize", int64(fertilizedNormal)) // 记实际成功施肥数，非目标块数
	}
	if len(organicTargets) > 0 {
		fertilizedOrganic := fertilizeOrganicLoop(c, organicTargets) // 有机肥：无次数即停止
		recordOperation(accountID, "fertilize", int64(fertilizedOrganic))
	}
	if len(normalTargets) > 0 || len(organicTargets) > 0 {
		appendOpLog(accountID, "farm", fmt.Sprintf("施肥(%s) 普通%d/有机%d", mode, len(normalTargets), len(organicTargets)))
	}
}

func standaloneIDs(lands []*proto.LandInfo) []int64 {
	out := make([]int64, 0, len(lands))
	for _, l := range lands {
		out = append(out, l.ID)
	}
	return out
}

// getFastMatureLands MATURE begin_time 在 [0, threshold] 内且未枯死，
// 且 left_inorc_fert_times > 0（无有机肥余次的地块不进入候选）
func getFastMatureLands(lands []*proto.LandInfo, thresholdSec, now int64) []int64 {
	out := make([]int64, 0, len(lands))
	for _, l := range lands {
		if l == nil || l.Plant == nil || len(l.Plant.Phases) == 0 {
			continue
		}
		ph := currentPhase(l.Plant.Phases, now)
		if ph == nil || ph.Phase == proto.PhaseDead || ph.Phase == proto.PhaseMature {
			continue
		}
		if l.Plant.HasLeftInorcFertTimes && l.Plant.LeftInorcFertTimes <= 0 {
			continue
		}
		for _, p := range l.Plant.Phases {
			if p.Phase == proto.PhaseMature && p.BeginTime > 0 {
				if p.BeginTime-now >= 0 && p.BeginTime-now <= thresholdSec {
					out = append(out, l.ID)
				}
				break
			}
		}
	}
	return out
}

// getOrganicFertilizerTargetsFromLands
// 仅挑选"还能再施有机肥"的地块（left_inorc_fert_times > 0）。HasLeftInorcFertTimes=false 时
// 视为服务端未下发该字段，按 Node Object.hasOwn 语义包含（不跳过）。
func getOrganicFertilizerTargetsFromLands(lands []*proto.LandInfo) []int64 {
	out := []int64{}
	for _, l := range lands {
		if l == nil || l.ID <= 0 || !l.Unlocked || l.MasterLandID > 0 {
			continue
		}
		p := l.Plant
		if p == nil || len(p.Phases) == 0 {
			continue
		}
		ph := currentPhase(p.Phases, time.Now().Unix())
		if ph == nil || ph.Phase == proto.PhaseDead {
			continue
		}
		if p.HasLeftInorcFertTimes && p.LeftInorcFertTimes <= 0 {
			continue
		}
		out = append(out, l.ID)
	}
	return out
}

// getFinalStageLands 当前阶段恰为 MATURE 前一阶段；
// organicOnly 时仅保留 left_inorc_fert_times > 0 的地块
func getFinalStageLands(lands []*proto.LandInfo, organicOnly bool, now int64) []int64 {
	out := make([]int64, 0, len(lands))
	for _, l := range lands {
		if l == nil || l.Plant == nil || len(l.Plant.Phases) == 0 {
			continue
		}
		if organicOnly && l.Plant.HasLeftInorcFertTimes && l.Plant.LeftInorcFertTimes <= 0 {
			continue
		}
		phases := append([]*proto.PlantPhaseInfo{}, l.Plant.Phases...)
		sort.SliceStable(phases, func(i, j int) bool { return phases[i].BeginTime < phases[j].BeginTime })
		matureIdx := -1
		for i, p := range phases {
			if p.Phase == proto.PhaseMature {
				matureIdx = i
				break
			}
		}
		if matureIdx <= 0 {
			continue
		}
		cur := currentPhase(l.Plant.Phases, now)
		if cur == nil {
			continue
		}
		curIdx := -1
		for i, p := range phases {
			if p == cur {
				curIdx = i
				break
			}
		}
		if curIdx == matureIdx-1 {
			out = append(out, l.ID)
		}
	}
	return out
}

// fertilizeLands 逐块施化肥，返回实际成功施肥的地块数（ 返回成功数）
func fertilizeLands(c *gw.Client, ids []int64, fertID int64) int {
	n := 0
	for _, id := range ids {
		if err := execFarmOp(c, "Fertilize", proto.EncodeFertilizeRequest([]int64{id}, fertID)); err == nil {
			n++
		}
		time.Sleep(200 * time.Millisecond)
	}
	return n
}

// fertilizeOrganicLoop 逐块施有机肥，无次数即停止；返回实际成功施的块数
func fertilizeOrganicLoop(c *gw.Client, ids []int64) int {
	n := 0
	for _, id := range ids {
		if err := execFarmOp(c, "Fertilize", proto.EncodeFertilizeRequest([]int64{id}, organicFertilizerID)); err != nil {
			return n // 首次失败（无有机化肥次数）即停止，返回已成功数
		}
		n++
		time.Sleep(200 * time.Millisecond)
	}
	return n
}

// ── 自动卖果实 ──
// Node 最新版要点：
//  1. 果实判定用 isFruitItemId(id) = Boolean(getPlantByFruitId(id))，纯查 Plant.json；
//  2. 自动出售跳过名单 AUTO_SELL_SKIP_ITEM_IDS = new Set([41221])，
//     这些物品即便可售也会被服务端以 code=1000020 拒绝，需从候选剔除，否则整批 Sell 失败；
//  3. 分批出售（SELL_BATCH_SIZE=15）批量失败改逐个重试并跳过不可售 item。

// autoSellSkipItemIDs 自动出售跳过名单
// 41221 为青梅活动果实，调 ItemService.Sell 会被服务端以 code=1000020 拒绝。
var autoSellSkipItemIDs = map[int64]bool{41221: true}

const sellBatchSize = 15

func autoSellAfterHarvest(accountID string, c *gw.Client) {
	prevGold := c.Gold() // 卖前余额
	rep, err := c.Request(context.Background(), "gamepb.itempb.ItemService", "Bag",
		proto.EncodeBagRequest(), 12*time.Second)
	if err != nil {
		return
	}
	items := proto.DecodeBagReply(rep.Body).Items
	sell := make([]proto.SellItem, 0, len(items))
	for _, it := range items {
		if it.ID > 0 && it.Count > 0 && isFruitItemID(it.ID) && !autoSellSkipItemIDs[it.ID] {
			sell = append(sell, proto.SellItem(it))
		}
	}
	if len(sell) == 0 {
		return
	}
	parsedGold := int64(0) // 出售响应解析金币
	soldKinds := 0
	// 分批出售
	for i := 0; i < len(sell); i += sellBatchSize {
		end := i + sellBatchSize
		if end > len(sell) {
			end = len(sell)
		}
		batch := sell[i:end]
		if gold, ok := trySellBatch(accountID, c, batch); ok {
			parsedGold += gold
			soldKinds += len(batch)
			continue
		}
		// 批量失败，逐个重试
		for _, it := range batch {
			g, ok := trySellOne(accountID, c, it)
			if ok {
				parsedGold += g
				soldKinds++
			}
		}
		if end < len(sell) {
			time.Sleep(300 * time.Millisecond)
		}
	}
	// (出售响应解析值, 余额差值兜底)
	// 等待余额状态刷新（.gold 更新，最多 3s）
	afterGold := prevGold
	waitStart := time.Now()
	for time.Since(waitStart) < 3*time.Second {
		if g := c.Gold(); g != prevGold {
			afterGold = g
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	// get_items 有金币(parsedGold)就直接作为收益，
	// 不掺余额差值——避免 ItemNotify 把卖前余额覆盖成小值时，差值兜底把几十亿余额当收益。
	// 仅当 get_items 无金币(parsedGold==0)且余额已知且增长时才用差值兜底。
	totalGold := parsedGold
	if totalGold == 0 && prevGold > 0 && afterGold > prevGold {
		totalGold = afterGold - prevGold
	}
	if totalGold > 0 {
		recordOperation(accountID, "sell", totalGold)
	}
	if soldKinds > 0 {
		appendOpLog(accountID, "farm", fmt.Sprintf("自动卖果实 %d 种, 获得 %d 金币", soldKinds, totalGold))
	}
}

// trySellBatch 批量出售，成功返回获得金币数与 true。
func trySellBatch(accountID string, c *gw.Client, batch []proto.SellItem) (int64, bool) {
	if len(batch) == 0 {
		return 0, false
	}
	rep, err := c.Request(context.Background(), "gamepb.itempb.ItemService", "Sell",
		proto.EncodeSellRequest(batch), 12*time.Second)
	if err != nil {
		return 0, false
	}
	_, gold := proto.DecodeSellReply(rep.Body)
	return gold, true
}

// trySellOne 单个出售，失败（含不可售）记录日志并返回 false，不中断其余。
func trySellOne(accountID string, c *gw.Client, it proto.SellItem) (int64, bool) {
	rep, err := c.Request(context.Background(), "gamepb.itempb.ItemService", "Sell",
		proto.EncodeSellRequest([]proto.SellItem{it}), 12*time.Second)
	if err != nil {
		appendOpLog(accountID, "farm", fmt.Sprintf("自动卖果实跳过不可售物品 ID=%d x%d: %s", it.ID, it.Count, err.Error()))
		return 0, false
	}
	_, gold := proto.DecodeSellReply(rep.Body)
	return gold, true
}

// ── 自动买化肥 ──
// 化肥容器：普通 1011 / 有机 1012（count 为秒，/3600 为小时）；商品：有机 1002 / 普通 1003（slot_type=1）
// 购买策略：分批小额（每轮 ≤10 个、轮间 120ms）、点券余额预判与降级、容器已满识别、余额不足当天暂停。

// buyPausedNoGoldDate 点券不足当天暂停（key=日期，次日自动恢复）
var (
	buyPausedMu     sync.Mutex
	buyPausedNoGold = map[string]string{} // accountID → 暂停日期（todayKey）
)

func doCheckAndBuyFertilizer(accountID string, c *gw.Client, cfg config.AccountConfig) {
	// 点券不足当天暂停：不再反复打空炮
	buyPausedMu.Lock()
	pausedDate := buyPausedNoGold[accountID]
	buyPausedMu.Unlock()
	if pausedDate == todayKey() {
		return
	}

	rep, err := c.Request(context.Background(), "gamepb.itempb.ItemService", "Bag",
		proto.EncodeBagRequest(), 12*time.Second)
	if err != nil {
		return
	}
	hoursNormal, hoursOrganic := 0.0, 0.0
	for _, it := range proto.DecodeBagReply(rep.Body).Items {
		switch it.ID {
		case normalFertilizerID:
			hoursNormal = float64(it.Count) / 3600
		case organicFertilizerID:
			hoursOrganic = float64(it.Count) / 3600
		}
	}

	bought := false
	noGold := false
	if cfg.Automation.FertilizerBuyOrganic && cfg.FertilizerBuyOrganicCount > 0 &&
		cfg.FertilizerBuyOrganicThresholdHours > 0 && hoursOrganic < float64(cfg.FertilizerBuyOrganicThresholdHours) {
		if n, ng := buyMallFertilizer(c, 1002, int64(cfg.FertilizerBuyOrganicCount), c.Coupon()); n > 0 {
			bought = true
		} else if ng {
			noGold = true
		}
	}
	if cfg.Automation.FertilizerBuyNormal && cfg.FertilizerBuyNormalCount > 0 &&
		cfg.FertilizerBuyNormalThresholdHours > 0 && hoursNormal < float64(cfg.FertilizerBuyNormalThresholdHours) {
		if bought {
			time.Sleep(time.Duration(1000+rand.Intn(2000)) * time.Millisecond) // 有机/普通间 1–3s
		}
		if n, ng := buyMallFertilizer(c, 1003, int64(cfg.FertilizerBuyNormalCount), c.Coupon()); n > 0 {
			bought = true
		} else if ng {
			noGold = true
		}
	}
	if bought {
		appendOpLog(accountID, "farm", "自动购买化肥")
	}
	// 点券不足：当天暂停（次日 todayKey 变化自动恢复）
	if noGold {
		buyPausedMu.Lock()
		buyPausedNoGold[accountID] = todayKey()
		buyPausedMu.Unlock()
		appendOpLog(accountID, "farm", "点券不足，自动购买化肥今日暂停")
	}
}

// buyMallFertilizer 分批购买化肥，返回成功购买数量与是否因余额不足失败。
// 策略：单轮 ≤10 个（最多 100 轮、轮间 120ms）；用点券余额预判每轮可买量，
// 余额不足降级为单买一次，仍失败则标记 noGold 并停止；容器已满（1003002）识别后停止。
func buyMallFertilizer(c *gw.Client, goodsID, count int64, ticket int64) (int64, bool) {
	rep, err := c.Request(context.Background(), "gamepb.mallpb.MallService", "GetMallListBySlotType",
		proto.EncodeGetMallListBySlotTypeRequest(1), 12*time.Second)
	if err != nil {
		return 0, false
	}
	var goods *proto.MallGoods
	list := proto.DecodeMallListBySlotTypeReply(rep.Body)
	for i := range list.GoodsList {
		if list.GoodsList[i].GoodsID == goodsID {
			goods = &list.GoodsList[i]
			break
		}
	}
	if goods == nil {
		return 0, false
	}
	singlePrice := proto.ParseMallPriceValue(goods.PriceBytes)

	const perRound = int64(10)
	const maxRounds = 100
	total := int64(0)
	round := perRound
	for i := 0; i < maxRounds && total < count; i++ {
		// 余额预判：点券足够时按可买量限购，不够则降级单买
		if singlePrice > 0 && ticket > 0 {
			if ticket < singlePrice {
				return total, true
			}
			if maxBuy := ticket / singlePrice; maxBuy < round {
				round = maxBuy
				if round <= 0 {
					round = 1
				}
			}
		}
		buy := round
		if remain := count - total; buy > remain {
			buy = remain
		}
		if _, err := c.Request(context.Background(), "gamepb.mallpb.MallService", "Purchase",
			proto.EncodePurchaseRequest(goodsID, buy), 12*time.Second); err != nil {
			// 容器已满：不再购买（1003002）
			if proto.IsFertilizerContainerFullError(err.Error()) {
				break
			}
			// 余额/点券不足：降级单买一次，仍失败则当天暂停
			if strings.Contains(err.Error(), "code=1000019") || strings.Contains(err.Error(), "余额不足") ||
				strings.Contains(err.Error(), "点券不足") {
				if round > 1 {
					round = 1
					continue
				}
				return total, true
			}
			break
		}
		total += buy
		if singlePrice > 0 && ticket > 0 {
			ticket -= singlePrice * buy
			if ticket < singlePrice {
				break
			}
		}
		time.Sleep(120 * time.Millisecond) // 轮间间隔，避免密集购买触发风控
	}
	return total, false
}
