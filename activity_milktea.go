package main

// S4「奶茶热销中」奶茶店铺面板后端。
//
// 已确认协议（抓包/代码实锤）：
//   - 活动列表   ActivityService.List          → 活动节点（奶茶组 20261101xx）
//   - 活动分组树 ActivityService.GetGroup      → 节点树 + 兑换商店(field102) + 店铺状态(field121)
//
// 注：奶茶游记（战令 SeasonService）活动已下线，相关领取逻辑已删除；
//     千星游记等仍在使用 SeasonService 的功能见 activity_api.go / task_auto.go。
//
// 待抓包映射（游戏内奶茶店铺界面独有数据，字段号未知）：
//   累计营业额/销售额/营业速度/员工（名称、特长、工作时长、互动收益）
//   → 通过 /api/activity/milktea/shop 返回的 tree（原始字段树）做人工对照后补映射。

import (
	"context"
	"encoding/base64"
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

// milkTeaCampaignPrefix S4 奶茶热销中活动组前缀（2026110100 / 20261101xx）
const milkTeaCampaignPrefix = 20261101

// ----- 通用原始字段树（诊断/字段映射用） -----

// actTreeStats 限制树规模防止回包过大
type actTreeStats struct {
	Nodes int
	Max   int
}

func (s *actTreeStats) ok() bool { return s.Nodes < s.Max }

// buildActTree 把原始 protobuf 解码为 {f字段号: 值|嵌套|[]} 树。
// wire0 → 数字；wire2 → 可解析则递归嵌套，否则 base64；重复字段 → 数组。
func buildActTree(buf []byte, depth int, st *actTreeStats) interface{} {
	fs := readActFields(buf)
	if len(fs) == 0 {
		return nil
	}
	m := map[string]interface{}{}
	for _, f := range fs {
		key := "f" + strconv.Itoa(f.No)
		var val interface{}
		switch f.Wire {
		case 0:
			val = f.Varint
		case 2:
			if depth > 0 && st.ok() {
				if sub := buildActTree(f.Bytes, depth-1, st); sub != nil {
					st.Nodes++
					val = sub
				} else {
					val = actTreeBytes(f.Bytes)
				}
			} else {
				val = actTreeBytes(f.Bytes)
			}
		default:
			continue
		}
		actTreeAppend(m, key, val)
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

func actTreeAppend(m map[string]interface{}, key string, val interface{}) {
	if old, ok := m[key]; ok {
		switch t := old.(type) {
		case []interface{}:
			m[key] = append(t, val)
		default:
			m[key] = []interface{}{old, val}
		}
		return
	}
	m[key] = val
}

// actTreeBytes bytes 字段取值：可打印文本直接给文本，否则给 base64（截断）
func actTreeBytes(b []byte) interface{} {
	if len(b) == 0 {
		return ""
	}
	printable := true
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			// 允许 UTF-8 中文等
			if c < 0x80 {
				printable = false
				break
			}
		}
	}
	if printable {
		s := string(b)
		if len(s) > 300 {
			s = s[:300] + "…"
		}
		return s
	}
	s := base64.StdEncoding.EncodeToString(b)
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return "b64:" + s
}

// ----- 奶茶活动定位 -----

// milkTeaFindNodes 从活动列表中定位奶茶活动（标题含「奶茶」或 ID 前缀 20261101）
func milkTeaFindNodes(ctx context.Context, accountID string) []*ActivityInfo {
	body, err := rpcRequest(ctx, accountID, actSvc, "List", []byte{}, 15*time.Second)
	if err != nil {
		return nil
	}
	items := ParseActivityList(body)
	var out []*ActivityInfo
	for _, it := range items {
		if it == nil || it.ID <= 0 {
			continue
		}
		if it.ID/100 == milkTeaCampaignPrefix || strings.Contains(it.Title, "奶茶") {
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ----- GET /api/activity/milktea/shop -----

func handleMilkTeaShop(w http.ResponseWriter, r *http.Request) {
	accountID := resolveAccountIDWithOwner(r, r.URL.Query().Get("accountId"))
	if accountID == "" {
		writeJSONMap(w, "ok", false, "error", "缺少 accountId")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	// 1. 定位奶茶活动
	nodes := milkTeaFindNodes(ctx, accountID)

	out := map[string]interface{}{
		"ok":      true,
		"account": accountID,
		"now":     time.Now().Unix(),
	}

	// 2. 首个在期活动节点：分组树 + 兑换商店 + 店铺实时状态 + 原始字段树（协议映射诊断）
	if len(nodes) > 0 {
		node := nodes[0]
		gb, err := rpcRequest(ctx, accountID, actSvc, "GetGroup", actGroupReqBody(node.ID), 20*time.Second)
		if err == nil {
			tree := ParseActivityGroup(gb)
			out["activity"] = map[string]interface{}{
				"id":         node.ID,
				"title":      node.Title,
				"start_time": node.StartTime,
				"end_time":   node.EndTime,
			}
			if tree != nil {
				out["exchange_shop"] = tree.ExchangeShop
				if shop := findMilkTeaShop(tree); shop != nil {
					out["shop"] = shop
				}
			}
			// 全节点清单（含各节点自身载荷字段树）：定位「制作奶茶」「领取员工收益」等操作协议
			nst := &actTreeStats{Max: 900}
			var nfs []*milkTeaNodeBrief
			collectMilkTeaNodes(tree, 0, nst, &nfs)
			out["nodes_full"] = nfs
			// 原始字段树：累计营业额/销售额/营业速度/员工等字段映射的数据来源
			st := &actTreeStats{Max: 4000}
			out["tree"] = buildActTree(gb, 12, st)
			out["tree_note"] = "原始 protobuf 字段树（f字段号）。店铺名/等级/累计营业额已映射（f121）；今日售出/营业速度/员工及制作奶茶、领取员工收益的操作协议待抓包对照后补充。"
		} else {
			out["group_error"] = actErrMsg(err)
		}
	}
	// 其余节点仅列摘要
	type nodeBrief struct {
		ID    int64  `json:"id"`
		Title string `json:"title"`
	}
	var briefs []nodeBrief
	for _, n := range nodes {
		briefs = append(briefs, nodeBrief{ID: n.ID, Title: n.Title})
	}
	out["nodes"] = briefs

	writeJSON(w, out)
}

// actGroupReqBody 组装 GetGroup 请求（1=id, 2=uid 空串）
func actGroupReqBody(id int64) []byte {
	b := proto.NewBuilder()
	b.FieldInt64(1, id)
	b.FieldString(2, "")
	return b.Bytes()
}

// findMilkTeaShop 递归查找活动树中第一个携带 field121 店铺状态的节点
func findMilkTeaShop(n *ActivityNode) *MilkTeaShop {
	if n == nil {
		return nil
	}
	if n.MilkTeaShop != nil {
		return n.MilkTeaShop
	}
	for _, c := range n.Children {
		if s := findMilkTeaShop(c); s != nil {
			return s
		}
	}
	return nil
}

// milkTeaNodeBrief 全节点摘要（定位操作节点用）；Raw 为节点自身载荷字段树
// （跳过 children field2 —— 子节点数据在各自条目里，避免重复）
type milkTeaNodeBrief struct {
	ID    int64       `json:"id"`
	Title string      `json:"title"`
	Type  int64       `json:"type"`
	Depth int         `json:"depth"`
	Raw   interface{} `json:"raw,omitempty"`
}

func collectMilkTeaNodes(n *ActivityNode, depth int, st *actTreeStats, out *[]*milkTeaNodeBrief) {
	if n == nil {
		return
	}
	if n.Info != nil {
		b := &milkTeaNodeBrief{ID: n.Info.ID, Title: n.Info.Title, Type: n.Info.Type, Depth: depth}
		if len(n.RawBytes) > 0 && st.ok() {
			b.Raw = nodePayloadBrief(n.RawBytes, st)
		}
		*out = append(*out, b)
	}
	for _, c := range n.Children {
		collectMilkTeaNodes(c, depth+1, st, out)
	}
}

// nodePayloadBrief 节点自身载荷字段树（wire2 递归展开、wire0 取值，跳过 field2/children）
func nodePayloadBrief(raw []byte, st *actTreeStats) interface{} {
	defer func() { _ = recover() }()
	m := map[string]interface{}{}
	for _, f := range readActFields(raw) {
		if f.No == 2 {
			continue
		}
		var val interface{}
		switch f.Wire {
		case 0:
			val = f.Varint
		case 2:
			if st.ok() {
				if sub := buildActTree(f.Bytes, 6, st); sub != nil {
					st.Nodes++
					val = sub
				} else {
					val = actTreeBytes(f.Bytes)
				}
			} else {
				val = actTreeBytes(f.Bytes)
			}
		default:
			continue
		}
		actTreeAppend(m, "f"+strconv.Itoa(f.No), val)
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// ----- POST /api/activity/milktea/probe 协议探测（游戏内抓包用） -----
//
// 组装 ActivityService.Operate{1:id, 2:cmd, ext_field:ext_hex} 发往游戏网关，
// 回包原文以字段树返回（错误时带回服务端报错原文）。用于定位制作奶茶/领取员工收益
// 等操作的真实 id/cmd 组合；探测历史记录在浏览器端，避免误发业务操作请先在诊断页确认节点。
func handleMilkTeaProbe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccountID string `json:"accountId"`
		ID        int64  `json:"id"`
		Cmd       int64  `json:"cmd"`
		ExtField  int    `json:"ext_field"`
		ExtHex    string `json:"ext_hex"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8192))
	if err == nil && len(body) > 0 {
		_ = json.Unmarshal(body, &req)
	}
	accountID := resolveAccountIDWithOwner(r, req.AccountID)
	if accountID == "" {
		writeJSONMap(w, "ok", false, "error", "缺少 accountId")
		return
	}
	sent := map[string]interface{}{"id": req.ID, "cmd": req.Cmd, "ext_field": req.ExtField, "ext_hex": req.ExtHex}

	b := proto.NewBuilder()
	b.FieldInt64(1, req.ID)
	b.FieldInt64(2, req.Cmd)
	if req.ExtField > 0 && req.ExtHex != "" {
		if raw, hexErr := hex.DecodeString(strings.TrimSpace(req.ExtHex)); hexErr == nil {
			b.FieldBytes(req.ExtField, raw)
		} else {
			writeJSON(w, map[string]interface{}{"ok": false, "account": accountID, "sent": sent, "error": "ext_hex 不是合法十六进制: " + hexErr.Error()})
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	rep, err := rpcRequest(ctx, accountID, actSvc, "Operate", b.Bytes(), 15*time.Second)
	if err != nil {
		writeJSON(w, map[string]interface{}{
			"ok":      false,
			"account": accountID,
			"sent":    sent,
			"error":   actErrMsg(err),
		})
		return
	}
	st := &actTreeStats{Max: 800}
	writeJSON(w, map[string]interface{}{
		"ok":      true,
		"account": accountID,
		"sent":    sent,
		"reply":   buildActTree(rep, 10, st),
	})
}
