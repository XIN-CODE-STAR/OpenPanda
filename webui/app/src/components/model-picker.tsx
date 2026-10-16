// SPDX-License-Identifier: AGPL-3.0-or-later

import { useEffect, useRef, useState } from 'preact/hooks'
import { t } from '../i18n'
import { Icon } from './icons'

/** A model-id field with the fetched list built into it — one control, not two.
 *
 *  The input owns the value, so typing a model the provider does not list
 *  still works; a caret inside the field opens the ids the provider just
 *  returned. This is the affordance `<datalist>` was reaching for, except the
 *  browser filters a datalist by whatever the input already holds — so a
 *  prefilled catalogue default left the list empty and the caret dead. Here
 *  the list is ours: it always shows every fetched id, and a pick writes
 *  straight back into the field. */
export function ModelPicker(props: {
  value: string
  onValue(v: string): void
  options: string[]
  placeholder?: string
  required?: boolean
  id?: string
  disabled?: boolean
}) {
  const [open, setOpen] = useState(false)
  const box = useRef<HTMLDivElement>(null)
  const hasOptions = props.options.length > 0

  // Close on an outside click, and on Escape without letting the dialog's own
  // Escape handler see it — one Escape closes the list, the next the modal.
  useEffect(() => {
    if (!open) return
    const onDown = (e: MouseEvent) => {
      if (box.current && !box.current.contains(e.target as Node)) setOpen(false)
    }
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return
      e.stopPropagation()
      setOpen(false)
    }
    document.addEventListener('mousedown', onDown)
    document.addEventListener('keydown', onKey, true)
    return () => {
      document.removeEventListener('mousedown', onDown)
      document.removeEventListener('keydown', onKey, true)
    }
  }, [open])

  return (
    <div class="model-picker" ref={box}>
      <input
        id={props.id}
        class="input mono"
        type="text"
        value={props.value}
        placeholder={props.placeholder}
        required={props.required}
        disabled={props.disabled}
        onInput={(e) => props.onValue((e.target as HTMLInputElement).value)}
      />
      {hasOptions && (
        <button
          type="button"
          class="model-picker-toggle"
          aria-label={t('models.pickFromList')}
          title={t('models.pickFromList')}
          aria-haspopup="menu"
          aria-expanded={open}
          disabled={props.disabled}
          onClick={() => setOpen((v) => !v)}
        >
          <Icon name="chevron-down" size={14} />
        </button>
      )}
      {open && hasOptions && (
        <div class="model-picker-list" role="menu">
          {props.options.map((id) => (
            <button
              key={id}
              type="button"
              role="menuitem"
              class={`model-picker-item${id === props.value ? ' on' : ''}`}
              onClick={() => {
                props.onValue(id)
                setOpen(false)
              }}
            >
              {id}
            </button>
          ))}
        </div>
      )}
    </div>
  )
}
