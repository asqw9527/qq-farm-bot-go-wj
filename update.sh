#!/bin/bash
# 服务器端一键更新（配合 workbench exec 远程执行，或 SSH 登录后执行）
# 在源码仓库目录运行：bash update.sh
# 前置：服务器已安装 Go（能 go build）、Node（能 npm run build），且 install.sh 曾成功部署
set -e

SRC_DIR="$(cd "$(dirname "$0")" && pwd)"        # 源码仓库 /opt/qq-farm-bot-go-wy
DES_BIN="/opt/go-farm-bot/go-farm-bot"          # 运行二进制（install.sh 约定）
SVC="go-farm-bot"                              # systemd 服务名

cd "$SRC_DIR"
echo "[update] 当前目录: $SRC_DIR"

echo "[update] 1/4 拉取最新代码 ..."
git pull --ff-only

echo "[update] 2/4 构建前端 web/dist ..."
cd "$SRC_DIR/web"
npm install --no-audit --no-fund
npm run build
cd "$SRC_DIR"

echo "[update] 3/4 编译后端二进制 ..."
go build -ldflags="-s -w" -o "$DES_BIN" .
chmod +x "$DES_BIN"
echo "[update] 已写入 $DES_BIN"

echo "[update] 4/4 重启服务 ..."
if command -v systemctl >/dev/null 2>&1 && systemctl list-unit-files "${SVC}.service" >/dev/null 2>&1; then
  systemctl restart "$SVC"
  sleep 2
  systemctl --no-pager --full status "$SVC" | head -8
else
  echo "[update] 未检测到 systemd 服务，尝试 farm.sh restart ..."
  "$SRC_DIR/farm.sh" restart
fi

echo "[update] 完成。日志：journalctl -u $SVC -f 或 /tmp/go-farm-bot.log"
