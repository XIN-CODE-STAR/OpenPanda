// SPDX-License-Identifier: AGPL-3.0-or-later

import { favoriteKey } from './dir-rows.ts'

type Edit = { path: string; favorite: boolean }

/** Keep reads and writes in server order. Each response is a full snapshot,
 *  overlaid with the edits whose writes have yet to settle. */
export function createDirFavorites(
  read: () => Promise<string[]>,
  write: (path: string, favorite: boolean) => Promise<string[]>,
) {
  let committed: string[] = []
  let visible: string[] = []
  let tail = Promise.resolve()
  const pending: Edit[] = []
  const listeners = new Set<() => void>()

  function publish() {
    visible = pending.reduce((favorites, { path, favorite }) => {
      const rest = favorites.filter((dir) => favoriteKey(dir) !== favoriteKey(path))
      return favorite ? [path, ...rest] : rest
    }, committed)
    for (const listener of listeners) listener()
  }

  function enqueue(task: () => Promise<void>) {
    const result = tail.then(task)
    tail = result.catch(() => {})
    return result
  }

  return {
    snapshot: () => visible,
    subscribe(listener: () => void) {
      listeners.add(listener)
      return () => { listeners.delete(listener) }
    },
    refresh() {
      return enqueue(async () => {
        committed = await read()
        publish()
      })
    },
    toggle(path: string) {
      const favorite = !visible.some((dir) => favoriteKey(dir) === favoriteKey(path))
      pending.push({ path, favorite })
      const saving = enqueue(async () => {
        try {
          committed = await write(path, favorite)
        } finally {
          pending.shift()
          publish()
        }
      })
      publish()
      return saving
    },
  }
}
