import { useEffect, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { api, Job, ModelStatus, ModelProgress } from '../api/client'
import Brand from '../components/Brand'
import PhaseBadge from '../components/PhaseBadge'

const MODEL_STORAGE_KEY = 'scribe-web:model'
const LANGUAGE_STORAGE_KEY = 'scribe-web:language'
const VISION_STORAGE_KEY = 'scribe-web:enable-vision'
const DEFAULT_MODEL = 'tiny'
const DEFAULT_LANGUAGE = 'auto'
const MODEL_OPTIONS = [
  { key: 'tiny', label: 'Tiny' },
  { key: 'base', label: 'Base' },
  { key: 'small', label: 'Small' },
  { key: 'medium', label: 'Medium' },
]
const VALID_MODEL_KEYS = new Set(MODEL_OPTIONS.map((m) => m.key))
const VALID_LANGUAGE_KEYS = new Set(['auto', 'zh', 'en', 'ja'])

function readStoredValue(key: string, allowedValues: Set<string>, defaultValue: string): string {
  try {
    const value = window.localStorage.getItem(key)
    return value && allowedValues.has(value) ? value : defaultValue
  } catch {
    return defaultValue
  }
}

function writeStoredValue(key: string, value: string) {
  try {
    window.localStorage.setItem(key, value)
  } catch {
    // Ignore storage failures so private browsing or blocked storage does not break the form.
  }
}

function readStoredBool(key: string, defaultValue: boolean): boolean {
  try {
    const value = window.localStorage.getItem(key)
    if (value === 'true') return true
    if (value === 'false') return false
    return defaultValue
  } catch {
    return defaultValue
  }
}

function writeStoredBool(key: string, value: boolean) {
  try {
    window.localStorage.setItem(key, String(value))
  } catch {
    // Ignore storage failures so private browsing or blocked storage does not break the form.
  }
}

export default function Home() {
  const nav = useNavigate()
  const [url, setUrl] = useState('')
  const [model, setModel] = useState(() =>
    readStoredValue(MODEL_STORAGE_KEY, VALID_MODEL_KEYS, DEFAULT_MODEL),
  )
  const [language, setLanguage] = useState(() =>
    readStoredValue(LANGUAGE_STORAGE_KEY, VALID_LANGUAGE_KEYS, DEFAULT_LANGUAGE),
  )
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [models, setModels] = useState<ModelStatus[]>([])
  const [progress, setProgress] = useState<Record<string, ModelProgress>>({})
  const [jobs, setJobs] = useState<Job[]>([])
  const [modelsCollapsed, setModelsCollapsed] = useState(true)
  const [vlmEnabled, setVlmEnabled] = useState(false)
  const [enableVision, setEnableVision] = useState(() => readStoredBool(VISION_STORAGE_KEY, false))
  const [kbQuestion, setKbQuestion] = useState('')
  const [kbLoading, setKbLoading] = useState(false)
  const [kbError, setKbError] = useState<string | null>(null)
  const [kbAnswer, setKbAnswer] = useState<{ answer: string; citations: { job_id?: string; job_title?: string; start: number; end: number; text: string }[] } | null>(null)

  async function refreshModels() {
    try {
      const res = await api.listModels()
      setModels(res.models ?? [])
      const pmap: Record<string, ModelProgress> = {}
      for (const p of res.progress ?? []) pmap[p.key] = p
      setProgress(pmap)
    } catch (e) {
      console.warn('listModels failed', e)
    }
  }

  async function refreshHealth() {
    try {
      const res = await api.health()
      setVlmEnabled(!!res.vlm?.enabled)
    } catch (e) {
      console.warn('health failed', e)
      setVlmEnabled(false)
    }
  }

  function updateLanguage(nextLanguage: string) {
    setLanguage(nextLanguage)
    writeStoredValue(LANGUAGE_STORAGE_KEY, nextLanguage)
  }

  function updateEnableVision(nextEnableVision: boolean) {
    setEnableVision(nextEnableVision)
    writeStoredBool(VISION_STORAGE_KEY, nextEnableVision)
  }

  async function refreshJobs() {
    try {
      const res = await api.listJobs()
      setJobs(res.jobs ?? [])
    } catch (e) {
      console.warn('listJobs failed', e)
    }
  }

  useEffect(() => {
    refreshHealth()
    refreshModels()
    refreshJobs()
    const t = setInterval(() => {
      refreshJobs()
      refreshModels()
    }, 3000)
    return () => clearInterval(t)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setError(null)
    const raw = url.trim()
    if (!raw) {
      setError('请输入视频链接')
      return
    }
    const normalized = /^https?:\/\//i.test(raw) ? raw : `https://${raw}`
    try {
      const u = new URL(normalized)
      if (!u.host) throw new Error('no host')
    } catch {
      setError('链接格式不正确，请确认是 YouTube / B 站等视频链接')
      return
    }
    setSubmitting(true)
    try {
      const job = await api.createJob({
        url: normalized,
        model,
        language,
        enable_vision: vlmEnabled && enableVision,
      })
      nav(`/jobs/${job.id}`)
    } catch (err) {
      setError(String(err))
    } finally {
      setSubmitting(false)
    }
  }

  async function downloadModel(key: string) {
    try {
      await api.downloadModel(key)
      refreshModels()
    } catch (e) {
      setError(String(e))
    }
  }

  async function askGlobalQA() {
    const question = kbQuestion.trim()
    if (!question || kbLoading) return
    setKbLoading(true)
    setKbError(null)
    try {
      const res = await api.globalQA({ question, top_k: 6 })
      setKbAnswer({ answer: res.answer, citations: res.citations })
    } catch (err) {
      setKbError(String(err))
    } finally {
      setKbLoading(false)
    }
  }

  // Picking a not-yet-installed model from the <select> should auto-kick
  // the download — the back-end Start() is idempotent, so re-issuing
  // while a download is already running is a safe no-op.
  function handleModelChange(key: string) {
    setModel(key)
    writeStoredValue(MODEL_STORAGE_KEY, key)
    const m = models.find((x) => x.key === key)
    if (m && !m.installed) {
      const p = progress[key]
      const downloading = p && !p.done && !p.error
      if (!downloading) downloadModel(key)
    }
  }

  const installedCount = models.filter((m) => m.installed).length
  const selectedModel = models.find((m) => m.key === model)
  const selectedProgress = progress[model]
  // Treat "models not yet loaded" as ready so the form is not stuck
  // disabled on first paint before /api/models comes back.
  const selectedReady = !selectedModel || selectedModel.installed
  const selectedDownloading =
    !selectedReady && !!selectedProgress && !selectedProgress.done && !selectedProgress.error
  const selectedFailed = !selectedReady && !!selectedProgress?.error
  const selectedPercent = selectedProgress?.fraction
    ? Math.round(selectedProgress.fraction * 100)
    : 0
  const submitLabel = submitting
    ? '提交中…'
    : selectedDownloading
    ? selectedPercent > 0
      ? `模型下载中（${selectedPercent}%）`
      : '模型下载中…'
    : !selectedReady
    ? '请先下载模型'
    : '开始转写 →'

  return (
    <div className="shell">
      <Brand />

      <section className="hero">
        <span className="hero-eyebrow">
          <span style={{ width: 6, height: 6, borderRadius: '50%', background: 'var(--accent)' }} />
          本地 Whisper · 隐私优先 · 无需 API Key
        </span>
        <h1>把 B 站 / YouTube 视频<br />一键转成可搜索的文字稿</h1>
        <p className="lead">
          粘贴任意 yt-dlp 兼容的视频链接，服务器抽音 + 本地 Whisper 转写，输出带时间戳的字幕、分段与可导出的 SRT / Markdown / TXT。
        </p>
        <form className="url-form" onSubmit={submit}>
          <input
            type="text"
            inputMode="url"
            autoComplete="off"
            spellCheck={false}
            placeholder="粘贴 YouTube / B 站链接，省略 https:// 也可以"
            value={url}
            onChange={(e) => setUrl(e.target.value)}
          />
          <button
            className="btn"
            type="submit"
            disabled={submitting || !selectedReady}
          >
            {submitLabel}
          </button>
        </form>
        <div className="hero-options">
          <div className="hero-option">
            <span className="label">模型</span>
            <select
              className="select"
              value={model}
              onChange={(e) => handleModelChange(e.target.value)}
            >
              {MODEL_OPTIONS.map((option) => {
                const status = models.find((m) => m.key === option.key)
                return (
                  <option key={option.key} value={option.key}>
                    {status?.label ?? option.label} {status && !status.installed ? '（未下载）' : ''}
                  </option>
                )
              })}
            </select>
          </div>
          <div className="hero-option">
            <span className="label">语言</span>
            <select className="select" value={language} onChange={(e) => updateLanguage(e.target.value)}>
              <option value="auto">自动</option>
              <option value="zh">中文</option>
              <option value="en">English</option>
              <option value="ja">日本語</option>
            </select>
          </div>
          {vlmEnabled && (
            <label className="hero-option checkbox-option">
              <input
                type="checkbox"
                checked={enableVision}
                onChange={(e) => updateEnableVision(e.target.checked)}
              />
              <span>
                画面理解 VLM
                <small>更慢，会下载视频并抽帧</small>
              </span>
            </label>
          )}
        </div>
        {!selectedReady && selectedModel && (
          <div className="model-inline" style={{ marginTop: 16 }}>
            <div className="model-inline-head">
              <span className="model-inline-label">
                模型 <strong>{selectedModel.label}</strong> 未下载
              </span>
              {selectedDownloading && (
                <span className="model-inline-percent">
                  {selectedPercent > 0 ? `${selectedPercent}%` : '准备中…'}
                </span>
              )}
              {selectedFailed && <span className="model-inline-percent error">下载失败</span>}
              {!selectedDownloading && !selectedFailed && (
                <button
                  type="button"
                  className="btn secondary"
                  style={{ height: 30, padding: '0 12px', fontSize: 12.5 }}
                  onClick={() => downloadModel(selectedModel.key)}
                >
                  立即下载
                </button>
              )}
            </div>
            {selectedDownloading && (
              <>
                <div
                  className={`job-progress-bar${selectedPercent > 0 ? '' : ' is-indeterminate'}`}
                  style={{ marginTop: 10 }}
                >
                  <div
                    className="job-progress-fill"
                    style={selectedPercent > 0 ? { width: `${selectedPercent}%` } : undefined}
                  />
                </div>
                {selectedProgress?.message && (
                  <div className="job-progress-msg">{selectedProgress.message}</div>
                )}
              </>
            )}
            {selectedFailed && (
              <div className="model-inline-error">
                {selectedProgress?.error}
                <button
                  type="button"
                  className="btn ghost"
                  style={{ height: 28, padding: '0 10px', fontSize: 12, marginLeft: 8 }}
                  onClick={() => downloadModel(selectedModel.key)}
                >
                  重试
                </button>
              </div>
            )}
            {!selectedDownloading && !selectedFailed && (
              <div className="model-inline-hint">
                选择该模型后已自动开始下载，完成前无法提交转写
              </div>
            )}
          </div>
        )}
        {error && <div className="error" style={{ marginTop: 16 }}>{error}</div>}
      </section>

      <section className="qa-panel" style={{ marginBottom: 24 }}>
        <div className="section-head" style={{ marginTop: 0 }}>
          <h2 className="section-title">跨视频知识库问答（MVP）</h2>
        </div>
        <div className="qa-form">
          <textarea
            value={kbQuestion}
            onChange={(e) => setKbQuestion(e.target.value)}
            placeholder="例如：最近几个视频里，对 RAG 检索质量的共同观点是什么？"
          />
          <button className="btn" type="button" onClick={askGlobalQA} disabled={kbLoading || !kbQuestion.trim()}>
            {kbLoading ? '检索中…' : '提问'}
          </button>
        </div>
        {kbError && <div className="error" style={{ marginTop: 12 }}>{kbError}</div>}
        {kbAnswer && (
          <div className="qa-answer">
            <div className="qa-answer-title">回答</div>
            <div className="qa-answer-text">{kbAnswer.answer}</div>
            <div className="qa-citations">
              <div className="qa-citations-title">跨视频引用</div>
              {kbAnswer.citations.map((c, idx) => (
                <div className="qa-citation" key={`${idx}-${c.job_id}-${c.start}`}>
                  <button
                    type="button"
                    className="time-link"
                    onClick={() => c.job_id && nav(`/jobs/${c.job_id}`)}
                  >
                    [{c.job_title || c.job_id || 'unknown'}] {formatDuration(c.start)} → {formatDuration(c.end)}
                  </button>
                  <div className="qa-citation-text">{c.text}</div>
                </div>
              ))}
            </div>
          </div>
        )}
      </section>

      <div className="section-head">
        <h2 className="section-title">Whisper 模型</h2>
        <button
          className="btn ghost"
          onClick={() => setModelsCollapsed((v) => !v)}
          style={{ height: 32, padding: '0 12px', fontSize: 12.5 }}
        >
          {modelsCollapsed
            ? `展开（已安装 ${installedCount}/${models.length}）`
            : '收起'}
        </button>
      </div>
      {!modelsCollapsed && (
        models.length === 0 ? (
          <div className="empty">暂未读取到模型信息</div>
        ) : (
          <div className="model-grid">
            {models.map((m) => {
              const p = progress[m.key]
              const downloading = p && !p.done
              return (
                <div
                  className={`model-card ${m.installed ? 'installed' : ''}`}
                  key={m.key}
                >
                  <div className="model-card-head">
                    <div className="model-card-key">{m.key}</div>
                    {m.installed ? (
                      <span className="badge done">已安装</span>
                    ) : downloading ? (
                      <span className="badge downloading">下载中</span>
                    ) : null}
                  </div>
                  <div className="model-card-label">{m.label}</div>
                  {downloading && (
                    <div className="model-card-progress">
                      {p.message} {p.fraction > 0 ? `(${Math.round(p.fraction * 100)}%)` : ''}
                    </div>
                  )}
                  {p?.error && (
                    <div className="model-card-progress" style={{ color: 'var(--danger)' }}>
                      {p.error}
                    </div>
                  )}
                  {!m.installed && !downloading && (
                    <div className="model-card-action">
                      <button className="btn secondary" onClick={() => downloadModel(m.key)} style={{ width: '100%' }}>
                        下载
                      </button>
                    </div>
                  )}
                </div>
              )
            })}
          </div>
        )
      )}

      <div className="section-head">
        <h2 className="section-title">历史记录</h2>
        <span className="section-hint">{jobs.length > 0 ? `共 ${jobs.length} 条` : ''}</span>
      </div>
      {jobs.length === 0 ? (
        <div className="empty">
          还没有转写过的视频
          <div className="empty-hint">提交后所有任务都会保存在这里，下次打开还能看到</div>
        </div>
      ) : (
        jobs.map((j) => (
          <HistoryCard
            key={j.id}
            job={j}
            onOpen={() => nav(`/jobs/${j.id}`)}
            onDelete={async () => {
              if (!confirm('确定删除这条转写记录吗？\n视频文件和转写文本都会被删除，无法恢复。')) return
              try {
                await api.deleteJob(j.id)
                setJobs((prev) => prev.filter((x) => x.id !== j.id))
              } catch (err) {
                setError(`删除失败：${err}`)
              }
            }}
          />
        ))
      )}
    </div>
  )
}

// ---------------- presentation helpers ----------------

function TrashIcon() {
  return (
    <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor"
         strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
      <path d="M3 6h18" />
      <path d="M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2" />
      <path d="M19 6l-1 14a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2L5 6" />
      <path d="M10 11v6" />
      <path d="M14 11v6" />
    </svg>
  )
}

function HistoryCard({
  job,
  onOpen,
  onDelete,
}: {
  job: Job
  onOpen: () => void
  onDelete: () => void
}) {
  const platform = detectPlatform(job.source?.webpage_url || job.url)
  const duration = job.source?.duration ? formatDuration(job.source.duration) : null
  const preview = job.transcript?.full_text
    ? job.transcript.full_text.replace(/\s+/g, ' ').slice(0, 120)
    : null
  const inProgress = job.phase !== 'done' && job.phase !== 'failed'
  return (
    <div className="history-card" onClick={onOpen}>
      <div className="history-head">
        <span className={`platform-badge ${platform.cls}`}>{platform.label}</span>
        {duration && <span className="duration-badge">{duration}</span>}
        <div className="history-title">{job.source?.title || job.url}</div>
        <PhaseBadge phase={job.phase} />
        <button
          type="button"
          className="icon-btn danger"
          title={inProgress ? '任务进行中，无法删除' : '删除该转写记录'}
          aria-label="删除"
          disabled={inProgress}
          onClick={(e) => {
            e.stopPropagation()
            onDelete()
          }}
        >
          <TrashIcon />
        </button>
      </div>
      <div className="history-meta">
        {job.source?.uploader && <span>{job.source.uploader}</span>}
        {job.source?.uploader && <span className="dot" />}
        <span>{relativeTime(job.created_at)}</span>
        <span className="dot" />
        <span>{job.model}</span>
      </div>
      {preview && <div className="history-preview">{preview}…</div>}
      {job.error && (
        <div className="card-meta" style={{ color: 'var(--danger)', marginTop: 4 }}>
          {job.error}
        </div>
      )}
    </div>
  )
}

function detectPlatform(url: string): { label: string; cls: string } {
  try {
    const host = new URL(url).hostname.replace(/^www\./, '')
    if (host.endsWith('bilibili.com') || host.endsWith('b23.tv')) return { label: 'B 站', cls: 'bili' }
    if (host.endsWith('youtube.com') || host === 'youtu.be') return { label: 'YouTube', cls: 'youtube' }
    return { label: host, cls: 'other' }
  } catch {
    return { label: '链接', cls: 'other' }
  }
}

function formatDuration(sec: number): string {
  if (!isFinite(sec) || sec <= 0) return ''
  const total = Math.round(sec)
  const h = Math.floor(total / 3600)
  const m = Math.floor((total % 3600) / 60)
  const s = total % 60
  const pad = (n: number) => n.toString().padStart(2, '0')
  return h > 0 ? `${h}:${pad(m)}:${pad(s)}` : `${pad(m)}:${pad(s)}`
}

function relativeTime(iso: string): string {
  const t = new Date(iso).getTime()
  if (!isFinite(t)) return ''
  const diff = Date.now() - t
  const sec = Math.floor(diff / 1000)
  if (sec < 60) return '刚刚'
  const min = Math.floor(sec / 60)
  if (min < 60) return `${min} 分钟前`
  const hr = Math.floor(min / 60)
  if (hr < 24) return `${hr} 小时前`
  const day = Math.floor(hr / 24)
  if (day < 7) return `${day} 天前`
  const d = new Date(t)
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`
}
