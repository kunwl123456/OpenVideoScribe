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
  | 'done'
  | 'failed'

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
}

export type LogLine = {
  at: string
  phase: Phase
  message: string
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
  logs?: LogLine[]
}

export type JobEvent = {
  job_id: string
  phase: Phase
  message?: string
  done?: boolean
  error?: string
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    headers: { 'Content-Type': 'application/json' },
    ...init,
  })
  if (!res.ok) {
    const text = await res.text().catch(() => '')
    throw new Error(text || `HTTP ${res.status}`)
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
  exportURL: (id: string, format: 'srt' | 'md' | 'txt') =>
    `/api/jobs/${encodeURIComponent(id)}/export?format=${format}`,
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
