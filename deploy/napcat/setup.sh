#!/usr/bin/env bash
# NapCat + napcat-plugin-qq-farm-code 一键部署（供 qq-farm-bot-go-wj 的 QQ 扫码登录使用）
#
# 前置：能访问网络（首次会自动安装 Docker，使用阿里云镜像；仅支持 x86_64 架构）。
# 用法：bash setup.sh [WEBUI_TOKEN]
#   - 不传 token 则自动生成随机 token，最后打印出来。
#   - 部署完成后，在面板「扫码登录 → QQ」里点「生成二维码」即可（地址默认 http://127.0.0.1:6099，
#     WebUI Token 填脚本打印的值，勾选"保存为默认"下次免填）。
#   - 也可给 BOT 进程加环境变量 NAPCAT_WEBUI_TOKEN=<token>，实现完全零配置。
set -euo pipefail
cd "$(dirname "$0")"

CONTAINER=napcat-farm
PLUGIN=napcat-plugin-qq-farm-code
ZIP_URL="https://github.com/fubiwangluo/qq-code/releases/latest/download/${PLUGIN}.zip"

TOKEN="${1:-}"
if [ -z "$TOKEN" ]; then
  # 幂等：复用已运行容器的 token，避免重跑脚本使已登录的 QQ 掉线
  TOKEN="$(docker exec "$CONTAINER" printenv WEBUI_TOKEN 2>/dev/null || true)"
fi
if [ -z "$TOKEN" ]; then
  TOKEN="$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')"
fi

# ---------- 架构检查（NapCat/QQ NT 仅支持 x86_64） ----------
ARCH="$(uname -m)"
if [ "$ARCH" != "x86_64" ]; then
  echo "!! 当前架构为 $ARCH，NapCat / QQ NT 仅支持 x86_64，无法在此机器部署。"
  exit 1
fi

# ---------- 自动安装 Docker（若未安装） ----------
ensure_docker() {
  if command -v docker >/dev/null 2>&1; then
    echo "==> [0/6] 检测到 Docker 已安装：$(docker --version)"
    return 0
  fi
  echo "==> [0/6] 未检测到 Docker，开始自动安装（使用阿里云镜像）..."
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL https://get.docker.com | bash -s docker --mirror Aliyun
  elif command -v wget >/dev/null 2>&1; then
    wget -qO- https://get.docker.com | bash -s docker --mirror Aliyun
  else
    echo "!! 没有 curl 也没有 wget，无法自动安装 Docker。请手动安装后重跑本脚本。"
    exit 1
  fi
  # 启动并设为开机自启
  if command -v systemctl >/dev/null 2>&1; then
    systemctl enable --now docker 2>/dev/null || systemctl start docker
  else
    service docker start 2>/dev/null || true
  fi
  # 等待 daemon 就绪（最多 60s）
  local i
  for i in $(seq 1 30); do
    if docker info >/dev/null 2>&1; then break; fi
    sleep 2
  done
  if ! docker info >/dev/null 2>&1; then
    echo "!! Docker 已安装但 daemon 未启动，请检查：journalctl -u docker"
    exit 1
  fi
  echo "    Docker 安装并启动成功：$(docker --version)"
}

ensure_docker

# docker compose 兼容性（优先 v2 插件，回退 v1 二进制）
if docker compose version >/dev/null 2>&1; then
  COMPOSE="docker compose"
elif command -v docker-compose >/dev/null 2>&1; then
  COMPOSE="docker-compose"
else
  echo "!! 没有 docker compose 插件，请安装 docker-compose-plugin 后重试。"
  exit 1
fi

echo "==> [1/6] 启动 NapCat 容器（token 已配置，登录态/配置/插件均持久化在当前目录）"
mkdir -p config QQ plugins
export WEBUI_TOKEN="$TOKEN" NAPCAT_UID="$(id -u)" NAPCAT_GID="$(id -g)"
$COMPOSE up -d

echo "==> [2/6] 等待 NapCat 启动..."
sleep 6
docker ps --filter "name=${CONTAINER}" --format '{{.Names}}  {{.Status}}'

echo "==> [3/6] 安装插件 ${PLUGIN}"
if curl -fsSL "$ZIP_URL" -o /tmp/${PLUGIN}.zip 2>/dev/null; then
  rm -rf "plugins/${PLUGIN}"
  if command -v unzip >/dev/null 2>&1; then
    unzip -oq /tmp/${PLUGIN}.zip -d plugins/
  else
    python3 -c "import zipfile;zipfile.ZipFile('/tmp/${PLUGIN}.zip').extractall('plugins/')"
  fi
  # 兼容 zip 根目录直接是文件的情况（规范 zip 根目录是 ${PLUGIN}/）
  if [ ! -d "plugins/${PLUGIN}" ]; then
    mkdir -p "plugins/${PLUGIN}" && mv plugins/* "plugins/${PLUGIN}/" 2>/dev/null || true
  fi
  echo "    已解压到 plugins/${PLUGIN}"
else
  echo "    !! 下载失败（GitHub 不通？）。请手动下载 ${ZIP_URL}"
  echo "       解压到 $(pwd)/plugins/${PLUGIN}/ 后重新执行本脚本。"
fi

# 在 config/plugins.json 中启用插件
PJ="config/plugins.json"
if [ -f "$PJ" ]; then
  grep -q "\"${PLUGIN}\"" "$PJ" || python3 - "$PJ" <<'PYEOF'
import json,sys
p=sys.argv[1]
d=json.load(open(p,encoding='utf-8'))
d["napcat-plugin-qq-farm-code"]=True
json.dump(d,open(p,'w',encoding='utf-8'),ensure_ascii=False,indent=2)
PYEOF
else
  echo "{\"${PLUGIN}\": true}" > "$PJ"
fi
echo "    plugins.json 已启用"

echo "==> [4/6] 第三方插件白名单补丁（若插件商店索引未合入则必需，已打过的自动跳过）"
docker exec "$CONTAINER" node -e '
const fs=require("fs");const p="/app/napcat/napcat.mjs";
try{
  let s=fs.readFileSync(p,"utf8");
  const t="return this.isOfficialPlugin(e) ? null : r ? `sensitive keyword \"${r}\"` : \"not in official plugin whitelist\";";
  if(s.includes(t)){
    fs.copyFileSync(p,p+".bak");
    fs.writeFileSync(p,s.replace(t,"return r ? `sensitive keyword \"${r}\"` : null"));
    console.log("    白名单已打补丁（备份 napcat.mjs.bak）");
  }else if(s.includes("not in official plugin whitelist")){
    console.log("    NapCat 版本不同，未匹配到完整片段；若插件加载报 not in official plugin whitelist，");
    console.log("    请在 /app/napcat/napcat.mjs 里搜索该字符串，按 qq-code README 去掉 isOfficialPlugin 判定。");
  }else{
    console.log("    无需补丁（官方索引已收录或版本已变化）");
  }
}catch(e){console.log("    补丁跳过: "+e.message)}
' || echo "    补丁步骤失败（不阻塞，稍后看插件能否加载）"

echo "==> [5/6] 重启生效"
$COMPOSE restart >/dev/null 2>&1 || docker restart "$CONTAINER" >/dev/null

echo "==> [6/6] 验证 WebUI 是否就绪（应在数秒内返回 200）"
for i in $(seq 1 20); do
  if curl -fsS -o /dev/null "http://127.0.0.1:6099/webui" 2>/dev/null; then
    echo "    WebUI 已就绪 ✅"
    break
  fi
  sleep 3
done

echo "==> [7/7] 将 WebUI Token 注入 BOT 服务环境变量（实现面板零配置扫码，可选）"
DROPIN_DIR="/etc/systemd/system/go-farm-bot.service.d"
if systemctl list-unit-files 2>/dev/null | grep -q "^go-farm-bot.service"; then
  mkdir -p "$DROPIN_DIR"
  cat > "$DROPIN_DIR/napcat.conf" <<EOF2
[Service]
Environment=NAPCAT_BASE=http://127.0.0.1:6099
Environment=NAPCAT_WEBUI_TOKEN=${TOKEN}
EOF2
  systemctl daemon-reload
  if systemctl restart go-farm-bot >/dev/null 2>&1; then
    echo "    go-farm-bot 已重启并载入 Token：面板「扫码登录 → QQ」进页签直接出二维码，无需手动填。"
  else
    echo "    go-farm-bot 重启失败（不影响 NapCat）。请手动重启服务或给进程加 NAPCAT_WEBUI_TOKEN=${TOKEN} 后重启。"
  fi
else
  echo "    未检测到 go-farm-bot systemd 服务，跳过自动注入。"
  echo "    若想零配置，请给 BOT 进程加环境变量 NAPCAT_BASE=http://127.0.0.1:6099 与"
  echo "    NAPCAT_WEBUI_TOKEN=${TOKEN} 后重启；否则在面板手动填上方 Token 即可。"
fi

cat <<EOF

==============================================================
 部署完成！
 架构：     x86_64（已校验）
 WebUI:    http://127.0.0.1:6099/webui  （仅本机可访问）
 Token:    ${TOKEN}
 下一步：
    1. 面板「扫码登录 → QQ」→ 填 NapCat 地址 http://127.0.0.1:6099
       与上面的 WebUI Token → 点「生成二维码」→ 手机 QQ 扫码。
       勾选「保存为默认」后下次免填。
    2. 或给 BOT 服务加环境变量 NAPCAT_WEBUI_TOKEN=${TOKEN}
       （可再配 NAPCAT_BASE=http://127.0.0.1:6099），面板完全零配置。
  注意：QQ 登录态在 ./QQ 目录，插件重启后自动加载；二维码 3 分钟有效，
        失效可在面板点刷新。NapCat 容器已设为 unless-stopped，开机自启。
==============================================================
EOF
