// SPDX-License-Identifier: AGPL-3.0-or-later

// CC Switch's own import sheet: one tab per app roster (Claude Code, Claude
// Desktop, Codex, Gemini), one roster on screen at a time.
//
// It is a separate door from the discover sheet on purpose. The store holds
// four independent lists that answer different questions — "which Claude Code
// relays do I have", "which Codex providers" — and folding them into one list
// of thirty rows made the two a person actually wanted impossible to find.
// Choosing a tab is choosing which list to look at; nothing is shown for the
// rosters nobody opened.
//
// Inside a tab the list is split a second time, by the provider the person
// saved in CC Switch. A roster of twelve providers flattened into one
// checklist is the same wall the tabs were meant to remove; a heading per
// provider — with its own tick, its host and its model count — is the shape
// CC Switch itself shows, and the one a person can scan.

import { useEffect, useRef, useState } from 'preact/hooks'
import { api, type DiscoverSource, type ModelImportResult } from '../api/client'
import { useModalFocus } from '../components/modal-focus'
import { Icon } from '../components/icons'
import { t } from '../i18n'
import { splitProviders, usable } from './model-ccswitch-groups'
import { fmtTokens } from './model-discover'
import { ImportFailures, markImported, useModelImport } from './model-import'

export function CCSwitchSheet(props: {
  onClose(): void
  onDone(result: ModelImportResult): void
}) {
  const [groups, setGroups] = useState<DiscoverSource[] | null>(null)
  const [error, setError] = useState('')
  const [tab, setTab] = useState('')
  // Nothing is ticked on open. A CC Switch roster is somebody's list of saved
  // relays, not a shopping list: the sheet starts blank so the footer's "N
  // selected" is always a number the person chose. Ticking a whole roster is
  // one click away, which is the right cost for "import all of these".
  const [picked, setPicked] = useState<Set<string>>(new Set())
  // Provider sections start open: the sheet's job is ticking models, and a
  // closed section would hide both the rows and their "already imported"
  // state behind a click nobody asked for. Collapsing is for skipping a
  // provider the person already knows they do not want.
  const [closed, setClosed] = useState<Set<string>>(new Set())
  const importer = useModelImport(props.onDone, props.onClose)
  const box = useRef<HTMLDivElement>(null)
  useModalFocus(box, true)

  function load() {
    importer.clearFailures()
    setError('')
    setGroups(null)
    api
      .ccswitchGroups()
      .then((r) => {
        setGroups(r.groups)
        // Open the first roster that has something to offer. A group that is
        // configured but has no importable row stays one click away instead of
        // taking the opening screen.
        const first = r.groups.find((g) => g.candidates.some(usable))
          ?? r.groups.find((g) => g.candidates.length > 0)
          ?? r.groups[0]
        setTab((previous) => r.groups.some((g) => g.id === previous) ? previous : first?.id ?? '')
        setPicked((previous) => new Set(r.groups.flatMap((g) => g.candidates)
          .filter((c) => usable(c) && previous.has(c.id))
          .map((c) => c.id)))
        setClosed(new Set())
      })
      .catch((e: unknown) => setError(e instanceof Error ? e.message : String(e)))
  }

  useEffect(load, [])

  function toggle(id: string) {
    setPicked((cur) => {
      const next = new Set(cur)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }

  // One tick, many rows: a provider's models or a whole roster. The caller
  // says which way the tick goes, so "some of them are on" resolves to "all
  // of them" instead of flipping each row to its opposite.
  function tick(ids: string[], on: boolean) {
    setPicked((cur) => {
      const next = new Set(cur)
      for (const id of ids) {
        if (on) next.add(id)
        else next.delete(id)
      }
      return next
    })
  }

  function submit() {
    if (!groups) return
    void importer.submit(groups, picked, (ids) => {
      setGroups((current) => markImported(current, ids))
      setPicked((current) => new Set([...current].filter((id) => !ids.has(id))))
    })
  }

  const current = groups?.find((g) => g.id === tab) ?? null
  const noStore = groups?.find((g) => g.id === 'ccswitch')
  const count = groups ? picked.size : 0
  const rows = current ? current.candidates.filter((c) => usable(c) || c.existing) : []
  const sections = splitProviders(rows)
  const hereIds = current ? current.candidates.filter(usable).map((c) => c.id) : []
  // How many of the ticked rows the open roster owns. A selection made on
  // another tab survives (the import covers every group), so the footer has to
  // say where the rest of it lives — otherwise "5 selected" next to one ticked
  // row reads as a bug.
  const here = current ? current.candidates.filter((c) => usable(c) && picked.has(c.id)).length : 0
  const allHere = hereIds.length > 0 && hereIds.every((id) => picked.has(id))

  function toggleOpen(key: string) {
    setClosed((cur) => {
      const next = new Set(cur)
      if (next.has(key)) next.delete(key)
      else next.add(key)
      return next
    })
  }

  return (
    <div class="modal-backdrop" onClick={(e) => e.target === e.currentTarget && importer.close()}>
      <div
        ref={box}
        class="modal wizard-modal"
        role="dialog"
        aria-modal="true"
        aria-labelledby="ccswitch-title"
        onKeyDown={(e) => {
          if (e.key === 'Escape') {
            e.stopPropagation()
            importer.close()
          }
        }}
      >
        <header class="wizard-head">
          <div class="wizard-head-text">
            <h3 id="ccswitch-title" class="wizard-title">
              {t('models.ccswitch.title')}
            </h3>
            <p class="wizard-sub">{t('models.ccswitch.sub')}</p>
          </div>
          <button
            type="button"
            class="btn-icon wizard-close"
            aria-label={t('common.close')}
            title={t('common.close')}
            onClick={importer.close}
            disabled={importer.busy}
          >
            <Icon name="x" size={16} />
          </button>
        </header>

        <div class="wizard-form">
          <div class="wizard-pane">
            {!groups && !error && <p class="wizard-empty">{t('models.ccswitch.scanning')}</p>}

            {error && (
              <div class="gate-error discover-error">
                <span>{error}</span>
                <button type="button" class="btn small" onClick={load}>
                  {t('common.retry')}
                </button>
              </div>
            )}

            <ImportFailures failures={importer.failures} />

            {noStore && (
              <p class="discover-none" role="status" title={noStore.detail}>
                {t(noStore.status === 'missing' ? 'models.ccswitch.missing' : 'models.ccswitch.readError')}
              </p>
            )}

            {groups && !noStore && (
              <div class="ccswitch-tabs" role="tablist" aria-label={t('models.ccswitch.title')}>
                {groups.map((g) => {
                  const usableCount = g.candidates.filter(usable).length
                  const on = g.id === tab
                  return (
                    <button
                      key={g.id}
                      type="button"
                      role="tab"
                      aria-selected={on}
                      class={`ccswitch-tab${on ? ' on' : ''}${usableCount === 0 ? ' bare' : ''}`}
                      onClick={() => {
                        setTab(g.id)
                        // Sections are keyed by endpoint, and the same relay
                        // can appear in two rosters: a collapse made on the
                        // Claude tab must not follow the person to Codex.
                        setClosed(new Set())
                      }}
                    >
                      <span class="ccswitch-tab-name">{g.label}</span>
                      <span class="ccswitch-tab-count">{usableCount}</span>
                    </button>
                  )
                })}
              </div>
            )}

            {current && !noStore && (
              <>
                {hereIds.length === 0 && (
                  <p class="discover-none">
                    {t(sections.length > 0 ? 'models.ccswitch.allRegistered' : 'models.ccswitch.empty')}
                  </p>
                )}

                {hereIds.length > 0 && (
                  <div class="ccswitch-bar">
                    <label class="ccswitch-all">
                      <TriCheck
                        checked={allHere}
                        indeterminate={!allHere && here > 0}
                        ariaLabel={t('models.discover.selectAll')}
                        disabled={importer.busy}
                        onChange={() => tick(hereIds, !allHere)}
                      />
                      <span>{t('models.discover.selectAll')}</span>
                    </label>
                    {/* The server's own sentence ("12 provider(s), 10
                        importable · 2 skipped") stays a tooltip: it counts
                        providers and models, and the two numbers do not line
                        up with the rows on screen. What the bar states is
                        what the list holds. */}
                    <span class="ccswitch-roster" title={current.detail || undefined}>
                      {t('models.ccswitch.roster', { p: sections.length, n: hereIds.length })}
                    </span>
                  </div>
                )}

                {sections.map((s) => {
                  const all = s.ids.length > 0 && s.ids.every((id) => picked.has(id))
                  const some = s.ids.some((id) => picked.has(id))
                  const open = !closed.has(s.key)
                  return (
                    <section class="ccswitch-group" key={s.key}>
                      <header class="ccswitch-group-head">
                        {s.ids.length > 0 ? (
                          <label class="ccswitch-group-pick">
                            <TriCheck
                              checked={all}
                              indeterminate={!all && some}
                              ariaLabel={`${all ? t('models.discover.clearAll') : t('models.discover.selectAll')} · ${s.label}`}
                              disabled={importer.busy}
                              onChange={() => tick(s.ids, !all)}
                            />
                          </label>
                        ) : (
                          <span class="ccswitch-group-pick" aria-hidden="true" />
                        )}
                        <button
                          type="button"
                          class="ccswitch-group-name"
                          aria-expanded={open}
                          onClick={() => toggleOpen(s.key)}
                        >
                          <Icon name={open ? 'chevron-down' : 'chevron-right'} size={13} />
                          <span class="ccswitch-group-label">{s.label}</span>
                          {s.host && s.host !== s.label && (
                            <span class="ccswitch-group-host mono">{s.host}</span>
                          )}
                          <span class="ccswitch-group-count">{s.rows.length}</span>
                        </button>
                      </header>
                      {open &&
                        s.rows.map((c) => {
                          const on = usable(c)
                          return (
                            <label
                              key={c.id}
                              class={`discover-row ccswitch-row${on ? '' : ' off'}`}
                            >
                              <input
                                type="checkbox"
                                disabled={!on || importer.busy}
                                checked={on && picked.has(c.id)}
                                onChange={() => toggle(c.id)}
                              />
                              <span class="discover-model mono">{c.model}</span>
                              <span class="discover-meta">
                                {c.context_window ? (
                                  <span class="ccswitch-window">{fmtTokens(c.context_window)}</span>
                                ) : null}
                                {c.existing && (
                                  <span class="discover-tag">{t('models.discover.existing')}</span>
                                )}
                              </span>
                            </label>
                          )
                        })}
                    </section>
                  )
                })}
              </>
            )}
          </div>

          <footer class="wizard-foot">
            <span class="wizard-foot-note">
              {here === count
                ? t('models.discover.picked', { n: count })
                : t('models.ccswitch.pickedSplit', { n: count, here })}
            </span>
            <button type="button" class="btn" onClick={load} disabled={importer.busy || !groups}>
              {t('models.discover.refresh')}
            </button>
            <button type="button" class="btn" onClick={importer.close} disabled={importer.busy}>
              {t('common.cancel')}
            </button>
            <button
              type="button"
              class="btn primary"
              disabled={count === 0 || importer.busy}
              onClick={submit}
            >
              {importer.busy
                ? t('models.discover.importing')
                : count === 0
                  ? t('models.discover.importNone')
                  : t('models.discover.import', { n: count })}
            </button>
          </footer>
        </div>
      </div>
    </div>
  )
}

/** A checkbox that can also render "some of this group". The DOM has no
 *  attribute for indeterminate — it is a property that only exists after the
 *  element does — so it has to be written on every change. */
function TriCheck(props: {
  checked: boolean
  indeterminate: boolean
  ariaLabel: string
  disabled: boolean
  onChange(on: boolean): void
}) {
  const ref = useRef<HTMLInputElement>(null)
  useEffect(() => {
    if (ref.current) ref.current.indeterminate = props.indeterminate
  }, [props.indeterminate])
  return (
    <input
      type="checkbox"
      ref={ref}
      aria-label={props.ariaLabel}
      checked={props.checked}
      disabled={props.disabled}
      onChange={(e) => props.onChange((e.target as HTMLInputElement).checked)}
    />
  )
}
