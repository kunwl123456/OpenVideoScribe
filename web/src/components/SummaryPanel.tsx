import { useState } from 'react'
import { type Summary, type SummaryKind, type VisionStatus } from '../api/client'
import Markdown from './Markdown'
import MindmapView from './MindmapView'

// SummaryPanel is a pure presentational component driven entirely by
// the persisted Summary entry. The four lifecycle states map to UI:
//
//   entry == undefined        → "还没生成" + 生成总结 button
//   entry.status == pending   → spinner "正在请求大模型…"
//   entry.status == done      → render markdown / mindmap + meta row
//   entry.status == failed    → show entry.error + 重新生成 button
//
// Dispatch errors (LLM not configured, 429 from POST itself) come in
// via dispatchError and stack on top of whatever the entry shows.

type Props = {
  kind: SummaryKind
  entry?: Summary
  framesCount: number
  visionStatus?: VisionStatus
  dispatchError: { error: string; hint: string | null } | null
  onGenerate: () => void
}

const KIND_HINTS: Record<SummaryKind, string> = {
  brief: '一句话总结视频核心观点（约 50 字）',
  detailed: '300 字左右的详细摘要，覆盖主线、论据与结论',
  outline: '层级化的 Markdown 大纲，方便快速浏览',
  mindmap: '可视化思维导图，可拖拽 / 缩放查看',
  study_notes: '学习笔记：概念、结论、易错点与复习清单',
  wechat_article: '公众号文案：导语、分节正文与结尾互动',
  course_handout: '课程讲义：目标、要点、练习与作业',
  short_video_script: '短视频脚本：口播、字幕与互动话术',
  quote_cards: '金句卡片：金句 + 解读 + 适用场景',
}

export default function SummaryPanel({ kind, entry, framesCount, visionStatus, dispatchError, onGenerate }: Props) {
  const [copied, setCopied] = useState(false)

  const status = entry?.status
  const isPending = status === 'pending'
  const isDone = status === 'done' && !!entry?.markdown
  const isFailed = status === 'failed'
  const visualHint = (() => {
    if (framesCount > 0) {
      return (
        <div className="summary-visual-hint">
          本次 AI 总结会融合 {framesCount} 条画面理解结果。
          若画面理解是在摘要之后完成，请点击重新生成以融合画面信息。
        </div>
      )
    }
    if (visionStatus === 'pending' || visionStatus === 'running') {
      return (
        <div className="summary-visual-hint">
          当前总结仅基于转写；画面理解完成后可重新生成视觉增强版。
        </div>
      )
    }
    return null
  })()

  async function copy() {
    if (!entry?.markdown) return
    try {
      await navigator.clipboard.writeText(entry.markdown)
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    } catch {
      // best effort; ignore
    }
  }

  // -- DONE: full meta + body --
  if (isDone && entry) {
    return (
      <div className="summary-panel">
        {visualHint}
        <div className="summary-meta">
          <span className="summary-meta-item">
            模型 <b>{entry.model || '—'}</b>
          </span>
          {entry.tokens_used ? (
            <span className="summary-meta-item">
              Tokens <b>{entry.tokens_used}</b>
              {(entry.prompt_tokens || entry.completion_tokens) ? (
                <span style={{ opacity: 0.7, marginLeft: 6 }}>
                  （输入 {entry.prompt_tokens ?? '—'} / 输出 {entry.completion_tokens ?? '—'}）
                </span>
              ) : null}
            </span>
          ) : null}
          <span
            className="summary-meta-item"
            title={
              entry.estimated_cost_text
                ? '基于 scribe-llm.json 中的 price_input_per_mtok / price_output_per_mtok 估算'
                : '在 scribe-llm.json 配置 price_input_per_mtok / price_output_per_mtok 即可估算费用'
            }
          >
            费用 <b>{entry.estimated_cost_text || 'N/A'}</b>
          </span>
          {entry.duration_ms ? (
            <span className="summary-meta-item">
              耗时 <b>{(entry.duration_ms / 1000).toFixed(1)}s</b>
            </span>
          ) : null}
          <span className="summary-meta-item">
            生成于 <b>{new Date(entry.generated_at).toLocaleString()}</b>
          </span>
          <div style={{ flex: 1 }} />
          <button
            type="button"
            className="btn secondary"
            onClick={copy}
            style={{ height: 32, padding: '0 12px', fontSize: 12.5 }}
          >
            {copied ? '已复制 ✓' : '复制 Markdown'}
          </button>
          <button
            type="button"
            className="btn ghost"
            onClick={onGenerate}
            style={{ height: 32, padding: '0 12px', fontSize: 12.5 }}
          >
            重新生成
          </button>
        </div>
        {kind === 'mindmap' ? (
          <MindmapView markdown={entry.markdown!} />
        ) : (
          <div className="summary-body">
            <Markdown source={entry.markdown!} />
          </div>
        )}
        {dispatchError && (
          <div className="error" style={{ marginTop: 12 }}>
            {dispatchError.error}
            {dispatchError.hint && (
              <div style={{ marginTop: 6, fontSize: 12.5 }}>{dispatchError.hint}</div>
            )}
          </div>
        )}
      </div>
    )
  }

  // -- PENDING / EMPTY / FAILED: call-to-action card --
  return (
    <div className="summary-panel">
      <div className="summary-empty">
        {isFailed ? (
          <>
            <div className="summary-empty-title">{kindLabel(kind)} 生成失败</div>
            {visualHint}
            <div className="summary-empty-hint">{entry?.error || '请重试'}</div>
            <button
              type="button"
              className="btn"
              onClick={onGenerate}
              style={{ marginTop: 18 }}
            >
              重新生成 →
            </button>
          </>
        ) : (
          <>
            <div className="summary-empty-title">
              {isPending ? `正在生成${kindLabel(kind)}…` : `还没生成${kindLabel(kind)}`}
            </div>
            {visualHint}
            <div className="summary-empty-hint">{KIND_HINTS[kind]}</div>
            <button
              type="button"
              className="btn"
              onClick={onGenerate}
              disabled={isPending}
              style={{ marginTop: 18 }}
            >
              {isPending ? (
                <>
                  <span className="spinner" /> 正在请求大模型…
                </>
              ) : (
                '生成总结 →'
              )}
            </button>
          </>
        )}
        {dispatchError && (
          <div className="error" style={{ marginTop: 16, textAlign: 'left' }}>
            <div style={{ fontWeight: 600 }}>{dispatchError.error}</div>
            {dispatchError.hint && (
              <div style={{ marginTop: 6, fontSize: 12.5 }}>{dispatchError.hint}</div>
            )}
          </div>
        )}
      </div>
    </div>
  )
}

function kindLabel(k: SummaryKind): string {
  switch (k) {
    case 'brief': return 'AI 总结'
    case 'detailed': return '详细摘要'
    case 'outline': return '大纲'
    case 'mindmap': return '思维导图'
    case 'study_notes': return '学习笔记'
    case 'wechat_article': return '公众号文案'
    case 'course_handout': return '课程讲义'
    case 'short_video_script': return '短视频脚本'
    case 'quote_cards': return '金句卡片'
  }
}
