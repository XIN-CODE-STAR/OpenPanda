// SPDX-License-Identifier: AGPL-3.0-or-later

/** The finalizer's report line, read back from the transcript.
 *
 *  A finished task is appended to the thread as an assistant turn whose first
 *  line is `[state] @agent · title` followed by the employee's closing
 *  paragraph (see webui/panel/finalize.go, `summaryText`). The prefix is
 *  protocol, not prose: the history's unread dot reads it to tell a dead task
 *  from a live thread, so it has to stay in the stored text — but printing
 *  `[done]` at a reader in a Chinese thread is the console leaking its own
 *  wire format. This module splits that one line off so the view can draw it
 *  as a report header (state, who ran it, what the job was) and leave the
 *  body as ordinary Markdown.
 *
 *  Only the first line is considered, and only a state the console knows:
 *  a reply that happens to start with a bracketed word ("[注意] ...") is an
 *  ordinary answer, not a report. */

export interface ReportHead {
  /** One of the task states the finalizer can emit (`done`, `failed`, …). */
  state: string
  /** The agent that ran it, when the finalizer could name one. */
  agent: string
  /** The job's title — the line that used to follow the state tag. */
  title: string
  /** Everything after the header line: the closing paragraph, or ''. */
  body: string
}

const STATES = new Set([
  'submitted',
  'queued',
  'dispatched',
  'waiting_context',
  'running',
  'review',
  'done',
  'failed',
  'cancelled',
  'expired',
])

/** `[state] @agent · title` — the agent leg is optional (a local run whose
 *  events never named one, and every cancelled run, has no `@`). */
const HEAD_LINE = /^\[([a-z_]+)\](?: @(\S+) ·)? ?(.*)$/

export function splitReportHead(text: string): ReportHead | null {
  if (!text || text[0] !== '[') return null
  const nl = text.indexOf('\n')
  const first = nl === -1 ? text : text.slice(0, nl)
  const body = nl === -1 ? '' : text.slice(nl + 1).replace(/^\n+/, '')
  const m = HEAD_LINE.exec(first)
  if (!m) return null
  const state = m[1] ?? ''
  if (!STATES.has(state)) return null
  const agent = (m[2] ?? '').replace(/^@/, '')
  const title = (m[3] ?? '').trim()
  // A bare `[done]` with nothing after it is the finalizer's no-detail case;
  // there is still a header worth drawing, but only if the state is real.
  return { state, agent, title, body }
}
