// SPDX-License-Identifier: AGPL-3.0-or-later

// The dock's side conversation: a full conversation beside the thread without
// becoming part of it.
//
// The everyday interruption is a question or a small piece of work that does
// not belong in the thread. Typed into the thread, it either steers a running
// turn or becomes a turn of a thread it had nothing to do with. This pane is
// the same engine on a separate transcript, modelled on Claude Code's side
// sessions:
//
//   - Capability: the same tool face as the thread, dispatch included. The
//     channel may read the workspace, query live state, write and submit
//     tasks or plans; every action passes the same permission and approval
//     gates. Isolation is the transcript, not a crippled tool set.
//   - Transcript: the thread's history is replayed above a boundary marker as
//     reference; the channel's own turns live in the session record
//     (Session.SideTurns) and never enter the thread's turns or summary.
//   - Background: the ask runs under the panel's lifetime, and the question
//     and answer are filed server-side. The pane can close, reload, or open on
//     another device and still find the conversation; GET /api/sessions/{id}/side
//     is the reconnect surface, and a running operation is followed by polling
//     until the answer lands.
//
// The pane's `+` clears the server-side conversation and cancels a running
// side ask.

import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks'
import {
  api,
  ApiError,
  askSideStream,
  isAbort,
  type AskStatusEvent,
  type ClarifyAnswer,
  type ClarifySpec,
  type SideChatSummary,
  type SideConversation,
  type OperationActivity,
} from '../api/client'
import { locale, t } from '../i18n'
import { relativeTime } from '../format/time'
import { Markdown } from '../md/render'
import { Icon } from './icons'
import { MessageCopyButton } from './copy'
import { ClarifyCard } from './clarify-card'
import { confirmDialog } from './confirm'
import { ModelSelect } from './model-select'
import { ReplyActivity } from './execution-steps-view'
import { upsertActivity } from './execution-steps'

interface Line {
  operationID?: string
  thought?: string
  activities?: OperationActivity[]
  id: number
  role: 'user' | 'assistant'
  text: string
  live?: boolean
  /** What this answer checked, in order: the tool and its subject. Display
   *  only — the engine never replays it, so it never enters the prompt. */
  tools?: string[]
  /** Billed tokens for the answer, when the provider reported them. */
  tokens?: number
  /** The task this answer dispatched, when it started work. */
  taskId?: string
  /** A failed answer's error. The text may carry the partial prose already
   *  streamed before the failure. */
  error?: string
}

export interface SideChatProps {
  /** The thread this conversation sits beside. */
  sessionId: string | null
  /** Bumped by the panel's `+`: start a fresh conversation. */
  resetSignal?: number
  /** `+` opened this pane for the first time, from another pane. An epoch bump
   *  cannot be seen by a component that did not exist when it happened, so
   *  the fresh start arrives as a mount-time flag instead. Read once. */
  freshStart?: boolean
  /** Bumped when the reader returns to this tab. The pane stays mounted, so
   *  re-read the thread's stored side conversation instead of showing a stale
   *  one. A tab switch is never a new conversation — only `+` is. */
  reloadSignal?: number
  /** Bumped by a second click on the 对话 tab: toggle the history popover.
   *  The pane keeps no permanent history bar, so this is its only door. */
  historySignal?: number
  /** Put a reply into the composer — the one crossing back into the thread. */
  onInsert(text: string): void
  /** Open a task the side answer dispatched. */
  onOpenTask?(id: string): void
}

let seq = 0

/** The side channel's one-line progress note. Retry and outage milestones are
 *  re-phrased here from the same structured fields the main composer reads, so
 *  two panes in one console do not speak different languages about the same
 *  provider hiccup. */
function sideStatusNote(text: string, ev?: AskStatusEvent): string {
  if (ev?.kind === 'retry') {
    const reason = ev.http_status ? `HTTP ${ev.http_status}` : t('sessions.retry.transport')
    return t('sessions.retry', {
      reason,
      delay: Math.max(0, Math.round((ev.delay_ms ?? 0) / 1000)),
      attempt: ev.attempt ?? 0,
      max: ev.max ?? 0,
    })
  }
  if (ev?.kind === 'offline') {
    const waited = Math.max(0, Math.round((ev.waited_ms ?? 0) / 1000))
    return ev.detail === 'resumed'
      ? t('sessions.offline.resumed', { delay: waited })
      : t('sessions.offline.waiting')
  }
  return text
}

/** Compact token count for the answer foot: 950, 1.2k, 1.1M. */
function formatTokens(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`
  if (n >= 1000) return `${(n / 1000).toFixed(1)}k`
  return String(n)
}

/** The history rows carry unix seconds; relativeTime wants an ISO stamp. */
function isoOf(ts?: number): string {
  return ts ? new Date(ts * 1000).toISOString() : ''
}

/** Task ids are UUIDv7 strings; the first segment is enough to recognize one
 *  in a chip, and the full id stays in the title. */
function shortId(id: string): string {
  return id.length > 12 ? id.slice(0, 8) : id
}

export function SideChat(props: SideChatProps) {
  const [lines, setLines] = useState<Line[]>([])
  // The conversations beside this thread, newest first, and the one on
  // screen. The list is the pane's history: `+` adds a row, the trash drops
  // one, and picking a row swaps the transcript without touching the thread.
  const [chats, setChats] = useState<SideChatSummary[]>([])
  const [activeId, setActiveId] = useState('')
  const activeRef = useRef('')
  // The history is a popover opened from the dock's 对话 tab, not a bar over
  // the transcript: it is a place you go on purpose, not a permanent row.
  const [historyOpen, setHistoryOpen] = useState(false)
  const [input, setInput] = useState('')
  const [busy, setBusy] = useState(false)
  const [stopping, setStopping] = useState(false)
  const [stopError, setStopError] = useState('')
  // attached: this pane holds the live SSE stream. When false and busy is
  // true, the answer is being produced in the background and the pane polls.
  const [attached, setAttached] = useState(false)
  const [loaded, setLoaded] = useState(false)
  const [status, setStatus] = useState('')
  const [err, setErr] = useState('')
  const [retryPrompt, setRetryPrompt] = useState('')
  const [clarify, setClarify] = useState<{ spec: ClarifySpec; operationID: string } | null>(null)
  const live = useRef('')
  const abort = useRef<AbortController | null>(null)
  const logRef = useRef<HTMLDivElement>(null)
  const inputRef = useRef<HTMLTextAreaElement>(null)
  // The history popover, for the outside-click check.
  const popRef = useRef<HTMLDivElement>(null)
  // Which ask the visible conversation belongs to. Switching threads (or
  // clearing the pane) bumps it, so callbacks from an aborted ask cannot
  // append their late deltas to the conversation now on screen.
  const run = useRef(0)
  const selection = useRef(0)
  const selecting = useRef(false)
  const reading = useRef<number | null>(null)
  // The side operation this pane is following, from the live stream or from the
  // stored operation on load. Stop names it to the cancel endpoint.
  const opID = useRef('')
  const stopPending = useRef<'waiting' | 'sending' | null>(null)
  // The reload effect must not read stale render state: while an answer is
  // streaming (or being followed in the background) the pane's lines are
  // newer than the stored transcript, so a re-read would erase them.
  const liveState = useRef({ busy: false, attached: false, loaded: false })
  liveState.current = { busy, attached, loaded }

  function showChat(id: string) {
    activeRef.current = id
    setActiveId(id)
  }

  const applyConversation = (conv: SideConversation) => {
    setChats(conv.chats ?? [])
    // `chat` names the transcript in this payload; `active` is the stored
    // pointer, which a read for a specific chat deliberately does not move.
    showChat(conv.chat ?? conv.active ?? '')
    const stored: Line[] = (conv.turns ?? []).map((turn, index) => ({
      operationID: turn.operation_id || String(turn.ts ?? 0) + ":" + index,
      id: ++seq,
      role: turn.role,
      text: turn.text,
      tools: turn.tools,
      thought: turn.thought,
      activities: turn.activities,
      tokens: turn.tokens,
      taskId: turn.task_id,
      error: turn.error,
    }))
    const op = conv.operation
    if (op?.status === 'running') stored.push({ operationID: op.id, id: -1, role: 'assistant', text: op.text ?? '', thought: op.thought, activities: op.activities, live: true })
    setLines(stored)
    opID.current = op?.status === 'running' ? op.id : ''
    const failed = op?.status === 'failed' || op?.status === 'cancelled'
    setRetryPrompt(failed ? stored.filter((line) => line.role === 'user').at(-1)?.text ?? '' : '')
    setErr(failed ? op.error ?? '' : '')
    if (op?.status === 'running') {
      setBusy(true)
      setAttached(false)
      setStatus(t('side.background'))
    } else {
      setBusy(false)
      setAttached(false)
      setStatus('')
      setClarify(null)
    }
  }

  // Poll one snapshot at a time per conversation. A slow read from the
  // conversation being left must not block or replace its successor.
  async function readConversation(id: string) {
    const generation = run.current
    if (selecting.current || reading.current === generation) return
    reading.current = generation
    const current = () => run.current === generation && !selecting.current
    try {
      const conversation = await api.sideConversation(id, activeRef.current || undefined)
      if (!current()) return
      applyConversation(conversation)
    } catch (e) {
      if (!current()) return
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      if (reading.current === generation) reading.current = null
    }
  }

  function detachConversation() {
    run.current++
    abort.current?.abort()
    abort.current = null
    opID.current = ''
    stopPending.current = null
    setStopping(false)
    setStopError('')
    setBusy(false)
    setAttached(false)
    setStatus('')
    setErr('')
    setRetryPrompt('')
    setClarify(null)
  }

  // Keep reading the current chat until its replacement actually exists. If
  // a history row was deleted elsewhere, its failed selection leaves the
  // current stream or polling intact.
  async function selectConversation(read: () => Promise<SideConversation>, clearDraft: boolean) {
    const ticket = ++selection.current
    selecting.current = true
    setLoaded(false)
    try {
      const conversation = await read()
      if (selection.current !== ticket) return
      detachConversation()
      if (clearDraft) setInput('')
      applyConversation(conversation)
    } catch (e) {
      if (selection.current === ticket) setErr(e instanceof Error ? e.message : String(e))
    } finally {
      if (selection.current === ticket) {
        selecting.current = false
        setLoaded(true)
      }
    }
  }

  // refreshList re-reads only the history rows — the transcript on screen is
  // newer than the stored one while an answer streams, so a full re-read here
  // would erase the reply the reader is watching.
  const refreshList = async (id: string) => {
    const generation = run.current
    try {
      const conv = await api.sideConversation(id)
      if (run.current !== generation) return
      setChats(conv.chats ?? [])
      if (!activeRef.current) showChat(conv.chat ?? conv.active ?? '')
    } catch {
      /* the list is an index; a failed read must not disturb the transcript */
    }
  }

  // A new thread loads its own stored conversations; the pane's `+` starts
  // another one beside this thread. The old answers were about a different
  // thread (or a conversation the user deliberately left), and keeping them on
  // screen would be a lie about what they are reading.
  const seen = useRef<{ session: string | null; reset?: number }>()
  // A `+` pressed on a tool page or the start state both opens this pane and
  // asks for a new conversation. The epoch moves in the same render that
  // mounts the pane, so there is no earlier value to compare it with; the
  // mount flag is the only way to tell "new" from "reopen". Consumed once,
  // so a later thread switch still loads that thread's stored conversation.
  const freshOnMount = useRef(props.freshStart === true)
  useLayoutEffect(() => {
    const before = seen.current
    seen.current = { session: props.sessionId, reset: props.resetSignal }
    const sameThread = before?.session === props.sessionId
    const reset = freshOnMount.current || (sameThread && before?.reset !== props.resetSignal)
    freshOnMount.current = false
    setHistoryOpen(false)
    if (!sameThread) {
      selection.current++
      selecting.current = false
      detachConversation()
      setInput('')
      setLines([])
      showChat('')
      setChats([])
    }
    if (!props.sessionId) {
      selecting.current = false
      setLoaded(true)
      return
    }
    const id = props.sessionId
    if (reset) {
      // `+` opens another conversation rather than discarding the one on
      // screen: the transcript stays in the history list, one click away.
      // A fresh, empty conversation looks exactly like the empty one it
      // replaced. Put the caret in the composer so the click always has a
      // visible answer, even on a thread whose last side chat was blank.
      inputRef.current?.focus()
      void selectConversation(() => api.newSideChat(id), true)
      return
    }
    void selectConversation(() => api.sideConversation(id), false)
  }, [props.sessionId, props.resetSignal])

  // Coming back to the 对话 tab shows the conversation the pane was reading —
  // its stored transcript, not a fresh one. `+` (resetSignal) is the only thing
  // that starts a new side chat. Never interrupt a live answer.
  const seenReload = useRef(props.reloadSignal)
  useEffect(() => {
    const n = props.reloadSignal
    if (n === undefined || n === seenReload.current) return
    seenReload.current = n
    // Coming back to the tab means reading the conversation, not the index:
    // the popover is a detour, and it should not still be hanging there.
    setHistoryOpen(false)
    const s = liveState.current
    const id = props.sessionId
    if (!id || selecting.current || !s.loaded || s.busy || s.attached) return
    void readConversation(id)
  }, [props.reloadSignal, props.sessionId])

  // A second click on the lit 对话 tab toggles the history. That tab is the
  // pane's only door to it now; the same click closes what it opened.
  const seenHistory = useRef(props.historySignal)
  useEffect(() => {
    const n = props.historySignal
    if (n === undefined || n === seenHistory.current) return
    seenHistory.current = n
    if (!props.sessionId) return
    setHistoryOpen((open) => !open)
  }, [props.historySignal, props.sessionId])

  // Clicking anywhere else closes the list. The dock's tab strip is exempt:
  // that click is the toggle above, and closing here first would make a
  // second click on 对话 reopen the list instead of closing it.
  useEffect(() => {
    if (!historyOpen) return
    const onDown = (e: MouseEvent) => {
      const target = e.target as Element | null
      if (!target) return
      if (popRef.current?.contains(target)) return
      if (target.closest?.('.dock-tabs')) return
      setHistoryOpen(false)
    }
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return
      e.stopPropagation()
      setHistoryOpen(false)
    }
    document.addEventListener('mousedown', onDown)
    document.addEventListener('keydown', onKey, true)
    return () => {
      document.removeEventListener('mousedown', onDown)
      document.removeEventListener('keydown', onKey, true)
    }
  }, [historyOpen])

  // A background answer: the pane is not holding the stream, so follow the
  // stored operation until it settles and the conversation comes back whole.
  useEffect(() => {
    if (!props.sessionId || !busy || attached) return
    const id = props.sessionId
    const timer = window.setInterval(() => {
      if (seen.current?.session === id && !liveState.current.attached) void readConversation(id)
    }, 1500)
    return () => window.clearInterval(timer)
  }, [props.sessionId, busy, attached])

  // An in-flight ask does not outlive the panel: the dock stays mounted once
  // opened, but a session switch or an unmount should not leave a stream
  // feeding a component that is gone. The server keeps the ask (it runs under
  // the panel's lifetime), so the answer is waiting on the next load.
  useLayoutEffect(() => () => {
    selection.current++
    run.current++
    abort.current?.abort()
  }, [])

  // Follow the answer as it arrives; a reader who scrolled up stays where they
  // are (the distance check is the same contract the transcript keeps).
  useEffect(() => {
    const el = logRef.current
    if (!el) return
    const near = el.scrollHeight - el.scrollTop - el.clientHeight < 80
    if (near) el.scrollTop = el.scrollHeight
  }, [lines])

  // The field is one line at rest and grows with the text. A fixed two-row
  // textarea parked a short question at the top of a tall empty box while the
  // Send button sat alone at the bottom — the footer read as two unrelated
  // controls. Measuring on every value change also covers the starter chips
  // and the clear after sending, which do not go through onInput.
  const fitInput = () => {
    const el = inputRef.current
    if (!el) return
    el.style.height = 'auto'
    el.style.height = `${Math.min(el.scrollHeight, 120)}px`
  }
  useEffect(fitInput, [input])

  const patchLast = (text: string, done = false) =>
    setLines((prev) => {
      if (prev.length === 0) return prev
      const next = prev.slice()
      const last = next[next.length - 1]!
      next[next.length - 1] = { ...last, text, live: !done }
      return next
    })

  const setLastTokens = (tokens: number) =>
    setLines((prev) => {
      const last = prev[prev.length - 1]
      if (!last || last.role !== 'assistant') return prev
      const next = prev.slice()
      next[next.length - 1] = { ...last, tokens }
      return next
    })

  const setLastTask = (taskId: string) =>
    setLines((prev) => {
      const last = prev[prev.length - 1]
      if (!last || last.role !== 'assistant') return prev
      const next = prev.slice()
      next[next.length - 1] = { ...last, taskId }
      return next
    })

  // appendTool records one tool call on the answer being streamed. The chip is
  // added when the call starts, so the reader watches the channel work
  // (system_status, then workspace_read side.go) instead of a mute spinner.
  const appendTool = (label: string) =>
    setLines((prev) => {
      const last = prev[prev.length - 1]
      if (!last || last.role !== 'assistant' || !last.live) return prev
      const tools = last.tools ?? []
      if (tools.length >= 24 || tools[tools.length - 1] === label) return prev
      const next = prev.slice()
      next[next.length - 1] = { ...last, tools: [...tools, label] }
      return next
    })

  async function ask(prompt: string) {
    const id = props.sessionId
    if (!id || selecting.current || busy || abort.current) return
    const submitted = input.trim() === prompt ? input : prompt
    if (input.trim() === prompt) setInput('')
    setErr('')
    setRetryPrompt('')
    setStatus('')
    setClarify(null)
    stopPending.current = null
    setStopping(false)
    setStopError('')
    live.current = ''
    setBusy(true)
    setAttached(true)
    const userLine = ++seq
    const answerLine = ++seq
    setLines((prev) => [
      ...prev,
      { id: userLine, role: 'user', text: prompt },
      { id: answerLine, role: 'assistant', text: '', live: true },
    ])
    const ctrl = new AbortController()
    abort.current = ctrl
    const runId = ++run.current
    const current = () => run.current === runId
    let accepted = false
    let settled = false
    function failed(message: string | undefined, rejected: boolean) {
      if (!current()) return
      if (message) {
        setErr(message)
        setRetryPrompt(prompt)
      }
      if (accepted || !rejected) {
        patchLast(live.current, true)
      } else {
        setLines((prev) => prev.filter((line) => line.id !== userLine && line.id !== answerLine))
        setInput((draft) => draft || submitted)
      }
    }
    try {
      await askSideStream(
        id,
        prompt,
        // The question belongs to the conversation on screen. With none (a
        // thread that never had a side chat) the server mints one, and the
        // list refresh below picks up its title.
        { chat: activeId },
        {
          onReasoning: (delta) => {
            if (!current()) return
            setLines(prev => prev.map((line, index) => index === prev.length - 1 && line.live
              ? { ...line, thought: (line.thought ?? '') + delta } : line))
          },
          onActivity: (record) => {
            if (!current()) return
            setLines(prev => prev.map((line, index) => index === prev.length - 1 && line.live
              ? { ...line, activities: upsertActivity(line.activities ?? [], record) } : line))
          },
          onDelta: (d) => {
            if (!current()) return
            live.current += d
            patchLast(live.current)
          },
          onStatus: (text, ev) => {
            if (!current()) return
            // A tool call is progress worth keeping, not a transient note: it
            // becomes a chip on the answer, and the status line repeats it in
            // the reader's own language while the call runs.
            if (ev?.kind === 'tool' && ev.name) {
              const label = ev.detail ? `${ev.name} ${ev.detail}` : ev.name
              appendTool(label)
              setStatus(t('side.tool', { tool: label }))
              return
            }
            setStatus(sideStatusNote(text, ev))
          },
          onOperation: (operationID, chatID) => {
            if (current() && operationID) {
              accepted = true
              opID.current = operationID
              if (chatID) showChat(chatID)
              setLines(prev => prev.map(line => line.live ? { ...line, operationID } : line))
              if (stopPending.current === 'waiting') void stop()
            }
          },
          onTask: (taskID) => {
            if (current()) setLastTask(taskID)
          },
          onQuestion: (spec, operationID) => {
            if (!current()) return
            setClarify({ spec, operationID })
            setStatus('')
          },
          onResult: (r) => {
            if (!current()) return
            accepted = true
            settled = true
            const text = r.answer || live.current
            // A dispatch or plan has no prose answer of its own; name what it
            // started so the live turn is not an empty bubble.
            if (!text && r.plan_id) {
              patchLast(t('side.planStarted', { goal: r.plan_goal ?? '' }), true)
            } else {
              patchLast(text, true)
            }
            if (typeof r.tokens === 'number' && r.tokens > 0) setLastTokens(r.tokens)
            if (r.task_id) setLastTask(r.task_id)
          },
          onError: (message) => {
            settled = true
            failed(message, true)
          },
        },
        ctrl.signal,
      )
    } catch (e) {
      // A broken connection doesn't tell us whether the server filed the
      // question. Keep the transcript unless the server actually refused it.
      const message = isAbort(e) ? undefined : e instanceof Error ? e.message : String(e)
      if (!accepted && !settled) failed(message, e instanceof ApiError && e.status > 0)
    } finally {
      if (current()) {
        setAttached(false)
        abort.current = null
        if (stopPending.current === 'waiting') {
          stopPending.current = null
          setStopping(false)
        }
        if (accepted && !settled) {
          // EOF can be a lost connection, even when fetch reports it as a
          // clean end. Continue tracking the accepted operation on the node.
          setStatus(t('side.background'))
          void readConversation(id)
        } else {
          setBusy(false)
          setStatus('')
          setClarify(null)
          opID.current = ''
          void refreshList(id)
        }
      }
    }
  }

  // pick swaps the transcript without disturbing the thread. A live answer in
  // the chat being left keeps running server-side — its row says so, and
  // coming back re-attaches to it.
  function pick(chatID: string) {
    setHistoryOpen(false)
    const id = props.sessionId
    if (!id || (chatID === activeId && !selecting.current)) return
    void selectConversation(() => api.selectSideChat(id, chatID), true)
  }

  // remove drops one conversation. Deleting the one on screen falls back to
  // the next newest, which the server returns in the same response.
  async function remove(chatID: string) {
    const id = props.sessionId
    if (!id) return
    const generation = run.current
    const ok = await confirmDialog({
      title: t('side.deleteChat'),
      message: t('side.deleteChatBody'),
      confirmLabel: t('side.deleteChat'),
    })
    if (!ok || generation !== run.current) return
    await selectConversation(() => api.deleteSideChat(id, chatID), false)
  }

  function send() {
    const prompt = input.trim()
    if (!prompt) return
    void ask(prompt)
  }

  async function stop() {
    const id = props.sessionId
    if (!id || stopPending.current === 'sending') return
    setStopping(true)
    setStopError('')
    const operationID = opID.current
    if (!operationID) {
      // The user can request Stop while the first response is still on its
      // way. Wait for the operation ID so the cancellation has an owner.
      stopPending.current = 'waiting'
      return
    }
    stopPending.current = 'sending'
    const generation = run.current
    try {
      const result = await api.cancelSession(id, operationID)
      if (run.current !== generation) return
      if (result.cancelled && abort.current) abort.current.abort()
      if (!abort.current) void readConversation(id)
    } catch (e) {
      if (run.current === generation) setStopError(e instanceof Error ? e.message : String(e))
    } finally {
      if (run.current === generation) {
        stopPending.current = null
        setStopping(false)
      }
    }
  }

  function answerClarify(answers: ClarifyAnswer[]) {
    const pending = clarify
    if (!pending || !props.sessionId) return
    const generation = run.current
    // Keep the card until the answer lands: a failed POST must not leave the
    // turn parked behind a card the pane already threw away.
    void api.answerSessionClarify(props.sessionId, pending.operationID, answers)
      .then(() => {
        if (run.current === generation) setClarify(null)
      })
      .catch((e) => {
        if (run.current === generation) setErr(e instanceof Error ? e.message : String(e))
      })
  }

  return (
    <div class="side-chat">
      {/* The history, on request: a second click on the 对话 tab opens it.
          It floats over the transcript instead of taking a row above it —
          the list is usually one item, and a permanent bar spent the pane's
          scarcest axis on a place the reader visits occasionally. */}
      {props.sessionId && historyOpen && (
        <div class="side-history-pop" ref={popRef} role="dialog" aria-label={t('side.history')}>
          <div class="side-history-pop-head">
            <Icon name="message" size={13} />
            <span>{t('side.history')}</span>
            <span class="side-history-count mono">{chats.length}</span>
            <button
              type="button"
              class="side-history-close"
              aria-label={t('common.close')}
              title={t('common.close')}
              onClick={() => setHistoryOpen(false)}
            >
              <Icon name="x" size={13} />
            </button>
          </div>
          <div class="side-history-list">
            {chats.length === 0 && <p class="side-history-empty">{t('side.historyEmpty')}</p>}
            {chats.map((c) => (
              <div class={`side-history-row${c.id === activeId ? ' is-active' : ''}`} key={c.id}>
                <button type="button" class="side-history-pick" onClick={() => pick(c.id)}>
                  <span class="side-history-title">{c.title || t('side.untitled')}</span>
                  <span class="side-history-meta">
                    {c.running ? t('side.answering') : relativeTime(isoOf(c.updated_at), locale())}
                    <span aria-hidden="true"> · </span>
                    {t('side.turns', { n: c.turns })}
                  </span>
                </button>
                <button
                  type="button"
                  class="side-history-del"
                  aria-label={t('side.deleteChat')}
                  title={t('side.deleteChat')}
                  onClick={() => void remove(c.id)}
                >
                  <Icon name="trash" size={13} />
                </button>
              </div>
            ))}
          </div>
        </div>
      )}
      <div class="side-scroll" ref={logRef}>
        {loaded && lines.length === 0 && !busy && (
          <div class="side-empty">
            <span class="side-empty-icon"><Icon name="message" size={18} /></span>
            {/* A thread with no id has nothing to attach to, so the pane is
                missing its anchor — say that instead of offering a box whose
                send button never lights up. */}
            {props.sessionId ? (
              <>
                <p class="side-empty-lead">{t('side.empty')}</p>
                <p class="side-empty-note">{t('side.readonly')}</p>
                <p class="side-empty-note">{t('side.context')}</p>
                {/* The channel is most useful with a question that names what
                    it can check. These fill the composer rather than sending:
                    the reader edits them to this thread's own words. */}
                <div class="side-starters">
                  <span class="side-starters-label">{t('side.starters')}</span>
                  {(['status', 'summary', 'files'] as const).map((k) => (
                    <button
                      type="button"
                      class="side-starter"
                      key={k}
                      onClick={() => {
                        setInput(t(`side.starter.${k}`))
                        inputRef.current?.focus()
                      }}
                    >
                      {t(`side.starter.${k}`)}
                    </button>
                  ))}
                </div>
              </>
            ) : (
              <p class="side-empty-lead">{t('side.noThread')}</p>
            )}
          </div>
        )}

        {lines.map((l, i) => (
          <div class={`side-turn is-${l.role}`} key={l.id}>
            {l.role === 'user' ? (
              // Your own turn is a bubble on the right, with no face and no
              // label — the same rule the thread keeps. Two voices told apart
              // by position alone is the one distinction a reader never has
              // to decode.
              <p class="side-q">{l.text}</p>
            ) : (
              <>
                {/* The panda keeps a face. In a 320px pane the head is the
                    only thing that still reads at a glance; a bare name in
                    10px type did not. */}
                <div class="side-head">
                  <img class="side-ava" src="/panda-avatar.png?v=1" alt="" width={20} height={20} />
                  <span class="side-name">{t('side.panda')}</span>
                </div>
                <div class="side-body">
                  {l.text && !(l.error && l.text.startsWith('⚠')) ? (
                    <Markdown text={l.text} class="side-md" streaming={Boolean(l.live)} />
                  ) : !l.live && !l.error && !l.taskId && !(l.tools && l.tools.length) ? (
                    // A stored answer that carried nothing but tool calls has
                    // no prose left after the server strips its markup; its
                    // chips are the whole record, not a missing answer.
                    <p class="side-typing dim">{t('side.thinking')}</p>
                  ) : null}
                  {l.error && <p class="side-err">{l.error}</p>}
                  {l.taskId && (
                    <div class="side-task-row">
                      {props.onOpenTask ? (
                        <button
                          type="button"
                          class="side-task"
                          title={l.taskId}
                          onClick={() => props.onOpenTask?.(l.taskId!)}
                        >
                          {t('side.task')} <span class="side-task-id">{shortId(l.taskId)}</span>
                        </button>
                      ) : (
                        <span class="side-task" title={l.taskId}>
                          {t('side.task')} <span class="side-task-id">{shortId(l.taskId)}</span>
                        </span>
                      )}
                    </div>
                  )}
                  {!l.activities?.length && l.tools && l.tools.length > 0 && (
                    <div class="side-tools">
                      <span class="side-tools-label">{t('side.tools')}</span>
                      {l.tools.map((tool, k) => (
                        <span class="side-tool" key={k} title={tool}>
                          {tool}
                        </span>
                      ))}
                    </div>
                  )}
                  <ReplyActivity thought={l.thought} activities={l.activities}
                    scope={'side:' + props.sessionId + ':' + (l.operationID || l.id)} />
                  {!l.live && l.text && (
                    <div class="side-acts">
                      <MessageCopyButton text={l.text} markdown />
                      <button type="button" class="link-btn" onClick={() => props.onInsert(l.text)}>
                        {t('side.insert')}
                      </button>
                      <button
                        type="button"
                        class="link-btn"
                        onClick={() => {
                          // Hand the question back, with what this channel already
                          // found. The main thread would otherwise redo the same
                          // reading; carrying the answer forward is the explicit
                          // bridge back that the isolation is supposed to have.
                          const q = lines[i - 1]?.role === 'user' ? lines[i - 1]!.text : ''
                          if (!q) return
                          props.onInsert(l.text ? `${q}\n\n${t('side.carry')}\n${l.text}` : q)
                        }}
                      >
                        {t('side.toMain')}
                      </button>
                      {l.tokens ? (
                        <span class="side-tokens dim">{formatTokens(l.tokens)} tokens</span>
                      ) : null}
                    </div>
                  )}
                </div>
              </>
            )}
          </div>
        ))}

        {status !== '' && <p class="side-status dim">{status}</p>}
        {err !== '' && (
          <div class="side-error">
            <p class="side-err">{err}</p>
            {retryPrompt && (
              <button type="button" class="link-btn" onClick={() => void ask(retryPrompt)} disabled={busy || !loaded}>
                {t('side.retry')}
              </button>
            )}
          </div>
        )}
      </div>

      {clarify && (
        <div class="side-clarify">
          <ClarifyCard spec={clarify.spec} onSubmit={answerClarify} onCancel={stop} />
        </div>
      )}
      {stopError && <p class="side-err" role="alert">{stopError}</p>}

      <form
        class="side-compose"
        onSubmit={(e) => {
          e.preventDefault()
          void send()
        }}
      >
        {/* One surface, like the main composer: the field, the model that
            will answer and Send all live inside the same rounded box. A
            bordered textarea beside a detached square made the dock's footer
            look like two unrelated controls. */}
        <div class="side-compose-box">
          <textarea
            ref={inputRef}
            class="side-input"
            rows={1}
            value={input}
            placeholder={t('side.placeholder')}
            disabled={!props.sessionId}
            onInput={(e) => setInput((e.currentTarget as HTMLTextAreaElement).value)}
            onKeyDown={(e) => {
              // Enter sends, Shift+Enter breaks the line — the composer's rule,
              // because two text boxes in one pane should not disagree.
              if (e.key === 'Enter' && !e.shiftKey && !e.isComposing && e.keyCode !== 229) {
                e.preventDefault()
                void send()
              }
            }}
          />
          <div class="side-compose-actions">
            {/* The same switch the main composer carries, not a second
                registry: `api.useModel` sets the node's active alias, so the
                side channel answers with whatever the thread is using. */}
            <ModelSelect portal disabled={!props.sessionId || busy} />
            {busy ? (
              <button type="button" class="btn small side-stop" onClick={stop} disabled={stopping}>
                {t('common.stop')}
              </button>
            ) : (
              <button
                type="submit"
                class="btn small primary side-send"
                aria-label={t('sessions.send')}
                title={t('sessions.send')}
                disabled={!input.trim() || !props.sessionId || !loaded}
              >
                <Icon name="send" size={16} />
              </button>
            )}
          </div>
        </div>
      </form>
    </div>
  )
}
