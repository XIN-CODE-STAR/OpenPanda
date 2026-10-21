// SPDX-License-Identifier: AGPL-3.0-or-later

import type { ComponentChildren } from 'preact'
import { useEffect, useMemo, useRef, useState } from 'preact/hooks'
import {
  api,
  askSessionStream,
  type AgentInfo,
  type NodeAgentDetail,
  type NodeInfo,
  type PlanSummary,
  type SelfInfo,
  type Session,
  type Task,
} from '../api/client'
import { useAsync, useChangeSignal, useLocaleRerender, useVisibleInterval } from '../hooks'
import { locale, t } from '../i18n'
import { isNodeConnected } from '../format/node-status'
import { relativeTime } from '../format/time'
import { navigate } from '../nav'
import { ErrorState } from '../components/page'
import { Icon } from '../components/icons'
import { StateBadge } from '../components/state-badge'
import { toast, toastError } from '../components/toast'
import { DirButton } from '../components/dir-picker'
import { ModelSelect } from '../components/model-select'
import { suggestProjectName } from '../components/dir-utils'
import { expandFileRefs } from '../components/attach'
import { OrchestrateCard } from '../components/orchestrate'
import { layerAboveOwnsEscape } from '../components/card-dismiss'
import { SessionPick } from '../components/session-pick'
import { autoGrow, editorClear, editorPlain, editorTextOf, range_or_append, useBrParagraphSeparator } from './sessions'

/** 工作空间 — the fleet as a map. 节点网络 answers "what can each device do"
 *  with cards and forms; this page answers the prior question, "what is
 *  connected to what, and what kind of machine is on the other end", as a
 *  topology: one plate per device, one line per live adjacency, the glyph
 *  drawn from the form factor the machine advertises about itself.
 *
 *  The graph is dependency-free SVG, like the memory map: a deterministic
 *  ring layout (distance from this console = ring index) plus pan/zoom. No
 *  force simulation — a mesh this small reads better when the same fleet
 *  always lands in the same picture. */

type Form = 'laptop' | 'desktop' | 'mini' | 'server' | 'vm' | 'unknown'

const FORMS: Form[] = ['laptop', 'desktop', 'mini', 'server', 'vm', 'unknown']

interface Device {
  id: string
  name: string
  form: Form
  /** Raw GOOS ("darwin"/"windows"/"linux"/…); empty when unreported. */
  os: string
  online: boolean
  local: boolean
  chip: string
  cores: number
  ram: number
  gpu: number
  tasks: number
  maxTasks: number
  queued: number
  rtt: number
  transport: string
  trust: string
  ver: string
  lastSeen: string
  neighbors: string[]
  /** The node's own capability card: what it says it can hand work to. */
  agents: Record<string, NodeAgentDetail>
  abilities: string[]
  kind: string
  tier: number
  /** Is this node holding a work slot right now — the question the map's
   *  hover card answers before anything else. A row's `current_tasks` is the
   *  heartbeat's own count; the ledger adds the tasks this console holds a row
   *  for, which is what makes the local machine light up immediately. */
  busy: boolean
  /** Tasks this node owns, newest first, from this console's ledger. */
  work: Task[]
}

/** One line on the map. `mesh` is a link both ends advertise (a real
 *  adjacency in the routing graph); `measured` is an RTT this console
 *  measured toward a peer that did not advertise us back — still a live
 *  edge, drawn lighter. */
interface Edge {
  a: string
  b: string
  kind: 'mesh' | 'measured'
  rtt: number
  transport: string
}

interface View {
  k: number
  x: number
  y: number
}

const VIEW_HOME: View = { k: 1, x: 0, y: 0 }
const ZOOM_MIN = 0.5
const ZOOM_MAX = 3

/** Plate edge in graph units, the label block under it, and the breathing
 *  room two neighbouring plates need. The ring radii are derived from these
 *  so a crowded fleet never draws plates on top of each other. */
const PLATE = 56
const LABEL_H = 32
const NODE_GAP = 30

/** The hover card's box, in CSS pixels (it does not scale with the map).
 *  Width is fixed by the stylesheet; the height is an estimate used only to
 *  decide whether the card goes above or below the plate. */
const PEEK_W = 236
const PEEK_H = 118

/** Advertised form factors normalize into the six glyph classes. Unknown
 *  spellings (a future class from a newer peer) fall back to `unknown`
 *  rather than being mistaken for a desktop. */
function normalizeForm(raw: string | undefined): Form {
  switch ((raw ?? '').toLowerCase()) {
    case 'laptop':
    case 'notebook':
    case 'portable':
      return 'laptop'
    case 'desktop':
    case 'tower':
    case 'imac':
    case 'workstation':
      return 'desktop'
    case 'mini':
    case 'macmini':
    case 'mac-mini':
    case 'nuc':
      return 'mini'
    case 'server':
    case 'rack':
      return 'server'
    case 'vm':
    case 'virtual':
      return 'vm'
    default:
      return 'unknown'
  }
}

/** Last-resort hint for a peer whose row predates the form-factor field: the
 *  hostname sometimes names the family. Only ever used when the node itself
 *  said nothing — a reported class always wins. */
function guessForm(name: string, os: string): Form {
  const n = name.toLowerCase()
  if (/macbook|laptop|notebook|thinkpad|book pro|book air/.test(n)) return 'laptop'
  if (/mac ?mini|\bmini\b|\bnuc\b/.test(n)) return 'mini'
  if (/server|rack|nas/.test(n)) return 'server'
  if (os === 'darwin') return 'desktop'
  return 'unknown'
}

function osLabel(os: string): string {
  switch (os) {
    case 'darwin':
      return t('workspace.os.mac')
    case 'windows':
      return t('workspace.os.windows')
    case 'linux':
      return t('workspace.os.linux')
    default:
      return t('workspace.os.unknown')
  }
}

function formLabel(form: Form): string {
  return t(`workspace.form.${form}`)
}

/** `node_kind` is a wire value ("physical"/"vm"). A panel that says
 *  "physical" under a Chinese heading is the one place the protocol leaks
 *  into the reading surface. */
function kindLabel(kind: string): string {
  switch (kind) {
    case 'physical':
      return t('workspace.detail.kindPhysical')
    case 'vm':
      return t('workspace.form.vm')
    default:
      return kind
  }
}

/** The three answers the hover card owes: is it working, what is it called,
 *  and what hardware is inside. */
function workStateLabel(d: Device): string {
  if (!d.online) return t('workspace.offline')
  return d.busy ? t('workspace.state.working') : t('workspace.state.idle')
}

function specLine(d: Device): string {
  const parts: string[] = []
  if (d.chip) parts.push(d.chip)
  if (d.cores > 0) parts.push(t('nodes.cores', { n: d.cores }))
  if (d.ram > 0) parts.push(`${d.ram} GB`)
  // A node that reports no hardware still has a class and a system; saying
  // that is better than a blank line under its name.
  return parts.length > 0 ? parts.join(' · ') : `${osLabel(d.os)} · ${formLabel(d.form)}`
}

function loadLine(d: Device): string {
  if (!d.online) {
    const seen = d.lastSeen === 'never' ? t('nodes.never') : relativeTime(d.lastSeen, locale())
    return seen ? `${t('nodes.lastSeen')} ${seen}` : ''
  }
  const parts: string[] = []
  if (d.maxTasks > 0) parts.push(`${t('workspace.detail.tasks')} ${d.tasks}/${d.maxTasks}`)
  else if (d.tasks > 0) parts.push(`${t('workspace.detail.tasks')} ${d.tasks}`)
  if (d.queued > 0) parts.push(`${t('workspace.detail.queued')} ${d.queued}`)
  return parts.join(' · ')
}

function isLocal(self: SelfInfo | null, n: NodeInfo): boolean {
  if (n.is_local) return true
  if (!self) return false
  if (self.node_id && self.node_id === n.id) return true
  if (self.node && self.node.id === n.id) return true
  const name = self.node_name || self.hostname
  return name !== '' && (n.name === name || n.id === name)
}

/** States that mean the node is actually executing: `queued` is work waiting
 *  for a slot, `review` is work parked for a human. Neither is load. */
const RUNNING_STATES = new Set(['dispatched', 'waiting_context', 'running'])
const PENDING_STATES = new Set(['submitted', 'queued'])

/** The comparable half of a node reference. A task's `owner` is a runtime id —
 *  `<name>@<identity>` for a node that carries one, `<name>-<8 hex>` for an
 *  ephemeral one — while a directory row's id is whatever the row was filed
 *  under. Comparing the normalized forms is what lets a task find its node
 *  without the console knowing which spelling the ledger used. */
function nodeKey(id: string): string {
  if (!id) return ''
  let s = id.trim().toLowerCase()
  const at = s.lastIndexOf('@')
  if (at > 0) s = s.slice(0, at)
  // Ephemeral suffix: the daemon refuses node names that look like one, so
  // stripping it can never merge two real nodes.
  if (/-[0-9a-f]{8}$/.test(s)) s = s.slice(0, -9)
  return s
}

function buildDevices(nodes: NodeInfo[], self: SelfInfo | null, tasks: Task[]): Device[] {
  const devices = nodes
    .map((n) => {
      const local = isLocal(self, n)
      const os = (local && self ? self.os : n.os) || ''
      const name = n.name || n.id
      const reported = normalizeForm(n.form_factor)
      return {
        id: n.id,
        name,
        form: reported !== 'unknown' ? reported : guessForm(name, os),
        os,
        // The row's status is the directory's view; `running` adds "this
        // process is up right now" for the local row only.
        online: isNodeConnected(n.status),
        local,
        chip: (local && self ? self.chip : '') || n.chip || '',
        cores: n.capacity?.cpu_cores ?? (local && self ? self.cpu_cores : 0),
        ram: n.capacity?.ram_gb ?? (local && self ? self.ram_gb ?? 0 : 0),
        gpu: n.resource_profile?.gpu_vram_gb ?? 0,
        tasks: n.capacity?.current_tasks ?? 0,
        maxTasks: n.capacity?.max_concurrent_tasks ?? 0,
        queued: n.capacity?.queued_tasks ?? 0,
        rtt: n.rtt_ms ?? 0,
        transport: n.transport ?? '',
        trust: n.trust ?? '',
        ver: n.ver ?? '',
        lastSeen: n.last_seen,
        neighbors: n.neighbors ?? [],
        agents: n.agents ?? {},
        abilities: n.abilities ?? [],
        kind: n.node_kind ?? '',
        tier: n.scheduler_tier ?? 0,
        busy: false,
        work: [] as Task[],
      } satisfies Device
    })
    .sort((a, b) => {
      if (a.local !== b.local) return a.local ? -1 : 1
      if (a.online !== b.online) return a.online ? -1 : 1
      return a.name.localeCompare(b.name)
    })
  // The map always starts from here. A store the node has not registered in
  // yet (a fresh database, a console that came up before the first heartbeat)
  // has no rows at all, and an empty canvas is a worse answer than "this is
  // the machine you are on" — the one device the console can always name.
  if (self && !devices.some((d) => d.local)) {
    const name = self.node_name || self.hostname || t('workspace.local')
    const form = normalizeForm(self.form_factor)
    devices.unshift({
      id: self.node_id || name,
      name,
      form: form !== 'unknown' ? form : guessForm(name, self.os),
      os: self.os,
      online: true,
      local: true,
      chip: self.chip ?? '',
      cores: self.cpu_cores,
      ram: self.ram_gb ?? 0,
      gpu: 0,
      tasks: 0,
      maxTasks: 0,
      queued: 0,
      rtt: 0,
      transport: '',
      trust: '',
      ver: self.version ?? '',
      lastSeen: new Date().toISOString(),
      neighbors: [],
      agents: self.node?.agents ?? {},
      abilities: self.node?.abilities ?? [],
      kind: self.node_kind ?? '',
      tier: self.node?.scheduler_tier ?? 0,
      busy: false,
      work: [] as Task[],
    })
  }
  attachWork(devices, tasks)
  return devices
}

/** File the ledger's tasks under the device that owns them, then derive the
 *  two facts the map is asked for: is this node working, and what has it been
 *  doing. Both ends of the match are normalized because the ledger spells a
 *  node by its runtime id and the directory by its row id. */
function attachWork(devices: Device[], tasks: Task[]): void {
  const byKey = new Map<string, Device>()
  for (const d of devices) {
    for (const raw of [d.id, d.name]) {
      const k = nodeKey(raw)
      if (k && !byKey.has(k)) byKey.set(k, d)
    }
  }
  for (const task of tasks) {
    const owner = byKey.get(nodeKey(task.owner ?? ''))
    if (!owner) continue
    owner.work.push(task)
  }
  for (const d of devices) {
    d.work.sort((a, b) => b.updated_at.localeCompare(a.updated_at))
    const live = d.work.filter((task) => RUNNING_STATES.has(task.state)).length
    d.busy = d.online && (d.tasks > 0 || live > 0)
    // A row's own queue count is authoritative when it reports one; the
    // ledger's pending rows only fill the gap for this console's own store.
    if (d.queued === 0) d.queued = d.work.filter((task) => PENDING_STATES.has(task.state)).length
  }
}

/** How long a finished report keeps its bubble. Long enough to read a stage's
 *  result you were not watching for, short enough that yesterday's work does
 *  not sit over every plate. A task still in flight never expires. */
const SAY_TTL_MS = 30 * 60 * 1000
/** Bubble width and one bubble's height, in CSS pixels. The stylesheet owns the
 *  real numbers; these two exist only to place the column and to decide whether
 *  it hangs above the plate or, on a plate against the top edge, below it. The
 *  height is the three-line worst case, so the estimate errs tall rather than
 *  letting a report run off the top of the map. */
const SAY_W = 190
const SAY_H = 92
/** Characters of a result a bubble shows. A node's report is a sentence, not
 *  the log — the task page keeps the whole thing. */
const SAY_CHARS = 96

/** A plate's own line on the map. One per device: the map is a picture of the
 *  fleet, and a column of history over every plate would be a transcript with
 *  the drawing hidden behind it. */
interface Say {
  key: string
  deviceId: string
  /** run = executing, done/fail/review = what it handed back, plan = the
   *  pipeline's own overview on the machine that started it. */
  tone: 'run' | 'done' | 'fail' | 'review' | 'plan'
  body: string
  /** Second line: the work's title when the body is its result, the stage chain
   *  for a plan. Absent when the body already says it. */
  note?: string
  meta: string
  taskId?: string
  plan?: boolean
}

/** A task the map should still speak for: anything in flight, plus anything
 *  that settled inside the bubble window. */
function speaks(t: Task, now: number): boolean {
  if (RUNNING_STATES.has(t.state) || PENDING_STATES.has(t.state) || t.state === 'review') return true
  const at = Date.parse(t.updated_at)
  return Number.isFinite(at) && now - at < SAY_TTL_MS
}

/** The readable half of a task's result blob, clipped to one bubble. Adapters
 *  disagree about where that text lives (result / answer / stdout / stderr /
 *  failed — see webui/panel/finalize.go), so the same key order is read here;
 *  the blob itself is JSON, and printing it is what the bubble exists to avoid. */
function resultBrief(raw: string | undefined): string {
  const blob = (raw ?? '').trim()
  if (!blob) return ''
  let obj: Record<string, unknown> | null = null
  try {
    const parsed = JSON.parse(blob)
    obj = parsed && typeof parsed === 'object' && !Array.isArray(parsed) ? (parsed as Record<string, unknown>) : null
  } catch {
    // Not JSON: an adapter that wrote prose straight through. Read it as is.
  }
  if (!obj) return clipSay(blob)
  for (const key of ['result', 'answer', 'stdout', 'stderr', 'failed', 'reason']) {
    const v = obj[key]
    if (typeof v === 'string' && v.trim()) return clipSay(v)
  }
  return ''
}

/** One running line out of an agent's output: fences, headings and indentation
 *  are the shape of a console, not of a sentence a bubble can show. */
function clipSay(s: string): string {
  const flat = s
    .replace(/```[A-Za-z0-9_+-]*\n?/g, ' ')
    .replace(/^#{1,6}\s*/gm, '')
    .replace(/\s+/g, ' ')
    .trim()
  const chars = Array.from(flat)
  return chars.length > SAY_CHARS ? chars.slice(0, SAY_CHARS).join('') + '…' : flat
}

function sayTone(state: string): Say['tone'] {
  if (state === 'done') return 'done'
  if (state === 'review') return 'review'
  if (state === 'failed' || state === 'cancelled' || state === 'expired') return 'fail'
  return 'run'
}

/** What one node says about the work it owns. A task in flight reports what it
 *  is working on (its title); a settled one reports what it produced, with the
 *  title demoted to the note line so the bubble still says which job it was. */
function taskSay(device: Device, task: Task, loc: string): Say {
  const stage = task.plan_meta?.stage_id ?? ''
  const title = task.title || stage
  const live = RUNNING_STATES.has(task.state) || PENDING_STATES.has(task.state) || task.state === 'review'
  const brief = live ? '' : resultBrief(task.result)
  return {
    key: task.id,
    deviceId: device.id,
    tone: sayTone(task.state),
    body: brief || title,
    note: brief && title && title !== brief ? title : undefined,
    meta: [t(`state.${task.state}`, task.state), stage, relativeTime(task.updated_at, loc)]
      .filter(Boolean)
      .join(' · '),
    taskId: task.id,
  }
}

/** The plan still worth a bubble: one with stages left to run, or one that
 *  settled inside the window. Newest wins — the map has one voice per machine,
 *  and the pipeline the user just started is the one they are watching. */
function newestPlan(plans: PlanSummary[], now: number): PlanSummary | null {
  let best: PlanSummary | null = null
  for (const p of plans) {
    const live = p.done + p.failed < p.stages || p.review > 0
    const fresh = now / 1000 - p.updated_at < SAY_TTL_MS / 1000
    if (!live && !fresh) continue
    if (!best || p.updated_at > best.updated_at) best = p
  }
  return best
}

/** The pipeline's overview, hung off the machine that started it: the goal it
 *  was launched for, the stages it was cut into, and how far it has come. This
 *  is the "what is being orchestrated" half of the report — the per-node
 *  bubbles are the "what each machine handed back" half. */
function planSay(device: Device, p: PlanSummary, loc: string): Say {
  const settled = p.done + p.failed
  const live = settled < p.stages && p.failed === 0
  const chain = (p.stage_ids ?? []).join(' → ')
  const label = live ? t('workspace.say.plan') : p.failed > 0 ? t('workspace.say.planFailed') : t('workspace.say.planDone')
  return {
    key: `plan:${p.plan_id}`,
    deviceId: device.id,
    tone: live ? 'plan' : p.failed > 0 ? 'fail' : 'done',
    body: clipSay(p.goal || chain || p.plan_id),
    note: p.goal && chain ? chain : undefined,
    meta: [label, `${settled}/${p.stages}`, relativeTime(new Date(p.updated_at * 1000).toISOString(), loc)]
      .filter(Boolean)
      .join(' · '),
    plan: true,
  }
}

function buildEdges(devices: Device[]): Edge[] {
  const known = new Map(devices.map((d) => [d.id, d]))
  const out = new Map<string, Edge>()
  const key = (a: string, b: string) => [a, b].sort().join('\u0000')
  const put = (a: string, b: string, kind: Edge['kind'], rtt: number, transport: string) => {
    const k = key(a, b)
    const prev = out.get(k)
    if (!prev) {
      out.set(k, { a, b, kind, rtt, transport })
      return
    }
    // Both ends agreeing is the stronger claim; a measurement only ever adds
    // the label to a line that already exists.
    if (kind === 'mesh') prev.kind = 'mesh'
    if (rtt > 0 && prev.rtt === 0) {
      prev.rtt = rtt
      prev.transport = transport
    }
  }
  for (const d of devices) {
    for (const nb of d.neighbors) {
      if (nb === d.id || !known.has(nb)) continue
      put(d.id, nb, 'mesh', 0, '')
    }
  }
  // This console's own RTT samples: an edge it measured is live even when
  // the peer never advertised it back (one-way visibility, older build).
  const local = devices.find((d) => d.local)
  if (local) {
    for (const d of devices) {
      if (d.id === local.id || d.rtt <= 0) continue
      put(local.id, d.id, 'measured', d.rtt, d.transport)
    }
  }
  return [...out.values()]
}

/** Ring index = hop distance from this console over the drawn edge set.
 *  Disconnected rows (a directory entry with no live peer) land on the
 *  outermost ring, where "not connected" is visible as distance. */
function ringDepths(devices: Device[], edges: Edge[]): { depth: Map<string, number>; max: number } {
  const adj = new Map<string, string[]>()
  const link = (a: string, b: string) => {
    const list = adj.get(a) ?? []
    list.push(b)
    adj.set(a, list)
  }
  for (const e of edges) {
    link(e.a, e.b)
    link(e.b, e.a)
  }
  const depth = new Map<string, number>()
  const root = devices.find((d) => d.local) ?? devices[0]
  if (!root) return { depth, max: 0 }
  depth.set(root.id, 0)
  const queue = [root.id]
  let max = 0
  while (queue.length > 0) {
    const cur = queue.shift()!
    const d = depth.get(cur) ?? 0
    for (const nb of adj.get(cur) ?? []) {
      if (depth.has(nb)) continue
      depth.set(nb, d + 1)
      max = Math.max(max, d + 1)
      queue.push(nb)
    }
  }
  // Unreached rows go one ring past the deepest reachable one.
  const orphan = devices.some((d) => !depth.has(d.id)) ? max + 1 : max
  for (const d of devices) if (!depth.has(d.id)) depth.set(d.id, orphan)
  return { depth, max: orphan }
}

interface Placed {
  device: Device
  x: number
  y: number
}

function layout(devices: Device[], depth: Map<string, number>, w: number, h: number): Placed[] {
  const cx = w / 2
  const cy = h / 2
  // The outermost ring is bounded by the canvas: half the short side, minus
  // the plate and the label block under it.
  const maxR = Math.max(
    90,
    Math.min(w / 2 - PLATE / 2 - 16, h / 2 - PLATE / 2 - LABEL_H - 22),
  )
  // Two plates on adjacent rings need a full plate + label between them, or
  // the outer one's plate covers the inner one's name. When the canvas cannot
  // pay for one ring per hop, deeper nodes collapse onto the outermost ring
  // (their exact hop count still shows in the rail's detail card).
  const minStep = PLATE + LABEL_H
  const rawMax = Math.max(1, ...devices.map((d) => depth.get(d.id) ?? 0))
  const maxRing = rawMax > 1 ? Math.max(1, Math.min(rawMax, Math.floor(maxR / minStep))) : 1
  const ringOf = (d: Device) => Math.min(depth.get(d.id) ?? 0, maxRing)

  const byRing = new Map<number, Device[]>()
  for (const d of devices) {
    const r = ringOf(d)
    const list = byRing.get(r) ?? []
    list.push(d)
    byRing.set(r, list)
  }
  // Equal radial steps: the gap between rings is what keeps labels readable,
  // so it must not be squeezed by an inner-ring offset.
  const radiusOf = (ring: number) => (maxRing === 1 ? maxR : (maxR * ring) / maxRing)
  const out: Placed[] = []
  for (const [ring, list] of byRing) {
    const sorted = [...list].sort((a, b) => a.name.localeCompare(b.name))
    if (ring === 0) {
      out.push({ device: sorted[0]!, x: cx, y: cy })
      continue
    }
    // A ring must also be long enough to hold its own plates: circumference
    // ≥ n × (plate + gap). With one node on it, the geometric radius wins.
    const needed = (sorted.length * (PLATE + NODE_GAP)) / (2 * Math.PI)
    const radius = Math.max(radiusOf(ring), sorted.length > 1 ? needed : 0)
    // Rings are ellipses: the canvas is wider than it is tall, and a circle
    // would leave two thirds of the map empty on either side.
    const rx = Math.min(w / 2 - PLATE / 2 - 20, radius * 1.55)
    sorted.forEach((d, i) => {
      // Half-step offset per ring so a ring's plates never sit exactly on
      // the spokes of the one inside it.
      const angle = -Math.PI / 2 + ((i + 0.5) / sorted.length) * Math.PI * 2 + (ring % 2 ? 0.22 : 0)
      out.push({ device: d, x: cx + Math.cos(angle) * rx, y: cy + Math.sin(angle) * radius })
    })
  }
  return centerPlaced(out, w, h)
}

/** Shift the whole drawing so its ink — plates plus the label block under
 *  them — is centred in the canvas. Rings are concentric around this
 *  console, but the deepest ring is rarely symmetric, so the hub alone is
 *  not the visual centre. The shift is clamped to the canvas so a wide
 *  fleet can never push a plate off the edge. */
function centerPlaced(placed: Placed[], w: number, h: number): Placed[] {
  if (placed.length < 2) return placed
  let minX = Infinity
  let maxX = -Infinity
  let minY = Infinity
  let maxY = -Infinity
  for (const p of placed) {
    // Rough text metrics: 6.6px per character at the label's 12px, and the
    // os line is the widest fixed part of the block.
    const half = Math.max(PLATE / 2, p.device.name.length * 3.6, 40)
    minX = Math.min(minX, p.x - half)
    maxX = Math.max(maxX, p.x + half)
    minY = Math.min(minY, p.y - PLATE / 2 - 6)
    maxY = Math.max(maxY, p.y + PLATE / 2 + LABEL_H)
  }
  const dx = clampShift(w / 2 - (minX + maxX) / 2, 10 - minX, w - 10 - maxX)
  const dy = clampShift(h / 2 - (minY + maxY) / 2, 10 - minY, h - 10 - maxY)
  return placed.map((p) => ({ ...p, x: p.x + dx, y: p.y + dy }))
}

/** Clamp a desired shift into [lo, hi]; when the ink is wider than the
 *  canvas (lo > hi) the low bound wins so the leading edge stays visible. */
function clampShift(want: number, lo: number, hi: number): number {
  if (lo > hi) return lo
  return Math.min(hi, Math.max(lo, want))
}

/** A device glyph in a 24×24 box centered on the origin — screen+base for a
 *  laptop, a tower for a desktop, a low wide box for a mini, a rack for a
 *  server, a dashed outline for a guest. Stroke only, so the theme's ink
 *  color carries through (the plates are monochrome by design: hue is
 *  reserved for status). */
function DeviceGlyph({ form }: { form: Form }) {
  switch (form) {
    case 'laptop':
      return (
        <>
          <rect x={-9} y={-8} width={18} height={12} rx={1.8} />
          <path d="M-12 6.4 H12 L10 9 H-10 Z" />
        </>
      )
    case 'desktop':
      return (
        <>
          <rect x={-7.5} y={-10.5} width={15} height={21} rx={2.4} />
          <path d="M-4.2 -6.8 h8.4" />
          <path d="M-4.2 -3.8 h8.4" />
          <circle cx={-4} cy={6.4} r={1.15} />
        </>
      )
    case 'mini':
      return (
        <>
          <rect x={-10.5} y={-5.4} width={21} height={10.8} rx={3} />
          <path d="M-10.5 -1.8 H10.5" opacity={0.45} />
          <circle cx={6.6} cy={2.6} r={0.95} />
        </>
      )
    case 'server':
      return (
        <>
          <rect x={-8.5} y={-9.5} width={17} height={5.4} rx={1.4} />
          <rect x={-8.5} y={-2.7} width={17} height={5.4} rx={1.4} />
          <rect x={-8.5} y={4.1} width={17} height={5.4} rx={1.4} />
          <circle cx={5} cy={-6.8} r={0.9} />
          <circle cx={5} cy={0} r={0.9} />
          <circle cx={5} cy={6.8} r={0.9} />
        </>
      )
    case 'vm':
      return (
        <>
          <rect x={-9.5} y={-8.5} width={19} height={17} rx={3} stroke-dasharray="3 2.6" />
          <rect x={-4.6} y={-3.6} width={9.2} height={7.2} rx={1.4} />
          <path d="M-9.5 1.5 H-6.4 M6.4 1.5 H9.5" opacity={0.4} />
        </>
      )
    default:
      return (
        <>
          <rect x={-8.5} y={-8.5} width={17} height={17} rx={3.2} stroke-dasharray="2.6 2.6" />
          <text
            x={0}
            y={3.6}
            text-anchor="middle"
            font-size="10"
            font-weight="600"
            fill="currentColor"
            stroke="none"
          >
            ?
          </text>
        </>
      )
  }
}

export function WorkspaceView() {
  useLocaleRerender()
  const change = useChangeSignal()
  const [tick, setTick] = useState(0)
  const { data: nodes, error } = useAsync(() => api.nodes(), [], change + tick)
  const { data: self } = useAsync(() => api.self(), [], tick)
  // The ledger answers both questions the panel asks about a node: is it
  // holding a slot right now, and what has it been working on. Same read the
  // queue board makes, refreshed by the change feed rather than the 15s tick —
  // a task that starts or finishes must move the plate, not wait for a poll.
  const { data: tasks } = useAsync(() => api.tasks(), [], change + tick)
  // The plan board, for the one bubble the map puts on the orchestrator itself:
  // a multi-machine run has no transcript on this page, so "what am I running
  // across these machines, and how far along is it" has to be said on the map.
  const { data: plans } = useAsync(() => api.plans(), [], change + tick)
  // This node's drivable CLIs: the fallback when a local capability card
  // declares no agents (a fresh install has a card with hardware only).
  const { data: probedAgents } = useAsync(() => api.agents(), [], tick)
  const [selected, setSelected] = useState<string | null>(null)
  const [hover, setHover] = useState<string | null>(null)
  // The hover card is a read, not a selection: it waits a beat so sweeping
  // the cursor across the map does not flash a card per plate, and it never
  // steals the click that opens the rail.
  const [peek, setPeek] = useState<string | null>(null)
  const peekTimer = useRef<ReturnType<typeof setTimeout> | null>(null)
  const [view, setView] = useState<View>(VIEW_HOME)
  // The rail is a reading panel, not part of the map: it starts folded so the
  // drawing owns the pane, and opens when the reader asks for it — either by
  // the header toggle or by picking a device, whose facts only live in there.
  const [railOpen, setRailOpen] = useState(false)
  const [size, setSize] = useState({ w: 860, h: 520 })
  const [panning, setPanning] = useState(false)
  const wrap = useRef<HTMLDivElement>(null)
  const svgRef = useRef<SVGSVGElement>(null)
  const drag = useRef<{ px: number; py: number; vx: number; vy: number } | null>(null)

  // The bench's own state: where the next task runs, and which thread of that
  // directory it continues (null = mint a new one). It lives up here because
  // the header's orchestration card needs the resolved project — the card
  // answers "what has this directory produced", which is the only feedback
  // this page has, since it never paints a transcript.
  const [pickedDir, setPickedDir] = useState('')
  const [dirTouched, setDirTouched] = useState(false)
  const [pickedSession, setPickedSession] = useState<Session | null>(null)
  const [orchOpen, setOrchOpen] = useState(false)

  // Heartbeats land on the directory every ~15s; poll at the same cadence so
  // a device that goes quiet fades without a manual reload. SSE covers the
  // structural changes (a node joining, a task moving).
  useVisibleInterval(() => setTick((v) => v + 1), 15000)

  // The project directory table, refreshed on the change feed: a send that
  // mints a project (a directory nothing pointed at yet) has to show up here
  // before the picker can list what it now owns.
  const { data: projects } = useAsync(() => api.projects(), [], tick)

  // Seed the bench with the active project's directory, the way the thread
  // composer's footer does: the pick is a statement first and a control
  // second, and an empty footer would make the history picker useless on
  // arrival. A directory the user chose always wins over the seed.
  const seeded = useRef(false)
  useEffect(() => {
    if (seeded.current || dirTouched || !projects) return
    seeded.current = true
    const active = (projects.detail ?? []).find((d) => d.name === projects.active)
    if (active?.work_dir) setPickedDir(active.work_dir)
  }, [projects, dirTouched])

  const project = useMemo(() => projectOfDir(projects?.detail, pickedDir), [projects, pickedDir])

  // The directory's threads, newest first. Scoped by project because a
  // session's work dir is read from its project row — the picker can only
  // offer what a send can actually continue. It follows the page's own 15s
  // tick rather than the task feed: reading every session file on each task
  // event would be a lot of I/O for a list that changes when a thread does,
  // not when a task does.
  const { data: dirSessions } = useAsync(
    () => (project ? api.sessions(project) : Promise.resolve([] as Session[])),
    [project],
    tick,
  )

  useEffect(() => {
    const el = wrap.current
    if (!el) return
    const ro = new ResizeObserver(() => {
      const r = el.getBoundingClientRect()
      // The drawing is sized to the box it is given (the pane fills the
      // window), so the viewBox tracks the measured height as well — an
      // aspect-corrected viewBox would letterbox the canvas instead.
      if (r.width > 0 && r.height > 0) setSize({ w: r.width, h: r.height })
    })
    ro.observe(el)
    return () => ro.disconnect()
  }, [])

  const devices = useMemo(() => buildDevices(nodes ?? [], self, tasks ?? []), [nodes, self, tasks])
  const edges = useMemo(() => buildEdges(devices), [devices])
  const { depth } = useMemo(() => ringDepths(devices, edges), [devices, edges])
  const placed = useMemo(() => layout(devices, depth, size.w, size.h), [devices, depth, size.w, size.h])
  const posOf = useMemo(() => new Map(placed.map((p) => [p.device.id, p])), [placed])
  const deviceOf = useMemo(() => new Map(devices.map((d) => [d.id, d])), [devices])

  // Hovering a plate lights its own lines; hovering a line's endpoints lights
  // the line. Either way everything unrelated fades, which is what makes a
  // five-device mesh readable at a glance.
  const litEdges = useMemo(() => {
    const set = new Set<number>()
    if (!hover) return set
    edges.forEach((e, i) => {
      if (e.a === hover || e.b === hover) set.add(i)
    })
    return set
  }, [edges, hover])

  const neighborsOf = useMemo(() => {
    const m = new Map<string, Set<string>>()
    const add = (a: string, b: string) => {
      const set = m.get(a) ?? new Set<string>()
      set.add(b)
      m.set(a, set)
    }
    for (const e of edges) {
      add(e.a, e.b)
      add(e.b, e.a)
    }
    return m
  }, [edges])

  const zoomAt = (cx: number, cy: number, factor: number) => {
    setView((v) => {
      const k = Math.min(ZOOM_MAX, Math.max(ZOOM_MIN, v.k * factor))
      const s = k / v.k
      return { k, x: cx - (cx - v.x) * s, y: cy - (cy - v.y) * s }
    })
  }

  // Wheel zoom needs a non-passive listener: a synthetic handler cannot
  // preventDefault, and the page would scroll behind the map.
  useEffect(() => {
    const el = svgRef.current
    if (!el) return
    const onWheel = (e: WheelEvent) => {
      e.preventDefault()
      const r = el.getBoundingClientRect()
      const s = size.w / r.width
      zoomAt((e.clientX - r.left) * s, (e.clientY - r.top) * s, e.deltaY < 0 ? 1.12 : 1 / 1.12)
    }
    el.addEventListener('wheel', onWheel, { passive: false })
    return () => el.removeEventListener('wheel', onWheel)
  }, [size.w])

  const sel = selected ? deviceOf.get(selected) ?? null : null
  // Hover reads twice: the plate lights its edges at once (the map's own
  // language), and the fact card follows a beat later. One timer, cleared on
  // leave and on unmount, so a fast sweep never leaves a card behind.
  const hoverIn = (id: string) => {
    setHover(id)
    if (peekTimer.current) clearTimeout(peekTimer.current)
    peekTimer.current = setTimeout(() => setPeek(id), 140)
  }
  const hoverOut = (id: string) => {
    setHover((v) => (v === id ? null : v))
    if (peekTimer.current) clearTimeout(peekTimer.current)
    peekTimer.current = null
    setPeek((v) => (v === id ? null : v))
  }
  useEffect(() => () => {
    if (peekTimer.current) clearTimeout(peekTimer.current)
  }, [])
  // Panning is a gesture, not a read: a card hanging off the cursor while the
  // fleet slides under it would lie about which plate it describes.
  useEffect(() => {
    if (panning) setPeek(null)
  }, [panning])

  // The picked device's card is already open on the right; repeating its
  // header over the map would be noise, and the panel is the fuller answer.
  const peekDevice = peek && !panning && peek !== selected ? deviceOf.get(peek) ?? null : null
  // The card is HTML over the canvas, not an SVG child: at 1× the map's type
  // is 9–11px, and a tooltip that scaled with the zoom would be unreadable at
  // 0.5× and enormous at 3×. Its anchor is the plate's own graph position
  // mapped through the current pan/zoom, so it tracks the device exactly.
  const peekPos = useMemo(() => {
    if (!peekDevice) return null
    const p = posOf.get(peekDevice.id)
    if (!p) return null
    const cx = p.x * view.k + view.x
    const cy = p.y * view.k + view.y
    const half = PEEK_W / 2
    const left = Math.min(Math.max(cx, half + 10), Math.max(half + 10, size.w - half - 10))
    const rim = (PLATE / 2) * view.k
    const above = cy - rim - 12
    // Above the plate is the reading position; the label block below it is
    // the fallback for a plate on the top edge. Either way the card never
    // covers the device it describes.
    const below = above - PEEK_H < 8
    return { left, top: below ? cy + rim + (LABEL_H + 6) * view.k : above, below }
  }, [peekDevice, posOf, view, size.w])

  // The map's voices: one bubble per plate saying what that machine is doing or
  // what it just handed back, plus the pipeline's overview on the machine that
  // started it. This is the whole of the "a node reports when it finishes"
  // rule — this page never opens a transcript, so the report has to live where
  // the fleet already is.
  const says = useMemo(() => {
    const loc = locale()
    const now = Date.now()
    const out: Say[] = []
    for (const d of devices) {
      // d.work is newest first, so the first row still worth a bubble is the
      // node's latest word on the subject.
      const task = d.work.find((x) => speaks(x, now))
      if (task) out.push(taskSay(d, task, loc))
    }
    const plan = newestPlan(plans ?? [], now)
    const orchestrator = devices.find((d) => d.local)
    if (plan && orchestrator) out.push(planSay(orchestrator, plan, loc))
    return out
  }, [devices, plans, locale()])

  // Placement: the column hangs off the plate's own graph position mapped
  // through pan/zoom, exactly like the hover card — it tracks the device at
  // every scale instead of being resized by it.
  const sayLayers = useMemo(() => {
    const byDevice = new Map<string, Say[]>()
    for (const s of says) {
      const list = byDevice.get(s.deviceId) ?? []
      list.push(s)
      byDevice.set(s.deviceId, list)
    }
    const out: { id: string; says: Say[]; left: number; top: number; below: boolean; dim: boolean }[] = []
    for (const [id, list] of byDevice) {
      // The hover card owns the space above a plate while it is up: two
      // surfaces saying different things from the same anchor read as one
      // contradictory card.
      if (peekDevice?.id === id) continue
      const p = posOf.get(id)
      if (!p) continue
      const cx = p.x * view.k + view.x
      const cy = p.y * view.k + view.y
      const rim = (PLATE / 2) * view.k
      const half = SAY_W / 2
      const left = Math.min(Math.max(cx, half + 10), Math.max(half + 10, size.w - half - 10))
      // The pipeline overview reads first — it is the older news — and the
      // node's own report sits under it, closest to the plate that said it.
      list.sort((a, b) => Number(b.plan ?? false) - Number(a.plan ?? false))
      const height = list.length * SAY_H + (list.length - 1) * 6
      const below = cy - rim - 12 - height < 8
      const nb = neighborsOf.get(id)
      out.push({
        id,
        says: list,
        left,
        top: below ? cy + rim + (LABEL_H + 6) * view.k : cy - rim - 12,
        below,
        dim: hover !== null && hover !== id && !(nb?.has(hover) ?? false),
      })
    }
    return out
  }, [says, posOf, neighborsOf, view, size.w, peekDevice, hover])

  // The two cards hang off the same corner and reserve the same lane, so only
  // one can be up at a time: stacking them would paint the orchestration card
  // over the device list, and the composer — which shifts by the lane — would
  // land somewhere different depending on which one was clicked last.
  const toggleRail = () => {
    const next = !railOpen
    setRailOpen(next)
    if (next) {
      setOrchOpen(false)
      // The toggle is the device *list*; a device read before the panel was
      // folded is a different question. Picking a plate opens the detail
      // directly, so nothing is lost by starting here.
      setSelected(null)
    }
  }
  const toggleOrch = (open: boolean) => {
    setOrchOpen(open)
    if (open) setRailOpen(false)
  }
  // Picking a device is a request for its facts, and those only live in the
  // rail — so a pick opens the panel it writes to. Clearing a selection
  // leaves the panel's own state alone.
  const pick = (id: string | null) => {
    setSelected(id)
    setPeek(null)
    if (id) toggleOrch(false)
    if (id) setRailOpen(true)
  }
  // The card's own ×: a close button on a panel puts the panel away, it does
  // not leave a list standing in place of the facts that were being read.
  const closeRail = () => {
    setSelected(null)
    setRailOpen(false)
  }
  // Back, not close: the panel stays up and returns to the fleet list, which
  // is the one place to switch devices without hunting for a plate.
  const backToList = () => setSelected(null)

  // Escape closes the card, like the two cards on a thread: the trigger and
  // the key are the only two ways out, so a click on the map never puts the
  // list away mid-read.
  useEffect(() => {
    if (!railOpen) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape' || layerAboveOwnsEscape()) return
      e.stopPropagation()
      setRailOpen(false)
    }
    document.addEventListener('keydown', onKey, true)
    return () => document.removeEventListener('keydown', onKey, true)
  }, [railOpen])

  if (error)
    return (
      <ErrorState
        title={t('nav.workspace')}
        sub={t('workspace.subtitle')}
        error={error}
        onRetry={() => setTick((v) => v + 1)}
      />
    )

  return (
    <section
      class={`ws-page${orchOpen || railOpen ? ' panel-open' : ''}${railOpen && sel ? ' detail-open' : ''}`}
    >
      {/* Same top edge as a thread: a mark, the name, the key for the
          drawing, and the two controls the page has. The drawing below is the
          page — no counts, no toolbar. The orchestration card is the exception:
          this surface never shows a transcript, so the tasks a directory has
          produced are the only place a dispatched turn becomes visible. It sits
          at the far right, exactly where the chat's own card hangs. */}
      <header class="ws-head">
        <Icon name="devices" size={16} class="ws-head-icon" />
        <span class="ws-title">{t('nav.workspace')}</span>
        {/* The key reads as a subtitle rather than a footer: one line of
            glyph + name for the six machine classes the plates encode. The
            two line styles are still named by the edges' own tooltips. */}
        <div class="ws-legend">
          {FORMS.map((f) => (
            <span class="ws-legend-item" key={f}>
              <svg viewBox="-14 -14 28 28" aria-hidden="true">
                <DeviceGlyph form={f} />
              </svg>
              {formLabel(f)}
            </span>
          ))}
        </div>
        <div class="ws-head-tools">
          {/* The rail is a card, not a column: it hangs off the same corner as
              the orchestration card beside it, over the drawing. Taking a grid
              column instead made the map narrower, which re-laid the plates out
              under the reader's cursor every time the list opened — the fleet
              moved because a list appeared. */}
          <div class="ws-rail-picker">
            <button
              type="button"
              class={`icon-btn ws-rail-toggle${railOpen ? ' on' : ''}`}
              aria-label={t('workspace.rail')}
              aria-expanded={railOpen}
              title={t('workspace.rail')}
              onClick={toggleRail}
            >
              <Icon name="panel-right" size={16} />
            </button>
            {railOpen && (
              <aside
                class={`ws-rail${sel ? ' detail' : ''}`}
                role="dialog"
                aria-label={t('workspace.rail')}
              >
                {sel ? (
                  <DeviceDetail
                    device={sel}
                    devices={deviceOf}
                    probedAgents={sel.local ? probedAgents ?? [] : []}
                    onPick={pick}
                    onBack={backToList}
                    onClose={closeRail}
                  />
                ) : (
                  <div class="ws-list">
                    {devices.map((d) => (
                      <button
                        type="button"
                        key={d.id}
                        class={`ws-row${selected === d.id ? ' sel' : ''}${d.online ? '' : ' off'}`}
                        onClick={() => pick(d.id)}
                        onMouseEnter={() => setHover(d.id)}
                        onMouseLeave={() => setHover((v) => (v === d.id ? null : v))}
                      >
                        <svg class="ws-row-glyph" viewBox="-14 -14 28 28" aria-hidden="true">
                          <DeviceGlyph form={d.form} />
                        </svg>
                        <span class="ws-row-text">
                          <span class="ws-row-name">{d.name}</span>
                          <span class="ws-row-meta dim">
                            {osLabel(d.os)} · {formLabel(d.form)}
                          </span>
                        </span>
                        <span
                          class={`ws-row-dot${d.online ? '' : ' off'}${d.busy ? ' busy' : ''}`}
                          aria-hidden="true"
                        />
                        <span class="ws-row-rtt mono dim">{d.rtt > 0 ? `${d.rtt} ms` : ''}</span>
                      </button>
                    ))}
                  </div>
                )}
              </aside>
            )}
          </div>
          <OrchestrateCard
            key={`orch-${project || 'none'}`}
            project={project || undefined}
            open={orchOpen}
            onOpenChange={toggleOrch}
            onlineNodeCount={devices.filter((d) => d.online).length}
            selfNodeId={devices.find((d) => d.local)?.id}
          />
        </div>
      </header>

      <div class="ws-body">
        <div class="ws-main">
          <div class={`ws-map${panning ? ' panning' : ''}`} ref={wrap}>
            {devices.length === 0 ? (
              <p class="dim ws-empty">{t('workspace.empty')}</p>
            ) : (
              <svg
                ref={svgRef}
                class="ws-canvas"
                viewBox={`0 0 ${size.w} ${size.h}`}
                role="img"
                aria-label={t('nav.workspace')}
                onDblClick={(e) => {
                  // Double-click on empty canvas recentres the drawing — the
                  // page has no reset button any more, and a wheel zoom with
                  // no way home is a trap. A double-click on a plate is a
                  // double pick, not a reset.
                  if ((e.target as Element).classList?.contains('ws-bg')) setView(VIEW_HOME)
                }}
                onPointerDown={(e) => {
                  const el = e.target as Element
                  if (!el.classList?.contains('ws-bg')) return
                  el.setPointerCapture?.(e.pointerId)
                  drag.current = { px: e.clientX, py: e.clientY, vx: view.x, vy: view.y }
                  setPanning(true)
                }}
                onPointerMove={(e) => {
                  const d = drag.current
                  if (!d) return
                  const r = svgRef.current!.getBoundingClientRect()
                  const s = size.w / r.width
                  setView((v) => ({
                    ...v,
                    x: d.vx + (e.clientX - d.px) * s,
                    y: d.vy + (e.clientY - d.py) * s,
                  }))
                }}
                onPointerUp={() => {
                  drag.current = null
                  setPanning(false)
                }}
                onPointerCancel={() => {
                  drag.current = null
                  setPanning(false)
                }}
              >
                <defs>
                  <pattern id="ws-dots" width="26" height="26" patternUnits="userSpaceOnUse">
                    <circle cx="1.1" cy="1.1" r="1.1" style={{ fill: 'var(--border-soft)' }} />
                  </pattern>
                  <filter id="ws-plate-shadow" x="-60%" y="-60%" width="220%" height="220%">
                    <feDropShadow dx="0" dy="1.4" stdDeviation="2.2" flood-opacity="0.16" />
                  </filter>
                  <filter id="ws-edge-glow" x="-40%" y="-40%" width="180%" height="180%">
                    <feDropShadow
                      dx="0"
                      dy="0"
                      stdDeviation="2.2"
                      style={{ floodColor: 'var(--accent)', floodOpacity: 0.5 }}
                    />
                  </filter>
                </defs>
                <rect width={size.w} height={size.h} fill="url(#ws-dots)" opacity="0.5" />
                <g transform={`translate(${view.x} ${view.y}) scale(${view.k})`}>
                  <rect
                    class="ws-bg"
                    x={-size.w * 3}
                    y={-size.h * 3}
                    width={size.w * 7}
                    height={size.h * 7}
                  />
                  {edges.map((e, i) => {
                    const a = posOf.get(e.a)
                    const b = posOf.get(e.b)
                    if (!a || !b) return null
                    const dx = b.x - a.x
                    const dy = b.y - a.y
                    const dist = Math.max(Math.hypot(dx, dy), 1)
                    const ux = dx / dist
                    const uy = dy / dist
                    // Stop at the plate rim so the line meets the device rather
                    // than disappearing under it.
                    const gap = PLATE / 2 + 5
                    const x1 = a.x + ux * gap
                    const y1 = a.y + uy * gap
                    const x2 = b.x - ux * gap
                    const y2 = b.y - uy * gap
                    const lit = litEdges.has(i)
                    const dim = hover !== null && !lit
                    return (
                      <g key={`${e.a}-${e.b}`} class={`ws-edge-g${dim ? ' dim' : ''}${lit ? ' lit' : ''}`}>
                        <line
                          class={`ws-edge ws-edge-${e.kind}`}
                          x1={x1}
                          y1={y1}
                          x2={x2}
                          y2={y2}
                        />
                        <line class="ws-edge-hit" x1={x1} y1={y1} x2={x2} y2={y2}>
                          <title>
                            {e.kind === 'mesh' ? t('workspace.edge.mesh') : t('workspace.edge.measured')}
                            {e.rtt > 0 ? ` · ${e.rtt} ms` : ''}
                          </title>
                        </line>
                      </g>
                    )
                  })}
                  {placed.map(({ device: d, x, y }) => {
                    const nb = neighborsOf.get(d.id)
                    const dim = hover !== null && hover !== d.id && !(nb?.has(hover) ?? false)
                    return (
                      <g
                        key={d.id}
                        class={`ws-node${d.online ? '' : ' off'}${d.local ? ' self' : ''}${
                          selected === d.id ? ' sel' : ''
                        }${dim ? ' dim' : ''}`}
                        transform={`translate(${x},${y})`}
                        role="button"
                        tabIndex={0}
                        aria-label={`${d.name} · ${formLabel(d.form)} · ${osLabel(d.os)} · ${workStateLabel(d)}`}
                        aria-pressed={selected === d.id}
                        onMouseEnter={() => hoverIn(d.id)}
                        onMouseLeave={() => hoverOut(d.id)}
                        onFocus={() => setHover(d.id)}
                        onBlur={() => hoverOut(d.id)}
                        onClick={() => pick(selected === d.id ? null : d.id)}
                        onKeyDown={(e) => {
                          if (e.key === 'Enter' || e.key === ' ') {
                            e.preventDefault()
                            pick(selected === d.id ? null : d.id)
                          } else if (e.key === 'Escape') {
                            setSelected(null)
                          }
                        }}
                      >
                        {selected === d.id && <circle class="ws-halo" r={47} />}
                        <rect
                          class="ws-plate"
                          x={-31}
                          y={-31}
                          width={62}
                          height={62}
                          rx={16}
                          filter="url(#ws-plate-shadow)"
                        />
                        <g class="ws-glyph" transform="scale(1.5)">
                          <DeviceGlyph form={d.form} />
                        </g>
                        <circle
                          class={`ws-status${d.busy ? ' busy' : ''}`}
                          cx={23}
                          cy={-23}
                          r={3.4}
                        />
                        {d.local && (
                          <text class="ws-tag" y={27} text-anchor="middle">
                            {t('workspace.local')}
                          </text>
                        )}
                        <text class="ws-name" y={46} text-anchor="middle">
                          {d.name.length > 16 ? `${d.name.slice(0, 15)}…` : d.name}
                        </text>
                        <text class="ws-os" y={59} text-anchor="middle">
                          {osLabel(d.os)} · {formLabel(d.form)}
                        </text>
                      </g>
                    )
                  })}
                </g>
              </svg>
            )}
            {sayLayers.map((col) => (
              <div
                key={`say-${col.id}`}
                class={`ws-says${col.below ? ' below' : ''}${col.dim ? ' dim' : ''}`}
                style={{ left: `${col.left}px`, top: `${col.top}px` }}
              >
                {col.says.map((s) => (
                  <button
                    key={s.key}
                    type="button"
                    class={`ws-say ${s.tone}`}
                    // The bubble is the report. The rest of it lives one click
                    // away: a plan has no page of its own — its stages are rows
                    // in the orchestration card — and a task has its detail page.
                    title={s.plan ? t('workspace.say.planOpen') : t('workspace.say.open')}
                    onClick={() => {
                      if (s.plan) toggleOrch(true)
                      else if (s.taskId) navigate({ view: 'detail', id: s.taskId })
                    }}
                  >
                    <span class="ws-say-body">{s.body}</span>
                    {s.note && <span class="ws-say-note">{s.note}</span>}
                    <span class="ws-say-meta">{s.meta}</span>
                  </button>
                ))}
              </div>
            ))}
            {peekDevice && peekPos && (
              <div
                class={`ws-peek${peekPos.below ? ' below' : ''}`}
                style={{ left: `${peekPos.left}px`, top: `${peekPos.top}px` }}
                role="tooltip"
              >
                <div class="ws-peek-head">
                  <span
                    class={`ws-peek-dot${peekDevice.online ? (peekDevice.busy ? ' busy' : '') : ' off'}`}
                    aria-hidden="true"
                  />
                  <span class="ws-peek-state">{workStateLabel(peekDevice)}</span>
                  {peekDevice.local && <span class="ws-peek-tag">{t('workspace.local')}</span>}
                </div>
                <p class="ws-peek-name">{peekDevice.name}</p>
                <p class="ws-peek-spec">{specLine(peekDevice)}</p>
                <div class="ws-peek-foot">
                  <span class="ws-peek-load">{loadLine(peekDevice)}</span>
                  <span class="ws-peek-hint">{t('workspace.peek.hint')}</span>
                </div>
              </div>
            )}
          </div>
        </div>
      </div>

      {/* The chat composer, reused whole and docked exactly where 新对话 has
          it: same box, same type, same Enter rules, same footer. The footer
          now carries both halves of the bench — the directory, and the thread
          of that directory the next task is appended to — and a send stays
          here: no thread opens, no transcript is painted, the orchestration
          card is where the dispatched work becomes visible. */}
      <WorkspaceComposer
        dir={pickedDir}
        onDirChange={(path) => {
          setPickedDir(path)
          setDirTouched(true)
          // A thread belongs to the directory it was listed under; keeping it
          // picked across a directory switch would silently append the next
          // task to a conversation from somewhere else.
          setPickedSession(null)
        }}
        project={project}
        sessions={dirSessions ?? []}
        picked={pickedSession}
        onPick={setPickedSession}
        onSent={(s) => {
          // A new thread becomes the pick: the next send continues what this
          // one started, the same rule the chat composer follows.
          setPickedSession(s)
          setTick((v) => v + 1)
          // Feedback, since there is no transcript to watch: the card that
          // answers "what did I just dispatch" is the task list.
          toggleOrch(true)
        }}
      />
    </section>
  )
}

/** Resolve a picked directory to the project whose work dir it is, creating
 *  the project when no row points at it yet — the same rule the thread
 *  composer's footer follows. */
async function projectForDir(dir: string): Promise<string> {
  const pl = await api.projects()
  const norm = (p: string) => p.replace(/[\\/]+$/, '').toLowerCase()
  const want = norm(dir)
  const hit = (pl.detail ?? []).find((d) => d.work_dir && norm(d.work_dir) === want)
  if (hit) return hit.name
  const taken = new Set(pl.projects ?? [])
  const base = suggestProjectName(dir) || 'project'
  let name = base
  for (let i = 2; taken.has(name); i++) name = `${base}-${i}`
  const created = await api.createProject({ name, work_dir: dir })
  return created.name
}

/** The read-only half of the rule above: which project already owns this
 *  directory, or '' when none does. The picker needs the answer without the
 *  side effect — opening 工作空间 on a directory that was never used must not
 *  mint a project just to say "no history here yet". */
function projectOfDir(
  detail: Array<{ name: string; work_dir?: string }> | undefined,
  dir: string,
): string {
  if (!dir) return ''
  const norm = (p: string) => p.replace(/[\\/]+$/, '').toLowerCase()
  const want = norm(dir)
  return (detail ?? []).find((d) => d.work_dir && norm(d.work_dir) === want)?.name ?? ''
}

/** The chat composer, reused whole: same box, same type, same Enter rules,
 *  same footer. What a send *does* is the difference — it is a dispatch, not
 *  a turn in the page you are looking at:
 *
 *  - the target thread is the footer's pick, or a brand-new one when nothing
 *    is picked (titled after the prompt, like 新对话);
 *  - the stream is fired and never awaited. There is no transcript here, so
 *    there is nothing to paint: the turn runs on the server and the header's
 *    task card is where its progress shows up;
 *  - nothing navigates. Not on send, not after it — staying on the map is the
 *    whole point of the surface, and leaving the page must not cancel work. */
function WorkspaceComposer(props: {
  dir: string
  onDirChange(path: string): void
  /** The project the picked directory resolves to; '' when nothing owns it. */
  project: string
  sessions: Session[]
  picked: Session | null
  onPick(session: Session | null): void
  /** Called with the thread the task was sent to, after the stream is up. */
  onSent(session: Session): void
}) {
  useLocaleRerender()
  const composer = useRef<HTMLDivElement>(null)
  const [input, setInput] = useState('')
  const [sending, setSending] = useState(false)
  const [authorize, setAuthorize] = useState(false)

  useEffect(() => {
    composer.current?.focus()
    // No abort on unmount on purpose: this is a dispatch surface, and a turn
    // that survives the page being left is the feature, not a leak.
  }, [])

  async function send(e?: Event) {
    e?.preventDefault()
    const raw = input.trim()
    if (!raw || sending) return
    setSending(true)
    try {
      // The directory resolves first: a session's work dir is read from its
      // project row, so a directory no project points at would be silently
      // ignored. An existing project is reused; a new directory gets one
      // minted so the thread — and its tasks — are filed where the picker can
      // find them next time.
      let threadProject: string | undefined = props.project || undefined
      if (!threadProject && props.dir) {
        try {
          threadProject = await projectForDir(props.dir)
        } catch (err) {
          // The send matters more than the pick — report and fall back to the
          // default project, the same trade the thread composer makes.
          toastError(err)
        }
      }
      // @file references expand to fenced blocks before the prompt leaves —
      // the same rule the thread composer applies.
      const expanded = await expandFileRefs(raw, (p) => api.fsRead(p))
      const prompt = expanded.prompt
      const target =
        props.picked ?? (await api.createSession(prompt.slice(0, 48), threadProject))
      void askSessionStream(
        target.id,
        prompt,
        authorize,
        {
          onDelta() {},
          onReasoning() {},
          onStatus() {},
          onOperation() {},
          onResult() {},
          // The one handler that has to do something: with no transcript, a
          // turn that failed (no model, dead key, rejected prompt) would
          // otherwise vanish without a trace.
          onError(msg) {
            toast(msg, 'error')
          },
        },
        undefined,
      ).catch(toastError)
      editorClear(composer.current)
      setInput('')
      props.onSent(target)
      toast(
        t('workspace.sentToast', { title: target.title || t('sessions.untitled') }),
        'success',
      )
    } catch (err) {
      toastError(err)
    } finally {
      setSending(false)
    }
  }

  return (
    <form class="composer" onSubmit={send}>
      <div class="composer-box">
        <div
          ref={(el) => {
            composer.current = el
          }}
          class={`composer-input${editorPlain(input) === '' ? ' is-empty' : ''}`}
          contenteditable="true"
          role="textbox"
          aria-multiline="true"
          aria-label={t('sessions.placeholder')}
          data-placeholder={t('sessions.placeholder')}
          onInput={(e) => {
            useBrParagraphSeparator()
            const el = e.currentTarget as HTMLDivElement
            setInput(editorTextOf(el))
            autoGrow(el)
          }}
          onKeyDown={(e) => {
            // Shift+Enter breaks the line — the browser's own newline is a
            // literal text node Chromium does not lay out here, so the <br> is
            // inserted by hand, exactly as the thread composer does.
            if (e.key === 'Enter' && e.shiftKey && !e.isComposing) {
              e.preventDefault()
              const el = e.currentTarget as HTMLDivElement
              const br = el.ownerDocument.createElement('br')
              const sel = document.getSelection()
              if (sel && sel.rangeCount > 0) {
                const rg = sel.getRangeAt(0)
                rg.deleteContents()
                rg.insertNode(br)
                rg.setStartAfter(br)
                rg.collapse(true)
                sel.removeAllRanges()
                sel.addRange(rg)
              } else {
                el.appendChild(br)
              }
              range_or_append(br, el.ownerDocument.createTextNode('\u200b'))
              setInput(editorTextOf(el))
              autoGrow(el)
              return
            }
            // Enter sends; a composing IME keeps its Enter.
            if (e.key === 'Enter' && !e.shiftKey && !e.isComposing && !(e.metaKey || e.ctrlKey)) {
              e.preventDefault()
              void send()
            }
            if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
              e.preventDefault()
              void send()
            }
          }}
        />
        <div class="composer-actions">
          <label class="ask-authorize" title={t('approval.stateHint')}>
            <span class="switch">
              <input
                type="checkbox"
                checked={authorize}
                onChange={(e) => setAuthorize((e.target as HTMLInputElement).checked)}
              />
              <span class="switch-track" aria-hidden="true" />
            </span>
            {t('sessions.authorize')}
          </label>
          <ModelSelect disabled={sending} />
          <button class="btn primary" type="submit" disabled={!input.trim() || sending}>
            {t('sessions.send')}
          </button>
        </div>
      </div>
      {/* Both halves of the bench, in the footer's own grammar: where the task
          runs, then which thread it continues. The thread pill is the reason
          this page exists — a send appends to the picked conversation without
          ever opening it. */}
      <div class="composer-foot">
        <DirButton
          value={props.dir}
          onChange={props.onDirChange}
          label={t('sessions.chooseDir')}
          disabled={sending}
        />
        <SessionPick
          sessions={props.sessions}
          value={props.picked}
          onChange={props.onPick}
          hasDir={props.dir !== ''}
          disabled={sending}
        />
        <p class="composer-shortcut-hint">{t('sessions.sendShortcut')}</p>
      </div>
    </form>
  )
}

/** One device, in full. The map answers "who is out there"; this panel answers
 *  everything else about the one plate the reader picked — its hardware, its
 *  runtime state, the agents it declares, and the work it has actually done.
 *  Sections in reading order: identity → hardware → runtime → abilities →
 *  agents → recent work → adjacency. The last two are the reason the panel
 *  exists at all: "what can this box do" is the card's job, "what has it been
 *  doing" is only visible here. */
function DeviceDetail({
  device: d,
  devices,
  probedAgents,
  onPick,
  onBack,
  onClose,
}: {
  device: Device
  devices: Map<string, Device>
  probedAgents: AgentInfo[]
  onPick(id: string): void
  onBack(): void
  onClose(): void
}) {
  const neighbors = d.neighbors.filter((id) => devices.has(id))
  const agents = Object.entries(d.agents)
  const work = d.work.slice(0, 6)
  const cliAgents = probedAgents.filter((a) => a.installed)
  return (
    <div class="ws-detail">
      <div class="ws-detail-head">
        <button
          type="button"
          class="icon-btn ws-detail-back"
          aria-label={t('workspace.detail.back')}
          title={t('workspace.detail.back')}
          onClick={onBack}
        >
          <Icon name="arrow-left" size={16} />
        </button>
        <svg class="ws-detail-glyph" viewBox="-14 -14 28 28" aria-hidden="true">
          <DeviceGlyph form={d.form} />
        </svg>
        <span class="ws-detail-name">{d.name}</span>
        <button
          type="button"
          class="icon-btn"
          aria-label={t('common.close')}
          onClick={onClose}
        >
          ×
        </button>
      </div>
      <p class="ws-detail-id mono dim">{d.id}</p>
      <div class="ws-detail-badges">
        <span class={`badge ${d.online ? (d.busy ? 'yellow' : 'green') : ''}`}>
          {workStateLabel(d)}
        </span>
        {d.local && <span class="badge green">{t('workspace.local')}</span>}
        <span class="badge">{formLabel(d.form)}</span>
        <span class="badge">{osLabel(d.os)}</span>
        {d.trust && d.trust !== 'mesh' && <span class="badge">{d.trust}</span>}
      </div>

      <DetailSection title={t('workspace.detail.hardware')}>
        <dl class="ws-facts">
          <Fact label={t('workspace.detail.chip')} value={d.chip} />
          <Fact label={t('workspace.detail.cores')} value={d.cores > 0 ? t('nodes.cores', { n: d.cores }) : ''} />
          <Fact label={t('workspace.detail.memory')} value={d.ram > 0 ? `${d.ram} GB` : ''} />
          <Fact label={t('workspace.detail.gpu')} value={d.gpu > 0 ? `${d.gpu} GB` : ''} />
          <Fact label={t('workspace.detail.form')} value={formLabel(d.form)} />
          <Fact label={t('workspace.detail.os')} value={osLabel(d.os)} />
        </dl>
      </DetailSection>

      <DetailSection title={t('workspace.detail.runtime')}>
        <dl class="ws-facts">
          <Fact
            label={t('workspace.detail.tasks')}
            value={
              d.maxTasks > 0
                ? t('nodes.concurrency', { cur: d.tasks, max: d.maxTasks })
                : d.tasks > 0
                  ? String(d.tasks)
                  : ''
            }
          />
          <Fact label={t('workspace.detail.queued')} value={d.queued > 0 ? String(d.queued) : ''} />
          <Fact label={t('workspace.detail.kind')} value={kindLabel(d.kind)} />
          <Fact label={t('workspace.detail.tier')} value={d.tier > 0 ? String(d.tier) : ''} />
          <Fact label={t('workspace.detail.version')} value={d.ver} />
          <Fact
            label={t('workspace.detail.rtt')}
            value={d.rtt > 0 ? `${d.rtt} ms${d.transport ? ` · ${d.transport}` : ''}` : ''}
          />
          <Fact
            label={t('nodes.lastSeen')}
            value={d.lastSeen === 'never' ? t('nodes.never') : new Date(d.lastSeen).toLocaleString()}
          />
        </dl>
      </DetailSection>

      {d.abilities.length > 0 && (
        <DetailSection title={t('workspace.detail.abilities')}>
          <div class="ws-detail-chips">
            {d.abilities.slice(0, 12).map((a) => (
              <span class="ability-tag" key={a}>
                {a}
              </span>
            ))}
            {d.abilities.length > 12 && (
              <span class="ability-tag more">+{d.abilities.length - 12}</span>
            )}
          </div>
        </DetailSection>
      )}

      <DetailSection title={t('workspace.detail.agents')}>
        {agents.length > 0 ? (
          <div class="ws-agent-list">
            {agents.map(([name, a]) => (
              <div class="ws-agent" key={name}>
                <div class="ws-agent-top">
                  <span class="ws-agent-name mono">{name}</span>
                  {a.cost_tier && <span class="badge">{a.cost_tier}</span>}
                  {typeof a.tier === 'number' && a.tier > 0 && (
                    <span class="ws-agent-tier mono dim">T{a.tier}</span>
                  )}
                </div>
                {a.capabilities && a.capabilities.length > 0 && (
                  <p class="ws-agent-caps dim">{a.capabilities.join(' · ')}</p>
                )}
                {a.best_at && a.best_at.length > 0 && (
                  <p class="ws-agent-caps dim">
                    {t('nodes.bestAt')}: {a.best_at.join(' · ')}
                  </p>
                )}
              </div>
            ))}
          </div>
        ) : cliAgents.length > 0 ? (
          <>
            {/* A local card that declares no agents still has the CLIs this
                machine can drive — the probe is a better answer than "none". */}
            <p class="ws-detail-note dim">{t('workspace.detail.agentsCli')}</p>
            <div class="ws-agent-list">
              {cliAgents.map((a) => (
                <div class="ws-agent" key={a.name}>
                  <div class="ws-agent-top">
                    <span class="ws-agent-name mono">{a.binary}</span>
                    {a.version && <span class="badge">{a.version}</span>}
                  </div>
                </div>
              ))}
            </div>
          </>
        ) : (
          <p class="ws-detail-note dim">{t('workspace.detail.agentsEmpty')}</p>
        )}
      </DetailSection>

      <DetailSection title={t('workspace.detail.work')}>
        {work.length === 0 ? (
          <p class="ws-detail-note dim">{t('workspace.detail.workEmpty')}</p>
        ) : (
          <div class="ws-work-list">
            {work.map((task) => (
              <button
                type="button"
                class="ws-work"
                key={task.id}
                title={task.title}
                onClick={() => navigate({ view: 'detail', id: task.id })}
              >
                <span class="ws-work-badge">
                  <StateBadge state={task.state} />
                </span>
                <span class="ws-work-text">
                  <span class="ws-work-title">{task.title || t('sessions.untitled')}</span>
                  <span class="ws-work-meta dim">
                    {task.project ? `${task.project} · ` : ''}
                    {relativeTime(task.updated_at, locale())}
                  </span>
                </span>
              </button>
            ))}
          </div>
        )}
      </DetailSection>

      <DetailSection title={t('workspace.detail.links')}>
        {neighbors.length === 0 ? (
          <p class="ws-detail-note dim">{t('workspace.detail.none')}</p>
        ) : (
          <div class="ws-detail-chips">
            {neighbors.map((id) => (
              <button type="button" class="ws-chip" key={id} onClick={() => onPick(id)}>
                {devices.get(id)?.name ?? id}
              </button>
            ))}
          </div>
        )}
      </DetailSection>
    </div>
  )
}

/** One titled block in the device panel. The hairline above every section but
 *  the first is what makes a long fact list scannable. */
function DetailSection({ title, children }: { title: string; children: ComponentChildren }) {
  return (
    <section class="ws-detail-sec">
      <h3 class="ws-detail-sec-title">{title}</h3>
      {children}
    </section>
  )
}

/** A label-over-value fact. Empty facts draw nothing — an unreported field is
 *  missing information, not a zero. */
function Fact({ label, value }: { label: string; value: string }) {
  if (!value) return null
  return (
    <div class="ws-fact">
      <dt>{label}</dt>
      <dd class="mono">{value}</dd>
    </div>
  )
}
