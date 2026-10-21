// SPDX-License-Identifier: AGPL-3.0-or-later

// Inline image preview for the transcript. A tool row that touched a picture
// — a `Read` of a screenshot, a capture a browser step dropped on disk — used
// to show nothing but its path. The path stays; the picture now sits under it,
// one click from full screen.
//
// Bytes move through fetchBlob: an <img src> cannot carry the Bearer token, so
// the console fetches /api/fs/image itself and hands the <img> an object URL.

import { useEffect, useRef, useState } from 'preact/hooks'
import { fetchBlob } from '../api/client'
import { t } from '../i18n'
import { Icon } from './icons'
import { useModalFocus } from './modal-focus'

function baseName(path: string): string {
  const i = Math.max(path.lastIndexOf('/'), path.lastIndexOf('\\'))
  return i >= 0 ? path.slice(i + 1) : path
}

export interface ImagePreviewProps {
  /** Filesystem path served by /api/fs/image. */
  path?: string
  /** Bytes the adapter already inlined — shown without a round trip. */
  dataUrl?: string
  className?: string
}

/** A bounded thumbnail plus the lightbox it opens. Rendering the picture is
 *  best-effort by design: a path outside the readable roots (or a file the
 *  agent already deleted) leaves one dim line, never a broken image icon. */
export function ImagePreview({ path, dataUrl, className = '' }: ImagePreviewProps) {
  const [url, setUrl] = useState(dataUrl ?? '')
  const [failed, setFailed] = useState(false)
  const [zoom, setZoom] = useState(false)
  const [near, setNear] = useState(false)
  const holder = useRef<HTMLDivElement>(null)

  // A computer-use run reads dozens of screenshots, and "expand all" on that
  // log used to fire every fetch at once. The bytes are only worth asking for
  // once the thumbnail is about to be seen.
  useEffect(() => {
    if (dataUrl) return
    const el = holder.current
    if (!el || typeof IntersectionObserver === 'undefined') {
      setNear(true)
      return
    }
    const io = new IntersectionObserver(
      (entries) => {
        if (entries.some((e) => e.isIntersecting)) {
          setNear(true)
          io.disconnect()
        }
      },
      { rootMargin: '300px 0px' },
    )
    io.observe(el)
    return () => io.disconnect()
  }, [dataUrl, path])

  useEffect(() => {
    if (dataUrl) {
      setUrl(dataUrl)
      setFailed(false)
      return
    }
    if (!path || !near) return
    let alive = true
    let made = ''
    setUrl('')
    setFailed(false)
    fetchBlob(`/api/fs/image?path=${encodeURIComponent(path)}`)
      .then((blob) => {
        made = URL.createObjectURL(blob)
        if (!alive) {
          URL.revokeObjectURL(made)
          return
        }
        setUrl(made)
      })
      .catch(() => {
        if (alive) setFailed(true)
      })
    return () => {
      alive = false
      if (made) URL.revokeObjectURL(made)
    }
  }, [path, dataUrl, near])

  if (failed) {
    return <div class={`img-miss dim ${className}`}>{t('events.imageUnavailable')}</div>
  }
  if (!url) {
    return <div class={`img-skel ${className}`} aria-hidden="true" ref={holder} />
  }

  const name = path ? baseName(path) : 'image'
  return (
    <div class={`img-preview ${className}`}>
      <button
        type="button"
        class="img-preview-btn"
        onClick={() => setZoom(true)}
        title={t('events.imageOpen')}
        aria-label={`${t('events.imageOpen')}: ${name}`}
      >
        <img src={url} alt={name} />
        <span class="img-preview-hint" aria-hidden="true">
          <Icon name="search" size={13} />
        </span>
      </button>
      {path && <span class="img-preview-name">{name}</span>}
      {zoom && <Lightbox src={url} path={path} onClose={() => setZoom(false)} />}
    </div>
  )
}

/** Full-screen view. Rendered inline with position:fixed — the same overlay
 *  convention the confirm dialog and the todo sheet use — and it closes on
 *  Esc, on the × button, and on a click outside the picture. */
function Lightbox({ src, path, onClose }: { src: string; path?: string; onClose: () => void }) {
  const box = useRef<HTMLDivElement>(null)
  useModalFocus(box, true)

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return
      // Capture phase: the chat's own Esc handler must not also fire.
      e.stopPropagation()
      onClose()
    }
    document.addEventListener('keydown', onKey, true)
    return () => document.removeEventListener('keydown', onKey, true)
  }, [onClose])

  const name = path ? baseName(path) : 'image'
  return (
    <div
      class="img-lightbox"
      role="dialog"
      aria-modal="true"
      aria-label={name}
      ref={box}
      onClick={onClose}
    >
      <img class="img-lightbox-img" src={src} alt={name} onClick={(e) => e.stopPropagation()} />
      <div class="img-lightbox-bar" onClick={(e) => e.stopPropagation()}>
        {path && (
          <span class="img-lightbox-path" title={path}>
            {path}
          </span>
        )}
        <button
          type="button"
          class="img-lightbox-close"
          onClick={onClose}
          title={t('common.close')}
          aria-label={t('common.close')}
        >
          <Icon name="x" size={16} />
        </button>
      </div>
    </div>
  )
}
