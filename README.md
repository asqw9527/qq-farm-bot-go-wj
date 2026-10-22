# qq-farm-bot-go-wj · QQ农场自动化助手（纯AI零人工）

本项目基于二改：原项目 https://github.com/Aoluis1005/QQ-farm-BOT-GO ，上游二改 https://github.com/wuyoukeji8888-stack/qq-farm-bot-go-wy
本仓库（asqw9527/qq-farm-bot-go-wj）在二改基础上新增：扫码登录微信/QQ 渠道切换、S4 奶茶店铺面板（奶茶热销中）、奶茶经营收益自动收取。

## 功能亮点

- **多账号管理**：支持添加多个农场账号，统一界面管理
- **多用户系统**：支持多用户登录、注册、卡密激活，权限分为 admin / user
- **卡密系统**：时间卡用于注册与续期，额度卡用于提升账号上限
- **自动巡查**：自动偷菜、帮忙、收菜
- **活动中心**：查看活动状态，手动领取秋祈良愿、快乐不独享等奖励
- **Web 管理面板**：响应式设计，支持移动端访问
- **一键部署**：install.sh 自动安装依赖、编译前后端
- **Docker 支持**：docker-compose 一键启动，数据持久化

### 多用户与卡密系统

- **用户注册/登录**：注册必须使用时间卡密；密码至少 6 位（允许纯数字），经 PBKDF2 加密存储
- **角色权限**：admin 拥有卡密管理、用户管理全部权限；user 受卡密时长和账号上限限制
- **时间卡**：用于注册或续期，按天数延长使用期限
- **额度卡**：登录后在「后台」使用，提升可绑定的农场账号数量（默认上限 2）
- **管理员操作**：在「更多 → 卡密管理」批量生成时间卡/额度卡；在「用户管理」调整到期时间、账号上限（支持无限时长/无限账号）、重置用户密码、删除用户
- **免费领取**：注册页可领取空闲时间卡，同一 UA 24 小时限领 1 张

### API 接口概览

| 接口 | 方法 | 说明 |
|------|------|------|
| `/api/users/register` | POST | 用户注册（必须时间卡密） |
| `/api/users/login` | POST | 用户登录 |
| `/api/users/me` | GET | 获取当前用户信息 |
| `/api/users/renew` | POST | 使用时间卡续期或额度卡提升上限 |
| `/api/users/change-password` | POST | 修改登录密码 |
| `/api/users/claim-card` | POST | 领取空闲时间卡（按 UA 防刷） |
| `/api/admin/cards` | GET/POST | 管理员卡密列表、生成、启用/禁用、删除 |
| `/api/admin/users` | GET/POST/DELETE | 管理员用户列表、修改时限/上限、重置密码、删除 |

### 安全加固（传输加密 + 账号数据加密）

**① 传输加密（HTTPS/TLS）**

管理面板默认以明文 HTTP 提供，登录口令与账号凭据会明文传输。现已支持 TLS：

| 方式 | 配置 | 说明 |
|------|------|------|
| 正式证书（推荐） | `ADMIN_TLS_CERT=/path/cert.pem` + `ADMIN_TLS_KEY=/path/key.pem` | 可由 Let's Encrypt / 云厂商签发，或前置 Nginx 反代 HTTPS |
| 自签名证书 | `ADMIN_TLS_AUTO=1` | 首次启动在数据目录生成 `cert.pem` / `key.pem`（权限 0600），5 年有效；浏览器首次访问需手动信任 |
| 不启用 | 都不设置 | 仍以 HTTP 启动，日志会打印明文告警 |

`install.sh` 部署时已在 systemd 单元写入 `Environment=ADMIN_TLS_AUTO=1`，即默认 HTTPS。自签名证书可用 `-k` 验证：

```bash
curl -sk https://127.0.0.1:3009/api/health
```

**② accounts.json 落盘加密（AES-256-GCM）**

账号的 `Code`、`apiToken`、`openId`、`uin` 等敏感字段不再明文落盘，改为加密信封：

```json
{ "v": 1, "encrypted": "<base64(nonce|ciphertext)>" }
```

密钥来源（按优先级）：

1. 环境变量 `QQ_FARM_ENC_KEY`（32 字节，hex 64 字符或 base64）——**生产推荐**；
2. 数据目录 `.enckey` 文件，首次运行随机生成，权限 `0600`。

> ⚠️ **务必备份 `.enckey`**：密钥丢失将无法解密已保存的账号。旧的明文 `accounts.json` 会在首次启动时自动迁移为加密格式，无需手工处理。

建议同时收紧数据目录权限：`chmod 700 ~/.qq-farm-bot`。

### 一键部署（推荐，Rocky Linux 9.6 / Debian / Ubuntu）

root 执行即可。Rocky 9 走 dnf，脚本会安装 Go 1.25 与 Node 22、2G 机自动加 1G swap、编译前后端、装到 `/opt/go-farm-bot`、注册 systemd、放行 3009。

```bash
git clone https://github.com/asqw9527/qq-farm-bot-go-wj.git
cd qq-farm-bot-go-wj
bash install.sh
```

部署完成后打开 **`https://<服务器 IP>:3009`**（自签名证书需手动信任），注意云厂商安全组也要放行 TCP 3009。

**后续更新**同样是这两步，脚本会自己先停服务再覆盖、装完自动拉起，不用手动 `systemctl stop`：

```bash
cd qq-farm-bot-go-wy
git pull
sudo bash install.sh
```

更新完想确认线上跑的是哪一版，登录后在个人资料栏以年月日时分秒的方式标记版本更新时间。

`install.sh` 每次都会**自动重新构建前端**（检测并自动安装 Node → `npm ci` + `vite build`）后再编译后端，保证页面主题/样式始终完整。请勿删除或替换 `web/dist`，也不要手动放置旧版可执行文件，否则可能导致页面白屏、无任何 UI 样式。

### Docker 部署（可选）

仓库自带 `Dockerfile` + `docker-compose.yml`，适合没有 systemd 的环境（LXC 容器 / 群晖 / 其他主机），与 install.sh 互不影响：

```bash
# 方式一：docker compose（推荐）
docker compose up -d --build

# 方式二：纯 docker（可选指定版本号，默认 dev）
docker build --build-arg VERSION=$(git rev-parse --short HEAD) -t go-farm-bot .
docker run -d --name go-farm-bot -p 3009:3009 -e ADMIN_PORT=3009 \
  -v qq-farm-bot-data:/root/.qq-farm-bot go-farm-bot
```

- 管理页面：`http://<服务器IP>:3009`
- **数据持久化**：账号/配置/日志全部在容器卷 `qq-farm-bot-data`（挂载到 `/root/.qq-farm-bot`），重建容器不丢数据
- **前端内嵌**：`web/dist` 已随镜像打进二进制，无需额外静态服务器
- **资源目录**：`game-config`（素材配置）与 `yyb-resource`（YYB 扫码）已随镜像复制，无需手动放置
- 升级：`docker compose up -d --build` 重新构建即可；想换版本号先 `FARM_VERSION=<hash> docker compose build`
- 时区无关：日志固定输出北京时间（UTC+8）

## 快速开始

### 编译

```bash
# 需先构建前端（web/dist），否则 embed 会失败
cd web && npm ci && npm run build && cd ..
# Go 1.20+
CGO_ENABLED=0 go build -o go-farm-bot .
```

### 运行

```bash
./go-farm-bot
# 默认监听 :3009
```

### 首次配置

1. 浏览器打开 `http://<服务器IP>:3009`
2. **登录**：使用默认账号 `admin` / `admin` 登录
3. **生成卡密**：管理员进入「更多 → 卡密管理」，生成时间卡（注册/续期）或额度卡（提升账号上限）
4. **注册新用户**：登录页切换到注册，填写用户名、密码和时间卡密
5. **添加农场账号**：进入「账号」页，用扫码、手动 code 或第三方登录绑定 QQ/微信农场号，再打开自动化开关

> **安全提示**：首次登录后请尽快修改默认密码。
> - 默认管理员账号：`admin` / `admin`（首次启动自动创建）
> - 登录后进入「更多 → 后台」修改密码

### 多用户系统

| 角色 | 说明 |
|------|------|
| `admin` | 拥有全部权限，默认账号 `admin/admin` |
| `user` | 受卡密时长和账号上限限制，注册必须使用时间卡 |

登录方式：

```bash
# 1. 注册新用户（必须时间卡密）
curl -X POST http://localhost:3009/api/users/register \
  -H "Content-Type: application/json" \
  -d '{"username":"user1","password":"Pass1234","cardCode":"XXXX-XXXX"}'

# 2. 登录获取 token
curl -X POST http://localhost:3009/api/users/login \
  -H "Content-Type: application/json" \
  -d '{"username":"admin","password":"admin"}'

# 3. 使用 token 访问功能面板
curl http://localhost:3009/api/accounts \
  -H "Authorization: Bearer <token>"
```

### systemd 部署（推荐，裸机）

```ini
# /etc/systemd/system/go-farm-bot.service
[Unit]
Description=QQ Farm Bot (Go)
After=network.target

[Service]
WorkingDirectory=/opt/go-farm-bot
ExecStart=/opt/go-farm-bot/go-farm-bot
Environment=ADMIN_PORT=3009
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl enable --now go-farm-bot
```

## 免责声明

- 仅用于学习与个人自动化研究，请遵守游戏用户协议
- 本项目完全免费

## 相关项目

- 本项目为二改，原项目：https://github.com/Aoluis1005/QQ-farm-BOT-GO —— 本项目的协议参考与功能对照
- 当前仓库：https://github.com/wuyoukeji8888-stack/qq-farm-bot-go-wy
