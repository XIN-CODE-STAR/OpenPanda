// SPDX-License-Identifier: AGPL-3.0-or-later

import { useEffect, useRef, useState } from 'preact/hooks'
import type { NodeInfo } from '../api/client'
import { isNodeConnected } from '../format/node-status'
import { t } from '../i18n'
import { layerAboveOwnsEscape } from './card-dismiss'
import { Icon } from './icons'

/** Where this turn runs, as a button that opens a card.
 *
 *  It used to be a `<select>` with the choice spelled out beside it. The
 *  problem was not the control but the sentence: "自动（最佳节点）" is a
 *  15-character label sitting in the header for a decision you make rarely,
 *  and a native select cannot show what the choice *means* — which machine is
 *  local, which is offline, which one the scheduler would pick. So the header
 *  keeps a single glyph (the network mark) and the card behind it carries the
 *  list.
 *
 *  The card is the roster's and the directory pick's twin: a right-edge sheet
 *  with its own scrim, the same width band, the same head and the same row
 *  rhythm (`node-dialog` in chat.css). The three glyphs stand in one row, so
 *  the three cards must not disagree about what a pick looks like — this one
 *  used to be a popover hanging under the bar, which made the third pick the
 *  only one that behaved like a menu. */
export function NodePicker(props: {
  nodes: NodeInfo[]
  /** Empty string = let the scheduler route by capability. */
  value: string
  onPick(v: string): void
  /** Fired when the card opens or closes, so the pane can make room. */
  onOpenChange?(open: boolean): void
  /** Controlled open state. The pane owns it because the node card and the
   *  orchestration card hang off the same corner and must not both be up;
   *  omitted, the card manages its own like it always did. */
  open?: boolean
  /** Render only the list, as a page inside the side dock. No trigger button
   *  and no floating card: the dock head owns the button and its body owns the
   *  surface. Omitted, this stays the header card workspace.tsx uses. */
  inline?: boolean
}) {
  const [selfOpen, setSelfOpen] = useState(false)
  const open = props.open ?? selfOpen
  const dialog = useRef<HTMLDialogElement>(null)
  const setOpen = (v: boolean) => {
    if (props.open === undefined) setSelfOpen(v)
    props.onOpenChange?.(v)
  }
  const picked = props.nodes.find((n) => n.id === props.value)
  const current = picked ? picked.name || picked.id : t('sessions.nodeAuto')

  // The card is a modal, the same way the roster and the directory pick are:
  // it takes its own layer with a scrim behind it, so a click outside is a
  // click on the scrim (not on the composer underneath), and Escape is the
  // element's own cancel. The inline form is a dock page with no element to
  // cancel, so it keeps the listener below.
  useEffect(() => {
    if (props.inline) return
    const el = dialog.current
    if (!el) return
    if (open && !el.open) {
      el.showModal()
      // `showModal` focuses the first control it finds — the corner cross —
      // and the ring around it on open reads as "the card is asking me to
      // close it". The roster's sheet lands on the dialog itself (its body is
      // rendered a frame later), so this one does the same on purpose.
      el.focus()
    } else if (!open && el.open) el.close()
  }, [open, props.inline])

  // Escape closes the dock page, and it stops here so it cannot also reach the
  // thread drawer behind it. The header card is a native dialog and does not
  // need this — its cancel event is the element's own.
  useEffect(() => {
    if (!open || !props.inline) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape' || layerAboveOwnsEscape()) return
      e.stopPropagation()
      setOpen(false)
    }
    document.addEventListener('keydown', onKey, true)
    return () => {
      document.removeEventListener('keydown', onKey, true)
    }
  }, [open, props.inline])

  const pick = (id: string) => {
    props.onPick(id)
    setOpen(false)
  }

  // One list for both forms: the sheet's body and the dock's node page differ
  // in their head and their frame, never in the rows.
  const list = (
    <div class="node-panel-list">
      <button
        type="button"
        class={`node-row${props.value === '' ? ' on' : ''}`}
        aria-pressed={props.value === ''}
        onClick={() => pick('')}
      >
        <Icon name="sparkles" size={16} class="node-row-ico" />
        <span class="node-row-name">{t('sessions.nodeAuto')}</span>
        {props.value === '' && <Icon name="check" size={14} class="node-row-check" />}
      </button>
      {props.nodes.map((n) => {
        const offline = !isNodeConnected(n.status)
        return (
          <button
            key={n.id}
            type="button"
            class={`node-row${props.value === n.id ? ' on' : ''}${offline ? ' off' : ''}`}
            aria-pressed={props.value === n.id}
            disabled={offline}
            onClick={() => pick(n.id)}
          >
            <Icon name="cpu" size={16} class="node-row-ico" />
            <span class="node-row-name">{n.name || n.id}</span>
            {(n.capacity?.execution_protocol ?? 0) < 1 && <span class="node-row-tag">{t('activity.summaryOnly')}</span>}
            {n.is_local && <span class="node-row-tag">{t('nodes.self')}</span>}
            {offline && <span class="node-row-tag">{t('sessions.nodeOffline')}</span>}
            {props.value === n.id && <Icon name="check" size={14} class="node-row-check" />}
          </button>
        )
      })}
      {props.nodes.length === 0 && <p class="node-panel-empty">{t('nodes.empty')}</p>}
    </div>
  )

  return (
    <div class="node-picker">
      {!props.inline && (
        <button
          type="button"
          class={`icon-btn tip-down node-pick-btn${open ? ' open' : ''}${props.value ? ' pinned' : ''}`}
          /* The tip is the control's name, not the current choice: the choice is
             already spelled out by the card behind it, and a header tooltip that
             changes with the selection reads as a status line rather than a
             label. The selection is still announced to assistive tech below. */
          data-tip={open ? undefined : t('sessions.nodeBtn')}
          aria-label={`${t('sessions.nodePick')}: ${current}`}
          aria-haspopup="dialog"
          aria-expanded={open}
          onClick={() => setOpen(!open)}
        >
          <Icon name="git-fork" size={19} />
        </button>
      )}
      {props.inline ? (
        open && (
          <div class="node-panel is-inline" role="dialog" aria-label={t('sessions.nodePick')}>
            <p class="node-panel-label">{t('sessions.nodePanelTitle')}</p>
            {list}
            <p class="node-panel-hint">{t('sessions.nodePickTip')}</p>
          </div>
        )
      ) : (
        <dialog
          ref={dialog}
          class="node-dialog"
          aria-label={t('sessions.nodePanelTitle')}
          onClose={() => setOpen(false)}
          onClick={(e) => {
            if (e.target === e.currentTarget) setOpen(false)
          }}
        >
          {open && (
            <div class="node-panel">
              {/* The roster's head: the card's name in card-title type, the one
                  way out in the corner every card in this console puts it. */}
              <div class="node-panel-head">
                <p class="node-panel-title">{t('sessions.nodePanelTitle')}</p>
                <button type="button" class="icon-btn" aria-label={t('common.close')} onClick={() => setOpen(false)}>
                  <Icon name="x" size={16} />
                </button>
              </div>
              {list}
              <p class="node-panel-hint">{t('sessions.nodePickTip')}</p>
            </div>
          )}
        </dialog>
      )}
    </div>
  )
}
