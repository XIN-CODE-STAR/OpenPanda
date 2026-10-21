// SPDX-License-Identifier: AGPL-3.0-or-later

import { useEffect, useRef, useState } from 'preact/hooks'
import { api, type Task } from '../api/client'
import { useAsync, useChangeSignal } from '../hooks'
import { t } from '../i18n'
import { layerAboveOwnsEscape } from './card-dismiss'
import { confirmDialog } from './confirm'
import { Icon } from './icons'
import DecisionOrbit from './orbit'
import { StateBadge } from './state-badge'
import { bindStripDrag } from './strip-drag'
import { fmtElapsed, isActiveState } from './task-activity'
import { toast, toastError } from './toast'

/** The tasks this conversation started — the card behind the header's task
 *  button.
 *
 *  Same shell as the node card beside it: ghost icon button, card hanging off
 *  the bar, the button toggles it and Escape closes it, and the pane reserves a
 *  lane on the right while it is up. The two answer the same kind of question
 *  about a turn — the node card says *where* it runs, this one says *what it
 *  became* — and both open in place so a running turn can be watched without
 *  leaving the transcript. Neither closes on an outside click: reading the
 *  thread under the card is the point, not a dismissal.
 *
 *  Three strips: work that is moving (进行中), work parked on the human
 *  (待审核), and the archive (已完成 — done, failed and cancelled all landed
 *  somewhere, and none of them wants a decision). The card is tall enough for
 *  all three to be on screen at once, so the state of the thread is one glance
 *  rather than one scroll. A row's own actions (取消 / 执行) sit on the task,
 *  not behind a detail trip.
 *
 *  The card is a task list first. It used to carry a DecisionOrbit rail above
 *  the strips, always pointed at the newest task whether or not that was the
 *  task you were reading; the rail said "how is this turn going" before you had
 *  asked. It is gone from there. Picking a task now opens the trace in a card
 *  of its own, to the *left* of this one: the list says what the thread
 *  produced, the card beside it says how that one task was classified, routed
 *  and run. Two cards, one selection — the trace never describes a task the
 *  list is not pointing at.
 *
 *  The trace is a sibling rather than a section inside this card because it is
 *  read in a different direction: the list is scanned top-down (and a strip
 *  that outgrows its four tiles continues downwards), the trace is a
 *  three-step column. Stacked in one card, the
 *  steps either pushed the strips down or had to live in a scroller inside a
 *  scroller. Side by side each keeps its own height and its own scroll, and the
 *  8px gap is the whole relationship. Narrow panes wrap the trace back under
 *  the list — it never floats over the transcript on a phone.
 *
 *  The card reads top-down in the order the questions come: what has this
 *  thread produced (strips), what is in this one task (detail). Every section
 *  keeps its full height whether or not it has rows in it (see
 *  `.orch-sec-slot`), so the three strips read as one table and the card does
 *  not jump when a task arrives or leaves.
 *
 *  A task opens *under the strips*, in the same card, and the strips stay where
 *  they are: the detail is a reading pane, not a page. Trading the rows for a
 *  detail view meant a back arrow for every look, and the whole point of
 *  reading tasks from here is that nothing has to be left behind. Clicking the
 *  same task again (or the × on the detail) puts both panes away. */
export function OrchestrateCard(props: {
  /** The thread whose tasks are shown; null before a new chat is saved. */
  sessionId?: string | null
  /** The other scope this card can answer for: a work directory. 工作空间
   *  dispatches into a directory rather than into a thread — it never shows a
   *  transcript, so "what has this directory produced" is the only question
   *  its header can usefully answer. Ignored when `sessionId` is set, so the
   *  chat's card keeps its exact behaviour. */
  project?: string
  /** Controlled by the pane: this card and the node card hang off the same
   *  corner, so opening one has to close the other. */
  open: boolean
  onOpenChange(open: boolean): void
  /** Node-directory facts for the trace's Step 2: how many machines can take
   *  this work decides whether it lists candidates or offers the "add a second
   *  device" CTA. Optional so the card still renders before the fleet loads. */
  onlineNodeCount?: number
  selfNodeId?: string
}) {
  const [picked, setPicked] = useState<string | null>(null)
  // Local nudge after an action: the shared change feed usually arrives a beat
  // later, and the row the user just cancelled should not sit there looking
  // alive while they wait for it.
  const [bump, setBump] = useState(0)
  const [busy, setBusy] = useState('')
  const tick = useChangeSignal()
  const box = useRef<HTMLDivElement>(null)

  // Fetch only while the card is up: closed, there is nothing to paint, and
  // the header button carries no count that would need the data early.
  const { data, error } = useAsync(
    () => {
      if (!props.open) return Promise.resolve([] as Task[])
      if (props.sessionId) return api.tasks({ session: props.sessionId })
      // Directory scope: one read for every task filed under the project the
      // picked work dir resolves to, whatever state it is in.
      if (props.project) return api.tasks({ project: props.project })
      return Promise.resolve([] as Task[])
    },
    [props.open, props.sessionId, props.project],
    tick + bump,
  )
  const tasks = data ?? []
  const doing = tasks.filter((x) => isActiveState(x.state))
  const review = tasks.filter((x) => x.state === 'review')
  // Everything that is neither moving nor parked: done, failed, cancelled. One
  // strip, newest first — the archive is read top-down, not triaged.
  const finished = tasks
    .filter((x) => !isActiveState(x.state) && x.state !== 'review')
    .sort((a, b) => Date.parse(b.updated_at) - Date.parse(a.updated_at))

  // The newest task the thread produced: what the trace shows before anything
  // is picked, so opening the card answers "how is this turn going" without a
  // click. One selection drives everything under the strips — the trace and
  // the detail pane always describe the same task, which is what keeps the two
  // from fighting over the phrase "the task on screen".
  const newest = tasks.reduce<Task | null>(
    (a, b) => (!a || Date.parse(b.updated_at) >= Date.parse(a.updated_at) ? b : a),
    null,
  )
  const focusedId = picked ?? newest?.id ?? null

  // The full row (events, result, disposition, traces) for the focused task.
  // The list row paints immediately; this fills in behind it.
  const detail = useAsync(
    () => (focusedId ? api.task(focusedId) : Promise.resolve(null)),
    [focusedId],
    tick + bump,
  )
  const focused = focusedId
    ? detail.data && detail.data.id === focusedId
      ? detail.data
      : tasks.find((x) => x.id === focusedId) ?? null
    : null

  // Reopening starts at the top of the card: nothing picked — not with
  // whatever was open last time.
  useEffect(() => {
    if (props.open) return
    setPicked(null)
  }, [props.open])

  // Escape puts the detail away first, so one key undoes one step. It is the
  // card's only way out besides the header trigger: an outside click used to
  // close it too, which meant every click on a message — to select a line,
  // copy a path, follow a link — folded the card mid-read. The card is a
  // reading companion, not a menu; it stays until the button is pressed again.
  useEffect(() => {
    if (!props.open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape' || layerAboveOwnsEscape()) return
      e.stopPropagation()
      if (picked) setPicked(null)
      else props.onOpenChange(false)
    }
    document.addEventListener('keydown', onKey, true)
    return () => {
      document.removeEventListener('keydown', onKey, true)
    }
  }, [props.open, picked])

  /** Cancel or approve one task from the row that shows it. Both are
   *  destructive-ish, so cancel asks first (the run is killed); approve does
   *  not (it is the action the review row exists for). */
  async function act(task: Task, kind: 'cancel' | 'run') {
    if (kind === 'cancel') {
      const ok = await confirmDialog({
        title: t('detail.cancelConfirmTitle'),
        message: t('detail.cancelConfirmMsg'),
        confirmLabel: t('detail.cancelTask'),
        danger: true,
      })
      if (!ok) return
    }
    setBusy(`${task.id}:${kind}`)
    try {
      if (kind === 'cancel') {
        await api.cancel(task.id)
        toast(t('detail.cancelledToast'), 'success')
      } else {
        await api.approve(task.id)
        toast(t('detail.approvedToast'), 'success')
      }
      setBump((v) => v + 1)
    } catch (e) {
      toastError(e)
    } finally {
      setBusy('')
    }
  }

  const chipProps = (task: Task) => ({
    task,
    busy: busy.startsWith(`${task.id}:`),
    picked: picked === task.id,
    // Toggling on the same chip closes the pane: with the strips still up, the
    // chip is both the way in and the way out.
    onOpen: () => {
      setPicked((cur) => (cur === task.id ? null : task.id))
    },
    onCancel: () => void act(task, 'cancel'),
    onRun: () => void act(task, 'run'),
  })

  return (
    <div class="orch-picker" ref={box}>
      <button
        type="button"
        class={`icon-btn tip-down orch-pick-btn${props.open ? ' open' : ''}`}
        /* The tip is the control's name; the card behind it is the answer. */
        data-tip={props.open ? undefined : t('sessions.orchestrate')}
        aria-label={t('sessions.orchestrate')}
        aria-haspopup="dialog"
        aria-expanded={props.open}
        onClick={() => props.onOpenChange(!props.open)}
      >
        <Icon name="route" size={19} />
      </button>
      {props.open && (
        /* One row of cards hanging off the bar: the picked task on the left,
           the list on the right. The row is the positioning context — each card
           keeps its own height and scroll, and the gap between them is the only
           thing tying the two together. */
        <div class="orch-flyout">
          <div
            class="orch-panel"
            role="dialog"
            /* The visible label is the card's name; "任务" alone would say less
               than the heading two pixels under it. */
            aria-label={props.sessionId ? t('orch.title') : t('orch.projectTitle')}
          >
            {/* The strips, and nothing else: this card is the list. The picked
                task — its brief, its result and how it was routed — is read in
                the card beside it, so nothing here grows when a chip is
                clicked. */}
            <div class="orch-body">
              <p class="orch-panel-label">
                {props.sessionId ? t('orch.title') : t('orch.projectTitle')}
              </p>
              {error ? (
                <p class="orch-sec-empty">{t('common.loadFail')}</p>
              ) : !props.sessionId && !props.project ? (
                <p class="orch-sec-empty">{t('orch.noSession')}</p>
              ) : (
                <>
                  <OrchRow
                    title={t('orch.doing')}
                    tasks={doing}
                    empty={t('orch.emptyDoing')}
                    chipProps={chipProps}
                  />
                  <OrchRow
                    title={t('orch.review')}
                    tasks={review}
                    empty={t('orch.emptyReview')}
                    chipProps={chipProps}
                  />
                  <OrchRow
                    title={t('orch.done')}
                    tasks={finished}
                    empty={t('orch.emptyDone')}
                    chipProps={chipProps}
                  />
                </>
              )}
            </div>
          </div>
          {/* Second in the DOM, first on screen: the row is `row-reverse`, so
              the picked task sits to the left of the list and a narrow pane
              wraps it *under* the list rather than off the left edge. */}
          {picked && focused && (
            <OrchTask
              /* Keyed by task: switching tasks remounts the card, so its scroll
                 starts at the top and the orbit starts expanded for this task
                 rather than inheriting the previous one's state. */
              key={`task-${focused.id}`}
              task={focused}
              busy={busy.startsWith(`${focused.id}:`)}
              onlineNodeCount={props.onlineNodeCount}
              selfNodeId={props.selfNodeId}
              onClose={() => setPicked(null)}
              onCancel={() => void act(focused, 'cancel')}
              onRun={() => void act(focused, 'run')}
            />
          )}
        </div>
      )}
    </div>
  )
}

/** One strip: a label with its count, then the tasks in a two-column grid —
 *  four tiles in view (two rows of two), the rest one wheel-notch below. An
 *  empty strip keeps a tile-sized slot rather than collapsing to a line of
 *  text: the three strips are read as one table, and a section that shrank to
 *  its placeholder made the card jump every time a task arrived or left.
 *
 *  The grid scrolls itself, not the card: a thread can hold twenty finished
 *  tasks and none of them should push 待审核 off the bottom of the popover. The
 *  tiles are tall enough for a three-line title, so the four on screen say what
 *  they are; the count on the label says how many are below the fold, and the
 *  wheel and a press-drag both move the grid (see `strip-drag.ts`). */
function OrchRow(props: {
  title: string
  tasks: Task[]
  empty: string
  chipProps(task: Task): {
    task: Task
    busy: boolean
    picked: boolean
    onOpen(): void
    onCancel(): void
    onRun(): void
  }
}) {
  // Bound to the element's own lifetime, not to the task count: the strip
  // only exists in the filled branch, and rebinding on every list change
  // would tear the listeners down mid-gesture — a task arriving over SSE
  // during a drag could then drop the press and let the release click
  // through to the chip.
  const [strip, setStrip] = useState<HTMLDivElement | null>(null)
  useEffect(() => {
    if (!strip) return
    // The grid runs downwards: two tiles wide, four in view.
    return bindStripDrag(strip, 'y')
  }, [strip])
  return (
    <section class="orch-sec">
      <p class="orch-sec-head">
        <span>{props.title}</span>
        <span class="orch-sec-count">{props.tasks.length}</span>
      </p>
      {props.tasks.length === 0 ? (
        <p class="orch-sec-slot">{props.empty}</p>
      ) : (
        <div class="orch-grid" ref={setStrip}>
          {props.tasks.map((task) => (
            <OrchChip key={task.id} {...props.chipProps(task)} />
          ))}
        </div>
      )}
    </section>
  )
}

/** A task as it appears in a row: title, state, age, and its one action.
 *  The open target is a real button (not the whole tile) so the action button
 *  beside it is not a control nested inside another control. */
function OrchChip(props: {
  task: Task
  busy: boolean
  picked: boolean
  onOpen(): void
  onCancel(): void
  onRun(): void
}) {
  const active = isActiveState(props.task.state)
  const blocked = props.task.approval_disposition === 'needs_changed_input'
  return (
    <div class="orch-chip" data-state={props.task.state} data-picked={props.picked || undefined}>
      <button
        type="button"
        class="orch-chip-open"
        aria-expanded={props.picked}
        aria-controls="orch-detail"
        onClick={props.onOpen}
      >
        <span class="orch-chip-title">{props.task.title || props.task.id}</span>
        <span class="orch-chip-meta">
          <StateBadge state={props.task.state} />
          <span class="orch-chip-age">{fmtElapsed(props.task.updated_at)}</span>
        </span>
      </button>
      <div class="orch-chip-acts">
        {active && (
          <button
            type="button"
            class="orch-act"
            disabled={props.busy}
            onClick={props.onCancel}
            title={t('detail.cancelTask')}
          >
            {t('orch.cancel')}
          </button>
        )}
        {props.task.state === 'review' && (
          <button
            type="button"
            class="orch-act run"
            disabled={props.busy || blocked}
            onClick={props.onRun}
            title={blocked ? t('detail.needsChangedInputHint') : t('detail.approve')}
          >
            {t('orch.run')}
          </button>
        )}
      </div>
    </div>
  )
}

/** The picked task, in the card to the left of the list — everything about it
 *  in one place: what it is (title, state, owner, age), how it was classified,
 *  routed and run (the orbit), what it was asked to do (brief), and what came
 *  back (result), with the actions that still apply.
 *
 *  This used to be two things in two places: a trace card on the left and a
 *  detail pane stacked under the strips on the right. That split the same task
 *  across the width of the screen — you read "识别图片内容…" on the right and
 *  the steps that produced it on the left, and had to keep both in your head.
 *  One card, one task: the list on the right stays a list (it never grows when
 *  a chip is clicked), and the reading happens here.
 *
 *  The orbit starts expanded — clicking a task already said you wanted to look
 *  — and the card scrolls as a whole, so a long trace and a long result never
 *  fight over the same height. */
function OrchTask(props: {
  task: Task
  busy: boolean
  onlineNodeCount?: number
  selfNodeId?: string
  onClose(): void
  onCancel(): void
  onRun(): void
}) {
  const task = props.task
  const active = isActiveState(task.state)
  const review = task.state === 'review'
  const blocked = task.approval_disposition === 'needs_changed_input'
  const brief = task.spec || task.intent || ''
  return (
    <aside class="orch-orch-card" id="orch-detail" role="region" aria-label={t('orch.detail')}>
      <div class="orch-detail-head">
        <p class="orch-detail-title">{task.title || task.id}</p>
        <button
          type="button"
          class="icon-btn orch-detail-close"
          aria-label={t('orch.closeDetail')}
          title={t('orch.closeDetail')}
          onClick={props.onClose}
        >
          <Icon name="x" size={14} />
        </button>
      </div>
      <div class="orch-detail-meta">
        <StateBadge state={task.state} />
        {task.project && <span class="orch-detail-tag">{task.project}</span>}
        {task.owner && <span class="orch-detail-tag">@{task.owner}</span>}
        <span class="orch-detail-age">{t('time.ago', { d: fmtElapsed(task.updated_at) })}</span>
      </div>
      {review && task.approval_disposition && (
        <p class={`orch-detail-note${blocked ? ' blocked' : ''}`}>
          {t(`detail.reviewKind.${task.approval_disposition}`)}
        </p>
      )}
      {/* A task that never went through the dispatcher has no classification,
          no route and no execution trace — the orbit would be three empty steps
          under a "大总管" bar that did nothing, so the section is skipped. */}
      {hasTrace(task) && (
        <section class="orch-orch-trace" aria-label={t('orch.trace')}>
          <p class="orch-detail-label">{t('orch.trace')}</p>
          <DecisionOrbit
            task={task}
            onlineNodeCount={props.onlineNodeCount}
            selfNodeId={props.selfNodeId}
            defaultOpen
          />
        </section>
      )}
      {brief && (
        <div class="orch-detail-sec">
          <p class="orch-detail-label">{t('orch.brief')}</p>
          <p class="orch-detail-text">{brief}</p>
        </div>
      )}
      <div class="orch-detail-sec">
        <p class="orch-detail-label">{t('orch.result')}</p>
        {task.result ? (
          <pre class="orch-detail-pre">{task.result}</pre>
        ) : (
          <p class="orch-detail-text dim">{t('orch.noResult')}</p>
        )}
      </div>
      {/* Only the actions that still apply: a finished task has none, and an
          empty footer row was just a strip of padding under the result. The
          link out to the full task page is gone with it — this card already
          shows the brief, the result and the trace, so "go read it elsewhere"
          was a detour, not an action. */}
      {(active || review) && (
        <div class="orch-detail-foot">
          {active && (
            <button class="btn small" disabled={props.busy} onClick={props.onCancel}>
              {t('orch.cancel')}
            </button>
          )}
          {review && (
            <button
              class="btn primary small"
              disabled={props.busy || blocked}
              title={blocked ? t('detail.needsChangedInputHint') : undefined}
              onClick={props.onRun}
            >
              {t('orch.run')}
            </button>
          )}
        </div>
      )}
    </aside>
  )
}

/** Whether there is an orchestration story to tell for this task. A task that
 *  was dispatched straight to an agent — or is still being created — has no
 *  classification, no route decision and no execution trace, and the orbit
 *  would render three empty steps under a "大总管" bar that did nothing. */
function hasTrace(task: Task): boolean {
  return Boolean(task.plan_meta) || (task.traces?.length ?? 0) > 0 || Boolean(task.last_event)
}
