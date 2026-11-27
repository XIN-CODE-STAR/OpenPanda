// SPDX-License-Identifier: AGPL-3.0-or-later

import assert from 'node:assert/strict'
import { test } from 'node:test'
import type { DiscoveredModel } from '../api/client.ts'
import { splitProviders, usable } from './model-ccswitch-groups.ts'

function row(over: Partial<DiscoveredModel> & { id: string; model: string }): DiscoveredModel {
  return {
    source: 'ccswitch',
    provider: 'custom',
    base_url: 'https://relay.example/v1',
    api_type: 'anthropic',
    importable: true,
    ...over,
  }
}

test('one section per provider, in the order the server sent them', () => {
  const sections = splitProviders([
    row({
      id: 'a1',
      model: 'claude-opus-4-8',
      group: 'https://a.example',
      group_label: 'AgentRouter',
      base_url: 'https://a.example',
    }),
    row({
      id: 'a2',
      model: 'gpt-5.6-sol',
      group: 'https://a.example',
      group_label: 'AgentRouter',
      base_url: 'https://a.example',
    }),
    row({
      id: 'b1',
      model: 'MiniMax-M3',
      group: 'https://b.example',
      group_label: 'MiniMax',
      base_url: 'https://b.example/anthropic',
    }),
  ])
  assert.deepEqual(
    sections.map((s) => [s.key, s.label, s.host]),
    [
      ['https://a.example', 'AgentRouter', 'a.example'],
      ['https://b.example', 'MiniMax', 'b.example'],
    ],
  )
  assert.equal(sections[0]!.rows.length, 2)
  assert.equal(sections[1]!.rows.length, 1)
  assert.deepEqual(sections[0]!.ids, ['a1', 'a2'])
  assert.deepEqual(sections[1]!.ids, ['b1'])
})

test('an already registered row is listed but not tickable', () => {
  const sections = splitProviders([
    row({ id: 'fresh', model: 'm-1', group: 'https://a.example', group_label: 'A' }),
    row({ id: 'have', model: 'm-2', group: 'https://a.example', group_label: 'A', existing: true }),
  ])
  assert.equal(sections.length, 1)
  assert.deepEqual(sections[0]!.rows.map((c) => c.id), ['fresh', 'have'])
  // A group tick must not spend a request on a row the registry already holds.
  assert.deepEqual(sections[0]!.ids, ['fresh'])
  assert.equal(usable(sections[0]!.rows[1]!), false)
})

test('a row with no group falls back to one section named after its host', () => {
  const sections = splitProviders([
    row({ id: 'x', model: 'm', base_url: 'https://relay.example/v1' }),
    row({ id: 'y', model: 'n', base_url: 'https://relay.example/v1' }),
  ])
  assert.equal(sections.length, 1)
  assert.equal(sections[0]!.key, '')
  assert.equal(sections[0]!.label, 'relay.example')
  assert.equal(sections[0]!.host, 'relay.example')
  assert.deepEqual(sections[0]!.ids, ['x', 'y'])
})

test('a section without a label borrows the provider label', () => {
  const sections = splitProviders([
    row({ id: 'x', model: 'm', base_url: '', group: 'g', provider_label: 'Custom' }),
  ])
  assert.equal(sections[0]!.label, 'Custom')
  assert.equal(sections[0]!.host, '')
})
