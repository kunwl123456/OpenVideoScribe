#!/usr/bin/env bash
# Linux helper: install ffmpeg + yt-dlp + whisper-cli for local dev.
# For container builds use deploy/Dockerfile; this script is for bare
# Linux servers and developer laptops.
set -euo pipefail

if [[ "$(uname -s)" != "Linux" ]]; then
  echo "[fetch-bins] this script targets Linux. On Windows use winget; on macOS use brew." >&2
  exit 0
fi

if ! command -v ffmpeg >/dev/null 2>&1; then
  if command -v apt-get >/dev/null 2>&1; then
    sudo apt-get update && sudo apt-get install -y ffmpeg
  else
    echo "[fetch-bins] please install ffmpeg manually" >&2
  fi
fi

if ! command -v yt-dlp >/dev/null 2>&1; then
  if command -v pip3 >/dev/null 2>&1; then
    pip3 install --user --upgrade yt-dlp
  else
    echo "[fetch-bins] please install yt-dlp manually (pip3 install -U yt-dlp)" >&2
  fi
fi

if ! command -v whisper-cli >/dev/null 2>&1; then
  cache="${SCRIBE_BUILD_CACHE:-$HOME/.cache/scribe-build}"
  src="$cache/whisper.cpp"
  mkdir -p "$cache"
  if [[ ! -d "$src/.git" ]]; then
    git clone --depth 1 https://github.com/ggerganov/whisper.cpp.git "$src"
  else
    (cd "$src" && git fetch --depth 1 origin master && git reset --hard origin/master)
  fi
  cmake -S "$src" -B "$src/build" \
    -DCMAKE_BUILD_TYPE=Release \
    -DBUILD_SHARED_LIBS=OFF >/dev/null
  cmake --build "$src/build" --target whisper-cli --config Release -j
  echo "[fetch-bins] whisper-cli built at $src/build/bin/whisper-cli"
  echo "             copy or symlink it onto your PATH, e.g.:"
  echo "             sudo install -m 0755 \"$src/build/bin/whisper-cli\" /usr/local/bin/whisper-cli"
fi

echo "[fetch-bins] done."
