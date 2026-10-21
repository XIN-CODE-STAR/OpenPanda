// SPDX-License-Identifier: AGPL-3.0-or-later

import type { ComponentChildren } from 'preact'
import { useMemo, useState } from 'preact/hooks'
import type { ScoredCandidate, Task, TraceEvent } from '../api/client'
import { t } from '../i18n'
import { orbitPhaseFromTraces, useTraceForTask } from '../hooks'
import { Icon } from './icons'
import { latestActivity } from './task-activity'

// —---------------------------------------------------------------------------
// TaskTrace — the task detail page's execution strip.
//
// The detail page used to carry the full 大总管 card: three stacked steps, each
// with its own grey plate, a full-width progress bar and an action rail. On a
// one-shot task that is ~300px of chrome for two facts ("classified as task",
// "routed to this machine") that were already implied by the task existing.
//
// This is the same trace read as one line: [step] ─ [step] ─ [step], with the
// facts that actually vary (node, agent, rounds) attached to their step, and
// everything else — candidates, plan goal, supervisor note — one click down.
// Steps that never happened are not drawn at all: a direct dispatch has no
// classification and no routing decision, so it shows "dispatch → execute"
// instead of two rows that say "skipped".
// ---------------------------------------------------------------------------

type StepState = 'done' | 'active' | 'failed' | 'idle'

interface Step {
  key: string
  label: string
  fact?: string
  state: StepState
  /** Tooltip when the fact is clipped. */
  hint?: string
}

export function TaskTrace({ task, defaultOpen }: { task: Task; defaultOpen?: boolean }) {
  // Same seeding as the orbit: the task's newest event may be the only row
  // that says what a mid-flight run is doing on first paint.
  const seed = useMemo(() => {
    const arr = [...(task.traces ?? [])]
    if (task.last_event && !arr.some((e) => e.id === task.last_event!.id)) arr.push(task.last_event)
    return arr
  }, [task.traces, task.last_event])
  const traces = useTraceForTask(task.id, seed)

  const classify = useMemo(() => firstData<ClassifyData>(traces, 'classify_result'), [traces])
  const route = useMemo(() => firstData<RouteData>(traces, 'route_decision'), [traces])
  const exec = useMemo(() => firstData<ExecData>(traces, 'exec_agent_start'), [traces])
  const supervision = useMemo(
    () => lastData<SupervisionData>(traces, 'supervision_round'),
    [traces],
  )

  const state = task.state
  const running = state === 'running' || state === 'dispatched' || state === 'waiting_context'
  const queued = state === 'queued' || state === 'submitted'
  const failed = state === 'failed' || state === 'cancelled' || state === 'expired'
  const settled = state === 'done' || state === 'review'

  const live = useMemo(() => (running ? latestActivity(traces, task) : ''), [running, traces, task])

  // The step name follows the run: "执行中" over a finished task was the same
  // lie the orbit used to tell.
  const execLabel = settled || failed ? t('orbit.step.execEnded') : t('orbit.step.exec')

  // "任务" as the classification fact said nothing: every row on this page is
  // already a task, so the strip printed the word twice — once in the page and
  // once as the classifier's verdict. Other kinds (answer, tool, plan) still
  // carry their fact because they actually distinguish the run.
  const classifyText = useMemo(() => (classify ? classifyFact(classify) : undefined), [classify])

  const steps = useMemo<Step[]>(() => {
    const out: Step[] = []
    if (classify) {
      out.push({
        key: 'classify',
        label: t('orbit.step.classify'),
        fact: classifyText,
        state: 'done',
        hint: classify?.plan_goal ?? classify?.note ?? classify?.label,
      })
    }
    if (route) {
      out.push({
        key: 'route',
        label: t('orbit.step.route'),
        fact: routedName(route),
        state: 'done',
      })
    }
    if (!classify && !route) {
      // Direct dispatch: no classifier ran and no candidate was scored. Say
      // where it went rather than drawing two "skipped" rows.
      out.push({
        key: 'dispatch',
        label: t('detail.trace.dispatch'),
        fact: exec?.node_name || t('orbit.route.localAttr'),
        state: 'done',
      })
    }
    out.push({
      key: 'exec',
      label: execLabel,
      fact: exec?.agent || (queued ? t('detail.trace.waiting') : undefined),
      state: failed ? 'failed' : settled ? 'done' : running ? 'active' : 'idle',
    })
    return out
  }, [classify, classifyText, route, exec, settled, failed, running, queued, execLabel])

  const round =
    supervision?.round !== undefined && supervision?.budget !== undefined
      ? t('orbit.exec.round', { round: String(supervision.round), budget: String(supervision.budget) })
      : ''

  const progress = useMemo(
    () => (settled ? 100 : running ? progressOf(traces, task.state, supervision) : 0),
    [settled, running, traces, task.state, supervision],
  )

  const hasDetail = Boolean(
    route?.candidates?.length ||
      classify?.plan_goal ||
      classify?.note ||
      supervision?.judge_summary ||
      supervision?.verdict_status,
  )
  // Folded on the page, where the strip is one of eight sections and the
  // reader is scrolling; open in the chat card, where clicking the task *was*
  // the request to see how it ran.
  const [open, setOpen] = useState(Boolean(defaultOpen))

  return (
    <section class="tt" aria-label={t('detail.trace')}>
      <div class="tt-row">
        <span class="tt-label">{t('detail.trace')}</span>

        <ol class="tt-steps">
          {steps.map((s, i) => (
            <li key={s.key} class={`tt-step is-${s.state}`}>
              {i > 0 && <span class="tt-link" aria-hidden="true" />}
              <span class="tt-dot" aria-hidden="true">
                {s.state === 'done' ? (
                  <Icon name="check" size={8} />
                ) : s.state === 'failed' ? (
                  <Icon name="x" size={8} />
                ) : null}
              </span>
              <span class="tt-name">{s.label}</span>
              {s.fact && (
                <span class="tt-fact" title={s.hint ?? s.fact}>
                  {s.fact}
                </span>
              )}
            </li>
          ))}
        </ol>

        <span class="tt-tail">
          {live && (
            <span class="tt-live" role="status">
              <span class="live-dot beating" aria-hidden="true" />
              <span class="tt-live-text">{live}</span>
            </span>
          )}
          {round && <span class="tt-meta">{round}</span>}
          {running && <span class="tt-meta tt-pct">{progress}%</span>}
          {hasDetail && (
            <button
              type="button"
              class={'tt-toggle' + (open ? ' is-open' : '')}
              aria-expanded={open}
              aria-label={t('detail.trace.detail')}
              title={t('detail.trace.detail')}
              onClick={() => setOpen((v) => !v)}
            >
              <Icon name="chevron-down" size={14} />
            </button>
          )}
        </span>
      </div>

      {running && (
        <span
          class="tt-progress"
          role="progressbar"
          aria-valuenow={progress}
          aria-valuemin={0}
          aria-valuemax={100}
          aria-label={t('orbit.summary.progress', { pct: progress })}
        >
          <span class="tt-progress-fill" style={`--prog:${progress}%`} />
        </span>
      )}

      {open && hasDetail && (
        <div class="tt-detail">
          <TraceCell label={t('orbit.step.classify')}>
            {classify ? (
              <>
                {classifyText && <span>{classifyText}</span>}
                {(classify.plan_goal ?? classify.note ?? classify.label) && (
                  <span class="dim">{classify.plan_goal ?? classify.note ?? classify.label}</span>
                )}
                {!classifyText && !(classify.plan_goal ?? classify.note ?? classify.label) && (
                  <span class="dim">—</span>
                )}
              </>
            ) : (
              <span class="dim">{t('detail.trace.dispatch')}</span>
            )}
          </TraceCell>

          <TraceCell label={t('orbit.step.route')}>
            {route?.candidates?.length ? (
              route.candidates.map((c) => {
                const chosen = routedId(route) === c.node_id
                const score = c.score_breakdown?.total ?? c.total_score
                return (
                  <span key={c.node_id} class={chosen ? 'tt-cand is-chosen' : 'tt-cand'}>
                    <span>{c.node_name ?? c.node_id}</span>
                    {typeof score === 'number' && (
                      <span class="dim">{score.toFixed(2)}★</span>
                    )}
                  </span>
                )
              })
            ) : (
              <span class="dim">
                {route ? routedName(route) ?? t('orbit.route.declined') : t('detail.trace.dispatch')}
              </span>
            )}
          </TraceCell>

          <TraceCell label={execLabel}>
            {exec?.agent && <span>{exec.agent}</span>}
            {exec?.node_name && <span class="dim">@{exec.node_name}</span>}
            {/* The round is on the strip's own top line (`.tt-tail`), beside
                the live status — repeating it down here put the same "轮次
                1/5" twice in one card, one under the other. */}
            {supervision?.verdict_status && (
              <span class="dim">
                {t('detail.result.verdict', { verdict: supervision.verdict_status })}
              </span>
            )}
            {supervision?.judge_summary && (
              <span class="dim tt-judge">{supervision.judge_summary}</span>
            )}
          </TraceCell>
        </div>
      )}
    </section>
  )
}

function TraceCell({ label, children }: { label: string; children: ComponentChildren }) {
  return (
    <div class="tt-cell">
      <span class="tt-cell-label">{label}</span>
      {children}
    </div>
  )
}

// —---------------------------------------------------------------------------
// Data shapes — the trace payloads this strip reads. Kept local and narrow:
// the orbit has its own copies for the chat card.
// ---------------------------------------------------------------------------

interface ClassifyData {
  kind?: string
  class?: string
  note?: string
  label?: string
  plan_goal?: string
  stages_count?: number
  stage_count?: number
  tool_count?: number
  tools?: unknown[]
}

interface RouteData {
  action?: string
  /** The wire key the scheduler emits (submit.go); `target`/`chosen` stay as
   *  fallbacks for traces written before the name settled. */
  target_node?: string
  target?: string
  chosen?: string
  candidates?: ScoredCandidate[]
}

interface ExecData {
  agent?: string
  node_name?: string
}

interface SupervisionData {
  round?: number
  budget?: number
  verdict_status?: string
  judge_summary?: string
}

function classifyFact(c: ClassifyData): string | undefined {
  switch (c.kind ?? c.class) {
    case 'answer':
      return t('orbit.classify.answer')
    case 'tool':
    case 'tool_call': {
      const n = c.tool_count ?? (Array.isArray(c.tools) ? c.tools.length : 0)
      return n > 0 ? t('orbit.classify.toolN', { n }) : t('orbit.classify.tool')
    }
    case 'task':
      // Deliberately blank — see the classifyText note above.
      return undefined
    case 'plan':
      return t('orbit.classify.plan', { n: c.stages_count ?? c.stage_count ?? 1 })
    default:
      return undefined
  }
}

/** Coarse completion for the strip's hairline bar: the phase ramp the orbit
 *  uses, with the supervision round as partial credit inside execution. */
function progressOf(
  traces: TraceEvent[],
  state: string | undefined,
  sup: SupervisionData | null,
): number {
  const phase = orbitPhaseFromTraces(traces, state)
  if (phase === null) return 0
  if (phase >= 3) return 100
  let p = [10, 35, 60][phase] ?? 0
  if (phase === 2 && sup?.round !== undefined && sup?.budget !== undefined) {
    const budget = Math.max(1, sup.budget)
    p = Math.min(98, p + (sup.round / budget) * (100 - p) * 0.9)
  }
  return Math.max(0, Math.min(100, Math.round(p)))
}

/** The node id the route picked, or undefined when the decision was a
 *  decline or carried no target. */
function routedId(route: RouteData): string | undefined {
  return route.target_node ?? route.target ?? route.chosen
}

/** The picked node as a display name: `target_node` is an id, and the
 *  candidate list maps it to the name the operator chose. A decline resolves
 *  to undefined rather than to the top candidate — naming one anyway would
 *  claim a route that never happened. */
function routedName(route: RouteData): string | undefined {
  const id = routedId(route)
  if (id) return route.candidates?.find((c) => c.node_id === id)?.node_name ?? id
  if (route.action === 'decline') return undefined
  return route.candidates?.[0]?.node_name ?? route.candidates?.[0]?.node_id
}

function firstData<T>(traces: TraceEvent[], type: TraceEvent['type']): T | null {
  for (const e of traces) if (e?.type === type) return (e.data ?? null) as T | null
  return null
}

function lastData<T>(traces: TraceEvent[], type: TraceEvent['type']): T | null {
  for (let i = traces.length - 1; i >= 0; i--) {
    const e = traces[i]
    if (e?.type === type) return (e.data ?? null) as T | null
  }
  return null
}
