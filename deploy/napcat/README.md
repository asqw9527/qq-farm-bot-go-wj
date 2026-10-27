# NapCat 一键部署（QQ 扫码登录依赖）

QQ 经典农场的登录 code 只能由 QQNT 内核（即 QQ 客户端/NapCat）签发，所以面板的
「QQ 扫码登录」需要一个 NapCat 实例。**推荐与 BOT 同机部署，不需要额外设备**：
手机 QQ 只负责扫一次码授权登录。

## 一键部署

```bash
cd deploy/napcat
bash setup.sh            # 不传参则自动生成随机 WebUI Token
# 或指定 token： bash setup.sh my-token-123
```

脚本做的事：
1. **自动安装 Docker**（若服务器未装，使用阿里云镜像 `get.docker.com --mirror Aliyun`，
   支持 CentOS/Anolis/Ubuntu/Debian；仅支持 **x86_64**，ARM 机器无法运行 QQ NT）。
2. 启动 `mlikiowa/napcat-docker` 容器（WebUI 仅绑定 127.0.0.1:6099）→
   从 [fubiwangluo/qq-code](https://github.com/fubiwangluo/qq-code) Releases 下载
   `napcat-plugin-qq-farm-code` 插件放入 `plugins/` 并启用 → 按需给第三方插件白名单
   打补丁 → 重启生效并打印 Token。

> 若你已手动装好 Docker，脚本会跳过安装步骤直接部署。

## 部署后：面板扫码

1. 面板 →「账号 → 扫码登录 → QQ」
2. NapCat 地址填 `http://127.0.0.1:6099`，WebUI Token 填脚本打印的值，
   勾选「保存为默认」→ 点「生成二维码」→ 手机 QQ 扫码确认。
3. 扫码后 NapCat 登录该 QQ，插件自动签出农场 code 并加入账号列表；
   之后重登、被踢都会自动重签 code 重连，无需再扫码。

## 完全零配置（可选）

给 BOT 的 systemd 服务加环境变量，面板进 QQ 页签直接出二维码：

```ini
# /etc/systemd/system/go-farm-bot.service 的 [Service] 段
Environment=NAPCAT_BASE=http://127.0.0.1:6099
Environment=NAPCAT_WEBUI_TOKEN=<setup.sh 打印的 token>
```

`systemctl daemon-reload && systemctl restart go-farm-bot` 生效。

## 目录说明

| 路径 | 内容 |
|---|---|
| `config/` | NapCat 配置（webui.json、plugins.json、onebot11_*.json） |
| `QQ/` | QQ 登录态，持久化后重启/升级免重扫 |
| `plugins/` | 第三方插件，`napcat-plugin-qq-farm-code` 装在这里 |

## 常见问题

- **插件加载报 `not in official plugin whitelist`**：说明所用 NapCat 版本的官方
  索引尚未收录该插件，按 [qq-code README 方式二](https://github.com/fubiwangluo/qq-code#%E5%AE%89%E8%A3%85)
  手工放开（setup.sh 已自动尝试，版本不同需手工）。
- **扫码提示二维码过期**：面板二维码卡片上点「点击刷新」。
- **QQ 已在 NapCat 在线但取 code 失败**：确认 `plugins/napcat-plugin-qq-farm-code`
  存在且 `config/plugins.json` 已启用；看容器日志 `docker logs napcat-farm`。
- **安全**：WebUI 端口只绑定 127.0.0.1；若需远程访问 NapCat，请走反代 + 鉴权，
  勿裸暴露 6099（能访问者可代取该 QQ 的 code）。

## 原生安装（不使用 Docker，备选）

若不想装 Docker，NapCat 官方提供 Shell 安装（自动附带 QQ）。注意：**官方 Shell
安装仅支持 Ubuntu 20+/Debian 10+，CentOS 系仅支持 9**（旧版 CentOS 7/Anolis 不支持，
会失败）。命令：

```bash
curl -o napcat.sh https://nclatest.znin.net/NapNeko/NapCat-Installer/main/script/install.sh \
  && sudo bash napcat.sh --docker n --cli y
```

安装后需自行配置 WebUI（端口 6099、绑定 127.0.0.1、设 Token）并把
`napcat-plugin-qq-farm-code` 插件放入 `plugins/` 目录、在 `plugins.json` 启用。
推荐优先用上面的 Docker 一键脚本，兼容性最好。
