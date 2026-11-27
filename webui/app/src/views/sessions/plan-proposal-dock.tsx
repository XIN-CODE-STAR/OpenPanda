// SPDX-License-Identifier: AGPL-3.0-or-later

import { Icon } from '../../components/icons'
import { t } from '../../i18n'
import { proposalSummary } from './plan-proposal'

/** The whole bar is one read-only door into the side dock. A decision belongs
 *  to the pane with the full document, never to this summary. */
export function PlanProposalDock({ version, text, expanded, onOpen }: {
  version: number
  text: string
  expanded: boolean
  onOpen(): void
}) {
  const summary = proposalSummary(text)
  return (
    <div class="plan-proposal-slot">
      <button type="button" id="plan-proposal-trigger" class="plan-dock-card plan-dock-proposal"
        title={t('sessions.plan.view')} aria-label={t('sessions.plan.review')}
        aria-expanded={expanded} aria-controls={expanded ? 'plan-panel' : undefined}
        aria-describedby="plan-proposal-title plan-proposal-summary" onClick={onOpen}>
        <span class="plan-dock-icon" aria-hidden="true"><Icon name="lightbulb" size={17} /></span>
        <span class="plan-dock-copy">
          <strong id="plan-proposal-title" class="plan-dock-title">{t('sessions.plan.proposal')} · {version}</strong>
          <span id="plan-proposal-summary" class="plan-dock-summary">{summary || t('sessions.plan.ready')}</span>
        </span>
        <span class="btn small primary plan-dock-review" aria-hidden="true">
          <span>{t('sessions.plan.review')}</span>
          <Icon name="chevron-right" size={15} />
        </span>
      </button>
    </div>
  )
}

/** What a running implementation is doing, in the reader's language. The
 *  server sends a stable token; an unknown one (a phase added later) falls
 *  back to "the model is thinking" rather than printing the raw token. */
const PLAN_PHASES: Record<string, string> = {
  pending: 'sessions.plan.phase.pending',
  running: 'sessions.plan.phase.members',
  dispatch: 'sessions.plan.phase.dispatch',
  plan: 'sessions.plan.phase.plan',
  route: 'sessions.plan.phase.route',
  exec: 'sessions.plan.phase.exec',
  tool: 'sessions.plan.phase.tool',
  judge: 'sessions.plan.phase.judge',
  wait: 'sessions.plan.phase.wait',
  retry: 'sessions.plan.phase.retry',
  offline: 'sessions.plan.phase.offline',
  clarify: 'sessions.plan.phase.clarify',
  waiting_input: 'sessions.plan.phase.clarify',
  answer: 'sessions.plan.phase.answer',
  model: 'sessions.plan.phase.model',
}

export function PlanRunningDock({ phase, detail, onStop, onTask }: {
  phase: string
  detail?: string
  onTask?: () => void
  onStop?: () => void
}) {
  const key = PLAN_PHASES[phase] ?? 'sessions.plan.phase.model'
  return (
    <div class="plan-dock">
      <section class="plan-dock-card plan-dock-live" aria-live="polite">
        <span class="plan-dock-icon" aria-hidden="true">
          {phase === 'waiting_input' ? <Icon name="message" size={17} /> : <span class="plan-dock-pulse" />}
        </span>
        <span class="plan-dock-copy">
          <strong class="plan-dock-title">{t('sessions.plan.running')}</strong>
          <span class="plan-dock-summary">
            {t(key)}{detail ? <span class="plan-dock-detail mono"> · {detail}</span> : null}
          </span>
        </span>
        {onTask && <button type="button" class="btn ghost" onClick={onTask}>{t('sessions.taskLog')}</button>}
        {onStop && <button type="button" class="btn ghost plan-dock-review" onClick={onStop}>
          {t('sessions.stop')}
        </button>}
      </section>
    </div>
  )
}
