import type { Phase } from '../api/client'

const LABELS: Record<Phase, string> = {
  queued: '排队中',
  downloading: '下载中',
  extracting: '抽音轨',
  transcribing: '转写中',
  analyzing: '画面理解',
  done: '完成',
  failed: '失败',
}

export default function PhaseBadge({ phase }: { phase: Phase }) {
  return <span className={`badge ${phase}`}>{LABELS[phase] ?? phase}</span>
}
