package main

// S4 奶茶店铺「操作」接口：领取店铺营业值 / 制作奶茶 / 添加员工 / 协议扫描。
//
// 【协议状态——如实标注，不臆造】
//   - 已实锤（只读查询）：cmd 55 店铺全量 / 56 雇佣列表 / 58 配方表 / 59 资产 /
//     67 今日统计 / 79 服务器时间（见 activity_milktea_overview.go 头部）
//   - 制作奶茶：cmd 62 已被服务端受理（能返回业务级中文报错），
//     但 **ext 字段号与载荷均未解出** —— 8 种载荷一律回「选择的果实无效，请重新选择」
//   - 领取店铺营业值 / 添加员工：cmd 均未实锤
//
// ★ 2026-10-27 重大修正（推翻此前假设）：
//   1. ext 字段号 121 此前是**推断值，没有任何 proto 依据**。核对上游 Node 版
//      activitypb.proto 全文（198 行）确认：OperateRequest 的扩展字段只有
//      3(draw) / 101 / 102 / 103 / 107 / 112 / 113 / 114，**不存在 121**，
//      且上游三个项目**都没有实现奶茶**（S4 为全新活动，无参考实现）。
//   2. 因此 8 种载荷全回同一个错，最合理的解释是：服务端**根本没解析我们的 ext**
//      （未知字段被忽略），cmd 62 走了「参数缺失」分支回默认业务错误。
//      → 继续在载荷形态上穷举是无效的，必须改攻「ext 字段号」本身。
//   3. 可参照的正确形态（proto 里真实存在、同为「提交材料」语义）：
//        QingmeiWineStartParams { repeated corepb.Item items = 1; }  // 字段 112
//        corepb.Item { int64 id = 1; int64 count = 2; ... }
//      即 ext = {1:{1:id,2:count}} —— 形态我们对过，但字段号要从 100..130 扫。
//   4. cmd 64 回包 f169 为**空字符串**（响应字段号 = cmd+105，64→169），
//      返回体里只有节点 id 与活动定义文案 → 64 大概率是**查询/状态类**接口，
//      不是真领取。故领取成功判据升级为「无错 **且** f(cmd+105) 有实体载荷」。
//
// 因此本接口不假装「协议已知」：提供
//   · craft/hire/claim：按候选组合依次真实发送，回显服务端原文
//   · scan：cmd × ext 字段号矩阵扫描，以「报错指纹是否偏离基线」定位真实字段号

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/asqw9527/qq-farm-bot-go-wj/proto"
)

// milkTeaExtFieldGuess 奶茶操作 ext 扩展字段号的**猜测值**，仅作默认兜底。
// ⚠ 121 没有 proto 依据：上游 Node 版 activitypb.proto（198 行）里
// OperateRequest 的扩展字段只有 3/101/102/103/107/112/113/114，并无 121。
const milkTeaExtFieldGuess = 121

// milkTeaAttempt 单次尝试的结果
type milkTeaAttempt struct {
	Cmd        int64       `json:"cmd"`
	ExtField   int         `json:"ext_field,omitempty"`
	ExtHex     string      `json:"ext_hex,omitempty"`
	Desc       string      `json:"desc,omitempty"`
	Accepted   bool        `json:"accepted"`          // 服务端已受理并处理（有明确业务返回，无论成败）
	OK         bool        `json:"ok"`                // 服务端无错返回
	HasPayload bool        `json:"has_payload"`       // 回包 f(cmd+105) 有实体载荷 → 真产生了业务结果
	Error      string      `json:"error,omitempty"`   // 服务端业务报错原文
	Payload    interface{} `json:"payload,omitempty"` // 回包 f(cmd+105) 载荷（空 = 仅查询/状态返回）
	Reply      interface{} `json:"reply,omitempty"`   // 回包字段树摘要（去 f3）
}

// mtPayloadOf 取回包中该 cmd 对应的业务载荷字段（规律：响应字段号 = cmd + 105）
func mtPayloadOf(cmd int64, tree map[string]interface{}) interface{} {
	if tree == nil {
		return nil
	}
	return tree["f"+strconv.FormatInt(cmd+105, 10)]
}

// mtPayloadNonEmpty 载荷是否有实体内容（空串 / 空表 / nil 均视为「无实体」）
func mtPayloadNonEmpty(v interface{}) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(t) != ""
	case map[string]interface{}:
		return len(t) > 0
	case []interface{}:
		return len(t) > 0
	case int64:
		return t != 0
	}
	return false
}

// mtReplyExcerpt 回包摘要：去掉 f3（活动定义大块，每个回包都重复且体积大）
func mtReplyExcerpt(tree map[string]interface{}) map[string]interface{} {
	if tree == nil {
		return nil
	}
	out := map[string]interface{}{}
	for k, v := range tree {
		if k == "f3" {
			continue
		}
		out[k] = v
	}
	return out
}

// milkTeaOperateExt 发送带 ext 的 Operate；extField<=0 时不附加 ext
func milkTeaOperateExt(ctx context.Context, accountID string, id, cmd int64, extField int, ext []byte, dur time.Duration) (map[string]interface{}, error) {
	b := proto.NewBuilder()
	b.FieldInt64(1, id)
	b.FieldInt64(2, cmd)
	if extField > 0 {
		b.FieldBytes(extField, ext)
	}
	rep, err := rpcRequest(ctx, accountID, actSvc, "Operate", b.Bytes(), dur)
	if err != nil {
		return nil, err
	}
	st := &actTreeStats{Max: 600}
	if tree, ok := buildActTree(rep, 8, st).(map[string]interface{}); ok {
		return tree, nil
	}
	return map[string]interface{}{}, nil
}

// milkTeaResolveShopID 定位店铺节点：逐个候选发 cmd 55，首个回包带 f160 的即店铺节点
func milkTeaResolveShopID(ctx context.Context, accountID string, forcedID int64) (int64, []map[string]interface{}) {
	var probe []map[string]interface{}
	if forcedID > 0 {
		if tree, err := milkTeaOperate(ctx, accountID, forcedID, milkTeaShopProbeCmd, 10*time.Second); err == nil && actTreeGet(tree, "f160") != nil {
			return forcedID, probe
		}
		probe = append(probe, map[string]interface{}{"id": forcedID, "error": "cmd 55 无 f160（非店铺节点）"})
	}
	nodes := milkTeaFindNodes(ctx, accountID)
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
	for _, n := range nodes {
		push(n.ID)
		for d := int64(1); d <= 4; d++ {
			push(n.ID + d)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		tree, err := milkTeaOperate(ctx, accountID, id, milkTeaShopProbeCmd, 10*time.Second)
		if err != nil {
			probe = append(probe, map[string]interface{}{"id": id, "error": actErrMsg(err)})
			continue
		}
		if actTreeGet(tree, "f160") != nil {
			return id, probe
		}
	}
	return 0, probe
}

// mtExtBuild 组装 ext 子消息：[字段号, 值] 列表
func mtExtBuild(kvs ...[2]int64) []byte {
	b := proto.NewBuilder()
	for _, kv := range kvs {
		b.FieldInt64(int(kv[0]), kv[1])
	}
	return b.Bytes()
}

// mtExtPair 一个候选载荷
type mtExtPair struct {
	Desc string
	Data []byte
}

// mtExtNested 组装嵌套子消息：{ field: { kvs... } }
func mtExtNested(field int, kvs ...[2]int64) []byte {
	sub := proto.NewBuilder()
	for _, kv := range kvs {
		sub.FieldInt64(int(kv[0]), kv[1])
	}
	b := proto.NewBuilder()
	b.FieldMessage(field, sub.Bytes())
	return b.Bytes()
}

// mtCraftCandidates 制作奶茶（cmd 62）的 ext 载荷候选
//
// ⚠ 2026-10-27 结论：在字段号 121 下，8 种载荷形态全部返回同一句
// 「选择的果实无效，请重新选择」。这说明问题几乎肯定**不在载荷形态**，
// 而在 ext 字段号本身（121 无 proto 依据，可能压根没被解析）。
// 因此这里只保留**精简**的形态集合（≤10 次请求）用于对照，
// 真正的定位手段是 action=scan（固定载荷、横扫字段号）。
func mtCraftCandidates(recipeID int64, fruitIDs []int64, count int64) []mtExtPair {
	if count <= 0 {
		count = 1
	}
	var out []mtExtPair
	add := func(desc string, data []byte) { out = append(out, mtExtPair{Desc: desc, Data: data}) }

	// 只取前 3 个候选 id，每个 2 种核心形态（Item 嵌套 / 扁平），避免请求爆炸
	if len(fruitIDs) > 3 {
		fruitIDs = fruitIDs[:3]
	}
	for _, id := range fruitIDs {
		add("id="+strconv.FormatInt(id, 10)+" · {1:{1:id,2:count}}",
			mtExtNested(1, [2]int64{1, id}, [2]int64{2, count}))
		add("id="+strconv.FormatInt(id, 10)+" · {1:id,2:count}",
			mtExtBuild([2]int64{1, id}, [2]int64{2, count}))
	}

	// ② 多材料组合：repeated Item（配方常需 2 种材料；与 QingmeiWineStartParams 同形态）
	if len(fruitIDs) >= 2 {
		nest := proto.NewBuilder()
		for _, id := range fruitIDs[:2] {
			sub := proto.NewBuilder()
			sub.FieldInt64(1, id)
			sub.FieldInt64(2, 1)
			nest.FieldMessage(1, sub.Bytes())
		}
		add("repeated Item[果1,果2]", nest.Bytes())
	}

	// ③ 配方 id 形态（亦已试过，保留 1 条对照）
	if recipeID > 0 {
		add("配方id="+strconv.FormatInt(recipeID, 10)+" · {1:{1:id,2:count}}",
			mtExtNested(1, [2]int64{1, recipeID}, [2]int64{2, count}))
	}

	// ④ 空 ext 兜底
	add("空 ext", nil)
	return out
}

// mtDeriveFruitIDs 从 cmd 58 配方表的原始树里挖「果实/材料 id」候选：
// 递归收集配方条目里所有 ≥10000 的 varint（排除配方 id 自身），
// f6（作物 id 候选）排最前，最多 6 个。
func mtDeriveFruitIDs(ctx context.Context, accountID string, shopID, recipeID int64) []int64 {
	tree, err := milkTeaOperate(ctx, accountID, shopID, 58, 10*time.Second)
	if err != nil {
		return nil
	}
	arr := actTreeArr(tree, "f163", "f1")
	var entry interface{}
	for _, it := range arr {
		if actTreeNum(it, "f1") == recipeID {
			entry = it
			break
		}
	}
	if entry == nil {
		return nil
	}
	var out []int64
	has := func(v int64) bool {
		for _, x := range out {
			if x == v {
				return true
			}
		}
		return false
	}
	// f6（作物 id）优先
	if f6 := actTreeNum(entry, "f6"); f6 >= 10000 && f6 != recipeID {
		out = append(out, f6)
	}
	var walk func(v interface{})
	walk = func(v interface{}) {
		switch t := v.(type) {
		case int64:
			if t >= 10000 && t != recipeID && !has(t) && len(out) < 6 {
				out = append(out, t)
			}
		case []interface{}:
			for _, x := range t {
				walk(x)
			}
		case map[string]interface{}:
			for _, x := range t {
				walk(x)
			}
		}
	}
	walk(entry)
	return out
}

// mtDeriveAssetIDs 从 cmd 59「资产」表取 item_id 作为材料/果实候选。
// 「选择的果实」很可能指店铺资产里的作物条目，而非配方表里的静态字段。
// 排序：≥10000（作物/材料类 id）优先，小额货币（1001 金币等）靠后。
func mtDeriveAssetIDs(ctx context.Context, accountID string, shopID int64) []int64 {
	tree, err := milkTeaOperate(ctx, accountID, shopID, 59, 10*time.Second)
	if err != nil {
		return nil
	}
	arr := actTreeArr(tree, "f164", "f1")
	var big, small []int64
	for _, it := range arr {
		id := actTreeNum(it, "f1")
		if id <= 0 {
			continue
		}
		if id >= 10000 {
			big = append(big, id)
		} else {
			small = append(small, id)
		}
	}
	sort.Slice(big, func(i, j int) bool { return big[i] < big[j] })
	sort.Slice(small, func(i, j int) bool { return small[i] < small[j] })
	return append(big, small...)
}

// mtHireCandidates 添加员工（cmd 57 候选）的 ext 载荷候选
func mtHireCandidates(uid int64) []mtExtPair {
	var out []mtExtPair
	add := func(desc string, data []byte) { out = append(out, mtExtPair{Desc: desc, Data: data}) }
	if uid > 0 {
		add("uid→f1", mtExtBuild([2]int64{1, uid}))
		add("uid→f2", mtExtBuild([2]int64{2, uid}))
		add("f1=uid + f2=1", mtExtBuild([2]int64{1, uid}, [2]int64{2, 1}))
		add("121:{1:{1:uid}}", mtExtNested(1, [2]int64{1, uid}))
		add("121:{1:{1:uid,2:1}}", mtExtNested(1, [2]int64{1, uid}, [2]int64{2, 1}))
	}
	add("空 ext", nil)
	return out
}

// mtClaimCmds 领取店铺营业值的候选 cmd。
// ⚠ 64 排在首位只是历史惯性：实测它能「无错返回」，但回包 f169 为**空字符串**，
// 即没有任何业务实体 → 64 大概率是查询/状态类接口，不是真领取。
// 真正的成功判据已升级为 has_payload（f(cmd+105) 非空），64 不会再被误判为成功。
var mtClaimCmds = []int64{64, 57, 61, 68, 69, 70, 71, 72, 73, 74, 75, 76, 77, 78}

// mtScanPayload 生成扫描用的固定载荷（扫描期间保持不变，只变字段号 → 信噪比最高）
//
//	form=item → {1:{1:id,2:count}}  corepb.Item 形态，与 proto 里真实存在的
//	            QingmeiWineStartParams{repeated Item items=1} 完全一致
//	form=flat → {1:id,2:count}      RandomShop/ExchangeShop 的扁平形态
func mtScanPayload(form string, id, count int64) []byte {
	if count <= 0 {
		count = 1
	}
	if form == "flat" {
		return mtExtBuild([2]int64{1, id}, [2]int64{2, count})
	}
	return mtExtNested(1, [2]int64{1, id}, [2]int64{2, count})
}

// mtParseCmdList 解析 cmd 列表，支持三种写法：
//
//	"62"          单个
//	"55,57,61,62" 逗号列表
//	"50-90"       范围（也接受 50~90）
//
// 结果去重升序；解析不出任何值则回落到 baseCmd。
func mtParseCmdList(s string, baseCmd int64) []int64 {
	var out []int64
	seen := map[int64]bool{}
	push := func(v int64) {
		if v > 0 && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		// 范围：50-90 / 50~90
		if i := strings.IndexAny(p, "-~"); i > 0 && i < len(p)-1 {
			lo, err1 := strconv.ParseInt(strings.TrimSpace(p[:i]), 10, 64)
			hi, err2 := strconv.ParseInt(strings.TrimSpace(p[i+1:]), 10, 64)
			if err1 == nil && err2 == nil && lo > 0 && hi >= lo {
				if hi-lo > 200 {
					hi = lo + 200
				}
				for v := lo; v <= hi; v++ {
					push(v)
				}
				continue
			}
		}
		if v, err := strconv.ParseInt(p, 10, 64); err == nil {
			push(v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	if len(out) == 0 {
		return []int64{baseCmd}
	}
	return out
}

// mtRunScan 并发扫描 (cmd × extField) 矩阵，固定同一份载荷。
// 并发上限 4，避免短时间大量 Operate 触发风控。
func mtRunScan(ctx context.Context, accountID string, shopID int64, cmds []int64, extFields []int, payload []byte) []milkTeaAttempt {
	type job struct{ cmd int64; ext int }
	var jobs []job
	for _, c := range cmds {
		for _, e := range extFields {
			jobs = append(jobs, job{c, e})
		}
	}
	res := make([]milkTeaAttempt, len(jobs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			it := milkTeaAttempt{Cmd: j.cmd, ExtField: j.ext}
			if len(payload) > 0 {
				it.ExtHex = hex.EncodeToString(payload)
			}
			tree, err := milkTeaOperateExt(ctx, accountID, shopID, j.cmd, j.ext, payload, 8*time.Second)
			if err != nil {
				msg := actErrMsg(err)
				it.Error = msg
				if msg != "" && msg != "empty response" {
					it.Accepted = true
				}
			} else {
				it.OK = true
				it.Accepted = true
				p := mtPayloadOf(j.cmd, tree)
				it.Payload = p
				it.HasPayload = mtPayloadNonEmpty(p)
				it.Reply = mtReplyExcerpt(tree)
			}
			res[i] = it
		}(i, j)
	}
	wg.Wait()
	return res
}

// ----- POST /api/activity/milktea/action -----

func handleMilkTeaAction(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccountID string `json:"accountId"`
		Action    string `json:"action"`    // claim | craft | hire | custom | scan
		ID        int64  `json:"id"`        // 店铺节点（留空自动定位）
		RecipeID  int64  `json:"recipeId"`  // 制作奶茶：配方 id
		FruitID   int64  `json:"fruitId"`   // 制作奶茶：果实/作物 id（cmd 58 配方 raw_f6 / cmd 59 资产 item_id）
		Count     int64  `json:"count"`     // 制作奶茶：份数/数量（对应 {1:id,2:count} 的 count）
		UID       int64  `json:"uid"`       // 添加员工：目标玩家 uid
		Cmd       int64  `json:"cmd"`       // custom：指定 cmd / scan：扫描基准 cmd
		ExtField  int    `json:"ext_field"` // custom：ext 字段号
		ExtHex    string  `json:"ext_hex"`  // custom：ext 十六进制
		CmdList   string  `json:"cmd_list"` // scan：cmd 列表，逗号分隔（如 "62" 或 "55,57,61,62"）
		ExtFrom   int     `json:"ext_from"` // scan：ext 字段号起始（含）
		ExtTo     int     `json:"ext_to"`   // scan：ext 字段号结束（含）
		Form      string  `json:"form"`     // scan：载荷形态 item|flat
		NoExt     bool    `json:"no_ext"`   // scan：不带 ext，纯扫 cmd（画语义地图）
	}
	if body, err := io.ReadAll(io.LimitReader(r.Body, 65536)); err == nil && len(body) > 0 {
		_ = json.Unmarshal(body, &req)
	}
	accountID := resolveAccountIDWithOwner(r, req.AccountID)
	if accountID == "" {
		writeJSONMap(w, "ok", false, "error", "缺少 accountId")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 150*time.Second)
	defer cancel()

	shopID, probe := milkTeaResolveShopID(ctx, accountID, req.ID)
	if shopID == 0 {
		writeJSONMap(w, "ok", false, "error", "未能定位店铺节点（cmd 55 全部无 f160）", "probe", probe)
		return
	}

	var attempts []milkTeaAttempt
	try := func(cmd int64, extField int, ext []byte, desc string) {
		it := milkTeaAttempt{Cmd: cmd, ExtField: extField, Desc: desc}
		if len(ext) > 0 {
			it.ExtHex = hex.EncodeToString(ext)
		}
		tree, err := milkTeaOperateExt(ctx, accountID, shopID, cmd, extField, ext, 10*time.Second)
		if err != nil {
			msg := actErrMsg(err)
			it.Error = msg
			// 有中文业务原文 → 服务端受理并处理了（参数不对，而非协议不通）
			if msg != "" && msg != "empty response" {
				it.Accepted = true
			}
		} else {
			it.OK = true
			it.Accepted = true
			p := mtPayloadOf(cmd, tree)
			it.Payload = p
			it.HasPayload = mtPayloadNonEmpty(p)
			it.Reply = mtReplyExcerpt(tree)
		}
		attempts = append(attempts, it)
	}

	switch req.Action {
	case "craft":
		if req.RecipeID <= 0 {
			writeJSONMap(w, "ok", false, "error", "制作奶茶需要提供 recipeId（配方 id）")
			return
		}
		// 报错「选择的果实无效」→ 载荷需要果实条目。候选来源两条线：
		//   ① cmd 58 配方原始树里挖出的 ≥10000 varint（材料/作物 id 候选）
		//   ② cmd 59 资产表里的 item_id ——「选择的果实」很可能是店铺资产中的作物条目
		fruitIDs := mtDeriveFruitIDs(ctx, accountID, shopID, req.RecipeID)
		fruitIDs = append(fruitIDs, mtDeriveAssetIDs(ctx, accountID, shopID)...)
		if req.FruitID > 0 {
			fruitIDs = append([]int64{req.FruitID}, fruitIDs...)
		}
		// 去重
		var uniq []int64
		for _, id := range fruitIDs {
			dup := false
			for _, x := range uniq {
				if x == id {
					dup = true
					break
				}
			}
			if !dup {
				uniq = append(uniq, id)
			}
		}
		for _, c := range mtCraftCandidates(req.RecipeID, uniq, req.Count) {
			try(62, milkTeaExtFieldGuess, c.Data, c.Desc)
			if lastOK(attempts) {
				break // 成功即停，不再继续发
			}
		}
	case "hire":
		for _, c := range mtHireCandidates(req.UID) {
			try(57, milkTeaExtFieldGuess, c.Data, c.Desc)
			if lastOK(attempts) {
				break
			}
		}
	case "claim":
		// 两轮：先全部空载荷扫一遍，再带 {1:1} 载荷扫一遍（官方 params 多为 {1:id,2:count}）
		for _, cmd := range mtClaimCmds {
			try(cmd, 0, nil, "")
			if lastOK(attempts) {
				break
			}
		}
		if !wasOK(attempts) {
			one := mtExtBuild([2]int64{1, 1})
			for _, cmd := range mtClaimCmds {
				try(cmd, milkTeaExtFieldGuess, one, "{1:1}")
				if lastOK(attempts) {
					break
				}
			}
		}
	case "scan":
		// ── ext 字段号矩阵扫描 ──
		// 前提：cmd 62 能返回业务级中文报错 → 服务端确实处理了这个 cmd；
		// 但 8 种载荷全回同一句错 → 极可能是 ext 字段号压根没被解析。
		// 故固定载荷、只变字段号，用「报错是否偏离基线」定位真实字段号。
		baseCmd := req.Cmd
		if baseCmd <= 0 {
			baseCmd = 62
		}
		id := req.FruitID
		if id <= 0 {
			if ids := mtDeriveAssetIDs(ctx, accountID, shopID); len(ids) > 0 {
				id = ids[0]
			} else if ids := mtDeriveFruitIDs(ctx, accountID, shopID, req.RecipeID); len(ids) > 0 {
				id = ids[0]
			}
		}
		payload := mtScanPayload(req.Form, id, req.Count)

		// 基线：同 cmd 完全不带 ext
		baseIt := milkTeaAttempt{Cmd: baseCmd, Desc: "基线（无 ext）"}
		if tree, err := milkTeaOperateExt(ctx, accountID, shopID, baseCmd, 0, nil, 10*time.Second); err != nil {
			baseIt.Error = actErrMsg(err)
			if baseIt.Error != "" && baseIt.Error != "empty response" {
				baseIt.Accepted = true
			}
		} else {
			baseIt.OK = true
			baseIt.Accepted = true
			p := mtPayloadOf(baseCmd, tree)
			baseIt.Payload = p
			baseIt.HasPayload = mtPayloadNonEmpty(p)
			baseIt.Reply = mtReplyExcerpt(tree)
		}
		baseline := baseIt.Error

		cmds := mtParseCmdList(req.CmdList, baseCmd)
		var extFields []int
		from, to := req.ExtFrom, req.ExtTo
		if req.NoExt {
			// 不带 ext 扫 cmd → 画 cmd 语义地图
			extFields = []int{0}
			from, to = 0, 0
		} else {
			if from <= 0 {
				from = 100
			}
			if to <= 0 {
				to = 130
			}
			if to < from {
				to = from
			}
			for e := from; e <= to; e++ {
				extFields = append(extFields, e)
			}
		}
		total := len(cmds) * len(extFields)
		if total > 240 {
			writeJSONMap(w, "ok", false, "error", "扫描规模过大（"+strconv.FormatInt(int64(total), 10)+" 次请求，上限 240），请缩小 cmd 列表或 ext 区间")
			return
		}

		attempts = mtRunScan(ctx, accountID, shopID, cmds, extFields, payload)

		// 异常项 = 报错偏离基线，或无错返回 → 说明该字段号被服务端真正解析/消费
		var anomalies []milkTeaAttempt
		for _, it := range attempts {
			if it.OK || it.Error != baseline {
				anomalies = append(anomalies, it)
			}
		}

		// ── cmd 语义地图：按返回形态给每个点分类 ──
		//   queries 查询类：无错 + f(cmd+105) 有实体载荷（如 55/56/58/59/67/79）
		//   silent  静默类：无错但载荷为空（多为查询/状态，如 64）
		//   errors  报错类：带业务原文（需要参数，或流程前置条件未满足）
		var queries, silent, errs []milkTeaAttempt
		errGroups := map[string][]string{}
		for _, it := range attempts {
			tag := "cmd" + strconv.FormatInt(it.Cmd, 10)
			if it.ExtField > 0 {
				tag += "/ext" + strconv.Itoa(it.ExtField)
			}
			switch {
			case it.OK && it.HasPayload:
				queries = append(queries, it)
			case it.OK:
				silent = append(silent, it)
			default:
				errs = append(errs, it)
				msg := it.Error
				if msg == "" {
					msg = "(无返回)"
				}
				errGroups[msg] = append(errGroups[msg], tag)
			}
		}

		note := "固定载荷只变 ext 字段号。基线条（无 ext）的报错是「参数缺失」默认值；报错与基线不同、或无错返回的字段号，即为服务端真正消费的字段。"
		if req.NoExt {
			note = "不带 ext 扫 cmd → 画语义地图：queries=查询类（有实体回包）、silent=静默类（无错但回包空）、errors=报错类。重点看 errors 里报错原文**各不相同**的 cmd —— 若某个 cmd 报「请先选择果实」之类，它就是制作前的「选择果实」步骤。"
		}
		writeJSON(w, map[string]interface{}{
			"ok":           true,
			"action":       "scan",
			"shop_id":      shopID,
			"baseline":     baseIt,
			"baseline_msg": baseline,
			"payload_hex":  hex.EncodeToString(payload),
			"payload_desc": "id=" + strconv.FormatInt(id, 10) + " count=" + strconv.FormatInt(max64(req.Count, 1), 10) + " form=" + orDefault(req.Form, "item"),
			"cmds":         cmds,
			"ext_range":    []int{from, to},
			"no_ext":       req.NoExt,
			"total":        total,
			"attempts":     attempts,
			"anomalies":    anomalies,
			"map": map[string]interface{}{
				"queries":     queries,
				"silent":      silent,
				"errors":      errs,
				"error_groups": errGroups,
			},
			"note": note,
		})
		return
	case "custom":
		var extRaw []byte
		if req.ExtHex != "" {
			raw, err := hex.DecodeString(strings.TrimSpace(req.ExtHex))
			if err != nil {
				writeJSONMap(w, "ok", false, "error", "ext_hex 不是合法十六进制: "+err.Error())
				return
			}
			extRaw = raw
		}
		try(req.Cmd, req.ExtField, extRaw, "")
	default:
		writeJSONMap(w, "ok", false, "error", "未知 action: "+req.Action)
		return
	}

	// 成功项排前，便于面板一眼看到
	sort.SliceStable(attempts, func(i, j int) bool {
		if attempts[i].OK != attempts[j].OK {
			return attempts[i].OK
		}
		if attempts[i].Accepted != attempts[j].Accepted {
			return attempts[i].Accepted
		}
		return false
	})

	// 成功判据：无错返回 **且** f(cmd+105) 有实体载荷。
	// 只看「无错」会把查询类接口（如 cmd 64，回包 f169 为空）误判成领取成功。
	var won *milkTeaAttempt
	for i := range attempts {
		if attempts[i].OK && attempts[i].HasPayload {
			won = &attempts[i]
			break
		}
	}

	out := map[string]interface{}{
		"ok":       true,
		"action":   req.Action,
		"shop_id":  shopID,
		"attempts": attempts,
		"note": mtActionNote(req.Action, strconv.FormatInt(shopID, 10)),
	}
	if won != nil {
		out["success"] = won
		out["done"] = true
	} else {
		out["done"] = false
	}
	writeJSON(w, out)
}

// wasOK 是否已有「无错且有载荷」的成功项
func wasOK(list []milkTeaAttempt) bool {
	for i := range list {
		if list[i].OK && list[i].HasPayload {
			return true
		}
	}
	return false
}

// lastOK 最近一次尝试是否为「无错且有实体载荷」
func lastOK(list []milkTeaAttempt) bool {
	if len(list) == 0 {
		return false
	}
	last := list[len(list)-1]
	return last.OK && last.HasPayload
}

// orDefault 小工具（空串回落默认值）
func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// mtActionNote 未实锤协议的说明（面板展示用）
func mtActionNote(action, shopID string) string {
	switch action {
	case "craft":
		return "制作奶茶 cmd 62 能返回业务级中文报错 → 服务端确实受理了，但 8 种载荷全回同一句「选择的果实无效」。已核对上游 activitypb.proto：OperateRequest 扩展字段只有 3/101/102/103/107/112/113/114，**不存在 121**，故 121 大概率没被解析。请改用「扫描 cmd×ext」定位真实字段号——那里固定载荷只变字段号，看哪个字段号的报错偏离基线。"
	case "hire":
		return "添加员工 cmd 未实锤（尝试 57，回「活动参数错误」）。此外雇佣列表（cmd 56）解析为空，需要先把 cmd 56 的原始字段树 dump 出来才能确定 uid 取值位置。建议先用「扫描 cmd×ext」在 55~80 区间找。"
	case "claim":
		return "cmd 64 虽无错返回，但回包 f169 为**空字符串**（响应字段号 = cmd+105 → 64 对应 169），没有任何业务实体，因此 64 大概率是查询/状态类接口而非真领取。成功判据已升级为「无错 + f(cmd+105) 有实体载荷」，64 不会再被判为成功。真领取 cmd 仍未锁定，可用「扫描 cmd×ext」在 55~80 区间找（看哪个 cmd 的回包 f(cmd+105) 有内容）。"
	}
	return "自定义 Operate 尝试。"
}
