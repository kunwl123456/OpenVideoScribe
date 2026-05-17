#!/usr/bin/env bash
# fetch-whisper-model.sh — 在 host 上下载 Whisper ggml 模型，docker cp 进容器
#
# 当容器内网络不通（如 WSL2 + docker bridge 对 hf-mirror 的某些 redirect
# 链失败）时，用 host 网络下载更稳。host 上 curl 跟随 redirect 一般 OK。
#
# 用法：
#   ./scripts/fetch-whisper-model.sh tiny
#   ./scripts/fetch-whisper-model.sh base
#   ./scripts/fetch-whisper-model.sh small
#   ./scripts/fetch-whisper-model.sh medium
#   ./scripts/fetch-whisper-model.sh all     # 全部下载
#
# 需要：host 有 curl，并且 docker 命令可用（脚本内部已 sudo）。

set -euo pipefail

CONTAINER="${SCRIBE_CONTAINER:-scribe-web}"
MIRROR="${WHISPER_MIRROR:-https://hf-mirror.com/ggerganov/whisper.cpp/resolve/main}"
MODELS_DIR_IN_CONTAINER="/data/models"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

if [[ $# -lt 1 ]]; then
  echo "用法: $0 <tiny|base|small|medium|all>" >&2
  exit 2
fi

declare -A FILES=(
  [tiny]="ggml-tiny.bin"
  [base]="ggml-base.bin"
  [small]="ggml-small.bin"
  [medium]="ggml-medium.bin"
)

download_one() {
  local key="$1"
  local file="${FILES[$key]:-}"
  if [[ -z "$file" ]]; then
    echo "未知模型: $key (可选: tiny/base/small/medium)" >&2
    exit 2
  fi

  echo ">>> 下载 $key ($file)..."
  local url="$MIRROR/$file"
  curl -L --fail --max-time 600 \
    -w "HTTP:%{http_code} SIZE:%{size_download} TIME:%{time_total}s\n" \
    -o "$TMP/$file" "$url"

  echo ">>> 注入容器 $CONTAINER:$MODELS_DIR_IN_CONTAINER/$file ..."
  # 先用 sudo -v 预提权，避免子命令重新弹密码
  sudo -v
  sudo docker exec "$CONTAINER" mkdir -p "$MODELS_DIR_IN_CONTAINER"
  sudo docker cp "$TMP/$file" "$CONTAINER:$MODELS_DIR_IN_CONTAINER/$file"
  echo ">>> $key 安装完成"
  echo ""
}

if [[ "$1" == "all" ]]; then
  for k in tiny base small medium; do
    download_one "$k"
  done
else
  download_one "$1"
fi

echo "全部完成。验证："
echo "  curl -s http://localhost:8787/api/models | python3 -m json.tool"
