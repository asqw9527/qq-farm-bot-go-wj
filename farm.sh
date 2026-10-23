#!/bin/bash
# 一键启停脚本（服务器端）：systemd 优先，未安装 systemd 服务时退回裸进程 + pidfile 管理
# 用法：./farm.sh start|stop|restart|status
# 二进制路径：默认 /opt/go-farm-bot/go-farm-bot，可用环境变量 FARM_BIN 覆盖
# 服务名：默认 go-farm-bot，可用环境变量 FARM_SERVICE 覆盖

FARM_BIN="${FARM_BIN:-/opt/go-farm-bot/go-farm-bot}"
FARM_SERVICE="${FARM_SERVICE:-go-farm-bot}"
PID_FILE="/tmp/go-farm-bot.pid"
LOG_FILE="/tmp/go-farm-bot.log"

have_systemd() {
  command -v systemctl >/dev/null 2>&1 && systemctl list-unit-files 2>/dev/null | grep -q "^${FARM_SERVICE}.service"
}

do_start() {
  if have_systemd; then
    echo "[farm] 通过 systemd 启动 ${FARM_SERVICE} ..."
    systemctl start "${FARM_SERVICE}" && systemctl status "${FARM_SERVICE}" --no-pager | head -5
    return
  fi
  if [ -f "$PID_FILE" ] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
    echo "[farm] 已在运行 (pid $(cat "$PID_FILE"))"
    return
  fi
  if [ ! -x "$FARM_BIN" ]; then
    echo "[farm] 错误：找不到可执行文件 $FARM_BIN（可用 FARM_BIN=路径 覆盖）" >&2
    exit 1
  fi
  echo "[farm] 裸进程启动 $FARM_BIN ..."
  nohup "$FARM_BIN" >>"$LOG_FILE" 2>&1 &
  echo $! > "$PID_FILE"
  sleep 2
  if kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
    echo "[farm] 启动成功 (pid $(cat "$PID_FILE"))，日志：$LOG_FILE"
  else
    echo "[farm] 启动失败，最近日志：" >&2
    tail -20 "$LOG_FILE" >&2
    exit 1
  fi
}

do_stop() {
  if have_systemd; then
    echo "[farm] 通过 systemd 停止 ${FARM_SERVICE} ..."
    systemctl stop "${FARM_SERVICE}"
    return
  fi
  if [ -f "$PID_FILE" ]; then
    pid="$(cat "$PID_FILE")"
    if kill -0 "$pid" 2>/dev/null; then
      echo "[farm] 停止进程 $pid ..."
      kill "$pid"
      for _ in $(seq 1 10); do
        kill -0 "$pid" 2>/dev/null || break
        sleep 1
      done
      kill -0 "$pid" 2>/dev/null && kill -9 "$pid"
      echo "[farm] 已停止"
    else
      echo "[farm] 进程不存在，清理 pid 文件"
    fi
    rm -f "$PID_FILE"
    return
  fi
  # 兜底按进程名停（仅精确匹配二进制名）
  pkill -f "$FARM_BIN" 2>/dev/null && echo "[farm] 已停止（按路径匹配）" || echo "[farm] 未发现运行中的进程"
}

do_status() {
  if have_systemd; then
    systemctl status "${FARM_SERVICE}" --no-pager | head -8
    return
  fi
  if [ -f "$PID_FILE" ] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
    echo "[farm] 运行中 (pid $(cat "$PID_FILE"))"
  else
    echo "[farm] 未运行"
    exit 3
  fi
}

case "${1:-}" in
  start)   do_start ;;
  stop)    do_stop ;;
  restart) do_stop; sleep 1; do_start ;;
  status)  do_status ;;
  *)
    echo "用法：$0 start|stop|restart|status"
    echo "环境变量：FARM_BIN（二进制路径，默认 $FARM_BIN）、FARM_SERVICE（systemd 服务名，默认 $FARM_SERVICE）"
    exit 2
    ;;
esac
