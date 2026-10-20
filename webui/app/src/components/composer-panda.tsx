// SPDX-License-Identifier: AGPL-3.0-or-later

import { useEffect, useState } from 'preact/hooks'
import { t } from '../i18n'
import { PANDA_REST_POSE_MS, isPandaAwake, restingPose } from './panda-rest'

/* The pet that stands on the composer's top-left corner.
 *
 * It used to be a row of its own above the box — a 56px sprite plus a dot and
 * the word "待命" — which cost the new-chat page a line of empty canvas and
 * read as a status readout for a thread that does not exist yet. The pose is
 * the readout now: it stands when the console is free and cheers while a turn
 * streams. The words stay for screen readers, so the live region still says
 * what the sprite shows.
 *
 * It also keeps office hours. From 08:00 to 20:00 the pose answers the
 * console; the other twelve hours it lies on the edge and turns over every
 * half minute, because a pet that is standing at three in the morning is a
 * pet that has not noticed it is three in the morning. Night wins over the
 * working frame on purpose: the schedule is the user's, and an agent running
 * overnight is exactly the case where nobody needs the sprite shouting about
 * it. Screen readers still hear "Working" — the pose is decoration, the live
 * region is the status.
 *
 * Pressing it is a toy: a press squashes it flat and lets it ring back up, the
 * way a jelly toy answers a poke. Every press is that same press — presses are
 * not counted, there is no streak to chase and nothing to remember. The whole
 * interaction leaves the page alone but for the sprite. */
export function ComposerPanda({ working }: { working: boolean }) {
  /* A counter, not a flag: the animation has to replay on every press, and
     bumping the sprite's `key` remounts it, which is the only reliable way to
     restart a CSS animation that is already on the element (re-adding the same
     class does nothing). It returns to 0 when the animation ends, so the next
     press starts from a clean element. */
  const [bounce, setBounce] = useState(0)
  const bouncing = bounce > 0

  /* The hour is sampled on a timer, not once at mount: this panel is a tab
     people leave open overnight, and a pet that went to bed at 20:00 but is
     still standing at 08:05 would be the one thing on the page that lies. A
     minute is fine — the pose has two deadlines a day, not a stopwatch. */
  const [hour, setHour] = useState(() => new Date().getHours())
  useEffect(() => {
    const id = window.setInterval(() => setHour(new Date().getHours()), 60_000)
    return () => window.clearInterval(id)
  }, [])
  const awake = isPandaAwake(hour)

  /* Off duty it naps, and it turns over now and then: one frozen frame for
     eight hours reads as a broken image, not as a sleeping pet. The timer only
     exists while the pet is asleep, and the pose resets on the way back on
     duty so the next night starts from the same frame. */
  const [restStep, setRestStep] = useState(0)
  useEffect(() => {
    if (awake) {
      setRestStep(0)
      return
    }
    const id = window.setInterval(() => setRestStep((n) => n + 1), PANDA_REST_POSE_MS)
    return () => window.clearInterval(id)
  }, [awake])
  const pose = restingPose(restStep)

  /* `is-bouncing` is added on top of whatever pose is showing, never instead
     of it: a poke at night squashes the pet it is actually looking at, and the
     sheet keeps the resting frame. The class order matters in the stylesheet,
     not here. */
  const cls =
    'composer-panda' +
    (awake ? (working ? ' is-working' : '') : ' is-resting') +
    (bouncing ? ' is-bouncing' : '')

  return (
    <button
      type="button"
      class={cls}
      /* Named for what a press does. "Ready" on a button tells you nothing
         about the press; the state is the live region's job, below.
         No `title`: the pet is decoration first, and a native tooltip popped
         a grey bubble over the composer's top-left corner on every pass of
         the pointer. The name still reaches assistive tech through
         `aria-label`, so nothing is lost but the bubble. */
      aria-label={t('panda.poke')}
      onClick={() => {
        // While a turn streams the pet is busy cheering, and a squish would
        // fight the working animation — a press there is ignored rather than
        // queued for the moment the turn ends.
        if (working) return
        setBounce((n) => n + 1)
      }}
    >
      {/* The stage is the mirror: turning over at night flips it, and the
          transition on `transform` walks it through zero width, which reads as
          the pet rolling onto its other side. The sprite inside keeps its own
          animations, so the two never fight over `transform`. */}
      <span class={'composer-panda-stage' + (!awake && pose === 'flip' ? ' is-flipped' : '')} aria-hidden="true">
        <span
          key={bounce}
          class="composer-panda-sprite"
          onAnimationEnd={() => setBounce(0)}
        />
      </span>
      <span class="composer-panda-label" role="status" aria-live="polite" aria-atomic="true">
        {t(working ? 'panda.working' : awake ? 'panda.ready' : 'panda.resting')}
      </span>
    </button>
  )
}
