// Tiny API client. We hand-roll fetch + EventSource because the server
// only has half a dozen endpoints; a full SDK would be overkill.

export type ModelStatus = {
  key: string
  filename: string
  bytes: number
  label: string
  installed: boolean
}

export type ModelProgress = {
  key: string
  fraction: number
  message: string
  done: boolean
  error?: string
}

export type Phase =
  | 'queued'
  | 'downloading'
  | 'extracting'
  | 'transcribing'
  | 'analyzing'
  | 'done'
  | 'failed'

export type VisionStatus =
  | 'disabled'
  | 'pending'
  | 'running'
  | 'done'
  | 'failed'

export type FrameInsight = {
  index: number
  timestamp_sec: number
  image_path?: string
  caption: string
  ocr_text?: string
  tokens_used?: number
  prompt_tokens?: number
  completion_tokens?: number
  estimated_cost?: number
  estimated_cost_text?: string
  duration_ms?: number
  error?: string
}

export type Segment = {
  start: number
  end: number
  text: string
}

export type TranscriptResult = {
  language: string
  model: string
  duration: number
  segments: Segment[]
  full_text: string
}

export type SourceInfo = {
  id: string
  title: string
  uploader: string
  duration: number
  webpage_url: string
  thumbnail?: string
  view_count?: number
  like_count?: number
  comment_count?: number
  favorite_count?: number
  repost_count?: number
}

export type LogLine = {
  at: string
  phase: Phase
  message: string
}

export type SummaryKind = 'brief' | 'detailed' | 'outline' | 'mindmap'

export type SummaryStatus = 'pending' | 'done' | 'failed'

export type Summary = {
  kind: SummaryKind
  status: SummaryStatus
  markdown?: string
  model?: string
  tokens_used?: number
  prompt_tokens?: number
  completion_tokens?: number
  estimated_cost?: number
  estimated_cost_text?: string
  duration_ms?: number
  error?: string
  generated_at: string
}

export type SummaryError = {
  error: string
  hint?: string
  detail?: string
}

export type Job = {
  id: string
  url: string
  model: string
  language: string
  phase: Phase
  message?: string
  error?: string
  created_at: string
  started_at?: string
  finished_at?: string
  source?: SourceInfo
  transcript?: TranscriptResult
  media_path?: string
  vision_status?: VisionStatus
  vision_message?: string
  vision_error?: string
  vision_started_at?: string
  vision_finished_at?: string
  frames_dir?: string
  frames?: FrameInsight[]
  logs?: LogLine[]
  progress?: Partial<Record<Phase, number>>
  summaries?: Partial<Record<SummaryKind, Summary>>
}

export type JobEvent = {
  job_id: string
  phase: Phase
  message?: string
  done?: boolean
  error?: string
  vision_status?: VisionStatus
}

// ApiError carries the HTTP status + any structured JSON body the
// server attached (e.g. {error, hint, detail} from /summarize on 503).
// UI code that wants to display friendly hints can do
// `if (err instanceof ApiError && err.status === 503) ...`.
export class ApiError extends Error {
  status: number
  body: unknown
  constructor(status: number, message: string, body: unknown) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.body = body
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    headers: { 'Content-Type': 'application/json' },
    ...init,
  })
  if (!res.ok) {
    const text = await res.text().catch(() => '')
    let body: unknown = text
    let message = text
    try {
      body = JSON.parse(text)
      if (body && typeof body === 'object' && 'error' in (body as any)) {
        message = String((body as any).error)
      }
    } catch {
      // not JSON — keep the plain text
    }
    throw new ApiError(res.status, message || `HTTP ${res.status}`, body)
  }
  if (res.status === 204) return undefined as unknown as T
  return res.json() as Promise<T>
}

export const api = {
  listModels: () =>
    request<{ models: ModelStatus[]; progress: ModelProgress[] }>('/api/models'),
  downloadModel: (key: string) =>
    request<{ status: string }>(`/api/models/${encodeURIComponent(key)}/download`, {
      method: 'POST',
    }),
  listJobs: () => request<{ jobs: Job[] }>('/api/jobs'),
  createJob: (payload: { url: string; model: string; language?: string }) =>
    request<Job>('/api/jobs', { method: 'POST', body: JSON.stringify(payload) }),
  getJob: (id: string) => request<Job>(`/api/jobs/${encodeURIComponent(id)}`),
  deleteJob: (id: string) =>
    request<void>(`/api/jobs/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  // summarize triggers an async generation on the server. Returns
  // resolved with:
  //   - 200 + done body (legacy / unused now but tolerated)
  //   - 202 + pending body (new normal path)
  //   - 409 + pending body when another request is already in-flight
  //     for the same (job, kind). The frontend treats 409 the same as
  //     202: "someone else started it; keep polling".
  // Real failures (401/429/500/503/...) still throw ApiError so the
  // UI can map them to friendly hints.
  summarize: async (id: string, kind: SummaryKind): Promise<Summary> => {
    const path = `/api/jobs/${encodeURIComponent(id)}/summarize?kind=${encodeURIComponent(kind)}`
    const res = await fetch(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
    })
    const text = await res.text().catch(() => '')
    let body: unknown = text
    try { body = JSON.parse(text) } catch { /* keep text */ }
    if (res.status === 200 || res.status === 202) {
      return body as Summary
    }
    // 409 only counts as pending when the body says so; some other
    // 409 (e.g. "transcript not ready") is a real error.
    if (res.status === 409 && body && typeof body === 'object' && (body as { status?: string }).status === 'pending') {
      return body as Summary
    }
    const message = (body && typeof body === 'object' && 'error' in (body as Record<string, unknown>))
      ? String((body as { error: unknown }).error)
      : (text || `HTTP ${res.status}`)
    throw new ApiError(res.status, message, body)
  },
  exportURL: (id: string, format: 'srt' | 'md' | 'txt') =>
    `/api/jobs/${encodeURIComponent(id)}/export?format=${format}`,
  thumbnailURL: (id: string) => `/api/jobs/${encodeURIComponent(id)}/thumbnail`,
  frameURL: (id: string, index: number) =>
    `/api/jobs/${encodeURIComponent(id)}/frames/${index}`,
}

// streamJob opens an SSE connection. Returns a closer.
export function streamJob(
  id: string,
  handlers: {
    onSnapshot?: (job: Job) => void
    onEvent?: (ev: JobEvent) => void
  },
): () => void {
  const url = `/api/jobs/${encodeURIComponent(id)}/events`
  const es = new EventSource(url)
  es.addEventListener('snapshot', (e) => {
    try {
      handlers.onSnapshot?.(JSON.parse((e as MessageEvent).data))
    } catch (err) {
      console.warn('snapshot parse failed', err)
    }
  })
  es.addEventListener('event', (e) => {
    try {
      handlers.onEvent?.(JSON.parse((e as MessageEvent).data))
    } catch (err) {
      console.warn('event parse failed', err)
    }
  })
  es.onerror = () => {
    // Browsers retry SSE automatically. Nothing to do here unless we
    // want to surface "lost connection" UI; the snapshot on reconnect
    // will refresh the state.
  }
  return () => es.close()
}
