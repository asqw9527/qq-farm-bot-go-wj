package main

import (
	"os"
	"path/filepath"
	"testing"
)

// snapshotConfigGlobals 保存/恢复 game_config 全局映射，
// 避免单测之间以及与其他测试文件互相污染配置状态。
func snapshotConfigGlobals() func() {
	oldSeedToPlant, oldFruitToPlant, oldPlantByID := seedToPlantMap, fruitToPlantMap, plantByIDMap
	oldItemInfo, oldSeedItemSet, oldMutant := itemInfoMap, seedItemSet, mutantEffectMap
	oldFruitItemMap := fruitItemMap
	return func() {
		seedToPlantMap, fruitToPlantMap, plantByIDMap = oldSeedToPlant, oldFruitToPlant, oldPlantByID
		itemInfoMap, seedItemSet, mutantEffectMap = oldItemInfo, oldSeedItemSet, oldMutant
		fruitItemMap = oldFruitItemMap
	}
}

func TestIsFruitItemID(t *testing.T) {
	defer snapshotConfigGlobals()()
	fruitToPlantMap = map[int]plantEntry{41001: {ID: 101, Name: "草莓"}}
	if !isFruitItemID(41001) {
		t.Fatal("已登记果实应命中")
	}
	if isFruitItemID(99999) {
		t.Fatal("未登记果实不应命中")
	}
	if isFruitItemID(0) || isFruitItemID(-1) {
		t.Fatal("非正数应直接返回 false")
	}
}

func TestIsSeedItemID(t *testing.T) {
	defer snapshotConfigGlobals()()
	seedItemSet = map[int]bool{20435: true}
	seedToPlantMap = map[int]plantEntry{20500: {ID: 500, SeedID: 20500}}
	if !isSeedItemID(20435) {
		t.Fatal("ItemInfo type5 种子应命中")
	}
	if !isSeedItemID(20500) {
		t.Fatal("Plant.json seed_id 应命中")
	}
	if !isSeedItemID(20001) || !isSeedItemID(29999) {
		t.Fatal("启发式 20001~29999 范围内应命中")
	}
	if isSeedItemID(20000) || isSeedItemID(30000) {
		t.Fatal("启发式范围边界外不应命中")
	}
	if isSeedItemID(0) {
		t.Fatal("0 不应命中")
	}
}

func TestIsFertilizerItemID(t *testing.T) {
	defer snapshotConfigGlobals()()
	itemInfoMap = map[int]itemInfoEntry{
		5001: {ID: 5001, InteractionType: "fertilizer"},
		5002: {ID: 5002, InteractionType: "fertilizerpro"},
		5003: {ID: 5003, InteractionType: "seed"},
	}
	if !isFertilizerItemID(100003) {
		t.Fatal("硬编码化肥段应命中")
	}
	if isFertilizerItemID(1001) || isFertilizerItemID(1002) {
		t.Fatal("1001/1002 应被排除")
	}
	if !isFertilizerItemID(5001) || !isFertilizerItemID(5002) {
		t.Fatal("interaction_type 为化肥类应命中")
	}
	if isFertilizerItemID(5003) {
		t.Fatal("非化肥 interaction_type 不应命中")
	}
}

func TestPlantForSeedFallback(t *testing.T) {
	defer snapshotConfigGlobals()()
	seedToPlantMap = map[int]plantEntry{20435: {ID: 435, SeedID: 20435, Size: 2, Name: "山丹丹"}}
	seedItemSet = map[int]bool{26666: true}

	p, ok := plantForSeed(20435)
	if !ok || p.Size != 2 || p.Name != "山丹丹" {
		t.Fatalf("已登记种子应原样返回: %+v", p)
	}
	p, ok = plantForSeed(26666)
	if !ok || p.Size != 1 {
		t.Fatalf("ItemInfo 种子缺 Plant 映射时应按 1x1 兜底: %+v", p)
	}
	p, ok = plantForSeed(27777)
	if !ok || p.Size != 1 {
		t.Fatalf("20001~29999 未登记种子应按 1x1 兜底: %+v", p)
	}
	if _, ok = plantForSeed(123); ok {
		t.Fatal("未知 id 不应兜底")
	}
}

func TestItemNameAliasesPriority(t *testing.T) {
	defer snapshotConfigGlobals()()
	itemInfoMap = map[int]itemInfoEntry{
		1041: {ID: 1041, Name: "官方名"},
		999:  {ID: 999, Name: "官方999"},
	}
	if got := itemDisplayName(1041); got != "奶茶券" {
		t.Fatalf("别名应覆盖官方名, got=%q", got)
	}
	if got := seedPlantName(1041); got != "奶茶券" {
		t.Fatalf("seedPlantName 别名应优先, got=%q", got)
	}
	if got := fruitPlantName(1041); got != "奶茶券" {
		t.Fatalf("fruitPlantName 别名应优先, got=%q", got)
	}
	if got := itemDisplayName(999); got != "官方999" {
		t.Fatalf("无别名时应回退 ItemInfo 名, got=%q", got)
	}
	if got := itemDisplayName(888); got != "" {
		t.Fatalf("完全未知物品应返回空, got=%q", got)
	}
}

func TestFruitPlantNameLayers(t *testing.T) {
	defer snapshotConfigGlobals()()
	itemNameAliases = map[int]string{} // 本用例验证非别名层
	fruitItemMap = map[int64]fruitItem{40002: {Name: "白萝卜"}}
	fruitToPlantMap = map[int]plantEntry{40003: {ID: 3, Name: "胡萝卜"}}
	if got := fruitPlantName(40002); got != "白萝卜" {
		t.Fatalf("fruitItemMap 层失败: %q", got)
	}
	if got := fruitPlantName(40003); got != "胡萝卜" {
		t.Fatalf("fruitToPlantMap 兜底层失败: %q", got)
	}
	if got := fruitPlantName(49999); got != "" {
		t.Fatalf("未知果实应返回空: %q", got)
	}
}

func TestFillMissingSeedPlants(t *testing.T) {
	defer snapshotConfigGlobals()()
	seedItemSet = map[int]bool{20001: true, 20002: true}
	seedToPlantMap = map[int]plantEntry{20001: {ID: 1, SeedID: 20001, Size: 1}}
	itemInfoMap = map[int]itemInfoEntry{20002: {ID: 20002, Name: "新种子"}}
	fillMissingSeedPlants()
	p, ok := seedToPlantMap[20002]
	if !ok || p.Size != 1 || p.Name != "新种子" {
		t.Fatalf("缺映射种子应被登记且名称取自 ItemInfo: %+v", p)
	}
	if seedToPlantMap[20001].ID != 1 {
		t.Fatal("已有映射不应被覆盖")
	}
}

func TestLoadPlantJSONNormalizesSize(t *testing.T) {
	defer snapshotConfigGlobals()()
	path := filepath.Join(t.TempDir(), "Plant.json")
	data := `[{"id":101,"name":"A","seed_id":20101,"grow_phases":"种子:5760;成熟:0;","size":0,"fruit":{"id":41001,"count":3}},` +
		`{"id":102,"name":"B","seed_id":20102,"size":2}]`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	loadPlantJSON(path)
	if seedToPlantMap[20101].Size != 1 {
		t.Fatalf("size=0 应归一为 1, got=%d", seedToPlantMap[20101].Size)
	}
	if seedToPlantMap[20102].Size != 2 {
		t.Fatalf("size=2 应保持, got=%d", seedToPlantMap[20102].Size)
	}
	if fruitToPlantMap[41001].SeedID != 20101 {
		t.Fatal("fruit.id -> plant 映射缺失")
	}
	if plantByIDMap[102].Name != "B" {
		t.Fatal("plant.id -> plant 映射缺失")
	}
}

func TestGetPlantGrowTime(t *testing.T) {
	defer snapshotConfigGlobals()()
	plantByIDMap = map[int]plantEntry{
		1: {ID: 1, GrowPhases: "种子:5760;发芽:5760;成熟:0;"},
		2: {ID: 2, GrowPhases: ""},
	}
	if got := getPlantGrowTime(1); got != 11520 {
		t.Fatalf("各阶段求和错误, got=%d", got)
	}
	if got := getPlantGrowTime(2); got != 0 {
		t.Fatalf("空 grow_phases 应为 0, got=%d", got)
	}
	if got := getPlantGrowTime(999); got != 0 {
		t.Fatalf("未知植物应为 0, got=%d", got)
	}
}

func TestMutantIconShort(t *testing.T) {
	cases := []struct{ in, want string }{
		{"gui/texture/mutant/icon/frozen/spriteFrame", "frozen"},
		{"frozen", "frozen"},
		{"", ""},
	}
	for _, c := range cases {
		if got := mutantIconShort(c.in); got != c.want {
			t.Fatalf("mutantIconShort(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

func TestGetMutantEffectsByIDs(t *testing.T) {
	defer snapshotConfigGlobals()()
	mutantEffectMap = map[int]mutantEffectEntry{
		1: {ID: 1, Name: "冰冻", EffectName: "冰冻", Icon: "gui/texture/mutant/icon/frozen/spriteFrame", Tag: "tag"},
	}
	out := getMutantEffectsByIDs([]int64{1, 2})
	if len(out) != 1 {
		t.Fatalf("未知 id 应被跳过, len=%d", len(out))
	}
	if out[0].Icon != "frozen" {
		t.Fatalf("icon 应转短名, got=%q", out[0].Icon)
	}
}
