// SPDX-License-Identifier: AGPL-3.0-or-later

import assert from 'node:assert/strict'
import { test } from 'node:test'
import { createDirFavorites } from './dir-favorites.ts'

function deferredSnapshot() {
  let resolve!: (snapshot: string[]) => void
  let reject!: (error: Error) => void
  const promise = new Promise<string[]>((onResolve, onReject) => {
    resolve = onResolve
    reject = onReject
  })
  return { promise, resolve, reject }
}

test('favorite writes send in order and server snapshots preserve later optimistic edits', async () => {
  const first = deferredSnapshot()
  const second = deferredSnapshot()
  const requests: string[] = []
  const write = (path: string) => {
    requests.push(path)
    return path === '/a' ? first.promise : second.promise
  }
  const store = createDirFavorites(async () => [], write)
  const a = store.toggle('/a')
  const b = store.toggle('/b')
  assert.deepEqual(store.snapshot(), ['/b', '/a'])
  await Promise.resolve()
  assert.deepEqual(requests, ['/a'])

  first.resolve(['/a', '/from-other-client'])
  await a
  assert.deepEqual(store.snapshot(), ['/b', '/a', '/from-other-client'])
  await Promise.resolve()
  assert.deepEqual(requests, ['/a', '/b'])
  second.resolve(['/b', '/a', '/from-other-client'])
  await b
  assert.deepEqual(store.snapshot(), ['/b', '/a', '/from-other-client'])
})

test('a failed favorite write rolls back only its own edit and permits later writes', async () => {
  const first = deferredSnapshot()
  const second = deferredSnapshot()
  const store = createDirFavorites(async () => ['/prior'], (path) =>
    path === '/a' ? first.promise : second.promise)
  await store.refresh()
  const a = store.toggle('/a')
  const b = store.toggle('/b')
  assert.deepEqual(store.snapshot(), ['/b', '/a', '/prior'])

  first.reject(new Error('write failed'))
  await assert.rejects(a, /write failed/)
  assert.deepEqual(store.snapshot(), ['/b', '/prior'])
  second.resolve(['/b', '/prior'])
  await b
  assert.deepEqual(store.snapshot(), ['/b', '/prior'])
})

test('refresh waits for the pending write and same-directory toggles keep their intent', async () => {
  const write = deferredSnapshot()
  const mutations: [string, boolean][] = []
  let reads = 0
  let persisted = ['/prior']
  const store = createDirFavorites(async () => {
    reads++
    return persisted
  }, (path, favorite) => {
    mutations.push([path, favorite])
    if (favorite) return write.promise.then((snapshot) => persisted = snapshot)
    persisted = persisted.filter((dir) => dir !== path)
    return Promise.resolve(persisted)
  })
  await store.refresh()
  const add = store.toggle('/a')
  const remove = store.toggle('/a')
  const refresh = store.refresh()
  await Promise.resolve()
  assert.deepEqual(mutations, [['/a', true]])
  assert.deepEqual(store.snapshot(), ['/prior'])
  assert.equal(reads, 1)

  write.resolve(['/a', '/prior'])
  await Promise.all([add, remove, refresh])
  assert.deepEqual(mutations, [['/a', true], ['/a', false]])
  assert.deepEqual(store.snapshot(), ['/prior'])
  assert.equal(reads, 2)
})
