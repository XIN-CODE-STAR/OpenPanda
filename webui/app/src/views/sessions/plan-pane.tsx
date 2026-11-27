// SPDX-License-Identifier: AGPL-3.0-or-later

import { useEffect, useRef, useState } from 'preact/hooks'
import { t } from '../../i18n'
import { Markdown } from '../../md/render'

export function PlanPane({ version, text, resumesGoal, busy, confirming, note, onNote, onAdjust, onConfirm, onClose }: {
  version: number
  text: string
  resumesGoal: boolean
  busy: boolean
  confirming: boolean
  note: string
  onNote(note: string): void
  /** Send the note back to the model as the next planning turn. */
  onAdjust(note: string): Promise<boolean>
  onConfirm(): Promise<void>
  onClose(): void
}) {
  const body = useRef<HTMLDivElement>(null)
  const editor = useRef<HTMLTextAreaElement>(null)
  const adjustButton = useRef<HTMLButtonElement>(null)
  const [editing, setEditing] = useState(false)
  const sendingRef = useRef(false)
  const [sending, setSending] = useState(false)
  const trimmed = note.trim()

  useEffect(() => { body.current?.focus({ preventScroll: true }) }, [])
  useEffect(() => { if (editing) editor.current?.focus() }, [editing])

  async function submitAdjustment() {
    if (!trimmed || busy || confirming || sendingRef.current) return
    sendingRef.current = true
    setSending(true)
    try {
      if (await onAdjust(trimmed)) closeEditor()
    } finally {
      sendingRef.current = false
      setSending(false)
    }
  }

  function closeEditor() {
    onNote('')
    setEditing(false)
  }

  return (
    <section id="plan-panel" class="plan-pane" aria-labelledby="plan-pane-title">
      <header class="plan-pane-head">
        <span id="plan-pane-title" class="plan-pane-title">{t('sessions.plan.proposal')} · {version}</span>
        <button type="button" class="btn ghost plan-pane-close" onClick={onClose}>
          {t('common.close')}
        </button>
      </header>

      <div ref={body} class="plan-pane-body" tabIndex={0}>
        <Markdown text={text} />
      </div>

      <footer class="plan-pane-foot" aria-busy={sending || confirming}>
        {editing && <>
          <p id="plan-revision-hint" class="plan-pane-hint">{t('sessions.plan.decideHint')}</p>
          <textarea
            ref={editor}
            class="plan-pane-note"
            rows={3}
            value={note}
            disabled={sending || confirming}
            placeholder={t('sessions.plan.adjustPlaceholder')}
            aria-label={t('sessions.plan.requestChanges')}
            aria-describedby="plan-revision-hint"
            onInput={(event) => onNote(event.currentTarget.value)}
          />
        </>}
        <div class="plan-pane-actions">
          {editing ? <>
            <button type="button" class="btn ghost" disabled={sending || confirming} onClick={() => {
              closeEditor()
              // The opener is mounted again after the editor is removed.
              requestAnimationFrame(() => adjustButton.current?.focus())
            }}>{t('common.cancel')}</button>
            <button type="button" class="btn primary" disabled={!trimmed || busy || sending || confirming} onClick={() => void submitAdjustment()}>{t('sessions.plan.adjust')}</button>
          </> : <>
            <button ref={adjustButton} type="button" class="btn" disabled={busy || sending || confirming} onClick={() => setEditing(true)}>
              {t('sessions.plan.requestChanges')}
            </button>
            <button type="button" class="btn primary" disabled={busy || sending || confirming} onClick={() => void onConfirm()}>
              {t(resumesGoal ? 'sessions.plan.implementGoal' : 'sessions.plan.implement')}
            </button>
          </>}
        </div>
      </footer>
    </section>
  )
}
