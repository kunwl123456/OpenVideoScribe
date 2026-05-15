// A deliberately tiny Markdown renderer. We only need:
//  - Headings: `#`/`##`/`###`/`####`
//  - Unordered list items: `- ` (with two-space nesting for sub-items)
//  - Bold (**...**) and inline code (`...`)
//  - Paragraphs
// Anything else falls through as plain text. Bringing in react-markdown
// + dompurify would triple our gzip size for these four primitives.
//
// We never call dangerouslySetInnerHTML — we emit React nodes directly,
// so XSS is not a concern; user-supplied markdown can only ever produce
// text + whitelisted tags.

import { Fragment, type ReactNode } from 'react'

type Block =
  | { type: 'heading'; level: number; text: string }
  | { type: 'paragraph'; text: string }
  | { type: 'list'; items: ListItem[] }
  | { type: 'blank' }

type ListItem = {
  indent: number
  text: string
  children: ListItem[]
}

function parseBlocks(src: string): Block[] {
  const lines = src.replace(/\r\n?/g, '\n').split('\n')
  const blocks: Block[] = []
  let paraBuf: string[] = []
  let listBuf: { indent: number; text: string }[] = []

  const flushPara = () => {
    if (paraBuf.length > 0) {
      blocks.push({ type: 'paragraph', text: paraBuf.join(' ') })
      paraBuf = []
    }
  }
  const flushList = () => {
    if (listBuf.length > 0) {
      blocks.push({ type: 'list', items: nestList(listBuf) })
      listBuf = []
    }
  }

  for (const raw of lines) {
    const line = raw
    const trimmed = line.trim()
    if (trimmed === '') {
      flushPara()
      flushList()
      continue
    }
    const h = /^(#{1,6})\s+(.*)$/.exec(trimmed)
    if (h) {
      flushPara()
      flushList()
      blocks.push({ type: 'heading', level: h[1].length, text: h[2] })
      continue
    }
    // List item: capture leading spaces to compute indent depth (2 spaces = 1 level).
    const li = /^(\s*)[-*]\s+(.*)$/.exec(line)
    if (li) {
      flushPara()
      const indent = Math.floor(li[1].length / 2)
      listBuf.push({ indent, text: li[2] })
      continue
    }
    flushList()
    paraBuf.push(trimmed)
  }
  flushPara()
  flushList()
  return blocks
}

// nestList walks a flat list of (indent, text) and turns it into a tree
// using indent depth. Items with smaller-or-equal indent close prior
// sub-branches. Behaviour mirrors typical Markdown parsers.
function nestList(flat: { indent: number; text: string }[]): ListItem[] {
  const root: ListItem[] = []
  const stack: { indent: number; arr: ListItem[] }[] = [{ indent: -1, arr: root }]
  for (const f of flat) {
    while (stack.length > 1 && stack[stack.length - 1].indent >= f.indent) {
      stack.pop()
    }
    const item: ListItem = { indent: f.indent, text: f.text, children: [] }
    stack[stack.length - 1].arr.push(item)
    stack.push({ indent: f.indent, arr: item.children })
  }
  return root
}

// renderInline handles **bold** and `code` only. Anything else is text.
function renderInline(s: string): ReactNode {
  const out: ReactNode[] = []
  const re = /(\*\*([^*]+)\*\*)|(`([^`]+)`)/g
  let last = 0
  let m: RegExpExecArray | null
  let i = 0
  while ((m = re.exec(s)) !== null) {
    if (m.index > last) out.push(<Fragment key={i++}>{s.slice(last, m.index)}</Fragment>)
    if (m[2] !== undefined) {
      out.push(<strong key={i++}>{m[2]}</strong>)
    } else if (m[4] !== undefined) {
      out.push(<code key={i++}>{m[4]}</code>)
    }
    last = m.index + m[0].length
  }
  if (last < s.length) out.push(<Fragment key={i++}>{s.slice(last)}</Fragment>)
  return <>{out}</>
}

function renderList(items: ListItem[], keyPrefix = 'l'): ReactNode {
  return (
    <ul>
      {items.map((it, i) => (
        <li key={`${keyPrefix}-${i}`}>
          {renderInline(it.text)}
          {it.children.length > 0 && renderList(it.children, `${keyPrefix}-${i}`)}
        </li>
      ))}
    </ul>
  )
}

export default function Markdown({ source }: { source: string }) {
  const blocks = parseBlocks(source)
  return (
    <div className="markdown">
      {blocks.map((b, i) => {
        if (b.type === 'heading') {
          const H = (`h${Math.min(Math.max(b.level, 1), 4)}` as unknown) as 'h1'
          return <H key={i}>{renderInline(b.text)}</H>
        }
        if (b.type === 'paragraph') {
          return <p key={i}>{renderInline(b.text)}</p>
        }
        if (b.type === 'list') {
          return <Fragment key={i}>{renderList(b.items, `b${i}`)}</Fragment>
        }
        return null
      })}
    </div>
  )
}
