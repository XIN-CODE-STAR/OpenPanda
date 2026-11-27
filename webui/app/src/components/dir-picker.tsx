// SPDX-License-Identifier: AGPL-3.0-or-later

import { Fragment } from 'preact'
import { useSyncExternalStore } from 'preact/compat'
import { useEffect, useMemo, useRef, useState } from 'preact/hooks'
import { api, currentToken, type DirectoryListing } from '../api/client'
import { locale, t } from '../i18n'
import { relativeTime } from '../format/time'
import { layerAboveOwnsEscape } from './card-dismiss'
import { createDirFavorites } from './dir-favorites'
import { Icon } from './icons'
import { lastPathSegment, suggestProjectName } from './dir-utils'
import {
  favoriteKey,
  groupRows,
  normDir,
  pathEnterTarget,
  pathLike,
  pathQueryRows,
  splitPathQuery,
  type DirGroupKey,
  type WorkdirRow,
} from './dir-rows'
import { useModalFocus } from './modal-focus'
import { toast, toastError } from './toast'

// The two workdir pickers share the node's favorites. A queued write from a
// closed picker can finish after navigation, but only with the same login.
let favoritesToken = ''
let favoritesStore: ReturnType<typeof createDirFavorites> | undefined

function dirFavorites() {
  const token = currentToken()
  if (!favoritesStore || token !== favoritesToken) {
    favoritesToken = token
    favoritesStore = createDirFavorites(
      async () => currentToken() === token ? (await api.prefs())?.favorites ?? [] : [],
      async (path, favorite) => {
        if (currentToken() !== token) throw new Error('session changed')
        return (await api.setFavorite(path, favorite))?.favorites ?? []
      },
    )
  }
  return favoritesStore
}

export interface DirPickerProps {
  value?: string
  onChange: (path: string) => void
  onSuggestName?: (name: string) => void
  placeholder?: string
  disabled?: boolean
}

/** The pick itself: the native chooser first, the in-page browser as the
 *  fallback for a node whose panel cannot raise one. Shared by the boxed
 *  picker on the projects page and the compact button the composer's
 *  new-chat footer uses — the two look nothing alike but must fail alike. */
function useDirPick(onPicked: (path: string) => void, disabled: boolean) {
  const [modalOpen, setModalOpen] = useState(false)
  const [loading, setLoading] = useState(false)

  async function open(current: string) {
    if (disabled || loading) return
    setLoading(true)
    try {
      // The caption is ours to write: the panel serves five languages and
      // cannot tell which one the browser is in.
      const res = await api.chooseDirectory(current, t('sessions.chooseDir'))
      const canceled = !!(res && (res.canceled ?? res.cancelled))
      if (res && res.path && !canceled) {
        onPicked(res.path)
        return
      }
      // A cancel is an answer — the user looked and changed their mind. Do not
      // answer it by opening a second picker over the one they just closed.
      if (canceled) return
      // Otherwise no window can be raised for this console (a browser on
      // another device, a host with no desktop shell) or it failed to open:
      // fall back to the in-page browser, which always works.
      setModalOpen(true)
    } catch {
      setModalOpen(true)
    } finally {
      setLoading(false)
    }
  }

  return { modalOpen, setModalOpen, loading, open }
}

export function DirPicker({
  value = '',
  onChange,
  onSuggestName,
  placeholder,
  disabled = false,
}: DirPickerProps) {
  const pick = useDirPick((path) => {
    onChange(path)
    if (onSuggestName) {
      onSuggestName(suggestProjectName(path))
    }
  }, disabled)
  const { modalOpen, setModalOpen, loading } = pick

  function handlePick() {
    void pick.open(value)
  }

  return (
    <div class="dir-picker-wrap">
      <div class={`dir-picker-box ${value ? 'has-value' : ''} ${disabled ? 'disabled' : ''}`}>
        <Icon name="folder" class="dir-picker-icon" />
        <span class="dir-picker-path" title={value || placeholder || t('projects.dirPlaceholder')}>
          {value || <span class="dim">{placeholder || t('projects.dirPlaceholder')}</span>}
        </span>
        {value && !disabled && (
          <button
            type="button"
            class="dir-picker-clear"
            onClick={(e) => {
              e.stopPropagation()
              onChange('')
            }}
            title={t('common.clear')}
          >
            <Icon name="x" size={12} />
          </button>
        )}
        <button
          type="button"
          class="btn dir-picker-browse-btn"
          onClick={handlePick}
          disabled={disabled || loading}
        >
          {loading ? t('common.loading') : t('projects.chooseDir')}
        </button>
      </div>

      {modalOpen && (
        <DirBrowserModal
          initialPath={value}
          onSelect={(selectedPath) => {
            onChange(selectedPath)
            if (onSuggestName) {
              onSuggestName(suggestProjectName(selectedPath))
            }
            setModalOpen(false)
          }}
          onClose={() => setModalOpen(false)}
        />
      )}
    </div>
  )
}


/** The compact pick: a folder glyph and the chosen path set in the same small
 *  print as the composer's keyboard legend. The projects page needs a form
 *  field; a new thread only needs to say where it will run, so this one is a
 *  footnote that happens to be clickable.
 *
 *  Clicking it opens a card, not the OS dialog. The dialog was the wrong first
 *  answer for the common case: a directory you have used before is one you are
 *  about to use again, and making the user walk the tree to find `panda` when
 *  the console already knows where `panda` lives is the slow path through a
 *  control that has all the answers. So the card lists the directories this
 *  node has filed work under — newest first, the current one ticked, a search
 *  field on top for the day the list outgrows the card — and the native
 *  chooser moves to the bottom row, where "a directory I have never used" now
 *  lives. The card is the same shape as the thread pick beside it on 工作空间:
 *  search, rows, one footer of actions.
 *
 *  A chosen directory is still undoable in place: under the pointer the glyph
 *  turns into a cross, and the cross drops the pick back to "choose a
 *  directory". The glyph is the cross's slot rather than a second control
 *  beside the name — the chip is one line of small print, and an × parked next
 *  to a long folder name would either crowd it or be pushed out of the clamp. */
export function DirButton({
  tourTarget,
  value = '',
  onChange,
  label,
  hint,
  tip,
  autoDir,
  disabled = false,
}: {
  value?: string
  onChange: (path: string) => void
  tourTarget?: string
  /** The picker's own name, shown while nothing is chosen. */
  label: string
  hint?: string
  /** What the bubble says when nothing is picked yet: the folder the turn will
   *  actually run in when the caller resolves it from elsewhere (a project).
   *  Without it a pick inherited from the project reads as "choose a folder",
   *  which is the opposite of what will happen. */
  tip?: string
  /** Resolved path used when "自动" (default directory) is active. */
  autoDir?: string
  disabled?: boolean
}) {
  const pick = useDirPick(onChange, disabled)
  const [open, setOpen] = useState(false)
  const [query, setQuery] = useState('')
  /** null while the list is in flight: an empty menu and a loading menu must
   *  not look alike, or "no workspaces yet" flashes on every open. */
  const [spaces, setSpaces] = useState<WorkdirRow[] | null>(null)
  const stars = dirFavorites()
  const favorites = useSyncExternalStore(stars.subscribe, stars.snapshot)
  const box = useRef<HTMLDivElement>(null)
  const dialog = useRef<HTMLDialogElement>(null)

  // The card is a modal, the same way the roster is: it takes its own layer
  // with a scrim behind it, so a click outside is a click on the scrim (not on
  // the composer underneath), and Escape is the element's own cancel.
  useEffect(() => {
    const el = dialog.current
    if (!el) return
    if (open && !el.open) el.showModal()
    else if (!open && el.open) el.close()
  }, [open])

  // Read on every open rather than cached at mount: filing work under a
  // project touches its row, which is what orders this list, and the endpoint
  // is local — a stale menu that hides the directory you just used is worse
  // than one extra round-trip.
  useEffect(() => {
    if (!open) return
    let alive = true
    setSpaces(null)
    api
      .projects()
      .then((pl) => {
        if (!alive) return
        setSpaces(
          (pl.detail ?? [])
            .filter((d) => !!d.work_dir)
            .map((d) => ({
              name: d.name,
              dir: d.work_dir as string,
              updated: d.updated_at,
              description: d.description,
            })),
        )
      })
      .catch(() => {
        if (alive) setSpaces([])
      })
    // Preference reads are independent of the project listing.
    void stars.refresh().catch(() => {})
    return () => {
      alive = false
    }
  }, [open, stars])

  // Escape still stops here so it cannot also reach the thread drawer behind
  // the card. The click-outside half moved to the dialog's own scrim (see the
  // `<dialog onClick>` below): a modal never lets a press reach the page, so a
  // document-level listener would never fire.
  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape' || layerAboveOwnsEscape('.wd-panel')) return
      e.stopPropagation()
      setOpen(false)
      setQuery('')
    }
    document.addEventListener('keydown', onKey, true)
    return () => {
      document.removeEventListener('keydown', onKey, true)
    }
  }, [open])

  // A pick no project owns yet (a folder chosen but not sent into) still
  // deserves a row: otherwise the card opens with nothing ticked and the pill's
  // answer nowhere in its own list.
  const all = useMemo(() => {
    const rows = spaces ?? []
    if (!value || rows.some((r) => normDir(r.dir) === normDir(value))) return rows
    return [{ name: lastPathSegment(value), dir: value }, ...rows]
  }, [spaces, value])

  const rows = useMemo(() => {
    const q = query.trim().toLowerCase()
    if (!q) return all
    return all.filter(
      (r) => r.name.toLowerCase().includes(q) || r.dir.toLowerCase().includes(q),
    )
  }, [all, query])

  /** Stars ride the same search as the project rows — a query narrows the card
   *  to what it names, and a starred folder is not exempt. The band still
   *  paints a star whose folder has no project row (groupRows materializes it),
   *  which is the "starred before any work was filed there" case. */
  const shownFavorites = useMemo(() => {
    const q = query.trim().toLowerCase()
    if (!q) return favorites
    return favorites.filter(
      (f) => f.toLowerCase().includes(q) || lastPathSegment(f).toLowerCase().includes(q),
    )
  }, [favorites, query])

  /** A query that names a path, not a project. Typing one flips the card from
   *  "pick a folder you have used" to "open any folder": the list becomes the
   *  parent's subdirectories filtered by the last segment. That is the
   *  `/add-dir <path>` model Claude Code and Codex share — the typed path is
   *  the answer, the suggestions are only help — and it is the one case a list
   *  of known projects cannot serve. */
  const pathMode = pathLike(query)
  const [pathRows, setPathRows] = useState<WorkdirRow[] | null>(null)
  /** The query the rows above were fetched for. Typing does not clear the
   *  list (that would flash "loading" on every keystroke), so Enter has to
   *  know whether what is on screen answers what is in the field. */
  const [pathRowsFor, setPathRowsFor] = useState('')
  const [pathError, setPathError] = useState('')
  /** The row the keyboard is on. Arrows move it, Enter takes it, Tab completes
   *  it into the field (the same affordance `/add-dir` gives a partial path). */
  const [active, setActive] = useState(0)

  useEffect(() => {
    if (!open || !pathMode) {
      setPathRows(null)
      setPathRowsFor('')
      setPathError('')
      return
    }
    const q = query.trim()
    const parts = splitPathQuery(q)
    if (!parts) return
    let alive = true
    // Typing is faster than the disk: one listing per pause, not per key.
    const timer = setTimeout(() => {
      api
        .listDirectories(parts.base)
        .then((res) => {
          if (!alive) return
          setPathRows(pathQueryRows(q, parts, res))
          setPathRowsFor(q)
          setPathError('')
        })
        .catch((err: unknown) => {
          if (!alive) return
          setPathRows([])
          setPathRowsFor(q)
          setPathError(err instanceof Error ? err.message : String(err))
        })
    }, 140)
    return () => {
      alive = false
      clearTimeout(timer)
    }
  }, [open, query, pathMode])

  /** What the list shows: the typed path's subdirectories, or the projects
   *  this node has filed work under. The first row of the project list is
   *  always "no pick" — the same shape the roster's "自动" row gives, and the
   *  answer to "what happens if I choose nothing" without leaving the card. */
  // A create action belongs only to the query that produced it.
  const pathItems = pathRowsFor === query.trim() ? pathRows : pathRows?.filter((r) => !r.create)
  const folderRows = pathMode ? (pathItems ?? []) : rows
  const listed = pathMode ? pathRows !== null : spaces !== null

  /** The project list in the card's three bands. Path mode has no bands: what
   *  is listed there is a filesystem, and "recently used" is not a property of
   *  a folder the console has never opened. */
  const groups = pathMode ? [] : groupRows(folderRows, shownFavorites)
  const items: WorkdirRow[] = pathMode
    ? folderRows
    : [{ name: '', dir: '', auto: true }, ...groups.flatMap((g) => g.rows)]
  /** Where each row sits in the keyboard's flat order, so the grouped render
   *  and the ↑↓ cursor agree even though the groups are drawn separately. */
  const rowIndex = new Map<string, number>()
  items.forEach((r, i) => rowIndex.set(r.auto ? '\u0000auto' : r.create ? '\u0000create' : normDir(r.dir), i))
  /** The keyboard's row, clamped: a shorter list must not leave the highlight
   *  pointing past its end. */
  const cursor = Math.max(0, Math.min(active, items.length - 1))

  // The keyboard starts on the row that is already chosen — the "自动" row
  // when nothing is — and goes back to the top whenever the query changes: a
  // highlight left two screens down a new list is a highlight on a folder
  // nobody pointed at.
  useEffect(() => {
    if (!open) return
    if (pathMode || query.trim()) {
      setActive(0)
      return
    }
    const i = items.findIndex((r) => !!value && normDir(r.dir) === normDir(value))
    setActive(i >= 0 ? i : 0)
    // Only the list's identity matters; arrow keys must not re-run this.
  }, [open, query, pathMode, spaces, pathRows])

  /** Closing always forgets the search: a card reopened a minute later must not
   *  open onto the filter typed before, with half the list missing. */
  function close() {
    setOpen(false)
    setQuery('')
  }

  function choose(dir: string) {
    onChange(dir)
    close()
  }

  function isFav(dir: string): boolean {
    const key = favoriteKey(dir)
    return favorites.some((f) => favoriteKey(f) === key)
  }

  /** Star or unstar one folder. Optimistic: a 22px toggle that waits on a
   *  round-trip reads as broken, and the server answers with the whole list, so
   *  the card ends up holding exactly what the store holds. */
  async function toggleFav(dir: string) {
    try {
      await stars.toggle(dir)
    } catch {
      toast(t('sessions.workdirFavFail'), 'error')
    }
  }

  async function reveal(dir: string) {
    try {
      const res = await api.revealPath(dir)
      if (!res?.opened) toast(t('sessions.openFolderMissing'), 'error')
    } catch {
      toast(t('sessions.openFolderMissing'), 'error')
    }
  }

  async function copyPath(dir: string) {
    try {
      await navigator.clipboard.writeText(dir)
      toast(t('sessions.workdirCopied'))
    } catch (e) {
      toastError(e)
    }
  }

  /** Create the folder that was typed, then take it — the same step the send
   *  path would take later (projects.Create's MkdirAll), moved up so the user
   *  can see the folder they just made. */
  async function createAndPick(dir: string) {
    try {
      const res = await api.mkdir(dir)
      choose(res.path)
    } catch {
      toast(t('sessions.workdirMkdirFail'), 'error')
    }
  }

  /** The pill used to be this action outright; it is now the card's last row,
   *  and the native chooser still falls back to the in-page browser when no
   *  window can be raised (see `useDirPick`). */
  function browse() {
    close()
    void pick.open(value)
  }

  const groupLabel = (k: DirGroupKey) =>
    k === 'fav'
      ? t('sessions.workdirFavGroup')
      : k === 'recent'
        ? t('sessions.workdirRecentGroup')
        : t('sessions.workdirOtherGroup')

  /** One row. A `div`, not a `button`: the row now carries its own buttons
   *  (open, copy, star) and a button inside a button is not markup a browser
   *  keeps. The listbox's keyboard lives on the search field, so the row only
   *  needs `role=option`, `aria-selected` and a click. */
  function row(r: WorkdirRow) {
    const key = r.auto ? '\u0000auto' : r.create ? '\u0000create' : normDir(r.dir)
    const i = rowIndex.get(key) ?? 0
    const on = r.auto ? !value : !!value && !r.create && normDir(r.dir) === normDir(value)
    const fav = !r.auto && !r.create && isFav(r.dir)
    return (
      <div
        key={key}
        class={`wd-row${r.auto ? ' auto' : ''}${r.create ? ' create' : ''}${on ? ' sel' : ''}${i === cursor ? ' active' : ''}`}
        role="option"
        aria-selected={on}
        title={r.auto || r.create ? undefined : r.dir}
        onMouseEnter={() => setActive(i)}
        onClick={() => (r.create ? void createAndPick(r.dir) : choose(r.dir))}
      >
        <Icon
          name={r.auto ? 'sparkles' : r.create ? 'folder-plus' : 'folder'}
          size={15}
          class="wd-row-icon"
        />
        <span class="wd-row-text">
          <span class="wd-row-top">
            <span class="wd-row-name">
              {r.auto
                ? t('sessions.workdirAuto')
                : r.create
                  ? t('sessions.workdirMkdir', { name: r.name })
                  : r.name}
            </span>
            {on && <Icon name="check" size={14} class="wd-row-check" />}
          </span>
          {/* The name is usually the folder's own name, which is exactly why the
              path earns the second line: two `panda` folders on two drives are
              one name. */}
          <span class="wd-row-path">
            {r.auto
              ? (autoDir ? t('sessions.workdirAutoCurrent', { dir: autoDir }) : t('sessions.workdirAutoHint'))
              : r.dir}
          </span>
          {r.description && <span class="wd-row-desc">{r.description}</span>}
        </span>
        {/* The actions take the stamp's space on hover. Rows without a stamp
            fit the actions beside the star. */}
        <span class="wd-row-side">
          {r.updated && <span class="wd-row-when">{relativeTime(r.updated, locale())}</span>}
          {!r.auto && !r.create && (
            <button
              type="button"
              class={`wd-star${fav ? ' on' : ''}`}
              aria-pressed={fav}
              title={t(fav ? 'sessions.workdirUnfav' : 'sessions.workdirFav')}
              aria-label={t(fav ? 'sessions.workdirUnfav' : 'sessions.workdirFav')}
              onClick={(e) => {
                e.stopPropagation()
                void toggleFav(r.dir)
              }}
            >
              <Icon name="star" size={14} />
            </button>
          )}
          {!r.auto && !r.create && (
            <span class="wd-row-acts">
              <button
                type="button"
                class="wd-act"
                title={t('sessions.workdirReveal')}
                aria-label={t('sessions.workdirReveal')}
                onClick={(e) => {
                  e.stopPropagation()
                  void reveal(r.dir)
                }}
              >
                <Icon name="folder-open" size={13} />
              </button>
              <button
                type="button"
                class="wd-act"
                title={t('sessions.workdirCopyPath')}
                aria-label={t('sessions.workdirCopyPath')}
                onClick={(e) => {
                  e.stopPropagation()
                  void copyPath(r.dir)
                }}
              >
                <Icon name="copy" size={13} />
              </button>
            </span>
          )}
        </span>
      </div>
    )
  }

  return (
    <div class="workdir-pick" ref={box} data-tour-target={tourTarget}>
      <div
        class={`workdir-btn${value ? ' has-value' : ''}${disabled ? ' is-disabled' : ''}`}
        /* The bubble is where the whole path lives: the label shows the folder
           on its own, so hovering is how you check *which* folder. Nothing
           while the card is up: the card lists every path. */
        data-tip={!open ? (value || tip) : undefined}
      >
        <button
          type="button"
          class="workdir-open"
          data-tour-focus={tourTarget ? '' : undefined}
          aria-haspopup="dialog"
          aria-expanded={open}
          aria-label={value || tip ? `${label}: ${value || tip}` : label}
          disabled={disabled}
          onClick={() => (open ? close() : setOpen(true))}
        >
          {/* The rail's working-directory mark, at the rail's own size: the
              header's closed folder would read as a second title down here. */}
          <Icon name="folder-open" size={13} class="workdir-glyph" />
          <span class="workdir-path">{value ? lastPathSegment(value) : label}</span>
          {/* The caret is the whole difference between "opens a dialog" and
              "opens a card", and it costs 12px of a chip that already ends in
              a hairline. */}
          <Icon name="chevron-down" size={12} class="workdir-caret" />
        </button>
      </div>
      {/* The card is the roster's twin — a right-edge sheet with its own
          scrim — so the two picks on the setup strip behave the same way. The
          popover form it had (a 340px menu hanging off a chip) only worked
          while the trigger sat in the composer's footer; from the strip above
          the box it had no side to hang from. */}
      <dialog
        ref={dialog}
        class="wd-dialog"
        aria-label={label}
        onClose={() => {
          setOpen(false)
          setQuery('')
        }}
        onClick={(e) => {
          if (e.target === e.currentTarget) close()
        }}
      >
        {open && (
          <div class="wd-panel" data-tour-region={tourTarget ? '' : undefined}>
            {/* A title and a count, because the card is a place now, not a
                menu: it lists every directory the console knows, and the
                number is the cheapest way to say whether the list is the
                whole list. The cross is the roster's own — one way out, in
                the corner every card in this console puts it. */}
            <div class="wd-head">
              <p class="wd-title">
                {t('sessions.workdirTitle')}
                {!!spaces?.length && <span class="wd-count">{spaces.length}</span>}
              </p>
              <button type="button" class="icon-btn" aria-label={t('common.close')} onClick={close}>
                <Icon name="x" size={16} />
              </button>
            </div>
            <div class="wd-search-wrap">
              <Icon name="search" size={14} class="wd-search-icon" />
              <input
                class="input wd-search"
                type="search"
                value={query}
                placeholder={t('sessions.workdirSearch')}
                aria-label={t('sessions.workdirSearch')}
                autoFocus
                onInput={(e) => setQuery((e.target as HTMLInputElement).value)}
                onKeyDown={(e) => {
                  // An IME's confirm key is an Enter too. Committing here would
                  // take the highlighted row (or mint a folder) while the
                  // candidate the person is still choosing has not been
                  // composed into the field.
                  if (e.isComposing || e.keyCode === 229) return
                  if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
                    e.preventDefault()
                    const step = e.key === 'ArrowDown' ? 1 : -1
                    setActive((i) => Math.max(0, Math.min(items.length - 1, i + step)))
                    return
                  }
                  if (e.key === 'Tab' && pathMode && items[cursor]) {
                    // `/add-dir`'s Tab: the suggestion goes into the field, so
                    // the next keystroke continues the path instead of starting
                    // it over.
                    e.preventDefault()
                    setQuery(items[cursor]!.dir)
                    return
                  }
                  if (e.key !== 'Enter') return
                  // Enter belongs to the card, never to the form around it: the
                  // composer is a `<form>` whose submit is "send", so an Enter
                  // that fell through would post the draft because the search
                  // found nothing.
                  e.preventDefault()
                  // In path mode the field is the answer: a listing that was
                  // fetched for an earlier query must not be what Enter
                  // commits (see pathEnterTarget).
                  const row = pathMode
                    ? pathEnterTarget(query, items, pathRowsFor, cursor)
                    : (items[cursor] ?? items[0])
                  if (!row) return
                  if (row.create) void createAndPick(row.dir)
                  else choose(row.dir)
                }}
              />
            </div>
            <div class="wd-list" role="listbox" aria-label={label}>
              {!listed ? (
                <p class="wd-empty">{t('common.loading')}</p>
              ) : pathError ? (
                <p class="wd-empty">{t('sessions.workdirPathEmpty')}</p>
              ) : (
                <>
                  {pathMode ? (
                    items.map((r) => row(r))
                  ) : (
                    <>
                      {items[0] && row(items[0])}
                      {groups.map((g) => (
                        <Fragment key={g.key}>
                          <p class="wd-group-head">{groupLabel(g.key)}</p>
                          {g.rows.map((r) => row(r))}
                        </Fragment>
                      ))}
                    </>
                  )}
                  {/* A band can exist without a project row: a starred folder
                      nobody has filed work under is still a row in the card,
                      and the empty state under it would contradict what is
                      on screen. */}
                  {folderRows.length === 0 && groups.length === 0 && (
                    <p class="wd-empty">
                      {pathMode
                        ? t('sessions.workdirNoMatch')
                        : all.length === 0
                          ? t('sessions.workdirEmpty')
                          : t('sessions.workdirNoMatch')}
                    </p>
                  )}
                </>
              )}
            </div>
            {hint && <p class="wd-hint">{hint}</p>}
            <div class="wd-actions">
              <button type="button" class="wd-action" onClick={browse}>
                <Icon name="folder-plus" size={15} />
                {t('sessions.workdirNew')}
                {/* The roster's footer ends in the same chevron: both rows
                    leave the card for somewhere else (the OS chooser, the
                    agents page), so both say so the same way. */}
                <Icon name="chevron-right" size={14} class="node-foot-arrow" />
              </button>
            </div>
          </div>
        )}
      </dialog>
      {pick.modalOpen && (
        <DirBrowserModal
          initialPath={value}
          onSelect={(selectedPath) => {
            onChange(selectedPath)
            pick.setModalOpen(false)
          }}
          onClose={() => pick.setModalOpen(false)}
        />
      )}
    </div>
  )
}

function DirBrowserModal({
  initialPath,
  onSelect,
  onClose,
}: {
  initialPath?: string
  onSelect: (path: string) => void
  onClose: () => void
}) {
  const [currentPath, setCurrentPath] = useState(initialPath || '')
  const [listing, setListing] = useState<DirectoryListing | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const box = useRef<HTMLDivElement>(null)
  useModalFocus(box, true)

  async function loadPath(path: string) {
    setLoading(true)
    setError('')
    try {
      const res = await api.listDirectories(path)
      setListing(res)
      setCurrentPath(res.current)
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    loadPath(currentPath)
  }, [])

  const parts = currentPath ? currentPath.split(/[/\\]/).filter(Boolean) : []
  const isWindows = listing?.separator === '\\'

  return (
    <div
      class="modal-backdrop"
      onClick={(e) => {
        if (e.target === e.currentTarget) onClose()
      }}
    >
      <div
        ref={box}
        class="modal dir-browser-modal"
        role="dialog"
        aria-modal="true"
        aria-label={t('projects.selectDirTitle')}
        onKeyDown={(e) => {
          if (e.key === 'Escape') {
            e.stopPropagation()
            onClose()
          }
        }}
      >
        <div class="dir-browser-header">
          <h2 class="modal-title">{t('projects.selectDirTitle')}</h2>
          <button type="button" class="btn-icon" onClick={onClose} title={t('common.close')}>
            ✕
          </button>
        </div>

        <div class="dir-breadcrumbs">
          <button
            type="button"
            class="dir-crumb-btn"
            onClick={() => loadPath(isWindows ? 'C:\\' : '/')}
          >
            {isWindows ? 'C:\\' : '/'}
          </button>
          {parts.map((p, i) => {
            // On Windows parts[0] is the drive ("C:") — a bare "C:" resolves
            // to the process cwd on that drive, not the drive root, so it
            // needs its trailing separator.
            const pathUpTo = isWindows && i === 0
              ? parts[0] + '\\'
              : (isWindows ? '' : '/') + parts.slice(0, i + 1).join(isWindows ? '\\' : '/')
            return (
              <span key={i} class="dir-crumb-item">
                <span class="dir-crumb-sep">/</span>
                <button
                  type="button"
                  class={`dir-crumb-btn ${i === parts.length - 1 ? 'active' : ''}`}
                  onClick={() => loadPath(pathUpTo)}
                >
                  {p}
                </button>
              </span>
            )
          })}
        </div>

        {listing?.parent && listing.parent !== listing.current && (
          <div class="dir-up-row">
            <button
              type="button"
              class="dir-entry-btn up"
              onClick={() => loadPath(listing.parent)}
            >
              <Icon name="arrow-up" class="dir-icon" />
              <span>.. ({t('projects.parentDir')})</span>
            </button>
          </div>
        )}

        <div class="dir-entries-list">
          {loading ? (
            <div class="dim dir-loading">
              <span class="spinner spinner-inline" aria-hidden="true" />
              {t('common.loading')}
            </div>
          ) : error ? (
            <div class="dir-error">{error}</div>
          ) : listing?.directories && listing.directories.length > 0 ? (
            listing.directories.map((entry) => (
              <button
                key={entry.path}
                type="button"
                class="dir-entry-btn"
                onClick={() => loadPath(entry.path)}
              >
                <Icon name="folder" class="dir-icon" />
                <span class="dir-entry-name">{entry.name}</span>
              </button>
            ))
          ) : (
            <div class="dim dir-empty">{t('projects.noSubdirs')}</div>
          )}
        </div>

        <div class="dir-browser-footer">
          <div class="dir-current-selected">
            <span class="dim">{t('projects.currentPath')}: </span>
            <code>{currentPath || '/'}</code>
          </div>
          <div class="modal-actions">
            <button type="button" class="btn" onClick={onClose}>
              {t('common.cancel')}
            </button>
            <button
              type="button"
              class="btn primary"
              disabled={!currentPath}
              onClick={() => onSelect(currentPath)}
            >
              {t('projects.confirmSelect')}
            </button>
          </div>
        </div>
      </div>
    </div>
  )
}
