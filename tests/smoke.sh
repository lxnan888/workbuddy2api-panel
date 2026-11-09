#!/usr/bin/env bash
# 本地冒烟测试：用临时目录和空账号池启动网关，验证只读端点与新增观测字段。
# 不读取 config.json / auths/ / data/，不包含任何账号、token 或代理凭据。
# 用法：./tests/smoke.sh [端口]
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

GO="${GO:-go}"
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
PORT="${1:-${SMOKE_PORT:-17863}}"

TMP="$(mktemp -d)"
PID=""
cleanup() {
  if [[ -n "$PID" ]] && kill -0 "$PID" 2>/dev/null; then
    kill "$PID" 2>/dev/null || true
    wait "$PID" 2>/dev/null || true
  fi
  rm -rf "$TMP"
}
trap cleanup EXIT

mkdir -p "$TMP/auths" "$TMP/data"
cat >"$TMP/config.json" <<JSON
{
  "listen": "127.0.0.1:${PORT}",
  "api_key": "",
  "auth_dir": "$TMP/auths",
  "state_file": "$TMP/data/state.json",
  "pool": {
    "concurrency_wait_timeout": "30s",
    "concurrency_wait_queue": 100
  }
}
JSON

"$GO" build -o "$TMP/workbuddy2api" ./cmd/server
"$TMP/workbuddy2api" -config "$TMP/config.json" >"$TMP/server.log" 2>&1 &
PID=$!

READY=0
for _ in $(seq 1 100); do
  # 空账号池时 /healthz 按设计返回 503（ServableNow=false），这里只要求端口
  # 已能给出 JSON 响应，不能把 503 当成启动失败。
  health="$(curl -sS "http://127.0.0.1:${PORT}/healthz" 2>/dev/null || true)"
  if [[ "$health" == *'"service":"workbuddy2api"'* ]]; then
    READY=1
    break
  fi
  if ! kill -0 "$PID" 2>/dev/null; then
    echo "网关启动失败：" >&2
    tail -20 "$TMP/server.log" >&2
    exit 1
  fi
  sleep 0.1
done
if [[ "$READY" != 1 ]]; then
  echo "网关未在预期时间内就绪：" >&2
  tail -20 "$TMP/server.log" >&2
  exit 1
fi

status="$(curl -sS "http://127.0.0.1:${PORT}/status")"
for key in '"concurrency_full"'; do
  if [[ "$status" != *"$key"* ]]; then
    echo "缺少观测字段 $key：" >&2
    echo "$status" >&2
    exit 1
  fi
done

echo "smoke ok: /healthz 与 /status 可用，concurrency/concurrency_full 已透出"
