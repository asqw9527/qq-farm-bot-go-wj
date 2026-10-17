# User Instruction Memory

This file records user instructions, preferences, and teachings for reference in future interactions.

## Format

### User Instruction Entry
[User Instruction Summary]
- Date: 2026-09-25
- Context: 推送到仓库前更新助手版本
- Instructions:
  - 每次执行 git push 前，先重新生成 version.go（运行 bash install.sh 或手动执行：VERSION=$(git rev-parse --short HEAD) && printf 'package main\n\nvar buildVersion = "%s"\nvar buildTime = "%s"\n' "$VERSION" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > version.go）
  - 然后构建前端（cd web && npm run build）
  - 再编译后端（go build -o go-farm-bot .）
  - 最后才执行 git add -A && git commit && git push

### Project Knowledge Entry
[Project Knowledge Summary]
- Date: 2026-09-25
- Context: Agent 发现的项目构建规范
- Category: Build Methods
- Instructions:
  - Go 模块名仍是 github.com/Aoluis1005/go-farm-bot（与 GitHub 仓库名 qq-farm-bot-go-wy 不同）
  - 可执行文件名为 go-farm-bot，安装到 /opt/go-farm-bot
  - 前端构建产物嵌入后端二进制（//go:embed all:web/dist），必须先构建前端再编译后端
  - 助手版本号由 /api/health 接口返回，格式为 buildTime（UTC ISO 8601），前端转换为北京时间显示
- 推送前版本更新流程：先更新 version.go，再前端 build，再 go build

### Project Knowledge Entry
[Project Knowledge Summary]
- Date: 2026-09-27
- Context: Agent 发现的作物展示与合种逻辑
- Category: Troubleshooting & Debugging
- Instructions:
  - 月下美人：种子 21404，plant 1021404，fruit 41404，asset Crop_1404；Plant.size=null 导致展示图缺失，需 game_images.go 路径编码 + itemNameAliases 绕过 JSON 优先级
  - 2x2 合种协议：游戏网格 4×6（farmGridCols=4），锚点为左下角 master。合法组 [master, master+1, master-4, master-3]，例如 [5,6,1,2]、[23,24,19,20]。旧 6 列左上角组如 [1,2,7,8] 会触发 1001052。Plant 只发完整四格；失败后 reservedLandSet() 挡住 1x1 补种。开关 prioritize2x2Crops 默认关闭。

### Project Knowledge Entry
[Project Knowledge Summary]
- Date: 2026-10-16
- Context: Agent 排查活动种子/装饰无图无名
- Category: Troubleshooting & Debugging
- Instructions:
  - 活动物品权威名来源：`GET /api/activity/shop?id=<活动id>`（或 /api/activity/group）兑换商店项的 name（proto field7）由服务端下发；未同步进 ItemInfo.json 的活动物品可据此补进 game_config.go 的 extraItemNames。
  - 活动装饰/货币图标（如 1041 奶茶券、1042 宣传喇叭、2163 头像框、202011/203012/205011/206011/207012/208012 缤纷奶茶套装、210011-210017 套装）属最新 mainscene bundle；本地 cdn-settings.json 的 mainscene 版本（9e333）不含这些 S4 资源，需先更新抓包快照（wxgame_out 下最新 settings.*.json 的 assets.bundleVers）再跑素材同步才能取到图。
  - 种子/物品图片落盘规则：放入 game-config/seed_images_named/ 并命名 `{itemId}_任意.png`，InitImageMap 按文件名数字前缀收录，无需依赖 ItemInfo.asset_name。
  - 本地调试 /api/* 鉴权：Web 默认用户 admin/admin，可用 `POST /api/users/login` 换取 token 后带 `x-user-token` + `x-account-id` 调用（admin 后台登录需另设 ADMIN_PASSWORD 环境变量或 store.json 的 adminPasswordHash）。
