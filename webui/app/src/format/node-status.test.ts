// SPDX-License-Identifier: AGPL-3.0-or-later

import assert from 'node:assert/strict'
import { describe, it } from 'node:test'

import { isNodeConnected, isSelfNodeUp } from './node-status.ts'

describe('isNodeConnected', () => {
  it('treats the ledger words for a live node as connected', () => {
    assert.equal(isNodeConnected('online'), true)
    assert.equal(isNodeConnected('connected'), true)
  })

  it('ignores case and surrounding space', () => {
    assert.equal(isNodeConnected(' Online '), true)
    assert.equal(isNodeConnected('CONNECTED'), true)
  })

  it('leaves an offline or unknown node dark', () => {
    assert.equal(isNodeConnected('offline'), false)
    assert.equal(isNodeConnected(''), false)
    assert.equal(isNodeConnected(undefined), false)
    assert.equal(isNodeConnected(null), false)
    assert.equal(isNodeConnected('draining'), false)
  })
})

describe('isSelfNodeUp', () => {
  it('lights up when a daemon holds the identity lock', () => {
    assert.equal(isSelfNodeUp({ node_running: true }), true)
  })

  it('lights up on the console shape, where nobody locks but the node is online', () => {
    // `panda web` serves the node in-process: node_running is false while the
    // ledger row it registers is online — the dot must still be green.
    assert.equal(isSelfNodeUp({ node_running: false, node: { status: 'online', running: true } }), true)
  })

  it('stays dark when the registered row is offline or not running', () => {
    assert.equal(isSelfNodeUp({ node_running: false, node: { status: 'offline', running: false } }), false)
    assert.equal(isSelfNodeUp({ node_running: false, node: { status: 'online', running: false } }), false)
    assert.equal(isSelfNodeUp({ node_running: false }), false)
    assert.equal(isSelfNodeUp(null), false)
  })
})
