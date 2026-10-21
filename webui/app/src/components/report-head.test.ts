// SPDX-License-Identifier: AGPL-3.0-or-later

import assert from 'node:assert/strict'
import { describe, it } from 'node:test'

import { splitReportHead } from './report-head.ts'

describe('splitReportHead', () => {
  it('splits the finalizer line into state, agent, title and body', () => {
    const text = '[done] @claude_code · 修复登录重定向\n\n登录页现在会带着来源地址跳回来。'
    assert.deepEqual(splitReportHead(text), {
      state: 'done',
      agent: 'claude_code',
      title: '修复登录重定向',
      body: '登录页现在会带着来源地址跳回来。',
    })
  })

  it('reads a header with no agent (a local run that never named one)', () => {
    assert.deepEqual(splitReportHead('[failed] 训练脚本超时\n\nGPU 队列满了。'), {
      state: 'failed',
      agent: '',
      title: '训练脚本超时',
      body: 'GPU 队列满了。',
    })
  })

  it('keeps a header with no body', () => {
    assert.deepEqual(splitReportHead('[done] @codex · 只改了标题'), {
      state: 'done',
      agent: 'codex',
      title: '只改了标题',
      body: '',
    })
  })

  it('drops the blank line the finalizer writes between head and body', () => {
    assert.equal(splitReportHead('[done] @codex · t\n\n\nbody')?.body, 'body')
  })

  it('leaves an ordinary reply alone', () => {
    assert.equal(splitReportHead('这是一段普通回复。'), null)
    // A bracketed word the console does not know is prose, not a report.
    assert.equal(splitReportHead('[注意] 这里要小心'), null)
    assert.equal(splitReportHead('[done-ish] 不是状态'), null)
    // Only the first line can be the header.
    assert.equal(splitReportHead('先说明一下\n[done] @codex · 标题'), null)
    assert.equal(splitReportHead(''), null)
  })

  it('handles a title that itself contains a middot', () => {
    assert.deepEqual(splitReportHead('[review] @claude_code · 阶段 1 · 设计'), {
      state: 'review',
      agent: 'claude_code',
      title: '阶段 1 · 设计',
      body: '',
    })
  })
})
