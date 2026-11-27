// SPDX-License-Identifier: AGPL-3.0-or-later

// The directory card's pure half: what a row is, how a typed query becomes a
// path, and how the list bands up. Kept out of `dir-picker.tsx` so the rules
// can be tested without a DOM — the component only paints what these return.

import { lastPathSegment } from './dir-utils.ts'
import type { DirectoryListing } from '../api/client.ts'

/** One directory the console has already used. A directory only means
 *  something to the panel once a project points at it (`projectForDir` makes
 *  that row on send), so "already opened" and "has a project" are the same
 *  set — and the project's name is the label the rail already shows it under. */
export interface WorkdirRow {
  name: string
  dir: string
  /** When work was last filed here. Two `panda` folders on two drives are one
   *  name; the stamp is what tells the one you used this morning from the one
   *  you touched in March. */
  updated?: string
  /** The project's own one-liner, when it has one. */
  description?: string
  /** The "no pick" row: an empty dir that means "let the console decide". */
  auto?: boolean
  /** The path-mode "this folder does not exist yet" row. */
  create?: boolean
}

/** Windows and POSIX both: a trailing separator is not a different directory.
 *  Case is another matter — a Windows path is case-insensitive, a POSIX one is
 *  not, and folding both made `/w/New` and `/w/new` one row: the cursor landed
 *  on the wrong folder and the "create" row disappeared. */
export const normDir = (p: string) => {
  const trimmed = trimPath(p)
  return /^[A-Za-z]:/.test(trimmed) || (!trimmed.startsWith('/') && trimmed.includes('\\'))
    ? trimmed.replaceAll('/', '\\').toLowerCase()
    : trimmed
}

/** How a starred folder is identified: the same rule the server stores by
 *  (`favoriteKey` in internal/projects), so "D:\X\a" and "d:\x\a\" are one
 *  star on both sides. */
export function favoriteKey(p: string): string {
  return normDir(p.trim())
}

export type DirGroupKey = 'fav' | 'recent' | 'other'

export interface DirGroup {
  key: DirGroupKey
  rows: WorkdirRow[]
}

/** Seven days: long enough that "the thing I worked on this week" stays in one
 *  band, short enough that the band means something. */
export const RECENT_WINDOW_MS = 7 * 24 * 60 * 60 * 1000

/** The card's three bands: starred first (in the order they were starred),
 *  then what was touched this week, then everything else. A folder appears
 *  exactly once — the star wins over recency, because a row that showed up in
 *  two bands would make the card look like it had lost track of it. Empty bands
 *  are dropped rather than drawn with an empty header. */
export function groupRows(rows: WorkdirRow[], favorites: string[], now = Date.now()): DirGroup[] {
  const byKey = new Map<string, WorkdirRow>()
  for (const r of rows) byKey.set(normDir(r.dir), r)
  const fav: WorkdirRow[] = []
  for (const f of favorites) {
    const key = favoriteKey(f)
    const hit = byKey.get(key)
    // A star is not a property of a project: a folder can be starred before
    // any work is filed under it, and skipping it there made the star look
    // lost the next time the card opened — the star was saved, the card just
    // refused to paint it. The star itself is enough to draw the row.
    fav.push(hit ?? { name: lastPathSegment(f), dir: f })
    byKey.delete(key)
  }
  const recent: WorkdirRow[] = []
  const other: WorkdirRow[] = []
  for (const r of rows) {
    if (!byKey.has(normDir(r.dir))) continue
    const t = r.updated ? Date.parse(r.updated) : NaN
    if (!Number.isNaN(t) && now - t <= RECENT_WINDOW_MS) recent.push(r)
    else other.push(r)
  }
  const out: DirGroup[] = [
    { key: 'fav', rows: fav },
    { key: 'recent', rows: recent },
    { key: 'other', rows: other },
  ]
  return out.filter((g) => g.rows.length > 0)
}

/** A query that names a path rather than a project: a separator or a drive
 *  letter. Both `/add-dir` (Claude Code) and `--cd` (Codex) take a path as an
 *  argument; a picker that only lists projects cannot. */
export function pathLike(q: string): boolean {
  const s = q.trim()
  // A path starts at a root: a separator, `~`, or a drive. `foo/bar` is a
  // search word, not a path — the server would resolve it against its own
  // working directory, which is not the directory the person is looking at.
  return /^(?:[\\/]|(?:~|[A-Za-z]:)(?:[\\/]|$))/.test(s)
}

interface PathQuery {
  base: string
  prefix: string
  self: string
}

function trimPath(path: string): string {
  return path.replace(/[\\/]+$/, '') || path.charAt(0)
}

/** `D:\X\proj` → parent `D:\X`, prefix `proj`; `D:\X\` → parent `D:\X`, an
 *  empty prefix, and the folder itself offered as the first row. */
export function splitPathQuery(q: string): PathQuery | null {
  const raw = q.trim()
  if (!raw) return null
  if (raw === '~' || /[\\/]$/.test(raw)) {
    const self = driveRoot(trimPath(raw))
    return { base: self, prefix: '', self }
  }
  const cut = Math.max(raw.lastIndexOf('\\'), raw.lastIndexOf('/'))
  // `C:` on its own: no separator to cut at, but the drive is a place to look.
  // Anything else without a separator is a search word, not a path.
  if (cut < 0) {
    return /^[A-Za-z]:$/.test(raw) ? { base: driveRoot(raw), prefix: '', self: '' } : null
  }
  // `/usr` cuts at 0: the base is the root, not the empty string. An empty
  // base means "no path" to the server, which answers with the home directory,
  // and the prefix filter then drops every row.
  const head = raw.slice(0, cut)
  const base = head === '' ? raw[0]! : driveRoot(head)
  return { base, prefix: raw.slice(cut + 1), self: '' }
}

/** `D:` is not a folder — Windows reads it as "the current directory on D:". */
export function driveRoot(p: string): string {
  return /^[A-Za-z]:$/.test(p) ? p + '\\' : p
}

/** Suggest the literal match before case-insensitive prefix completions. */
export function pathQueryRows(query: string, parts: PathQuery, listing: DirectoryListing): WorkdirRow[] {
  const prefix = parts.prefix.toLowerCase()
  // Older panels can return null for an empty directory.
  const rows: WorkdirRow[] = (listing.directories ?? [])
    .filter((entry) => entry.name.toLowerCase().startsWith(prefix))
    .map((entry) => ({ name: entry.name, dir: entry.path }))
  if (parts.self) {
    const dir = parts.self.startsWith('~') ? listing.current : parts.self
    rows.unshift({ name: lastPathSegment(dir), dir })
  } else if (parts.prefix) {
    const typed = query.trim()
    const base = listing.current.endsWith(listing.separator)
      ? listing.current
      : listing.current + listing.separator
    const resolved = parts.base.startsWith('~')
      ? base + parts.prefix
      : typed
    const key = normDir(resolved)
    const exact = rows.findIndex((row) => normDir(row.dir) === key)
    if (exact > 0) rows.unshift(rows.splice(exact, 1)[0]!)
    if (exact < 0) rows.push({ name: lastPathSegment(typed), dir: typed, create: true })
  }
  return rows
}

/** What Enter takes in path mode.
 *
 *  A listing answers the query it was fetched for, and while a newer query is
 *  still in flight the old rows are what is on screen. Committing one of them
 *  picked a directory from the *previous* search — typing `D:\review\slow\new`
 *  and pressing Enter took `D:\review\old`, and the next task ran there. The
 *  typed path is the answer and the rows are only help, so a listing that does
 *  not belong to the current query falls back to the field instead. */
export function pathEnterTarget(
  query: string,
  rows: WorkdirRow[],
  rowsQuery: string,
  cursor: number,
): WorkdirRow | null {
  const q = query.trim()
  // A bare drive has no row of its own to highlight (the listing shows its
  // children), so the field is the only answer it can have.
  if (rowsQuery !== q || /^[A-Za-z]:$/.test(q)) {
    // `D:` means "the current directory on D:" and Windows resolves it to
    // wherever the last shell left that drive. The listing offers `D:\` for
    // the same input, so the fallback must too.
    // The home alias needs the node's response before it can be selected.
    if (q.startsWith('~')) return null
    const typed = driveRoot(trimPath(q))
    return typed ? { name: lastPathSegment(typed), dir: typed } : null
  }
  return rows[cursor] ?? rows[0] ?? null
}
