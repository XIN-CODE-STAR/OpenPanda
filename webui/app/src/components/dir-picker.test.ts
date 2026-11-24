// SPDX-License-Identifier: AGPL-3.0-or-later

import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  favoriteKey,
  groupRows,
  pathEnterTarget,
  pathLike,
  splitPathQuery,
  type DirGroup,
  type WorkdirRow,
} from './dir-rows.ts'

const day = 24 * 60 * 60 * 1000
const now = Date.parse('2026-11-23T12:00:00Z')
const rows = (...specs: [string, string, number][]): WorkdirRow[] =>
  specs.map(([name, dir, ageDays]) => ({
    name,
    dir,
    updated: new Date(now - ageDays * day).toISOString(),
  }))

/** The names in one band, or [] when the band was dropped. */
const band = (groups: DirGroup[], key: DirGroup['key']): string[] =>
  groups.find((g) => g.key === key)?.rows.map((r) => r.name) ?? []

test('favoriteKey folds case and trailing separators', () => {
  assert.equal(favoriteKey('D:\\X\\project'), favoriteKey('d:\\x\\PROJECT\\'))
  assert.equal(favoriteKey('D:\\X\\project\\\\'), 'd:\\x\\project')
  assert.notEqual(favoriteKey('D:\\X\\a'), favoriteKey('D:\\X\\b'))
})

test('groupRows puts stars first, in the order they were starred', () => {
  const list = rows(
    ['a', 'D:\\X\\a', 30],
    ['b', 'D:\\X\\b', 1],
    ['c', 'D:\\X\\c', 2],
  )
  // Starred newest-first: c, then a.
  const groups = groupRows(list, ['D:\\X\\c', 'D:\\X\\a'], now)
  assert.deepEqual(groups.map((g) => g.key), ['fav', 'recent'])
  assert.deepEqual(band(groups, 'fav'), ['c', 'a'])
  // b is recent, and the stars are not repeated in it.
  assert.deepEqual(band(groups, 'recent'), ['b'])
  assert.deepEqual(band(groups, 'other'), [])
})

test('groupRows keeps a star whose folder has no project row yet', () => {
  const list = rows(['a', 'D:\\X\\a', 1], ['b', 'D:\\X\\b', 2])
  // D:\gone was starred but no work has been filed there, so it has no
  // project row. The star was saved; the card must still paint it.
  const groups = groupRows(list, ['D:\\gone', 'D:\\X\\a'], now)
  assert.deepEqual(groups.map((g) => g.key), ['fav', 'recent'])
  assert.deepEqual(band(groups, 'fav'), ['gone', 'a'])
  assert.deepEqual(band(groups, 'recent'), ['b'])
})

test('pathEnterTarget commits the field when the listing is stale', () => {
  const stale: WorkdirRow[] = [{ name: 'old', dir: 'D:\\review\\old' }]
  // The rows answer D:\review\, the field says D:\review\slow\new: taking the
  // highlighted row ran the next task in D:\review\old.
  assert.deepEqual(
    pathEnterTarget('D:\\review\\slow\\new', stale, 'D:\\review\\', 0),
    { name: 'new', dir: 'D:\\review\\slow\\new' },
  )
  // A trailing separator means the folder itself, not a child of it.
  assert.deepEqual(
    pathEnterTarget('D:\\review\\slow\\', stale, 'D:\\review\\', 0),
    { name: 'slow', dir: 'D:\\review\\slow' },
  )
  // Nothing typed, nothing to take.
  assert.equal(pathEnterTarget('  ', stale, 'D:\\review\\', 0), null)
})

test('pathEnterTarget takes the highlighted row when the listing is current', () => {
  const fresh: WorkdirRow[] = [
    { name: 'old', dir: 'D:\\review\\old' },
    { name: 'slow', dir: 'D:\\review\\slow' },
    { name: 'new', dir: 'D:\\review\\slow\\new', create: true },
  ]
  assert.deepEqual(pathEnterTarget('D:\\review\\sl', fresh, 'D:\\review\\sl', 1), fresh[1])
  // The cursor may point past a list that shrank: fall back to the first row.
  assert.deepEqual(pathEnterTarget('D:\\review\\sl', fresh, 'D:\\review\\sl', 9), fresh[0])
  // A create row keeps its flag so the caller can mkdir before choosing.
  assert.equal(pathEnterTarget('D:\\review\\sl', fresh, 'D:\\review\\sl', 2)?.create, true)
})

test('pathEnterTarget turns a bare drive into its root', () => {
  // `D:` is a drive-relative path Windows resolves to whatever directory that
  // drive was last left in — never a place to run a task.
  assert.deepEqual(pathEnterTarget('D:\\', [], 'C:\\', 0), { name: 'D:', dir: 'D:\\' })
  assert.deepEqual(pathEnterTarget('D:', [], 'C:\\', 0), { name: 'D:', dir: 'D:\\' })
  // Even with its own listing on screen: `D:` has no row of its own, and the
  // children are only suggestions.
  const children: WorkdirRow[] = [{ name: 'review', dir: 'D:\\review' }]
  assert.deepEqual(pathEnterTarget('D:', children, 'D:', 0), { name: 'D:', dir: 'D:\\' })
  // A lone POSIX root is the folder the user typed.
  assert.deepEqual(pathEnterTarget('/', [], '', 0), { name: '/', dir: '/' })
})

test('groupRows calls seven days recent and eight days old', () => {
  const list = rows(['edge', 'D:\\X\\edge', 7], ['old', 'D:\\X\\old', 8])
  const groups = groupRows(list, [], now)
  assert.deepEqual(groups.map((g) => g.key), ['recent', 'other'])
  assert.deepEqual(band(groups, 'recent'), ['edge'])
  assert.deepEqual(band(groups, 'other'), ['old'])
})

test('groupRows puts rows without a stamp in the tail band', () => {
  const list: WorkdirRow[] = [{ name: 'x', dir: 'D:\\X\\x' }]
  const groups = groupRows(list, [], now)
  assert.deepEqual(groups.map((g) => g.key), ['other'])
  assert.deepEqual(band(groups, 'other'), ['x'])
})

test('pathLike tells a path from a search word', () => {
  assert.equal(pathLike('D:\\X'), true)
  assert.equal(pathLike('~/code'), true)
  assert.equal(pathLike('C:'), true)
  assert.equal(pathLike('panda'), false)
  assert.equal(pathLike(''), false)
})

test('splitPathQuery splits at the last separator', () => {
  assert.deepEqual(splitPathQuery('D:\\X\\proj'), { base: 'D:\\X', prefix: 'proj', self: '' })
  // A trailing separator means "this folder": it is offered as itself.
  assert.deepEqual(splitPathQuery('D:\\X\\'), { base: 'D:\\X', prefix: '', self: 'D:\\X' })
  // A drive letter on its own is a place to look, not a prefix.
  assert.deepEqual(splitPathQuery('D:'), { base: 'D:\\', prefix: '', self: '' })
  assert.equal(splitPathQuery('proj'), null)
})
