// SPDX-License-Identifier: AGPL-3.0-or-later

import { useEffect, useRef, useState } from 'preact/hooks'
import { api, type Reminder } from '../api/client'
import { useAsync, useChangeSignal, useLocaleRerender } from '../hooks'
import { t } from '../i18n'
import { Icon } from './icons'
import { toastError } from './toast'
import { confirmDialog } from './confirm'
import { layerAboveOwnsEscape } from './card-dismiss'

/** The reminder system's one door.
 *
 *  提醒 used to be a tab of 设置, which buried a thing you touch several times
 *  a day behind a page whose other tabs you open once a month. It is a bell in
 *  the rail's head now, beside 搜索 — the two things you reach for from
 *  wherever you are — and the bell opens a card instead of navigating: you look
 *  at what is pending, add one, close. The card carries the whole feature
 *  (relative or absolute due time, repeat, the live pending queue, what just
 *  fired, the browser-push switch), so the settings tab could go without
 *  leaving anything behind.
 *
 *  A card, not a page, is also why it dismisses on an outside click: the header
 *  cards in the chat pane are read *while* the thread is used (see
 *  card-dismiss), but nobody reads a reminder list while doing something else. */

/** Raise the card from anywhere — the ⌘K palette, or a `#/reminders` bookmark
 *  saved before the tab went away. No-op until the bell has mounted, which is
 *  the same contract openPalette keeps. */
let raise: (() => void) | null = null
export function openReminders(): void {
  raise?.()
}

/** A reminder is pending while it has not fired yet, or while it repeats: a
 *  recurring row keeps its LAST fire in `fired_at`, so a non-zero stamp is not
 *  a retirement. */
function isPending(r: Reminder): boolean {
  return r.fired_at === 0 || r.repeat_seconds > 0
}

export function ReminderBell() {
  useLocaleRerender()
  const [open, setOpen] = useState(false)
  const [reload, setReload] = useState(0)
  // Countdowns are painted from a clock, so they go stale while the card sits
  // open. A 30s beat re-renders them without re-fetching the list.
  const [now, setNow] = useState(() => Date.now())
  const root = useRef<HTMLDivElement>(null)
  const change = useChangeSignal()
  const { data, error } = useAsync(() => api.reminders(), [], change + reload)
  const pending = (data ?? []).filter(isPending)

  useEffect(() => {
    raise = () => setOpen(true)
    return () => {
      raise = null
    }
  }, [])

  useEffect(() => {
    if (!open) return
    setNow(Date.now())
    const id = setInterval(() => setNow(Date.now()), 30_000)
    return () => clearInterval(id)
  }, [open])

  // Escape closes the card, and stops there so it cannot also reach the thread
  // drawer behind it. A modal (the delete confirm) owns Escape while it is up,
  // and an outside click on that modal must not tear the card down either.
  useEffect(() => {
    if (!open) return
    const onDown = (e: PointerEvent) => {
      if (root.current?.contains(e.target as Node) || layerAboveOwnsEscape()) return
      setOpen(false)
    }
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape' || layerAboveOwnsEscape()) return
      e.stopPropagation()
      setOpen(false)
    }
    document.addEventListener('pointerdown', onDown, true)
    document.addEventListener('keydown', onKey, true)
    return () => {
      document.removeEventListener('pointerdown', onDown, true)
      document.removeEventListener('keydown', onKey, true)
    }
  }, [open])

  const label = pending.length
    ? `${t('nav.reminders')} (${pending.length})`
    : t('nav.reminders')

  return (
    <div class="reminder-bell" ref={root}>
      <button
        type="button"
        class={`icon-btn tip-down${open ? ' open' : ''}`}
        data-tip={open ? undefined : t('nav.reminders')}
        aria-label={label}
        aria-haspopup="dialog"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        <Icon name="bell" />
        {pending.length > 0 && (
          <span class="reminder-bell-dot" aria-hidden="true">
            {pending.length > 9 ? '9+' : pending.length}
          </span>
        )}
      </button>
      {open && (
        <div class="reminder-pop" role="dialog" aria-label={t('reminders.title')}>
          <RemindersCard
            reminders={data}
            error={error}
            now={now}
            onRetry={() => setReload((v) => v + 1)}
            onClose={() => setOpen(false)}
          />
        </div>
      )}
    </div>
  )
}

/** The card's body: compose, then the pending queue, then what just fired. */
function RemindersCard(props: {
  reminders: Reminder[] | null
  error: string | null
  now: number
  onRetry(): void
  onClose(): void
}) {
  const [message, setMessage] = useState('')
  const [minutes, setMinutes] = useState('')
  const [dueAt, setDueAt] = useState('')
  const [everyMin, setEveryMin] = useState('')
  const [adding, setAdding] = useState(false)
  const [notify, setNotify] = useState<'idle' | 'on' | 'unsupported' | 'denied' | 'error'>('idle')

  // The switch is a statement about *this* browser, so it starts from what the
  // browser says rather than from an idle "enable it" button: a live push
  // subscription means the line is already on, and re-offering it would be a
  // lie the first click would expose.
  useEffect(() => {
    if (!('serviceWorker' in navigator) || !('PushManager' in window)) {
      setNotify('unsupported')
      return
    }
    let alive = true
    navigator.serviceWorker.ready
      .then((reg) => reg.pushManager.getSubscription())
      .then((sub) => {
        if (alive && sub) setNotify('on')
      })
      .catch(() => {})
    return () => {
      alive = false
    }
  }, [])

  async function add(e: Event) {
    e.preventDefault()
    if (adding || !message.trim()) return
    setAdding(true)
    try {
      // repeat_seconds rides along whichever due-time form was used.
      const rep = Number(everyMin)
      const repeat_seconds = rep > 0 ? Math.round(rep * 60) : undefined
      if (dueAt) {
        // datetime-local has no timezone; interpret it in the browser's zone.
        await api.createReminder({
          message: message.trim(),
          due_at: new Date(dueAt).toISOString(),
          repeat_seconds,
        })
      } else {
        const n = Number(minutes)
        await api.createReminder({
          message: message.trim(),
          after_minutes: n > 0 ? n : 1,
          repeat_seconds,
        })
      }
      setMessage('')
      setMinutes('')
      setDueAt('')
      setEveryMin('')
    } catch (err) {
      toastError(err)
    } finally {
      setAdding(false)
    }
  }

  async function remove(id: number) {
    // Deleting a pending reminder cancels it for good — confirm first.
    const ok = await confirmDialog({
      title: t('reminders.deleteTitle'),
      message: t('reminders.deleteMsg'),
      confirmLabel: t('reminders.delete'),
    })
    if (!ok) return
    try {
      await api.deleteReminder(id)
    } catch (e) {
      toastError(e)
    }
  }

  /** Ask for notification permission and subscribe this browser to the node's
   *  Web Push service so reminders fire as OS notifications. */
  async function enableNotifications() {
    if (!('serviceWorker' in navigator) || !('PushManager' in window)) {
      setNotify('unsupported')
      return
    }
    try {
      const perm = await Notification.requestPermission()
      if (perm !== 'granted') {
        setNotify('denied')
        return
      }
      const { key } = await api.pushKey()
      const reg = await navigator.serviceWorker.ready
      const sub = await reg.pushManager.subscribe({
        userVisibleOnly: true,
        applicationServerKey: urlBase64ToBytes(key),
      })
      await api.pushSubscribe(sub.toJSON())
      setNotify('on')
    } catch (e) {
      setNotify('error')
      console.warn('push subscribe', e)
    }
  }

  const list = props.reminders ?? []
  const pending = list.filter(isPending)
  const fired = list.filter((r) => !isPending(r))

  return (
    <>
      <div class="reminder-pop-head">
        <span class="reminder-pop-title">{t('reminders.title')}</span>
        <button
          type="button"
          class="icon-btn reminder-pop-close"
          aria-label={t('common.close')}
          onClick={props.onClose}
        >
          <Icon name="x" size={14} />
        </button>
      </div>

      <form class="reminder-form" onSubmit={add}>
        <input
          class="input reminder-message"
          type="text"
          placeholder={t('reminders.messagePlaceholder')}
          value={message}
          onInput={(e) => setMessage((e.target as HTMLInputElement).value)}
          required
        />
        <div class="reminder-line">
          <input
            class="input reminder-minutes"
            type="number"
            min={1}
            step={1}
            placeholder={t('reminders.minutes')}
            title={t('reminders.afterMinutes')}
            value={minutes}
            onInput={(e) => setMinutes((e.target as HTMLInputElement).value)}
            disabled={!!dueAt}
          />
          <span class="dim reminder-or">{t('reminders.or')}</span>
          <input
            class="input reminder-at"
            type="datetime-local"
            title={t('reminders.dueAt')}
            value={dueAt}
            onInput={(e) => setDueAt((e.target as HTMLInputElement).value)}
          />
        </div>
        <div class="reminder-line">
          <input
            class="input reminder-every"
            type="number"
            min={1}
            step={1}
            placeholder={t('reminders.everyMin')}
            title={t('reminders.everyMinTitle')}
            value={everyMin}
            onInput={(e) => setEveryMin((e.target as HTMLInputElement).value)}
          />
          <button class="btn primary reminder-add" type="submit" disabled={adding || !message.trim()}>
            {t('reminders.add')}
          </button>
        </div>
      </form>

      <div class="reminder-pop-scroll">
        {props.error ? (
          <div class="reminder-pop-err">
            <p class="dim">{props.error}</p>
            <button class="btn" type="button" onClick={props.onRetry}>
              {t('common.retry')}
            </button>
          </div>
        ) : (
          <>
            <p class="reminder-pop-label">
              {t('reminders.pending')} ({pending.length})
            </p>
            {pending.length === 0 && <p class="reminder-pop-empty dim">{t('reminders.empty')}</p>}
            {pending.map((r) => (
              <ReminderRow key={r.id} r={r} now={props.now} onDelete={remove} />
            ))}
            {fired.length > 0 && (
              <>
                <p class="reminder-pop-label">{t('reminders.fired')}</p>
                {fired.map((r) => (
                  <ReminderRow key={r.id} r={r} now={props.now} onDelete={remove} />
                ))}
              </>
            )}
          </>
        )}
      </div>

      <div class="reminder-pop-foot">
        {notify === 'idle' && (
          <button type="button" class="link-btn reminder-notify" onClick={enableNotifications}>
            {t('reminders.notify')}
          </button>
        )}
        {notify === 'on' && <span class="dim">{t('reminders.notifyOn')}</span>}
        {notify === 'unsupported' && <span class="dim">{t('reminders.notifyUnsupported')}</span>}
        {notify === 'denied' && <span class="gate-error">{t('reminders.notifyDenied')}</span>}
        {notify === 'error' && <span class="gate-error">{t('reminders.notifyError')}</span>}
      </div>
    </>
  )
}

function ReminderRow({ r, now, onDelete }: { r: Reminder; now: number; onDelete(id: number): void }) {
  const pending = isPending(r)
  const left = r.due_at * 1000 - now
  const when = pending
    ? left <= 0
      ? t('reminders.overdue')
      : t('reminders.dueIn', { d: formatDuration(left) })
    : new Date(r.fired_at * 1000).toLocaleString()
  const repeat =
    r.repeat_seconds > 0
      ? ' · ' + t('reminders.repeats', { d: formatDuration(r.repeat_seconds * 1000) })
      : ''
  return (
    <div class={`reminder-row${pending ? '' : ' fired'}`}>
      <div class="reminder-info">
        <span class="reminder-msg">{r.message}</span>
        <span class="reminder-meta dim">
          {pending ? new Date(r.due_at * 1000).toLocaleString() : t('reminders.firedAt') + ' ' + when}
          {repeat} · {r.source}
        </span>
      </div>
      <div class="reminder-side">
        <span class={`badge ${pending ? (left <= 0 ? 'yellow' : 'green') : 'dim'}`}>
          {pending ? when : '✓'}
        </span>
        <button
          type="button"
          class="icon-btn reminder-x"
          title={t('reminders.delete')}
          aria-label={t('reminders.delete')}
          onClick={() => onDelete(r.id)}
        >
          <Icon name="x" size={13} />
        </button>
      </div>
    </div>
  )
}

/** "5d 3h" / "2h 05m" / "12m" / "45s" — compact human countdown, units
 *  localized so CJK locales read 天/小时/分钟/秒 instead of latin letters. */
function formatDuration(ms: number): string {
  const s = Math.max(1, Math.round(ms / 1000))
  const d = Math.floor(s / 86400)
  const h = Math.floor((s % 86400) / 3600)
  const m = Math.floor((s % 3600) / 60)
  const sec = s % 60
  if (d > 0) return `${d}${t('time.unit.d')} ${h}${t('time.unit.h')}`
  if (h > 0) return `${h}${t('time.unit.h')} ${String(m).padStart(2, '0')}${t('time.unit.m')}`
  if (m > 0) return `${m}${t('time.unit.m')}`
  return `${sec}${t('time.unit.s')}`
}

/** VAPID keys arrive URL-base64 encoded; PushManager wants raw bytes. */
function urlBase64ToBytes(b64: string): Uint8Array<ArrayBuffer> {
  const pad = '='.repeat((4 - (b64.length % 4)) % 4)
  const raw = atob((b64 + pad).replace(/-/g, '+').replace(/_/g, '/'))
  const out = new Uint8Array(new ArrayBuffer(raw.length))
  for (let i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i)
  return out
}
