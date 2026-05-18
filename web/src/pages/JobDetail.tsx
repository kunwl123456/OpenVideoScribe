import { useEffect, useMemo, useRef, useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import {
  ApiError,
  api,
  FrameInsight,
  Job,
  JobEvent,
  LogLine,
  Phase,
  Summary,
  SummaryKind,
  VisionStatus,
  streamJob,
} from '../api/client'
import Brand from '../components/Brand'
import PhaseBadge from '../components/PhaseBadge'
import SummaryPanel from '../components/SummaryPanel'

// The summary UI is now fully driven by the persisted
// `job.summaries[kind].status` field — `pending` / `done` / `failed`.
// No ephemeral React state survives across navigation; that was the
// root cause of the "返回首页再进来就显示未生成" regression. Inflight
// generates poll /api/jobs/{id} until every entry leaves pending.

const PHASE_LABEL: Record<Phase, string> = {
  queued: '排队中',
  downloading: '下载中',
  extracting: '抽取音频',
  transcribing: '转写中',
  analyzing: '画面理解',
  done: '已完成',
  failed: '失败',
}

const IN_FLIGHT_PHASES: Phase[] = ['queued', 'downloading', 'extracting', 'transcribing', 'analyzing']

type Tab =
  | 'brief'
  | 'detailed'
  | 'outline'
  | 'mindmap'
  | 'transcript'
  | 'segments'
  | 'frames'
  | 'logs'

const SUMMARY_TABS: { key: Exclude<Tab, 'transcript' | 'segments' | 'frames' | 'logs'>; label: string }[] = [
  { key: 'brief', label: 'AI 总结' },
  { key: 'detailed', label: '详细摘要' },
  { key: 'outline', label: '大纲' },
  { key: 'mindmap', label: '思维导图' },
]

function formatTime(sec: number) {
  if (!isFinite(sec)) return '00:00'
  const m = Math.floor(sec / 60)
  const s = Math.floor(sec % 60)
  return `${m.toString().padStart(2, '0')}:${s.toString().padStart(2, '0')}`
}

function formatDurationMs(ms: number | undefined): string {
  if (!ms || !isFinite(ms)) return 'N/A'
  return `${(ms / 1000).toFixed(1)}s`
}

function formatTokens(n: number | undefined): string {
  if (!n || !isFinite(n)) return 'N/A'
  return n.toLocaleString('zh-CN')
}

function formatYuan(v: number): string {
  if (!isFinite(v) || v <= 0) return '¥0'
  if (v < 0.01) return `¥${v.toFixed(4)}`
  if (v < 1) return `¥${v.toFixed(3)}`
  return `¥${v.toFixed(2)}`
}

function visionStatusOf(job: Job): VisionStatus | undefined {
  if (job.vision_status) return job.vision_status
  return (job.frames?.length ?? 0) > 0 ? 'done' : undefined
}

function frameCostText(frame: FrameInsight): string {
  return frame.estimated_cost_text || 'N/A'
}

function summarizeFrames(frames: FrameInsight[]) {
  const totalTokens = frames.reduce((sum, f) => sum + (f.tokens_used || 0), 0)
  const totalDurationMs = frames.reduce((sum, f) => sum + (f.duration_ms || 0), 0)
  const pricedFrames = frames.filter((f) => typeof f.estimated_cost === 'number' && f.estimated_cost > 0)
  const tokenFrames = frames.filter((f) => (f.tokens_used || 0) > 0)
  const hasCompleteCost = tokenFrames.length > 0 && tokenFrames.every((f) => f.estimated_cost_text)
  const totalCost = pricedFrames.reduce((sum, f) => sum + (f.estimated_cost || 0), 0)
  return {
    totalTokens,
    totalDurationMs,
    totalCostText: hasCompleteCost ? formatYuan(totalCost) : 'N/A',
    costIsNA: !hasCompleteCost,
  }
}

// formatCount renders an engagement counter using zh-CN conventions:
// • < 10k → straight thousands separator (1,234)
// • 10k..1亿 → "万" (12.3 万)
// • >= 1亿 → "亿" (1.23 亿)
function formatCount(n: number | undefined): string | null {
  if (n === undefined || n === null) return null
  if (!isFinite(n) || n <= 0) return null
  if (n < 10000) return n.toLocaleString('zh-CN')
  if (n < 1_0000_0000) return (n / 10000).toFixed(n < 100000 ? 1 : 1).replace(/\.0$/, '') + ' 万'
  return (n / 1_0000_0000).toFixed(2).replace(/\.?0+$/, '') + ' 亿'
}

function detectPlatform(url: string): { label: string; cls: string; mark: string } {
  try {
    const host = new URL(url).hostname.replace(/^www\./, '')
    if (host.endsWith('bilibili.com') || host.endsWith('b23.tv')) return { label: 'B 站', cls: 'bili', mark: 'B' }
    if (host.endsWith('youtube.com') || host === 'youtu.be') return { label: 'YouTube', cls: 'youtube', mark: 'Y' }
    return { label: host, cls: 'other', mark: '·' }
  } catch {
    return { label: '链接', cls: 'other', mark: '·' }
  }
}

// JobProgress shows a phase-aware progress bar while a job is still
// running. The backend currently doesn't write per-phase percentages
// into `job.progress` (the map is created but never updated mid-flight),
// so when no value is available we fall back to an indeterminate
// animation. The phase label + step indicator at least tells the user
// "we're in extraction, not stuck" even without fine-grained percents.
function JobProgress({ job }: { job: Job }) {
  if (!IN_FLIGHT_PHASES.includes(job.phase)) return null
  const stepIdx = IN_FLIGHT_PHASES.indexOf(job.phase) // 0..3
  const stepCount = IN_FLIGHT_PHASES.length
  const pctRaw = job.progress?.[job.phase]
  const hasPct = typeof pctRaw === 'number' && pctRaw > 0
  const pct = hasPct ? Math.max(0, Math.min(100, pctRaw as number)) : null

  return (
    <div className="job-progress" role="status" aria-live="polite">
      <div className="job-progress-head">
        <span className="job-progress-label">{PHASE_LABEL[job.phase] ?? job.phase}</span>
        <span className="job-progress-step">
          步骤 {stepIdx + 1} / {stepCount}
        </span>
        <div style={{ flex: 1 }} />
        {pct !== null && (
          <span className="job-progress-percent">{pct.toFixed(0)}%</span>
        )}
      </div>
      <div className={`job-progress-bar${pct === null ? ' is-indeterminate' : ''}`}>
        <div
          className="job-progress-fill"
          style={pct !== null ? { width: `${pct}%` } : undefined}
        />
      </div>
      {job.message && (
        <div className="job-progress-msg" title={job.message}>{job.message}</div>
      )}
    </div>
  )
}

// PosterWithFallback renders the per-job thumbnail served by the API,
// degrading gracefully to the original gradient-letter placeholder when
// the image fails to load. This covers three cases without extra logic:
//   1. Older jobs (no thumbnail was ever downloaded → 404)
//   2. Brand-new jobs whose poster is still mid-fetch (404 → retry on
//      next page load is fine; we don't auto-poll)
//   3. yt-dlp didn't expose a thumbnail URL at all
function PosterWithFallback({ jobId, mark }: { jobId: string; mark: string }) {
  const [failed, setFailed] = useState(false)
  if (failed) {
    return (
      <div className="video-card-poster" aria-hidden>
        {mark}
      </div>
    )
  }
  return (
    <div className="video-card-poster has-img" aria-hidden>
      <img
        src={api.thumbnailURL(jobId)}
        alt=""
        loading="lazy"
        onError={() => setFailed(true)}
      />
    </div>
  )
}

function VisualInsightsPanel({ job, logs }: { job: Job; logs: LogLine[] }) {
  const frames = job.frames ?? []
  const visionStatus = visionStatusOf(job)
  const isAnalyzing = visionStatus === 'pending' || visionStatus === 'running'
  const isFailed = visionStatus === 'failed'
  const latestLog = [...logs].reverse().find((l) => l.phase === 'analyzing' && l.message)
  const totals = summarizeFrames(frames)

  return (
    <div className="frames-panel">
      {isAnalyzing && (
        <div className="frames-status" role="status" aria-live="polite">
          <div className="frames-status-title">画面理解正在后台进行</div>
          <div className="frames-status-msg">
            不影响转写、导出和 AI 总结；完成后这里会自动出现关键帧结果。
          </div>
          {(job.vision_message || latestLog?.message) && (
            <div className="frames-status-msg">
              {job.vision_message ? `当前状态：${job.vision_message}` : null}
              {job.vision_message && latestLog?.message ? ' · ' : null}
              {latestLog?.message ? `日志：${latestLog.message}` : null}
            </div>
          )}
        </div>
      )}
      {isFailed && (
        <div className="error">
          画面理解失败，但转写任务已完成：{job.vision_error || job.vision_message || '请查看日志'}
        </div>
      )}

      {frames.length > 0 ? (
        <>
          <div className="frames-summary">
            <div className="frames-summary-item">
              <span className="k">总帧数</span>
              <span className="v">{frames.length}</span>
            </div>
            <div className="frames-summary-item">
              <span className="k">总 Token</span>
              <span className="v">{formatTokens(totals.totalTokens)}</span>
            </div>
            <div className="frames-summary-item">
              <span className="k">总估算费用</span>
              <span className="v">{totals.totalCostText}</span>
            </div>
            <div className="frames-summary-item">
              <span className="k">总耗时</span>
              <span className="v">{formatDurationMs(totals.totalDurationMs)}</span>
            </div>
          </div>
          {totals.costIsNA && (
            <div className="frames-cost-note">
              费用 N/A 通常表示 VLM 未返回 prompt/completion tokens，或 `scribe-vlm.json` 未配置输入/输出单价。
            </div>
          )}
          <div className="frames-list">
            {frames.map((frame) => (
              <article className="frame-card" key={`${frame.index}-${frame.timestamp_sec}`}>
                <div className="frame-shot">
                  <img
                    src={api.frameURL(job.id, frame.index)}
                    alt={`关键帧 ${formatTime(frame.timestamp_sec)}`}
                    loading="lazy"
                  />
                </div>
                <div className="frame-body">
                  <div className="frame-head">
                    <span className="frame-time">{formatTime(frame.timestamp_sec)}</span>
                    <span className="frame-index">Frame #{frame.index + 1}</span>
                  </div>
                  <div className="frame-caption">
                    {frame.caption || '无画面描述'}
                  </div>
                  <div className="frame-metrics">
                    Tokens <b>{formatTokens(frame.tokens_used)}</b>
                    <span className="dot">·</span>
                    费用 <b>{frameCostText(frame)}</b>
                    <span className="dot">·</span>
                    耗时 <b>{formatDurationMs(frame.duration_ms)}</b>
                  </div>
                  {(frame.prompt_tokens || frame.completion_tokens) ? (
                    <div className="frame-token-detail">
                      输入 {formatTokens(frame.prompt_tokens)} / 输出 {formatTokens(frame.completion_tokens)}
                    </div>
                  ) : (
                    <div className="frame-token-detail">
                      无法精确拆分输入/输出 token，费用显示为 N/A。
                    </div>
                  )}
                  {frame.ocr_text && (
                    <div className="frame-ocr">
                      <div className="frame-ocr-title">OCR 文本</div>
                      <pre>{frame.ocr_text}</pre>
                    </div>
                  )}
                  {frame.error && (
                    <div className="frame-error">失败信息：{frame.error}</div>
                  )}
                </div>
              </article>
            ))}
          </div>
        </>
      ) : (
        !isAnalyzing && (
          <div className="empty">
            {isFailed
              ? '本任务没有可展示的画面理解结果。'
              : '本任务没有画面理解结果；可能是 VLM 未配置、视频没有可抽帧画面，或该任务在 VLM 功能上线前生成。'}
          </div>
        )
      )}
    </div>
  )
}

export default function JobDetail() {
  const { id = '' } = useParams()
  const nav = useNavigate()
  const [job, setJob] = useState<Job | null>(null)
  const [tab, setTab] = useState<Tab>('brief')
  const [liveLogs, setLiveLogs] = useState<LogLine[]>([])
  const [error, setError] = useState<string | null>(null)
  const [deleting, setDeleting] = useState(false)
  // dispatchErrors holds 4xx/5xx errors from the POST /summarize call
  // itself (e.g. LLM not configured). These are transient — the
  // persisted entry status covers the "in-flight / completed / failed"
  // lifecycle. We surface dispatch errors in the panel until the user
  // hits 重新生成 again.
  const [dispatchErrors, setDispatchErrors] = useState<Partial<Record<SummaryKind, { error: string; hint: string | null }>>>({})
  const logsRef = useRef<HTMLDivElement | null>(null)

  function recordSummary(s: Summary) {
    setJob((prev) => {
      if (!prev) return prev
      const next: Job = { ...prev, summaries: { ...(prev.summaries ?? {}), [s.kind]: s } }
      return next
    })
  }

  async function generateSummary(kind: SummaryKind) {
    // Optimistic local pending so the button flips to spinner
    // instantly, before the POST /summarize round-trip resolves.
    setDispatchErrors((prev) => ({ ...prev, [kind]: undefined }))
    setJob((prev) => {
      if (!prev) return prev
      return {
        ...prev,
        summaries: {
          ...(prev.summaries ?? {}),
          [kind]: {
            kind,
            status: 'pending',
            generated_at: new Date().toISOString(),
          },
        },
      }
    })
    try {
      const res = await api.summarize(id, kind)
      // Server reply may be pending (202/409) or already done; either
      // way it's authoritative — overwrite the optimistic entry.
      recordSummary(res)
    } catch (e) {
      let errMsg = String(e)
      let hint: string | null = null
      if (e instanceof ApiError) {
        const body = (e.body ?? {}) as { error?: string; hint?: string; detail?: string }
        errMsg = body.error || e.message
        hint = body.hint || body.detail || null
      }
      // Dispatch failed → roll back the optimistic entry (if it was
      // optimistic; if there was a prior done/failed entry, just
      // surface the dispatch error alongside it).
      setJob((prev) => {
        if (!prev || !prev.summaries) return prev
        const existing = prev.summaries[kind]
        if (existing && existing.status === 'pending' && !existing.markdown) {
          const { [kind]: _drop, ...rest } = prev.summaries
          return { ...prev, summaries: rest }
        }
        return prev
      })
      setDispatchErrors((prev) => ({ ...prev, [kind]: { error: errMsg, hint } }))
    }
  }

  async function doDelete() {
    if (deleting) return
    if (!confirm('确定删除这条转写记录吗？\n视频文件和转写文本都会被删除，无法恢复。')) return
    setDeleting(true)
    try {
      await api.deleteJob(id)
      nav('/')
    } catch (err) {
      setError(`删除失败：${err}`)
      setDeleting(false)
    }
  }

  useEffect(() => {
    let stopped = false
    api.getJob(id).then(setJob).catch((e) => setError(String(e)))
    const close = streamJob(id, {
      onSnapshot: (j) => {
        if (!stopped) setJob(j)
      },
      onEvent: (ev: JobEvent) => {
        setLiveLogs((prev) => [
          ...prev,
          { at: new Date().toISOString(), phase: ev.phase, message: ev.message ?? '' },
        ])
        if (ev.done || ev.phase === 'done' || ev.phase === 'failed') {
          api.getJob(id).then(setJob).catch(() => {})
        }
      },
    })
    return () => {
      stopped = true
      close()
    }
  }, [id])

  useEffect(() => {
    if (logsRef.current) logsRef.current.scrollTop = logsRef.current.scrollHeight
  }, [liveLogs.length])

  // Poll while any summary entry is still pending. SSE could push a
  // summary event instead, but the existing /events bus is phase-only
  // and polling is fine for a handful of LLM calls.
  const hasPending = useMemo(() => {
    if (!job?.summaries) return false
    return Object.values(job.summaries).some((s) => s?.status === 'pending')
  }, [job?.summaries])
  const hasVisionPending = useMemo(() => {
    if (!job) return false
    const status = visionStatusOf(job)
    return status === 'pending' || status === 'running'
  }, [job])

  useEffect(() => {
    if (!hasPending && !hasVisionPending) return
    let cancelled = false
    const timer = setInterval(() => {
      api.getJob(id)
        .then((j) => { if (!cancelled) setJob(j) })
        .catch(() => { /* ignore — next tick will retry */ })
    }, 1500)
    return () => {
      cancelled = true
      clearInterval(timer)
    }
  }, [hasPending, hasVisionPending, id])

  const combinedLogs = useMemo(() => {
    const persisted = job?.logs ?? []
    return [...persisted, ...liveLogs]
  }, [job?.logs, liveLogs])

  if (!job && !error) {
    return (
      <div className="shell">
        <Brand />
        <div className="empty">加载中…</div>
      </div>
    )
  }

  if (error && !job) {
    return (
      <div className="shell">
        <Brand />
        <div className="error">{error}</div>
        <Link to="/" className="btn secondary" style={{ marginTop: 12 }}>
          ← 返回首页
        </Link>
      </div>
    )
  }

  const j = job!
  const platform = detectPlatform(j.source?.webpage_url || j.url)
  const duration = j.source?.duration ? formatTime(j.source.duration) : null
  const canDelete = j.phase === 'done' || j.phase === 'failed'

  return (
    <div className="shell">
      <Brand />

      <section className="video-card">
        <div className="video-card-head">
          <PosterWithFallback jobId={j.id} mark={platform.mark} />
          <div className="video-card-main">
            <div className="video-card-pills">
              <span className={`platform-badge ${platform.cls}`}>{platform.label}</span>
              {duration && <span className="duration-badge">{duration}</span>}
              <PhaseBadge phase={j.phase} />
            </div>
            <h1 className="video-card-title">{j.source?.title || j.url}</h1>
            <div className="video-card-url">
              <a href={j.url} target="_blank" rel="noreferrer">{j.url}</a>
            </div>
          </div>
        </div>

        <JobProgress job={j} />

        {j.error && <div className="error" style={{ marginTop: 16 }}>{j.error}</div>}
        {error && <div className="error" style={{ marginTop: 16 }}>{error}</div>}

        <div className="video-card-meta">
          {j.source?.uploader && (
            <div className="video-card-meta-item">
              <span className="k">上传者</span>
              <span className="v">{j.source.uploader}</span>
            </div>
          )}
          {/* Engagement counters from yt-dlp. Each platform exposes a
              different subset (YouTube has no 收藏/分享), so we render
              each one only when the source actually carries a value.
              Old jobs persisted before this field landed simply show
              nothing — no migration is needed. */}
          {(() => {
            const counts: { k: string; v: number | undefined }[] = [
              { k: '播放', v: j.source?.view_count },
              { k: '点赞', v: j.source?.like_count },
              { k: '收藏', v: j.source?.favorite_count },
              { k: '评论', v: j.source?.comment_count },
              { k: '分享', v: j.source?.repost_count },
            ]
            return counts
              .filter((c) => formatCount(c.v) !== null)
              .map((c) => (
                <div className="video-card-meta-item" key={c.k}>
                  <span className="k">{c.k}</span>
                  <span className="v">{formatCount(c.v)}</span>
                </div>
              ))
          })()}
          <div className="video-card-meta-item">
            <span className="k">模型</span>
            <span className="v">{j.model}</span>
          </div>
          <div className="video-card-meta-item">
            <span className="k">语言</span>
            <span className="v">{j.language || 'auto'}</span>
          </div>
          <div className="video-card-meta-item">
            <span className="k">创建时间</span>
            <span className="v">{new Date(j.created_at).toLocaleString()}</span>
          </div>
          {j.finished_at && (
            <div className="video-card-meta-item">
              <span className="k">完成时间</span>
              <span className="v">{new Date(j.finished_at).toLocaleString()}</span>
            </div>
          )}
          <div className="video-card-meta-item">
            <span className="k">任务 ID</span>
            <span className="v" style={{ fontFamily: 'JetBrains Mono, Menlo, Consolas, monospace', fontSize: 12, color: 'var(--muted)' }}>{j.id}</span>
          </div>
        </div>

        <div className="toolbar" style={{ marginTop: 22 }}>
          {j.transcript && (
            <>
              <a className="btn secondary" href={api.exportURL(j.id, 'srt')} download>导出 SRT</a>
              <a className="btn secondary" href={api.exportURL(j.id, 'md')} download>导出 Markdown</a>
              <a className="btn secondary" href={api.exportURL(j.id, 'txt')} download>导出 TXT</a>
            </>
          )}
          <button
            type="button"
            className="btn danger"
            style={{ marginLeft: 'auto' }}
            disabled={!canDelete || deleting}
            title={canDelete ? '删除该任务及其音视频文件' : '任务进行中，无法删除'}
            onClick={doDelete}
          >
            {deleting ? '删除中…' : '删除任务'}
          </button>
        </div>
      </section>

      {j.transcript ? (
        <>
          <div className="tabs scroll-x">
            {SUMMARY_TABS.map((t) => (
              <button
                key={t.key}
                className={`tab ${tab === t.key ? 'active' : ''}`}
                onClick={() => setTab(t.key)}
              >
                {t.label}
                {j.summaries?.[t.key as SummaryKind] && <span className="tab-dot" />}
              </button>
            ))}
            <span className="tab-sep" />
            <button className={`tab ${tab === 'transcript' ? 'active' : ''}`} onClick={() => setTab('transcript')}>正文</button>
            <button className={`tab ${tab === 'segments' ? 'active' : ''}`} onClick={() => setTab('segments')}>分段</button>
            <button className={`tab ${tab === 'frames' ? 'active' : ''}`} onClick={() => setTab('frames')}>
              画面理解
              {(j.frames?.length ?? 0) > 0 && <span className="tab-dot" />}
            </button>
            <button className={`tab ${tab === 'logs' ? 'active' : ''}`} onClick={() => setTab('logs')}>日志</button>
          </div>

          {(tab === 'brief' || tab === 'detailed' || tab === 'outline' || tab === 'mindmap') && (() => {
            // UI is driven entirely by the persisted entry status now:
            //   no entry  → "生成总结" button
            //   pending   → spinner ("正在请求大模型…")
            //   done      → render markdown / mindmap
            //   failed    → show server error + "重新生成"
            // Dispatch errors (LLM not configured, 429, ...) come from
            // the POST /summarize call itself; they're separate from
            // the persisted entry and shown alongside.
            const entry = j.summaries?.[tab as SummaryKind]
            const dispatch = dispatchErrors[tab as SummaryKind]
            return (
              <SummaryPanel
                kind={tab}
                entry={entry}
                framesCount={j.frames?.length ?? 0}
                visionStatus={visionStatusOf(j)}
                dispatchError={dispatch ?? null}
                onGenerate={() => generateSummary(tab as SummaryKind)}
              />
            )
          })()}
          {tab === 'transcript' && (
            <div className="transcript">{j.transcript.full_text}</div>
          )}
          {tab === 'segments' && (
            <div className="segments-list">
              {j.transcript.segments.map((s, i) => (
                <div className="segment" key={i}>
                  <div className="segment-time">
                    {formatTime(s.start)} → {formatTime(s.end)}
                  </div>
                  <div className="segment-text">{s.text}</div>
                </div>
              ))}
            </div>
          )}
          {tab === 'frames' && (
            <VisualInsightsPanel job={j} logs={combinedLogs} />
          )}
          {tab === 'logs' && (
            <div className="logs" ref={logsRef}>
              {combinedLogs.map((l, i) => (
                <div className="log-line" key={i}>
                  <span className="phase">[{l.phase}]</span>
                  {l.message}
                </div>
              ))}
            </div>
          )}
        </>
      ) : (
        <>
          <div className="section-head">
            <h2 className="section-title">实时日志</h2>
            <span className="section-hint">通过 SSE 流式更新</span>
          </div>
          <div className="logs" ref={logsRef}>
            {combinedLogs.length === 0 ? (
              <div style={{ opacity: 0.6 }}>等待第一条日志…</div>
            ) : (
              combinedLogs.map((l, i) => (
                <div className="log-line" key={i}>
                  <span className="phase">[{l.phase}]</span>
                  {l.message}
                </div>
              ))
            )}
          </div>
        </>
      )}

      <div style={{ marginTop: 28 }}>
        <Link to="/" className="btn secondary">← 返回首页</Link>
      </div>
    </div>
  )
}
