// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it } from 'node:test'
import assert from 'node:assert/strict'
import { isPandaAwake, restingPose } from './panda-rest.ts'

describe('isPandaAwake', () => {
  it('is on duty from 08:00 up to, but not including, 20:00', () => {
    assert.equal(isPandaAwake(8), true)
    assert.equal(isPandaAwake(12), true)
    assert.equal(isPandaAwake(19), true)
  })

  it('is off duty from 20:00 to 07:59', () => {
    assert.equal(isPandaAwake(20), false)
    assert.equal(isPandaAwake(23), false)
    assert.equal(isPandaAwake(0), false)
    assert.equal(isPandaAwake(7), false)
  })

  it('handles the edges of the day exactly once each', () => {
    assert.equal(isPandaAwake(7.99), false)
    assert.equal(isPandaAwake(8), true)
    assert.equal(isPandaAwake(19.99), true)
    assert.equal(isPandaAwake(20), false)
  })
})

describe('restingPose', () => {
  it('starts lying down', () => {
    assert.equal(restingPose(0), 'lie')
  })

  it('alternates on every step', () => {
    assert.equal(restingPose(1), 'flip')
    assert.equal(restingPose(2), 'lie')
    assert.equal(restingPose(3), 'flip')
    assert.equal(restingPose(4), 'lie')
  })

  it('survives a counter that runs backwards or arrives fractional', () => {
    assert.equal(restingPose(-1), 'flip')
    assert.equal(restingPose(2.7), 'lie')
  })
})
