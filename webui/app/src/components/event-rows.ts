// SPDX-License-Identifier: AGPL-3.0-or-later

// Timeline row grouping: the flat task_events stream becomes render rows —
// consecutive dispatch-plumbing writes (submit → queue → delegate → accept)
// fold into one pipeline strip so the log reads as what the scheduler did,
// not four bookkeeping cards; a run of quiet rows (the adapter's "still
// running, no output" heartbeats) folds into one row with a ×N repeat count
// so a four-minute silence is one thin line, not five badge cards.

import type { TaskEvent } from '../api/client'
import { formatTaskEvent } from './event-parser.ts'

export type TimelineRow =
  | { kind: 'pipeline'; ts: number; steps: { step: string; node?: string; failed?: boolean }[] }
  | { kind: 'event'; event: TaskEvent; formatted: ReturnType<typeof formatTaskEvent>; count?: number }

/** Groups the flat event list into render rows: consecutive dispatch-plumbing
 *  events merge into one pipeline strip; consecutive quiet events of the same
 *  type fold into their newest row with a repeat count. */
export function buildRows(events: TaskEvent[]): TimelineRow[] {
  const rows: TimelineRow[] = []
  let pipe: Extract<TimelineRow, { kind: 'pipeline' }> | null = null
  // The pushed row a quiet-run folds into — kept by reference so absorbing
  // the next same-type quiet event just updates it in place.
  let quietRun: Extract<TimelineRow, { kind: 'event' }> | null = null
  const flush = () => {
    if (pipe && pipe.steps.length) rows.push(pipe)
    pipe = null
  }
  for (const ev of events) {
    const formatted = formatTaskEvent(ev.type, ev.data)
    if (formatted.lifecycle) {
      quietRun = null
      if (!pipe) pipe = { kind: 'pipeline', ts: ev.ts, steps: [] }
      pipe.steps.push(formatted.lifecycle)
      continue
    }
    if (formatted.quiet && quietRun && quietRun.event.type === ev.type) {
      quietRun.event = ev
      quietRun.formatted = formatted
      quietRun.count = (quietRun.count ?? 1) + 1
      continue
    }
    // A retry storm — the adapter reporting the same 429 over and over — is
    // one fact with a moving counter. Folding those keeps the newest message
    // (and its raw payload) behind a ×N chip instead of painting eight
    // identical rows. Ordinary progress notes are NOT folded: two different
    // notes ("bash: inspect…", "write: plans/…") are two different facts even
    // though they share the 执行进度 label. Nor may a retry merge across an
    // open pipeline run: `tail` is the last *flushed* row, so folding while
    // `pipe` is open would order a newer event before the lifecycle strip it
    // actually follows.
    const tail = rows[rows.length - 1]
    if (
      !pipe &&
      isRetryNote(formatted) &&
      tail?.kind === 'event' &&
      tail.event.type === ev.type &&
      isRetryNote(tail.formatted)
    ) {
      tail.event = ev
      tail.formatted = formatted
      tail.count = (tail.count ?? 1) + 1
      quietRun = null
      continue
    }
    flush()
    const row: Extract<TimelineRow, { kind: 'event' }> = { kind: 'event', event: ev, formatted }
    rows.push(row)
    quietRun = formatted.quiet ? row : null
  }
  flush()
  return rows
}

/** Retry notices from the adapter ("Claude: API retry 3 (429)…"). Only these
 *  fold by repetition. */
function isRetryNote(f: ReturnType<typeof formatTaskEvent>): boolean {
  return /\bretry\b/i.test(f.summary ?? '') || /\bretry\b/i.test(f.label ?? '')
}
