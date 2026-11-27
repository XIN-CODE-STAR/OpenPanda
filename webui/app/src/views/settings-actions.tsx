// SPDX-License-Identifier: AGPL-3.0-or-later

import { useEffect, useRef, useState } from 'preact/hooks'
import { t } from '../i18n'

/** What a section hands the sheet's one action bar. The section keeps its own
 *  form state and its own PUT; it only *publishes* the two verbs, so the bar
 *  never has to know what the section is editing. */
export interface SettingsActionState {
  dirty: boolean
  saving: boolean
  /** A blocking validation problem: it takes the place of 未保存 and keeps
   *  保存 dead even though the form differs from the node. */
  invalid?: string
  error?: string
  notice?: string
  /** What 保存 says while the save is in flight, for a section whose save
   *  takes long enough to explain itself (MCP spawns the server to validate).
   *  Falls back to 保存…. */
  busyLabel?: string
  save(): void
  discard(): void
}

interface Entry {
  owner: object
  state: SettingsActionState
}

/** One bar for the whole sheet. Sections publish into this store when they
 *  mount and on every render after that; the bar subscribes. Sections with
 *  nothing to save (语言、主题、快捷键… apply on the spot) publish nothing and
 *  the bar simply shows its two dead verbs. */
export interface SettingsActionsStore {
  read(): SettingsActionState | null
  write(owner: object, state: SettingsActionState): void
  clear(owner: object): void
  subscribe(fn: () => void): () => void
}

export function createSettingsActions(): SettingsActionsStore {
  let entry: Entry | null = null
  const listeners = new Set<() => void>()
  const notify = () => {
    for (const fn of [...listeners]) fn()
  }
  return {
    read: () => entry?.state ?? null,
    write(owner, state) {
      entry = { owner, state }
      notify()
    },
    clear(owner) {
      // A section that unmounts *after* its successor has registered must not
      // take the live state with it: only the owner may clear its own entry.
      if (entry?.owner !== owner) return
      entry = null
      notify()
    },
    subscribe(fn) {
      listeners.add(fn)
      return () => {
        listeners.delete(fn)
      }
    },
  }
}

/** Publish this render's state. Deliberately no dependency list: the state
 *  object is rebuilt every render so the bar always calls the newest
 *  save/discard closure — a cached one would PUT the form as it looked two
 *  keystrokes ago. Writing is cheap; the bar is two buttons and a line. */
export function useSettingsActions(store: SettingsActionsStore, state: SettingsActionState) {
  const owner = useRef<object>({})
  useEffect(() => {
    store.write(owner.current, state)
  })
  useEffect(() => () => store.clear(owner.current), [store])
}

/** The sheet's footer. It is part of the chrome, not of any one section: every
 *  tab gets the same two verbs in the same place, and a tab with nothing to
 *  save leaves them disabled instead of making the bar appear and disappear
 *  under the reader's cursor. A tab with nothing to save says nothing — the
 *  two dead verbs already do. */
export function SettingsActionBar({ store }: { store: SettingsActionsStore }) {
  const [state, setState] = useState<SettingsActionState | null>(() => store.read())

  useEffect(() => {
    setState(store.read())
    return store.subscribe(() => setState(store.read()))
  }, [store])

  const dirty = state?.dirty ?? false
  const saving = state?.saving ?? false
  const invalid = state?.invalid ?? ''

  return (
    <div class="policy-actions settings-save-bar">
      {state?.error && (
        <p class="gate-error" role="alert">
          {state.error}
        </p>
      )}
      {state?.notice && !dirty && (
        <p class="hint" role="status">
          {state.notice}
        </p>
      )}
      <div class="policy-actions-bar">
        {/* The live region stays mounted: a screen reader announces changes
            inside it, and a node inserted with its first text in the same
            frame is often not announced at all. */}
        <span class={`policy-actions-state${invalid ? ' bad' : dirty ? ' dirty' : ''}`} role="status">
          {(dirty || invalid) && (
            <>
              <span class="policy-actions-dot" aria-hidden="true" />
              {invalid || t('policy.unsaved')}
            </>
          )}
        </span>
        <button
          type="button"
          class="btn ghost"
          disabled={!dirty || saving}
          onClick={() => store.read()?.discard()}
        >
          {t('policy.discard')}
        </button>
        <button
          type="button"
          class="btn primary"
          disabled={!dirty || saving || !!invalid}
          onClick={() => store.read()?.save()}
        >
          {saving ? state?.busyLabel || `${t('common.save')}…` : t('common.save')}
        </button>
      </div>
    </div>
  )
}
