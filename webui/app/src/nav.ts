// SPDX-License-Identifier: AGPL-3.0-or-later

// The console's navigation vocabulary: the route union, the hash codec, and
// the sidebar's grouping. It lives in its own module because two surfaces need
// it — the sidebar (app.tsx) and the ⌘K palette (components/palette.tsx) — and
// a palette that lists a different set of destinations than the sidebar is a
// bug waiting to be filed.

/** 总控's three boards. They used to be three rail rows (任务队列, 计划, 项目)
 *  over three routes; they are now one page with one tab strip, because all
 *  three answer the same question — "what is this node working on, and where
 *  does that work live" — and a rail row per board made the work group read
 *  as three unrelated destinations. */
export type ControlTab = 'queue' | 'plans' | 'projects'

export type Route =
  | { view: 'sessions'; id: string | null; project?: string | null }
  | { view: 'control'; tab?: ControlTab; project?: string | null; review?: boolean }
  | { view: 'workspace' }
  | { view: 'fleet' }
  | { view: 'profile' }
  | { view: 'settings'; tab?: string | null }
  | { view: 'detail'; id: string }

/** One section of the sidebar rail: an optional heading, then its rows. */
export type NavGroup = {
  /** Stable identity — the rail's React key. */
  id: string
  /** Heading rendered above the rows as a small uppercase label. Omitted for
   *  the work group, which sits at the top of the rail where a "Work" label
   *  would only label the first thing on screen. */
  head?: string
  items: Array<[view: string, key: string]>
}

/** The primary workspace views on the main sidebar rail, grouped the way the
 *  product reads: work first (control), then the network that runs it
 *  (fleet).
 *
 *  Two views are deliberately not rows here. 新对话 is an action, and 工作空间
 *  is its peer — both sit in the quick block above the nav (app.tsx), which is
 *  why neither appears in the groups below. 对话 is not a row either: the
 *  thread history is the tail of the rail (app.tsx), and a row whose only job
 *  was to unfold a list that is now always visible just named the list again.
 *  计划 and 项目 are not rows either: both are tabs of 总控.
 *  记忆 and 技能 are not rows either: both are tabs of 设置 — the rail keeps
 *  only the destinations you *work* in, and everything about the agent's mind
 *  or its configuration lives in the one management centre. */
export const primaryNavGroups: NavGroup[] = [
  {
    id: 'work',
    items: [['control', 'nav.control']],
  },
  {
    // No heading: the group has one row, and "节点网络" above "节点网络"
    // named the same destination twice. Spacing already separates the group.
    id: 'fleet',
    items: [['fleet', 'nav.fleet']],
  },
]

/** Flat nav list — the mobile top bar and any code that iterates views. */
export const primaryNav: Array<[view: string, key: string]> = primaryNavGroups.flatMap(
  (g) => g.items,
)

/** Palette views: includes primary workspaces plus direct jumps to settings
 *  sections. 对话 is listed here even though the rail has no row for it — the
 *  palette is a jump list, and chat is still a destination (the rail reaches
 *  it through the history and 新对话). Dropping it here too would leave ⌘K
 *  with no way to reach the chat surface. 计划 and 项目 get the same
 *  treatment: their rail rows are gone (they are tabs of 总控 now), but ⌘K
 *  still jumps straight to those tabs. */
export const navViews: Array<[view: string, key: string]> = [
  ['sessions', 'nav.sessions'],
  // 工作空间 is a rail row of its own, above the groups (app.tsx renders it
  // beside 新对话), so the palette lists it explicitly rather than through
  // primaryNav.
  ['workspace', 'nav.workspace'],
  ...primaryNav.flatMap((item): Array<[string, string]> =>
    item[0] === 'control'
      ? [item, ['control:plans', 'nav.plans'], ['control:projects', 'nav.projects']]
      : [item],
  ),
  ['profile', 'nav.profile'],
  ['settings', 'nav.settings'],
]

/** `?tab=` → a valid 总控 tab. Anything unknown (including a missing param)
 *  falls back to the kanban, which is the page's default board. */
function parseControlTab(raw: string | null): ControlTab {
  return raw === 'plans' || raw === 'projects' ? raw : 'queue'
}

export function parseHash(): Route {
  const raw = location.hash.replace(/^#\/?/, '')
  const [path = '', queryStr = ''] = raw.split('?', 2)
  const query = new URLSearchParams(queryStr)
  const projectParam = query.get('project') || null
  const tabParam = query.get('tab') || null

  if (path.startsWith('task/')) return { view: 'detail', id: decodeURIComponent(path.slice(5)) }
  if (path.startsWith('chat/')) return { view: 'sessions', id: decodeURIComponent(path.slice(5)), project: projectParam }
  if (path === 'chat') return { view: 'sessions', id: null, project: projectParam }
  if (path.startsWith('projects/') && path.length > 9) {
    return { view: 'sessions', id: null, project: decodeURIComponent(path.slice(9)) }
  }
  if (path === 'control') {
    return {
      view: 'control',
      tab: parseControlTab(query.get('tab')),
      project: projectParam,
      review: query.has('review'),
    }
  }
  // Legacy URLs: 任务队列, 计划 and 项目 were their own routes before they
  // became the three tabs of 总控. Old deep links (and anything still linking
  // `#/queue`, `#/plans`, `#/projects`) land on the same board, just under the
  // merged route.
  if (path === 'queue') {
    return { view: 'control', tab: 'queue', project: projectParam, review: query.has('review') }
  }
  if (path === 'plans') return { view: 'control', tab: 'plans' }
  if (path === 'projects') return { view: 'control', tab: 'projects' }
  if (path === 'fleet' || path === 'nodes') return { view: 'fleet' }
  // 工作空间: the topology of connected devices. `#/workspace` is the only
  // spelling — there is no legacy URL for a page that never existed.
  if (path === 'workspace') return { view: 'workspace' }
  // 记忆 and 技能 were standalone rows before they became tabs of 设置. Old
  // deep links (`#/memory`, `#/memory/topics`, `#/skills`) land on the same
  // surfaces, just under the merged route.
  if (path === 'memory' || path.startsWith('memory/')) return { view: 'settings', tab: 'memory' }
  if (path === 'skills') return { view: 'settings', tab: 'skills' }
  // The identity chip's destination. It is not a rail row: the chip in the
  // footer is the only way in, which is the point — "who am I / what am I
  // running / what have I done" belongs to the identity, not to Settings.
  if (path === 'profile') return { view: 'profile' }
  if (path.startsWith('settings/') || path === 'settings') {
    const tab = path.startsWith('settings/') ? decodeURIComponent(path.slice(9)) : tabParam
    // Promoted views: old settings deep-links land on the standalone routes.
    if (tab === 'nodes' || tab === 'devices') return { view: 'fleet' }
    return { view: 'settings', tab }
  }

  // Graceful redirects for legacy URLs: promoted views land on their own
  // routes now; reminders/system stay inside Settings.
  if (path === 'reminders') return { view: 'settings', tab: 'reminders' }
  if (path === 'system') return { view: 'settings', tab: 'system' }

  return { view: 'sessions', id: null, project: projectParam }
}

export function navigate(route: Route): void {
  if (route.view === 'detail') {
    location.hash = `#/task/${encodeURIComponent(route.id)}`
  } else if (route.view === 'sessions') {
    let base = route.id ? `#/chat/${encodeURIComponent(route.id)}` : '#/chat'
    if (route.project) {
      base += `?project=${encodeURIComponent(route.project)}`
    }
    location.hash = base
  } else if (route.view === 'control') {
    // The queue tab is the page's default, so it stays out of the URL — a
    // plain `#/control` is the board, and only 计划 / 项目 need saying.
    const params: string[] = []
    if (route.tab && route.tab !== 'queue') params.push(`tab=${route.tab}`)
    if (route.project) params.push(`project=${encodeURIComponent(route.project)}`)
    if (route.review) params.push('review=1')
    location.hash = params.length ? `#/control?${params.join('&')}` : '#/control'
  } else if (route.view === 'settings') {
    location.hash = route.tab ? `#/settings?tab=${encodeURIComponent(route.tab)}` : '#/settings'
  } else {
    location.hash = `#/${route.view}`
  }
}

/** Navigate by bare view name — supports settings:tab and control:tab syntax
 *  for palette jumps. */
export function navigateView(view: string): void {
  if (view.startsWith('settings:')) {
    navigate({ view: 'settings', tab: view.slice(9) })
  } else if (view.startsWith('control:')) {
    navigate({ view: 'control', tab: view.slice(8) as ControlTab })
  } else if (view === 'sessions') {
    navigate({ view: 'sessions', id: null })
  } else {
    navigate({ view } as Route)
  }
}
