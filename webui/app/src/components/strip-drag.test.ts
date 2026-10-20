// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { wheelStep } from './strip-drag.ts'

// A 348px strip holding 7 tiles of 156px + 8px gaps: 1140px of content, so
// ~792px of travel — enough to tell "the middle" from either end.
const strip = { clientWidth: 348, scrollWidth: 1140 }

describe('strip wheel', () => {
  it('turns a wheel notch into horizontal travel', () => {
    assert.equal(wheelStep({ ...strip, deltaX: 0, deltaY: 120, shiftKey: false, scrollLeft: 100 }), 120)
    assert.equal(wheelStep({ ...strip, deltaX: 0, deltaY: -120, shiftKey: false, scrollLeft: 100 }), -120)
  })

  it('leaves a gesture the browser already reads as horizontal alone', () => {
    // Shift-wheel and a trackpad's sideways swipe both move the strip natively;
    // adding our step on top would scroll it twice as far.
    assert.equal(wheelStep({ ...strip, deltaX: 0, deltaY: 120, shiftKey: true, scrollLeft: 100 }), 0)
    assert.equal(wheelStep({ ...strip, deltaX: 120, deltaY: 4, shiftKey: false, scrollLeft: 100 }), 0)
  })

  it('gives the wheel back to the card at either end', () => {
    // The card scrolls vertically and the strip is one band of it: a strip that
    // ate the notch at its own end would be a dead spot in the middle of a page.
    assert.equal(wheelStep({ ...strip, deltaX: 0, deltaY: 120, shiftKey: false, scrollLeft: 792 }), 0)
    assert.equal(wheelStep({ ...strip, deltaX: 0, deltaY: -120, shiftKey: false, scrollLeft: 0 }), 0)
  })

  it('does nothing to a strip that has no room to move', () => {
    assert.equal(
      wheelStep({ deltaX: 0, deltaY: 120, shiftKey: false, scrollLeft: 0, clientWidth: 348, scrollWidth: 348 }),
      0,
    )
  })
})
