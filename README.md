# Scribe Web

> 把视频链接（YouTube / B 站 等）粘贴进网页，服务端用 yt-dlp + ffmpeg + whisper.cpp 转成文字稿。

Go HTTP 服务 + 嵌入式 React 前端，单二进制部署，目标 Linux 服务器。前身是桌面项目 [scribe-studio](../scribe-studio)，本项目去掉 Wails / 桌面 / 微信注入等本地能力，专注服务端转写。

## 功能（v0.1 MVP）

- 粘贴视频 URL 触发任务（yt-dlp 兼容来源）
- 任务实时进度（SSE 推送，三阶段：下载 / 抽音 / 转写）
- 网页查看正文 + 时间戳分段
- 导出 SRT / Markdown / TXT
- Whisper 模型在网页一键下载，存到数据目录
- 单可执行文件，前端通过 `embed.FS` 内嵌

不包含：用户系统、多租户、LLM 校对、词表（后续版本再加）。

## 架构

```
┌─────────────┐  POST /api/jobs  ┌──────────────┐
│  React UI   │ ───────────────▶│  HTTP Server │
└─────────────┘   SSE events     └──────┬───────┘
       ▲                                 │
       │                                 ▼
       │                          ┌──────────────┐
       │                          │  Job Worker  │
       │                          └──────┬───────┘
       │                                 │
       │              ┌──────────────────┼──────────────────┐
       │              ▼                  ▼                  ▼
       │        yt-dlp 下载         ffmpeg 抽音        whisper-cli 转写
       │              │                  │                  │
       └──────────────┴──────────────────┴──────────────────┘
                            files persisted under SCRIBE_DATA_DIR/
```

## 目录结构

```
scribe-web/
├── cmd/server/                # Go 入口 + embed:web_dist
├── internal/
│   ├── config/                # 路径 + 二进制定位 + 环境变量
│   ├── models/                # Whisper 模型清单 / 下载
│   ├── media/                 # ffmpeg 抽音
│   ├── asr/                   # whisper-cli 调用
│   ├── ytdlp/                 # yt-dlp 调用
│   ├── store/                 # JSON 文件持久化任务
│   ├── jobs/                  # 任务队列 + pipeline + 事件 fan-out
│   └── httpapi/               # REST + SSE 路由
├── web/                       # React + Vite 前端
├── deploy/                    # Dockerfile + docker-compose
├── scripts/                   # Linux 安装辅助脚本
└── README.md
```

## 一键部署（推荐）

```bash
docker compose -f deploy/docker-compose.yml up -d --build
# 浏览器打开 http://<host>:8787
```

镜像内自带 `ffmpeg`、`yt-dlp`、`whisper-cli`。模型在容器内通过网页下载到 `/data/models`，重启不丢。

## 本地开发

依赖：

- Go 1.23+
- Node 20+ 和 pnpm
- ffmpeg、yt-dlp、whisper-cli（Linux 可跑 `scripts/fetch-bins.sh`）

```bash
# 1. 装前端依赖
cd web && pnpm install && cd ..

# 2. 启 Go 后端（默认 :8787）
go run ./cmd/server

# 3. 启前端 dev server（:5174，自动代理 /api 到 :8787）
cd web && pnpm run dev
```

打包成单二进制：

```bash
cd web && pnpm run build && cd ..
go build -o bin/scribe-web ./cmd/server
./bin/scribe-web
```

## 环境变量

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `SCRIBE_ADDR` | `:8787` | HTTP 监听地址 |
| `SCRIBE_DATA_DIR` | OS 默认（容器内是 `/data`） | 模型 / 下载 / 任务 / 工作目录的根 |
| `SCRIBE_FFMPEG_BIN` | 自动从 PATH 找 | 显式 ffmpeg 路径 |
| `SCRIBE_YTDLP_BIN` | 自动从 PATH 找 | 显式 yt-dlp 路径 |
| `SCRIBE_WHISPER_BIN` | 自动从 PATH 找 | 显式 whisper-cli 路径 |
| `WHISPER_MODEL_BASE_URL` | `https://huggingface.co/ggerganov/whisper.cpp/resolve/main` | 模型下载源；国内可换 `https://hf-mirror.com/...` |

## API（v1）

- `GET  /api/health`
- `GET  /api/models`
- `POST /api/models/{key}/download`
- `GET  /api/jobs`
- `POST /api/jobs` body: `{"url":"...","model":"base","language":"auto"}`
- `GET  /api/jobs/{id}`
- `GET  /api/jobs/{id}/events` SSE
- `GET  /api/jobs/{id}/export?format=srt|md|txt`

## 路线图

- v0.1：当前 MVP
- v0.2：sqlite 持久化、并发 worker、模型断点续传
- v0.3：LLM 校对（接入 Claude / Gemini）+ 词表
- v0.4：多用户 + 鉴权 + 任务私有化
