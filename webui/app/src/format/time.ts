// SPDX-License-Identifier: AGPL-3.0-or-later

import { t } from '../i18n/index.ts'

/** Relative timestamps for list rows ("3 分钟前", "昨天").
 *
 *  Intl rather than an i18n key set: a relative-time ladder is a locale
 *  *format*, not a string — CLDR already carries the plural rules, and
 *  `numeric: 'auto'` yields the idiomatic word ("昨天" / "yesterday") where a
 *  hand-rolled key set would say "1 天前". Falls back to a short date when the
 *  stamp is unparseable, and to an empty string when the runtime has no
 *  RelativeTimeFormat (the caller renders nothing rather than a raw ISO). */

const MINUTE = 60_000
const HOUR = 60 * MINUTE
const DAY = 24 * HOUR

type Unit = 'second' | 'minute' | 'hour' | 'day'

export function relativeTime(iso: string, locale: string, now: number = Date.now()): string {
  const at = Date.parse(iso)
  if (!Number.isFinite(at)) return ''
  const age = now - at
  // A future stamp (clock skew between nodes) reads as "just now" rather than
  // "-1 分钟前".
  if (age < MINUTE) return spell(0, 'second', locale)
  if (age < HOUR) return spell(-Math.floor(age / MINUTE), 'minute', locale)
  if (age < DAY) return spell(-Math.floor(age / HOUR), 'hour', locale)
  // Past a week the ladder stops being useful — an absolute date is what a
  // reader wants for anything older.
  if (age < 7 * DAY) return spell(-Math.floor(age / DAY), 'day', locale)
  try {
    return new Date(at).toLocaleDateString(locale)
  } catch {
    return ''
  }
}

function spell(value: number, unit: Unit, locale: string): string {
  try {
    return new Intl.RelativeTimeFormat(locale, { numeric: 'auto' }).format(value, unit)
  } catch {
    return ''
  }
}

/** The wall clock on one turn ("08:31"), from its Unix-seconds stamp.
 *
 *  Hour and minute only: the question already sits in its thread, so the day
 *  is context rather than content, and seconds would be noise on a message
 *  the user wrote once. The locale decides 12- or 24-hour form, the same way
 *  every other clock string in the console does — a hand-rolled "HH:mm" would
 *  read as 24-hour time to a reader whose locale never shows it.
 *
 *  Returns an empty string for a missing stamp (transcripts written before the
 *  store filed one), so the caller draws nothing instead of a placeholder. */
export function clockTime(ts: number | undefined, locale: string): string {
  const at = stampDate(ts)
  if (!at) return ''
  try {
    return new Intl.DateTimeFormat(locale, { hour: '2-digit', minute: '2-digit' }).format(at)
  } catch {
    return ''
  }
}

/** The same stamp spelled out for a tooltip ("2026年10月20日 08:31"), so a
 *  thread that spans days is not read as one morning. Empty when there is no
 *  stamp, matching `clockTime`. */
export function clockStamp(ts: number | undefined, locale: string): string {
  const at = stampDate(ts)
  if (!at) return ''
  try {
    return new Intl.DateTimeFormat(locale, { dateStyle: 'medium', timeStyle: 'short' }).format(at)
  } catch {
    return ''
  }
}

/** Unix seconds → Date, or null when the stamp is absent/unusable. A turn
 *  stored without one (older transcripts) must not print 1970. */
function stampDate(ts: number | undefined): Date | null {
  if (!ts || ts <= 0) return null
  const at = new Date(ts * 1000)
  return Number.isFinite(at.getTime()) ? at : null
}

/** "5d 3h" / "2h 05m" / "12m" / "45s" — compact human duration, units
 *  localized so CJK locales read 天/小时/分钟/秒 instead of latin letters.
 *  Whole days drop the zero hour ("3d", not "3d 0h"). */
export function formatDuration(ms: number): string {
  const s = Math.max(1, Math.round(ms / 1000))
  const d = Math.floor(s / 86400)
  const h = Math.floor((s % 86400) / 3600)
  const m = Math.floor((s % 3600) / 60)
  const sec = s % 60
  if (d > 0) return h > 0 ? `${d}${t('time.unit.d')} ${h}${t('time.unit.h')}` : `${d}${t('time.unit.d')}`
  if (h > 0) return `${h}${t('time.unit.h')} ${String(m).padStart(2, '0')}${t('time.unit.m')}`
  if (m > 0) return `${m}${t('time.unit.m')}`
  return `${sec}${t('time.unit.s')}`
}
