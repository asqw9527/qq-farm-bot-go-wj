package main

// S4「奶茶热销中」店铺「今日经营概览」真实数据（cmd 已实锤，不再走猜测）。
//
// 已实锤协议（ActivityService.Operate{1:id, 2:cmd}；节点 id = 店铺节点 2026110101，
// 由活动 root 或从树中携带 field121 的节点确定，代码中自动探测）：
//
//	cmd 55 → f160  店铺全量：等级/累计营业额/配方累计销量/**在岗员工 f9**/合同信息
//	cmd 56 → f161  雇佣候选池：**员工数组在 f161.f4**（f1/f2/f3 为标量）
//	cmd 58 → f163  配方表（9 个配方）
//	cmd 59 → f164  资产（1001 金币 / 1041 奶茶券 / 1042 …）
//	cmd 63 → f168  语义待定（含玩家信息 + f6{f1:2000,f2:136} 金额结构，疑似领取/结算）
//	cmd 66 → f171  候选/榜单池（玩家信息 + 星座 + 数值，语义待定）
//	cmd 67 → f172  今日统计：今日 0 点戳 / 制杯总数 / 按配方分布 / 按品质分布 / **员工班次 f7** / 资产快照
//	cmd 62 → 制作奶茶（「选择的果实无效」；ext 101~130 全同错，参数位未锁定）
//	cmd 79 → f179  服务器时间戳
//
// 规律：响应顶层业务字段号 = cmd + 105（55→f160、58→f163、59→f164、67→f172）；
// **特例 cmd 79 → f179 = cmd + 100**（2026-10-27 实测修正，此前误判为静默类）。
//
// 【在岗员工 vs 候选池 — 2026-10-27 用户实测厘清】
//   · 在岗员工：cmd 55 的 f160.f1.f9（用户「已雇 1 人」→ 该数组 1 项）
//   · 班次记录：cmd 67 的 f172.f7（含在岗员工的班次与收益 f10{f1:已付,f2:待领}）
//   · 雇佣候选池：cmd 56 的 f161.f4（姓名集合与在岗员工不同 → 是可雇的人，不是已雇的人）

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/asqw9527/qq-farm-bot-go-wj/proto"
)

// milkTeaShopProbeCmd 用于定位店铺节点的握手命令（回包带 f160 即为店铺节点）
const milkTeaShopProbeCmd = int64(55)

// milkTeaOverviewCmds 概览需要拉取的全部命令（均为不带 ext 即返回实体的只读查询）
//
// 66 与 63 为 2026-10-27 新增：两者不带 ext 均返回实体回包，行为与 55/56/58 一致，
// 判定为查询类；63 若实为「领取」则每次刷新会自动领取一次（对玩家有利，保留）。
// 注意：62（制作）不得加入此列表 —— 它会产生真实业务副作用。
var milkTeaOverviewCmds = []int64{55, 56, 58, 59, 63, 66, 67, 79}

// ----- 原始字段树取值辅助（buildActTree 输出：map[string]interface{} / []interface{} / int64 / string） -----

func actTreeGet(v interface{}, path ...interface{}) interface{} {
	cur := v
	for _, p := range path {
		if cur == nil {
			return nil
		}
		switch key := p.(type) {
		case string:
			m, ok := cur.(map[string]interface{})
			if !ok {
				return nil
			}
			cur = m[key]
		case int:
			arr, ok := cur.([]interface{})
			if !ok || key < 0 || key >= len(arr) {
				return nil
			}
			cur = arr[key]
		default:
			return nil
		}
	}
	return cur
}

func actTreeNum(v interface{}, path ...interface{}) int64 {
	switch t := actTreeGet(v, path...).(type) {
	case int64:
		return t
	case int:
		return int64(t)
	case float64:
		return int64(t)
	case string:
		if n, err := strconv.ParseInt(t, 10, 64); err == nil {
			return n
		}
	}
	return 0
}

func actTreeStr(v interface{}, path ...interface{}) string {
	if s, ok := actTreeGet(v, path...).(string); ok {
		return s
	}
	return ""
}

func actTreeArr(v interface{}, path ...interface{}) []interface{} {
	if a, ok := actTreeGet(v, path...).([]interface{}); ok {
		return a
	}
	return nil
}

// actTreeMap v 可能是数组的首个元素也可能是 map，统一取 map
func actTreeMap(v interface{}, path ...interface{}) map[string]interface{} {
	switch t := actTreeGet(v, path...).(type) {
	case map[string]interface{}:
		return t
	case []interface{}:
		if len(t) > 0 {
			if m, ok := t[0].(map[string]interface{}); ok {
				return m
			}
		}
	}
	return nil
}

// ----- milteaOperate 发一次 Operate 并返回原始字段树 -----

func milkTeaOperate(ctx context.Context, accountID string, id, cmd int64, dur time.Duration) (map[string]interface{}, error) {
	b := proto.NewBuilder()
	b.FieldInt64(1, id)
	b.FieldInt64(2, cmd)
	rep, err := rpcRequest(ctx, accountID, actSvc, "Operate", b.Bytes(), dur)
	if err != nil {
		return nil, err
	}
	st := &actTreeStats{Max: 900}
	if tree, ok := buildActTree(rep, 8, st).(map[string]interface{}); ok {
		return tree, nil
	}
	return map[string]interface{}{}, nil
}

// ----- GET /api/activity/milktea/overview 店铺今日经营概览（真实数据） -----

func handleMilkTeaOverview(w http.ResponseWriter, r *http.Request) {
	accountID := resolveAccountIDWithOwner(r, r.URL.Query().Get("accountId"))
	if accountID == "" {
		writeJSONMap(w, "ok", false, "error", "缺少 accountId")
		return
	}
	// 可选：强制指定店铺节点 id（面板诊断时用）
	forcedID, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	nodes := milkTeaFindNodes(ctx, accountID)
	if len(nodes) == 0 && forcedID == 0 {
		writeJSONMap(w, "ok", false, "error", "未找到奶茶活动节点")
		return
	}

	// 店铺节点候选：活动列表节点自身 + root+1..root+4（实锤 2026110100 root → 2026110101 店铺）
	var ids []int64
	push := func(id int64) {
		if id <= 0 {
			return
		}
		for _, x := range ids {
			if x == id {
				return
			}
		}
		ids = append(ids, id)
	}
	if forcedID > 0 {
		push(forcedID)
	}
	for _, n := range nodes {
		push(n.ID)
		for d := int64(1); d <= 4; d++ {
			push(n.ID + d)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	// 逐个发 cmd 55，第一个回包带 f160 的即店铺节点
	var shopID int64
	var shopTree map[string]interface{}
	var probeErrors []map[string]interface{}
	for _, id := range ids {
		tree, err := milkTeaOperate(ctx, accountID, id, milkTeaShopProbeCmd, 12*time.Second)
		if err != nil {
			probeErrors = append(probeErrors, map[string]interface{}{"id": id, "error": actErrMsg(err)})
			continue
		}
		if actTreeGet(tree, "f160") != nil {
			shopID, shopTree = id, tree
			break
		}
		probeErrors = append(probeErrors, map[string]interface{}{"id": id, "error": "cmd 55 无 f160（非店铺节点）"})
	}
	if shopID == 0 {
		writeJSONMap(w, "ok", false, "error", "未能定位店铺节点（cmd 55 全部无 f160）", "probe", probeErrors)
		return
	}

	// 拉取其余命令
	raws := map[int64]map[string]interface{}{milkTeaShopProbeCmd: shopTree}
	for _, cmd := range milkTeaOverviewCmds {
		if cmd == milkTeaShopProbeCmd {
			continue
		}
		tree, err := milkTeaOperate(ctx, accountID, shopID, cmd, 12*time.Second)
		if err != nil {
			probeErrors = append(probeErrors, map[string]interface{}{"id": shopID, "cmd": cmd, "error": actErrMsg(err)})
			continue
		}
		raws[cmd] = tree
	}

	out := map[string]interface{}{
		"ok":      true,
		"account": accountID,
		"shop_id": shopID,
		"now":     time.Now().Unix(),
	}

	// ---- cmd 55 → f160 店铺主体 ----
	if s := actTreeGet(raws[55], "f160", "f1"); s != nil {
		shop := map[string]interface{}{
			"level":              actTreeNum(s, "f1"),
			"revenue":            actTreeNum(s, "f2"), // 累计营业额（与 f121.f5 一致）
			"next_level_revenue": actTreeNum(s, "f3"), // 升级所需累计营业额
			"prev_level_revenue": actTreeNum(s, "f4"), // 本级起始营业额
			"raw_f5":             actTreeNum(s, "f5"), // 未映射，供对照
			"sync_at":            actTreeNum(s, "f11"),
		}
		// 配方累计销量 f10: [{f1:配方id, f2:杯数}]
		var sales []map[string]interface{}
		for _, it := range actTreeArr(s, "f10") {
			sales = append(sales, map[string]interface{}{
				"recipe_id": actTreeNum(it, "f1"),
				"cups":      actTreeNum(it, "f2"),
			})
		}
		shop["recipe_sales"] = sales
		// 在岗员工 f9
		var emps []map[string]interface{}
		for _, it := range actTreeArr(s, "f9") {
			emps = append(emps, map[string]interface{}{
				"instance_id": actTreeNum(it, "f3"),
				"hired_at":    actTreeNum(it, "f4"),
				"expire_at":   actTreeNum(it, "f5"),
				"wage":        actTreeNum(it, "f6"),
				"role_id":     actTreeNum(it, "f7"),
				"role":        actTreeStr(it, "f8"),
				"raw_f1":      actTreeNum(it, "f1"),
				"raw_f9":      actTreeNum(it, "f9"),
				"raw_f10":     actTreeNum(it, "f10"),
				"uid":         actTreeNum(it, "f11", "f1"),
				"name":        actTreeStr(it, "f11", "f2"),
				"user_level":  actTreeNum(it, "f11", "f4"),
				"avatar":      actTreeNum(it, "f11", "f3", "f13"),
			})
		}
		shop["employees"] = emps
		out["shop"] = shop
	}

	// ---- cmd 58 → f163 配方表 ----
	if arr := actTreeArr(raws[58], "f163", "f1"); arr != nil {
		var recipes []map[string]interface{}
		for _, it := range arr {
			name := actTreeStr(it, "f2")
			if name == "" {
				name = "(未知配方)" // 名称可能是嵌套结构（服务端偶发）
			}
			recipes = append(recipes, map[string]interface{}{
				"id":         actTreeNum(it, "f1"),
				"name":       name,
				"unlocked":   actTreeNum(it, "f3"),
				"raw_f4":     actTreeNum(it, "f4"),
				"raw_f5":     actTreeNum(it, "f5"), // 价格/等级门槛候选
				"raw_f6":     actTreeNum(it, "f6"), // 作物/材料 id 候选
				"total_sold": actTreeNum(it, "f7"),
				"raw":        it, // 完整字段树（材料/果实 id 挖掘与诊断用）
			})
		}
		out["recipes"] = recipes
	}

	// ---- cmd 59 → f164 资产 ----
	if arr := actTreeArr(raws[59], "f164", "f1"); arr != nil {
		var assets []map[string]interface{}
		for _, it := range arr {
			assets = append(assets, map[string]interface{}{
				"item_id": actTreeNum(it, "f1"),
				"count":   actTreeNum(it, "f2"),
				"raw_f6":  actTreeNum(it, "f6"),
			})
		}
		out["assets"] = assets
	}

	// ---- cmd 56 → f161 雇佣列表（候选池）----
	//
	// 【2026-10-27 实测校正】此前按 f161.f1 取数组，但 f161.f1 是标量（值 1），
	// 员工数组实际在 f161.f4 → hire_list 恒为空，「添加员工」下拉无选项。
	//
	// 实测结构（节点 2026110101）：
	//   f161 = { f1:1, f2:40, f3:600,
	//     f4:[ { f1:{f1:uid, f2:昵称, f3:{f13:头像}, f4:等级},
	//            f3:9001, f4:"金牌销售", f5:1, f6:1793176288 }, ... ] }
	//   · f1 推测为已雇数（用户实测：店铺已雇 1 人、另 1 个工位空着）
	//   · f4[] 内玩家信息与 cmd55 的 f160.f1.f9 在岗员工、cmd67 的 f172.f7 班次
	//     是同一嵌套形态，但姓名集合不同 → 判定 f161.f4 为「可雇佣候选池」，
	//     而非在岗员工（在岗员工见 shop.employees，来自 cmd55 f160.f1.f9）。
	if v := actTreeGet(raws[56], "f161"); v != nil {
		out["hire_meta"] = map[string]interface{}{
			"raw_f1": actTreeNum(v, "f1"), // 推测：当前已雇佣数
			"raw_f2": actTreeNum(v, "f2"),
			"raw_f3": actTreeNum(v, "f3"),
		}
		var hires []map[string]interface{}
		for _, it := range actTreeArr(v, "f4") {
			hires = append(hires, map[string]interface{}{
				"uid":        actTreeNum(it, "f1", "f1"),
				"name":       actTreeStr(it, "f1", "f2"),
				"user_level": actTreeNum(it, "f1", "f4"),
				"avatar":     actTreeNum(it, "f1", "f3", "f13"),
				"role_id":    actTreeNum(it, "f3"), // 实测 9001
				"role":       actTreeStr(it, "f4"), // 实测「金牌销售」
				"status":     actTreeNum(it, "f5"),
				"expire_at":  actTreeNum(it, "f6"), // 推测：雇佣到期时间戳
			})
		}
		out["hire_list"] = hires
	}

	// ---- cmd 66 → f171 候选/榜单池 ----
	// 实测：f171.f1[] = { f1:{f1:uid,f2:昵称,f3:{f13:头像},f4:等级},
	//                     f2:"白羊座"(星座), f3:11, f4:472723, f5:1 }
	// 语义待定（访客榜 / 好友榜 / 另一路候选池），先原样解析供对照。
	if v := actTreeGet(raws[66], "f171"); v != nil {
		var pool []map[string]interface{}
		for _, it := range actTreeArr(v, "f1") {
			pool = append(pool, map[string]interface{}{
				"uid":        actTreeNum(it, "f1", "f1"),
				"name":       actTreeStr(it, "f1", "f2"),
				"user_level": actTreeNum(it, "f1", "f4"),
				"avatar":     actTreeNum(it, "f1", "f3", "f13"),
				"sign":       actTreeStr(it, "f2"),
				"raw_f3":     actTreeNum(it, "f3"),
				"raw_f4":     actTreeNum(it, "f4"),
				"raw_f5":     actTreeNum(it, "f5"),
			})
		}
		out["friend_pool"] = pool
	}

	// ---- cmd 63 → f168 语义待定（疑似领取/结算）----
	// 实测：f168.f1 = { f1:{f1:uid,f2:昵称,f3:{f13:头像},f4:等级},
	//                   f10:1, f11:270,
	//                   f12:{f1:3, f2:1, f3:90013, f4:1793068134,
	//                        f5:932560810177503500, f6:{f1:2000, f2:136}, f7:{…}} }
	// f12.f6 是成对金额结构，f12.f5 是超大整数（流水号）→ 疑似「领取/结算」凭证。
	// 语义未实锤，全部字段原样输出，不做业务解释。
	if v := actTreeGet(raws[63], "f168", "f1"); v != nil {
		out["cmd63"] = map[string]interface{}{
			"uid":        actTreeNum(v, "f1", "f1"),
			"name":       actTreeStr(v, "f1", "f2"),
			"user_level": actTreeNum(v, "f1", "f4"),
			"avatar":     actTreeNum(v, "f1", "f3", "f13"),
			"raw_f10":    actTreeNum(v, "f10"),
			"raw_f11":    actTreeNum(v, "f11"),
			"raw_f12":    actTreeGet(v, "f12"), // 完整结构，供判定是否为领取凭证
		}
	}

	// ---- cmd 67 → f172 今日统计 ----
	if t := actTreeGet(raws[67], "f172"); t != nil {
		// f4 即杯数（2026-10-27 实锤：f4=1163 ↔ 游戏内「今日售出 1163 杯」；
		// 此前 ÷10 定点一位小数的标定为误判，已回退）
		cupsRaw := actTreeNum(t, "f4")
		today := map[string]interface{}{
			"day_start":    actTreeNum(t, "f1"), // 今日 0 点时间戳
			"raw_f2":       actTreeNum(t, "f2"),
			"raw_f3":       actTreeNum(t, "f3"),
			"cups_raw":     cupsRaw,
			"cups":         float64(cupsRaw), // 今日售出杯数（f4 原值即杯数）
			"server_time":  actTreeNum(raws[79], "f179", "f1"),
			"raw_from_cmd": actTreeNum(t, "f4"),
		}
		// 营业速度：今日售出 ÷ 今日已过小时
		if dayStart := actTreeNum(t, "f1"); dayStart > 0 {
			nowUnix := actTreeNum(raws[79], "f179", "f1")
			if nowUnix <= 0 {
				nowUnix = time.Now().Unix()
			}
			if hours := float64(nowUnix-dayStart) / 3600; hours > 0.1 {
				today["hours_elapsed"] = float64(int(hours*100)) / 100
				today["speed_per_hour"] = float64(int(float64(cupsRaw)/hours*10)) / 10
			}
		}
		var byRecipe, byQuality []map[string]interface{}
		for _, it := range actTreeArr(t, "f5") {
			byRecipe = append(byRecipe, map[string]interface{}{"recipe_id": actTreeNum(it, "f1"), "cups": float64(actTreeNum(it, "f2"))})
		}
		for _, it := range actTreeArr(t, "f6") {
			byQuality = append(byQuality, map[string]interface{}{"quality": actTreeNum(it, "f1"), "cups": float64(actTreeNum(it, "f2"))})
		}
		today["by_recipe"], today["by_quality"] = byRecipe, byQuality
		// 员工班次 f7：含今日收益 f10={f1:已付, f2:累计/待领}
		var shifts []map[string]interface{}
		for _, it := range actTreeArr(t, "f7") {
			shifts = append(shifts, map[string]interface{}{
				"instance_id": actTreeNum(it, "f6"),
				"uid":         actTreeNum(it, "f1", "f1"),
				"name":        actTreeStr(it, "f1", "f2"),
				"user_level":  actTreeNum(it, "f1", "f4"),
				"role_id":     actTreeNum(it, "f8"),
				"role":        actTreeStr(it, "f9"),
				"shift_start": actTreeNum(it, "f2"),
				"raw_f3":      actTreeNum(it, "f3"),
				"raw_f4":      actTreeNum(it, "f4"),
				"paid":        actTreeNum(it, "f10", "f1"),
				"pending":     actTreeNum(it, "f10", "f2"),
			})
		}
		today["shifts"] = shifts
		var snap []map[string]interface{}
		for _, it := range actTreeArr(t, "f8") {
			snap = append(snap, map[string]interface{}{"item_id": actTreeNum(it, "f1"), "count": actTreeNum(it, "f2")})
		}
		today["assets"] = snap
		out["today"] = today
	}

	// ---- cmd 79 → f179 服务器时间 ----
	if v := actTreeGet(raws[79], "f179"); v != nil {
		out["server_time"] = actTreeNum(v, "f1")
	}

	// 原始字段树（保留，便于继续对照新字段）
	rawOut := map[string]interface{}{}
	for _, cmd := range milkTeaOverviewCmds {
		if tree, ok := raws[cmd]; ok {
			key := "cmd" + strconv.FormatInt(cmd, 10)
			// 去掉每个回包里重复的 f3（活动信息大块），减小体积
			delete(tree, "f3")
			rawOut[key] = tree
		}
	}
	out["raw"] = rawOut
	if len(probeErrors) > 0 {
		out["notes"] = probeErrors
	}

	writeJSON(w, out)
}
