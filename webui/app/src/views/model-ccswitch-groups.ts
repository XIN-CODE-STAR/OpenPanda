// SPDX-License-Identifier: AGPL-3.0-or-later

// The CC Switch sheet's DOM-free half: turning one roster's rows into the
// provider sections the sheet draws. It lives beside the view because the view
// is JSX and `node --test` only loads .ts — the split is what decides which
// rows a heading owns and what a group tick would take, so it is the part
// worth pinning.

import type { DiscoveredModel } from '../api/client'
import { hostOf } from './model-connections.ts'

/** A row can be ticked when it is importable and not already registered. */
export function usable(c: DiscoveredModel): boolean {
  return c.importable && !c.existing
}

/** One provider section: the heading, its rows, and the ids a group-level
 *  tick would take. `host` is the endpoint the rows share. */
export type ProviderSection = {
  key: string
  label: string
  host: string
  rows: DiscoveredModel[]
  ids: string[]
}

/** Split one roster into provider sections, in the order the server sent them
 *  (CC Switch's own sort_index). The split arrives on the rows themselves; a
 *  source that reports none collapses into a single unlabelled section, which
 *  renders exactly like the flat list this sheet used to be. */
export function splitProviders(rows: DiscoveredModel[]): ProviderSection[] {
  const order: string[] = []
  const byKey = new Map<string, ProviderSection>()
  for (const c of rows) {
    const key = c.group || ''
    let s = byKey.get(key)
    if (!s) {
      s = {
        key,
        label: c.group_label || (c.base_url ? hostOf(c.base_url) : c.provider_label || c.provider),
        host: c.base_url ? hostOf(c.base_url) : '',
        rows: [],
        ids: [],
      }
      byKey.set(key, s)
      order.push(key)
    }
    s.rows.push(c)
    if (usable(c)) s.ids.push(c.id)
  }
  return order.map((k) => byKey.get(k)!)
}
