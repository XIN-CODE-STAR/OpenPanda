// SPDX-License-Identifier: AGPL-3.0-or-later

/** The pet keeps office hours.
 *
 *  From 08:00 to 20:00 it is on duty and the sprite reports what the console
 *  is doing (standing when free, cheering while a turn streams). The other
 *  twelve hours it is off duty and lies on the composer's edge instead — the
 *  pose is the clock, so the corner reads as "nothing is expected of you
 *  tonight" without a single word.
 *
 *  20:00 is excluded from the day shift on purpose: at eight in the evening
 *  the pet has already gone to bed, which is the reading people expect from
 *  "8 to 8". */
export const PANDA_AWAKE_FROM = 8
export const PANDA_AWAKE_UNTIL = 20

/** How long each resting pose is held before the pet turns over. Eight hours
 *  on one frame looks like a broken image; a pet that shifts every half
 *  minute looks restless. Half a minute is a nap. */
export const PANDA_REST_POSE_MS = 30_000

/** `hour` is a 24-hour clock reading, the same number `Date#getHours` gives. */
export function isPandaAwake(hour: number): boolean {
  return hour >= PANDA_AWAKE_FROM && hour < PANDA_AWAKE_UNTIL
}

/** The two off-duty poses, named for what the sprite does.
 *
 *  Both are frame 6, the side-on sprawl with the face showing — the one that
 *  reads as a panda lying down. It used to alternate with frame 4 (face down,
 *  limbs splayed to the corners), which at 44px is a faceless blob: a pet that
 *  had turned its back on the console rather than gone to sleep. `flip` is the
 *  same frame mirrored, so the pet still turns over every half minute — it just
 *  turns over onto its other side instead of into a shape nobody can name. */
export type PandaRestPose = 'lie' | 'flip'

/** Alternates the poses on every step, so a caller can just count. */
export function restingPose(step: number): PandaRestPose {
  return Math.abs(Math.trunc(step)) % 2 === 0 ? 'lie' : 'flip'
}
