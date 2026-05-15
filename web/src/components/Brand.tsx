import { Link } from 'react-router-dom'

export default function Brand() {
  return (
    <Link to="/" className="brand" style={{ textDecoration: 'none', color: 'inherit' }}>
      <div className="brand-mark" aria-hidden>S</div>
      <div>
        <div className="brand-name">Scribe Web</div>
        <div className="brand-sub">YouTube / B 站 视频转文字</div>
      </div>
    </Link>
  )
}
