// SPDX-License-Identifier: AGPL-3.0-or-later

import { useLocaleRerender } from '../hooks'
import { t } from '../i18n'
import type { ControlTab } from '../nav'
import { QueueView } from './queue'
import { PlansView } from './plans'
import { ProjectsView } from './projects'

// 队列·任务·项目 — the node's three boards under one tab strip. They used to be
// three rail rows; they are now one page named after what it holds: the live
// kanban (what is waiting / running / needs your approval), the plan pipelines
// (what the engine has broken into stages), and the projects (where that work
// lands and what it remembers). They answer the same question at three time
// scales, so they share one page and one header; the boards themselves are
// unchanged — each view still owns its own layout and simply stops painting its
// own title (`head={false}`).

const TABS: ControlTab[] = ['queue', 'plans', 'projects']

/** One subtitle per board, so the header says what the visible tab shows. */
const SUBTITLE: Record<ControlTab, string> = {
  queue: 'queue.subtitle',
  plans: 'plans.subtitle',
  projects: 'projects.subtitle',
}

export function ControlView({
  tab,
  project,
  focusReview,
  onSelectTab,
  onOpen,
  onOpenSession,
  onOpenProject,
  onOpenQueue,
}: {
  tab: ControlTab
  project?: string | null
  focusReview?: boolean
  onSelectTab(tab: ControlTab): void
  onOpen(id: string): void
  onOpenSession(id: string): void
  onOpenProject(name: string): void
  onOpenQueue(project: string): void
}) {
  useLocaleRerender()

  return (
    <section class="control-section">
      {/* Title row, not a title block: the tab strip rides beside the heading
          instead of under it. A stacked strip would push the board down by
          ~44px, and the board is height-locked to six cards — it would then
          run past the fold and the page would scroll under a board that
          already scrolls inside each column. */}
      <div class="control-head">
        <div class="control-head-text">
          <h1 class="page-title">{t('nav.control')}</h1>
          <p class="page-sub">{t(SUBTITLE[tab])}</p>
        </div>
        <div class="segmented control-tabs" role="tablist" aria-label={t('nav.control')}>
          {TABS.map((k) => (
            <button
              key={k}
              type="button"
              role="tab"
              aria-selected={tab === k}
              class={`seg${tab === k ? ' on' : ''}`}
              onClick={() => onSelectTab(k)}
            >
              {t(`nav.${k}`)}
            </button>
          ))}
        </div>
      </div>

      {tab === 'plans' ? (
        <PlansView head={false} />
      ) : tab === 'projects' ? (
        <ProjectsView head={false} onOpenProject={onOpenProject} onOpenQueue={onOpenQueue} />
      ) : (
        <QueueView
          head={false}
          project={project}
          focusReview={focusReview}
          onOpen={onOpen}
          onOpenSession={onOpenSession}
        />
      )}
    </section>
  )
}
