// SPDX-License-Identifier: AGPL-3.0-or-later

import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  favoriteKey,
  groupRows,
  normDir,
  pathEnterTarget,
  pathLike,
  pathQueryRows,
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
  assert.equal(pathLike('~'), true)
  assert.equal(pathLike('~project'), false)
  assert.equal(pathLike('C:'), true)
  assert.equal(pathLike('C:relative'), false)
  assert.equal(pathLike('panda'), false)
  assert.equal(pathLike(''), false)
  // A separator inside a word does not make it a path: the server would
  // resolve it against its own working directory, not the one on screen.
  assert.equal(pathLike('src/utils'), false)
  assert.equal(pathLike('/usr'), true)
})

test('splitPathQuery splits at the last separator', () => {
  assert.deepEqual(splitPathQuery('D:\\X\\proj'), { base: 'D:\\X', prefix: 'proj', self: '' })
  // A trailing separator means "this folder": it is offered as itself.
  assert.deepEqual(splitPathQuery('D:\\X\\'), { base: 'D:\\X', prefix: '', self: 'D:\\X' })
  // A drive letter on its own is a place to look, not a prefix.
  assert.deepEqual(splitPathQuery('D:'), { base: 'D:\\', prefix: '', self: '' })
  assert.equal(splitPathQuery('proj'), null)
  // An absolute path cuts at 0: the base is the root, not the empty string —
  // an empty base asks the server for "no path", which it answers with home.
  assert.deepEqual(splitPathQuery('/usr'), { base: '/', prefix: 'usr', self: '' })
  // The root itself is a place to look; returning null left the card on
  // "loading" forever.
  assert.deepEqual(splitPathQuery('/'), { base: '/', prefix: '', self: '/' })
  assert.deepEqual(splitPathQuery('\\'), { base: '\\', prefix: '', self: '\\' })
  assert.deepEqual(splitPathQuery('~'), { base: '~', prefix: '', self: '~' })
})

test('normDir folds case for Windows paths only', () => {
  assert.equal(normDir('D:\\X\\Proj\\'), 'd:\\x\\proj')
  // A POSIX tree is case-sensitive: /w/New and /w/new are two folders, and
  // folding them made the cursor land on the wrong one.
  assert.equal(normDir('/w/New/'), '/w/New')
  assert.notEqual(normDir('/w/New'), normDir('/w/new'))
  assert.notEqual(normDir('/'), normDir(''))
})

test('path completions put the exact POSIX directory ahead of folded matches', () => {
  const query = '/work/new'
  const listing = {
    separator: '/',
    current: '/work',
    parent: '/',
    directories: [
      { name: 'New', path: '/work/New' },
      { name: 'new-project', path: '/work/new-project' },
      { name: 'new', path: '/work/new' },
    ],
  }
  const options = pathQueryRows(query, splitPathQuery(query)!, listing)
  assert.deepEqual(options.map((row) => row.dir), ['/work/new', '/work/New', '/work/new-project'])
  assert.equal(pathEnterTarget(query, options, query, 0)?.dir, '/work/new')
  assert.equal(pathEnterTarget(query, options, query, 1)?.dir, '/work/New')
  const differentCase = '/work/NEW'
  assert.deepEqual(pathQueryRows(differentCase, splitPathQuery(differentCase)!, listing).at(-1), {
    name: 'NEW', dir: differentCase, create: true,
  })
})

test('path completions match drive aliases and ignore Windows case and separators', () => {
  const query = 'd:/work/new'
  const options = pathQueryRows(query, splitPathQuery(query)!, {
    separator: '\\',
    current: 'D:\\Work',
    parent: 'D:\\',
    directories: [
      { name: 'New Project', path: 'D:\\Work\\New Project' },
      { name: 'New', path: 'D:\\Work\\New' },
    ],
  })
  assert.equal(options.length, 2)
  assert.equal(pathEnterTarget(query, options, query, 0)?.dir, 'D:\\Work\\New')
})

test('root and home paths select the directory itself when the listing arrives', () => {
  const root = pathQueryRows('/', splitPathQuery('/')!, {
    separator: '/', current: '/', parent: '', directories: [{ name: 'usr', path: '/usr' }],
  })
  assert.equal(pathEnterTarget('/', root, '/', 0)?.dir, '/')
  assert.equal(pathEnterTarget('/', root, '/', 1)?.dir, '/usr')

  const home = pathQueryRows('~', splitPathQuery('~')!, {
    separator: '/', current: '/home/reader', parent: '/home', directories: null,
  })
  assert.equal(pathEnterTarget('~', home, '', 0), null)
  assert.equal(pathEnterTarget('~', home, '~', 0)?.dir, '/home/reader')
})

test('a home-relative completion uses the node path and only offers creation for missing paths', () => {
  const query = '~/projects'
  const options = pathQueryRows(query, splitPathQuery(query)!, {
    separator: '/',
    current: '/home/reader',
    parent: '/home',
    directories: [{ name: 'projects', path: '/home/reader/projects' }],
  })
  assert.deepEqual(options, [{ name: 'projects', dir: '/home/reader/projects' }])

  const withSeparator = query + '/'
  const self = pathQueryRows(withSeparator, splitPathQuery(withSeparator)!, {
    separator: '/', current: '/home/reader/projects', parent: '/home/reader', directories: [],
  })
  assert.equal(pathEnterTarget(withSeparator, self, withSeparator, 0)?.dir, '/home/reader/projects')
})
