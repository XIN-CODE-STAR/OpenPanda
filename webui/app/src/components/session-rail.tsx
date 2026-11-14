// SPDX-License-Identifier: AGPL-3.0-or-later

/** The thread's status line, always printed in the pane head: what is running
 *  (and on which machine), how full the last request's context window was,
 *  and what the worktree holds.
 *
 *  There is no panel behind this line any more. The overview used to be a card
 *  that opened on click — three sections, a change list, a meter, a note. It
 *  covered the right edge of the transcript, which is exactly where a short
 *  thread's right-aligned question ends, and most threads never opened it, so
 *  the three answers it held were in practice never read. The head has room
 *  for one line, so the answers live in the line: idle or the running task's
 *  title with its machine, the context share, and the change description.
 *
 *  The change description is the only part that can be arbitrarily long (a
 *  path list, or the sentence explaining why there is no worktree). It scrolls
 *  inside a fixed viewport instead of being cut with an ellipsis or pushing
 *  the rest of the head off the row.
 *
 *  Deliberately NOT here: a per-category context breakdown. The line reports
 *  a turn's billed tokens, not what composed the prompt, so a breakdown would
 *  be invented numbers. The share is omitted when the window size is unknown. */

import { useEffect, useMemo, useRef, useState } from 'preact/hooks'
import type { NodeInfo, SessionDiff, Task } from '../api/client'
import { t } from '../i18n'
import { Icon } from './icons'
import { compactNumber } from '../format/number'
import { isActiveState } from './task-activity'
import { contextShare } from './session-rail-stats'

/** Gap between the two copies of a ticker's text. The keyframe translates by
 *  half the track plus half this gap, so the second copy lands exactly where
 *  the first one started and the loop has no visible seam. */
const TICKER_GAP = 28
const SEP = ' · '

/** One run of text in the ticker. The +/− counts keep the diff drawer's
 *  semantic colours, so even a scrolling line still reads as a diff. */
interface TickerPart {
  text: string
  tone?: 'add' | 'del' | 'muted'
}

function nodeName(nodes: NodeInfo[], id: string): string {
  const hit = nodes.find((n) => n.id === id)
  return hit?.name || hit?.id || id
}

/** Task titles are user text: a newline pasted into a title would otherwise
 *  break the one-line status bar into two. */
function oneLine(text: string): string {
  return text.replace(/\s+/g, ' ').trim()
}

/** A viewport that scrolls its text only when the text does not fit. Two
 *  copies make the loop seamless; a short string keeps one copy and sits
 *  still, because animating text that already fits is noise. Hovering pauses
 *  the scroll so a path can actually be read, and reduced-motion turns the
 *  viewport into a plain horizontally scrollable box. */
function ScrollingText({ text, parts }: { text: string; parts: TickerPart[] }) {
  const viewport = useRef<HTMLSpanElement>(null)
  const copy = useRef<HTMLSpanElement>(null)
  const [scroll, setScroll] = useState(false)
  const [span, setSpan] = useState(0)

  useEffect(() => {
    const vp = viewport.current
    const first = copy.current
    if (!vp || !first) return
    const measure = () => {
      const width = first.getBoundingClientRect().width
      setScroll(width > vp.clientWidth + 1)
      setSpan(width + TICKER_GAP)
    }
    measure()
    // Both the pane (a dock opening, a window resize) and the text itself can
    // change the fit; one observer answers both without a resize listener.
    if (typeof ResizeObserver === 'undefined') return
    const ro = new ResizeObserver(measure)
    ro.observe(vp)
    ro.observe(first)
    return () => ro.disconnect()
  }, [text])

  // Both copies are built from the same parts: the second one is a clone for
  // the loop, not a place where the colours may drift out of step.
  const renderCopy = (hidden: boolean) => (
    <span
      class="rail-ticker-copy"
      ref={hidden ? undefined : copy}
      aria-hidden={hidden ? 'true' : undefined}
    >
      {parts.map((part, i) => (
        <span key={i} class={part.tone ? `rail-tone-${part.tone}` : undefined}>
          {part.text}
        </span>
      ))}
    </span>
  )

  // ~20px/s, and the keyframes hold the first and last frames: long path
  // lists are not rushed, and the beginning of a sentence stays readable for
  // a moment instead of being scrolled away the instant it is printed.
  const duration = Math.min(36, Math.max(12, span / 20))
  return (
    <span class={`rail-ticker${scroll ? ' is-scrolling' : ''}`} ref={viewport} title={text}>
      <span class="rail-ticker-track" style={scroll ? `animation-duration:${duration}s` : undefined}>
        {renderCopy(false)}
        {scroll && renderCopy(true)}
      </span>
    </span>
  )
}

export interface SessionRailProps {
  /** The thread's worktree changes, or null before one exists. */
  diff: SessionDiff | null
  /** The thread's tasks, live rows included. */
  tasks: Task[]
  nodes: NodeInfo[]
  /** Billed tokens of the newest turn: the context footprint the last request
   *  actually carried. 0 when the thread has not asked anything yet. */
  contextTokens: number
  /** The active model's context window, or 0 when the registry has no row to
   *  read one from. Read by the page, not here: the dock's start pane prints
   *  the same number, and two readers of one registry is one read too many. */
  contextWindow: number
  /** The model that wrote the newest turn, already resolved for display. */
  modelName: string
  /** Where this thread is pinned to run, already resolved for display. */
  machine: string
  /** Entry-model work exists before a dispatched task row does. */
  working?: boolean
  onOpenDiff(): void
  onOpenTask(id: string): void
  onOpenQueue(): void
}

export function SessionRail(props: SessionRailProps) {
  const contextWindow = props.contextWindow
  const modelName = props.modelName

  // Root rows only: a sub-agent is part of its parent's row, and the count of
  // them is the "N 子任务" tail — the rail is a summary, so a delegation tree
  // must not read as N unrelated jobs.
  const live = props.tasks.filter((task) => !task.spawn_depth && isActiveState(task.state))
  const running = live[0]

  // What the thread's tasks reported touching. `files_changed` in a run's
  // result is a snapshot diff of its working directory, so unlike the worktree
  // diff it works in a plain folder — which is the only signal a thread whose
  // project is not a git repository ever gets.
  const reported = useMemo(() => {
    const out: string[] = []
    const seen = new Set<string>()
    for (const task of props.tasks) {
      for (const file of task.files_changed ?? []) {
        if (!file || seen.has(file)) continue
        seen.add(file)
        out.push(file)
      }
    }
    return out
  }, [props.tasks])

  const changes = props.diff?.changes ?? []
  // A file the worktree already lists is not repeated from a task report: the
  // worktree row carries a line count, the report row does not, so the same
  // path would appear twice with different amounts of truth.
  const changePaths = new Set(changes.map((c) => c.path))
  const extra = reported.filter((path) => !changePaths.has(path))
  const reportedStats = useMemo(() => {
    const map = new Map<string, { kind: string; lines: number }>()
    for (const task of props.tasks) {
      for (const stat of task.files_stat ?? []) {
        if (!stat?.path) continue
        map.set(stat.path, { kind: stat.kind, lines: stat.lines ?? 0 })
      }
    }
    return map
  }, [props.tasks])
  const reportedAdded = extra.reduce(
    (sum, path) => sum + (reportedStats.get(path)?.kind === 'added' ? reportedStats.get(path)!.lines : 0),
    0,
  )
  const stat = props.diff?.stat

  const pct = contextShare(props.contextTokens, contextWindow)
  // A thread that has spent 144 tokens of a million rounds to 0%, which reads
  // as "nothing was used" — the opposite of what the number says. Below one
  // percent the line says so instead of rounding down to nothing.
  const shareLabel =
    pct === null
      ? ''
      : pct === 0 && props.contextTokens > 0
        ? t('sessionRail.contextUnder1')
        : `${pct}%`

  // The running task's title and the machine it is on. When the thread is
  // idle the line answers the same question with the pinned machine instead.
  const where = running
    ? running.origin_local
      ? t('sessionRail.thisMachine')
      : running.origin_node
        ? nodeName(props.nodes, running.origin_node)
        : props.machine
    : props.machine
  const taskTitle = running
    ? oneLine(running.title || '') || t('sessionRail.running', { n: String(live.length) })
    : ''
  const taskLine = running
    ? [taskTitle, where, live.length > 1 ? `+${live.length - 1}` : ''].filter(Boolean).join(SEP)
    : props.working
      ? t('sessionRail.modelRunning')
      : t('sessionRail.idleWhere', { machine: props.machine })

  // Percentage first, then the raw numbers. A model with no catalogue window
  // still gets its token count: "unknown" is a reason to omit the share, not
  // a reason to hide the one number the turn actually reported.
  const used = props.contextTokens > 0 ? compactNumber(props.contextTokens) : '—'
  const total = contextWindow > 0 ? compactNumber(contextWindow) : ''
  const usage = total ? `${used} / ${total}` : props.contextTokens > 0 ? used : ''
  const contextLine = [shareLabel, usage].filter(Boolean).join(SEP) || '—'

  // The ticker is a file list, not prose. "Not a git repository" and "the task
  // reported no changes" used to scroll past here and told the reader nothing
  // they could act on; the reason now lives in the tooltip. With no paths the
  // line is a single dash — an empty list is an answer, not a missing panel.
  const paths = [...changes.map((c) => c.path), ...extra]
  const changeParts: TickerPart[] = []
  const add = (text: string, tone?: TickerPart['tone']) => changeParts.push({ text, tone })
  if (stat && changes.length > 0) {
    add(`+${stat.insertions}`, 'add')
    add(` −${stat.deletions}`, 'del')
    add(SEP)
  } else if (reportedAdded > 0) {
    add(`+${reportedAdded}`, 'add')
    add(' −0', 'del')
    add(SEP)
  }
  add(paths.length > 0 ? paths.slice(0, 20).join(' · ') : '—', 'muted')
  const changeText = changeParts.map((p) => p.text).join('')
  // Hover keeps what the line no longer spends width on: the totals, every
  // path (the ticker shows the first twenty), or why there is no list at all.
  const changeTitle = paths.length
    ? [
        stat ? `+${stat.insertions} −${stat.deletions}` : reportedAdded > 0 ? `+${reportedAdded} −0` : '',
        t('sessionRail.files', { n: String(paths.length) }),
        paths.join(' · '),
      ]
        .filter(Boolean)
        .join(SEP)
    : [
        props.diff ? t('sessionRail.noChanges') : t('sessionRail.noWorktree'),
        props.tasks.length > 0 ? t('sessionRail.noReported') : '',
      ]
        .filter(Boolean)
        .join(SEP)

  const openTask = () => {
    if (running && live.length === 1) props.onOpenTask(running.id)
    else props.onOpenQueue()
  }

  return (
    <div class="rail-dock">
      {/* `has-changes` is what tells the stylesheet there is a list to make
          room for: without it the bar hugs the sentence, so an idle thread's
          line ends at the dash instead of holding a blank lane open beside
          it. The ticker's own measurement still decides scrolling. */}
      <div
        class={`rail-bar${paths.length > 0 ? ' has-changes' : ''}`}
        role="group"
        aria-label={t('sessionRail.label')}
      >
        <Icon name="activity" size={16} class={`rail-bar-icon${running || props.working ? ' is-live' : ''}`} />
        <button
          type="button"
          class={`rail-bar-task${running ? ' is-live' : ''}`}
          title={taskLine}
          onClick={openTask}
        >
          {running ? (
            <>
              <span class="rail-bar-task-title">{taskTitle}</span>
              <span class="rail-bar-task-sep" aria-hidden="true">
                ·
              </span>
              <span class="rail-bar-task-where">
                {where}
                {live.length > 1 ? ` +${live.length - 1}` : ''}
              </span>
            </>
          ) : (
            <span class="rail-bar-task-title">{taskLine}</span>
          )}
        </button>
        <span class="rail-bar-sep" aria-hidden="true">
          ·
        </span>
        <span
          class={`rail-bar-ctx${pct !== null && pct >= 80 ? ' is-high' : ''}`}
          title={
            contextWindow > 0
              ? t('sessionRail.contextNote', { model: modelName })
              : t('sessionRail.contextNoWindow')
          }
        >
          {contextLine}
        </span>
        <span class="rail-bar-sep" aria-hidden="true">
          ·
        </span>
        {props.diff ? (
          <button type="button" class="rail-bar-changes" title={changeTitle} onClick={props.onOpenDiff}>
            <ScrollingText text={changeText} parts={changeParts} />
          </button>
        ) : (
          <span class="rail-bar-changes" title={changeTitle}>
            <ScrollingText text={changeText} parts={changeParts} />
          </span>
        )}
      </div>
    </div>
  )
}
