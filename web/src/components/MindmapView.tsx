import { useEffect, useRef, useState } from 'react'

// MindmapView renders Markmap-flavoured Markdown into an interactive
// SVG using markmap-lib (parse) + markmap-view (D3 render).
//
// Design notes:
// - We lazy-import both packages so the main bundle stays small; the
//   bundle hop only happens once the user actually opens "思维导图".
// - markmap-view mutates the SVG it's bound to; we recreate the
//   Markmap instance whenever the markdown changes to avoid stale
//   subtree state.
// - stripCodeFence already runs on the backend, but we keep a tiny
//   guard here for the rare case where the model wraps the body in
//   ```markmap ... ``` despite the prompt — markmap-lib otherwise
//   shows literal backticks at the root, which looks broken.

type Props = {
  markdown: string
  height?: number
}

function unfence(src: string): string {
  const trimmed = src.trim()
  if (!trimmed.startsWith('```')) return trimmed
  const firstNl = trimmed.indexOf('\n')
  if (firstNl < 0) return trimmed
  const body = trimmed.slice(firstNl + 1)
  const close = body.lastIndexOf('```')
  return (close >= 0 ? body.slice(0, close) : body).trim()
}

export default function MindmapView({ markdown, height = 520 }: Props) {
  const svgRef = useRef<SVGSVGElement | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    let cancelled = false
    let cleanup: (() => void) | null = null
    setLoading(true)
    setError(null)

    ;(async () => {
      try {
        const [{ Transformer }, viewMod] = await Promise.all([
          import('markmap-lib'),
          import('markmap-view'),
        ])
        if (cancelled) return
        const Markmap = (viewMod as { Markmap: typeof import('markmap-view').Markmap }).Markmap
        const transformer = new Transformer()
        const { root } = transformer.transform(unfence(markdown))
        if (cancelled || !svgRef.current) return
        // Wipe the previous render before binding a new Markmap so
        // re-renders (regenerate / markdown prop changes) don't stack
        // ghost nodes.
        svgRef.current.innerHTML = ''
        const mm = Markmap.create(svgRef.current, undefined, root)
        // Fit after mount so all nodes are visible at first paint.
        setTimeout(() => {
          try { mm.fit() } catch {/* ignore */}
        }, 0)
        setLoading(false)
        cleanup = () => {
          try { mm.destroy() } catch {/* ignore */}
        }
      } catch (e) {
        if (!cancelled) {
          setError(e instanceof Error ? e.message : String(e))
          setLoading(false)
        }
      }
    })()

    return () => {
      cancelled = true
      if (cleanup) cleanup()
    }
  }, [markdown])

  if (error) {
    return (
      <div className="error" style={{ marginTop: 8 }}>
        思维导图渲染失败：{error}
      </div>
    )
  }

  return (
    <div className="mindmap-wrap" style={{ height }}>
      {loading && (
        <div className="mindmap-loading">
          <span className="spinner" /> 正在渲染思维导图…
        </div>
      )}
      <svg ref={svgRef} className="mindmap-svg" />
    </div>
  )
}
