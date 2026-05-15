import { useEffect, useMemo, useRef, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { api, Job, JobEvent, LogLine, streamJob } from '../api/client'
import Brand from '../components/Brand'
import PhaseBadge from '../components/PhaseBadge'

type Tab = 'transcript' | 'segments' | 'logs'

function formatTime(sec: number) {
  if (!isFinite(sec)) return '00:00'
  const m = Math.floor(sec / 60)
  const s = Math.floor(sec % 60)
  return `${m.toString().padStart(2, '0')}:${s.toString().padStart(2, '0')}`
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

export default function JobDetail() {
  const { id = '' } = useParams()
  const [job, setJob] = useState<Job | null>(null)
  const [tab, setTab] = useState<Tab>('transcript')
  const [liveLogs, setLiveLogs] = useState<LogLine[]>([])
  const [error, setError] = useState<string | null>(null)
  const logsRef = useRef<HTMLDivElement | null>(null)

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

  return (
    <div className="shell">
      <Brand />

      <section className="video-card">
        <div className="video-card-head">
          <div className="video-card-poster" aria-hidden>{platform.mark}</div>
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

        {j.error && <div className="error" style={{ marginTop: 16 }}>{j.error}</div>}

        <div className="video-card-meta">
          {j.source?.uploader && (
            <div className="video-card-meta-item">
              <span className="k">上传者</span>
              <span className="v">{j.source.uploader}</span>
            </div>
          )}
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

        {j.transcript && (
          <div className="toolbar" style={{ marginTop: 22 }}>
            <a className="btn secondary" href={api.exportURL(j.id, 'srt')} download>导出 SRT</a>
            <a className="btn secondary" href={api.exportURL(j.id, 'md')} download>导出 Markdown</a>
            <a className="btn secondary" href={api.exportURL(j.id, 'txt')} download>导出 TXT</a>
          </div>
        )}
      </section>

      {j.transcript ? (
        <>
          <div className="tabs">
            <button className={`tab ${tab === 'transcript' ? 'active' : ''}`} onClick={() => setTab('transcript')}>正文</button>
            <button className={`tab ${tab === 'segments' ? 'active' : ''}`} onClick={() => setTab('segments')}>分段</button>
            <button className={`tab ${tab === 'logs' ? 'active' : ''}`} onClick={() => setTab('logs')}>日志</button>
          </div>

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
