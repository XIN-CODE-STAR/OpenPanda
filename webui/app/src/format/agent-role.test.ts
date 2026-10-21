// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it } from 'node:test'
import assert from 'node:assert/strict'

import { agentInitial, agentRoles, agentTint } from './agent-role.ts'

describe('agentRoles', () => {
  it('folds registry tags into roles, keeping the agent’s own order', () => {
    assert.deepEqual(agentRoles(['multi_file_edits', 'code_search', 'refactoring']), [
      'code',
      'search',
    ])
  })

  it('drops unknown tags instead of printing them', () => {
    assert.deepEqual(agentRoles(['gpu:train', 'running_tests']), ['test'])
    assert.deepEqual(agentRoles(['gpu:train']), [])
  })

  it('tolerates missing and malformed input', () => {
    assert.deepEqual(agentRoles(undefined), [])
    assert.deepEqual(agentRoles([]), [])
    assert.deepEqual(agentRoles(['  Code_Review ']), ['review'])
  })
})

describe('agentInitial', () => {
  it('takes the first code point, not the first UTF-16 unit', () => {
    assert.equal(agentInitial('Claude Code'), 'C')
    assert.equal(agentInitial('熊猫'), '熊')
    assert.equal(agentInitial('  codex'), 'C')
    assert.equal(agentInitial(''), '?')
  })
})

describe('agentTint', () => {
  it('is stable per name and stays inside the palette', () => {
    const a = agentTint('claude_code')
    assert.equal(a, agentTint('claude_code'))
    for (const n of ['claude_code', 'codex', 'opencode', 'pi', 'zcode', '']) {
      const v = agentTint(n)
      assert.ok(v >= 0 && v < 5, `${n} → ${v}`)
    }
  })
})
