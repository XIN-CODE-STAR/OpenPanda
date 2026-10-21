// SPDX-License-Identifier: AGPL-3.0-or-later

// The task detail page's sections, as components — so the chat's task card can
// show the same task the same way.
//
// These used to be private to `views/detail.tsx`. The card in the chat flyout
// had grown its own, thinner version of each (a JSON blob for the brief, the
// raw result envelope, no log at all), which meant the same task read
// differently depending on where you opened it — and the card's version was
// always the worse one. One implementation, two hosts: the page lays them out
// at 980px, the card stacks them in 360px, and the rendering rules live here.
//
// Everything below is presentation only: no fetching, no navigation of its
// own. `onOpenTask` is passed in by whichever host has somewhere to go.

import { useState } from 'preact/hooks'
import type { Task, TaskEvent } from '../api/client'
import { locale, t } from '../i18n'
import { EventTimeline } from './event-timeline'
import { Icon } from './icons'

/** Rows of a long log shown before the reader asks for the rest. */
export const TIMELINE_TAIL = 5

/** Parses a result/spec blob into an object, or returns null when it is not
 *  one. Callers treat null as "show the text as-is". */
export function parseResult(raw: string): Record<string, unknown> | null {
  try {
    const v: unknown = JSON.parse(raw)
    return v && typeof v === 'object' && !Array.isArray(v) ? (v as Record<string, unknown>) : null
  } catch {
    return null
  }
}

export function text(v: unknown): string {
  return typeof v === 'string' ? v : ''
}

export function shortId(id: string): string {
  return id ? id.slice(0, 8) : '—'
}

export function fmtFull(iso: string): string {
  return iso ? new Date(iso).toLocaleString(locale()) : '—'
}

/** Compact stamp for the header line: the year is noise on a run that started
 *  today, and the seconds never helped anyone. The locale decides field order
 *  and the 12/24-hour cycle rather than a hand-rolled "MM-DD HH:mm" that
 *  reads as 24-hour time to a reader whose locale never shows it. */
export function fmtShort(iso: string): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '—'
  try {
    return new Intl.DateTimeFormat(locale(), {
      month: 'numeric',
      day: 'numeric',
      hour: 'numeric',
      minute: '2-digit',
    }).format(d)
  } catch {
    return '—'
  }
}

/** The task spec (spec_json) as a readable brief: target as prose,
 *  constraints as a checklist, scope as file chips, success as the bar the
 *  supervisor evaluates. Absent or unparseable specs render nothing —
 *  older tasks simply have no brief.
 *
 *  Folded by default, but never silent: the collapsed state still shows the
 *  first sentence of the target, so the reader always gets "what was this
 *  asked to do" without a click. Open, the brief was 266px of dense prose
 *  sitting between the result and the log.
 *
 *  `fallback` is the intent sentence the user actually typed. A task with no
 *  spec envelope still has one, and a brief section that disappears because
 *  the dispatcher did not fill a JSON field is a worse answer than showing the
 *  sentence that started the run. The detail page leaves it unset (its header
 *  already carries the intent); the chat card passes it. */
export function SpecCard({ spec, fallback }: { spec?: string; fallback?: string }) {
  const [open, setOpen] = useState(false)
  const s = spec ? parseResult(spec) : null
  const target = text(s?.target) || (fallback ?? '').trim()
  const success = text(s?.success_definition)
  const scope = text(s?.scope)
  const constraints = Array.isArray(s?.constraints)
    ? s.constraints.filter((c): c is string => typeof c === 'string' && c !== '')
    : []
  if (!target && !success && !scope && constraints.length === 0) return null
  const scopeFiles = scope
    .split(/[,;\n]/)
    .map((f) => f.trim())
    .filter(Boolean)
  // A brief that is only a target has nothing to unfold — the toggle used to
  // sit there re-printing the sentence directly above it.
  const hasMore = constraints.length > 0 || scopeFiles.length > 0 || Boolean(success)
  return (
    <section class="dt-sec">
      <div class="dt-sec-head">
        <h2 class="dt-sec-title">{t('detail.spec.title')}</h2>
        {constraints.length > 0 && (
          <span class="dt-sec-hint">
            {t('detail.spec.constraints', { n: String(constraints.length) })}
          </span>
        )}
        {hasMore && (
          <button type="button" class="dt-sec-action" onClick={() => setOpen((v) => !v)}>
            {open ? t('events.collapse') : t('detail.spec.expand')}
          </button>
        )}
      </div>

      {(!open || !hasMore) && target && <p class="dt-spec-preview">{target}</p>}

      {open && hasMore && (
        <>
          {target && <p class="dt-spec-target">{target}</p>}
          {constraints.length > 0 && (
            <ul class="dt-spec-list">
              {constraints.map((c, i) => (
                <li key={i}>
                  {/* A dot, not a check: these are guardrails the run was
                      held to, not items it ticked off. */}
                  <span class="dt-spec-dot" aria-hidden="true" />
                  <span>{c}</span>
                </li>
              ))}
            </ul>
          )}
          {(scopeFiles.length > 0 || success) && (
            <dl class="dt-spec-foot">
              {scopeFiles.length > 0 && (
                <>
                  <dt>{t('detail.spec.scope')}</dt>
                  <dd>
                    <span class="dt-spec-files">
                      {scopeFiles.map((f, i) => (
                        <code key={i} class="result-file-chip" title={f}>
                          {f}
                        </code>
                      ))}
                    </span>
                  </dd>
                </>
              )}
              {success && (
                <>
                  <dt>{t('detail.spec.success')}</dt>
                  <dd>{success}</dd>
                </>
              )}
            </dl>
          )}
        </>
      )}
    </section>
  )
}

// —---------------------------------------------------------------------------
// Info — reference data, folded.
//
// These are the values you check once ("which attempt was this, on which
// node") and never read again. Open, they were the first band under the
// header: five columns of grey ids ahead of the result. Folded, the header
// line still carries the four that identify the run.
// —---------------------------------------------------------------------------

export function InfoSection({ task, onOpenTask }: { task: Task; onOpenTask?(id: string): void }) {
  const [open, setOpen] = useState(false)
  const summary = [
    `#${shortId(task.id)}`,
    task.project || '',
    task.owner || '',
    fmtShort(task.created_at),
  ]
    .filter(Boolean)
    .join(' · ')
  return (
    <section class="dt-sec">
      <button
        type="button"
        class={'dt-fold' + (open ? ' is-open' : '')}
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        <span class="dt-sec-title">{t('detail.info')}</span>
        <span class="dt-fold-summary">{summary}</span>
        <Icon name={open ? 'chevron-down' : 'chevron-right'} size={13} class="dt-fold-ico" />
      </button>
      {open && (
        <dl class="dt-facts">
          <Fact label={t('detail.id')} value={task.id} mono title={task.id} />
          {task.parent_id && (
            <div class="dt-fact">
              <dt>{t('detail.parent')}</dt>
              <dd>
                <button
                  type="button"
                  class="linklike mono"
                  onClick={() => onOpenTask?.(task.parent_id)}
                  title={task.parent_id}
                >
                  #{shortId(task.parent_id)}
                </button>
              </dd>
            </div>
          )}
          <Fact label={t('detail.project')} value={task.project || '—'} />
          <Fact label={t('detail.owner')} value={task.owner || '—'} />
          <Fact label={t('detail.attempt')} value={task.attempt_id || '—'} mono title={task.attempt_id} />
          <Fact label={t('detail.created')} value={fmtFull(task.created_at)} />
          <Fact label={t('detail.updated')} value={fmtFull(task.updated_at)} />
          {task.risk && <Fact label={t('detail.risk')} value={task.risk} />}
        </dl>
      )}
    </section>
  )
}

function Fact({
  label,
  value,
  mono,
  title,
}: {
  label: string
  value: string
  mono?: boolean
  title?: string
}) {
  return (
    <div class="dt-fact">
      <dt>{label}</dt>
      <dd class={mono ? 'mono' : ''} title={title ?? value}>
        {value}
      </dd>
    </div>
  )
}

/** The task's event log. A run that failed after forty tool calls has
 *  thousands of pixels of transcript, and the page used to open on all of it:
 *  the result, the brief and the facts sat above a wall you had to scroll
 *  past to reach anything else. Long logs therefore open on their tail — the
 *  part that says how the run ended — with the count and a way to see it all.
 *
 *  The fold trips at the tail size itself. It used to need three times the
 *  tail before it folded at all, so an eighteen-event run — the common case —
 *  printed every row: 963px, 55% of the page, for a log nobody came here to
 *  read top-down. */
export function TimelineSection({
  events,
  suppressResult,
  tail = TIMELINE_TAIL,
}: {
  events: TaskEvent[]
  suppressResult?: boolean
  /** How many rows the folded state shows. The chat card, at 360px, has less
   *  room than the page and asks for fewer. */
  tail?: number
}) {
  const [open, setOpen] = useState(false)
  const long = events.length > tail
  const shown = long && !open ? events.slice(-tail) : events
  return (
    <section class="dt-sec">
      <div class="dt-sec-head">
        <h2 class="dt-sec-title">{t('detail.timeline')}</h2>
        {long && (
          <>
            <span class="dt-sec-hint">
              {open
                ? t('detail.timeline.count', { n: String(events.length) })
                : t('detail.timeline.tail', { n: String(tail) })}
            </span>
            <button type="button" class="dt-sec-action" onClick={() => setOpen((v) => !v)}>
              {open ? t('events.collapse') : t('detail.timeline.expand')}
            </button>
          </>
        )}
      </div>
      {events.length > 0 ? (
        <EventTimeline events={shown} suppressResultBody={suppressResult} />
      ) : (
        <p class="dim">{t('common.empty')}</p>
      )}
    </section>
  )
}
