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
  - 背包优先 2x2 合种：游戏网格 6×4（farmGridCols=6），网页四列只是卡片排版；inferEmpty2x2IDs 按左上角取右/下/右下，末列末行不能成组。错误组 [19 20 23 24] 是 4 列误推，服务端 code=1001052。plantFromShopLands/plantBagSeedsForLands 需用 reservedLandSet() 过滤已预留地块
