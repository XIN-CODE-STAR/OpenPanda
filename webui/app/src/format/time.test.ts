// SPDX-License-Identifier: AGPL-3.0-or-later

import assert from 'node:assert/strict'
import { describe, it } from 'node:test'

import { clockStamp, clockTime } from './time.ts'

/** A stamp for 08:31 on the runner's own clock: the formatter prints local
 *  time, so the expectation has to be built the same way or the test would
 *  only pass in one timezone. */
function at(hour: number, minute: number): number {
  return Math.floor(new Date(2026, 9, 20, hour, minute, 0).getTime() / 1000)
}

describe('clockTime', () => {
  it('prints hour and minute, no seconds', () => {
    assert.equal(clockTime(at(8, 31), 'zh-CN'), '08:31')
    assert.equal(clockTime(at(23, 5), 'zh-CN'), '23:05')
  })

  it('lets the locale pick the 12/24-hour form', () => {
    // en-US spells the meridiem; the digits are still there.
    assert.match(clockTime(at(8, 31), 'en-US'), /^08:31\s?(AM)?$/)
  })

  it('draws nothing when the turn carries no stamp', () => {
    // Older transcripts have no ts, and 1970 is not a time anyone wrote.
    assert.equal(clockTime(undefined, 'zh-CN'), '')
    assert.equal(clockTime(0, 'zh-CN'), '')
    assert.equal(clockTime(-1, 'zh-CN'), '')
  })
})

describe('clockStamp', () => {
  it('spells out the day beside the clock, for the tooltip', () => {
    const full = clockStamp(at(8, 31), 'zh-CN')
    assert.match(full, /2026/)
    assert.match(full, /08:31/)
  })

  it('draws nothing when the turn carries no stamp', () => {
    assert.equal(clockStamp(undefined, 'zh-CN'), '')
    assert.equal(clockStamp(0, 'zh-CN'), '')
  })
})
