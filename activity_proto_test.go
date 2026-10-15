package main

import (
	"testing"

	"github.com/Aoluis1005/go-farm-bot/proto"
)

func TestReadActFieldsRoundTrip(t *testing.T) {
	nested := proto.NewBuilder()
	nested.FieldInt64(1, 42)
	b := proto.NewBuilder()
	b.FieldInt64(1, 7)
	b.FieldString(4, "标题")
	b.FieldMessage(2, nested.Bytes())
	b.FieldBytes(5, []byte{0x01, 0x02})
	fs := readActFields(b.Bytes())
	if got := actNum(fs, 1); got != 7 {
		t.Fatalf("field1 varint=%d", got)
	}
	if got := actStr(fs, 4); got != "标题" {
		t.Fatalf("field4 string=%q", got)
	}
	nfs := readActFields(actBytes(fs, 2))
	if got := actNum(nfs, 1); got != 42 {
		t.Fatalf("嵌套 field1 varint=%d", got)
	}
	if got := actBytes(fs, 5); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("field5 bytes=%v", got)
	}
}

func TestReadActFieldsMalformedNoPanic(t *testing.T) {
	// 声明长度 5 但实际只剩 1 字节：应安全返回已收集字段而非 panic
	fs := readActFields([]byte{0x08, 0x07, 0x0a, 0x05, 0x01})
	if len(fs) != 1 || actNum(fs, 1) != 7 {
		t.Fatalf("截断输入应返回截断前字段, got=%+v", fs)
	}
}

func TestActAccessors(t *testing.T) {
	b := proto.NewBuilder()
	b.FieldInt64(1, 11)
	b.FieldMessage(2, []byte{0x01})
	b.FieldMessage(2, []byte{0x02})
	if got := actNum(readActFields(b.Bytes()), 99); got != 0 {
		t.Fatalf("缺失 varint 字段应返回 0, got=%d", got)
	}
	all := actBytesAll(readActFields(b.Bytes()), 2)
	if len(all) != 2 {
		t.Fatalf("repeated bytes 应全部收集, len=%d", len(all))
	}
}

func TestCollectBytesNested(t *testing.T) {
	inner := proto.NewBuilder()
	inner.FieldInt64(1, 2)
	mid := proto.NewBuilder()
	mid.FieldMessage(102, inner.Bytes())
	outer := proto.NewBuilder()
	outer.FieldMessage(102, inner.Bytes())
	outer.FieldMessage(8, mid.Bytes())
	got := collectBytes(outer.Bytes(), 102, 8)
	if len(got) != 2 {
		t.Fatalf("递归扫描应同时命中浅层与深层 102 块, len=%d", len(got))
	}
}

func TestParseActivityInfo(t *testing.T) {
	b := proto.NewBuilder()
	b.FieldInt64(1, 2026110100)
	b.FieldInt64(3, 23)
	b.FieldString(4, "奶茶热销中")
	b.FieldInt64(6, 100)
	b.FieldInt64(7, 200)
	b.FieldInt64(20, 1)
	b.FieldInt64(22, 1)
	info := parseActivityInfo(readActFields(b.Bytes()))
	if info.ID != 2026110100 || info.Type != 23 || info.Title != "奶茶热销中" {
		t.Fatalf("基础字段解析错误: %+v", info)
	}
	if info.StartTime != 100 || info.EndTime != 200 {
		t.Fatalf("起止时间解析错误: %+v", info)
	}
	if !info.Visible || !info.Enabled {
		t.Fatal("visible/enabled 应为 true")
	}
}

func TestParseShopItemFull(t *testing.T) {
	defer snapshotConfigGlobals()()
	itemInfoMap = map[int]itemInfoEntry{}
	item := proto.NewBuilder()
	item.FieldInt64(1, 80002)
	item.FieldInt64(2, 5)
	cost := proto.NewBuilder()
	cost.FieldInt64(1, 1041)
	cost.FieldInt64(2, 10)
	it := proto.NewBuilder()
	it.FieldInt64(1, 301)
	it.FieldMessage(2, item.Bytes())
	it.FieldMessage(3, cost.Bytes())
	it.FieldInt64(4, 2)
	it.FieldInt64(6, 5)
	it.FieldString(7, "普通化肥")
	got := parseShopItemFull(it.Bytes())
	if got == nil {
		t.Fatal("应解析出商品")
	}
	if got.ID != 301 || got.ItemID != 80002 || got.Count != 5 {
		t.Fatalf("id/物品/数量错误: %+v", got)
	}
	if got.CurrencyID != 1041 || got.Price != 10 {
		t.Fatalf("货币/价格错误: %+v", got)
	}
	if got.Name != "普通化肥" || got.Sort != 5 {
		t.Fatalf("名称/sort 错误: %+v", got)
	}
	// 1041 是 S4 活动货币别名，币名应显示为奶茶券
	if got.CurrencyName != "奶茶券" {
		t.Fatalf("CurrencyName=%q, want 奶茶券", got.CurrencyName)
	}
	if got.StatusLabel != "可兑换" {
		t.Fatalf("status=2 默认应可兑换, got=%q", got.StatusLabel)
	}
}

func TestParseItem(t *testing.T) {
	defer snapshotConfigGlobals()()
	itemInfoMap = map[int]itemInfoEntry{}
	b := proto.NewBuilder()
	b.FieldInt64(1, 999999)
	it := parseItem(b.Bytes())
	if it == nil || it.ItemID != 999999 {
		t.Fatalf("解析失败: %+v", it)
	}
	if it.Count != 1 {
		t.Fatalf("缺失 count 应默认 1, got=%d", it.Count)
	}
	if it.Name != "物品999999" {
		t.Fatalf("未知物品名应为 物品{id}, got=%q", it.Name)
	}
}

func TestParseActivityList(t *testing.T) {
	e1 := proto.NewBuilder()
	e1.FieldInt64(1, 101)
	e1.FieldString(2, "活动A")
	e1.FieldInt64(3, 1)
	e1.FieldInt64(4, 2)
	e2 := proto.NewBuilder()
	e2.FieldInt64(1, 102)
	e2.FieldString(2, "活动B")
	body := proto.NewBuilder()
	body.FieldBytes(1, []byte{0xff})
	body.FieldMessage(2, e1.Bytes())
	body.FieldMessage(2, e2.Bytes())
	list := ParseActivityList(body.Bytes())
	if len(list) != 2 {
		t.Fatalf("应解析出 2 个活动, len=%d", len(list))
	}
	if list[0].ID != 101 || list[0].Title != "活动A" || list[0].StartTime != 1 || list[0].EndTime != 2 {
		t.Fatalf("条目1字段错误: %+v", list[0])
	}
}

func TestParseActivityGroupTree(t *testing.T) {
	defer snapshotConfigGlobals()()
	itemInfoMap = map[int]itemInfoEntry{}
	shopItem := proto.NewBuilder()
	shopItem.FieldInt64(1, 301)
	shop := proto.NewBuilder()
	shop.FieldMessage(1, shopItem.Bytes())

	childInfo := proto.NewBuilder()
	childInfo.FieldInt64(1, 2026110102)
	childInfo.FieldString(4, "商店")
	child := proto.NewBuilder()
	child.FieldMessage(1, childInfo.Bytes())

	rootInfo := proto.NewBuilder()
	rootInfo.FieldInt64(1, 2026110100)
	rootInfo.FieldString(4, "奶茶热销中")
	root := proto.NewBuilder()
	root.FieldMessage(1, rootInfo.Bytes())
	root.FieldMessage(2, child.Bytes())
	root.FieldMessage(102, shop.Bytes())

	reply := proto.NewBuilder()
	reply.FieldMessage(1, root.Bytes())
	node := ParseActivityGroup(reply.Bytes())
	if node == nil || node.Info == nil || node.Info.Title != "奶茶热销中" {
		t.Fatalf("根节点解析失败: %+v", node)
	}
	if len(node.Children) != 1 || node.Children[0].Info == nil || node.Children[0].Info.ID != 2026110102 {
		t.Fatalf("子节点解析失败: %+v", node.Children)
	}
	if len(node.ExchangeShop) != 1 || node.ExchangeShop[0].ID != 301 {
		t.Fatalf("field102 商店解析失败: %+v", node.ExchangeShop)
	}
	if ParseActivityGroup([]byte{}) != nil {
		t.Fatal("空 body 应返回 nil")
	}
}

func TestExchangeShopStatusLabel(t *testing.T) {
	cases := []struct {
		it   *ShopItem
		want string
	}{
		{&ShopItem{Owned: true}, "已拥有"},
		{&ShopItem{IsRepeatable: true, Owned: true}, "可兑换"},
		{&ShopItem{ExchangeLimit: 3}, "可兑换"},
		{&ShopItem{Status: 3}, "已售"},
		{&ShopItem{Status: 5}, "特殊商品"},
	}
	for _, c := range cases {
		if got := exchangeShopStatusLabel(c.it); got != c.want {
			t.Fatalf("statusLabel(%+v)=%q, want %q", c.it, got, c.want)
		}
	}
}

func TestSolarStatusLabel(t *testing.T) {
	cases := []struct {
		status int64
		want   string
	}{
		{2, "可领取"},
		{3, "已领取"},
		{1, "未开启"},
		{5, "已结束"},
		{9, "状态9"},
	}
	for _, c := range cases {
		if got := solarStatusLabel(c.status); got != c.want {
			t.Fatalf("solarStatusLabel(%d)=%q, want %q", c.status, got, c.want)
		}
	}
}
