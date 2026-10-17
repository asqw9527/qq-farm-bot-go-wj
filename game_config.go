package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ============================================================
// 游戏配置加载
// 用于背包物品分类：fruit / seed / props / other / fertilizer
// Plant.json + ItemInfo.json 位于服务器 game-config/ 目录；本地缺失时回退启发式规则。
// ============================================================

// plantEntry Plant.json 条目
type plantEntry struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	SeedID     int    `json:"seed_id"`
	Seasons    int    `json:"seasons"`
	GrowPhases string `json:"grow_phases"` // "种子:5760;发芽:5760;...;成熟:0;"
	Size       int    `json:"size"`        // 合种尺寸（2=2x2）可空
	Exp        int    `json:"exp"`         // 单次收获经验
	Fruit      *struct {
		ID    int `json:"id"`
		Count int `json:"count"` // 单次收获果实数量（用于分析金币计算）
	} `json:"fruit"`
}

// mutantEffectEntry MutantEffect.json 条目
type mutantEffectEntry struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	EffectName string `json:"effect_name"`
	Icon       string `json:"icon"`
	Tag        string `json:"tag"`
}

// itemInfoEntry ItemInfo.json 条目
type itemInfoEntry struct {
	ID              int     `json:"id"`
	Type            int     `json:"type"` // 5=种子物品；4=果实物品；6=果实/装扮
	Name            string  `json:"name"`
	Price           float64 `json:"price"`
	Level           int     `json:"level"`
	PriceID         int     `json:"price_id"`
	InteractionType string  `json:"interaction_type"`
	EffectDesc      string  `json:"effect_desc"` //
	Layer           int     `json:"layer"`       // 果实层级（图鉴 getFruitLayerByFruitId 用）
	AssetName       string  `json:"asset_name"`  // 图鉴/物品图片映射用
}

// 运行期从配置文件建立的映射
var (
	seedToPlantMap  = map[int]plantEntry{} // seed_id -> 植物
	fruitToPlantMap = map[int]plantEntry{} // fruit.id -> 植物
	plantByIDMap    = map[int]plantEntry{} // plant.id -> 植物
	itemInfoMap     = map[int]itemInfoEntry{}
	seedItemSet     = map[int]bool{}              // ItemInfo type==5 的种子物品 id
	mutantEffectMap = map[int]mutantEffectEntry{} // mutant id -> 效果
)

// extraItemNames 官方 ItemInfo.json 未收录的物品（新活动临时道具，配置未同步），
// 手动登记名称——缺了它们背包里只会显示一串 id，无法辨认。
// 图片素材暂缺，前端按无图处理；等官方补进 ItemInfo.json 后本表可安全移除（JSON 优先，见 loadItemInfoJSON）。
var extraItemNames = map[int]string{
	20883: "小红花种子", // 公益小红花：完成每日任务/每日分享获得，种下后结小红花果实
	1040:  "爱心值",   // 公益小红花：收获小红花果实获得；捐赠后从背包扣除（抓包 ItemNotify 1040 -7 实锤）
	20435: "山丹丹种子", // 山丹丹/月下美人活动变体
	26030: "月下美人种子", // 月下美人活动变体
	// S4「森野茶香季」活动种子（ItemInfo 未同步，名称取自兑换商店）
	20021: "森野可可豆种子",
	20525: "森野甜莓种子",
	20526: "森野白桃种子",
	20527: "森野甜橙种子",
	20528: "森野红柚种子",
	20529: "森野芭乐种子",
	21435: "森野柿子种子",
	29005: "森野黄金芽种子",
	// S4「奶茶热销中」兑换商店装饰（ItemInfo 未同步）
	2163:   "缤纷奶茶头像框",
	202011: "缤纷奶茶木牌",
	203012: "缤纷奶茶栅栏",
	205011: "缤纷奶茶狗屋",
	206011: "缤纷奶茶仓库",
	207012: "缤纷奶茶街道",
	208012: "缤纷奶茶围栏",
	210011: "屋顶套装",
	210012: "门套装",
	210013: "窗套装",
	210014: "墙面套装",
	210015: "地板套装",
	210016: "桌椅套装",
	210017: "装饰套装",
}

// itemNameAliases 覆盖官方配置展示名（月光花客户端俗称山丹丹/月下美人）。
var itemNameAliases = map[int]string{
	20435:   "山丹丹种子", // 活动变体
	21404:   "山丹丹种子", // 月下美人/月光花的玩家昵称
	26030:   "月下美人种子", // 活动变体
	41404:   "山丹丹",
	1021404: "山丹丹",
	1121404: "黄金·山丹丹",
	1041404: "黄金·山丹丹",
	1041:    "奶茶券", // S4 森野茶香季活动货币（ItemInfo 未入库）
	1042:    "宣传喇叭", // S4 森野茶香季活动道具
}

// assetNameAliases 覆盖 Plant.json ItemInfo.json 缺失的 asset_name。
// 用于通过种子ID找不到图片时，用作 assetName → seedAssetImageMap 回退。
var assetNameAliases = map[int]string{
	21404: "Crop_1404", // 山丹丹/月下美人种子
	41404: "Crop_1404", // 月下美人果实使用同一图片
}

// initGameConfig 从 gameConfigDir 加载 Plant.json / ItemInfo.json。
// 成功加载后调用方可用 IsFruitItemID / IsSeedItemID / itemName 做精确分类。
func initGameConfig(gameConfigDir string) {
	loadPlantJSON(filepath.Join(gameConfigDir, "Plant.json"))
	loadItemInfoJSON(filepath.Join(gameConfigDir, "ItemInfo.json"))
	loadMutantEffectJSON(filepath.Join(gameConfigDir, "MutantEffect.json"))
	fillMissingSeedPlants()
	if len(seedToPlantMap) > 0 || len(seedItemSet) > 0 {
		log.Printf("[config] 已加载植物配置(%d)与物品配置(%d)", len(seedToPlantMap), len(itemInfoMap))
	} else {
		log.Printf("[config] game-config 缺失，背包分类将使用启发式回退")
	}
}

// fillMissingSeedPlants 把 ItemInfo type=5 但 Plant.json 没有 seed_id 映射的种子
// 登记为 1x1 可种植，避免背包里有种却被 listBagSeeds / pickBagSeed 直接跳过。
func fillMissingSeedPlants() {
	n := 0
	for id := range seedItemSet {
		if _, ok := seedToPlantMap[id]; ok {
			continue
		}
		name := ""
		if it, ok := itemInfoMap[id]; ok {
			name = it.Name
		}
		seedToPlantMap[id] = plantEntry{ID: id, SeedID: id, Size: 1, Name: name}
		n++
	}
	if n > 0 {
		log.Printf("[config] Plant.json 缺映射种子 %d 个，已按 1x1 兜底可自动种植", n)
	}
}

// plantForSeed 按种子物品 ID 取植物配置。Plant.json 缺条目时，ItemInfo 种子或 20001–29999 按 1x1 兜底。
func plantForSeed(seedID int) (plantEntry, bool) {
	if p, ok := seedToPlantMap[seedID]; ok {
		return p, true
	}
	if seedItemSet[seedID] || (seedID >= 20001 && seedID <= 29999) {
		return plantEntry{ID: seedID, SeedID: seedID, Size: 1, Name: seedPlantName(int64(seedID))}, true
	}
	return plantEntry{}, false
}

func loadPlantJSON(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var rows []plantEntry
	if err := json.Unmarshal(data, &rows); err != nil {
		log.Printf("[config] 解析 %s 失败: %v", path, err)
		return
	}
	seedToPlantMap = make(map[int]plantEntry, len(rows))
	fruitToPlantMap = make(map[int]plantEntry, len(rows))
	plantByIDMap = make(map[int]plantEntry, len(rows))
	for _, p := range rows {
		// 占地尺寸归一：JSON size 为 null/0 时按 1（1x1）处理，
		// (1, toNum(plant.size) || 1)。
		// 否则 pickBagSeed/listBagSeeds 的 `Size != 1` 判断会把所有普通 1x1 种子误判为
		// "非1x1" 全部过滤掉，导致背包优先策略落空、回退到商城乱选种子。
		if p.Size <= 0 {
			p.Size = 1
		}
		if p.ID > 0 {
			if _, ok := plantByIDMap[p.ID]; !ok {
				plantByIDMap[p.ID] = p
			}
		}
		if p.SeedID > 0 {
			if _, ok := seedToPlantMap[p.SeedID]; !ok {
				seedToPlantMap[p.SeedID] = p
			}
		}
		if p.Fruit != nil && p.Fruit.ID > 0 {
			if _, ok := fruitToPlantMap[p.Fruit.ID]; !ok {
				fruitToPlantMap[p.Fruit.ID] = p
			}
		}
	}
}

func loadMutantEffectJSON(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var rows []mutantEffectEntry
	if err := json.Unmarshal(data, &rows); err != nil {
		log.Printf("[config] 解析 %s 失败: %v", path, err)
		return
	}
	mutantEffectMap = make(map[int]mutantEffectEntry, len(rows))
	for _, it := range rows {
		if it.ID <= 0 {
			continue
		}
		if _, ok := mutantEffectMap[it.ID]; !ok {
			mutantEffectMap[it.ID] = it
		}
	}
}

func loadItemInfoJSON(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var rows []itemInfoEntry
	if err := json.Unmarshal(data, &rows); err != nil {
		log.Printf("[config] 解析 %s 失败: %v", path, err)
		return
	}
	itemInfoMap = make(map[int]itemInfoEntry, len(rows))
	seedItemSet = map[int]bool{}
	for _, it := range rows {
		if it.ID <= 0 {
			continue
		}
		if _, ok := itemInfoMap[it.ID]; !ok {
			itemInfoMap[it.ID] = it
		}
		if it.Type == 5 {
			seedItemSet[it.ID] = true
		}
	}
	// 补登记官方配置缺失的活动物品（JSON 优先，已存在的不会被覆盖）
	for id, name := range extraItemNames {
		if _, ok := itemInfoMap[id]; !ok {
			itemInfoMap[id] = itemInfoEntry{ID: id, Name: name}
		}
	}
}

// ---- 背包物品分类判定 ----

// isFruitItemID 是否为果实物品（(getPlantByFruitId(id))）
// 统一以 Plant.json 为准：普通果实(4xxxx) 与变异果实(104xxxx) 均已收录于 fruitToPlantMap。
func isFruitItemID(id int64) bool {
	n := int(id)
	if n <= 0 {
		return false
	}
	if _, ok := fruitToPlantMap[n]; ok {
		return true
	}
	return false
}

// isSeedItemID 是否为种子物品（ItemInfo type==5 或 Plant.json seed_id）
func isSeedItemID(id int64) bool {
	n := int(id)
	if n <= 0 {
		return false
	}
	if seedItemSet[n] {
		return true
	}
	if _, ok := seedToPlantMap[n]; ok {
		return true
	}
	// 回退启发式：种子 id 范围 20001~29999
	if n >= 20001 && n <= 29999 {
		return true
	}
	return false
}

// isFertilizerItemID 是否为化肥相关物品
func isFertilizerItemID(id int64) bool {
	n := int(id)
	if n <= 0 {
		return false
	}
	if n == 1001 || n == 1002 {
		return false
	}
	switch n {
	case 100003, 100004, 100005, 100006, 100007, 100008, 100009, 100010, 100011, 100012:
		return true
	}
	if it, ok := itemInfoMap[n]; ok {
		t := string(it.InteractionType)
		return t == "fertilizer" || t == "fertilizerpro"
	}
	return false
}

// fruitPlantName 果实植物名（"草莓" 等，不含"果实"后缀；找不到返回空）
func fruitPlantName(id int64) string {
	if n, ok := itemNameAliases[int(id)]; ok {
		return n
	}
	if fi, ok := fruitItemMap[id]; ok {
		return fi.Name
	}
	if p, ok := fruitToPlantMap[int(id)]; ok {
		return p.Name
	}
	return ""
}

// seedPlantName 种子对应植物名（"草莓" 等；找不到返回空）
// 若 ItemInfo 有 type5 的种子条目则直接用其名（如"草莓种子"）。
func seedPlantName(id int64) string {
	if n, ok := itemNameAliases[int(id)]; ok {
		return n
	}
	if it, ok := itemInfoMap[int(id)]; ok && it.Type == 5 && it.Name != "" {
		return it.Name
	}
	if p, ok := seedToPlantMap[int(id)]; ok {
		return p.Name
	}
	return ""
}

// itemDisplayName 物品展示名（未在 ItemInfo 收录时返回 "物品{id}"）
func itemDisplayName(id int64) string {
	if n, ok := itemNameAliases[int(id)]; ok {
		return n
	}
	if it, ok := itemInfoMap[int(id)]; ok {
		return it.Name
	}
	return ""
}

// ============================================================
// 农场页所需配置查询
// ============================================================

// getPlantByID 按植物ID取植物配置
func getPlantByID(plantID int64) (plantEntry, bool) {
	p, ok := plantByIDMap[int(plantID)]
	return p, ok
}

// getPlantGrowTime 总生长秒数
// grow_phases 形如 "种子:5760;发芽:5760;...;成熟:0;"，取每段 ":(\d+)" 求和
func getPlantGrowTime(plantID int64) int64 {
	p, ok := plantByIDMap[int(plantID)]
	if !ok || p.GrowPhases == "" {
		return 0
	}
	var total int64
	for _, phase := range strings.Split(p.GrowPhases, ";") {
		if phase == "" {
			continue
		}
		if idx := strings.LastIndex(phase, ":"); idx >= 0 {
			if sec, err := strconv.ParseInt(phase[idx+1:], 10, 64); err == nil {
				total += sec
			}
		}
	}
	return total
}

// getPlantNameOrNull 按植物ID取名称（找不到返回空串）
func getPlantNameOrNull(plantID int64) string {
	if n, ok := itemNameAliases[int(plantID)]; ok {
		return n
	}
	if p, ok := plantByIDMap[int(plantID)]; ok {
		return p.Name
	}
	return ""
}

// getMutantEffectsByIDs 变异效果列表
type MutantEffect struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	EffectName string `json:"effect_name"`
	Icon       string `json:"icon"`
	Tag        string `json:"tag"`
}

// mutantIconShort 把 MutantEffect.icon 统一成短名:
// CDN 完整路径 "gui/texture/mutant/icon/frozen/spriteFrame" → "frozen";
// 已是短名(如 "frozen")或无 "/" 的原样返回。
// 前端按 /game-config/seed_images_named/mutant/{icon}.png 拼图, 必须用短名。
func mutantIconShort(icon string) string {
	if icon == "" || !strings.Contains(icon, "/") {
		return icon
	}
	parts := strings.Split(strings.Trim(icon, "/"), "/")
	if len(parts) >= 2 {
		return parts[len(parts)-2]
	}
	return icon
}

func getMutantEffectsByIDs(ids []int64) []MutantEffect {
	out := []MutantEffect{}
	for _, id := range ids {
		it, ok := mutantEffectMap[int(id)]
		if !ok {
			continue
		}
		out = append(out, MutantEffect{ID: it.ID, Name: it.Name, EffectName: it.EffectName, Icon: mutantIconShort(it.Icon), Tag: it.Tag})
	}
	return out
}
