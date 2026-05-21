# OpenVideoScribe

> 粘贴视频链接，一键得到可搜索文字稿、章节摘要、问答与导出内容。

这是一个面向网页使用者的视频学习整理工具：输入链接即可完成转写、提炼重点、生成二创内容，并沉淀到你的知识库。

![首页：粘贴 B 站 / YouTube 链接，一键转成可搜索的文字稿](docs/screenshot-home.png)

## 功能亮点

- 直接粘贴 YouTube / B 站链接，优先用平台字幕；无字幕时自动下载音频并转写。
- 单视频多轮问答（RAG）：支持连续追问，回答附 citations 证据片段。
- 章节化阅读：自动生成章节、要点、关键句，支持时间跳转回原文片段。
- 固定二创产物：学习笔记、公众号文案、课程讲义、短视频脚本、金句卡片。
- 导出完整：`srt|md|txt|md_bundle|obsidian|notion_import|xmind_outline|xmind_json`。
- 跨视频检索问答：可基于历史视频统一检索并回答。
- 任务更稳：支持并发、失败重试、异常恢复、进度状态更清晰。

## 网页如何使用

1. 打开首页，粘贴视频链接并选择 Whisper 模型。
2. 可选勾选“画面理解 VLM”（用于关键帧理解/OCR）。
3. 提交后在任务列表查看实时进度，完成后进入详情页。
4. 在详情页可查看 transcript、章节、摘要、问答，并继续多轮追问。
5. 按需生成二创内容，或导出 Markdown bundle / Obsidian / Notion / XMind 格式。
6. 如任务中断，系统会做重试与恢复，历史任务可继续使用。

## 部署

**Docker（推荐）**

```bash
git clone https://github.com/kunwl123456/OpenVideoScribe.git && cd OpenVideoScribe
cp scribe-llm.example.json scribe-llm.json   # 需要 AI 功能时填写
docker compose -f deploy/docker-compose.yml up -d --build
# 浏览器打开 http://<host>:8787
```

镜像内已包含 ffmpeg / yt-dlp / whisper-cli，模型默认保存在 `/data/models`。

**本地运行**

```bash
cd web && pnpm install && pnpm run build && cd ..
go build -o bin/scribe-web ./cmd/server
./bin/scribe-web
```

## 配置

本地私有配置文件请只保存在本机，不要提交到仓库（已在 `.gitignore`）：

- `scribe-llm.json`：文本 LLM（摘要、问答、章节、二创产物）。
- `scribe-vlm.json`：视觉 VLM（关键帧描述/OCR 融合）。
- `scribe-notion.json`：Notion 真导出（连接到 page/database）。

常用环境变量（按需配置）：

- `SCRIBE_ADDR`：服务监听地址（默认 `:8787`）。
- `SCRIBE_WORKER_CONCURRENCY`：任务并发数。
- `SCRIBE_JOB_RETRY_COUNT` / `SCRIBE_SUMMARY_RETRY_COUNT`：失败重试次数。
- `SCRIBE_RETRY_BACKOFF_MS`：重试退避时间。

## 常见问题

- AI 按钮灰掉：通常是 `scribe-llm.json` 未配置或 `api_key` 不可用。
- 模型下载慢/失败：先确认网络；可用 `scripts/fetch-whisper-model.sh` 预拉模型。
- Notion 导出失败：检查 token 是否有效、目标页面/数据库是否已共享给 Integration。
- 任务卡住后恢复：新版本已增强任务恢复与进度状态；刷新后可在历史任务继续查看。
- 我不需要 API 文档：本 README 以网页使用为主，接口细节已弱化，不影响日常使用。
