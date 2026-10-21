// SPDX-License-Identifier: AGPL-3.0-or-later

import { useState } from 'preact/hooks'
import { api, isTaskStalled, type ApprovalScope, type Task } from '../api/client'
import { useAsync, useChangeSignal, useLocaleRerender, useVisibleInterval } from '../hooks'
import { t } from '../i18n'
import { formatDuration } from '../format/time'
import { navigate } from '../nav'
import { ScopeSelect } from '../components/scope-select'
import { StateBadge } from '../components/state-badge'
import { toast, toastError } from '../components/toast'
import { confirmDialog } from '../components/confirm'
import { TaskTrace } from '../components/task-trace'
import {
  InfoSection,
  SpecCard,
  TimelineSection,
  fmtFull,
  fmtShort,
  parseResult,
  shortId,
  text,
} from '../components/task-sections'
import { DiffViewer } from '../components/diff-viewer'
import { Markdown } from '../md/render'
import { Icon } from '../components/icons'
import { prettifyJson } from '../format/json'

// The task detail page reads as a document, not a stack of cards: a header
// with the title and the facts that identify the run, one line of trace, then
// two columns — what happened (result / brief / timeline) beside what it is
// (ids, delegation, children). Everything that used to be a full-width card
// with its own padding is now a rule-separated section, which is what made
// the page three screens tall for one failed run.
export function DetailView({
  id,
  onBack,
  onOpenSession,
  onOpenTask,
}: {
  id: string
  onBack(): void
  onOpenSession?(id: string): void
  onOpenTask?(id: string): void
}) {
  useLocaleRerender()
  const change = useChangeSignal()
  const [tick, setTick] = useState(0)
  const { data: task, error } = useAsync(() => api.task(id), [id], change + tick)
  // The task's board rides the same refresh cadence: a queued note posted
  // below must render, not just be accepted, or the send reads as lost.
  const { data: board } = useAsync(() => api.taskBoardMessages(id), [id], change + tick)
  // A stalled task emits no SSE event, so the badge can only appear on a
  // local clock tick.
  useVisibleInterval(() => setTick((v) => v + 1), 30_000)
  const [busy, setBusy] = useState(false)
  const [rejecting, setRejecting] = useState(false)
  const [rejectReason, setRejectReason] = useState('')
  // Where the review answer is remembered — "once" by default; session and
  // project write a standing gate answer for what comes after.
  const [scope, setScope] = useState<ApprovalScope>('once')
  const [msgText, setMsgText] = useState('')

  async function act(fn: () => Promise<unknown>, okMsg?: string) {
    if (busy) return
    setBusy(true)
    try {
      await fn()
      if (okMsg) toast(okMsg, 'success')
    } catch (e) {
      toastError(e)
    } finally {
      setBusy(false)
    }
  }

  async function cancelTask() {
    const ok = await confirmDialog({
      title: t('detail.cancelConfirmTitle'),
      message: t('detail.cancelConfirmMsg'),
      confirmLabel: t('detail.cancelTask'),
    })
    if (!ok) return
    await act(() => api.cancel(task!.id), t('detail.cancelledToast'))
  }

  async function revertTask() {
    const ok = await confirmDialog({
      title: t('detail.revertConfirmTitle'),
      message: t('detail.revertConfirmMsg'),
      confirmLabel: t('detail.revert'),
    })
    if (!ok) return
    await act(async () => {
      const r = await api.taskRevert(task!.id)
      toast(
        t('detail.revertedToast', {
          restored: String(r.restored?.length ?? 0),
          removed: String(r.removed?.length ?? 0),
        }),
        'success',
      )
    })
  }

  // The two send modes: queue leaves the current round alone and the note
  // lands as context at the next one; inject aborts the in-flight round when
  // this console runs the task and makes the text the next round's leading
  // directive. The toast reports which one actually happened.
  async function sendMessage(inject: boolean) {
    const text = msgText.trim()
    if (!text) return
    await act(async () => {
      const r = await api.sendTaskMessage(task!.id, text, inject)
      toast(
        r.warning ||
          (r.status === 'interrupted' ? t('detail.msgInjectedToast') : t('detail.msgQueuedToast')),
        r.warning ? 'info' : 'success',
      )
      setMsgText('')
      // Refetch now: a store-only delivery (no engine to fan out a trace
      // event) emits no SSE signal, so the note would sit invisible until
      // the next tick — which reads exactly like a lost message.
      setTick((v) => v + 1)
    })
  }

  if (error)
    return (
      <section>
        <button class="dt-back" onClick={onBack}>
          <Icon name="arrow-left" size={14} /> {t('detail.back')}
        </button>
        <div class="card error-state u-mt-16">
          <span class="error-state-title">{t('common.loadFail')}</span>
          <span class="dim error-state-detail">{error}</span>
          <div>
            <button class="btn" onClick={() => setTick((v) => v + 1)}>
              {t('common.retry')}
            </button>
          </div>
        </div>
      </section>
    )
  if (!task) return <p class="dim">{t('common.loading')}</p>

  const cancellable = !['done', 'failed', 'cancelled', 'expired'].includes(task.state)
  // The third answer to a supervised "more work remains" park: approve would
  // accept the half-finished work, reject would throw it away — continue
  // spends a fresh round budget on finishing it, in the same agent session.
  const needsFollowup =
    task.state === 'review' && parseResult(task.result ?? '')?.needs_followup === true
  // A needs_changed_input park carries its failure in the review event, not in
  // result (no work was produced, so there is nothing to show there). Lift it
  // out of the timeline: a disabled Approve with no visible cause reads as a
  // broken button — the user clicks it, nothing happens, and the reason sits
  // hundreds of pixels below the fold.
  const blocked = task.state === 'review' && task.approval_disposition === 'needs_changed_input'
  // Read for every review state, not only the blocked banner: the verdict
  // block falls back to it when the result carries no reason of its own.
  const parkReason = task.state === 'review' ? reviewReason(task.events ?? []) : ''
  // Whether the result carries anything worth a section. A `{"failed": …}`
  // blob has already been read out in the verdict block, and the log's own
  // raw toggle still reaches it.
  const showResult = Boolean(task.result) && resultHasBody(task.result!)
  // Computed once: the verdict sentence is also the lead line of the output
  // when a run reported its outcome as prose, and printing it twice — once as
  // the answer, once as the first line of the panel — is the page's oldest
  // habit. ResultView drops it from the body when they match.
  const verdict = verdictOf(task, parkReason)

  return (
    <section class="dt">
      <button class="dt-back" onClick={onBack}>
        <Icon name="arrow-left" size={14} /> {t('detail.back')}
      </button>

      <header class="dt-head">
        <div class="dt-head-text">
          <h1 class="dt-title">{task.title || task.id}</h1>
          {/* Identity only: state, scope and the three facts that name the run.
              The timestamps, elapsed time, tokens and cost moved into the
              verdict block below — on one line with the rest they were seven
              grey tokens of equal weight, and the reader had to scan all of
              them to find the two that mattered. */}
          <div class="dt-meta">
            <StateBadge state={task.state} />
            {task.isolate && (
              <span class="badge blue" title={t('detail.isolateHint')}>{t('detail.isolated')}</span>
            )}
            {!task.isolate && task.scope_paths && task.scope_paths.length > 0 && (
              <span class="badge dim" title={task.scope_paths.join('\n')}>
                {t('detail.scoped')}: {task.scope_paths.join(', ')}
              </span>
            )}
            <span class="dt-meta-item mono" title={task.id}>
              #{shortId(task.id)}
            </span>
            {task.project && <span class="dt-meta-item">{task.project}</span>}
            {task.owner && <span class="dt-meta-item">{task.owner}</span>}
            {task.plan_meta?.plan_id && (
              <button
                type="button"
                class="dt-meta-link"
                onClick={() => navigate({ view: 'control', tab: 'plans' })}
              >
                {t('orbit.actions.openPlan')} →
              </button>
            )}
          </div>
        </div>

        <div class="dt-actions">
          {task.session_id && onOpenSession && (
            <button
              type="button"
              class="btn primary"
              onClick={() => onOpenSession(task.session_id!)}
            >
              <Icon name="message" size={13} /> {t('detail.openSession')}
            </button>
          )}
          {task.state === 'review' && task.approval_disposition && (
            <span class="dim review-kind">{t(`detail.reviewKind.${task.approval_disposition}`)}</span>
          )}
          {task.state === 'review' && !rejecting && (
            <>
              {needsFollowup && (
                <button
                  class="btn"
                  disabled={busy}
                  title={t('detail.continueHint')}
                  onClick={() =>
                    act(async () => {
                      const out = await api.continueTask(task.id)
                      toast(out.warning || t('detail.continuedToast'), out.warning ? 'info' : 'success')
                    })
                  }
                >
                  {t('detail.continue')}
                </button>
              )}
              <ScopeSelect value={scope} onChange={setScope} disabled={busy} />
              <button
                class="btn primary"
                disabled={busy || task.approval_disposition === 'needs_changed_input'}
                title={blocked ? t('detail.needsChangedInputHint') : undefined}
                onClick={() => act(() => api.approve(task.id, scope), t('detail.approvedToast'))}
              >
                {t('detail.approve')}
              </button>
              <button class="btn danger" disabled={busy} onClick={() => setRejecting(true)}>
                {t('detail.reject')}
              </button>
            </>
          )}
          {task.state === 'review' && rejecting && (
            <div class="reject-form">
              <input
                class="input"
                type="text"
                placeholder={t('detail.rejectReason')}
                value={rejectReason}
                onInput={(e) => setRejectReason((e.target as HTMLInputElement).value)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter') {
                    act(() =>
                      api.reject(task.id, rejectReason.trim() || t('detail.rejectedViaWeb'), scope),
                    ).then(() => setRejecting(false))
                  }
                  if (e.key === 'Escape') setRejecting(false)
                }}
              />
              <ScopeSelect value={scope} onChange={setScope} disabled={busy} />
              <button
                class="btn danger"
                disabled={busy}
                onClick={() =>
                  act(() =>
                    api.reject(task.id, rejectReason.trim() || t('detail.rejectedViaWeb'), scope),
                  ).then(() => setRejecting(false))
                }
              >
                {t('detail.rejectConfirm')}
              </button>
              <button class="btn" disabled={busy} onClick={() => setRejecting(false)}>
                {t('common.cancel')}
              </button>
            </div>
          )}
          {cancellable && (
            <button class="btn" disabled={busy} onClick={cancelTask}>
              {t('detail.cancelTask')}
            </button>
          )}
          {!cancellable && task.state !== 'expired' && (
            <button class="btn danger" disabled={busy} onClick={revertTask}>
              {t('detail.revert')}
            </button>
          )}
        </div>
      </header>

      {blocked && (
        // Why Approve cannot be clicked, and what to do instead. The button
        // stays disabled on purpose (consent cannot fix a deterministic
        // failure) — so the page owes the reader the instruction, not just a
        // greyed-out control. The failure itself now lives in the verdict
        // block, so it is not printed a second time here.
        <div class="dt-blocked">
          <div class="dt-blocked-head">
            <span class="badge yellow">{t('detail.reviewKind.needs_changed_input')}</span>
            <span class="dim">{t('detail.needsChangedInputHint')}</span>
          </div>
        </div>
      )}

      {/* What happened, first. Everything below this block is the evidence
          for its one sentence. */}
      <VerdictBlock verdict={verdict} />

      <TaskTrace task={task} />

      {cancellable && (
        // Send into the task while it runs: queue leaves the round alone
        // (the note is read as context at the next one), inject steers it
        // (the in-flight round aborts and the text leads the next one).
        <div class="dt-msg">
          <input
            class="input"
            type="text"
            placeholder={t('detail.msgPlaceholder')}
            value={msgText}
            onInput={(e) => setMsgText((e.target as HTMLInputElement).value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter') void sendMessage(false)
            }}
          />
          <button class="btn" disabled={busy || !msgText.trim()} onClick={() => sendMessage(false)}>
            {t('detail.msgQueue')}
          </button>
          <button
            class="btn primary"
            disabled={busy || !msgText.trim()}
            onClick={() => sendMessage(true)}
          >
            {t('detail.msgInject')}
          </button>
          <span class="dim dt-msg-hint">{t('detail.msgHint')}</span>
        </div>
      )}

      <div class="dt-body">
        {isTaskStalled(task) && (
          // The row has not moved past its own bound — nothing is consuming,
          // expiring, or answering for it. Surface that instead of letting the
          // open state sit silently.
          <section class="dt-sec">
            <div class="dt-note">
              <span class="badge yellow">{t('task.stalled')}</span>
              <span class="dim">{t('task.stalledHint')}</span>
            </div>
          </section>
        )}

        {showResult && <ResultView raw={task.result!} lead={verdict.text} />}

        {/* What the run was asked to produce — target, guardrails, scope and
            the bar the supervisor judges against. Without this section the
            spec only existed as a JSON blob users had to decode. Folded by
            default: it is the task's contract, not its outcome, and read open
            it pushed the result a third of a screen down. */}
        {/* A self-merge proposal's "spec" IS the patch set — the review the
            approve button answers is this diff, so it renders ahead of the
            generic spec card. */}
        <SelfMergeCard spec={task.spec} />

        <SpecCard spec={task.spec} />

        {board && board.messages.length > 0 && (
          // The board scope is where queued user notes, steers and sibling
          // posts land — reading it back is what makes "queued" a state the
          // user can see rather than a toast that forgot itself.
          <section class="dt-sec">
            <h2 class="dt-sec-title">{t('detail.board.title')}</h2>
            <ul class="dt-board">
              {board.messages.map((m, i) => (
                <li key={`${m.sender}-${m.sender_seq}-${i}`} class="dt-board-row">
                  <span class="dt-board-sender">
                    {m.sender === 'user' || m.sender.startsWith('user:')
                      ? t('detail.board.you')
                      : m.sender_label || m.sender}
                  </span>
                  {m.grade !== 'status' && (
                    <span class={`badge ${m.grade === 'steer' ? 'blue' : 'dim'}`}>{m.grade}</span>
                  )}
                  {/* Delivery state, on the user's own notes only: a note lands
                      on the board the moment it is posted, so without this the
                      row reads "queued" forever and "did the agent see it?"
                      has no answer on screen. consumed_at is stamped by the
                      round that folded the note into its prompt. */}
                  {(m.sender === 'user' || m.sender.startsWith('user:')) &&
                    (m.consumed_at ? (
                      <span class="badge dim">{t('detail.board.read')}</span>
                    ) : (
                      <span class="badge yellow">{t('detail.board.pending')}</span>
                    ))}
                  <span class="dt-board-body">{m.body}</span>
                  <span class="dim dt-board-time">
                    {m.sent_at ? fmtShort(new Date(m.sent_at * 1000).toISOString()) : ''}
                  </span>
                </li>
              ))}
            </ul>
          </section>
        )}

        {/* Structured delegation chain from the ledger: each hop is one leg of
            the task's trip across nodes (originator → executor → …). Unacked
            hops render hollow — the outbound leg fired but no node confirmed. */}
        {task.delegation_chain && task.delegation_chain.length > 0 && (
          <section class="dt-sec">
            <h2 class="dt-sec-title">{t('detail.chain')}</h2>
            <div class="dt-chain">
              <span class="chain-node root">{task.delegation_chain[0]?.from_node}</span>
              {task.delegation_chain.map((hop, i) => (
                <span key={i} style="display:contents">
                  <span class="chain-arrow" aria-hidden="true">→</span>
                  <span
                    class={`chain-node${hop.accepted ? '' : ' unacked'}`}
                    title={`hop ${hop.hop}${hop.via ? ` · ${hop.via}` : ''} · ${fmtFull(new Date(hop.ts * 1000).toISOString())}`}
                  >
                    {hop.to_node}
                  </span>
                </span>
              ))}
            </div>
          </section>
        )}

        {/* Sub-agent tree: the children this task is orchestrating, with live
            states and each child's parallel mode. This is the "parent fanned
            out to N sub-agents" view — without it the children only existed
            as separate board cards. */}
        {task.children && task.children.length > 0 && (
          <section class="dt-sec">
            <h2 class="dt-sec-title">{t('detail.children', { n: String(task.children.length) })}</h2>
            <div class="dt-children">
              {task.children.map((c) => (
                <button
                  key={c.id}
                  type="button"
                  class="dt-child"
                  onClick={() => onOpenTask?.(c.id)}
                  title={c.id}
                >
                  <StateBadge state={c.state} />
                  <span class="dt-child-title">{c.title || c.id}</span>
                </button>
              ))}
            </div>
          </section>
        )}

        <InfoSection task={task} onOpenTask={onOpenTask} />

        <TimelineSection events={task.events ?? []} suppressResult={showResult} />
      </div>
    </section>
  )
}

// —---------------------------------------------------------------------------
// Verdict — the one block the page owes the reader first.
//
// The old page opened on a facts table: five columns of grey UUIDs, then a
// trace strip, then a result section whose useful line was a 300-character
// error string. A reader arriving from the queue — usually because something
// failed — had to scan all of it to answer "did it work, and why not".
//
// This block answers that in one sentence, then lists what the answer cost
// (elapsed, rounds, tokens, money). It deliberately does NOT repeat the state
// word: the header badge already says 失败, and printing it twice was one of
// the ways the page said the same thing in three places.
// —---------------------------------------------------------------------------

type VerdictTone = 'ok' | 'bad' | 'warn' | 'info'

interface Verdict {
  tone: VerdictTone
  /** The sentence that answers "what happened". */
  text: string
  /** Cost of finding out: elapsed, rounds, tokens, money. */
  metrics: Array<[string, string]>
}

const OPEN_STATES = ['submitted', 'queued', 'dispatched', 'running', 'waiting_context']

function verdictOf(task: Task, fallbackReason: string): Verdict {
  const r = parseResult(task.result ?? '') ?? {}
  const failure = text(r.failed) || text(r.error)
  const settledBad = task.state === 'failed' || task.state === 'cancelled' || task.state === 'expired'
  const open = OPEN_STATES.includes(task.state)

  // The reason, in priority order: what the run itself reported, then the
  // review event that parked it. A cancelled run never produced one.
  let reason = failure
  if (!reason && task.state === 'review') reason = fallbackReason
  if (!reason) reason = text(r.verdict_reason)
  if (!reason && task.state === 'done') reason = firstLine(text(r.stdout)) || text(r.verdict)
  if (!reason) reason = open ? t('detail.verdict.empty') : t('detail.verdict.noDetail')

  const tone: VerdictTone = settledBad ? 'bad' : task.state === 'review' ? 'warn' : open ? 'info' : 'ok'

  const metrics: Array<[string, string]> = []
  const elapsed = elapsedLabel(task.created_at, task.updated_at, open)
  if (elapsed) metrics.push([t('detail.verdict.elapsed'), elapsed])
  if (task.supervision?.round !== undefined) {
    metrics.push([
      t('detail.verdict.rounds'),
      `${task.supervision.round}/${task.supervision.budget ?? '?'}`,
    ])
  }
  if (typeof r.tokens === 'number' && r.tokens > 0) {
    metrics.push([t('detail.verdict.tokens'), String(r.tokens)])
  }
  if (typeof r.cost === 'number' && r.cost > 0) {
    metrics.push([t('detail.verdict.cost'), '$' + r.cost.toFixed(4)])
  }

  return { tone, text: reason || t('detail.verdict.empty'), metrics }
}

/** Long reasons — an endpoint timeout plus a fallback's 429 is 300 characters
 *  — get three lines and a way to read the rest. Clamping the text is the
 *  point of the block: a wall of error is what it replaced. */
const VERDICT_CLAMP = 200

function VerdictBlock({ verdict }: { verdict: Verdict }) {
  const [open, setOpen] = useState(false)
  const v = verdict
  const long = v.text.length > VERDICT_CLAMP
  const shown = open || !long ? v.text : v.text.slice(0, VERDICT_CLAMP) + '…'
  return (
    <section class={`dt-verdict is-${v.tone}`}>
      <div class="dt-verdict-main">
        <span class="dt-verdict-dot" aria-hidden="true" />
        <p class="dt-verdict-text">{shown}</p>
      </div>
      {long && (
        <button type="button" class="dt-verdict-more" onClick={() => setOpen((o) => !o)}>
          {open ? t('events.collapse') : t('detail.verdict.more')}
        </button>
      )}
      {v.metrics.length > 0 && (
        <dl class="dt-verdict-metrics">
          {v.metrics.map(([label, value]) => (
            <div key={label} class="dt-verdict-metric">
              <dt>{label}</dt>
              <dd>{value}</dd>
            </div>
          ))}
        </dl>
      )}
    </section>
  )
}

/** First line of a multi-line output, shortened to something a sentence can
 *  carry. Used when a successful run has no verdict of its own. */
function firstLine(s: string): string {
  const line = s.trim().split('\n')[0]?.trim() ?? ''
  return line.length > 160 ? line.slice(0, 160) + '…' : line
}

/** Drops a leading sentence the page has already printed elsewhere — the
 *  verdict block quotes a done run's opening line, and the output panel then
 *  opened on the same words. Exact prefix only, and only when a real body
 *  remains: trimming a 12-character remainder would leave the panel emptier
 *  than it started. */
function stripLead(body: string, lead?: string): string {
  if (!lead) return body
  const trimmed = body.trimStart()
  if (!trimmed.startsWith(lead)) return body
  const rest = trimmed.slice(lead.length).replace(/^[\s。.!！]+/, '')
  return rest.length > 40 ? rest : body
}

/** SelfMergeCard renders a self-evolution merge proposal: the signed patch
 *  set's manifest (producer, base, target version, the RECOMPUTED critical
 *  flag) plus each patch file's diff behind an expander — the artifact the
 *  reviewer's approve/reject actually decides on. Only renders for tasks
 *  whose spec carries the self_merge envelope. */
function SelfMergeCard({ spec }: { spec?: string }) {
  const env = spec ? parseResult(spec) : null
  const setID = env && env.kind === 'self_merge' ? String(env.patchset || '') : ''
  const [open, setOpen] = useState(false)
  const [patchOpen, setPatchOpen] = useState<Record<string, boolean>>({})
  const { data: set } = useAsync(() => (setID ? api.selfEvoSet(setID) : Promise.resolve(null)), [setID])
  if (!setID || !set) return null
  return (
    <section class="dt-sec">
      <div class="dt-sec-head">
        <h2 class="dt-sec-title">
          {t('detail.selfmerge.title')}
          {set.critical && <span class="badge red">{t('detail.selfmerge.critical')}</span>}
        </h2>
        <span class="dt-sec-hint">
          {set.files.length} {t('detail.selfmerge.patches')}
          {set.gate_ok ? ` · ${t('detail.selfmerge.gateOk')}` : ` · ${t('detail.selfmerge.gateNone')}`}
        </span>
        <button type="button" class="dt-sec-action" onClick={() => setOpen((v) => !v)}>
          {open ? t('events.collapse') : t('detail.spec.expand')}
        </button>
      </div>

      <dl class="dt-spec-foot">
        <dt>{t('detail.selfmerge.producer')}</dt>
        <dd class="mono">{set.producer}</dd>
        <dt>{t('detail.selfmerge.base')}</dt>
        <dd class="mono">{set.base}</dd>
        <dt>{t('detail.selfmerge.version')}</dt>
        <dd class="mono">{set.version}</dd>
      </dl>
      {set.critical && set.critical_paths && set.critical_paths.length > 0 && (
        <p class="gate-error">
          {t('detail.selfmerge.critical')}: <span class="mono">{set.critical_paths.join(', ')}</span>
        </p>
      )}

      {open && (
        <ul class="diff-files">
          {set.files.map((f) => (
            <li key={f}>
              <button
                type="button"
                class="dt-sec-action"
                onClick={() => setPatchOpen((m) => ({ ...m, [f]: !m[f] }))}
              >
                {patchOpen[f] ? '▾' : '▸'}
              </button>
              <span class="mono">{f}</span>
              {patchOpen[f] && <SelfMergePatch id={set.id} name={f} />}
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}

/** SelfMergePatch lazy-loads one patch file's unified diff. */
function SelfMergePatch({ id, name }: { id: string; name: string }) {
  const { data, error } = useAsync(() => api.selfEvoFile(id, name), [id, name])
  if (error) return <p class="gate-error">{String(error)}</p>
  if (!data) return <p class="dim">…</p>
  return <DiffViewer patch={data} />
}

/** Whether a result blob carries anything beyond its outcome. The page uses
 *  this to decide if a 执行结果 section is worth drawing at all: a run that
 *  only reported `{"failed": …}` has already been read out in the verdict
 *  block, and an empty section under a title reads as a broken panel. */
function resultHasBody(raw: string): boolean {
  const r = parseResult(raw)
  if (!r) return true
  if (typeof r.exit_code === 'number' || r.injected === true) return true
  if (text(r.agent) || text(r.model) || text(r.executor) || text(r.stdout) || text(r.stderr)) {
    return true
  }
  return Array.isArray(r.files_changed)
    ? r.files_changed.some((f) => typeof f === 'string' && f !== '' && !FILE_NOISE.test(f))
    : false
}

/** A task result is a JSON blob with a handful of well-known keys — ok,
 *  exit_code, stdout, stderr, agent, plus whatever the supervisor added.
 *  Rendering the blob itself is what made this page read as a wall of JSON:
 *  the useful parts were in there, buried in escaping. This lifts them out,
 *  renders the agent's markdown the way it was written, and keeps the
 *  original one click away rather than one keypress of devtools.
 *
 *  What is left here is the *output*, not the verdict: the outcome sentence,
 *  the tokens and the money moved up into the verdict block. Repeating them
 *  here was the page's habit of saying one thing in three places.
 *
 *  Anything that is not an object we recognise falls back to the raw text —
 *  a half-understood result shown verbatim beats a blank panel. */
function ResultView({ raw, lead }: { raw: string; lead?: string }) {
  const [rawOpen, setRawOpen] = useState(false)
  const r = parseResult(raw)
  if (!r) return <RawResult raw={raw} />

  const exit = typeof r.exit_code === 'number' ? r.exit_code : null
  const agent = text(r.agent)
  const model = text(r.model)
  const executor = text(r.executor)
  const injected = r.injected === true
  // The verdict block above already reads out the run's opening sentence
  // (a done run's verdict *is* the first line of its output). Dropping that
  // one line here is what stops the page saying the same sentence twice, ten
  // pixels apart.
  const stdout = stripLead(text(r.stdout), lead)
  const stderr = text(r.stderr)
  const files = Array.isArray(r.files_changed)
    ? r.files_changed
        .filter((f): f is string => typeof f === 'string' && f !== '')
        .filter((f) => !FILE_NOISE.test(f))
    : []

  // Chips rather than a sentence: the old meta line was three grey phrases of
  // equal weight sitting one line above a label that looked exactly like them,
  // so the section opened on three lines nobody could rank. A chip names its
  // own field, and the exit code carries the tone — a zero is the one fact
  // that answers "did it work" before anything is read.
  const chips: Array<{ text: string; tone?: 'ok' | 'bad'; mono?: boolean }> = []
  if (exit !== null) {
    chips.push({
      text: t('detail.result.exit', { n: String(exit) }),
      tone: exit === 0 ? 'ok' : 'bad',
      mono: true,
    })
  }
  if (agent) chips.push({ text: t('detail.result.agent', { agent }) })
  if (model) chips.push({ text: t('detail.result.model', { model }), mono: true })
  if (injected) chips.push({ text: t('detail.result.injected') })
  if (executor) chips.push({ text: t('detail.result.node', { node: executor }), mono: true })

  // A result that carried nothing but its failure (`{"failed": "..."}`) has
  // already been read out in the verdict block. Rendering an empty section
  // titled 执行结果 with one lone "原始 JSON" toggle under it read as a broken
  // panel — the blob is still reachable from the log's own raw toggle.
  if (chips.length === 0 && files.length === 0 && !stdout && !stderr) return null

  return (
    <section class="dt-sec">
      {/* The raw blob used to trail the section as a bulleted "▸ 原始 JSON"
          line under a full-width button. It is an escape hatch, not content:
          it belongs on the section's action rail, where every other section
          keeps its secondary control. */}
      <div class="dt-sec-head">
        <h2 class="dt-sec-title">{t('detail.result')}</h2>
        <button
          type="button"
          class="dt-sec-action"
          aria-expanded={rawOpen}
          onClick={() => setRawOpen((v) => !v)}
        >
          {rawOpen ? t('events.collapse') : t('detail.result.raw')}
        </button>
      </div>

      <div class="result-view">
        {chips.length > 0 && (
          <div class="result-chips">
            {chips.map((c) => (
              <span
                key={c.text}
                class={`result-chip${c.tone ? ` is-${c.tone}` : ''}${c.mono ? ' mono' : ''}`}
                title={c.text}
              >
                {c.text}
              </span>
            ))}
          </div>
        )}

        {files.length > 0 && (
          <div class="result-files">
            <span class="dim result-files-label">
              {t('detail.result.files', { n: String(files.length) })}
            </span>
            <FileChips files={files} />
          </div>
        )}

        {stdout && <CollapsibleOutput text={stdout} label={t('detail.result.stdout')} />}
        {stderr && <CollapsibleOutput text={stderr} label={t('detail.result.stderr')} error />}

        {rawOpen && <pre class="detail-raw">{prettifyJson(raw)}</pre>}
      </div>
    </section>
  )
}

/** A result that is not a JSON object: shown verbatim rather than dropped —
 *  a half-understood blob beats a blank panel. */
function RawResult({ raw }: { raw: string }) {
  return (
    <section class="dt-sec">
      <h2 class="dt-sec-title">{t('detail.result')}</h2>
      <pre class="detail-raw detail-raw-capped">{raw}</pre>
    </section>
  )
}

/** VCS internals, dependency trees, build output and agent-session scratch
 *  files a task's file list picks up along the way — real on disk, noise on
 *  the card. `.claude` is the adapter's own transcript/snapshot directory:
 *  one run listed thirteen of its files as "changed", none of them the
 *  user's. `.codebuddy` and `.codex` are the same thing under the other two
 *  CLIs — one task's "4 files changed" was three of their logs plus a
 *  switcher's sqlite. */
const FILE_NOISE =
  /(?:^|\/|\\)(?:\.git|\.svn|\.hg|node_modules|dist|build|__pycache__|\.next|\.cache|\.claude|\.codebuddy|\.codex)(?:\/|\\|$)|\.DS_Store$/

const FILE_CHIP_CAP = 12

/** Changed-file chips: capped so a large change set reads as a summary
 *  instead of a wall, with the rest one click away. */
function FileChips({ files }: { files: string[] }) {
  const [open, setOpen] = useState(false)
  const shown = open ? files : files.slice(0, FILE_CHIP_CAP)
  const hidden = files.length - shown.length
  return (
    <div class="result-file-list">
      {shown.map((f, i) => (
        <code key={i} class="result-file-chip" title={f}>
          {f}
        </code>
      ))}
      {hidden > 0 && (
        <button type="button" class="result-file-more" onClick={() => setOpen(true)}>
          {t('detail.result.filesMore', { n: String(hidden) })}
        </button>
      )}
    </div>
  )
}

/** Agent output reads as markdown (the adapters report it that way) and lives
 *  in a panel of its own: label, size and copy in a head bar, the prose in a
 *  body, the fold as a footer band.
 *
 *  It used to be a bare label over a wall of text, clamped at 600 characters
 *  — which cut mid-word — with a full-width bordered button under it that
 *  read as a stray input field. The clamp is a height now, so the last line
 *  fades instead of breaking, and the fold is a band that says how much is
 *  behind it. */
const OUTPUT_CLAMP_PX = 300

function CollapsibleOutput({
  text,
  label,
  error = false,
}: {
  text: string
  label: string
  error?: boolean
}) {
  const [open, setOpen] = useState(false)
  const [copied, setCopied] = useState(false)
  // Short outputs are shown whole — folding a four-line report costs a click
  // and saves nothing.
  const long = text.length > 700
  const clamped = long && !open

  async function copy() {
    try {
      await navigator.clipboard.writeText(text)
      setCopied(true)
      setTimeout(() => setCopied(false), 1600)
    } catch {
      // Clipboard denied (insecure origin, permission): the text is selectable
      // and a failed copy needs no error of its own.
    }
  }

  return (
    <div class={`result-panel${error ? ' is-error' : ''}`}>
      <div class="result-panel-head">
        <span class="result-panel-label">{label}</span>
        <span class="result-panel-size">
          {t('detail.result.chars', { n: text.length.toLocaleString() })}
        </span>
        <button
          type="button"
          class="result-panel-copy"
          onClick={copy}
          aria-label={t('md.copy')}
          title={t('md.copy')}
        >
          {copied ? `✓ ${t('md.copied')}` : t('md.copy')}
        </button>
      </div>

      <div
        class={'result-panel-body' + (clamped ? ' is-clamped' : '')}
        style={clamped ? `max-height:${OUTPUT_CLAMP_PX}px` : undefined}
      >
        {error ? (
          <pre class="result-err">{text}</pre>
        ) : (
          <Markdown text={text} />
        )}
      </div>

      {long && (
        <button
          type="button"
          class="result-panel-more"
          aria-expanded={open}
          onClick={() => setOpen((v) => !v)}
        >
          <span>{open ? t('detail.result.collapse') : t('detail.result.expand')}</span>
          <Icon name="chevron-down" size={12} />
        </button>
      )}
    </div>
  )
}

/** The failure that parked a task in review: the newest review event carrying
 *  a `reason`. Rejections log `rejected` instead and are skipped, so a task
 *  that was rejected once and parked again still shows the live reason. */
function reviewReason(events: { type: string; data?: string }[]): string {
  for (let i = events.length - 1; i >= 0; i--) {
    const ev = events[i]
    if (!ev || ev.type !== 'review') continue
    const r = text(parseResult(ev.data ?? '')?.reason)
    if (r) return r
  }
  return ''
}

/** Wall-clock the run held the queue. Only for settled tasks: an open one
 *  would read as a duration and then keep growing while the page sits. */
function elapsedLabel(created: string, updated: string, open: boolean): string {
  if (open || !created || !updated) return ''
  const ms = new Date(updated).getTime() - new Date(created).getTime()
  if (!Number.isFinite(ms) || ms <= 0) return ''
  return formatDuration(ms)
}
