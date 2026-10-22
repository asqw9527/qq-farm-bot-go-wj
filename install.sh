#!/usr/bin/env bash
# ============================================================
#  QQ Farm Bot GO 一键部署
#
#  用法（root）:  bash install.sh            # 完整安装
#                 bash install.sh update     # 只更新代码、前后端、重启服务
#
#  适配：Rocky Linux 9.x（dnf）以及 Debian/Ubuntu（apt）
#  目标机建议：2C2G、端口 3009、工作目录 /opt/go-farm-bot
# ============================================================
set -euo pipefail

cd "$(dirname "$0")"
SRC="$(pwd)"
DES=/opt/go-farm-bot
GO_VER="1.25.0"
NODE_VER="22.20.0"
ADMIN_PORT="${ADMIN_PORT:-3009}"

# 版本时间取「代码提交到仓库的时间」（HEAD 的提交时间），而非本次编译/部署时间。
# 无 git 环境（如源码包解压）时，沿用 version.go 中已提交的时间；再兜底为当前时间。
commit_time() {
  local t
  t=$(git log -1 --format=%cI 2>/dev/null) || t=""
  if [ -z "$t" ] && [ -f version.go ]; then
    t=$(sed -n 's/^var buildTime = "\(.*\)"/\1/p' version.go 2>/dev/null)
  fi
  if [ -z "$t" ]; then
    t=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  fi
  printf '%s' "$t"
}

if [ "$(id -u)" -ne 0 ]; then
  echo "请用 root 运行: bash install.sh"
  exit 1
fi

echo "=========================================="
echo " QQ Farm Bot GO 一键部署"
echo " 源码: $SRC"
echo " 安装: $DES"
echo " 端口: $ADMIN_PORT"
echo "=========================================="

# 简易 update 模式：只拉取、构建、重启，不重装依赖
if [ "${1:-install}" = "update" ]; then
  echo "[update] 进入快速更新模式..."
  # 拉取最新代码
  if [ -d .git ]; then
    git fetch origin
    git reset --hard origin/master
  fi
  # 前端构建
  echo "[update/1] 构建前端..."
  cd "$SRC/web"
  npm ci || npm install
  npm run build
  cd "$SRC"
  # 编译 Go
  echo "[update/2] 编译后端..."
  VERSION=$(git rev-parse --short HEAD 2>/dev/null || echo dev)
  printf 'package main\n\nvar buildVersion = "%s"\nvar buildTime = "%s"\n' \
    "$VERSION" "$(commit_time)" > version.go
  go build -ldflags="-s -w" -o go-farm-bot .
  # 部署与重启
  mkdir -p "$DES"
  cp -f "$SRC/go-farm-bot" "$DES/"
  chmod +x "$DES/go-farm-bot"
  cp -a "$SRC/game-config" "$DES/" 2>/dev/null || true
  if systemctl list-unit-files go-farm-bot.service >/dev/null 2>&1; then
    systemctl daemon-reload
    systemctl restart go-farm-bot
  else
    pkill -f go-farm-bot || true
    nohup "$DES/go-farm-bot" > /var/log/go-farm-bot.log 2>&1 &
  fi
  sleep 2
  if curl -fsS "http://127.0.0.1:${ADMIN_PORT}/api/health" >/dev/null 2>&1; then
    echo "  health 检查通过"
  else
    echo "  health 暂未就绪，请稍后检查"
  fi
  echo ""
  echo "更新完成"
  exit 0
fi

have_cmd() { command -v "$1" >/dev/null 2>&1; }

pkg_install() {
  if have_cmd dnf; then
    dnf install -y "$@"
  elif have_cmd yum; then
    yum install -y "$@"
  elif have_cmd apt-get; then
    export DEBIAN_FRONTEND=noninteractive
    # Debian/Ubuntu 的 xz 裸名不存在，实际包名为 xz-utils（Alpine/RHEL 才叫 xz）
    local pkgs=()
    local p
    for p in "$@"; do
      if [ "$p" = "xz" ]; then
        p="xz-utils"
      fi
      pkgs+=("$p")
    done
    apt-get update -y
    apt-get install -y "${pkgs[@]}"
  else
    echo "未找到 dnf/yum/apt-get"
    exit 1
  fi
}

# 2G 内存编译前端+Go 可能吃紧，内存不足时加 1G swap
ensure_swap() {
  local mem_kb
  mem_kb=$(awk '/MemTotal/ {print $2}' /proc/meminfo)
  if [ "${mem_kb:-0}" -ge 3500000 ]; then
    return 0
  fi
  if [ "$(awk '/SwapTotal/ {print $2}' /proc/meminfo)" -ge 500000 ]; then
    return 0
  fi
  if [ -f /swapfile-farmbot ]; then
    swapon /swapfile-farmbot 2>/dev/null || true
    return 0
  fi
  echo "  内存较小，创建 1G swap..."
  dd if=/dev/zero of=/swapfile-farmbot bs=1M count=1024 status=none
  chmod 600 /swapfile-farmbot
  mkswap /swapfile-farmbot >/dev/null
  swapon /swapfile-farmbot
}

install_go() {
  local cur=""
  if have_cmd go; then
    cur=$(go env GOVERSION 2>/dev/null || true)
  fi
  case "$cur" in
    go1.25*) echo "  Go 已就绪: $cur"; return 0 ;;
  esac
  echo "  安装 Go ${GO_VER}..."
  mkdir -p /usr/local
  if [ -d /usr/local/go ]; then
    mv /usr/local/go "/usr/local/go.bak.$(date +%s)"
  fi
  curl -fsSL "https://go.dev/dl/go${GO_VER}.linux-amd64.tar.gz" -o /tmp/go.tgz
  tar -C /usr/local -xzf /tmp/go.tgz
  ln -sf /usr/local/go/bin/go /usr/local/bin/go
  ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt
  echo "  Go: $(go version)"
}

install_node() {
  local major=""
  if have_cmd node && have_cmd npm; then
    major=$(node -p "process.versions.node.split('.')[0]" 2>/dev/null || echo 0)
    if [ "${major:-0}" -ge 18 ]; then
      echo "  Node 已就绪: $(node -v)"
      return 0
    fi
  fi
  echo "  安装 Node ${NODE_VER}..."
  mkdir -p /usr/local
  # 优先使用国内镜像，避免 nodejs.org 直链下载超时
  local node_url="https://npmmirror.com/mirrors/node/v${NODE_VER}/node-v${NODE_VER}-linux-x64.tar.xz"
  curl -fsSL --connect-timeout 10 --max-time 120 "$node_url" -o /tmp/node.tar.xz
  tar -C /usr/local --strip-components=1 -xJf /tmp/node.tar.xz
  hash -r || true
  echo "  Node: $(node -v)  npm: $(npm -v)"
}

# ---- 0. 基础依赖 ----
echo "[0/6] 安装基础依赖..."
pkg_install git curl tar xz unzip ca-certificates
ensure_swap
install_go
install_node

export PATH="/usr/local/go/bin:/usr/local/bin:$PATH"
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
export GO111MODULE=on
export CGO_ENABLED=0
export NODE_OPTIONS="${NODE_OPTIONS:---max-old-space-size=768}"
npm config set registry https://registry.npmmirror.com >/dev/null 2>&1 || true

# 运行数据：有独立数据盘 /data 时放到数据盘
DATA_HOME="/root"
if [ -d /data ] && [ -w /data ]; then
  mkdir -p /data/qq-farm-bot-home
  DATA_HOME="/data/qq-farm-bot-home"
  echo "  运行数据目录: $DATA_HOME/.qq-farm-bot"
fi

# ---- 0.5 同步仓库到最新 master ----
if [ -d "$SRC/.git" ]; then
  echo "[0.5/6] 拉取仓库最新代码..."
  cd "$SRC"
  git fetch origin || true
  BRANCH="$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo master)"
  git reset --hard "origin/${BRANCH}" || true
  echo "  当前版本: $(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
fi

# ---- 1. 构建前端 ----
echo "[1/6] 构建前端..."
cd "$SRC/web"
if [ -f package-lock.json ]; then
  npm ci || npm install
else
  npm install
fi
npm run build
cd "$SRC"

# ---- 2. 编译后端 ----
echo "[2/6] 编译程序..."
VERSION=$(git rev-parse --short HEAD 2>/dev/null || echo dev)
printf 'package main\n\nvar buildVersion = "%s"\nvar buildTime = "%s"\n' \
  "$VERSION" "$(commit_time)" > version.go
echo "  版本号: $VERSION"
go build -ldflags="-s -w" -o go-farm-bot .
BIN="$SRC/go-farm-bot"

# ---- 3. 安装程序 + 素材 ----
echo "[3/6] 安装到 $DES ..."
if systemctl list-unit-files go-farm-bot.service >/dev/null 2>&1; then
  systemctl stop go-farm-bot 2>/dev/null || true
fi
mkdir -p "$DES"
cp -f "$BIN" "$DES/"
chmod +x "$DES/go-farm-bot"
cp -a "$SRC/game-config" "$DES/"
if [ -d "$SRC/yyb-resource" ]; then
  cp -a "$SRC/yyb-resource" "$DES/"
else
  mkdir -p "$DES/yyb-resource"
fi

# ---- 4. systemd ----
echo "[4/6] 注册 systemd 服务..."
cat >/etc/systemd/system/go-farm-bot.service <<SVC
[Unit]
Description=QQ Farm Bot Go
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=/opt/go-farm-bot
ExecStart=/opt/go-farm-bot/go-farm-bot
Environment=ADMIN_PORT=${ADMIN_PORT}
Environment=HOME=${DATA_HOME}
Environment=GOPROXY=https://goproxy.cn,direct
# 传输加密：默认生成自签名证书走 HTTPS（浏览器首次访问需手动信任）。
# 使用正式证书时改为：Environment=ADMIN_TLS_CERT=/path/cert.pem 与 ADMIN_TLS_KEY=/path/key.pem，并删除下行。
Environment=ADMIN_TLS_AUTO=1
Restart=on-failure
RestartSec=5
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
SVC
systemctl daemon-reload
systemctl enable go-farm-bot
systemctl restart go-farm-bot

# ---- 5. 防火墙放行 3009 ----
echo "[5/6] 放行端口 ${ADMIN_PORT}..."
if have_cmd firewall-cmd && systemctl is-active --quiet firewalld; then
  firewall-cmd --permanent --add-port="${ADMIN_PORT}/tcp" >/dev/null
  firewall-cmd --reload >/dev/null
  echo "  firewalld 已放行 ${ADMIN_PORT}/tcp"
else
  echo "  未检测到活动 firewalld，请确认云厂商安全组已放行 ${ADMIN_PORT}"
fi

# ---- 6. 健康检查 ----
echo "[6/6] 检查服务..."
sleep 2
systemctl --no-pager --full status go-farm-bot | head -20 || true
if curl -fsSk "https://127.0.0.1:${ADMIN_PORT}/api/health" >/dev/null 2>&1; then
  echo "  health 检查通过（HTTPS）"
elif curl -fsS "http://127.0.0.1:${ADMIN_PORT}/api/health" >/dev/null 2>&1; then
  echo "  health 检查通过（HTTP，未启用 TLS）"
else
  echo "  health 暂未就绪，稍后执行: curl -sk https://127.0.0.1:${ADMIN_PORT}/api/health"
fi

IP=$(hostname -I 2>/dev/null | awk '{print $1}')
echo ""
echo "部署完成"
echo "访问地址: https://${IP:-103.117.136.113}:${ADMIN_PORT}（自签名证书，浏览器需手动信任）"
echo ""
echo "安全提示："
echo "  1) 立即登录后台修改默认口令 admin/admin"
echo "  2) accounts.json 已加密存储，密钥在 \${DATA_HOME}/.enckey（权限 0600），请妥善备份，丢失无法解密"
echo "  3) 生产建议改用正式证书（ADMIN_TLS_CERT/ADMIN_TLS_KEY）或前置 Nginx 反代 HTTPS"
echo "后续更新:  git pull && bash install.sh"
echo "日志查看:  journalctl -u go-farm-bot -f"
