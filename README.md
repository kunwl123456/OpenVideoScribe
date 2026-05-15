# Scribe Web

> 把视频链接（YouTube / B 站 等）粘贴进网页，服务端用 yt-dlp + ffmpeg + whisper.cpp 转成文字稿。

Go HTTP 服务 + 嵌入式 React 前端，单二进制部署，目标 Linux 服务器。前身是桌面项目 [scribe-studio](../scribe-studio)，本项目去掉 Wails / 桌面 / 微信注入等本地能力，专注服务端转写。

## 功能

- 粘贴 URL 自动下载、抽音、Whisper 转写（默认输出简体中文）
- SSE 实时进度（下载 / 抽音 / 转写）
- 4 个 AI 视图：AI 总结 / 详细摘要 / 大纲 / 思维导图（页面内渲染脑图）
- 视频元信息：播放 / 点赞 / 评论 / 收藏 / 封面 / 时长
- 历史记录卡片，可删除（同时清掉媒体文件）
- 导出 SRT / Markdown / TXT
- Whisper 模型网页一键下载，自带 hf-mirror.com fallback
- 单可执行文件，前端通过 `embed.FS` 内嵌

不包含：用户系统 / 鉴权 / 多租户（后续版本再加）。

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

## 部署

**一键 Docker（推荐）**：

```bash
git clone https://github.com/kunwl123456/scribe-web.git && cd scribe-web
cp scribe-llm.example.json scribe-llm.json   # 想用 AI 摘要就填 api_key + model；不填也能跑转写
docker compose -f deploy/docker-compose.yml up -d --build
# 打开 http://<host>:8787
```

镜像内自带 ffmpeg / yt-dlp / whisper-cli，模型存在卷 `/data/models`，重启不丢。

**本地裸跑**（需 Go 1.23+ / Node 20+ / pnpm，PATH 里有 ffmpeg + yt-dlp + whisper-cli；Linux 可跑 `scripts/fetch-bins.sh`）：

```bash
cd web && pnpm install && pnpm run build && cd ..   # 把前端 build 进 web_dist
go build -o bin/scribe-web ./cmd/server
./bin/scribe-web                                     # 默认 :8787
```

只调前端时另开一个 `cd web && pnpm run dev`（:5174，自动代理 /api）。

## LLM 配置

把 `scribe-llm.example.json` 复制成 `scribe-llm.json`（项目根或 `<data_dir>/`），填 `api_key` + `model`。默认指向火山方舟（Doubao），改 `base_url` 即可接 DeepSeek / OpenAI / 自托管 vLLM 等任意 OpenAI-compatible endpoint。也可用环境变量 `SCRIBE_LLM_API_KEY` / `SCRIBE_LLM_MODEL` 覆盖。

不配 = 4 个 AI 视图禁用，但视频转写本身仍然可用。`scribe-llm.json` 已加进 `.gitignore`，不会被提交。

## 环境变量

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `SCRIBE_ADDR` | `:8787` | HTTP 监听地址 |
| `SCRIBE_DATA_DIR` | OS 默认（Docker 内是 `/data`） | 模型 / 下载 / 任务的根目录 |
| `SCRIBE_FFMPEG_BIN` / `SCRIBE_YTDLP_BIN` / `SCRIBE_WHISPER_BIN` | PATH 自动找 | 强制指定二进制路径 |
| `WHISPER_MODEL_BASE_URL` | huggingface.co + hf-mirror.com（按序回退） | 逗号分隔多个镜像 |

## API

- `GET  /api/health` 健康检查（含 LLM 配置脱敏）
- `GET  /api/models` / `POST /api/models/{key}/download`
- `GET  /api/jobs` / `POST /api/jobs` body: `{"url":"...","model":"base","language":"auto"}`
- `GET  /api/jobs/{id}` / `DELETE /api/jobs/{id}`
- `GET  /api/jobs/{id}/events` SSE（任务 + summary 状态）
- `GET  /api/jobs/{id}/export?format=srt|md|txt`
- `GET  /api/jobs/{id}/thumbnail` 视频封面
- `POST /api/jobs/{id}/summarize?kind=brief|detailed|outline|mindmap` 异步 LLM 摘要

## 路线图

- v0.2：sqlite 持久化、并发 worker、模型断点续传、真实进度百分比
- v0.3：评论 AI 分析、词表、用户系统 + 鉴权
