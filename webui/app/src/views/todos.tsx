// SPDX-License-Identifier: AGPL-3.0-or-later

import { useEffect, useMemo, useRef, useState } from 'preact/hooks'
import {
  api,
  ApiError,
  type Todo,
  type TodoStatus,
} from '../api/client'
import { useAsync, useChangeSignal, useLocaleRerender } from '../hooks'
import { t } from '../i18n'
import { Icon } from '../components/icons'
import { confirmDialog } from '../components/confirm'
import { toast, toastError } from '../components/toast'

/** 待办 — the human's own board.
 *
 *  This page is deliberately NOT the task queue. The queue answers "what is
 *  this node doing"; a todo answers "what did I decide to do". The two meet in
 *  exactly one place: 交给熊猫, which enqueues a task from a todo and links the
 *  rows, so a note can graduate into real work without being retyped.
 *
 *  Lanes are three, not the five states the store keeps:
 *    pending → 待处理
 *    running + paused → 进行中   (a parked todo is still in flight, and it
 *                                 wears a 已暂停 chip instead of owning a
 *                                 fourth column nobody would look at)
 *    done → 已完成
 *  `cancelled` is a decision, not a lane: it collapses into a footer strip
 *  that is one click from opening. Keeping it off the board is what lets the
 *  three lanes stay wide enough to read at a glance. */

type Lane = 'pending' | 'running' | 'done'
const LANES: Lane[] = ['pending', 'running', 'done']

/** Which lane a stored status is painted in. */
function laneOf(status: TodoStatus): Lane | 'cancelled' {
  switch (status) {
    case 'pending':
      return 'pending'
    case 'running':
    case 'paused':
      return 'running'
    case 'done':
      return 'done'
    default:
      return 'cancelled'
  }
}

/** The status a lane drop writes. Paused is not a drop target — a todo that
 *  was parked resumes as running the moment it is moved. */
const LANE_STATUS: Record<Lane, TodoStatus> = {
  pending: 'pending',
  running: 'running',
  done: 'done',
}

const PRIO_CYCLE: Array<Todo['priority']> = ['high', 'normal', 'low']

/** "today / tomorrow / 3 天前" for the card's due chip. The store keeps the
 *  deadline at the END of its local day (see parseDueAt on the server), so a
 *  todo due today is not overdue at noon. */
function dueInfo(ts: number): { label: string; tone: 'over' | 'today' | '' } | null {
  if (!ts) return null
  const now = new Date()
  const startOfToday = new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime()
  const due = new Date(ts * 1000)
  const dueDay = new Date(due.getFullYear(), due.getMonth(), due.getDate()).getTime()
  const days = Math.round((dueDay - startOfToday) / 86_400_000)
  if (days < 0) return { label: t('todos.due.overdue', { d: String(-days) }), tone: 'over' }
  if (days === 0) return { label: t('todos.due.today'), tone: 'today' }
  if (days === 1) return { label: t('todos.due.tomorrow'), tone: '' }
  return { label: `${due.getMonth() + 1}/${due.getDate()}`, tone: '' }
}

/** Unix seconds → the value a <input type="date"> wants. */
function toDateInput(ts: number): string {
  if (!ts) return ''
  const d = new Date(ts * 1000)
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`
}

export function TodosView({ onOpenTask }: { onOpenTask(id: string): void }) {
  useLocaleRerender()
  const change = useChangeSignal()
  const [refresh, setRefresh] = useState(0)
  const bump = () => setRefresh((v) => v + 1)
  const { data: todos, error } = useAsync(() => api.todos(), [], change + refresh)

  const [query, setQuery] = useState('')
  const [projectFilter, setProjectFilter] = useState('')
  const [quick, setQuick] = useState<'all' | 'today' | 'overdue' | 'undated'>('all')
  const [showCancelled, setShowCancelled] = useState(false)
  const [editing, setEditing] = useState<number | null>(null)
  const [drag, setDrag] = useState<{ id: number; over: Lane; index: number } | null>(null)

  const projects = useMemo(() => {
    const set = new Set((todos ?? []).map((x) => x.project).filter(Boolean))
    return [...set].sort()
  }, [todos])

  // Filtering is client-side: the whole board is one fetch, and a keystroke
  // should not cost a round trip.
  const visible = useMemo(() => {
    const q = query.trim().toLowerCase()
    const now = new Date()
    const startOfToday = new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime()
    return (todos ?? []).filter((x) => {
      if (projectFilter && x.project !== projectFilter) return false
      if (q) {
        const hay = `${x.title} ${x.description} ${(x.tags ?? []).join(' ')}`.toLowerCase()
        if (!hay.includes(q)) return false
      }
      if (quick !== 'all') {
        const dueDay = x.due_at
          ? new Date(new Date(x.due_at * 1000).getFullYear(), new Date(x.due_at * 1000).getMonth(), new Date(x.due_at * 1000).getDate()).getTime()
          : 0
        if (quick === 'today' && dueDay !== startOfToday) return false
        if (quick === 'overdue' && !(dueDay && dueDay < startOfToday)) return false
        if (quick === 'undated' && x.due_at !== 0) return false
      }
      return true
    })
  }, [todos, query, projectFilter, quick])

  const lanes = useMemo(() => {
    const out: Record<Lane, Todo[]> = { pending: [], running: [], done: [] }
    for (const x of visible) {
      const lane = laneOf(x.status)
      if (lane === 'cancelled') continue
      out[lane].push(x)
    }
    return out
  }, [visible])

  const cancelled = useMemo(
    () => visible.filter((x) => laneOf(x.status) === 'cancelled'),
    [visible],
  )
  const openCount = (todos ?? []).filter((x) => {
    const lane = laneOf(x.status)
    return lane === 'pending' || lane === 'running'
  }).length

  const editingTodo = useMemo(
    () => (editing == null ? null : (todos ?? []).find((x) => x.id === editing) ?? null),
    [todos, editing],
  )

  /** Move a card to another lane (and, inside the same lane, to a new slot).
   *  A cross-lane drop writes the status first; the order write then covers
   *  whatever lane the card ended up in. Both failures refresh from server
   *  truth rather than leaving the board showing a lie. */
  async function drop(lane: Lane, index: number) {
    const d = drag
    setDrag(null)
    if (!d) return
    const card = (todos ?? []).find((x) => x.id === d.id)
    if (!card) return
    try {
      if (laneOf(card.status) !== lane) {
        await api.patchTodo(card.id, { status: LANE_STATUS[lane] })
      }
      const ids = lanes[lane].map((x) => x.id).filter((id) => id !== d.id)
      ids.splice(Math.max(0, Math.min(index, ids.length)), 0, d.id)
      if (ids.length > 1) await api.reorderTodos(ids)
    } catch (e) {
      toastError(e)
    } finally {
      bump()
    }
  }

  async function tick(x: Todo) {
    const next: TodoStatus = x.status === 'done' ? 'pending' : 'done'
    try {
      await api.patchTodo(x.id, { status: next })
    } catch (e) {
      toastError(e)
    }
    bump()
  }

  async function cyclePriority(x: Todo) {
    const next = PRIO_CYCLE[(PRIO_CYCLE.indexOf(x.priority) + 1) % PRIO_CYCLE.length] ?? 'normal'
    try {
      await api.patchTodo(x.id, { priority: next })
    } catch (e) {
      toastError(e)
    }
    bump()
  }

  return (
    <section class="td-page">
      <header class="td-head">
        <div class="td-head-text">
          <h1 class="page-title">{t('nav.todos')}</h1>
          <p class="page-sub">{t('todos.subtitle')}</p>
        </div>
        {openCount > 0 && <span class="td-open-count">{t('todos.openCount', { n: String(openCount) })}</span>}
      </header>

      <Composer onCreated={bump} />

      <div class="td-toolbar">
        <div class="td-quick" role="tablist" aria-label={t('todos.filterAria')}>
          {(['all', 'today', 'overdue', 'undated'] as const).map((k) => (
            <button
              key={k}
              type="button"
              role="tab"
              aria-selected={quick === k}
              class={`td-quick-btn${quick === k ? ' on' : ''}`}
              onClick={() => setQuick(k)}
            >
              {t(`todos.filter.${k}`)}
            </button>
          ))}
        </div>
        <div class="td-toolbar-right">
          <input
            class="input td-search"
            type="search"
            placeholder={t('todos.search')}
            aria-label={t('todos.search')}
            value={query}
            onInput={(e) => setQuery((e.target as HTMLInputElement).value)}
          />
          <select
            class="input td-project-filter"
            aria-label={t('todos.projectFilter')}
            value={projectFilter}
            onChange={(e) => setProjectFilter((e.target as HTMLSelectElement).value)}
          >
            <option value="">{t('todos.allProjects')}</option>
            {projects.map((p) => (
              <option key={p} value={p}>
                {p}
              </option>
            ))}
          </select>
        </div>
      </div>

      {error && (
        <div class="td-error card">
          <p>{t('todos.loadError')}</p>
          <p class="dim">{error}</p>
          <button class="btn" onClick={bump}>
            {t('common.retry')}
          </button>
        </div>
      )}

      {todos === null && !error && <p class="dim">{t('common.loading')}</p>}

      {todos !== null && (
        <div class="td-board">
          {LANES.map((lane) => (
            <div
              key={lane}
              class={`td-lane td-lane-${lane}${drag && drag.over === lane ? ' drop-target' : ''}`}
              data-lane={lane}
              onDragOver={(e) => {
                if (!drag) return
                e.preventDefault()
                // A card under the pointer owns the index (it knows which half
                // it is on). Only the lane's empty space appends.
                if ((e.target as HTMLElement).closest('.td-card')) return
                setDrag({ ...drag, over: lane, index: lanes[lane].length })
              }}
              onDrop={(e) => {
                e.preventDefault()
                void drop(lane, drag?.over === lane ? drag.index : lanes[lane].length)
              }}
            >
              <h2 class="td-lane-head">
                <span class="td-lane-name">{t(`todos.col.${lane}`)}</span>
                <span class="td-lane-count">{lanes[lane].length}</span>
              </h2>
              <div class="td-lane-body" data-empty={lanes[lane].length === 0}>
                {lanes[lane].length === 0 && (
                  <p class="td-lane-empty">{t(`todos.empty.${lane}`)}</p>
                )}
                {lanes[lane].map((x, i) => (
                  <TodoCard
                    key={x.id}
                    todo={x}
                    dropBefore={drag != null && drag.over === lane && drag.index === i}
                    onOpen={() => setEditing(x.id)}
                    onTick={() => void tick(x)}
                    onCyclePriority={() => void cyclePriority(x)}
                    onOpenTask={onOpenTask}
                    onDragStart={() => setDrag({ id: x.id, over: lane, index: i })}
                    onDragOver={(above) =>
                      setDrag((d) => (d ? { ...d, over: lane, index: above ? i : i + 1 } : d))
                    }
                    onDragEnd={() => setDrag(null)}
                  />
                ))}
              </div>
            </div>
          ))}
        </div>
      )}

      {cancelled.length > 0 && (
        <div class="td-cancelled">
          <button
            type="button"
            class={`td-cancelled-toggle${showCancelled ? ' open' : ''}`}
            aria-expanded={showCancelled}
            onClick={() => setShowCancelled((v) => !v)}
          >
            <Icon name="chevron-right" size={12} />
            {t('todos.cancelledCount', { n: String(cancelled.length) })}
            <span class="dim">{showCancelled ? t('todos.hideCancelled') : t('todos.showCancelled')}</span>
          </button>
          {showCancelled && (
            <div class="td-cancelled-list">
              {cancelled.map((x) => (
                <button
                  key={x.id}
                  type="button"
                  class="td-cancelled-row"
                  onClick={() => setEditing(x.id)}
                >
                  <span class="td-cancelled-title">{x.title}</span>
                  <span class="dim">{t('todos.status.cancelled')}</span>
                </button>
              ))}
            </div>
          )}
        </div>
      )}

      {/* One datalist for both the composer and the detail sheet — it must
          exist whether or not either of them is open. */}
      <datalist id="td-project-list">
        {projects.map((p) => (
          <option key={p} value={p} />
        ))}
      </datalist>

      {editingTodo && (
        <TodoSheet
          key={editingTodo.id}
          todo={editingTodo}
          onClose={() => setEditing(null)}
          onChanged={bump}
          onOpenTask={onOpenTask}
          onDeleted={() => {
            setEditing(null)
            bump()
          }}
        />
      )}
    </section>
  )
}

/** The inline "新建待办" composer. One line, Enter to save, Esc to clear —
 *  the whole point of a to-do list is that adding one costs nothing. */
function Composer({ onCreated }: { onCreated(): void }) {
  const [title, setTitle] = useState('')
  const [open, setOpen] = useState(false)
  const [priority, setPriority] = useState<Todo['priority']>('normal')
  const [due, setDue] = useState('')
  const [project, setProject] = useState('')
  const [busy, setBusy] = useState(false)
  const input = useRef<HTMLInputElement>(null)

  useEffect(() => {
    if (open) input.current?.focus()
  }, [open])

  async function submit(e: Event) {
    e.preventDefault()
    const value = title.trim()
    if (!value || busy) return
    setBusy(true)
    try {
      await api.createTodo({
        title: value,
        priority,
        due_at: due || undefined,
        project: project || undefined,
      })
      setTitle('')
      setDue('')
      // Priority and project stick: adding three errands for the same project
      // should not mean re-picking it three times.
      onCreated()
      input.current?.focus()
    } catch (err) {
      toastError(err)
    } finally {
      setBusy(false)
    }
  }

  if (!open) {
    return (
      <button type="button" class="td-add" onClick={() => setOpen(true)}>
        <Icon name="plus" size={14} />
        {t('todos.new')}
      </button>
    )
  }

  return (
    <form class="td-composer" onSubmit={submit}>
      <div class="td-composer-main">
        <input
          ref={input}
          class="td-composer-input"
          type="text"
          placeholder={t('todos.newPlaceholder')}
          aria-label={t('todos.newPlaceholder')}
          value={title}
          onInput={(e) => setTitle((e.target as HTMLInputElement).value)}
          onKeyDown={(e) => {
            if (e.key === 'Escape') {
              setOpen(false)
              setTitle('')
            }
          }}
        />
        <button class="btn primary small" type="submit" disabled={busy || !title.trim()}>
          {t('todos.add')}
        </button>
        <button
          class="btn small"
          type="button"
          onClick={() => {
            setOpen(false)
            setTitle('')
          }}
        >
          {t('common.cancel')}
        </button>
      </div>
      <div class="td-composer-meta">
        <label class="td-inline-field">
          <span>{t('todos.field.due')}</span>
          <input
            class="input"
            type="date"
            value={due}
            onInput={(e) => setDue((e.target as HTMLInputElement).value)}
          />
        </label>
        <label class="td-inline-field">
          <span>{t('todos.field.priority')}</span>
          <select
            class="input"
            value={priority}
            onChange={(e) => setPriority((e.target as HTMLSelectElement).value as Todo['priority'])}
          >
            {PRIO_CYCLE.map((p) => (
              <option key={p} value={p}>
                {t(`todos.priority.${p}`)}
              </option>
            ))}
          </select>
        </label>
        <label class="td-inline-field">
          <span>{t('todos.field.project')}</span>
          <input
            class="input"
            type="text"
            list="td-project-list"
            placeholder={t('todos.field.projectPlaceholder')}
            value={project}
            onInput={(e) => setProject((e.target as HTMLInputElement).value)}
          />
        </label>
      </div>
    </form>
  )
}

function TodoCard({
  todo,
  dropBefore,
  onOpen,
  onTick,
  onCyclePriority,
  onOpenTask,
  onDragStart,
  onDragOver,
  onDragEnd,
}: {
  todo: Todo
  dropBefore: boolean
  onOpen(): void
  onTick(): void
  onCyclePriority(): void
  onOpenTask(id: string): void
  onDragStart(): void
  onDragOver(above: boolean): void
  onDragEnd(): void
}) {
  const done = todo.status === 'done'
  const paused = todo.status === 'paused'
  const due = dueInfo(todo.due_at)
  return (
    <div
      class={`td-card${done ? ' done' : ''}${paused ? ' paused' : ''}${dropBefore ? ' drop-before' : ''}`}
      data-priority={todo.priority}
      role="button"
      tabIndex={0}
      draggable
      onClick={onOpen}
      onKeyDown={(e) => {
        if (e.target !== e.currentTarget) return
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault()
          onOpen()
        }
      }}
      onDragStart={(e) => {
        if (e.dataTransfer) {
          e.dataTransfer.effectAllowed = 'move'
          e.dataTransfer.setData('text/plain', String(todo.id))
        }
        onDragStart()
      }}
      onDragEnd={onDragEnd}
      onDragOver={(e) => {
        if (!(e.dataTransfer?.types.includes('text/plain'))) return
        e.preventDefault()
        const rect = (e.currentTarget as HTMLElement).getBoundingClientRect()
        onDragOver(e.clientY < rect.top + rect.height / 2)
      }}
    >
      <button
        type="button"
        class={`td-tick${done ? ' on' : ''}`}
        aria-label={done ? t('todos.reopen') : t('todos.complete')}
        aria-pressed={done}
        onClick={(e) => {
          e.stopPropagation()
          onTick()
        }}
      >
        {done && <Icon name="check" size={11} />}
      </button>
      <div class="td-card-main">
        <div class="td-card-title">{todo.title}</div>
        {todo.description && <div class="td-card-desc">{todo.description}</div>}
        <div class="td-card-meta">
          <button
            type="button"
            class={`td-prio prio-${todo.priority}`}
            title={t('todos.priorityHint')}
            onClick={(e) => {
              e.stopPropagation()
              onCyclePriority()
            }}
          >
            {t(`todos.priority.${todo.priority}`)}
          </button>
          {due && <span class={`td-due${due.tone ? ` ${due.tone}` : ''}`}>{due.label}</span>}
          {(todo.tags ?? []).map((tag) => (
            <span key={tag} class="td-tag">
              {tag}
            </span>
          ))}
          {paused && <span class="td-chip">{t('todos.status.paused')}</span>}
          {todo.project && <span class="td-project">{todo.project}</span>}
          {todo.task_id && (
            <button
              type="button"
              class="td-task-link"
              title={t('todos.openTask')}
              onClick={(e) => {
                e.stopPropagation()
                onOpenTask(todo.task_id)
              }}
            >
              <Icon name="route" size={11} />
              {t('todos.openTask')}
            </button>
          )}
        </div>
      </div>
    </div>
  )
}

/** The detail sheet: every field a todo has, edited in place.
 *
 *  A right-hand sheet rather than a route, because a todo is a note — opening
 *  one must not cost a navigation, and closing it must return the board exactly
 *  as it was. Changes are saved explicitly: text fields would otherwise fire a
 *  PATCH per keystroke. */
function TodoSheet({
  todo,
  onClose,
  onChanged,
  onDeleted,
  onOpenTask,
}: {
  todo: Todo
  onClose(): void
  onChanged(): void
  onDeleted(): void
  onOpenTask(id: string): void
}) {
  const [title, setTitle] = useState(todo.title)
  const [description, setDescription] = useState(todo.description)
  const [status, setStatus] = useState<TodoStatus>(todo.status)
  const [priority, setPriority] = useState<Todo['priority']>(todo.priority)
  const [due, setDue] = useState(toDateInput(todo.due_at))
  const [tags, setTags] = useState((todo.tags ?? []).join(', '))
  const [project, setProject] = useState(todo.project)
  const [busy, setBusy] = useState(false)
  const [starting, setStarting] = useState(false)
  const [saved, setSaved] = useState(false)

  const dirty =
    title !== todo.title ||
    description !== todo.description ||
    status !== todo.status ||
    priority !== todo.priority ||
    due !== toDateInput(todo.due_at) ||
    tags !== (todo.tags ?? []).join(', ') ||
    project !== todo.project

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    addEventListener('keydown', onKey)
    return () => removeEventListener('keydown', onKey)
  }, [onClose])

  async function save() {
    if (busy || !title.trim()) return
    setBusy(true)
    try {
      await api.patchTodo(todo.id, {
        title: title.trim(),
        description,
        status,
        priority,
        due_at: due,
        tags: tags
          .split(/[,，]/)
          .map((s) => s.trim())
          .filter(Boolean),
        project,
      })
      setSaved(true)
      setTimeout(() => setSaved(false), 1600)
      onChanged()
    } catch (e) {
      toastError(e)
    } finally {
      setBusy(false)
    }
  }

  async function start() {
    if (starting || todo.task_id) return
    setStarting(true)
    try {
      const res = await api.startTodo(todo.id)
      if (res.warning) toast(res.warning)
      toast(t('todos.started'))
      onChanged()
    } catch (e) {
      if (e instanceof ApiError && e.status === 503) toastError(new Error(t('todos.startNoModel')))
      else toastError(e)
    } finally {
      setStarting(false)
    }
  }

  async function remove() {
    const ok = await confirmDialog({
      title: t('todos.deleteTitle'),
      message: t('todos.deleteMsg'),
      confirmLabel: t('todos.delete'),
      danger: true,
    })
    if (!ok) return
    try {
      await api.deleteTodo(todo.id)
      toast(t('todos.deleted'))
      onDeleted()
    } catch (e) {
      toastError(e)
    }
  }

  return (
    <>
      <div class="td-scrim" onClick={onClose} aria-hidden="true" />
      <aside class="td-sheet" role="dialog" aria-modal="true" aria-label={t('todos.detail')}>
        <header class="td-sheet-head">
          <span class="td-sheet-kicker">{t('todos.detail')}</span>
          <button type="button" class="td-sheet-x" aria-label={t('common.close')} onClick={onClose}>
            ×
          </button>
        </header>

        <div class="td-sheet-body">
          <input
            class="td-sheet-title"
            type="text"
            value={title}
            aria-label={t('todos.field.title')}
            onInput={(e) => setTitle((e.target as HTMLInputElement).value)}
          />
          <textarea
            class="input td-sheet-desc"
            rows={4}
            placeholder={t('todos.field.descriptionPlaceholder')}
            aria-label={t('todos.field.description')}
            value={description}
            onInput={(e) => setDescription((e.target as HTMLTextAreaElement).value)}
          />

          <div class="td-sheet-grid">
            <label class="td-field">
              <span>{t('todos.field.status')}</span>
              <select
                class="input"
                value={status}
                onChange={(e) => setStatus((e.target as HTMLSelectElement).value as TodoStatus)}
              >
                {(['pending', 'running', 'paused', 'done', 'cancelled'] as const).map((s) => (
                  <option key={s} value={s}>
                    {t(`todos.status.${s}`)}
                  </option>
                ))}
              </select>
            </label>
            <label class="td-field">
              <span>{t('todos.field.priority')}</span>
              <select
                class="input"
                value={priority}
                onChange={(e) => setPriority((e.target as HTMLSelectElement).value as Todo['priority'])}
              >
                {PRIO_CYCLE.map((p) => (
                  <option key={p} value={p}>
                    {t(`todos.priority.${p}`)}
                  </option>
                ))}
              </select>
            </label>
            <label class="td-field">
              <span>{t('todos.field.due')}</span>
              <input
                class="input"
                type="date"
                value={due}
                onInput={(e) => setDue((e.target as HTMLInputElement).value)}
              />
            </label>
            <label class="td-field">
              <span>{t('todos.field.project')}</span>
              <input
                class="input"
                type="text"
                list="td-project-list"
                placeholder={t('todos.field.projectPlaceholder')}
                value={project}
                onInput={(e) => setProject((e.target as HTMLInputElement).value)}
              />
            </label>
          </div>

          <label class="td-field">
            <span>{t('todos.field.tags')}</span>
            <input
              class="input"
              type="text"
              placeholder={t('todos.field.tagsPlaceholder')}
              value={tags}
              onInput={(e) => setTags((e.target as HTMLInputElement).value)}
            />
          </label>

          {todo.task_id && (
            <button type="button" class="td-task-row" onClick={() => onOpenTask(todo.task_id)}>
              <Icon name="route" size={13} />
              <span class="mono">{todo.task_id}</span>
              <span class="dim">{t('todos.openTask')}</span>
            </button>
          )}
        </div>

        <footer class="td-sheet-foot">
          <button type="button" class="btn danger small" onClick={() => void remove()}>
            {t('todos.delete')}
          </button>
          <div class="td-sheet-actions">
            {!todo.task_id && (
              <button
                type="button"
                class="btn small"
                disabled={starting}
                title={t('todos.startHint')}
                onClick={() => void start()}
              >
                {starting ? t('todos.starting') : t('todos.start')}
              </button>
            )}
            <button
              type="button"
              class="btn primary small"
              disabled={busy || !dirty || !title.trim()}
              onClick={() => void save()}
            >
              {busy ? t('todos.saving') : saved ? t('todos.saved') : t('todos.save')}
            </button>
          </div>
        </footer>
      </aside>
    </>
  )
}
