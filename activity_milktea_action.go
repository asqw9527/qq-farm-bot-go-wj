package main

// S4 奶茶店铺「操作」接口：领取店铺营业值 / 制作奶茶 / 添加员工。
//
// 【协议状态——如实标注，不臆造】
//   - 已实锤（只读查询）：cmd 55 店铺全量 / 56 雇佣列表 / 58 配方表 / 59 资产 /
//     67 今日统计 / 79 服务器时间（见 activity_milktea_overview.go 头部）
//   - 制作奶茶：cmd 62 已确认会被服务端受理（ext 字段 121 会被消费），
//     但 ext 载荷编码尚未解出 —— 服务端回「选择的果实无效，请重新选择」
//   - 领取店铺营业值 / 添加员工：cmd 尚未实锤（候选 57/61/64/68…）
//
// 因此本接口不假装「协议已知」：每个操作按候选组合依次真实发送，
// 并把每次尝试的「服务端原文报错 / 回包摘要」原样返回。
//   · err != nil（带中文业务报错）→ 说明该 cmd+载荷已被服务端受理并处理，只是参数不对
//   · err == nil              → 服务端正常返回，即视为本次操作成功
// 这样面板上一轮轮点、看返回，即可逐步收敛出真实协议。

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/asqw9527/qq-farm-bot-go-wj/proto"
)

// milkTeaExtField 奶茶操作 ext 扩展字段号（实锤：cmd 62 会消费该字段）
const milkTeaExtField = 121

// milkTeaAttempt 单次尝试的结果
type milkTeaAttempt struct {
	Cmd      int64       `json:"cmd"`
	ExtField int         `json:"ext_field,omitempty"`
	ExtHex   string      `json:"ext_hex,omitempty"`
	Desc     string      `json:"desc,omitempty"`
	Accepted bool        `json:"accepted"`          // 服务端已受理并处理（有明确业务返回，无论成败）
	OK       bool        `json:"ok"`                // 服务端无错返回（视为操作成功）
	Error    string      `json:"error,omitempty"`   // 服务端业务报错原文
	Reply    interface{} `json:"reply,omitempty"`   // 回包字段树摘要
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

// mtCraftCandidates 制作奶茶（cmd 62）的 ext 载荷候选
func mtCraftCandidates(recipeID, fruitID int64) []struct {
	Desc string
	Data []byte
} {
	var out []struct {
		Desc string
		Data []byte
	}
	add := func(label string, id int64) {
		out = append(out,
			struct {
				Desc string
				Data []byte
			}{label + "→f1", mtExtBuild([2]int64{1, id})},
			struct {
				Desc string
				Data []byte
			}{label + "→f2", mtExtBuild([2]int64{2, id})},
			struct {
				Desc string
				Data []byte
			}{label + "→f3", mtExtBuild([2]int64{3, id})},
			struct {
				Desc string
				Data []byte
			}{"f1=" + label + ", f2=1", mtExtBuild([2]int64{1, id}, [2]int64{2, 1})},
			struct {
				Desc string
				Data []byte
			}{"f1=1, f2=" + label, mtExtBuild([2]int64{1, 1}, [2]int64{2, id})},
		)
	}
	if recipeID > 0 {
		add("配方id", recipeID)
	}
	if fruitID > 0 && fruitID != recipeID {
		add("果实id", fruitID)
	}
	// 空 ext 兜底（实锤：cmd 62 不带 ext 时报「选择的果实无效」）
	out = append(out, struct {
		Desc string
		Data []byte
	}{"空 ext", nil})
	return out
}

// mtHireCandidates 添加员工（cmd 57 候选）的 ext 载荷候选
func mtHireCandidates(uid int64) []struct {
	Desc string
	Data []byte
} {
	var out []struct {
		Desc string
		Data []byte
	}
	if uid > 0 {
		out = append(out,
			struct {
				Desc string
				Data []byte
			}{"uid→f1", mtExtBuild([2]int64{1, uid})},
			struct {
				Desc string
				Data []byte
			}{"uid→f2", mtExtBuild([2]int64{2, uid})},
			struct {
				Desc string
				Data []byte
			}{"f1=uid, f2=1", mtExtBuild([2]int64{1, uid}, [2]int64{2, 1})},
		)
	}
	out = append(out, struct {
		Desc string
		Data []byte
	}{"空 ext", nil})
	return out
}

// mtClaimCmds 领取店铺营业值：尚未实锤的候选 cmd（其余均已实锤为查询/其它用途）
var mtClaimCmds = []int64{57, 61, 64, 68, 69, 70, 71, 72, 73, 74, 75, 76, 77, 78}

// ----- POST /api/activity/milktea/action -----

func handleMilkTeaAction(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccountID string `json:"accountId"`
		Action    string `json:"action"`    // claim | craft | hire | custom
		ID        int64  `json:"id"`        // 店铺节点（留空自动定位）
		RecipeID  int64  `json:"recipeId"`  // 制作奶茶：配方 id
		FruitID   int64  `json:"fruitId"`   // 制作奶茶：果实/作物 id（cmd 58 配方 raw_f6）
		UID       int64  `json:"uid"`       // 添加员工：目标玩家 uid
		Cmd       int64  `json:"cmd"`       // custom：指定 cmd
		ExtField  int    `json:"ext_field"` // custom：ext 字段号
		ExtHex    string  `json:"ext_hex"`  // custom：ext 十六进制
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
			it.Reply = tree
		}
		attempts = append(attempts, it)
	}

	switch req.Action {
	case "craft":
		if req.RecipeID <= 0 && req.FruitID <= 0 {
			writeJSONMap(w, "ok", false, "error", "制作奶茶需要提供 recipeId（配方 id）")
			return
		}
		for _, c := range mtCraftCandidates(req.RecipeID, req.FruitID) {
			try(62, milkTeaExtField, c.Data, c.Desc)
			if attempts[len(attempts)-1].OK {
				break // 成功即停，不再继续发
			}
		}
	case "hire":
		for _, c := range mtHireCandidates(req.UID) {
			try(57, milkTeaExtField, c.Data, c.Desc)
			if attempts[len(attempts)-1].OK {
				break
			}
		}
	case "claim":
		for _, cmd := range mtClaimCmds {
			try(cmd, 0, nil, "")
			if attempts[len(attempts)-1].OK {
				break
			}
		}
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

	var won *milkTeaAttempt
	for i := range attempts {
		if attempts[i].OK {
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

// mtActionNote 未实锤协议的说明（面板展示用）
func mtActionNote(action, shopID string) string {
	switch action {
	case "craft":
		return "制作奶茶 cmd 62 已实锤（服务端会消费 ext 字段 121）。若全部返回「选择的果实无效」，说明载荷还需携带果实条目结构；可到下方协议诊断逐个试 ext 编码。"
	case "hire":
		return "添加员工 cmd 未实锤（尝试 57）。候选不匹配时请到协议诊断卡用「扫描(cmd×ext)」扩大范围。"
	case "claim":
		return "领取店铺营业值 cmd 未实锤，已按候选列表逐一尝试。返回「活动参数错误」=该 cmd 不接受此 payload；若某一条无错返回即为命中。"
	}
	return "自定义 Operate 尝试。"
}
