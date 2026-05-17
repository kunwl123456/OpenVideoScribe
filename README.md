# OpenVideoScribe

> 把视频链接（YouTube / B 站 等）粘贴进网页，服务端用 yt-dlp + ffmpeg + whisper.cpp 转成文字稿，再调 LLM 出摘要 / 大纲 / 思维导图。

Go HTTP 服务 + 嵌入式 React 前端，单二进制部署，目标 Linux 服务器。前身是桌面项目 [scribe-studio](../scribe-studio)，本项目去掉 Wails / 桌面 / 微信注入等本地能力，专注服务端转写。原代号 `scribe-web`，仓库地址、Go module 名、Docker 镜像名仍沿用旧名（迁移成本大，等下个 major 版本一起改）。

![首页：粘贴 B 站 / YouTube 链接，一键转成可搜索的文字稿](docs/screenshot-home.png)

## 功能

- 粘贴 URL 自动下载、抽音、Whisper 转写（默认输出简体中文）
- SSE 实时进度（下载 / 抽音 / 转写 / 画面理解）
- 4 个 AI 视图：AI 总结 / 详细摘要 / 大纲 / 思维导图（页面内渲染脑图）
- **画面理解（VLM，可选）**：抽关键帧 → 视觉模型描述画面 + OCR → 自动并入 AI 摘要；BIBIGPT 同款思路，默认指向 Doubao Seed Vision，详见 [视觉理解（可选）](#视觉理解可选)
- 服务重启不丢任务：进行中的任务会在启动时被标记为「中断」而非永远卡死
- 视频元信息：播放 / 点赞 / 评论 / 收藏 / 封面 / 时长
- 历史记录卡片，可删除（同时清掉媒体文件）
- 导出 SRT / Markdown / TXT
- Whisper 模型网页一键下载，自带 hf-mirror.com fallback
- 单可执行文件，前端通过 `embed.FS` 内嵌

不包含：用户系统 / 鉴权 / 多租户（后续版本再加）。

## 架构

```
┌─────────────┐  POST /api/jobs   ┌──────────────┐
│  React UI   │ ────────────────▶│  HTTP Server │
└─────────────┘    SSE events     └──────┬───────┘
       ▲                                  │
       │                                  ▼
       │                           ┌──────────────┐
       │                           │  Job Worker  │
       │                           └──────┬───────┘
       │                                  │
       │   ┌──────────────────┬───────────┼──────────────────┬──────────────┐
       │   ▼                  ▼           ▼                  ▼              ▼
       │ yt-dlp 下载   ffmpeg 抽音  whisper-cli 转写  ffmpeg 抽关键帧   LLM 摘要
       │  *自动选 audio/video                                │           （融合画面）
       │   按 VLM 开关切换*                                  ▼
       │                                              VLM 看图（OCR + 描述）
       │                                                   │
       └───────────────────────────────────────────────────┘
                       files persisted under SCRIBE_DATA_DIR/
```

> 抽关键帧 + VLM 阶段仅在 `scribe-vlm.json` 已配置时启用，未启用时管线直接从转写跳到 LLM 摘要，整体表现等价于纯 ASR 版本。任何一帧失败都不会让任务挂掉，详见下文 [视觉理解（可选）](#视觉理解可选)。

## 目录结构

```
scribe-web/
├── cmd/server/                # Go 入口 + embed:web_dist
├── internal/
│   ├── config/                # 路径 + 二进制定位 + 环境变量（llm.go / vlm.go 分别加载两份 JSON）
│   ├── models/                # Whisper 模型清单 / 下载
│   ├── media/                 # ffmpeg 抽音 (extract.go) + 关键帧抽取 (frames.go)
│   ├── asr/                   # whisper-cli 调用
│   ├── ytdlp/                 # yt-dlp 调用
│   ├── llm/                   # OpenAI Chat Completions 客户端
│   ├── vlm/                   # OpenAI Vision Chat Completions 客户端（图文 wire types）
│   ├── vision/                # 抽帧 → VLM 看图 → Insight，带并发控制
│   ├── summary/               # 4 个 prompt 模板 + LLM 调用，融合 VisualInsights
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
git clone https://github.com/kunwl123456/OpenVideoScribe.git && cd OpenVideoScribe
cp scribe-llm.example.json scribe-llm.json   # 想用 AI 摘要就填 api_key + model；不填也能跑转写
docker compose -f deploy/docker-compose.yml up -d --build
# 打开 http://<host>:8787
```

镜像内自带 ffmpeg / yt-dlp / whisper-cli，模型存在卷 `/data/models`，重启不丢。

**模型下不下来？**（常见于 WSL2 + docker bridge 网络，对 hf-mirror 的部分 redirect 链不通）

```bash
./scripts/fetch-whisper-model.sh tiny     # 或 base / small / medium / all
```
脚本在 host 网络下用 curl 拉 hf-mirror，再 `docker cp` 进容器 `/data/models/`。host 网络通常能跨过 docker bridge 卡住的中转跳转。

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

## 视觉理解（可选）

参考 BIBIGPT 的做法，在转写之后追加一段 **关键帧抽取 → VLM 看图 → 把画面描述并入 LLM 摘要** 的流水线。开启后，4 个 AI 视图自动获得画面信息（PPT 文字、画面氛围、关键截图描述等），不需要新增 kind。

启用方式：把 `scribe-vlm.example.json` 复制成 `scribe-vlm.json`（项目根或 `<data_dir>/`），填 `api_key` + `model`。推荐模型 `doubao-seed-1-6-vision-250815`。也可用环境变量 `SCRIBE_VLM_API_KEY` / `SCRIBE_VLM_MODEL` 覆盖。

关键参数（其余字段含义与 `scribe-llm.json` 一致）：

| 字段 | 默认 | 含义 |
| --- | --- | --- |
| `frame_interval_seconds` | 15 | 兜底间隔：相邻两帧最小时间间隔（秒），保证讲座类视频也有最低密度 |
| `scene_threshold` | 0.4 | ffmpeg 场景切换阈值，0.3–0.5 之间；0 关闭场景检测，纯按 interval 抽帧 |
| `max_frames` | 60 | 每任务上限（成本闸门）。超出时按时间均匀下采样 |
| `concurrency` | 4 | 同一任务并发 VLM 调用数，太高易被上游限流 |

关闭方式：删除 / 重命名 `scribe-vlm.json` 即可。任何一帧失败、抽帧失败、VLM 限流都不会让任务挂掉，只是该次摘要少了画面信息。

帧图片可以通过 `GET /api/jobs/{id}/frames/{index}` 拉取，前端目前不展示缩略图栏（保持极简 UI），但接口已就绪供后续扩展。

## 环境变量

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `SCRIBE_ADDR` | `:8787` | HTTP 监听地址 |
| `SCRIBE_DATA_DIR` | OS 默认（Docker 内是 `/data`） | 模型 / 下载 / 任务的根目录 |
| `SCRIBE_FFMPEG_BIN` / `SCRIBE_YTDLP_BIN` / `SCRIBE_WHISPER_BIN` | PATH 自动找 | 强制指定二进制路径 |
| `WHISPER_MODEL_BASE_URL` | hf-mirror.com + huggingface.co（按序回退） | 逗号分隔多个镜像。Docker 部署默认值已写在 `deploy/docker-compose.yml`。 |

## API

- `GET  /api/health` 健康检查（含 LLM 配置脱敏）
- `GET  /api/models` / `POST /api/models/{key}/download`
- `GET  /api/jobs` / `POST /api/jobs` body: `{"url":"...","model":"base","language":"auto"}`
- `GET  /api/jobs/{id}` / `DELETE /api/jobs/{id}`
- `GET  /api/jobs/{id}/events` SSE（任务 + summary 状态）
- `GET  /api/jobs/{id}/export?format=srt|md|txt`
- `GET  /api/jobs/{id}/thumbnail` 视频封面
- `GET  /api/jobs/{id}/frames/{index}` 视觉理解抽取的单帧 jpg（仅当 `scribe-vlm.json` 已配置）
- `POST /api/jobs/{id}/summarize?kind=brief|detailed|outline|mindmap` 异步 LLM 摘要（自动并入画面信息，若视觉理解已启用）

## 路线图

- v0.2：sqlite 持久化、并发 worker、模型断点续传、真实进度百分比
- v0.3：评论 AI 分析、词表、用户系统 + 鉴权
